package molt.agent;

import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;
import java.util.Map;
import java.util.concurrent.Callable;
import java.util.concurrent.TimeUnit;
import net.minecraft.server.MinecraftServer;
import org.bukkit.Bukkit;
import org.bukkit.Chunk;
import org.bukkit.World;
import org.bukkit.craftbukkit.CraftServer;
import org.bukkit.craftbukkit.CraftWorld;
import org.bukkit.entity.Player;
import org.bukkit.event.Cancellable;
import org.bukkit.event.EventHandler;
import org.bukkit.event.EventPriority;
import org.bukkit.event.Listener;
import org.bukkit.event.block.BlockBreakEvent;
import org.bukkit.event.block.BlockPlaceEvent;
import org.bukkit.event.entity.EntityDamageByEntityEvent;
import org.bukkit.event.inventory.InventoryClickEvent;
import org.bukkit.event.player.AsyncPlayerPreLoginEvent;
import org.bukkit.event.player.PlayerCommandPreprocessEvent;
import org.bukkit.event.player.PlayerDropItemEvent;
import org.bukkit.event.player.PlayerInteractEntityEvent;
import org.bukkit.event.player.PlayerInteractEvent;
import org.bukkit.plugin.Plugin;

/** The hand-off: this server's role, and what each step does to it. */
final class Handoff implements Listener {
    /**
     * standby: started, refusing players. follower: frozen, waiting. frozen: a
     * leader handing off, noting what players do. leaving: handed off, waiting
     * for players to go.
     */
    enum Role { standby, follower, frozen, leaving, leader }

    record PlayerAt(String uuid, String name, String world, double x, double y, double z) {}

    record WorldState(String name, long fullTime, boolean storm, boolean thundering,
                      int weatherTicks, int thunderTicks, int clearTicks) {}

    record Freeze(List<PlayerAt> players, List<WorldState> worlds, List<String> folders) {}

    /** How far around each player chunks are loaded as they arrive. */
    private static final int PRELOAD = 3;

    private final Plugin plugin;
    private final Replay replay;
    private volatile Role role = Role.standby;
    private volatile boolean ready;

    Handoff(Plugin plugin) {
        this.plugin = plugin;
        this.replay = new Replay(plugin);
    }

    Replay replay() {
        return replay;
    }

    void ready() {
        ready = true;
    }

    private <T> T sync(Callable<T> c) throws Exception {
        try {
            return Bukkit.getScheduler().callSyncMethod(plugin, c).get(60, TimeUnit.SECONDS);
        } catch (java.util.concurrent.ExecutionException e) {
            throw e.getCause() instanceof Exception ex ? ex : e;
        }
    }

    private void sync(Runnable r) throws Exception {
        sync(() -> {
            r.run();
            return null;
        });
    }

    private void need(Role... roles) {
        for (Role r : roles) {
            if (role == r) {
                return;
            }
        }
        throw new IllegalStateException("this server is " + role + ", not " + List.of(roles));
    }

    Map<String, Object> state() throws Exception {
        return sync(() -> Map.of(
                "role", role.name(),
                "ready", ready,
                "frozen", Bukkit.getServerTickManager().isFrozen(),
                "players", Bukkit.getOnlinePlayers().size(),
                "version", Bukkit.getVersion(),
                "plugins", List.of(Bukkit.getPluginManager().getPlugins()).stream().map(Plugin::getName).toList(),
                "folders", folders()));
    }

    /**
     * The worlds' folders, relative to the server's, leaving out ones inside
     * another. A world's folder is the one holding its level.dat: a
     * dimension's own folder is inside it, and players are kept beside it.
     */
    static List<String> folders() {
        Path root = Bukkit.getWorldContainer().toPath().toAbsolutePath().normalize();
        List<Path> all = Bukkit.getWorlds().stream()
                .map(w -> level(root, w.getWorldFolder().toPath().toAbsolutePath().normalize()))
                .distinct()
                .sorted(Comparator.comparingInt(Path::getNameCount))
                .toList();
        List<Path> out = new ArrayList<>();
        for (Path p : all) {
            if (out.stream().noneMatch(p::startsWith)) {
                out.add(p);
            }
        }
        return out.stream().map(p -> root.relativize(p).toString()).toList();
    }

    private static Path level(Path root, Path folder) {
        for (Path p = folder; p != null && p.startsWith(root) && !p.equals(root); p = p.getParent()) {
            if (java.nio.file.Files.exists(p.resolve("level.dat"))) {
                return p;
            }
        }
        return folder;
    }

    private static MinecraftServer server() {
        return ((CraftServer) Bukkit.getServer()).getServer();
    }

    /** Saves players, chunks and world data, and waits for it all to be on disk. */
    private static void saveAll() {
        server().saveEverything(true, true, true);
    }

    private static void autosave(boolean on) {
        for (World w : Bukkit.getWorlds()) {
            w.setAutoSave(on);
        }
    }

    void snapshotBegin() throws Exception {
        need(Role.leader);
        sync(() -> {
            autosave(false);
            saveAll();
        });
    }

    void snapshotEnd() throws Exception {
        sync(() -> autosave(true));
    }

    /** Follow: stop the world and saving, and let go of every chunk read at start. */
    void follow() throws Exception {
        need(Role.standby);
        if (!ready) {
            throw new IllegalStateException("this server is still starting");
        }
        sync(() -> {
            saveAll();
            autosave(false);
            Bukkit.getServerTickManager().setFrozen(true);
        });
        unloadAll();
        role = Role.follower;
    }

    /** Freeze the leader for a hand-off: everything stops and is saved. */
    Freeze freeze() throws Exception {
        need(Role.leader);
        replay.clear();
        role = Role.frozen;
        try {
            return sync(() -> {
                Bukkit.getServerTickManager().setFrozen(true);
                autosave(false);
                saveAll();
                List<PlayerAt> players = new ArrayList<>();
                for (Player p : Bukkit.getOnlinePlayers()) {
                    var l = p.getLocation();
                    players.add(new PlayerAt(p.getUniqueId().toString(), p.getName(), l.getWorld().getName(),
                            l.getX(), l.getY(), l.getZ()));
                }
                List<WorldState> worlds = new ArrayList<>();
                for (World w : Bukkit.getWorlds()) {
                    worlds.add(new WorldState(w.getName(), w.getFullTime(), w.hasStorm(), w.isThundering(),
                            w.getWeatherDuration(), w.getThunderDuration(), w.getClearWeatherDuration()));
                }
                return new Freeze(players, worlds, folders());
            });
        } catch (Exception e) {
            thaw();
            throw e;
        }
    }

    /** Stop noting what players do, and say what they did and where they are now. */
    Replay.Handover handoff() throws Exception {
        need(Role.frozen);
        role = Role.leaving;
        return sync(replay::handover);
    }

    void thaw() throws Exception {
        replay.clear();
        sync(() -> {
            Bukkit.getServerTickManager().setFrozen(false);
            autosave(true);
        });
        role = Role.leader;
    }

    /**
     * Take over from a frozen leader whose files were copied here: reread the
     * worlds from disk, apply its time and weather, and start playing. A first
     * server takes over from nobody.
     */
    void takeover(Freeze f) throws Exception {
        if (role == Role.standby && ready && f.folders() == null) {
            role = Role.leader; // the first server: there's nobody to take over from
            return;
        }
        need(Role.follower);
        long t0 = System.nanoTime();
        unloadAll();
        long t1 = System.nanoTime();
        sync(() -> {
            for (World w : Bukkit.getWorlds()) {
                Regions.forget(((CraftWorld) w).getHandle());
            }
            apply(f.worlds());
            return null;
        });
        long t2 = System.nanoTime();
        preload(f.players());
        long t3 = System.nanoTime();
        sync(() -> {
            autosave(true);
            Bukkit.getServerTickManager().setFrozen(false);
        });
        role = Role.leader;
        plugin.getLogger().info(String.format("took over: unloading %dms, rereading %dms, preloading %dms, starting %dms",
                (t1 - t0) / 1_000_000, (t2 - t1) / 1_000_000, (t3 - t2) / 1_000_000, (System.nanoTime() - t3) / 1_000_000));
    }

    private static void apply(List<WorldState> worlds) {
        if (worlds == null) {
            return;
        }
        for (WorldState s : worlds) {
            World w = Bukkit.getWorld(s.name());
            if (w == null) {
                continue;
            }
            if (w.getEnvironment() == World.Environment.NORMAL) {
                w.setFullTime(s.fullTime()); // the others have no clock
            }
            w.setStorm(s.storm());
            w.setThundering(s.thundering());
            w.setWeatherDuration(s.weatherTicks());
            w.setThunderDuration(s.thunderTicks());
            w.setClearWeatherDuration(s.clearTicks());
        }
    }

    /** Unload every chunk without saving, waiting until they're gone. */
    private void unloadAll() throws Exception {
        long until = System.nanoTime() + TimeUnit.SECONDS.toNanos(10);
        while (true) {
            int left = sync(() -> {
                int n = 0;
                for (World w : Bukkit.getWorlds()) {
                    w.removePluginChunkTickets(plugin);
                    for (Chunk c : w.getLoadedChunks()) {
                        w.unloadChunk(c.getX(), c.getZ(), false);
                    }
                    n += w.getLoadedChunks().length;
                }
                return n;
            });
            if (left == 0) {
                return;
            }
            if (System.nanoTime() > until) {
                throw new IllegalStateException(left + " chunks wouldn't unload, so they can't be reread");
            }
            Thread.sleep(100);
        }
    }

    /**
     * Start loading chunks around where players will be, those they stand in
     * first. They load while players connect, and are held a while so they're
     * still there when players arrive.
     */
    private void preload(List<PlayerAt> players) throws Exception {
        if (players == null || players.isEmpty()) {
            return;
        }
        sync(() -> {
            for (PlayerAt p : players) {
                World w = Bukkit.getWorld(p.world());
                if (w == null) {
                    continue;
                }
                int cx = (int) Math.floor(p.x()) >> 4;
                int cz = (int) Math.floor(p.z()) >> 4;
                for (int x = -PRELOAD; x <= PRELOAD; x++) {
                    for (int z = -PRELOAD; z <= PRELOAD; z++) {
                        w.getChunkAtAsync(cx + x, cz + z, true, x == 0 && z == 0)
                                .thenAccept(c -> c.addPluginChunkTicket(plugin));
                    }
                }
            }
            Bukkit.getScheduler().runTaskLater(plugin, () -> Bukkit.getWorlds().forEach(w -> w.removePluginChunkTickets(plugin)), 30 * 20);
        });
    }

    // While frozen for a hand-off, players move freely, and what they break,
    // place and run is noted and done again on the next server. Anything else
    // would be lost, so it doesn't happen. Once handed off, nothing counts.

    private boolean handing() {
        return role == Role.frozen || role == Role.leaving;
    }

    private void stop(Cancellable e) {
        if (handing()) {
            e.setCancelled(true);
        }
    }

    /** Breaking and placing go ahead, but nothing else a click would do. */
    @EventHandler(priority = EventPriority.LOWEST)
    public void onInteract(PlayerInteractEvent e) {
        if (role == Role.leaving) {
            e.setCancelled(true);
        } else if (role == Role.frozen) {
            switch (e.getAction()) {
                case LEFT_CLICK_BLOCK, LEFT_CLICK_AIR -> {} // breaking
                case RIGHT_CLICK_BLOCK -> {
                    // Placing, unless the click would open or flip what it's on.
                    boolean placing = e.getItem() != null && e.getItem().getType().isBlock()
                            && (e.getPlayer().isSneaking() || !e.getClickedBlock().getType().isInteractable());
                    if (!placing) {
                        e.setCancelled(true);
                    }
                }
                default -> e.setCancelled(true);
            }
        }
    }

    @EventHandler(priority = EventPriority.LOWEST, ignoreCancelled = true)
    public void onBreakEarly(BlockBreakEvent e) {
        if (role == Role.leaving) {
            e.setCancelled(true);
        }
    }

    @EventHandler(priority = EventPriority.MONITOR, ignoreCancelled = true)
    public void onBreak(BlockBreakEvent e) {
        if (role == Role.frozen) {
            replay.broke(e.getPlayer(), e.getBlock());
        }
    }

    @EventHandler(priority = EventPriority.LOWEST, ignoreCancelled = true)
    public void onPlaceEarly(BlockPlaceEvent e) {
        if (role == Role.leaving) {
            e.setCancelled(true);
        }
    }

    @EventHandler(priority = EventPriority.MONITOR, ignoreCancelled = true)
    public void onPlace(BlockPlaceEvent e) {
        if (role == Role.frozen) {
            replay.placed(e.getPlayer(), e.getBlockPlaced(), e.getHand());
        }
    }

    @EventHandler(priority = EventPriority.LOWEST, ignoreCancelled = true)
    public void onCommand(PlayerCommandPreprocessEvent e) {
        if (handing()) {
            e.setCancelled(true);
            if (role == Role.frozen) {
                replay.ran(e.getPlayer(), e.getMessage());
            }
        }
    }

    @EventHandler(priority = EventPriority.LOWEST, ignoreCancelled = true)
    public void onInteractEntity(PlayerInteractEntityEvent e) {
        stop(e);
    }

    @EventHandler(priority = EventPriority.LOWEST, ignoreCancelled = true)
    public void onDrop(PlayerDropItemEvent e) {
        stop(e);
    }

    @EventHandler(priority = EventPriority.LOWEST, ignoreCancelled = true)
    public void onClick(InventoryClickEvent e) {
        stop(e);
    }

    @EventHandler(priority = EventPriority.LOWEST, ignoreCancelled = true)
    public void onHit(EntityDamageByEntityEvent e) {
        stop(e);
    }

    /** Only a leader takes players: anyone else's world isn't the real one yet. */
    @EventHandler(priority = EventPriority.LOWEST)
    public void onLogin(AsyncPlayerPreLoginEvent e) {
        if (role != Role.leader) {
            e.disallow(AsyncPlayerPreLoginEvent.Result.KICK_OTHER,
                    net.kyori.adventure.text.Component.text("This server isn't taking players yet."));
        }
    }
}
