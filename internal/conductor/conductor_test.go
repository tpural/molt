package conductor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tpural/molt/internal/agent"
	"github.com/tpural/molt/internal/runner"
	"github.com/tpural/molt/internal/worlds"
)

// world is what every fake backend's agent and the fake runner share.
type world struct {
	mu       sync.Mutex
	did      []string
	running  map[int]runner.Backend
	failAt   string // a call that fails, like "start" or "2 takeover"
	store    worlds.Store
	moveFail bool
	leader   int
}

func (w *world) log(s string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.did = append(w.did, s)
	if s == w.failAt {
		return errors.New("boom")
	}
	return nil
}

func (w *world) took() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := strings.Join(w.did, "; ")
	w.did = nil
	return s
}

type fakeRunner struct{ w *world }

func (r fakeRunner) Start(_ context.Context, gen int, _ string, spec runner.Spec) (runner.Backend, error) {
	if err := r.w.log(fmt.Sprintf("start %d %s", gen, spec.Image)); err != nil {
		return runner.Backend{}, err
	}
	if spec.Env["MOLT_TOKEN"] != "t" || spec.Env["ONLINE_MODE"] != "FALSE" {
		return runner.Backend{}, fmt.Errorf("env %v", spec.Env)
	}
	b := runner.Backend{Gen: gen, Name: runner.Name("m", gen)}
	r.w.mu.Lock()
	r.w.running[gen] = b
	r.w.mu.Unlock()
	return b, nil
}

func (r fakeRunner) Stop(_ context.Context, b runner.Backend) error {
	r.w.mu.Lock()
	delete(r.w.running, b.Gen)
	r.w.mu.Unlock()
	return r.w.log(fmt.Sprintf("stop %d", b.Gen))
}

func (r fakeRunner) Find(_ context.Context, gen int) (runner.Backend, bool, error) {
	r.w.mu.Lock()
	defer r.w.mu.Unlock()
	b, ok := r.w.running[gen]
	return b, ok, nil
}

type fakeAgent struct {
	w   *world
	gen int
}

func (a fakeAgent) do(s string) error { return a.w.log(fmt.Sprintf("%d %s", a.gen, s)) }

func (a fakeAgent) State(context.Context) (agent.State, error) {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	role := "standby"
	if a.w.leader == a.gen {
		role = "leader"
	}
	return agent.State{Role: role, Ready: true, Folders: []string{"world"}}, nil
}
func (a fakeAgent) SnapshotBegin(context.Context) error { return a.do("snapshot") }
func (a fakeAgent) SnapshotEnd(context.Context) error   { return a.do("snapshot end") }
func (a fakeAgent) Follow(context.Context) error        { return a.do("follow") }
func (a fakeAgent) Thaw(context.Context) error          { return a.do("thaw") }

func (a fakeAgent) Handoff(context.Context) (json.RawMessage, error) {
	if err := a.do("handoff"); err != nil {
		return nil, fmt.Errorf("%w", agent.ErrUnsupported)
	}
	return json.RawMessage(`{"players":[]}`), nil
}

func (a fakeAgent) Arrive(_ context.Context, h json.RawMessage) error {
	return a.do("arrive " + string(h))
}

// Freeze saves a chunk that changed after the snapshot.
func (a fakeAgent) Freeze(context.Context) (agent.Freeze, error) {
	_ = os.WriteFile(filepath.Join(a.w.store.Dir(a.gen), "world/r.0.0.mca"), []byte("after"), 0o644)
	return agent.Freeze{Folders: []string{"world"}, Players: []agent.Player{{Name: "tim"}}}, a.do("freeze")
}

func (a fakeAgent) Takeover(_ context.Context, f agent.Freeze) error {
	if err := a.do(fmt.Sprintf("takeover %d players", len(f.Players))); err != nil {
		return err
	}
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	a.w.leader = a.gen
	return nil
}

type fakeProxy struct{ w *world }

func (p fakeProxy) Serve(b runner.Backend) error { return p.w.log(fmt.Sprintf("serve %d", b.Gen)) }
func (p fakeProxy) Hold()                        { _ = p.w.log("hold") }
func (p fakeProxy) Release()                     { _ = p.w.log("release") }
func (p fakeProxy) Drop(b runner.Backend)        { _ = p.w.log(fmt.Sprintf("drop %d", b.Gen)) }
func (p fakeProxy) Disconnect(b runner.Backend, _ string) int {
	_ = p.w.log(fmt.Sprintf("disconnect %d", b.Gen))
	return 0
}

func (p fakeProxy) Move(_ context.Context, seamless bool) (int, int, []string) {
	_ = p.w.log("move")
	if p.w.moveFail {
		return 0, 0, []string{"tim: no"}
	}
	if seamless {
		return 1, 1, nil
	}
	return 1, 0, nil
}

func setup(t *testing.T) (*Conductor, *world) {
	t.Helper()
	w := &world{running: map[int]runner.Backend{}, store: worlds.Store{Root: t.TempDir()}}
	c := &Conductor{
		Config: Config{Store: w.store, Secret: "s", Token: "t", Keep: 2},
		Runner: fakeRunner{w}, Proxy: fakeProxy{w},
		Agent: func(b runner.Backend) Agent { return fakeAgent{w, b.Gen} },
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := c.Start(context.Background(), runner.Spec{Image: "paper:1"}); err != nil {
		t.Fatal(err)
	}
	if got := w.took(); got != "start 1 paper:1; 1 takeover 0 players; serve 1" {
		t.Fatalf("first start: %s", got)
	}
	if err := os.MkdirAll(filepath.Join(w.store.Dir(1), "world"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.store.Dir(1), "world/r.0.0.mca"), []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	return c, w
}

func TestStartAdopts(t *testing.T) {
	c, w := setup(t)
	again := &Conductor{Config: c.Config, Runner: c.Runner, Proxy: c.Proxy, Agent: c.Agent, Log: c.Log}
	if err := again.Start(context.Background(), runner.Spec{Image: "ignored"}); err != nil {
		t.Fatal(err)
	}
	if got := w.took(); got != "serve 1" || again.Status().Active.Spec.Image != "paper:1" {
		t.Errorf("adopting: %s, %+v", got, again.Status().Active)
	}
}

func TestSwap(t *testing.T) {
	c, w := setup(t)
	r, err := c.Swap(context.Background(), runner.Spec{Image: "paper:2"})
	if err != nil {
		t.Fatal(err)
	}
	want := "1 snapshot; 1 snapshot end; start 2 paper:2; 2 follow; 1 snapshot; 1 snapshot end; 1 freeze; 2 takeover 1 players; 1 handoff; 2 arrive {\"players\":[]}; hold; serve 2; move; release; drop 1; stop 1"
	if got := w.took(); got != want {
		t.Errorf("did\n %s\nwant\n %s", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(w.store.Dir(2), "world/r.0.0.mca")); string(b) != "after" {
		t.Errorf("the new world has %q", b)
	}
	if a := c.Status().Active; a.Backend.Gen != 2 || a.Spec.Image != "paper:2" || r.Moved != 1 || r.Error != "" {
		t.Errorf("active %+v, report %+v", a, r)
	}
	var names []string
	for _, s := range r.Steps {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"snapshot", "start", "presync", "freeze", "sync", "takeover", "handoff", "move", "retire"}) {
		t.Errorf("steps %v", names)
	}
	// The next swap is generation 3, and only two generations are kept.
	if _, err := c.Swap(context.Background(), runner.Spec{Image: "paper:3"}); err != nil {
		t.Fatal(err)
	}
	if gens, _ := w.store.Generations(); !slices.Equal(gens, []int{2, 3}) {
		t.Errorf("generations %v", gens)
	}
}

func TestSwapFailures(t *testing.T) {
	for _, tc := range []struct {
		failAt, want string
	}{
		// Before the freeze: the old backend never noticed.
		{"start 2 paper:2", "1 snapshot; 1 snapshot end; start 2 paper:2"},
		{"2 follow", "1 snapshot; 1 snapshot end; start 2 paper:2; 2 follow; stop 2"},
		// Frozen: it's thawed.
		{"1 freeze", "1 snapshot; 1 snapshot end; start 2 paper:2; 2 follow; 1 snapshot; 1 snapshot end; 1 freeze; 1 thaw; stop 2"},
		{"2 takeover 1 players", "1 snapshot; 1 snapshot end; start 2 paper:2; 2 follow; 1 snapshot; 1 snapshot end; 1 freeze; 2 takeover 1 players; 1 thaw; stop 2"},
		// Nobody could move: players stay on the old backend.
		{"move", "1 snapshot; 1 snapshot end; start 2 paper:2; 2 follow; 1 snapshot; 1 snapshot end; 1 freeze; 2 takeover 1 players; 1 handoff; 2 arrive {\"players\":[]}; hold; serve 2; move; move; move; drop 2; serve 1; release; 1 thaw; stop 2"},
	} {
		t.Run(tc.failAt, func(t *testing.T) {
			c, w := setup(t)
			w.failAt, w.moveFail = tc.failAt, tc.failAt == "move"
			if _, err := c.Swap(context.Background(), runner.Spec{Image: "paper:2"}); err == nil {
				t.Fatal("no error")
			}
			if got := w.took(); got != tc.want {
				t.Errorf("did\n %s\nwant\n %s", got, tc.want)
			}
			if a := c.Status().Active; a.Backend.Gen != 1 {
				t.Errorf("active gen %d", a.Backend.Gen)
			}
			if _, err := os.Stat(w.store.Dir(2)); err == nil {
				t.Error("left the new generation's directory")
			}
			if c.Status().Last.Error == "" {
				t.Error("the report has no error")
			}
		})
	}
}

// An old backend that can't hand off still swaps: players arrive where they froze.
func TestSwapWithoutHandoff(t *testing.T) {
	c, w := setup(t)
	w.failAt = "1 handoff"
	if _, err := c.Swap(context.Background(), runner.Spec{Image: "paper:2"}); err != nil {
		t.Fatal(err)
	}
	if got := w.took(); !strings.Contains(got, "1 handoff; hold; serve 2; move") || c.Status().Active.Backend.Gen != 2 {
		t.Errorf("did %s", got)
	}
}

func TestOneSwapAtATime(t *testing.T) {
	c, _ := setup(t)
	c.lock.Lock()
	defer c.lock.Unlock()
	if _, err := c.Swap(context.Background(), runner.Spec{}); !errors.Is(err, ErrBusy) {
		t.Errorf("err %v", err)
	}
	if err := c.Begin(context.Background(), runner.Spec{}); !errors.Is(err, ErrBusy) {
		t.Errorf("begin: %v", err)
	}
}

func TestChange(t *testing.T) {
	s := runner.Spec{Image: "a", Env: map[string]string{"VERSION": "26.2", "PLUGINS": "x"}}
	got := Change{Env: map[string]string{"VERSION": "26.3"}, Unset: []string{"PLUGINS"}}.Apply(s)
	if got.Image != "a" || len(got.Env) != 1 || got.Env["VERSION"] != "26.3" || s.Env["VERSION"] != "26.2" {
		t.Errorf("got %+v, from %+v", got, s)
	}
}
