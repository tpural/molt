# Plan: a switch with no loading screen

Today a swap ends with the client on a "Loading terrain" screen for a moment. The freeze before it is
about a second, so the screen is now most of what a player notices. This is the plan to remove it, and the
pause it causes along with it.

## Why there's a screen

A Minecraft client talks to one server at a time, and since 1.20.2 joining a server has two phases:
*configuration* (registries, tags, feature flags, resource packs) and then *play*. When the proxy moves a
player, Gate does what every proxy does:

1. It tells the client to go back to configuration (`StartUpdate`). The client drops its world.
2. It forwards the new server's configuration, then its `JoinGame`. The client builds a new world, with
   a new entity ID for the player.
3. The client shows the loading screen until the chunks around the player arrive.

Every step exists because, in general, the next server could be anything: another version, other
registries, other dimensions. A molt swap is the special case where it's the *same* world, usually on the
same version, a second later. The client already has nearly everything it needs.

## The idea

Keep the client in play and let it believe it never left. The proxy does the new server's login and
configuration *itself*, out of the client's sight. Then it makes the new server's view of the world land
on the client as ordinary updates: chunks resent, entities replaced, the player's own state set again.

That only works if the new server's configuration is what the client already has. So the proxy records the
configuration each client got when it joined. If the new server sends anything different (a new Paper
version with new registries, a plugin adding tags or a datapack), it falls back to today's switch. Version
upgrades keep the loading screen; plugin and config swaps, which are most swaps, don't.

## What has to be solved

**1. Configuration done by the proxy.**
- When B's backend connection reaches configuration, Gate answers B's known-packs request, client
  information (replaying the client's last settings: locale, view distance, skin parts) and the finish,
  instead of forwarding them.
- It compares B's registry data, tags and feature flags with what the client got at its own join, by
  hash.
- A resource pack from B is pushed in play, where clients accept it too.
- The client's chat session (signed chat) is replayed to B.

**2. The player's entity ID.**
- The client knows itself by A's ID, and B will use another. Rewriting IDs in every packet that carries
  one is how old BungeeCord did it. It's a lot of packets, and it breaks on every protocol change.
- molt controls both servers, so it can make B use **the same ID** instead:
  - at the freeze, A reports its entity counter and each player's ID;
  - when B follows, its counter (a static `AtomicInteger` on `Entity`) is raised above A's. Every entity
    B loads at takeover then gets a fresh, higher ID;
  - as a player joins B, molt-agent sets its ID to A's before it enters the world (`Entity.setId`).
- No packet rewriting, and no collisions: A's IDs are all below B's counter.

**3. Clearing A's leftovers on the client.** B's packets overwrite chunks and the player's own state.
Everything else the client was told by A has to go first, or it would be shown twice:
- **Entities:** the proxy tracks the IDs A spawned for each client (Spawn Entity / Remove Entities
  packets only) and removes them all.
- **Tab list, boss bars, scoreboards and teams, title, world border, open container:** cleared or reset.
  Gate already tracks the tab list and boss bars.

**4. B's `JoinGame`.**
- It's swallowed.
- What it carries that matters (game mode, view and simulation distance, the dimension) is applied with
  play packets.
- If the player is in another dimension on B than on A (they shouldn't be), it falls back.
- Whether the client needs a same-dimension `Respawn` to reset its player is an open question for the
  prototype. Ideally there's no respawn at all.

**5. Gate.**
- The session handlers that do this are internal to Gate (`session_backend_config.go`,
  `session_backend_transition.go`, `player.go`'s `switchToConfigState`).
- So molt needs a small Gate fork, pinned with a `replace`, adding a "seamless" connect path next to
  `CreateConnectionRequest`.
- Upstreaming it is worth offering later, since it's useful to any proxy whose servers share a world.

## Stages

1. **Prototype, to settle the open questions** (about a day). By hand, with a real 26.3 client, a
   mineflayer bot and a packet log, measure what the client does:
   - when it gets chunks it already has, Remove Entities for everything, and B's state, without a new
     `JoinGame`;
   - whether a same-dimension `Respawn` is needed, and whether it shows a screen.

   This decides the shape of stages 3 and 4.
2. **Same entity ID on B** (molt-agent only). The counter bump at follow, and `setId` as players join. It's
   harmless on its own, and testable: the e2e asserts the bot's entity ID is the same before and after.
3. **The seamless connect path in the Gate fork:**
   - configuration by the proxy, with the hash check and the fallback;
   - the swallowed `JoinGame`;
   - leftover tracking and clearing.

   molt's `Move` uses it, and falls back per player.
4. **e2e:**
   - the bot asserts it never left play (no `StartUpdate`, `JoinGame` or `Respawn`), and that its
     entity ID, position, inventory and the world all match;
   - nearby entities aren't shown twice;
   - the gap between the last packet from A and the first chunk from B is measured.

   A second case swaps the Paper version and checks the fallback.
5. **Your client:** swaps while you play, looking for flicker, duplicates, a wrong tab list, a stuck open
   chest, or riding a horse or boat across.

## Risks

- **The client may not accept a world being replaced under it.** Vanilla treats some state as fixed for a
  session, and some mods (minimaps, shaders) cache chunks. The prototype exists to find this early. If a
  respawn turns out to be needed, there may still be a brief screen, but without reconfiguring, which is
  most of today's pause.
- **Vehicles and passengers** carry entity IDs: a player riding something needs the ride's ID handled
  too, or is dismounted at the swap.
- **Each Minecraft version** can change configuration or play packets. Comparing configuration by hash
  keeps that safe, because anything unknown falls back. The fork has to follow Gate's releases.
- **Bedrock players through Geyser, and Forge or Fabric clients with their own handshakes,** always
  fall back.

## How it'll feel

On a plugin or config swap, the world pauses for about a second, which the freeze work already gets
down to. Chunks may refresh, and mobs may blink. No screen, no lost input.
