# molt

A Minecraft server that updates without restarting. Change its plugins or its Paper version, and players
keep playing: a short freeze, a moment of loading terrain, and they're where they were, holding what they
held, on the new server.

Plugins can't be loaded into a running server safely, so molt swaps the whole server instead:

1. **Players connect to molt**, an always-on proxy ([Gate](https://github.com/minekube/gate)), never to a
   server directly.
2. **A new server starts beside the old one**, on a copy of the world, while everyone keeps playing.
   Once it's up, what changed meanwhile is copied over too.
3. **The old server freezes**: it stops the world and saves everything. molt copies the few files that
   changed in the last moment to the new server, which rereads them.
4. **The proxy moves everyone across** and the old server stops.

During the freeze players can still walk, break and place blocks, and run commands: the old server notes
what they do and the new one does it again as they arrive, so nothing snaps back. Opening chests, using
items and moving things in inventories wait until they're across.

In the end-to-end test, with a bot building and walking through the swap, the freeze was about a second,
most of it the move itself.

## Parts

- **`molt`** (Go): the proxy, the conductor that runs swaps, and an API. It starts backends as Kubernetes
  Pods on its own node, or as Docker containers.
- **molt-agent** (a Paper plugin, in [plugin/](plugin/)): how molt snapshots, freezes and hands over
  each server.
- **`ghcr.io/tpural/molt-paper`**: the image backends run. It's
  [itzg/minecraft-server](https://github.com/itzg/docker-minecraft-server) with molt-agent, so its
  environment (`VERSION`, `PLUGINS`, `MEMORY`, …) is how you say what a server runs.

Each generation of server has its own directory (`<root>/gen-N`), so two servers never write the same files.
The last few are kept for going back by hand.

## Running it

`deploy/` has an example for Kubernetes. Set the node in `deployment.yaml` (worlds live on its disk; don't
use NFS for them), and make a Secret `molt` with `MOLT_SECRET` (shared with the backends),
`MOLT_AGENT_TOKEN` and `MOLT_API_TOKEN`.

```yaml
# config.yaml
root: /var/lib/molt
runner: kubernetes        # or docker
kubernetes:
  memory: 4Gi             # each backend's; two run at once during a swap
spec:                     # the first server; swaps change it from there
  image: ghcr.io/tpural/molt-paper:0.1.0
  env:
    VERSION: "26.3"
    MEMORY: 3G
```

## Swapping

```sh
export MOLT_URL=http://molt-api:8080 MOLT_API_TOKEN=...
molt swap -env PLUGINS=https://example.com/SomePlugin.jar   # add a plugin
molt swap -env VERSION=26.3                                 # another Paper version
molt swap -image ghcr.io/tpural/molt-paper:0.2.0            # a new molt-agent
molt status
```

A swap changes the running server's spec: `-env` sets variables, `-unset` removes them, `-image` replaces
the image. `POST /swap` with `{"image", "env", "unset"}` does the same.

If anything fails before players move, the old server thaws and carries on, and the new one is removed.

## What carries over

Everything in the worlds' folders (blocks, chests and machines, mobs, players and their inventories, world
data), the time and weather, and `ops.json`, the whitelist and the ban lists. Players arrive where they
were at the end of the freeze.

Not yet:
- **Plugins' own data** (`plugins/<name>/`): each server starts with its own.
- **World-wide state the new server read when it started** (scoreboards, raids, game rules, the world
  border) is from the first copy, before the new server started.
- **Restarting molt itself** drops players' connections. The backend keeps running and the new molt adopts
  it.

## Developing

```sh
make test     # unit tests
make e2e      # a real swap in Docker, with a bot playing through it (needs docker and node)
```

The bot speaks 26.1, so the end-to-end test runs Paper 26.1.2.
