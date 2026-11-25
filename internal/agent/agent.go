// Package agent talks to molt-agent, the Paper plugin on each backend.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// State is what a backend says about itself.
type State struct {
	// Role is "standby" (started, refusing joins), "follower" (frozen,
	// waiting for a takeover) or "leader" (playing).
	Role    string `json:"role"`
	Ready   bool   `json:"ready"`
	Frozen  bool   `json:"frozen"`
	Players int    `json:"players"`
	// Version is the server's Minecraft and Paper version.
	Version string   `json:"version"`
	Plugins []string `json:"plugins"`
	// Folders are the worlds' folders, relative to the server's directory.
	Folders []string `json:"folders"`
}

// Player is where a player was when the leader froze.
type Player struct {
	UUID  string  `json:"uuid"`
	Name  string  `json:"name"`
	World string  `json:"world"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Z     float64 `json:"z"`
}

// World is what a world keeps in memory that a follower read at start, and
// has to be told again: the time and weather.
type World struct {
	Name         string `json:"name"`
	FullTime     int64  `json:"fullTime"`
	Storm        bool   `json:"storm"`
	Thundering   bool   `json:"thundering"`
	WeatherTicks int    `json:"weatherTicks"`
	ThunderTicks int    `json:"thunderTicks"`
	ClearTicks   int    `json:"clearTicks"`
}

// Freeze is the leader's state at the moment it froze.
type Freeze struct {
	Players []Player `json:"players"`
	Worlds  []World  `json:"worlds"`
	Folders []string `json:"folders"`
}

// Client is one backend's agent.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// New returns a client for the agent at base.
func New(base, token string) *Client {
	return &Client{URL: strings.TrimRight(base, "/"), Token: token, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// State asks what the backend is doing.
func (c *Client) State(ctx context.Context) (State, error) {
	var s State
	return s, c.do(ctx, http.MethodGet, "/state", nil, &s)
}

// SnapshotBegin stops saving and flushes the worlds, so they can be copied.
func (c *Client) SnapshotBegin(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/snapshot/begin", struct{}{}, nil)
}

// SnapshotEnd turns saving back on.
func (c *Client) SnapshotEnd(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/snapshot/end", struct{}{}, nil)
}

// Follow freezes a standby backend and stops it saving, until a takeover.
func (c *Client) Follow(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/follow", struct{}{}, nil)
}

// Freeze stops the leader's world and players, saves everything, and says
// where everyone was.
func (c *Client) Freeze(ctx context.Context) (Freeze, error) {
	var f Freeze
	return f, c.do(ctx, http.MethodPost, "/freeze", struct{}{}, &f)
}

// Takeover makes a follower the leader: it rereads the worlds from disk,
// applies the old leader's state, and starts playing.
func (c *Client) Takeover(ctx context.Context, f Freeze) error {
	return c.do(ctx, http.MethodPost, "/takeover", f, nil)
}

// Thaw undoes a freeze.
func (c *Client) Thaw(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/thaw", struct{}{}, nil)
}
