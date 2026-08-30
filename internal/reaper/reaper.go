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
// event bus wires into it in cmd/wispd so contract.expiring / contract.expired
// are republished onto the same bus the HTTP surface reads and writes to (see
// docs/DESIGN.md §6).
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

	// defaultReleaseGrace is how long a contract may sit in StateReleasing
	// before the reaper stops skipping it. Inside the window the DELETE handler
	// owns the tear down (see reap); past it the release is presumed stuck (the
	// handler died mid-release or its request was cancelled before it reached
	// the final mark-released transition), and the reaper expires the contract
	// so its capacity and GPUs return to the allocators instead of leaking
	// until restart. A hung Docker daemon on the release-handler side is
	// bounded separately by defaultKillTimeout (see expire), not by this grace.
	defaultReleaseGrace = 30 * time.Second

	// defaultKillTimeout bounds a single reaper Kill call so a hung Docker
	// daemon cannot stall the reaper: the bounded Kill runs OFF the tick in
	// its own goroutine, so a wedged container adds no latency to the sweep.
	// On timeout the contract is left in place for a later kill attempt while
	// the sweep proceeds with the remaining contracts.
	defaultKillTimeout = 30 * time.Second
)

// Reason categorises WHY the reaper drove a contract into a particular state,
// so downstream consumers (the bus) can distinguish otherwise-identical
// terminal transitions. Only expired transitions carry a reason today; the
// warning-only expiring transition leaves it empty.
const (
	// ReasonTTLExpired is set on an expired transition triggered by the TTL
	// having elapsed (the ordinary lifecycle end).
	ReasonTTLExpired = "ttl_expired"

	// ReasonContainerDied is set on an expired transition triggered by the
	// backing container going away before the TTL (docker kill / docker rm /
	// OOM), detected by the reaper's liveness sweep.
	ReasonContainerDied = "container_died"
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

	// Reason categorises the transition (see the Reason* constants). Empty
	// unless To is StateExpired; the two current values distinguish a TTL
	// expiry from an out-of-band container death so subscribers can tell them
	// apart on the bus.
	Reason string
}

// Options configures a Reaper. Every field is optional; New fills in defaults.
type Options struct {
	// Lead is how long before a contract's TTL it is moved to expiring as a
	// warning. Defaults to defaultLead.
	Lead time.Duration

	// Interval is how often Run scans the store. Ignored by Tick, which sweeps
	// once per call. Defaults to defaultInterval.
	Interval time.Duration

	// ReleaseGrace bounds how long a contract may sit in StateReleasing before
	// the reaper stops skipping it. Inside the window the DELETE handler owns
	// the tear down; past it the release is presumed stuck (the handler died
	// mid-release or its request was cancelled before the final mark-released
	// transition) and the reaper expires the contract like any other
	// non-terminal one so its capacity and GPUs return to the allocators.
	// Defaults to defaultReleaseGrace.
	ReleaseGrace time.Duration

	// KillTimeout bounds a single reaper Kill call so a hung Docker daemon
	// cannot stall the reaper. The bounded Kill runs off the tick in its own
	// goroutine, so a wedged container adds no latency to the sweep. On
	// timeout the contract is left in place for a later kill attempt. Defaults
	// to defaultKillTimeout.
	KillTimeout time.Duration

	// Now is the injectable clock. Defaults to time.Now. Tests set this to make
	// transitions deterministic without real sleeping.
	Now func() time.Time

	// Notify, when set, is called once per transition the reaper makes. It runs
	// on the reaper's own goroutine, so it must not block for long.
	Notify func(Event)

	// ReleaseGPUs, when set, is called with a contract's assigned GPU device IDs
	// as the reaper expires it, so the exclusive device allocator reclaims them
	// (see gpu.Allocator.Free). It runs on the reaper's own goroutine and must be
	// idempotent - the allocator's Free is - so a contract already released and
	// freed elsewhere is never double-freed into a re-assignable state. Nil on a
	// host with no GPU allocator wired.
	ReleaseGPUs func(ids []string)

	// ReleaseCapacity, when set, is called with a contract as the reaper expires it
	// so the aggregate capacity allocator reclaims the lease's reserved cpus /
	// memory and contract slot (see capacity.Allocator.Free). Unlike ReleaseGPUs it
	// fires only AFTER the winning expired transition, so a lease released and reaped
	// in a race frees its capacity exactly once (the state machine admits one side).
	// It runs on the reaper's own goroutine. Nil when no capacity allocator is wired.
	ReleaseCapacity func(c contract.Contract)

	// Logger receives operational logs. Defaults to slog.Default().
	Logger *slog.Logger
}

// Reaper scans a contract store and applies time-based transitions, killing
// containers through the Runtime as contracts expire. It is safe for concurrent
// use.
type Reaper struct {
	store        *contract.Store
	rt           runtime.Runtime
	lead         time.Duration
	interval     time.Duration
	releaseGrace time.Duration
	killTimeout  time.Duration
	now          func() time.Time
	logger       *slog.Logger

	// releaseGPUs reclaims an expired contract's GPU devices back to the
	// allocator; nil when no GPU allocator is wired. See Options.ReleaseGPUs.
	releaseGPUs func(ids []string)

	// releaseCapacity reclaims an expired contract's reserved host capacity back to
	// the aggregate allocator; nil when none is wired. See Options.ReleaseCapacity.
	releaseCapacity func(c contract.Contract)

	mu     sync.RWMutex
	notify func(Event)

	// killMu guards killing, the set of contract IDs whose reaper-driven Kill
	// is currently outstanding. reap() skips a contract in this set so a hung
	// Kill goroutine is not fanned out again by a subsequent tick (bounding
	// concurrency to one in-flight kill per contract), and the goroutine
	// clears the entry on completion so a timed-out kill is retried on a
	// later tick.
	killMu  sync.Mutex
	killing map[string]struct{}

	// killWG counts every off-tick Kill goroutine so tests (and, on shutdown,
	// Run itself) can synchronize on the async completion path via
	// WaitForKills.
	killWG sync.WaitGroup
}

// New builds a Reaper over store and rt, applying option defaults.
func New(store *contract.Store, rt runtime.Runtime, opts Options) *Reaper {
	if opts.Lead <= 0 {
		opts.Lead = defaultLead
	}
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}
	if opts.ReleaseGrace <= 0 {
		opts.ReleaseGrace = defaultReleaseGrace
	}
	if opts.KillTimeout <= 0 {
		opts.KillTimeout = defaultKillTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Reaper{
		store:           store,
		rt:              rt,
		lead:            opts.Lead,
		interval:        opts.Interval,
		releaseGrace:    opts.ReleaseGrace,
		killTimeout:     opts.KillTimeout,
		now:             opts.Now,
		logger:          opts.Logger,
		notify:          opts.Notify,
		releaseGPUs:     opts.ReleaseGPUs,
		releaseCapacity: opts.ReleaseCapacity,
		killing:         make(map[string]struct{}),
	}
}

// SetNotify installs or replaces the lifecycle hook after construction. The
// event bus subscribes this way in cmd/wispd via server.LifecycleNotify.
// Passing nil disables notifications.
func (r *Reaper) SetNotify(fn func(Event)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notify = fn
}

// Run performs a startup reconcile - reaping any contract already past its TTL,
// which matters after a restart - and then scans the store every Interval until
// ctx is cancelled. It blocks until ctx is done, so callers typically launch it
// in a goroutine.
//
// On shutdown Run drains any Kill goroutines still in flight before returning,
// bounded by the configured KillTimeout so a Kill that does not honor its
// context cancellation (a wedged Docker daemon) cannot hang shutdown forever.
// Callers that want the drain to complete before releasing shutdown resources
// (see cmd/wispd/main.go) should wait for Run to return.
func (r *Reaper) Run(ctx context.Context) {
	r.Tick(ctx) // startup reconcile
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.waitForKillsBounded(r.killTimeout)
			return
		case <-t.C:
			r.Tick(ctx)
		}
	}
}

// waitForKillsBounded blocks until every in-flight Kill goroutine has returned
// or the timeout elapses, whichever comes first. On a graceful shutdown the
// outer ctx is already cancelled, so each Kill's killCtx is cancelled too and
// the goroutines finish promptly via the abandoned-kill path in killAndFinish;
// the bound covers only the pathological case of a Kill that ignores its
// context so wispd does not hang past the daemon's shutdown budget.
func (r *Reaper) waitForKillsBounded(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		r.killWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// Tick performs a single sweep of the store, applying every due transition,
// and drops from the store any contract that was ALREADY terminal when the
// sweep began, so the store (and this tick's List() walk) do not grow
// unboundedly across restarts and long-running hosts. A contract this tick
// itself transitioned to terminal is left in place for one full tick so a
// polling caller can still observe the terminal state once before it is
// removed. It is exported so tests drive the reaper deterministically (set
// the clock, call Tick) and so callers can force a reconcile on demand.
func (r *Reaper) Tick(ctx context.Context) {
	contracts := r.store.List()
	// Snapshot which contracts were already terminal before reap runs, so a lease
	// this tick moves to terminal does not get deleted in the same tick - polling
	// callers can still observe the transition once.
	terminalBefore := make([]string, 0)
	for _, c := range contracts {
		if c.State.Terminal() {
			terminalBefore = append(terminalBefore, c.ID)
		}
	}
	for _, c := range contracts {
		r.reap(ctx, c)
	}
	for _, id := range terminalBefore {
		if err := r.store.Delete(id); err != nil && !errors.Is(err, contract.ErrNotFound) {
			r.logger.Debug("reaper: delete terminal contract", "contract_id", id, "error", err)
		}
	}
}

// reap applies at most one due transition to a single contract. A contract past
// its TTL goes straight to expired (killing its container); a ready contract
// inside the lead window is warned by moving it to expiring; a contract whose
// backing container has died out of band (docker kill / rm / OOM) is expired
// on the same path, so its capacity and GPUs return to the allocators within a
// few ticks rather than at TTL expiry. The two expire paths tag their
// transition with distinct Reason values so the bus can tell them apart.
//
// A contract in StateReleasing is skipped for the reaper's release-grace window:
// the DELETE handler owns that fence and is in the middle of killing the
// container, so touching it here would double-kill the container ("removal
// already in progress"), expire-and-purge it from under the handler, and turn
// the DELETE into a 500 (the 2026-08-29 wisp-log failure the fence was added
// for). Past the grace the release is presumed stuck (the handler died
// mid-release or its request was cancelled before it reached the final
// mark-released transition), and the reaper expires the contract like any
// other non-terminal one so its capacity and GPUs return to the allocators
// rather than leaking until restart. A hung Docker daemon on the reaper's own
// Kill is bounded by killTimeout in expire, not by this grace.
//
// A contract with a reaper-driven Kill already outstanding is skipped
// unconditionally: the previous tick launched an off-tick Kill goroutine and
// the completion path owns the terminal transition; skipping here is what
// stops a subsequent tick from double-killing the same container.
func (r *Reaper) reap(ctx context.Context, c contract.Contract) {
	if c.State.Terminal() {
		return
	}
	if r.isKilling(c.ID) {
		return
	}
	now := r.now()
	if c.State == contract.StateReleasing {
		if !c.ReleasingSince.IsZero() && now.Sub(c.ReleasingSince) < r.releaseGrace {
			return
		}
		r.logger.Info("reaper: releasing contract past grace; expiring",
			"contract_id", c.ID,
			"container_id", c.ContainerID,
			"grace", r.releaseGrace,
			"releasing_since", c.ReleasingSince)
		r.expire(ctx, c, now, ReasonTTLExpired)
		return
	}
	switch {
	case !now.Before(c.ExpiresAt):
		r.expire(ctx, c, now, ReasonTTLExpired)
	case r.containerDied(ctx, c):
		// The container is gone or stopped before its TTL: route through the same
		// expire path so capacity, GPUs, and the contract slot free exactly once
		// via the state-machine gate. A distinct log line makes the death (as
		// opposed to a TTL expiry) visible to operators.
		r.logger.Info("reaper: container died before TTL; expiring contract",
			"contract_id", c.ID, "container_id", c.ContainerID, "state", c.State)
		r.expire(ctx, c, now, ReasonContainerDied)
	case c.State == contract.StateReady && !now.Before(c.ExpiresAt.Add(-r.lead)):
		r.transition(c, contract.StateExpiring, now, "")
	}
}

// containerDied reports whether the contract's backing container has died out
// of band and the contract should be reaped. It only inspects contracts in
// StateReady or StateExpiring - the states where a running container is
// EXPECTED - so a container mid-provision (created but not yet Started) is not
// mistaken for a dead one, and a contract with no ContainerID yet is skipped.
// A transport error from Inspect is treated as inconclusive: the reaper does
// NOT reap the lease on a transient daemon blip and retries on the next tick.
func (r *Reaper) containerDied(ctx context.Context, c contract.Contract) bool {
	if c.ContainerID == "" {
		return false
	}
	if c.State != contract.StateReady && c.State != contract.StateExpiring {
		return false
	}
	state, err := r.rt.Inspect(ctx, c.ContainerID)
	if err != nil {
		r.logger.Debug("reaper: inspect container", "contract_id", c.ID, "container_id", c.ContainerID, "error", err)
		return false
	}
	return state != runtime.LivenessRunning
}

// expire drives the contract's terminal transition. When a container is
// provisioned the Kill runs OFF the tick in its own goroutine so a hung
// Docker daemon (a wedged ContainerRemove) adds no latency to the sweep: the
// contract is marked "kill in flight", the bounded Kill is launched, and the
// state transition plus capacity / GPU free apply in the goroutine's
// completion. reap() skips a contract with a Kill in flight so the next tick
// never double-kills the same container. reason categorises WHY the reaper is
// expiring the contract (TTL vs container death) and rides through to the
// fired Event so the bus can republish it on contract.expired.
//
// A contract with no ContainerID is transitioned inline: there is no
// container to kill and no need to spend a goroutine on it.
func (r *Reaper) expire(ctx context.Context, c contract.Contract, now time.Time, reason string) {
	if c.ContainerID == "" {
		r.finishExpire(c, now, reason)
		return
	}
	if !r.beginKill(c.ID) {
		// A previous tick's Kill goroutine is still outstanding for this
		// contract; the completion path owns the terminal transition. Bounding
		// concurrency to one in-flight kill per contract is what stops a
		// wedged Kill from being fanned out again by subsequent ticks.
		return
	}
	r.killWG.Add(1)
	go func() {
		defer r.killWG.Done()
		r.killAndFinish(ctx, c, now, reason)
	}()
}

// WaitForKills blocks until every off-tick Kill goroutine launched so far has
// returned. It is exposed so tests (and, at shutdown, callers) can synchronize
// on the async completion path without polling: a test that asserts an
// expire's side effects (container killed, state transitioned, capacity
// freed) waits here after Tick before checking.
func (r *Reaper) WaitForKills() {
	r.killWG.Wait()
}

// killAndFinish runs the bounded Kill for the contract's container and then
// applies the terminal transition plus capacity / GPU free on completion. It
// always clears the in-flight mark on return so a later tick can retry a
// timed-out Kill. Kill's own return-value error (a real not-found or any
// other daemon reply) is a normal observation and does not stop the expire:
// not-found is treated as already-gone, and anything else is logged and the
// transition still runs so capacity is freed. A killCtx deadline that fired
// means the daemon has not answered within killTimeout: the contract is left
// in place for a later kill attempt and no terminal transition is applied.
// If the outer ctx has been cancelled (shutdown), the Kill was abandoned
// before the container was actually removed: log at Info and return without
// applying the terminal transition or freeing capacity, so a subsequent
// wispd process can reconcile the surviving container from its labels rather
// than seeing an "expired" contract whose container is in fact still alive.
func (r *Reaper) killAndFinish(ctx context.Context, c contract.Contract, now time.Time, reason string) {
	defer r.endKill(c.ID)

	killCtx, cancel := context.WithTimeout(ctx, r.killTimeout)
	err := r.rt.Kill(killCtx, c.ContainerID)
	cancel()
	if ctx.Err() != nil {
		r.logger.Info("reaper: shutdown, kill abandoned",
			"contract_id", c.ID, "container_id", c.ContainerID)
		return
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			r.logger.Error("reaper: kill container timed out; leaving for later attempt",
				"contract_id", c.ID, "container_id", c.ContainerID, "timeout", r.killTimeout)
			return
		}
		if !errors.Is(err, runtime.ErrNotFound) {
			r.logger.Error("reaper: kill container", "contract_id", c.ID, "error", err)
		}
	}
	r.finishExpire(c, now, reason)
}

// finishExpire applies the GPU release, the terminal transition, and the
// capacity free for a contract whose container has been killed (or was never
// provisioned). It is the shared completion path for both the inline
// no-container branch of expire and the async killAndFinish goroutine, so
// the state-machine gate that keeps capacity freeing exactly once is applied
// identically in both cases.
func (r *Reaper) finishExpire(c contract.Contract, now time.Time, reason string) {
	// Reclaim any whole GPU devices the lease held so an expired lease's devices
	// return to the allocator for reuse. The allocator's Free is idempotent, so a
	// lease already released and freed elsewhere is safe to free again here.
	if r.releaseGPUs != nil && len(c.GPUDeviceIDs) > 0 {
		r.releaseGPUs(c.GPUDeviceIDs)
	}
	// Reclaim the lease's reserved host capacity ONLY when this reaper wins the
	// expired transition. The capacity allocator's Free (unlike the GPU allocator's)
	// subtracts plain amounts and is not self-idempotent, so gating on the winning
	// transition is what guarantees a lease released and reaped in a race frees its
	// capacity exactly once (the store admits a single terminal transition).
	if r.transition(c, contract.StateExpired, now, reason) && r.releaseCapacity != nil {
		r.releaseCapacity(c)
	}
}

// beginKill marks the contract as having a reaper-driven Kill in flight. It
// reports true when this call installed the mark (proceed with the Kill), and
// false when another goroutine already holds one (bound concurrency to one
// in-flight Kill per contract). endKill clears the mark on the goroutine's
// return, so a timed-out Kill is retried on a later tick.
func (r *Reaper) beginKill(id string) bool {
	r.killMu.Lock()
	defer r.killMu.Unlock()
	if _, ok := r.killing[id]; ok {
		return false
	}
	r.killing[id] = struct{}{}
	return true
}

// endKill clears the in-flight mark for the contract.
func (r *Reaper) endKill(id string) {
	r.killMu.Lock()
	defer r.killMu.Unlock()
	delete(r.killing, id)
}

// isKilling reports whether a reaper-driven Kill for the contract is
// currently in flight so reap() can skip it and let the goroutine own the
// outcome.
func (r *Reaper) isKilling(id string) bool {
	r.killMu.Lock()
	defer r.killMu.Unlock()
	_, ok := r.killing[id]
	return ok
}

// transition moves the contract to next and, on success, fires the hook. It
// reports whether the move actually happened so a caller (see expire) can gate a
// once-per-contract side effect on winning the transition. A rejected transition
// (the contract raced to a terminal state via DELETE, or was deleted outright) is
// logged at debug and ignored, returning false: the store's state machine is the
// source of truth. reason is propagated onto the fired Event so subscribers can
// distinguish otherwise-identical terminal transitions (see Event.Reason).
func (r *Reaper) transition(c contract.Contract, next contract.State, now time.Time, reason string) bool {
	if _, err := r.store.UpdateState(c.ID, next); err != nil {
		if errors.Is(err, contract.ErrIllegalTransition) || errors.Is(err, contract.ErrNotFound) {
			r.logger.Debug("reaper: skip transition", "contract_id", c.ID, "to", next, "error", err)
			return false
		}
		r.logger.Error("reaper: transition", "contract_id", c.ID, "to", next, "error", err)
		return false
	}
	r.fire(Event{ContractID: c.ID, From: c.State, To: next, At: now, Reason: reason})
	return true
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
