package molt.agent;

import com.google.gson.Gson;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import java.io.IOException;
import java.io.InputStreamReader;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.Map;
import java.util.concurrent.Executors;
import java.util.logging.Level;
import org.bukkit.plugin.Plugin;

/** molt's way in: a small HTTP API, for callers with the token. */
final class Api {
    @FunctionalInterface
    interface Call {
        Object run(HttpExchange ex) throws Exception;
    }

    private static final Gson GSON = new Gson();

    private final HttpServer http;
    private final Plugin plugin;
    private final byte[] auth;

    Api(Plugin plugin, Handoff h, String token, int port) throws IOException {
        this.plugin = plugin;
        this.auth = ("Bearer " + token).getBytes(StandardCharsets.UTF_8);
        http = HttpServer.create(new InetSocketAddress(port), 0);
        http.setExecutor(Executors.newFixedThreadPool(4));
        route("GET", "/state", ex -> h.state());
        route("POST", "/snapshot/begin", ex -> {
            h.snapshotBegin();
            return null;
        });
        route("POST", "/snapshot/end", ex -> {
            h.snapshotEnd();
            return null;
        });
        route("POST", "/follow", ex -> {
            h.follow();
            return null;
        });
        route("POST", "/freeze", ex -> h.freeze());
        route("POST", "/takeover", ex -> {
            h.takeover(GSON.fromJson(new InputStreamReader(ex.getRequestBody(), StandardCharsets.UTF_8), Handoff.Freeze.class));
            return null;
        });
        route("POST", "/thaw", ex -> {
            h.thaw();
            return null;
        });
        http.start();
    }

    void stop() {
        http.stop(0);
    }

    private void route(String method, String path, Call call) {
        http.createContext(path, ex -> {
            try {
                if (!ex.getRequestURI().getPath().equals(path)) {
                    reply(ex, 404, Map.of("error", "not found"));
                } else if (!MessageDigest.isEqual(auth, String.valueOf(ex.getRequestHeaders().getFirst("Authorization")).getBytes(StandardCharsets.UTF_8))) {
                    reply(ex, 401, Map.of("error", "unauthorized"));
                } else if (!ex.getRequestMethod().equals(method)) {
                    reply(ex, 405, Map.of("error", "use " + method));
                } else {
                    Object out = call.run(ex);
                    reply(ex, 200, out == null ? Map.of() : out);
                }
            } catch (IllegalStateException e) {
                reply(ex, 409, Map.of("error", e.getMessage()));
            } catch (Throwable e) {
                plugin.getLogger().log(Level.WARNING, path, e);
                reply(ex, 500, Map.of("error", String.valueOf(e.getMessage())));
            } finally {
                ex.close();
            }
        });
    }

    private static void reply(HttpExchange ex, int code, Object body) {
        try {
            byte[] b = GSON.toJson(body).getBytes(StandardCharsets.UTF_8);
            ex.getResponseHeaders().set("Content-Type", "application/json");
            ex.sendResponseHeaders(code, b.length);
            ex.getResponseBody().write(b);
        } catch (IOException ignored) {
            // the caller went away
        }
    }
}
