// Package proxy is the part of molt players connect to. It never restarts:
// backends come and go behind it, and it moves players from one to the next.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/robinbraemer/event"
	"go.minekube.com/common/minecraft/component"
	"go.minekube.com/gate/pkg/edition/java/config"
	jproxy "go.minekube.com/gate/pkg/edition/java/proxy"
	"go.minekube.com/gate/pkg/util/configutil"

	"github.com/tpural/molt/internal/runner"
)

// Config is what the proxy needs to know.
type Config struct {
	// Bind is the address players connect to.
	Bind string
	// OnlineMode checks players with Mojang. Off only for tests.
	OnlineMode bool
	// Secret is shared with the backends (Velocity modern forwarding).
	Secret string
	// Motd is the server list's text.
	Motd string
}

// Proxy holds players' connections and sends them to the active backend.
type Proxy struct {
	p   *jproxy.Proxy
	log logr.Logger

	mu     sync.Mutex
	active jproxy.RegisteredServer
	// held is closed when a swap ends; logins wait for it.
	held chan struct{}
}

// New makes a proxy; Start runs it.
func New(cfg Config, log logr.Logger) (*Proxy, error) {
	c := config.DefaultConfig
	c.Bind = cfg.Bind
	c.OnlineMode = cfg.OnlineMode
	c.Forwarding.Mode = config.VelocityForwardingMode
	c.Forwarding.VelocitySecret = cfg.Secret
	c.FailoverOnUnexpectedServerDisconnect = false
	// A server that just took over may be slow to log players in: give it time
	// rather than drop them.
	c.ConnectionTimeout = configutil.Duration(30 * time.Second)
	if cfg.Motd != "" {
		c.Status.Motd = &configutil.Component{Value: &component.Text{Content: cfg.Motd}}
	}
	if _, errs := c.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("proxy config: %w", errors.Join(errs...))
	}
	mgr := event.New()
	p, err := jproxy.New(jproxy.Options{Config: &c, EventMgr: mgr})
	if err != nil {
		return nil, err
	}
	px := &Proxy{p: p, log: log}
	event.Subscribe(mgr, 0, px.chooseServer)
	event.Subscribe(mgr, 0, px.kicked)
	return px, nil
}

// Start runs the proxy until ctx ends.
func (p *Proxy) Start(ctx context.Context) error {
	return p.p.Start(logr.NewContext(ctx, p.log))
}

// wait blocks while a swap is moving players, up to a minute.
func (p *Proxy) wait() jproxy.RegisteredServer {
	p.mu.Lock()
	held := p.held
	p.mu.Unlock()
	if held != nil {
		select {
		case <-held:
		case <-time.After(time.Minute):
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

func (p *Proxy) chooseServer(e *jproxy.PlayerChooseInitialServerEvent) {
	if s := p.wait(); s != nil {
		e.SetInitialServer(s)
	}
}

// kicked sends a player who lost an old backend to the active one.
func (p *Proxy) kicked(e *jproxy.KickedFromServerEvent) {
	if e.KickedDuringServerConnect() {
		return
	}
	s := p.wait()
	if s == nil || s == e.Server() {
		return
	}
	e.SetResult(&jproxy.RedirectPlayerKickResult{Server: s})
}

// Serve registers a backend and makes it where new players go.
func (p *Proxy) Serve(b runner.Backend) error {
	addr, err := net.ResolveTCPAddr("tcp", b.Game)
	if err != nil {
		return err
	}
	s, err := p.p.Register(jproxy.NewServerInfo(b.Name, addr))
	if err != nil && !errors.Is(err, jproxy.ErrServerAlreadyExists) {
		return err
	}
	p.mu.Lock()
	p.active = s
	p.mu.Unlock()
	return nil
}

// Hold makes new logins wait until Release.
func (p *Proxy) Hold() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held == nil {
		p.held = make(chan struct{})
	}
}

// Release lets waiting logins through to the active backend.
func (p *Proxy) Release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held != nil {
		close(p.held)
		p.held = nil
	}
}

// Move sends every player who isn't on the active backend to it, and says
// who couldn't be moved. Players move seamlessly, without a loading screen,
// when the backend configures their client as the last one did; others switch
// the usual way.
func (p *Proxy) Move(ctx context.Context, seamless bool) (moved, smooth int, failed []string) {
	p.mu.Lock()
	to := p.active
	p.mu.Unlock()
	if to == nil {
		return 0, 0, nil
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, pl := range p.p.Players() {
		if cs := pl.CurrentServer(); cs != nil && cs.Server() == to {
			continue
		}
		wg.Go(func() {
			var r jproxy.ConnectionResult
			err := jproxy.ErrNotSeamless
			if seamless {
				r, err = jproxy.ConnectSeamless(ctx, pl.CreateConnectionRequest(to))
				if errors.Is(err, jproxy.ErrNotSeamless) {
					p.log.Info("switching the usual way", "player", pl.Username(), "why", err.Error())
				}
			}
			ok := err == nil && r.Status().Successful()
			if errors.Is(err, jproxy.ErrNotSeamless) {
				r, err = pl.CreateConnectionRequest(to).Connect(ctx)
			}
			mu.Lock()
			defer mu.Unlock()
			if ok {
				smooth++
			}
			switch {
			case err != nil:
				failed = append(failed, fmt.Sprintf("%s: %v", pl.Username(), err))
			case !r.Status().Successful() && !r.Status().AlreadyConnected():
				failed = append(failed, fmt.Sprintf("%s: %s", pl.Username(), reason(r)))
			default:
				moved++
			}
		})
	}
	wg.Wait()
	return moved, smooth, failed
}

func reason(r jproxy.ConnectionResult) string {
	if t, ok := r.Reason().(*component.Text); ok && t != nil {
		return t.Content
	}
	return fmt.Sprintf("status %d", r.Status())
}

// Disconnect drops players still on the backend, asking them to reconnect.
func (p *Proxy) Disconnect(b runner.Backend, why string) int {
	s := p.p.Server(b.Name)
	if s == nil {
		return 0
	}
	n := 0
	s.Players().Range(func(pl jproxy.Player) bool {
		pl.Disconnect(&component.Text{Content: why})
		n++
		return true
	})
	return n
}

// Drop unregisters a backend.
func (p *Proxy) Drop(b runner.Backend) {
	if s := p.p.Server(b.Name); s != nil {
		p.p.Unregister(s.ServerInfo())
	}
}

// Players is how many players are connected.
func (p *Proxy) Players() int { return p.p.PlayerCount() }
