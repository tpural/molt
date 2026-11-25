package molt.agent;

import com.destroystokyo.paper.event.server.ServerTickEndEvent;
import java.util.ArrayList;
import java.util.List;
import org.bukkit.Bukkit;
import org.bukkit.event.EventHandler;
import org.bukkit.event.HandlerList;
import org.bukkit.event.Listener;
import org.bukkit.plugin.Plugin;

/** Logs how the first ticks after a takeover went: the slow ones are what players wait for. */
final class Ticks implements Listener {
    private final Plugin plugin;
    private final long until;
    private final long start = System.nanoTime();
    private final List<String> slow = new ArrayList<>();
    private int ticks;

    private Ticks(Plugin plugin, int seconds) {
        this.plugin = plugin;
        this.until = start + seconds * 1_000_000_000L;
    }

    static void watch(Plugin plugin, int seconds) {
        Bukkit.getPluginManager().registerEvents(new Ticks(plugin, seconds), plugin);
    }

    @EventHandler
    public void onTick(ServerTickEndEvent e) {
        ticks++;
        if (e.getTickDuration() > 50) {
            slow.add(String.format("%d@%dms:%.0fms", ticks, (System.nanoTime() - start) / 1_000_000, e.getTickDuration()));
        }
        if (System.nanoTime() > until) {
            HandlerList.unregisterAll(this);
            plugin.getLogger().info("first " + ticks + " ticks after taking over; slow: " + (slow.isEmpty() ? "none" : String.join(" ", slow)));
        }
    }
}
