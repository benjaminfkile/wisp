// Package gpu provides an exclusive, concurrency-safe allocator over a fixed
// inventory of GPU device IDs.
//
// GPUs are leased as WHOLE devices: two live contracts must never share a device
// ID (see docs/DESIGN.md §7). Wisp owns the assignment — the marketplace above
// only ever asks for a COUNT, and the allocator turns that count into a set of
// specific, currently-free device IDs, reserving them exclusively until the
// owning contract reaches a terminal state and frees them.
//
// The allocator is deliberately BACKEND-AGNOSTIC: it deals in opaque device ID
// strings only and carries no runc/docker/VFIO assumptions. The same allocator
// hands IDs to today's runc/DeviceRequests path and, later, to a Kata + VFIO
// passthrough path — neither leaks into this package (the KATA-INTENT rule).
package gpu

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrInsufficientDevices is the sentinel returned (wrapped) by Allocate when
// fewer devices are free than requested, so a caller can match it with
// errors.Is while the create path maps it to a clean rejection.
var ErrInsufficientDevices = errors.New("gpu: insufficient free devices")

// InsufficientDevicesError reports an allocation that could not be satisfied
// because too few devices were free. It wraps ErrInsufficientDevices so callers
// can match with errors.Is and carries the counts for a precise message.
type InsufficientDevicesError struct {
	// Requested is the number of devices the caller asked for.
	Requested int

	// Available is the number of free devices at the time of the request.
	Available int
}

func (e *InsufficientDevicesError) Error() string {
	return fmt.Sprintf("gpu: insufficient free devices: requested %d, %d available", e.Requested, e.Available)
}

func (e *InsufficientDevicesError) Unwrap() error { return ErrInsufficientDevices }

// Allocator assigns whole GPU devices exclusively from a fixed inventory. It is
// safe for concurrent use by multiple goroutines; every method takes the
// allocator's lock so concurrent creates can never be handed the same device.
type Allocator struct {
	mu sync.Mutex

	// order is the full inventory of known device IDs in detection order,
	// deduplicated. Allocation walks it in order so assignment is deterministic.
	order []string

	// known is the membership set for order, so Reserve can tell a device that is
	// simply occupied from one this host no longer detects.
	known map[string]bool

	// occupied is the subset of known IDs currently assigned to a live contract.
	// It is always a subset of known: only Allocate and Reserve add to it, both of
	// which draw exclusively from known.
	occupied map[string]bool
}

// NewAllocator builds an allocator over deviceIDs (the IDs detected on this host,
// in enumeration order). Empty and duplicate IDs are dropped so the inventory is
// a clean set; the original order of the first occurrence is preserved. The
// returned allocator starts with every device free.
func NewAllocator(deviceIDs []string) *Allocator {
	a := &Allocator{
		known:    make(map[string]bool, len(deviceIDs)),
		occupied: make(map[string]bool),
	}
	for _, id := range deviceIDs {
		if id == "" || a.known[id] {
			continue
		}
		a.known[id] = true
		a.order = append(a.order, id)
	}
	return a
}

// Allocate reserves n currently-free devices exclusively and returns their IDs
// in detection order. It is atomic: either all n are reserved or none are. A
// request for more devices than are free fails with an *InsufficientDevicesError
// (wrapping ErrInsufficientDevices) and reserves nothing. A non-positive n
// reserves nothing and returns a nil slice with no error.
func (a *Allocator) Allocate(n int) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if n <= 0 {
		return nil, nil
	}
	free := a.freeLocked()
	if len(free) < n {
		return nil, &InsufficientDevicesError{Requested: n, Available: len(free)}
	}
	picked := make([]string, n)
	copy(picked, free[:n])
	for _, id := range picked {
		a.occupied[id] = true
	}
	return picked, nil
}

// Free returns the given device IDs to the free pool. It is idempotent: freeing
// a device that is already free — or an ID this allocator never knew — is a
// no-op, so a contract killed twice (e.g. released and then swept by the reaper)
// never double-frees a device into a state that could be re-handed to two
// leases.
func (a *Allocator) Free(ids []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, id := range ids {
		delete(a.occupied, id)
	}
}

// Reserve marks the given device IDs occupied during startup reconcile, so a
// restarted wispd never re-hands a device already held by a pre-existing lease.
// It returns the IDs it could NOT reserve because they name a device no longer
// present in this host's inventory (e.g. a GPU removed since the container was
// launched); the caller logs and skips those rather than crashing. Reserving an
// already-occupied known device is a no-op (idempotent).
func (a *Allocator) Reserve(ids []string) (unknown []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, id := range ids {
		if !a.known[id] {
			unknown = append(unknown, id)
			continue
		}
		a.occupied[id] = true
	}
	return unknown
}

// Available reports how many devices are currently free.
func (a *Allocator) Available() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.order) - len(a.occupied)
}

// Occupied returns the currently-assigned device IDs, sorted for a stable order.
// It is intended for introspection and test assertions.
func (a *Allocator) Occupied() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.occupied))
	for id := range a.occupied {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Devices returns the full inventory of known device IDs in detection order. The
// result is a fresh copy safe for the caller to retain.
func (a *Allocator) Devices() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.order))
	copy(out, a.order)
	return out
}

// freeLocked returns the free device IDs in detection order. The caller must
// hold a.mu.
func (a *Allocator) freeLocked() []string {
	out := make([]string, 0, len(a.order))
	for _, id := range a.order {
		if !a.occupied[id] {
			out = append(out, id)
		}
	}
	return out
}
