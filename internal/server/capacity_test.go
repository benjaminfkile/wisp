package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benjaminfkile/wisp/internal/bus"
	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/policy"
	"github.com/benjaminfkile/wisp/internal/reaper"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// capBroker builds a broker over a plain (GPU-less) fake with the given policy,
// returning the broker (so a test can inspect the shared capacity allocator the
// handlers reserve into and free from) plus a mux serving its routes.
func capBroker(t *testing.T, pol *policy.Config) (*broker, http.Handler, *contract.Store, *runtime.Fake) {
	t.Helper()
	store := contract.NewStore()
	fake := runtime.NewFake()
	b := newBroker(store, fake, pol, bus.New(nil), discardLogger(), "")
	mux := http.NewServeMux()
	b.routes(mux)
	return b, mux, store, fake
}

// budgetPolicy returns the built-in default policy with the aggregate host budgets
// set and the per-lease cpu/memory caps cleared, so a reservation equals exactly
// the resources requested (no clamp surprises) for precise budget assertions.
func budgetPolicy(maxContracts int, totalCPUs float64, totalMemoryMB int) *policy.Config {
	pol := policy.Default()
	pol.Limits.MaxCPUs = 0
	pol.Limits.MaxMemoryMB = 0
	pol.Limits.MaxContracts = maxContracts
	pol.Limits.TotalCPUs = totalCPUs
	pol.Limits.TotalMemoryMB = totalMemoryMB
	return pol
}

// A create that would exceed the concurrent-contract budget is a 409 carrying the
// stable "at capacity" token, and it records no contract or reservation.
func TestCreateRejectedAtContractBudget(t *testing.T) {
	b, h, store, _ := capBroker(t, budgetPolicy(1, 0, 0)) // one contract slot

	createContract(t, h, `{"ttl_seconds":600}`) // fills the only slot

	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":600}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
	assertAtCapacity(t, rec.Body.Bytes())
	if got := b.cap.ActiveContracts(); got != 1 {
		t.Errorf("ActiveContracts() = %d, want 1 (rejected create reserved nothing)", got)
	}
	if n := len(store.List()); n != 1 {
		t.Errorf("store has %d contracts, want 1 (rejected create recorded nothing)", n)
	}
}

// A create that would exceed the aggregate CPU budget is a 409, distinct from a
// per-lease over-ask, and reserves nothing.
func TestCreateRejectedAtCPUBudget(t *testing.T) {
	b, h, _, _ := capBroker(t, budgetPolicy(0, 3, 0)) // 3 total cpus

	createContract(t, h, `{"ttl_seconds":600,"resources":{"cpus":2}}`) // reserves 2 of 3

	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":600,"resources":{"cpus":2}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (2+2 > 3 cpus) (body: %s)", rec.Code, rec.Body.String())
	}
	assertAtCapacity(t, rec.Body.Bytes())
	if got := b.cap.UsedCPUs(); got != 2 {
		t.Errorf("UsedCPUs() = %v, want 2 (rejected create added no cpu)", got)
	}
}

// A create that would exceed the aggregate memory budget is a 409 and reserves
// nothing.
func TestCreateRejectedAtMemoryBudget(t *testing.T) {
	b, h, _, _ := capBroker(t, budgetPolicy(0, 0, 512)) // 512 MiB total

	createContract(t, h, `{"ttl_seconds":600,"resources":{"memory_mb":400}}`)

	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":600,"resources":{"memory_mb":200}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (400+200 > 512 MiB) (body: %s)", rec.Code, rec.Body.String())
	}
	assertAtCapacity(t, rec.Body.Bytes())
	if got := b.cap.UsedMemoryMB(); got != 400 {
		t.Errorf("UsedMemoryMB() = %d, want 400 (rejected create added no memory)", got)
	}
}

// With every budget at 0 (unlimited) no create is ever rejected for capacity, no
// matter how many leases or how large.
func TestUnlimitedBudgetsNeverReject(t *testing.T) {
	b, h, _, _ := capBroker(t, budgetPolicy(0, 0, 0))

	for i := 0; i < 25; i++ {
		rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":600,"resources":{"cpus":1000,"memory_mb":1000000}}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %d status = %d, want 201 under unlimited budgets (body: %s)", i, rec.Code, rec.Body.String())
		}
	}
	if got := b.cap.ActiveContracts(); got != 25 {
		t.Errorf("ActiveContracts() = %d, want 25", got)
	}
}

// Releasing a contract returns its reserved capacity to the allocator, so the freed
// budget admits a new lease that was previously at the limit.
func TestReleaseFreesCapacity(t *testing.T) {
	b, h, _, _ := capBroker(t, budgetPolicy(1, 4, 512)) // one slot

	created := createContract(t, h, `{"ttl_seconds":600,"resources":{"cpus":2,"memory_mb":256}}`)
	if b.cap.ActiveContracts() != 1 || b.cap.UsedCPUs() != 2 || b.cap.UsedMemoryMB() != 256 {
		t.Fatalf("usage after create = {%d, %v, %d}, want {1, 2, 256}",
			b.cap.ActiveContracts(), b.cap.UsedCPUs(), b.cap.UsedMemoryMB())
	}

	rec := do(t, h, http.MethodDelete, "/contracts/"+created.ContractID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("release status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if b.cap.ActiveContracts() != 0 || b.cap.UsedCPUs() != 0 || b.cap.UsedMemoryMB() != 0 {
		t.Fatalf("usage after release = {%d, %v, %d}, want all zero",
			b.cap.ActiveContracts(), b.cap.UsedCPUs(), b.cap.UsedMemoryMB())
	}
	// The freed slot admits a new lease.
	if rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":600}`); rec.Code != http.StatusCreated {
		t.Fatalf("post-release create status = %d, want 201 (freed slot) (body: %s)", rec.Code, rec.Body.String())
	}
}

// A provisioning failure frees the reservation the create made, so a doomed create
// never strands host capacity.
func TestProvisionFailureFreesCapacity(t *testing.T) {
	b, h, store, fake := capBroker(t, budgetPolicy(0, 4, 512))
	fake.CreateErr = errors.New("boom") // fail the container-create step of provisioning

	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":600,"resources":{"cpus":2,"memory_mb":256}}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (provisioning failed) (body: %s)", rec.Code, rec.Body.String())
	}
	if b.cap.ActiveContracts() != 0 || b.cap.UsedCPUs() != 0 || b.cap.UsedMemoryMB() != 0 {
		t.Fatalf("usage after failed provision = {%d, %v, %d}, want all zero",
			b.cap.ActiveContracts(), b.cap.UsedCPUs(), b.cap.UsedMemoryMB())
	}
	// The failed contract is recorded but expired, not stranding a live reservation.
	for _, c := range store.List() {
		if !c.State.Terminal() {
			t.Errorf("contract %s state = %q, want a terminal state after provision failure", c.ID, c.State)
		}
	}
}

// When the TTL reaper expires a lease it returns the lease's reserved capacity to
// the allocator, gated on winning the expired transition.
func TestReaperExpiryFreesCapacity(t *testing.T) {
	b, h, store, fake := capBroker(t, budgetPolicy(0, 4, 512))

	created := createContract(t, h, `{"ttl_seconds":1,"resources":{"cpus":2,"memory_mb":256}}`)
	if b.cap.UsedCPUs() != 2 {
		t.Fatalf("UsedCPUs() after create = %v, want 2", b.cap.UsedCPUs())
	}

	c, _ := store.Get(created.ContractID)
	// A reaper wired exactly as production wires it (see cmd/wispd), with a clock
	// past the lease's expiry so a single Tick expires it.
	rp := reaper.New(store, fake, reaper.Options{
		Now:             func() time.Time { return c.ExpiresAt.Add(time.Second) },
		Logger:          discardLogger(),
		ReleaseGPUs:     b.alloc.Free,
		ReleaseCapacity: func(c contract.Contract) { b.cap.Free(c.ReservedCPUs, c.ReservedMemoryMB) },
	})
	rp.Tick(context.Background())
	rp.WaitForKills()

	if got, _ := store.Get(created.ContractID); got.State != contract.StateExpired {
		t.Fatalf("state after reaper = %q, want expired", got.State)
	}
	if b.cap.ActiveContracts() != 0 || b.cap.UsedCPUs() != 0 || b.cap.UsedMemoryMB() != 0 {
		t.Fatalf("usage after reaper expiry = {%d, %v, %d}, want all zero",
			b.cap.ActiveContracts(), b.cap.UsedCPUs(), b.cap.UsedMemoryMB())
	}
}

// TestReaperReapsDeadContainerFreesCapacityAndImagesReflects covers ACs 149, 150,
// and the "images capacity block reflects the free" acceptance criterion end to
// end: a contract's backing container dies out of band (docker kill / rm); on
// the reaper's next tick the contract is moved to a terminal state, its cpu /
// memory / contract-slot capacity is freed, GET /contracts/{id} reports the
// terminal state, and GET /images shows the reclaimed usage - all before the
// TTL has any chance of firing.
func TestReaperReapsDeadContainerFreesCapacityAndImagesReflects(t *testing.T) {
	b, h, store, fake := capBroker(t, budgetPolicy(0, 4, 512))

	created := createContract(t, h, `{"ttl_seconds":3600,"resources":{"cpus":2,"memory_mb":256}}`)
	if b.cap.UsedCPUs() != 2 || b.cap.UsedMemoryMB() != 256 || b.cap.ActiveContracts() != 1 {
		t.Fatalf("post-create usage = {%d, %v, %d}, want {1, 2, 256}",
			b.cap.ActiveContracts(), b.cap.UsedCPUs(), b.cap.UsedMemoryMB())
	}
	if img := getImages(t, h); img.Capacity.UsedCPUs != 2 || img.Capacity.UsedMemoryMB != 256 || img.Capacity.ActiveContracts != 1 {
		t.Fatalf("post-create /images capacity = {active %d, cpus %v, mem %d}, want {1, 2, 256}",
			img.Capacity.ActiveContracts, img.Capacity.UsedCPUs, img.Capacity.UsedMemoryMB)
	}

	// Simulate a docker rm / docker kill out of band: the container is gone from
	// the runtime but the contract is still ready. The TTL is an hour away - the
	// only signal that can reap it is the liveness check.
	c, _ := store.Get(created.ContractID)
	fake.InspectOverrides = map[string]runtime.LivenessState{c.ContainerID: runtime.LivenessGone}

	// Wire the reaper exactly as cmd/wispd does.
	rp := reaper.New(store, fake, reaper.Options{
		Now:             func() time.Time { return c.CreatedAt.Add(time.Minute) },
		Logger:          discardLogger(),
		ReleaseGPUs:     b.alloc.Free,
		ReleaseCapacity: func(c contract.Contract) { b.cap.Free(c.ReservedCPUs, c.ReservedMemoryMB) },
	})
	// Multiple ticks: the second and third observe the same dead container. The
	// state-machine gate on the winning expired transition must keep ReleaseCapacity
	// firing EXACTLY ONCE across all three, so used cpus/memory land at zero - not
	// underflow - and stay there.
	// Each tick launches the Kill off the sweep in its own goroutine; wait
	// between them so the sweep sees the winning terminal transition rather
	// than racing on the in-flight mark.
	rp.Tick(context.Background())
	rp.WaitForKills()
	rp.Tick(context.Background())
	rp.WaitForKills()
	rp.Tick(context.Background())
	rp.WaitForKills()

	// After three ticks the contract has been expired (first tick) and then
	// cleaned up (a terminal-at-start tick removes it), so a subsequent Get
	// reports it missing. What matters here is exactly-once capacity release -
	// asserted below via the aggregate usage - not that a terminal record lingers.
	if _, err := store.Get(created.ContractID); !errors.Is(err, contract.ErrNotFound) {
		t.Fatalf("store.Get after 3 ticks err = %v, want ErrNotFound (terminal contract cleaned up)", err)
	}
	if b.cap.ActiveContracts() != 0 || b.cap.UsedCPUs() != 0 || b.cap.UsedMemoryMB() != 0 {
		t.Fatalf("post-reap usage = {%d, %v, %d}, want all zero (exactly-once free)",
			b.cap.ActiveContracts(), b.cap.UsedCPUs(), b.cap.UsedMemoryMB())
	}

	// GET /contracts/{id} for a cleaned-up contract is a 404 - the reaper's
	// cleanup sweep removed it after it went terminal.
	rec := doAuth(t, h, http.MethodGet, "/contracts/"+created.ContractID, created.Token, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /contracts status = %d, want 404 after cleanup (body: %s)", rec.Code, rec.Body.String())
	}

	// GET /images capacity block reflects the freed usage.
	if img := getImages(t, h); img.Capacity.UsedCPUs != 0 || img.Capacity.UsedMemoryMB != 0 || img.Capacity.ActiveContracts != 0 {
		t.Errorf("post-reap /images capacity = {active %d, cpus %v, mem %d}, want all zero",
			img.Capacity.ActiveContracts, img.Capacity.UsedCPUs, img.Capacity.UsedMemoryMB)
	}
}

// A create rejected because the GPU allocator is exhausted frees the capacity it
// reserved moments earlier - neither a GPU device nor host capacity is stranded.
func TestRejectedGPUCreateStrandsNoCapacity(t *testing.T) {
	store := contract.NewStore()
	fake := gpuFake("GPU-0") // exactly one device
	pol := budgetPolicy(0, 0, 0)
	b := newBroker(store, fake, pol, bus.New(nil), discardLogger(), "")
	mux := http.NewServeMux()
	b.routes(mux)

	// Contract A takes the only device and reserves 2 cpus.
	createContract(t, mux, `{"ttl_seconds":600,"resources":{"gpus":1,"cpus":2}}`)
	if b.cap.UsedCPUs() != 2 || b.alloc.Available() != 0 {
		t.Fatalf("after A: usedCPUs=%v available=%d, want 2 and 0", b.cap.UsedCPUs(), b.alloc.Available())
	}

	// Contract B asks for a device that is gone: a 409 from the GPU allocator. Its
	// capacity reservation (2 cpus) must be freed, not stranded.
	rec := do(t, mux, http.MethodPost, "/contracts", `{"ttl_seconds":600,"resources":{"gpus":1,"cpus":2}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("B status = %d, want 409 (GPU exhausted) (body: %s)", rec.Code, rec.Body.String())
	}
	if got := b.cap.UsedCPUs(); got != 2 {
		t.Errorf("UsedCPUs() = %v, want 2 (only A's; B's reservation was freed)", got)
	}
	if got := b.cap.ActiveContracts(); got != 1 {
		t.Errorf("ActiveContracts() = %d, want 1 (only A)", got)
	}
	if n := len(store.List()); n != 1 {
		t.Errorf("store has %d contracts, want 1 (B recorded nothing)", n)
	}
}

// GET /images reports the live aggregate usage the capacity allocator tracks, so an
// agent scheduling against the host sees real headroom, not just the budgets.
func TestImagesReflectsLiveCapacityUsage(t *testing.T) {
	_, h, _, _ := capBroker(t, budgetPolicy(10, 16, 32768))

	got := getImages(t, h)
	if got.Capacity.UsedCPUs != 0 || got.Capacity.UsedMemoryMB != 0 || got.Capacity.ActiveContracts != 0 {
		t.Fatalf("empty-host usage = {cpus %v, mem %d, active %d}, want all zero",
			got.Capacity.UsedCPUs, got.Capacity.UsedMemoryMB, got.Capacity.ActiveContracts)
	}

	createContract(t, h, `{"ttl_seconds":600,"resources":{"cpus":2.5,"memory_mb":1024}}`)
	createContract(t, h, `{"ttl_seconds":600,"resources":{"cpus":1,"memory_mb":512}}`)

	got = getImages(t, h)
	if got.Capacity.UsedCPUs != 3.5 {
		t.Errorf("used_cpus = %v, want 3.5", got.Capacity.UsedCPUs)
	}
	if got.Capacity.UsedMemoryMB != 1536 {
		t.Errorf("used_memory_mb = %d, want 1536", got.Capacity.UsedMemoryMB)
	}
	if got.Capacity.ActiveContracts != 2 {
		t.Errorf("active_contracts = %d, want 2", got.Capacity.ActiveContracts)
	}
	// The budgets are still advertised alongside the usage.
	if got.Capacity.TotalCPUs != 16 || got.Capacity.TotalMemoryMB != 32768 || got.Capacity.MaxContracts != 10 {
		t.Errorf("budgets = %+v, want max_contracts=10 total_cpus=16 total_memory_mb=32768", got.Capacity)
	}
}

// Concurrent creates against a fixed contract budget never oversubscribe it: exactly
// the budget are admitted (201) and the rest are 409, with the allocator settling at
// exactly the budget - the create-path mirror of the GPU allocator's race test.
func TestConcurrentCreatesCannotOversubscribe(t *testing.T) {
	const budget = 8
	const goroutines = 64
	b, h, store, _ := capBroker(t, budgetPolicy(budget, 0, 0))

	var (
		mu       sync.Mutex
		created  int
		rejected int
		wg       sync.WaitGroup
		start    = make(chan struct{})
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":600}`)
			mu.Lock()
			defer mu.Unlock()
			switch rec.Code {
			case http.StatusCreated:
				created++
			case http.StatusConflict:
				rejected++
			default:
				t.Errorf("unexpected status %d (body: %s)", rec.Code, rec.Body.String())
			}
		}()
	}
	close(start)
	wg.Wait()

	if created != budget {
		t.Fatalf("created %d, want exactly %d (the contract budget)", created, budget)
	}
	if rejected != goroutines-budget {
		t.Fatalf("rejected %d, want %d", rejected, goroutines-budget)
	}
	if got := b.cap.ActiveContracts(); got != budget {
		t.Fatalf("ActiveContracts() = %d, want %d (no oversubscription)", got, budget)
	}
	live := 0
	for _, c := range store.List() {
		if !c.State.Terminal() {
			live++
		}
	}
	if live != budget {
		t.Fatalf("live contracts = %d, want %d", live, budget)
	}
}

// assertAtCapacity fails unless body is a JSON error carrying the stable "at
// capacity" token the 409 budget rejection must contain.
func assertAtCapacity(t *testing.T, body []byte) {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body: %v (body: %s)", err, body)
	}
	if !strings.Contains(e.Error, "at capacity") {
		t.Errorf("error = %q, want it to contain the stable %q token", e.Error, "at capacity")
	}
}
