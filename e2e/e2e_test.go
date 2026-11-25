//go:build e2e

// The end-to-end test: a real proxy, real Paper backends in Docker, and a bot
// playing through a swap. It needs docker, node, and the backend image
// (make e2e builds it).
package e2e

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/tpural/molt/internal/agent"
	"github.com/tpural/molt/internal/conductor"
	"github.com/tpural/molt/internal/proxy"
	"github.com/tpural/molt/internal/runner"
	"github.com/tpural/molt/internal/worlds"
)

// The bot speaks 26.1, so the backends run it too.
const (
	paper  = "26.1.2"
	chunky = "https://cdn.modrinth.com/data/fALzjamp/versions/RBuKjN4k/Chunky-Paper-1.5.66.jar"
)

type event struct {
	Event   string    `json:"event"`
	Cmd     string    `json:"cmd"`
	Why     string    `json:"why"`
	Pos     []float64 `json:"pos"`
	Diamond int       `json:"diamond"`
	Iron    int       `json:"iron"`
	Gold    string    `json:"gold"`
	Above   string    `json:"above"`
	Chest   string    `json:"chest"`
	Pig     bool      `json:"pig"`
	Time    int64     `json:"time"`
}

type bot struct {
	t      *testing.T
	in     io.Writer
	events chan event
}

func startBot(t *testing.T, addr string) *bot {
	t.Helper()
	cmd := exec.Command("node", "bot/bot.js", addr)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	b := &bot{t: t, in: in, events: make(chan event, 100)}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			var e event
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				t.Logf("bot: %s", sc.Text())
				b.events <- e
			}
		}
		close(b.events)
	}()
	return b
}

// await waits for an event, failing on the bot leaving.
func (b *bot) await(name string, within time.Duration) event {
	b.t.Helper()
	deadline := time.After(within)
	for {
		select {
		case e, ok := <-b.events:
			if !ok || e.Event == "end" {
				b.t.Fatalf("the bot was disconnected waiting for %s: %s", name, e.Why)
			}
			if e.Event == "error" {
				b.t.Fatalf("the bot: %s: %s", e.Cmd, e.Why)
			}
			if e.Event == name {
				return e
			}
		case <-deadline:
			b.t.Fatalf("no %s from the bot in %s", name, within)
		}
	}
}

func (b *bot) do(cmd string) event {
	b.t.Helper()
	if _, err := fmt.Fprintln(b.in, cmd); err != nil {
		b.t.Fatal(err)
	}
	if cmd == "check" {
		return b.await("check", time.Minute)
	}
	return b.await("done", time.Minute)
}

// offlineUUID is a name's UUID when nobody checks it with Mojang.
func offlineUUID(name string) string {
	h := md5.Sum([]byte("OfflinePlayer:" + name)) //nolint:gosec // that's how Minecraft makes it
	h[6] = h[6]&0x0f | 0x30
	h[8] = h[8]&0x3f | 0x80
	x := hex.EncodeToString(h[:])
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}

func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Minute); !ok(); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("waited 10 minutes for %s", what)
		}
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestSwapWhilePlaying(t *testing.T) {
	image := os.Getenv("MOLT_E2E_IMAGE")
	if image == "" {
		image = "molt-paper:dev"
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	addr := freePort(t)
	px, err := proxy.New(proxy.Config{Bind: addr, Secret: "e2e-secret"}, logr.FromSlogHandler(log.Handler()))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = px.Start(ctx) }()

	root := t.TempDir() // before the cleanup below, so removed after the backends
	stopped := &strings.Builder{}
	prefix := fmt.Sprintf("molt-e2e-%d", time.Now().Unix())
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("stopped backends:\n%s", stopped)
			tree, _ := exec.Command("sh", "-c", "cd "+root+" && find . -type f -newer . -o -type f -path '*player*' | grep -v -e /libraries/ -e /versions/ -e /cache/ -e /logs/ | sort | xargs ls -la --time-style=+%T | awk '{print $6, $5, $7}'").Output()
			t.Logf("files:\n%s", tree)
			gen2, _ := exec.Command("sh", "-c", "cd "+root+" && find gen-2/world -type f | xargs ls -la --time-style=+%T | awk '{print $6, $5, $7}'").Output()
			t.Logf("gen-2:\n%s", gen2)
		}
		out, _ := exec.Command("docker", "ps", "-aq", "--filter", "name=^"+prefix).Output()
		for _, id := range strings.Fields(string(out)) {
			if t.Failed() {
				logs, _ := exec.Command("docker", "logs", "--tail", "80", id).CombinedOutput()
				t.Logf("backend %s:\n%s", id, logs)
			}
			_ = exec.Command("docker", "rm", "-f", id).Run()
		}
	})
	c := &conductor.Conductor{
		Config: conductor.Config{Store: worlds.Store{Root: root}, Secret: "e2e-secret", Token: "e2e-agent", Ready: 8 * time.Minute, Keep: 3},
		Runner: &runner.Docker{Prefix: prefix, Logs: stopped}, Proxy: px, Log: log,
		Agent: func(b runner.Backend) conductor.Agent { return agent.New(b.Agent, "e2e-agent") },
	}
	// The bot is op. Behind an offline proxy its UUID is the offline one.
	if err := os.MkdirAll(c.Store.Dir(1), 0o755); err != nil {
		t.Fatal(err)
	}
	ops := fmt.Sprintf(`[{"uuid":%q,"name":"molty","level":4,"bypassesPlayerLimit":false}]`, offlineUUID("molty"))
	if err := os.WriteFile(filepath.Join(c.Store.Dir(1), "ops.json"), []byte(ops), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := runner.Spec{Image: image, Env: map[string]string{"VERSION": paper, "MEMORY": "2G", "DIFFICULTY": "peaceful"}}
	if err := c.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}

	b := startBot(t, addr)
	b.await("ready", 2*time.Minute)
	b.do("setup")

	// A swap that can't start leaves the bot playing where it was.
	broken := conductor.Change{Image: "molt-paper:does-not-exist"}.Apply(spec)
	if _, err := c.Swap(ctx, broken); err == nil {
		t.Fatal("a swap to a missing image worked")
	}
	if got := c.Status().Active.Backend.Gen; got != 1 {
		t.Fatalf("active generation %d after a failed swap", got)
	}
	b.do("check")

	// A swap that adds a plugin, while the bot keeps changing things.
	next := conductor.Change{Env: map[string]string{"PLUGINS": chunky}}.Apply(spec)
	if err := c.Begin(ctx, next); err != nil {
		t.Fatal(err)
	}
	until(t, "the new server to start", func() bool {
		if st := c.Status(); st.Swapping == "" && st.Last != nil && st.Last.Error != "" {
			t.Fatalf("swap: %s", st.Last.Error)
		}
		return c.Status().Swapping == "start"
	})
	want := b.do("change")
	changed := time.Now()
	until(t, "the swap to finish", func() bool { return c.Status().Swapping == "" })
	r := c.Status().Last
	t.Logf("swap: %+v", *r)
	if r.Error != "" || r.Moved != 1 {
		t.Fatalf("swap: %+v", *r)
	}
	if r.Freeze > 5*time.Second {
		t.Errorf("froze for %s", r.Freeze)
	}

	// Let the bot settle on the new server, then look.
	time.Sleep(3 * time.Second)
	got := b.do("check")
	if d := math.Hypot(got.Pos[0]-want.Pos[0], got.Pos[2]-want.Pos[2]); d > 0.5 || math.Abs(got.Pos[1]-want.Pos[1]) > 0.5 {
		t.Errorf("moved %.2f blocks: %v, was %v", d, got.Pos, want.Pos)
	}
	if got.Diamond != 5 || got.Iron != 3 {
		t.Errorf("inventory: %d diamonds, %d iron", got.Diamond, got.Iron)
	}
	if got.Gold != "air" || got.Above != "diamond_block" || got.Chest != "chest" {
		t.Errorf("blocks: gold spot %s, above %s, chest %s", got.Gold, got.Above, got.Chest)
	}
	if !got.Pig {
		t.Error("the named pig didn't come over")
	}
	// The time carries on from the freeze (the old server played on while the
	// new one started), not from the snapshot before it.
	if lo, hi := want.Time+int64(r.Steps[1].Took.Seconds()*20)-100, want.Time+int64(time.Since(changed).Seconds()*20)+100; got.Time < lo || got.Time > hi {
		t.Errorf("time of day %d, want %d to %d", got.Time, lo, hi)
	}
	st, err := agent.New(c.Status().Active.Backend.Agent, "e2e-agent").State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Role != "leader" || !slices.Contains(st.Plugins, "Chunky") || st.Players != 1 {
		t.Errorf("the new backend: %+v", st)
	}
	_, _ = fmt.Fprintln(b.in, "quit")
}
