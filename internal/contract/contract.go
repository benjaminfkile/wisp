// Package contract models a Wisp contract: the unit of everything (see
// docs/DESIGN.md §4). A contract is a short-lived lease over one ephemeral
// container, tracked through a small lifecycle state machine and held in a
// thread-safe in-memory store.
//
// Nothing here is wired into the HTTP surface yet; the contract lifecycle API
// (a later task) consumes this package.
package contract

import (
	"errors"
	"fmt"
	"time"
)

// State is a point in a contract's lifecycle. The legal progression (see
// docs/DESIGN.md §4) is:
//
//	requested → provisioning → ready → expiring → released | expired
//
// released and expired are terminal (the container is destroyed). A DELETE may
// release the contract early from any active state; the TTL reaper may expire
// it from any state where a container exists.
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
		StateReleased:     true, // DELETE before a container is booted
		StateExpired:      true, // provisioning never got off the ground
	},
	StateProvisioning: {
		StateReady:    true,
		StateReleased: true, // DELETE mid-provision
		StateExpired:  true, // userdata failed, or TTL elapsed
	},
	StateReady: {
		StateExpiring: true,
		StateReleased: true, // DELETE while in use
		StateExpired:  true, // TTL elapsed with no lead-time warning
	},
	StateExpiring: {
		StateReleased: true, // client released after the warning
		StateExpired:  true, // TTL elapsed
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
	// policy.Isolation), recorded so a later task can select the container
	// runtime that satisfies it. Opaque to this package; empty on contracts
	// reconciled from Docker labels, where it did not survive the restart.
	Isolation string

	// Meta is arbitrary client-supplied metadata echoed back on status reads.
	// Opaque to Wisp.
	Meta map[string]any

	// State is the current lifecycle state.
	State State

	// ContainerID is the runtime container backing this contract. Empty until
	// a container is provisioned.
	ContainerID string

	// CreatedAt is when the contract was created.
	CreatedAt time.Time

	// ExpiresAt is when the TTL elapses (CreatedAt + TTL).
	ExpiresAt time.Time

	// Token is the bearer token required on contract-scoped calls (see
	// docs/DESIGN.md §8).
	Token string
}
