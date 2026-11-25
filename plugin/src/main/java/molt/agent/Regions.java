package molt.agent;

import ca.spottedleaf.moonrise.patches.chunk_system.io.MoonriseRegionFileIO;
import java.io.IOException;
import java.lang.reflect.Field;
import java.util.Map;
import net.minecraft.server.level.ServerLevel;
import net.minecraft.world.level.chunk.storage.RegionFileStorage;

/**
 * Region files are kept open, with their headers in memory. Once newer files
 * are copied in, the open ones are stale: forgetting them makes the server
 * open the new ones.
 */
final class Regions {
    private static final Field CACHE;

    static {
        try {
            CACHE = RegionFileStorage.class.getDeclaredField("regionCache");
            CACHE.setAccessible(true);
        } catch (NoSuchFieldException e) {
            throw new ExceptionInInitializerError(e);
        }
    }

    private Regions() {}

    /** Close and forget every open region file of a world: chunks, entities and POIs. */
    static void forget(ServerLevel level) throws IOException {
        for (MoonriseRegionFileIO.RegionFileType type : MoonriseRegionFileIO.RegionFileType.values()) {
            RegionFileStorage storage = MoonriseRegionFileIO.getControllerFor(level, type).getCache();
            synchronized (storage) {
                storage.close();
                try {
                    ((Map<?, ?>) CACHE.get(storage)).clear();
                } catch (IllegalAccessException e) {
                    throw new IOException("forgetting region files", e);
                }
            }
        }
    }
}
