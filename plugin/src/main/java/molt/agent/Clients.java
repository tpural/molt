package molt.agent;

import java.lang.reflect.Field;
import java.lang.reflect.Modifier;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.atomic.AtomicInteger;
import net.minecraft.network.protocol.game.ClientboundRemoveEntitiesPacket;
import net.minecraft.network.protocol.game.ClientboundSetObjectivePacket;
import net.minecraft.network.protocol.game.ClientboundSetPlayerTeamPacket;
import net.minecraft.server.level.ServerPlayer;
import net.minecraft.world.scores.Objective;
import net.minecraft.world.scores.PlayerTeam;
import net.minecraft.world.scores.Scoreboard;
import org.bukkit.Bukkit;
import org.bukkit.World;
import org.bukkit.boss.KeyedBossBar;
import org.bukkit.craftbukkit.entity.CraftPlayer;
import org.bukkit.craftbukkit.scoreboard.CraftScoreboard;
import org.bukkit.entity.Entity;
import org.bukkit.entity.Player;
import org.bukkit.plugin.Plugin;

/**
 * What a client was shown, for a switch the client doesn't notice. The new
 * server gives each player the ID it had, with every other entity numbered
 * above anything the old server used; the old one tells each client to forget
 * what the new one will show it again.
 */
final class Clients {
    private static final AtomicInteger COUNTER;

    static {
        // Entity has it up to 26.1, ServerLevel from 26.3.
        AtomicInteger found = null;
        for (Class<?> c : List.of(net.minecraft.server.level.ServerLevel.class, net.minecraft.world.entity.Entity.class)) {
            try {
                Field f = c.getDeclaredField("ENTITY_COUNTER");
                if (Modifier.isStatic(f.getModifiers()) && f.getType() == AtomicInteger.class) {
                    f.setAccessible(true);
                    found = (AtomicInteger) f.get(null);
                    break;
                }
            } catch (NoSuchFieldException e) {
                // the other one
            } catch (IllegalAccessException e) {
                throw new ExceptionInInitializerError(e);
            }
        }
        if (found == null) {
            throw new ExceptionInInitializerError("no entity counter on ServerLevel or Entity");
        }
        COUNTER = found;
    }

    /** Room for the entities the old server makes between its freeze and the new one's takeover. */
    private static final int MARGIN = 1 << 20;

    private Clients() {}

    /** The next entity ID this server gives out. */
    static int counter() {
        return COUNTER.get();
    }

    /** Number every entity from now on above the old server's. */
    static void above(int old) {
        COUNTER.accumulateAndGet(old + MARGIN, Math::max);
    }

    /** Give a player, not yet in the world, the ID it had on the old server. */
    static void keepId(Player p, int id) {
        ((CraftPlayer) p).getHandle().setId(id);
    }

    /** Tell a player's client to forget the entities, scoreboard and boss bars this server showed it. */
    static void forget(Player p) {
        ServerPlayer sp = ((CraftPlayer) p).getHandle();
        List<Integer> ids = new ArrayList<>();
        for (World w : Bukkit.getWorlds()) {
            for (Entity e : w.getEntities()) {
                if (e != p && e.getTrackedBy().contains(p)) {
                    ids.add(e.getEntityId());
                }
            }
        }
        if (!ids.isEmpty()) {
            sp.connection.send(new ClientboundRemoveEntitiesPacket(ids.stream().mapToInt(Integer::intValue).toArray()));
        }
        Scoreboard board = ((CraftScoreboard) p.getScoreboard()).getHandle();
        for (Objective o : board.getObjectives()) {
            sp.connection.send(new ClientboundSetObjectivePacket(o, ClientboundSetObjectivePacket.METHOD_REMOVE));
        }
        for (PlayerTeam t : board.getPlayerTeams()) {
            sp.connection.send(ClientboundSetPlayerTeamPacket.createRemovePacket(t));
        }
        for (var it = Bukkit.getBossBars(); it.hasNext(); ) {
            KeyedBossBar bar = it.next();
            if (bar.getPlayers().contains(p)) {
                bar.removePlayer(p);
            }
        }
    }

    /** Show a player again what forget made its client forget: the swap didn't happen. */
    static void remind(Plugin plugin, Player p) {
        for (World w : Bukkit.getWorlds()) {
            for (Entity e : w.getEntities()) {
                if (e != p && e.getTrackedBy().contains(p)) {
                    p.hideEntity(plugin, e);
                    p.showEntity(plugin, e);
                }
            }
        }
        var board = p.getScoreboard();
        p.setScoreboard(Bukkit.getScoreboardManager().getNewScoreboard());
        p.setScoreboard(board);
    }
}
