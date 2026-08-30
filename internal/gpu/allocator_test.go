package gpu

import (
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
)

// NewAllocator drops empty and duplicate device IDs while preserving the order
// of first occurrence, so the inventory is a clean, deterministic set.
func TestNewAllocatorDedupesAndOrders(t *testing.T) {
	a := NewAllocator([]string{"gpu-b", "", "gpu-a", "gpu-b", "gpu-a", "gpu-c"})
	got := a.Devices()
	want := []string{"gpu-b", "gpu-a", "gpu-c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Devices() = %v, want %v", got, want)
	}
	if a.Available() != 3 {
		t.Errorf("Available() = %d, want 3", a.Available())
	}
}

// Allocate hands out specific free IDs in detection order and marks them
// occupied; a second allocation never returns a device the first one holds.
func TestAllocateIsExclusiveAndOrdered(t *testing.T) {
	a := NewAllocator([]string{"gpu-0", "gpu-1", "gpu-2"})

	first, err := a.Allocate(2)
	if err != nil {
		t.Fatalf("Allocate(2): %v", err)
	}
	if !reflect.DeepEqual(first, []string{"gpu-0", "gpu-1"}) {
		t.Fatalf("first allocation = %v, want [gpu-0 gpu-1]", first)
	}
	if a.Available() != 1 {
		t.Errorf("Available() = %d, want 1", a.Available())
	}

	second, err := a.Allocate(1)
	if err != nil {
		t.Fatalf("Allocate(1): %v", err)
	}
	if !reflect.DeepEqual(second, []string{"gpu-2"}) {
		t.Fatalf("second allocation = %v, want [gpu-2]", second)
	}
	// No overlap between the two live allocations.
	for _, id := range second {
		for _, held := range first {
			if id == held {
				t.Fatalf("device %q assigned to two live allocations", id)
			}
		}
	}
}

// A non-positive count reserves nothing and is not an error.
func TestAllocateZeroOrNegative(t *testing.T) {
	a := NewAllocator([]string{"gpu-0"})
	for _, n := range []int{0, -1} {
		got, err := a.Allocate(n)
		if err != nil {
			t.Errorf("Allocate(%d) err = %v, want nil", n, err)
		}
		if got != nil {
			t.Errorf("Allocate(%d) = %v, want nil", n, got)
		}
	}
	if a.Available() != 1 {
		t.Errorf("Available() = %d, want 1 (nothing reserved)", a.Available())
	}
}

// Requesting more devices than are free fails with a typed, matchable error and
// reserves nothing (the allocation is atomic).
func TestAllocateInsufficientDevices(t *testing.T) {
	a := NewAllocator([]string{"gpu-0", "gpu-1"})

	got, err := a.Allocate(3)
	if got != nil {
		t.Fatalf("Allocate(3) = %v, want nil on failure", got)
	}
	if !errors.Is(err, ErrInsufficientDevices) {
		t.Fatalf("err = %v, want ErrInsufficientDevices", err)
	}
	var ide *InsufficientDevicesError
	if !errors.As(err, &ide) {
		t.Fatalf("err = %v, want *InsufficientDevicesError", err)
	}
	if ide.Requested != 3 || ide.Available != 2 {
		t.Errorf("error counts = requested %d available %d, want 3/2", ide.Requested, ide.Available)
	}
	// Nothing was reserved by the failed request.
	if a.Available() != 2 {
		t.Errorf("Available() = %d, want 2 (failed allocation reserved nothing)", a.Available())
	}
}

// Free returns devices to the pool and is idempotent: freeing an already-free
// device, or an unknown id, is a harmless no-op - a contract killed twice never
// corrupts occupancy.
func TestFreeIsIdempotent(t *testing.T) {
	a := NewAllocator([]string{"gpu-0", "gpu-1"})
	held, err := a.Allocate(2)
	if err != nil {
		t.Fatalf("Allocate(2): %v", err)
	}
	if a.Available() != 0 {
		t.Fatalf("Available() = %d, want 0", a.Available())
	}

	a.Free(held)
	if a.Available() != 2 {
		t.Fatalf("Available() = %d after free, want 2", a.Available())
	}
	// Free the same ids again, plus an unknown id: no-op, no panic, no negative
	// occupancy.
	a.Free(held)
	a.Free([]string{"gpu-unknown"})
	if a.Available() != 2 {
		t.Fatalf("Available() = %d after double free, want 2", a.Available())
	}
	// The freed devices can be re-allocated.
	again, err := a.Allocate(2)
	if err != nil {
		t.Fatalf("re-Allocate(2): %v", err)
	}
	sort.Strings(again)
	if !reflect.DeepEqual(again, []string{"gpu-0", "gpu-1"}) {
		t.Fatalf("re-allocation = %v, want both devices back", again)
	}
}

// Reserve marks known devices occupied (for startup reconcile) and reports the
// ids it could not reserve because the host no longer detects them.
func TestReserveMarksKnownAndReportsUnknown(t *testing.T) {
	a := NewAllocator([]string{"gpu-0", "gpu-1", "gpu-2"})

	unknown := a.Reserve([]string{"gpu-1", "gpu-gone"})
	if !reflect.DeepEqual(unknown, []string{"gpu-gone"}) {
		t.Fatalf("unknown = %v, want [gpu-gone]", unknown)
	}
	if got := a.Occupied(); !reflect.DeepEqual(got, []string{"gpu-1"}) {
		t.Fatalf("Occupied() = %v, want [gpu-1]", got)
	}
	// Reserving again is idempotent.
	if u := a.Reserve([]string{"gpu-1"}); len(u) != 0 {
		t.Fatalf("re-Reserve unknown = %v, want none", u)
	}

	// A reserved device is not handed out: allocating the remaining free count
	// succeeds, one more fails.
	got, err := a.Allocate(2)
	if err != nil {
		t.Fatalf("Allocate(2): %v", err)
	}
	if !reflect.DeepEqual(got, []string{"gpu-0", "gpu-2"}) {
		t.Fatalf("allocation = %v, want [gpu-0 gpu-2] (gpu-1 reserved)", got)
	}
	if _, err := a.Allocate(1); !errors.Is(err, ErrInsufficientDevices) {
		t.Fatalf("Allocate(1) err = %v, want ErrInsufficientDevices", err)
	}
}

// Under concurrent single-device allocations the allocator never hands the same
// device to two callers, never over-subscribes the inventory, and reports the
// exact number of successful grants as failures for the rest.
func TestAllocateConcurrentExclusive(t *testing.T) {
	const devices = 8
	const goroutines = 64

	ids := make([]string, devices)
	for i := range ids {
		ids[i] = "gpu-" + string(rune('a'+i))
	}
	a := NewAllocator(ids)

	var (
		mu       sync.Mutex
		granted  []string
		failures int
		wg       sync.WaitGroup
	)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := a.Allocate(1)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if !errors.Is(err, ErrInsufficientDevices) {
					t.Errorf("unexpected error: %v", err)
				}
				failures++
				return
			}
			granted = append(granted, got...)
		}()
	}
	close(start)
	wg.Wait()

	if len(granted) != devices {
		t.Fatalf("granted %d devices, want exactly %d", len(granted), devices)
	}
	if failures != goroutines-devices {
		t.Fatalf("failures = %d, want %d", failures, goroutines-devices)
	}
	// Every granted device is distinct - no device handed to two goroutines.
	seen := map[string]bool{}
	for _, id := range granted {
		if seen[id] {
			t.Fatalf("device %q granted to two goroutines", id)
		}
		seen[id] = true
	}
	if a.Available() != 0 {
		t.Fatalf("Available() = %d, want 0 (all devices held)", a.Available())
	}
}
