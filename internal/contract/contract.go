// Package contract models a Wisp contract: the unit of everything (see
// docs/DESIGN.md §4). A contract is a short-lived lease over one ephemeral
// container, tracked through a small lifecycle state machine and held in a
// thread-safe in-memory store.
//
// The HTTP broker (see internal/server) is the sole consumer: create records a
// new contract here, provisioning/release drive its state machine transitions,
// and the reaper expires contracts through the same store.
package contract

import (
	"errors"
	"fmt"
	"time"
)

// State is a point in a contract's lifecycle. The legal progression (see
// docs/DESIGN.md §4) is:
//
//	requested → provisioning → ready → expiring → releasing → released
//	                                                        ↘ expired
//
// released and expired are terminal (the container is destroyed). A DELETE
// first fences the reaper off the contract by transitioning it to
// StateReleasing before killing the container, and only after the container
// is torn down does it complete the terminal transition to StateReleased.
// The TTL reaper may expire an active contract from any state where a
// container exists, and skips StateReleasing only for a short grace window
// after the fence goes up so the release handler owns the tear down; past
// the grace the release is presumed stuck (the handler crashed mid-release
// or its Kill hung) and the reaper expires the contract like any other
// non-terminal one so its capacity and GPUs return to the allocators.
type State string

const (
	// StateRequested is the initial state: the contract exists but no
	// container has been provisioned yet.
	StateRequested State = "requested"

	// StateProvisioning means the container is up and the userdata script is
	// running. Execs are rejected until the contract is ready.
	StateProvisioning State = "provisioning"

	// StateReady means the client may exec and open shells freely.
	StateReady State = "ready"

	// StateExpiring is a warning window a configurable lead time before the
	// TTL, giving the client a chance to exfiltrate work before the hard kill.
	StateExpiring State = "expiring"

	// StateReleasing is the transient fence the DELETE handler installs BEFORE
	// killing the container: taking it holds a per-contract lock over the tear
	// down window so the reaper's TTL / container-died sweeps cannot race the
	// release, double-kill the container ("removal already in progress"), and
	// then expire-and-purge the contract from under the handler (the exact
	// 2026-08-29 wisp-log 500 the state was added for). StateReleased is the
	// DELETE handler's normal successor (serializing the terminal side effects
	// so freeing capacity and GPUs and publishing contract.released happens
	// exactly once), and StateExpired is the reaper's escape hatch: if the
	// contract sits in StateReleasing past the reaper's release grace, the
	// release is presumed stuck and the reaper expires it so the lease's
	// capacity and GPUs return to the allocators rather than leaking until
	// restart.
	StateReleasing State = "releasing"

	// StateReleased is terminal: the client called DELETE and the container
	// was destroyed.
	StateReleased State = "released"

	// StateExpired is terminal: the TTL elapsed (or provisioning failed) and
	// the container was destroyed.
	StateExpired State = "expired"
)

// legalTransitions maps each state to the set of states it may move to
// directly. A state absent from a source's set is an illegal transition.
var legalTransitions = map[State]map[State]bool{
	StateRequested: {
		StateProvisioning: true,
		StateReleasing:    true, // DELETE before a container is booted
		StateExpired:      true, // provisioning never got off the ground
	},
	StateProvisioning: {
		StateReady:     true,
		StateReleasing: true, // DELETE mid-provision
		StateExpired:   true, // userdata failed, or TTL elapsed
	},
	StateReady: {
		StateExpiring:  true,
		StateReleasing: true, // DELETE while in use
		StateExpired:   true, // TTL elapsed with no lead-time warning
	},
	StateExpiring: {
		StateReleasing: true, // client released after the warning
		StateExpired:   true, // TTL elapsed
	},
	StateReleasing: {
		// The DELETE handler's final "mark released" is the normal successor,
		// serializing the terminal side effects (freeing capacity and GPUs) to
		// one caller. StateExpired is the reaper's stuck-release escape hatch:
		// past the release grace the reaper expires a contract still in
		// StateReleasing so its capacity is not leaked. Whichever side wins the
		// state-machine transition owns the terminal side effects.
		StateReleased: true,
		StateExpired:  true,
	},
	// StateReleased and StateExpired are terminal: no outgoing transitions.
	StateReleased: {},
	StateExpired:  {},
}

// Terminal reports whether s is a terminal state (no legal outgoing
// transitions).
func (s State) Terminal() bool {
	return s == StateReleased || s == StateExpired
}

// CanTransition reports whether moving from s to next is a legal transition.
func (s State) CanTransition(next State) bool {
	return legalTransitions[s][next]
}

// ErrIllegalTransition is returned when a state change is not permitted by the
// lifecycle state machine.
var ErrIllegalTransition = errors.New("contract: illegal state transition")

// IllegalTransitionError describes a rejected state change. It wraps
// ErrIllegalTransition so callers can match with errors.Is.
type IllegalTransitionError struct {
	From State
	To   State
}

func (e *IllegalTransitionError) Error() string {
	return fmt.Sprintf("contract: illegal state transition %s → %s", e.From, e.To)
}

func (e *IllegalTransitionError) Unwrap() error { return ErrIllegalTransition }

// Contract is the unit of everything: a lease over one ephemeral container.
type Contract struct {
	// ID uniquely identifies the contract.
	ID string

	// TTL is the requested lease duration. ExpiresAt is CreatedAt + TTL.
	TTL time.Duration

	// Image is the base image the container was booted from (see
	// docs/DESIGN.md §7). Opaque to this package.
	Image string

	// Isolation is the resolved isolation level for the lease (see
	// policy.Isolation), recorded so the runtime can select the container
	// runtime that satisfies it (see runtime.launchMechanism). Opaque to this
	// package; empty on contracts reconciled from Docker labels, where it did
	// not survive the restart.
	Isolation string

	// Meta is arbitrary client-supplied metadata echoed back on status reads.
	// Opaque to Wisp.
	Meta map[string]any

	// ExternalID is an opaque caller-supplied identifier (e.g. an upstream lease
	// id) that the create endpoint accepted. It is persisted on the container's
	// wisp.external_id label so it survives a wispd restart's reconcile, and is
	// surfaced on list/status reads so an upstream agent can re-associate leases
	// it owns after wispd restarts. Empty when the caller supplied none.
	ExternalID string

	// State is the current lifecycle state.
	State State

	// ContainerID is the runtime container backing this contract. Empty until
	// a container is provisioned.
	ContainerID string

	// GPUDeviceIDs are the whole GPU devices this contract was exclusively
	// assigned, by their stable device IDs (see docs/DESIGN.md §7). Empty when the
	// lease holds no GPUs. Wisp owns the assignment: the allocator hands out these
	// specific IDs at create time and reclaims them when the contract reaches a
	// terminal state. The set is fixed for the contract's life and is surfaced on
	// status reads as "gpus" and persisted on the container's wisp.gpus label so a
	// restart can rebuild allocator occupancy.
	GPUDeviceIDs []string

	// ReservedCPUs and ReservedMemoryMB are the POST-CLAMP host capacity this lease
	// reserved against the aggregate budgets (see internal/capacity): exactly the
	// cpus / memory applied to the container. They are recorded so every terminal
	// path can return the SAME amount it reserved to the capacity allocator. Zero
	// when the lease reserved nothing on that dimension (an unbudgeted per-lease
	// dimension). Persisted on the container's wisp.cpus and wisp.memory_mb labels
	// so a restart's reconcile re-Reserves the same amounts, and recovered onto
	// contracts adopted from those labels.
	ReservedCPUs     float64
	ReservedMemoryMB int

	// CreatedAt is when the contract was created.
	CreatedAt time.Time

	// ExpiresAt is when the TTL elapses (CreatedAt + TTL).
	ExpiresAt time.Time

	// ReleasingSince is the time the contract transitioned into StateReleasing,
	// stamped by the store's clock on the winning UpdateState. The reaper reads
	// it to enforce its release grace: a contract that has been in StateReleasing
	// for less than the grace is skipped (the DELETE handler owns the tear down),
	// past that the release is presumed stuck and the reaper expires the contract
	// so its capacity and GPUs return to the allocators. Zero for a contract that
	// has never entered StateReleasing.
	ReleasingSince time.Time

	// Token is the bearer token required on contract-scoped calls (see
	// docs/DESIGN.md §8).
	Token string
}
