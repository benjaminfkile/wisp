package capacity

import (
	"sync"
	"testing"
)

// A fresh allocator starts with zero usage on every dimension.
func TestNewAllocatorStartsEmpty(t *testing.T) {
	a := NewAllocator(5, 16, 32768)
	if a.ActiveContracts() != 0 || a.UsedCPUs() != 0 || a.UsedMemoryMB() != 0 {
		t.Fatalf("fresh usage = {%d, %v, %d}, want all zero",
			a.ActiveContracts(), a.UsedCPUs(), a.UsedMemoryMB())
	}
}

// TryReserve admits leases up to each budget and rejects the one that would push
// any dimension over, all-or-nothing, without moving usage on a rejection.
func TestTryReserveEnforcesEachBudget(t *testing.T) {
	// contract-count budget: three slots.
	t.Run("contract count", func(t *testing.T) {
		a := NewAllocator(3, 0, 0)
		for i := 0; i < 3; i++ {
			if !a.TryReserve(1, 100) {
				t.Fatalf("reserve %d rejected, want admitted", i)
			}
		}
		if a.TryReserve(1, 100) {
			t.Fatal("4th reserve admitted, want rejected (contract budget exhausted)")
		}
		if a.ActiveContracts() != 3 {
			t.Errorf("ActiveContracts() = %d, want 3 (rejected reserve counted nothing)", a.ActiveContracts())
		}
	})

	// total-cpu budget: 4 cpus, so a 2.5 + 2.0 pair overflows.
	t.Run("cpu", func(t *testing.T) {
		a := NewAllocator(0, 4, 0)
		if !a.TryReserve(2.5, 0) {
			t.Fatal("first cpu reserve rejected, want admitted")
		}
		if a.TryReserve(2.0, 0) {
			t.Fatal("second cpu reserve admitted, want rejected (2.5+2.0 > 4)")
		}
		if a.UsedCPUs() != 2.5 {
			t.Errorf("UsedCPUs() = %v, want 2.5 (rejected reserve added nothing)", a.UsedCPUs())
		}
		// A reservation that exactly fills the remaining headroom is admitted.
		if !a.TryReserve(1.5, 0) {
			t.Fatal("exact-fit cpu reserve rejected, want admitted (2.5+1.5 == 4)")
		}
	})

	// total-memory budget: 512 MiB.
	t.Run("memory", func(t *testing.T) {
		a := NewAllocator(0, 0, 512)
		if !a.TryReserve(0, 400) {
			t.Fatal("first memory reserve rejected, want admitted")
		}
		if a.TryReserve(0, 200) {
			t.Fatal("second memory reserve admitted, want rejected (400+200 > 512)")
		}
		if a.UsedMemoryMB() != 400 {
			t.Errorf("UsedMemoryMB() = %d, want 400 (rejected reserve added nothing)", a.UsedMemoryMB())
		}
	})
}

// A reservation is all-or-nothing across the three budgets: if ANY dimension is
// full the whole reserve is rejected and no dimension moves, even when the others
// had room.
func TestTryReserveAllOrNothing(t *testing.T) {
	a := NewAllocator(10, 10, 512) // plenty of contract slots and cpu, tight memory
	if !a.TryReserve(1, 500) {
		t.Fatal("first reserve rejected, want admitted")
	}
	// cpu and contract-count have room, but memory (500+100 > 512) does not.
	if a.TryReserve(1, 100) {
		t.Fatal("reserve admitted, want rejected (memory over budget)")
	}
	if a.ActiveContracts() != 1 || a.UsedCPUs() != 1 || a.UsedMemoryMB() != 500 {
		t.Fatalf("usage after rejected all-or-nothing reserve = {%d, %v, %d}, want {1, 1, 500}",
			a.ActiveContracts(), a.UsedCPUs(), a.UsedMemoryMB())
	}
}

// A budget of 0 leaves that dimension unbudgeted: TryReserve never rejects on it,
// no matter how much is reserved.
func TestZeroBudgetIsUnlimited(t *testing.T) {
	a := NewAllocator(0, 0, 0)
	for i := 0; i < 1000; i++ {
		if !a.TryReserve(1000, 1_000_000) {
			t.Fatalf("reserve %d rejected under all-unlimited budgets", i)
		}
	}
	if a.ActiveContracts() != 1000 {
		t.Errorf("ActiveContracts() = %d, want 1000", a.ActiveContracts())
	}
}

// Free returns a lease's reserved capacity so it can be reserved again, and never
// drives usage negative on a redundant or mismatched free.
func TestFreeReturnsCapacityAndClampsAtZero(t *testing.T) {
	a := NewAllocator(1, 4, 512) // one slot only

	if !a.TryReserve(4, 512) {
		t.Fatal("reserve rejected, want admitted")
	}
	if a.TryReserve(0, 0) {
		t.Fatal("second reserve admitted, want rejected (only one contract slot)")
	}

	a.Free(4, 512)
	if a.ActiveContracts() != 0 || a.UsedCPUs() != 0 || a.UsedMemoryMB() != 0 {
		t.Fatalf("usage after free = {%d, %v, %d}, want all zero",
			a.ActiveContracts(), a.UsedCPUs(), a.UsedMemoryMB())
	}
	// The freed slot and budget can be reserved again.
	if !a.TryReserve(4, 512) {
		t.Fatal("re-reserve after free rejected, want admitted")
	}

	// A redundant free (e.g. a released-then-reaped race that slipped past the
	// caller's gating) clamps at zero rather than corrupting usage into negatives.
	a.Free(4, 512)
	a.Free(4, 512)
	if a.ActiveContracts() != 0 || a.UsedCPUs() != 0 || a.UsedMemoryMB() != 0 {
		t.Fatalf("usage after double free = {%d, %v, %d}, want all clamped to zero",
			a.ActiveContracts(), a.UsedCPUs(), a.UsedMemoryMB())
	}
}

// Reserve records capacity a reconciled lease already holds without checking the
// budgets, so startup rebuilds usage even to at-or-above a since-lowered budget.
func TestReserveIsUnconditional(t *testing.T) {
	a := NewAllocator(1, 4, 512)
	a.Reserve(6, 1024) // above both the cpu and memory budgets
	a.Reserve(6, 1024)
	if a.ActiveContracts() != 2 || a.UsedCPUs() != 12 || a.UsedMemoryMB() != 2048 {
		t.Fatalf("usage after unconditional Reserve = {%d, %v, %d}, want {2, 12, 2048}",
			a.ActiveContracts(), a.UsedCPUs(), a.UsedMemoryMB())
	}
	// With usage already over budget, a new lease is still rejected.
	if a.TryReserve(0, 0) {
		t.Fatal("TryReserve admitted while over budget, want rejected")
	}
}

// Under concurrent single-slot reservations against a fixed contract budget the
// allocator admits exactly the budget and rejects the rest, never oversubscribing
// - the aggregate mirror of the GPU allocator's exclusivity race test.
func TestTryReserveConcurrentCannotOversubscribe(t *testing.T) {
	const budget = 8
	const goroutines = 64

	a := NewAllocator(budget, 0, 0)

	var (
		mu        sync.Mutex
		admitted  int
		rejected  int
		wg        sync.WaitGroup
		startGate = make(chan struct{})
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startGate
			ok := a.TryReserve(1, 100)
			mu.Lock()
			defer mu.Unlock()
			if ok {
				admitted++
			} else {
				rejected++
			}
		}()
	}
	close(startGate)
	wg.Wait()

	if admitted != budget {
		t.Fatalf("admitted %d, want exactly %d (the contract budget)", admitted, budget)
	}
	if rejected != goroutines-budget {
		t.Fatalf("rejected %d, want %d", rejected, goroutines-budget)
	}
	if a.ActiveContracts() != budget {
		t.Fatalf("ActiveContracts() = %d, want %d (no oversubscription)", a.ActiveContracts(), budget)
	}
}

// Concurrent reservations against a tight CPU budget never let total reserved CPU
// exceed the budget, even when many goroutines race the last slice of headroom.
func TestTryReserveConcurrentCPUBudget(t *testing.T) {
	const goroutines = 100
	// Budget 10, each reserve wants 1.0 cpu → at most 10 admitted.
	a := NewAllocator(0, 10, 0)

	var wg sync.WaitGroup
	startGate := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startGate
			a.TryReserve(1, 0)
		}()
	}
	close(startGate)
	wg.Wait()

	if a.UsedCPUs() > 10 {
		t.Fatalf("UsedCPUs() = %v, want <= 10 (never oversubscribed)", a.UsedCPUs())
	}
	if a.UsedCPUs() != 10 {
		t.Fatalf("UsedCPUs() = %v, want exactly 10 (budget fully but not over used)", a.UsedCPUs())
	}
}
