package conductor

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"maps"
	"net/http"

	"github.com/tpural/molt/internal/runner"
)

// Change is a swap asked for over the API: what to change from the active
// backend's spec.
type Change struct {
	// Image replaces the image, if set.
	Image string `json:"image,omitempty"`
	// Env sets variables; Unset removes them.
	Env   map[string]string `json:"env,omitempty"`
	Unset []string          `json:"unset,omitempty"`
}

// Apply is the spec after the change.
func (ch Change) Apply(s runner.Spec) runner.Spec {
	out := runner.Spec{Image: s.Image, Env: maps.Clone(s.Env)}
	if ch.Image != "" {
		out.Image = ch.Image
	}
	if out.Env == nil {
		out.Env = map[string]string{}
	}
	maps.Copy(out.Env, ch.Env)
	for _, k := range ch.Unset {
		delete(out.Env, k)
	}
	return out
}

// Handler serves /status and /swap to callers with the token.
func (c *Conductor) Handler(ctx context.Context, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /status", auth(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c.Status())
	}))
	mux.HandleFunc("POST /swap", auth(func(w http.ResponseWriter, r *http.Request) {
		var ch Change
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&ch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := c.Begin(ctx, ch.Apply(c.Status().Active.Spec)); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	return mux
}
