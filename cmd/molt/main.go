// molt is a Minecraft server that updates without restarting: an always-on
// proxy in front of Paper backends, and a conductor that swaps a backend for
// a new one (other plugins, another version) while players stay connected.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	"github.com/tpural/molt/internal/agent"
	"github.com/tpural/molt/internal/conductor"
	"github.com/tpural/molt/internal/proxy"
	"github.com/tpural/molt/internal/runner"
	"github.com/tpural/molt/internal/worlds"
)

var version = "dev"

// Config is molt's configuration file. Secrets come from the environment:
// MOLT_SECRET (forwarding, shared with backends), MOLT_AGENT_TOKEN and
// MOLT_API_TOKEN.
type Config struct {
	// Bind is where players connect; API is where /status and /swap are.
	Bind string `json:"bind"`
	API  string `json:"api"`
	// OnlineMode checks players with Mojang (default true).
	OnlineMode *bool  `json:"onlineMode"`
	Motd       string `json:"motd"`
	// Root holds each generation's server directory.
	Root string `json:"root"`
	// Prefix names backends.
	Prefix string `json:"prefix"`
	// Runner is "kubernetes" or "docker".
	Runner     string `json:"runner"`
	Kubernetes struct {
		Namespace string `json:"namespace"`
		// Node defaults to $NODE_NAME.
		Node   string `json:"node"`
		Memory string `json:"memory"`
		UID    int64  `json:"uid"`
	} `json:"kubernetes"`
	Docker struct {
		Network string `json:"network"`
	} `json:"docker"`
	// Spec is what the first backend runs; later ones come from swaps.
	Spec runner.Spec `json:"spec"`
	// Ready is how long a backend may take to start.
	Ready string `json:"ready"`
	// Keep is how many generations stay on disk.
	Keep int `json:"keep"`
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var err error
	switch cmd := arg(1); cmd {
	case "", "serve":
		err = serve(log)
	case "swap", "status":
		err = client(cmd, os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("unknown command %q: serve, swap, status or version", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "molt:", err)
		os.Exit(1)
	}
}

func arg(i int) string {
	if len(os.Args) > i && !strings.HasPrefix(os.Args[i], "-") {
		return os.Args[i]
	}
	return ""
}

func load(path string) (Config, error) {
	c := Config{Bind: ":25565", API: ":8080", Root: "/var/lib/molt", Prefix: "molt", Runner: "kubernetes", Keep: 3}
	c.Kubernetes.Memory, c.Kubernetes.UID = "4Gi", 1000
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return c, err
	}
	if c.Spec.Image == "" {
		return c, errors.New("spec.image is required")
	}
	return c, nil
}

func serve(log *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := fs.String("config", "/etc/molt/config.yaml", "configuration file")
	args := os.Args[1:]
	if arg(1) == "serve" {
		args = os.Args[2:]
	}
	_ = fs.Parse(args)
	cfg, err := load(*path)
	if err != nil {
		return err
	}
	env := map[string]string{}
	for _, k := range []string{"MOLT_SECRET", "MOLT_AGENT_TOKEN", "MOLT_API_TOKEN"} {
		if env[k] = os.Getenv(k); env[k] == "" {
			return fmt.Errorf("$%s is required", k)
		}
	}
	ready := 10 * time.Minute
	if cfg.Ready != "" {
		if ready, err = time.ParseDuration(cfg.Ready); err != nil {
			return fmt.Errorf("ready: %w", err)
		}
	}
	run, err := newRunner(cfg)
	if err != nil {
		return err
	}

	online := cfg.OnlineMode == nil || *cfg.OnlineMode
	px, err := proxy.New(proxy.Config{Bind: cfg.Bind, OnlineMode: online, Secret: env["MOLT_SECRET"], Motd: cfg.Motd}, logr.FromSlogHandler(log.Handler()))
	if err != nil {
		return err
	}
	c := &conductor.Conductor{
		Config: conductor.Config{
			Store: worlds.Store{Root: cfg.Root}, Secret: env["MOLT_SECRET"], Token: env["MOLT_AGENT_TOKEN"],
			Ready: ready, Keep: cfg.Keep,
		},
		Runner: run, Proxy: px, Log: log,
		Agent: func(b runner.Backend) conductor.Agent { return agent.New(b.Agent, env["MOLT_AGENT_TOKEN"]) },
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 2)
	go func() { errc <- px.Start(ctx) }()
	srv := &http.Server{Addr: cfg.API, Handler: c.Handler(ctx, env["MOLT_API_TOKEN"]), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	log.Info("molt", "version", version, "bind", cfg.Bind, "api", cfg.API, "runner", cfg.Runner)
	if err := c.Start(ctx, cfg.Spec); err != nil {
		return fmt.Errorf("starting the backend: %w", err)
	}
	log.Info("serving", "gen", c.Status().Active.Backend.Gen)
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	// Backends keep running: a new molt adopts the active one.
	shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shut)
}

func newRunner(cfg Config) (runner.Runner, error) {
	switch cfg.Runner {
	case "docker":
		return &runner.Docker{Prefix: cfg.Prefix, Network: cfg.Docker.Network}, nil
	case "kubernetes":
		rc, err := rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
		k, err := kubernetes.NewForConfig(rc)
		if err != nil {
			return nil, err
		}
		mem, err := resource.ParseQuantity(cfg.Kubernetes.Memory)
		if err != nil {
			return nil, fmt.Errorf("kubernetes.memory: %w", err)
		}
		ns, node := cfg.Kubernetes.Namespace, cfg.Kubernetes.Node
		if ns == "" {
			b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
			if err != nil {
				return nil, err
			}
			ns = strings.TrimSpace(string(b))
		}
		if node == "" {
			if node = os.Getenv("NODE_NAME"); node == "" {
				return nil, errors.New("kubernetes.node or $NODE_NAME is required")
			}
		}
		return &runner.Kubernetes{Client: k, Namespace: ns, Prefix: cfg.Prefix, Node: node, Memory: mem, UID: cfg.Kubernetes.UID}, nil
	}
	return nil, fmt.Errorf("runner %q: kubernetes or docker", cfg.Runner)
}

// client runs `molt swap` and `molt status` against a running molt.
func client(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	url := fs.String("url", envOr("MOLT_URL", "http://localhost:8080"), "molt's API ($MOLT_URL)")
	image := fs.String("image", "", "new image")
	var set, unset multi
	fs.Var(&set, "env", "set KEY=value (repeatable)")
	fs.Var(&unset, "unset", "remove KEY (repeatable)")
	wait := fs.Bool("wait", true, "wait for the swap to finish")
	_ = fs.Parse(args)
	token := os.Getenv("MOLT_API_TOKEN")
	do := func(method, path string, body any, out any) error {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, strings.TrimRight(*url, "/")+path, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		if out != nil {
			return json.NewDecoder(resp.Body).Decode(out)
		}
		return nil
	}
	var st conductor.Status
	if cmd == "status" {
		if err := do(http.MethodGet, "/status", nil, &st); err != nil {
			return err
		}
		return show(st)
	}
	ch := conductor.Change{Image: *image, Unset: unset, Env: map[string]string{}}
	for _, kv := range set {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("-env %q: want KEY=value", kv)
		}
		ch.Env[k] = v
	}
	if err := do(http.MethodPost, "/swap", ch, nil); err != nil {
		return err
	}
	if !*wait {
		return nil
	}
	last := ""
	for {
		time.Sleep(time.Second)
		if err := do(http.MethodGet, "/status", nil, &st); err != nil {
			return err
		}
		if st.Swapping != last && st.Swapping != "" {
			fmt.Fprintln(os.Stderr, "…", st.Swapping)
			last = st.Swapping
		}
		if st.Swapping == "" {
			if err := show(st); err != nil {
				return err
			}
			if st.Last != nil && st.Last.Error != "" {
				return errors.New(st.Last.Error)
			}
			return nil
		}
	}
}

func show(st conductor.Status) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }
