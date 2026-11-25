// Package conductor swaps one backend for the next while players stay
// connected: the new one starts on a copy of the world, the old one freezes
// and hands over what changed, and the proxy moves everyone across.
package conductor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/tpural/molt/internal/agent"
	"github.com/tpural/molt/internal/runner"
	"github.com/tpural/molt/internal/worlds"
)

// Agent is the part of a backend's molt-agent the conductor uses.
type Agent interface {
	State(ctx context.Context) (agent.State, error)
	SnapshotBegin(ctx context.Context) error
	SnapshotEnd(ctx context.Context) error
	Follow(ctx context.Context) error
	Freeze(ctx context.Context) (agent.Freeze, error)
	Takeover(ctx context.Context, f agent.Freeze) error
	Thaw(ctx context.Context) error
	Handoff(ctx context.Context) (json.RawMessage, error)
	Arrive(ctx context.Context, h json.RawMessage) error
}

// Proxy is the part of the proxy the conductor uses.
type Proxy interface {
	Serve(b runner.Backend) error
	Hold()
	Release()
	Move(ctx context.Context) (moved int, failed []string)
	Disconnect(b runner.Backend, why string) int
	Drop(b runner.Backend)
}

// Config is how the conductor runs backends.
type Config struct {
	Store worlds.Store
	// Secret is the proxy's forwarding secret; Token is molt-agent's.
	Secret, Token string
	// Ready is how long a backend may take to start.
	Ready time.Duration
	// Keep is how many generations' directories stay on disk.
	Keep int
	// Carry are files and folders besides the worlds that each generation
	// gets from the last; Carried if unset.
	Carry []string
}

// Carried is what's carried by default: who's op, allowed and banned.
var Carried = []string{"ops.json", "whitelist.json", "banned-players.json", "banned-ips.json", "usercache.json"}

func (c *Conductor) paths(worlds []string) []string {
	carry := c.Carry
	if carry == nil {
		carry = Carried
	}
	return append(slices.Clone(worlds), carry...)
}

// Conductor owns the active backend and swaps it.
type Conductor struct {
	Config
	Runner runner.Runner
	Proxy  Proxy
	Agent  func(runner.Backend) Agent
	Log    *slog.Logger
	Now    func() time.Time
	// Frozen, if set, runs while the old backend is frozen: a test plays then.
	Frozen func()

	lock   sync.Mutex // held while swapping
	mu     sync.Mutex
	active Active
	status Status
}

// Active is the backend players are on, and what it runs.
type Active struct {
	Backend runner.Backend `json:"backend"`
	Spec    runner.Spec    `json:"spec"`
}

// Status is what /status shows.
type Status struct {
	Active   Active  `json:"active"`
	Swapping string  `json:"swapping,omitempty"`
	Last     *Report `json:"last,omitempty"`
}

// Report is how a swap went.
type Report struct {
	From    int           `json:"from"`
	To      int           `json:"to"`
	Started time.Time     `json:"started"`
	Steps   []Step        `json:"steps"`
	Freeze  time.Duration `json:"freeze"`
	Moved   int           `json:"moved"`
	Failed  []string      `json:"failed,omitempty"`
	// Presynced are the files copied while the new backend waited; Synced
	// and Removed those the freeze copied over, and removed.
	Presynced int    `json:"presynced"`
	Synced    int    `json:"synced"`
	Removed   int    `json:"removed"`
	Error     string `json:"error,omitempty"`
}

// Step is one part of a swap.
type Step struct {
	Name string        `json:"name"`
	Took time.Duration `json:"took"`
}

// Status says what's active and how the last swap went.
func (c *Conductor) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.status
	s.Active = c.active
	return s
}

func (c *Conductor) setSwapping(step string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.Swapping = step
}

func (c *Conductor) statePath() string { return filepath.Join(c.Store.Root, "active.json") }

func (c *Conductor) save(a Active) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.statePath()); err != nil {
		return err
	}
	c.mu.Lock()
	c.active = a
	c.mu.Unlock()
	return nil
}

// Start brings up the active backend: the one running from before molt
// restarted, or the last one again if it stopped, or a first one from spec.
func (c *Conductor) Start(ctx context.Context, spec runner.Spec) error {
	if c.Now == nil {
		c.Now = time.Now
	}
	if err := os.MkdirAll(c.Store.Root, 0o750); err != nil {
		return err
	}
	a := Active{Spec: spec, Backend: runner.Backend{Gen: 1}}
	if b, err := os.ReadFile(c.statePath()); err == nil {
		if err := json.Unmarshal(b, &a); err != nil {
			return fmt.Errorf("reading %s: %w", c.statePath(), err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if b, ok, err := c.Runner.Find(ctx, a.Backend.Gen); err != nil {
		return err
	} else if ok {
		c.Log.Info("adopting the running backend", "gen", b.Gen)
		a.Backend = b
		if err := c.Proxy.Serve(b); err != nil {
			return err
		}
		return c.save(a)
	}
	dir := c.Store.Dir(a.Backend.Gen)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if err := worlds.Prepare(dir, c.Secret); err != nil {
		return err
	}
	c.Log.Info("starting a backend", "gen", a.Backend.Gen, "image", a.Spec.Image)
	b, err := c.Runner.Start(ctx, a.Backend.Gen, dir, c.env(a.Spec))
	if err != nil {
		return err
	}
	if err := c.ready(ctx, b); err != nil {
		return err
	}
	if err := c.Agent(b).Takeover(ctx, agent.Freeze{}); err != nil {
		return err
	}
	if err := c.Proxy.Serve(b); err != nil {
		return err
	}
	a.Backend = b
	return c.save(a)
}

// env is what a backend runs with: its spec, and what molt needs of it.
func (c *Conductor) env(spec runner.Spec) runner.Spec {
	e := maps.Clone(spec.Env)
	if e == nil {
		e = map[string]string{}
	}
	e["EULA"] = "TRUE"
	e["ONLINE_MODE"] = "FALSE" // the proxy checks players
	e["MOLT_TOKEN"] = c.Token
	return runner.Spec{Image: spec.Image, Env: e}
}

// ready waits for a backend's server to finish starting.
func (c *Conductor) ready(ctx context.Context, b runner.Backend) error {
	wait := c.Ready
	if wait == 0 {
		wait = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	ag := c.Agent(b)
	for {
		if s, err := ag.State(ctx); err == nil && s.Ready {
			return nil
		}
		if _, ok, err := c.Runner.Find(ctx, b.Gen); err != nil || !ok {
			if err == nil {
				err = errors.New("it's gone")
			}
			return fmt.Errorf("%s stopped while starting: %w", b.Name, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s didn't start in %s", b.Name, wait)
		case <-time.After(time.Second):
		}
	}
}

// ErrBusy is a swap asked for while one is running.
var ErrBusy = errors.New("a swap is already running")

// Swap moves players to a new backend running spec. It returns once the new
// backend is active, or the old one is playing again.
func (c *Conductor) Swap(ctx context.Context, spec runner.Spec) (Report, error) {
	if !c.lock.TryLock() {
		return Report{}, ErrBusy
	}
	defer c.lock.Unlock()
	return c.run(ctx, spec)
}

// Begin starts a swap in the background, or says why it can't.
func (c *Conductor) Begin(ctx context.Context, spec runner.Spec) error {
	if !c.lock.TryLock() {
		return ErrBusy
	}
	c.setSwapping("starting")
	go func() {
		defer c.lock.Unlock()
		_, _ = c.run(ctx, spec) // logged and kept as the last report
	}()
	return nil
}

func (c *Conductor) run(ctx context.Context, spec runner.Spec) (r Report, err error) {
	defer c.setSwapping("")
	ctx = context.WithoutCancel(ctx) // half a swap is worse than a slow one

	from := c.Status().Active
	a, ag := from.Backend, c.Agent(from.Backend)
	gens, err := c.Store.Generations()
	if err != nil {
		return r, err
	}
	gen := max(a.Gen, slices.Max(append(gens, 0))) + 1
	r = Report{From: a.Gen, To: gen, Started: c.Now()}
	defer func() {
		if err != nil {
			r.Error = err.Error()
		}
		c.mu.Lock()
		c.status.Last = &r
		c.mu.Unlock()
		c.Log.Info("swap", "from", r.From, "to", r.To, "freeze", r.Freeze, "moved", r.Moved, "failed", len(r.Failed), "err", r.Error)
	}()
	step := func(name string, f func() error) error {
		c.setSwapping(name)
		t := c.Now()
		err := f()
		r.Steps = append(r.Steps, Step{name, c.Now().Sub(t)})
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	}

	dirA, dirB := c.Store.Dir(a.Gen), c.Store.Dir(gen)
	var b runner.Backend
	// undo puts things back as they were, for a failure before players move.
	undo := func(thaw bool) {
		if thaw {
			if err := ag.Thaw(ctx); err != nil {
				c.Log.Error("thawing the old backend", "err", err)
			}
		}
		if b.Name != "" {
			if err := c.Runner.Stop(ctx, b); err != nil {
				c.Log.Error("stopping the new backend", "err", err)
			}
		}
		if err := os.RemoveAll(dirB); err != nil {
			c.Log.Error("removing the new directory", "err", err)
		}
	}

	var st agent.State
	var m worlds.Manifest
	err = step("snapshot", func() error {
		if st, err = ag.State(ctx); err != nil {
			return err
		}
		if st.Role != "leader" {
			return fmt.Errorf("the active backend is %s, not leading", st.Role)
		}
		if err := ag.SnapshotBegin(ctx); err != nil {
			return err
		}
		m, err = worlds.Copy(dirA, dirB, c.paths(st.Folders))
		return errors.Join(err, ag.SnapshotEnd(ctx))
	})
	if err == nil {
		err = step("start", func() error {
			if err := worlds.Prepare(dirB, c.Secret); err != nil {
				return err
			}
			if b, err = c.Runner.Start(ctx, gen, dirB, c.env(spec)); err != nil {
				return err
			}
			if err := c.ready(ctx, b); err != nil {
				return err
			}
			return c.Agent(b).Follow(ctx)
		})
	}
	if err == nil {
		// What changed while the new backend started comes over now, while
		// everyone still plays, leaving the freeze little to copy.
		err = step("presync", func() error {
			if err := ag.SnapshotBegin(ctx); err != nil {
				return err
			}
			r.Presynced, _, err = worlds.Sync(dirA, dirB, c.paths(st.Folders), m)
			return errors.Join(err, ag.SnapshotEnd(ctx))
		})
	}
	if err != nil {
		undo(false)
		return r, err
	}

	// From here the old backend is frozen: players are waiting.
	frozen := c.Now()
	var f agent.Freeze
	err = step("freeze", func() error {
		f, err = ag.Freeze(ctx)
		return err
	})
	if err == nil && c.Frozen != nil {
		c.Frozen()
	}
	if err == nil {
		err = step("sync", func() error {
			if len(f.Folders) == 0 {
				return errors.New("the old backend didn't say where its worlds are")
			}
			r.Synced, r.Removed, err = worlds.Sync(dirA, dirB, c.paths(f.Folders), m)
			return err
		})
	}
	if err == nil {
		err = step("takeover", func() error { return c.Agent(b).Takeover(ctx, f) })
	}
	if err == nil {
		_ = step("handoff", func() error {
			c.handoff(ctx, ag, c.Agent(b))
			return nil
		})
		c.Proxy.Hold()
		if err = c.Proxy.Serve(b); err == nil {
			_ = step("move", func() error {
				for try := 0; try < 3; try++ {
					var moved int
					moved, r.Failed = c.Proxy.Move(ctx)
					r.Moved += moved
					if len(r.Failed) == 0 {
						break
					}
				}
				return nil
			})
			if r.Moved == 0 && len(r.Failed) > 0 {
				err = fmt.Errorf("no player could move: %s", r.Failed[0])
			}
		}
		if err != nil {
			c.Proxy.Drop(b)
			_ = c.Proxy.Serve(a)
		}
		c.Proxy.Release()
	}
	r.Freeze = c.Now().Sub(frozen)
	if err != nil {
		undo(true)
		return r, err
	}
	if len(r.Failed) > 0 {
		n := c.Proxy.Disconnect(a, "The server was updated. Please reconnect.")
		c.Log.Warn("players who couldn't move were asked to reconnect", "players", n, "why", r.Failed)
	}

	if err = c.save(Active{Backend: b, Spec: spec}); err != nil {
		return r, err
	}
	_ = step("retire", func() error {
		c.Proxy.Drop(a)
		if err := c.Runner.Stop(ctx, a); err != nil {
			c.Log.Error("stopping the old backend", "err", err)
		}
		if c.Keep > 0 {
			if err := c.Store.Prune(c.Keep, gen); err != nil {
				c.Log.Error("pruning old generations", "err", err)
			}
		}
		return nil
	})
	return r, nil
}

// handoff passes what players did while the old backend was frozen, and where
// they are now, to the new one. Without it (an older molt-agent on either)
// they arrive where they were when it froze.
func (c *Conductor) handoff(ctx context.Context, from, to Agent) {
	h, err := from.Handoff(ctx)
	if err == nil {
		err = to.Arrive(ctx, h)
	}
	switch {
	case errors.Is(err, agent.ErrUnsupported):
		c.Log.Info("players arrive where they froze: a backend's molt-agent can't hand off")
	case err != nil:
		c.Log.Warn("players arrive where they froze", "err", err)
	}
}
