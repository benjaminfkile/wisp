// Package server wires the Wisp HTTP surface: routing, middleware, and handlers.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/benjaminfkile/wisp/internal/bus"
	"github.com/benjaminfkile/wisp/internal/capacity"
	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/gpu"
	"github.com/benjaminfkile/wisp/internal/policy"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// New builds the root http.Handler for the daemon, registering all routes. The
// contract lifecycle endpoints are wired to store and rt; rt is the container
// backend (the real Docker runtime in production, the fake in tests). pol is the
// operator's launch policy - the image allow-list, default image, and limits a
// create request is validated and clamped against (see docs/DESIGN.md §7).
//
// appToken is the app-level bearer credential gating contract creation (see
// docs/DESIGN.md §8). An empty appToken disables that gate - the
// localhost-friendly default. Contract-scoped calls (/exec, /shell) are always
// gated by the per-contract token regardless of appToken.
//
// eventBus is the in-process pub/sub bus backing the /events endpoints and the
// channel contract lifecycle events are published on (see docs/DESIGN.md §6). It
// is shared with the reaper so its expiring/expired transitions reach the same
// subscribers (main wires that via LifecycleNotify).
//
// The returned handler is stdlib-only (net/http ServeMux); richer routing can
// be layered in later tasks.
func New(logger *slog.Logger, store *contract.Store, rt runtime.Runtime, pol *policy.Config, eventBus *bus.Bus, appToken string) http.Handler {
	return NewDaemon(logger, store, rt, pol, eventBus, appToken).Handler
}

// Daemon bundles the HTTP handler with the stateful pieces the wispd process
// must share across its background workers - the contract store, the container
// runtime, and the exclusive GPU device allocator - so the startup reconcile and
// the TTL reaper operate on the SAME allocator the HTTP create path allocates
// from. That single shared allocator is what makes the whole-device exclusivity
// guarantee hold end to end: a device freed by a released or reaped lease is
// immediately reusable, and a restart rebuilds occupancy from container labels
// instead of double-assigning a device a surviving lease still holds.
type Daemon struct {
	// Handler is the root HTTP handler, ready to serve.
	Handler http.Handler

	store  *contract.Store
	rt     runtime.Runtime
	br     *broker
	alloc  *gpu.Allocator
	cap    *capacity.Allocator
	logger *slog.Logger
}

// NewDaemon builds the HTTP surface and returns it alongside the shared
// components main wires the reconcile and reaper to. See New for the argument
// semantics; New is the thin wrapper that returns only the handler for callers
// (tests) that need nothing more.
func NewDaemon(logger *slog.Logger, store *contract.Store, rt runtime.Runtime, pol *policy.Config, eventBus *bus.Bus, appToken string) *Daemon {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	br := newBroker(store, rt, pol, eventBus, logger, appToken)
	br.routes(mux)
	return &Daemon{
		Handler: requestLogger(logger, mux),
		store:   store,
		rt:      rt,
		br:      br,
		alloc:   br.alloc,
		cap:     br.cap,
		logger:  logger,
	}
}

// SetKillTimeout configures the bound the DELETE handler applies to a single
// container kill run under its detached kill context. A non-positive value is
// ignored so the broker keeps its constructor default. Wired from
// WISP_KILL_TIMEOUT_SECONDS by cmd/wispd so the release handler and the
// reaper's release-grace expiry share the same kill bound end to end.
func (d *Daemon) SetKillTimeout(timeout time.Duration) {
	if timeout > 0 && d.br != nil {
		d.br.killTimeout = timeout
	}
}

// Reconcile rebuilds in-memory contract tracking, GPU allocator occupancy, AND
// aggregate capacity usage from the labels of containers a previous wispd left
// behind. It binds the daemon's shared allocators so a rediscovered lease's devices
// and its cpus/memory budget are reserved before any new create can allocate.
// Callers MUST run it before the reaper starts (see the package Reconcile).
func (d *Daemon) Reconcile(ctx context.Context) {
	Reconcile(ctx, d.store, d.rt, d.alloc, d.cap, d.logger)
}

// ReleaseGPUs frees the GPU devices a contract held back to the shared allocator.
// It is idempotent (the allocator's Free is) and is wired into the reaper as its
// ReleaseGPUs hook so an expired lease's devices return to the pool.
func (d *Daemon) ReleaseGPUs(ids []string) {
	d.alloc.Free(ids)
}

// ReleaseCapacity returns a contract's reserved host capacity (its post-clamp
// cpus / memory and its contract slot) to the shared capacity allocator. It is
// wired into the reaper as its ReleaseCapacity hook so an expired lease's budget
// is reclaimed. The reaper calls it only after the winning expired transition, so
// a lease released and reaped in a race frees its capacity exactly once.
func (d *Daemon) ReleaseCapacity(c contract.Contract) {
	d.cap.Free(c.ReservedCPUs, c.ReservedMemoryMB)
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
