// Package capacity provides a concurrency-safe allocator over a host's AGGREGATE
// capacity budgets — the number of concurrent contracts, the total reserved CPU,
// and the total reserved memory across all live leases (see docs/DESIGN.md §7).
//
// It is the aggregate sibling of internal/gpu.Allocator: where the GPU allocator
// hands out whole devices exclusively, this allocator admits a lease only when it
// fits inside every configured host budget at once, tracking the running totals so
// the create path can reject an over-subscription with a 409 and every terminal
// path can return the lease's reserved capacity. A single shared instance is
// threaded through create, release, provision-failure, and the reaper, exactly as
// the GPU allocator is.
//
// The allocator deals in plain numbers only — a contract count, a CPU fraction,
// and a memory figure in mebibytes — and carries no contract, runtime, or policy
// assumptions. Each budget is independent: a budget of 0 for a dimension means it
// is UNBUDGETED and never blocks a reservation, mirroring how a zero numeric limit
// means "no cap" throughout the policy layer.
package capacity

import "sync"

// Allocator admits leases against a host's aggregate capacity budgets and tracks
// the running usage. It is safe for concurrent use by multiple goroutines: every
// method takes the allocator's lock, so concurrent creates can never oversubscribe
// a budget (the check and the commit are one critical section).
type Allocator struct {
	mu sync.Mutex

	// Budgets. A zero budget leaves that dimension unbudgeted — TryReserve never
	// rejects on it — matching how a zero numeric limit means "no cap" elsewhere.
	maxContracts  int
	totalCPUs     float64
	totalMemoryMB int

	// Running usage reserved across all leases currently holding a reservation.
	// contracts counts one per live lease (bounded by maxContracts); cpus and
	// memoryMB sum the POST-CLAMP amounts actually applied to each container.
	contracts int
	cpus      float64
	memoryMB  int
}

// NewAllocator builds an allocator over the operator's aggregate budgets: the max
// concurrent contracts, the total CPU (as a fraction of host cores), and the total
// memory in mebibytes. A zero budget for any dimension means that dimension is
// unbudgeted (never checked). The returned allocator starts with zero usage.
func NewAllocator(maxContracts int, totalCPUs float64, totalMemoryMB int) *Allocator {
	return &Allocator{
		maxContracts:  maxContracts,
		totalCPUs:     totalCPUs,
		totalMemoryMB: totalMemoryMB,
	}
}

// TryReserve attempts to reserve one lease's worth of capacity — one contract slot
// plus cpus and memoryMB — against all three budgets ATOMICALLY and all-or-nothing:
// it succeeds (returning true and adding to usage) only when every BUDGETED
// dimension has room for the addition; if any budgeted dimension would be exceeded
// it reserves nothing and returns false. A budget of 0 for a dimension is
// unbudgeted and never blocks the reservation.
//
// cpus and memoryMB are the POST-CLAMP amounts actually applied to the container
// (see the broker's create path): a request that omits a dimension has already
// inherited the per-lease max when one is configured, or 0 when that dimension has
// no per-lease cap. Reserving 0 on a dimension leaves that dimension's usage
// unchanged, so an unbounded container on an unbudgeted dimension stays uncounted;
// it still consumes one contract slot.
func (a *Allocator) TryReserve(cpus float64, memoryMB int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.maxContracts > 0 && a.contracts+1 > a.maxContracts {
		return false
	}
	if a.totalCPUs > 0 && a.cpus+cpus > a.totalCPUs {
		return false
	}
	if a.totalMemoryMB > 0 && a.memoryMB+memoryMB > a.totalMemoryMB {
		return false
	}
	a.contracts++
	a.cpus += cpus
	a.memoryMB += memoryMB
	return true
}

// Free returns one lease's reserved capacity — one contract slot plus cpus and
// memoryMB — to the pool. It is designed to be called EXACTLY ONCE PER CONTRACT,
// with the same post-clamp amounts that were reserved: the create path frees on
// the single path a create fails, and each terminal transition (release, reaper
// expiry, provision failure) frees once, gated on the winning state-machine
// transition so a released-then-reaped lease frees only once. As a safety net —
// mirroring how gpu.Free can never corrupt occupancy — it clamps every dimension
// at zero, so a redundant or mismatched free can never drive usage negative and
// re-inflate apparent headroom.
func (a *Allocator) Free(cpus float64, memoryMB int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.contracts > 0 {
		a.contracts--
	}
	if a.cpus -= cpus; a.cpus < 0 {
		a.cpus = 0
	}
	if a.memoryMB -= memoryMB; a.memoryMB < 0 {
		a.memoryMB = 0
	}
}

// Reserve records capacity ALREADY HELD by a lease rediscovered at startup
// reconcile, rebuilding aggregate usage from surviving containers before any new
// create is admitted — the aggregate mirror of gpu.Allocator.Reserve. Unlike
// TryReserve it does not check the budgets: a reconciled lease already holds its
// capacity, so it is counted unconditionally (usage may legitimately sit at or
// above a since-lowered budget until those leases drain). It is wired fully in the
// next task (#567), which persists a lease's reserved cpus/memory on its container
// labels so the reconcile can re-Reserve them here.
func (a *Allocator) Reserve(cpus float64, memoryMB int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.contracts++
	a.cpus += cpus
	a.memoryMB += memoryMB
}

// ActiveContracts reports how many leases currently hold a reservation.
func (a *Allocator) ActiveContracts() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.contracts
}

// UsedCPUs reports the aggregate CPU (as a fraction of host cores) currently
// reserved across all live leases.
func (a *Allocator) UsedCPUs() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cpus
}

// UsedMemoryMB reports the aggregate memory (mebibytes) currently reserved across
// all live leases.
func (a *Allocator) UsedMemoryMB() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.memoryMB
}
