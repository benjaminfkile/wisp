// Package reaper drives contracts through their time-based lifecycle: it warns
// a configurable lead time before the TTL by moving a ready contract to
// expiring, then hard-kills the container and marks the contract expired once
// the TTL elapses (see docs/DESIGN.md §4 and §9, "TTL reaper").
//
// The reaper reads the wall clock through an injectable now function so tests
// drive transitions deterministically without real sleeping: a test sets the
// clock and calls Tick directly. In production Run scans the store on a ticker.
//
// A lifecycle hook (Notify) fires on every transition the reaper makes; the
// event bus (a later task, see docs/DESIGN.md §6) wires into it.
package reaper

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// Defaults applied by New when Options leaves a field zero.
const (
	// defaultInterval is how often Run scans the store.
	defaultInterval = time.Second

	// defaultLead is how long before the TTL a ready contract is warned by
	// moving it to expiring.
	defaultLead = time.Minute
)

// Event describes a lifecycle transition the reaper performed. It is passed to
// the Notify hook so downstream consumers (the event bus) can react.
type Event struct {
	// ContractID is the contract that transitioned.
	ContractID string

	// From and To bracket the transition. From is the state observed when the
	// reaper decided to act; the store's state machine is the authority on
	// whether the move was legal.
	From contract.State
	To   contract.State

	// At is the reaper clock time at which the transition happened.
	At time.Time
}

// Options configures a Reaper. Every field is optional; New fills in defaults.
type Options struct {
	// Lead is how long before a contract's TTL it is moved to expiring as a
	// warning. Defaults to defaultLead.
	Lead time.Duration

	// Interval is how often Run scans the store. Ignored by Tick, which sweeps
	// once per call. Defaults to defaultInterval.
	Interval time.Duration

	// Now is the injectable clock. Defaults to time.Now. Tests set this to make
	// transitions deterministic without real sleeping.
	Now func() time.Time

	// Notify, when set, is called once per transition the reaper makes. It runs
	// on the reaper's own goroutine, so it must not block for long.
	Notify func(Event)

	// ReleaseGPUs, when set, is called with a contract's assigned GPU device IDs
	// as the reaper expires it, so the exclusive device allocator reclaims them
	// (see gpu.Allocator.Free). It runs on the reaper's own goroutine and must be
	// idempotent — the allocator's Free is — so a contract already released and
	// freed elsewhere is never double-freed into a re-assignable state. Nil on a
	// host with no GPU allocator wired.
	ReleaseGPUs func(ids []string)

	// Logger receives operational logs. Defaults to slog.Default().
	Logger *slog.Logger
}

// Reaper scans a contract store and applies time-based transitions, killing
// containers through the Runtime as contracts expire. It is safe for concurrent
// use.
type Reaper struct {
	store    *contract.Store
	rt       runtime.Runtime
	lead     time.Duration
	interval time.Duration
	now      func() time.Time
	logger   *slog.Logger

	// releaseGPUs reclaims an expired contract's GPU devices back to the
	// allocator; nil when no GPU allocator is wired. See Options.ReleaseGPUs.
	releaseGPUs func(ids []string)

	mu     sync.RWMutex
	notify func(Event)
}

// New builds a Reaper over store and rt, applying option defaults.
func New(store *contract.Store, rt runtime.Runtime, opts Options) *Reaper {
	if opts.Lead <= 0 {
		opts.Lead = defaultLead
	}
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Reaper{
		store:       store,
		rt:          rt,
		lead:        opts.Lead,
		interval:    opts.Interval,
		now:         opts.Now,
		logger:      opts.Logger,
		notify:      opts.Notify,
		releaseGPUs: opts.ReleaseGPUs,
	}
}

// SetNotify installs or replaces the lifecycle hook after construction. The
// event bus (a later task) subscribes this way once it is built. Passing nil
// disables notifications.
func (r *Reaper) SetNotify(fn func(Event)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notify = fn
}

// Run performs a startup reconcile — reaping any contract already past its TTL,
// which matters after a restart — and then scans the store every Interval until
// ctx is cancelled. It blocks until ctx is done, so callers typically launch it
// in a goroutine.
func (r *Reaper) Run(ctx context.Context) {
	r.Tick(ctx) // startup reconcile
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Tick(ctx)
		}
	}
}

// Tick performs a single sweep of the store, applying every due transition. It
// is exported so tests drive the reaper deterministically (set the clock, call
// Tick) and so callers can force a reconcile on demand.
func (r *Reaper) Tick(ctx context.Context) {
	for _, c := range r.store.List() {
		r.reap(ctx, c)
	}
}

// reap applies at most one due transition to a single contract. A contract past
// its TTL goes straight to expired (killing its container); a ready contract
// inside the lead window is warned by moving it to expiring.
func (r *Reaper) reap(ctx context.Context, c contract.Contract) {
	if c.State.Terminal() {
		return
	}
	now := r.now()
	switch {
	case !now.Before(c.ExpiresAt):
		r.expire(ctx, c, now)
	case c.State == contract.StateReady && !now.Before(c.ExpiresAt.Add(-r.lead)):
		r.transition(c, contract.StateExpiring, now)
	}
}

// expire kills the contract's container (if one was provisioned) and marks the
// contract expired. A container the runtime no longer knows about is treated as
// already gone.
func (r *Reaper) expire(ctx context.Context, c contract.Contract, now time.Time) {
	if c.ContainerID != "" {
		if err := r.rt.Kill(ctx, c.ContainerID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			r.logger.Error("reaper: kill container", "contract_id", c.ID, "error", err)
		}
	}
	// Reclaim any whole GPU devices the lease held so an expired lease's devices
	// return to the allocator for reuse. The allocator's Free is idempotent, so a
	// lease already released and freed elsewhere is safe to free again here.
	if r.releaseGPUs != nil && len(c.GPUDeviceIDs) > 0 {
		r.releaseGPUs(c.GPUDeviceIDs)
	}
	r.transition(c, contract.StateExpired, now)
}

// transition moves the contract to next and, on success, fires the hook. A
// rejected transition (the contract raced to a terminal state via DELETE, or
// was deleted outright) is logged at debug and ignored: the store's state
// machine is the source of truth.
func (r *Reaper) transition(c contract.Contract, next contract.State, now time.Time) {
	if _, err := r.store.UpdateState(c.ID, next); err != nil {
		if errors.Is(err, contract.ErrIllegalTransition) || errors.Is(err, contract.ErrNotFound) {
			r.logger.Debug("reaper: skip transition", "contract_id", c.ID, "to", next, "error", err)
			return
		}
		r.logger.Error("reaper: transition", "contract_id", c.ID, "to", next, "error", err)
		return
	}
	r.fire(Event{ContractID: c.ID, From: c.State, To: next, At: now})
}

// fire invokes the lifecycle hook if one is installed.
func (r *Reaper) fire(e Event) {
	r.mu.RLock()
	fn := r.notify
	r.mu.RUnlock()
	if fn != nil {
		fn(e)
	}
}
