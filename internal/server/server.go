// Package server wires the Wisp HTTP surface: routing, middleware, and handlers.
package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/preset"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// New builds the root http.Handler for the daemon, registering all routes. The
// contract lifecycle endpoints are wired to store and rt; rt is the container
// backend (the real Docker runtime in production, the fake in tests). presets is
// the set of named launch configurations contracts reference by name (see
// docs/DESIGN.md §7).
//
// appToken is the app-level bearer credential gating contract creation (see
// docs/DESIGN.md §8). An empty appToken disables that gate — the
// localhost-friendly default. Contract-scoped calls (/exec, /shell) are always
// gated by the per-contract token regardless of appToken.
//
// The returned handler is stdlib-only (net/http ServeMux); richer routing can
// be layered in later tasks.
func New(logger *slog.Logger, store *contract.Store, rt runtime.Runtime, presets *preset.Set, appToken string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	newBroker(store, rt, presets, logger, appToken).routes(mux)
	return requestLogger(logger, mux)
}

// healthz is a liveness probe returning {"status":"ok"}.
func healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// writeJSON encodes v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error body {"error": msg} with the given status.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the wrapped ResponseWriter's Flush so streaming handlers
// (the streaming exec endpoint) can push output to the client through the
// logging middleware. If the underlying writer is not a Flusher this is a no-op.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the wrapped ResponseWriter's Hijacker so the shell
// endpoint can upgrade to a WebSocket through the logging middleware. Without
// this, the wrapper would mask the underlying http.Hijacker and gorilla's
// Upgrade would fail with "bad handshake". If the underlying writer is not a
// Hijacker (e.g. httptest.ResponseRecorder), it returns an error.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("server: response writer does not support hijacking")
	}
	return hj.Hijack()
}

// requestLogger emits one structured log line per request.
func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}
