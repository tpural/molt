package molt.agent;

import java.io.IOException;
import java.util.logging.Level;
import org.bukkit.Bukkit;
import org.bukkit.event.EventHandler;
import org.bukkit.event.Listener;
import org.bukkit.event.server.ServerLoadEvent;
import org.bukkit.plugin.java.JavaPlugin;

/**
 * molt-agent lets molt hand a running world from this server to the next: it
 * snapshots and freezes the leader, and makes a follower take over.
 */
public final class MoltAgent extends JavaPlugin implements Listener {
    private Api api;
    private Handoff handoff;

    @Override
    public void onEnable() {
        String token = System.getenv("MOLT_TOKEN");
        if (token == null || token.isBlank()) {
            getLogger().severe("$MOLT_TOKEN is empty: molt can't reach this server");
            Bukkit.getPluginManager().disablePlugin(this);
            return;
        }
        int port = Integer.parseInt(System.getenv().getOrDefault("MOLT_AGENT_PORT", "25580"));
        handoff = new Handoff(this);
        Bukkit.getPluginManager().registerEvents(this, this);
        Bukkit.getPluginManager().registerEvents(handoff, this);
        Bukkit.getPluginManager().registerEvents(handoff.replay(), this);
        try {
            api = new Api(this, handoff, token, port);
        } catch (IOException e) {
            getLogger().log(Level.SEVERE, "listening on " + port, e);
            Bukkit.getPluginManager().disablePlugin(this);
        }
    }

    @Override
    public void onDisable() {
        if (api != null) {
            api.stop();
        }
    }

    @EventHandler
    public void onLoad(ServerLoadEvent e) {
        if (e.getType() == ServerLoadEvent.LoadType.STARTUP) {
            handoff.ready();
        }
    }
}
