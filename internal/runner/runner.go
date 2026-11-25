// Package runner starts and stops backends: Paper servers, one per
// generation, each on its own directory.
package runner

import (
	"context"
	"maps"
	"slices"
	"strconv"
)

// Spec is what a generation runs: an image and its environment. For the
// itzg/minecraft-server image that's the Paper version (VERSION) and plugins
// (PLUGINS), among others.
type Spec struct {
	Image string            `json:"image"`
	Env   map[string]string `json:"env,omitempty"`
}

// Backend is a running generation.
type Backend struct {
	Gen  int    `json:"gen"`
	Name string `json:"name"`
	// Game is where the proxy sends players (host:port).
	Game string `json:"game"`
	// Agent is molt-agent's base URL.
	Agent string `json:"agent"`
}

// Runner starts and stops backends.
type Runner interface {
	// Start runs spec on dir as generation gen, and returns once it has an
	// address; the server inside is still starting.
	Start(ctx context.Context, gen int, dir string, spec Spec) (Backend, error)
	// Stop stops a backend, letting it shut down cleanly.
	Stop(ctx context.Context, b Backend) error
	// Find finds generation gen if it's running.
	Find(ctx context.Context, gen int) (Backend, bool, error)
}

// Ports inside a backend.
const (
	GamePort  = 25565
	AgentPort = 25580
)

// Name is generation gen's backend name.
func Name(prefix string, gen int) string { return prefix + "-gen-" + strconv.Itoa(gen) }

// env is the spec's environment, sorted, as KEY=value.
func env(spec Spec) []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(spec.Env)) {
		out = append(out, k+"="+spec.Env[k])
	}
	return out
}
