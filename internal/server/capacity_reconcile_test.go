package server

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/benjaminfkile/wisp/internal/bus"
	"github.com/benjaminfkile/wisp/internal/capacity"
	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// leasedCap builds a running LeasedContainer carrying the contract, expiry, and
// (when non-empty) the capacity labels, so a reconcile test can drive the parse
// path directly without provisioning a real container. Running=true because
// only running containers are adopted (a stopped one is removed and its
// capacity is never reserved - see Reconcile).
func leasedCap(containerID, contractID, expiresAt, cpus, memoryMB string) runtime.LeasedContainer {
	labels := map[string]string{contractLabel: contractID}
	if expiresAt != "" {
		labels[expiresAtLabel] = expiresAt
	}
	if cpus != "" {
		labels[cpusLabel] = cpus
	}
	if memoryMB != "" {
		labels[memoryMBLabel] = memoryMB
	}
	return runtime.LeasedContainer{ID: containerID, Labels: labels, Running: true}
}

// A create writes the POST-CLAMP reserved cpus/memory on the container labels, and
// a fresh wispd that reconciles those surviving containers rebuilds an IDENTICAL
// aggregate allocator state - same contract count, same used cpus, same used
// memory - so a restart never under-counts and oversubscribes the host.
func TestCapacityLabelsRoundTripThroughReconcile(t *testing.T) {
	pol := budgetPolicy(10, 8, 4096)
	b, h, _, fake := capBroker(t, pol)

	createContract(t, h, `{"ttl_seconds":600,"resources":{"cpus":2,"memory_mb":512}}`)
	createContract(t, h, `{"ttl_seconds":600,"resources":{"cpus":1.5,"memory_mb":256}}`)

	// Snapshot the live allocator before the "restart".
	wantContracts := b.cap.ActiveContracts()
	wantCPUs := b.cap.UsedCPUs()
	wantMem := b.cap.UsedMemoryMB()
	if wantContracts != 2 || wantCPUs != 3.5 || wantMem != 768 {
		t.Fatalf("pre-restart allocator = (%d contracts, %v cpus, %d mem), want (2, 3.5, 768)",
			wantContracts, wantCPUs, wantMem)
	}

	// Restart: a fresh store and broker (hence a fresh, empty capacity allocator)
	// over the SAME fake, whose containers still carry the create-time labels.
	store2 := contract.NewStore()
	b2 := newBroker(store2, fake, budgetPolicy(10, 8, 4096), bus.New(nil), discardLogger(), "")
	Reconcile(context.Background(), store2, fake, b2.alloc, b2.cap, discardLogger())

	if got := b2.cap.ActiveContracts(); got != wantContracts {
		t.Errorf("post-reconcile ActiveContracts() = %d, want %d", got, wantContracts)
	}
	if got := b2.cap.UsedCPUs(); got != wantCPUs {
		t.Errorf("post-reconcile UsedCPUs() = %v, want %v", got, wantCPUs)
	}
	if got := b2.cap.UsedMemoryMB(); got != wantMem {
		t.Errorf("post-reconcile UsedMemoryMB() = %d, want %d", got, wantMem)
	}
}

// Adopted contracts count toward max_contracts even when their capacity labels are
// missing entirely (e.g. a container from before this task): each still consumes
// one contract slot, reserving 0 on the cpu/memory dimensions, so the aggregate
// contract budget is enforced across a restart.
func TestReconcileCountsContractsWithMissingCapacityLabels(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	cap := capacity.NewAllocator(2, 0, 0) // two contract slots; cpu/mem unbudgeted
	expiry := strconv.FormatInt(time.Unix(1_900_000_000, 0).Unix(), 10)
	fake.LeasedContainers = []runtime.LeasedContainer{
		leasedCap("c1", "k1", expiry, "", ""), // no wisp.cpus / wisp.memory_mb labels
		leasedCap("c2", "k2", expiry, "", ""),
	}

	Reconcile(context.Background(), store, fake, nil, cap, discardLogger())

	if got := len(store.List()); got != 2 {
		t.Fatalf("adopted contracts = %d, want 2", got)
	}
	if got := cap.ActiveContracts(); got != 2 {
		t.Errorf("ActiveContracts() = %d, want 2 (both count toward max_contracts)", got)
	}
	if got := cap.UsedCPUs(); got != 0 {
		t.Errorf("UsedCPUs() = %v, want 0 (missing label => 0)", got)
	}
	if got := cap.UsedMemoryMB(); got != 0 {
		t.Errorf("UsedMemoryMB() = %d, want 0 (missing label => 0)", got)
	}
	// Both slots are now held, so a further reservation is rejected on the contract
	// budget - the reconciled leases really do count against max_contracts.
	if cap.TryReserve(0, 0) {
		t.Error("TryReserve succeeded after two reconciled leases filled max_contracts=2")
	}
}

// A malformed capacity label never fails the reconcile: the contract is still
// adopted and counted, and only the malformed dimension falls back to 0 while the
// well-formed sibling dimension is reserved as written.
func TestReconcileToleratesMalformedCapacityLabels(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	cap := capacity.NewAllocator(0, 100, 100000) // contracts unbudgeted; cpu/mem budgeted
	expiry := strconv.FormatInt(time.Unix(1_900_000_000, 0).Unix(), 10)
	fake.LeasedContainers = []runtime.LeasedContainer{
		leasedCap("c1", "k1", expiry, "not-a-float", "512"), // cpus malformed, memory good
		leasedCap("c2", "k2", expiry, "2", "nan-mb"),        // cpus good, memory malformed
	}

	Reconcile(context.Background(), store, fake, nil, cap, discardLogger())

	if got := len(store.List()); got != 2 {
		t.Fatalf("adopted contracts = %d, want 2 (malformed labels must not drop a lease)", got)
	}
	if got := cap.ActiveContracts(); got != 2 {
		t.Errorf("ActiveContracts() = %d, want 2", got)
	}
	// c1 contributes memory 512 (cpus dropped to 0); c2 contributes cpus 2 (memory
	// dropped to 0): the parsable dimension of each survives.
	if got := cap.UsedCPUs(); got != 2 {
		t.Errorf("UsedCPUs() = %v, want 2 (only c2's parsable cpus)", got)
	}
	if got := cap.UsedMemoryMB(); got != 512 {
		t.Errorf("UsedMemoryMB() = %d, want 512 (only c1's parsable memory)", got)
	}
}

// After a restart, a create is correctly rejected with a 409 when the re-Reserved
// capacity of surviving leases leaves no room - proof the reconcile's re-Reserve
// really guards the budget, not just the in-memory count. A create that still fits
// the rebuilt headroom is admitted.
func TestPostRestartCreate409sWhenReReservedCapacityFull(t *testing.T) {
	// 4 total cpus; contracts and memory unbudgeted so the cpu budget is the gate.
	b, h, _, fake := capBroker(t, budgetPolicy(0, 4, 0))
	createContract(t, h, `{"ttl_seconds":600,"resources":{"cpus":3}}`) // reserves 3 of 4
	if got := b.cap.UsedCPUs(); got != 3 {
		t.Fatalf("pre-restart UsedCPUs() = %v, want 3", got)
	}

	// Restart over the same fake and re-Reserve from the surviving container's labels.
	store2 := contract.NewStore()
	b2 := newBroker(store2, fake, budgetPolicy(0, 4, 0), bus.New(nil), discardLogger(), "")
	Reconcile(context.Background(), store2, fake, b2.alloc, b2.cap, discardLogger())
	if got := b2.cap.UsedCPUs(); got != 3 {
		t.Fatalf("post-reconcile UsedCPUs() = %v, want 3 (label round-trip)", got)
	}
	mux2 := http.NewServeMux()
	b2.routes(mux2)

	// 3 of 4 cpus are held after the restart, so a 2-cpu create would exceed the
	// budget (3+2 > 4): a 409 at capacity, distinct from a per-lease over-ask.
	rec := do(t, mux2, http.MethodPost, "/contracts", `{"ttl_seconds":600,"resources":{"cpus":2}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("post-restart create status = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
	assertAtCapacity(t, rec.Body.Bytes())

	// A 1-cpu create still fits the rebuilt headroom (3+1 = 4) and is admitted.
	createContract(t, mux2, `{"ttl_seconds":600,"resources":{"cpus":1}}`)
	if got := b2.cap.UsedCPUs(); got != 4 {
		t.Errorf("UsedCPUs() = %v, want 4 after the fitting create", got)
	}
}
