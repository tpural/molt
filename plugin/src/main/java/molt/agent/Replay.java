package molt.agent;

import io.papermc.paper.event.player.AsyncPlayerSpawnLocationEvent;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.TimeUnit;
import org.bukkit.Bukkit;
import org.bukkit.GameMode;
import org.bukkit.Location;
import org.bukkit.World;
import org.bukkit.block.Block;
import org.bukkit.block.data.BlockData;
import org.bukkit.entity.Player;
import org.bukkit.event.EventHandler;
import org.bukkit.event.EventPriority;
import org.bukkit.event.Listener;
import org.bukkit.event.player.PlayerJoinEvent;
import org.bukkit.inventory.EquipmentSlot;
import org.bukkit.inventory.ItemStack;
import org.bukkit.plugin.Plugin;

/**
 * What players did while their server was frozen for a hand-off, and where
 * they went: the old server notes it, and the new one does it again as each
 * player arrives.
 */
final class Replay implements Listener {
    /** A block broken or placed, or a command run. */
    record Action(String kind, String world, int x, int y, int z, String block, String hand, String command) {}

    record Arrival(String uuid, String world, double x, double y, double z, float yaw, float pitch,
                   List<Action> actions) {}

    record Handover(List<Arrival> players) {}

    /** How long a handed-off player is expected: one who couldn't move may come back. */
    private static final long EXPECT = TimeUnit.MINUTES.toNanos(2);

    private record Expected(Arrival arrival, long until) {}

    private final Plugin plugin;
    private final Map<UUID, List<Action>> done = new ConcurrentHashMap<>();
    private final Map<UUID, Expected> expected = new ConcurrentHashMap<>();

    Replay(Plugin plugin) {
        this.plugin = plugin;
    }

    // The old server.

    void clear() {
        done.clear();
    }

    private void note(Player p, Action a) {
        done.computeIfAbsent(p.getUniqueId(), k -> java.util.Collections.synchronizedList(new ArrayList<>())).add(a);
    }

    void broke(Player p, Block b) {
        note(p, new Action("break", b.getWorld().getName(), b.getX(), b.getY(), b.getZ(), null, null, null));
    }

    void placed(Player p, Block b, EquipmentSlot hand) {
        note(p, new Action("place", b.getWorld().getName(), b.getX(), b.getY(), b.getZ(),
                b.getBlockData().getAsString(), hand.name(), null));
    }

    void ran(Player p, String message) {
        note(p, new Action("command", null, 0, 0, 0, null, null, message.startsWith("/") ? message.substring(1) : message));
    }

    /** Everyone online: where they are, and what they did. Run on the main thread. */
    Handover handover() {
        List<Arrival> out = new ArrayList<>();
        for (Player p : Bukkit.getOnlinePlayers()) {
            Location l = p.getLocation();
            List<Action> did = done.getOrDefault(p.getUniqueId(), List.of());
            synchronized (did) {
                out.add(new Arrival(p.getUniqueId().toString(), l.getWorld().getName(), l.getX(), l.getY(), l.getZ(),
                        l.getYaw(), l.getPitch(), List.copyOf(did)));
            }
        }
        return new Handover(out);
    }

    // The new server.

    void expect(Handover h) {
        if (h == null || h.players() == null) {
            return;
        }
        long until = System.nanoTime() + EXPECT;
        for (Arrival a : h.players()) {
            expected.put(UUID.fromString(a.uuid()), new Expected(a, until));
        }
    }

    private Arrival arrival(UUID id, boolean take) {
        Expected e = take ? expected.remove(id) : expected.get(id);
        return e != null && System.nanoTime() < e.until() ? e.arrival() : null;
    }

    @EventHandler(priority = EventPriority.HIGHEST)
    public void onSpawn(AsyncPlayerSpawnLocationEvent e) {
        Arrival a = arrival(e.getConnection().getProfile().getId(), false);
        World w = a == null ? null : Bukkit.getWorld(a.world());
        if (w != null) {
            e.setSpawnLocation(new Location(w, a.x(), a.y(), a.z(), a.yaw(), a.pitch()));
        }
    }

    @EventHandler(priority = EventPriority.MONITOR)
    public void onJoin(PlayerJoinEvent e) {
        Player p = e.getPlayer();
        Arrival a = arrival(p.getUniqueId(), true);
        if (a == null || a.actions() == null || a.actions().isEmpty()) {
            return;
        }
        Bukkit.getScheduler().runTask(plugin, () -> {
            for (Action act : a.actions()) {
                try {
                    redo(p, act);
                } catch (RuntimeException ex) {
                    plugin.getLogger().warning("redoing " + p.getName() + "'s " + act.kind() + ": " + ex);
                }
            }
        });
    }

    private void redo(Player p, Action a) {
        if (!p.isOnline()) {
            return;
        }
        if (a.kind().equals("command")) {
            p.performCommand(a.command());
            return;
        }
        World w = Bukkit.getWorld(a.world());
        if (w == null) {
            return;
        }
        Block b = w.getBlockAt(a.x(), a.y(), a.z());
        switch (a.kind()) {
            case "break" -> {
                if (!b.isEmpty()) {
                    p.breakBlock(b);
                }
            }
            case "place" -> place(p, b, Bukkit.createBlockData(a.block()), EquipmentSlot.valueOf(a.hand()));
            default -> plugin.getLogger().warning("can't redo " + a.kind());
        }
    }

    /** Place as the old server did: where nothing's in the way, using up what the player held. */
    private static void place(Player p, Block b, BlockData data, EquipmentSlot hand) {
        if (!b.isReplaceable()) {
            return;
        }
        if (p.getGameMode() != GameMode.CREATIVE) {
            ItemStack held = p.getInventory().getItem(hand);
            if (held.getType() != data.getPlacementMaterial() || held.getAmount() < 1) {
                return;
            }
            held.setAmount(held.getAmount() - 1);
            p.getInventory().setItem(hand, held);
        }
        b.setBlockData(data);
    }
}
