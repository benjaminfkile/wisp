package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/benjaminfkile/wisp/internal/capacity"
	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/reaper"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// leased builds a running LeasedContainer with the given contract id and expiry
// label. Tests that need a stopped container use leasedStopped instead.
func leased(containerID, contractID, expiresAt string) runtime.LeasedContainer {
	lc := leasedStopped(containerID, contractID, expiresAt)
	lc.Running = true
	return lc
}

// leasedStopped builds a non-running LeasedContainer with the given contract id
// and expiry label — the shape the runtime reports for a wisp container the
// prior process left behind that a host reboot (or a docker stop) has since
// stopped. Reconcile must not adopt these.
func leasedStopped(containerID, contractID, expiresAt string) runtime.LeasedContainer {
	labels := map[string]string{contractLabel: contractID}
	if expiresAt != "" {
		labels[expiresAtLabel] = expiresAt
	}
	return runtime.LeasedContainer{ID: containerID, Labels: labels}
}

// Reconcile must rebuild a tracking entry for a well-labeled pre-existing
// container: id from wisp.contract, expiry from wisp.expires_at, container id
// from the runtime, in a ready state so the reaper treats it like a live lease.
func TestReconcileRebuildsTrackingEntry(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	expiry := time.Unix(2_000_000_000, 0)
	fake.LeasedContainers = []runtime.LeasedContainer{
		leased("cont-1", "contract-1", strconv.FormatInt(expiry.Unix(), 10)),
	}

	Reconcile(context.Background(), store, fake, nil, nil, discardLogger())

	c, err := store.Get("contract-1")
	if err != nil {
		t.Fatalf("contract-1 not adopted: %v", err)
	}
	if c.State != contract.StateReady {
		t.Errorf("state = %q, want ready", c.State)
	}
	if c.ContainerID != "cont-1" {
		t.Errorf("container id = %q, want cont-1", c.ContainerID)
	}
	if !c.ExpiresAt.Equal(expiry) {
		t.Errorf("expires_at = %v, want %v", c.ExpiresAt, expiry)
	}
}

// A container whose contract or expiry label is missing or non-numeric cannot be
// tracked (the reaper needs a trustworthy id and expiry to enforce a TTL), but it
// carries wisp.contract so it is unambiguously ours. Leaving it running would let
// an unaccounted-for lease outlive its intended TTL forever, so Reconcile must
// kill and remove it rather than skipping it. A container with a valid label in
// the same batch must still be adopted.
func TestReconcileReapsMalformedLabelContainers(t *testing.T) {
	ctx := context.Background()
	store := contract.NewStore()
	fake := runtime.NewFake()

	// Create real fake containers so Reconcile's Kill actually removes them and
	// the test can observe their removal from the fake.
	badCID, _ := fake.Create(ctx, "img", runtime.CreateOptions{
		Labels: map[string]string{contractLabel: "contract-bad"},
		// no expires_at label
	})
	if err := fake.Start(ctx, badCID); err != nil {
		t.Fatalf("fake.Start bad: %v", err)
	}
	nanCID, _ := fake.Create(ctx, "img", runtime.CreateOptions{
		Labels: map[string]string{contractLabel: "contract-nan", expiresAtLabel: "not-a-number"},
	})
	if err := fake.Start(ctx, nanCID); err != nil {
		t.Fatalf("fake.Start nan: %v", err)
	}
	goodCID, _ := fake.Create(ctx, "img", runtime.CreateOptions{
		Labels: map[string]string{contractLabel: "contract-good", expiresAtLabel: "1900000000"},
	})
	if err := fake.Start(ctx, goodCID); err != nil {
		t.Fatalf("fake.Start good: %v", err)
	}

	Reconcile(ctx, store, fake, nil, nil, discardLogger())

	// Neither malformed-label contract is adopted.
	for _, killed := range []string{"contract-bad", "contract-nan"} {
		if _, err := store.Get(killed); !errors.Is(err, contract.ErrNotFound) {
			t.Errorf("contract %q should not be adopted, got err=%v", killed, err)
		}
	}
	// Both malformed-label containers are force-removed from the runtime — a wisp
	// container with no trustworthy id/expiry must not be left running.
	for _, killedCID := range []string{badCID, nanCID} {
		if _, ok := fake.Container(killedCID); ok {
			t.Errorf("container %q should have been killed and removed by reconcile", killedCID)
		}
	}
	// The valid contract is adopted and its container is still alive.
	if got := len(store.List()); got != 1 {
		t.Fatalf("tracked contracts = %d, want 1 (only the valid one)", got)
	}
	if _, err := store.Get("contract-good"); err != nil {
		t.Errorf("valid contract not adopted: %v", err)
	}
	if _, ok := fake.Container(goodCID); !ok {
		t.Errorf("valid container %q should still be alive after reconcile", goodCID)
	}
}

// A container carrying the contract label that is not running (Exited after a
// host reboot, or a docker stop) is a dead lease — its process holds no capacity
// and would falsely reserve full CPU/memory if adopted, so Reconcile must not
// adopt it. It must also force-remove it so a `docker ps -a` after startup no
// longer shows the stale artifact.
func TestReconcileDropsStoppedContainersAndReservesNoCapacity(t *testing.T) {
	ctx := context.Background()
	store := contract.NewStore()
	fake := runtime.NewFake()
	cap := capacity.NewAllocator(10, 100, 100_000)

	// Two stopped labelled containers — a host reboot left them Exited. Create
	// them in the fake but do NOT Start them, so ListLeased reports Running=false.
	expiry := strconv.FormatInt(time.Unix(1_900_000_000, 0).Unix(), 10)
	labels := func(id, cpus, mem string) map[string]string {
		return map[string]string{
			contractLabel: id, expiresAtLabel: expiry,
			cpusLabel: cpus, memoryMBLabel: mem,
		}
	}
	stopped1, _ := fake.Create(ctx, "img", runtime.CreateOptions{Labels: labels("k1", "2", "512")})
	stopped2, _ := fake.Create(ctx, "img", runtime.CreateOptions{Labels: labels("k2", "1.5", "256")})

	Reconcile(ctx, store, fake, nil, cap, discardLogger())

	if got := len(store.List()); got != 0 {
		t.Fatalf("adopted contracts = %d, want 0 (stopped containers must not be adopted)", got)
	}
	if got := cap.ActiveContracts(); got != 0 {
		t.Errorf("cap.ActiveContracts() = %d, want 0 (no slot reserved for stopped containers)", got)
	}
	if got := cap.UsedCPUs(); got != 0 {
		t.Errorf("cap.UsedCPUs() = %v, want 0", got)
	}
	if got := cap.UsedMemoryMB(); got != 0 {
		t.Errorf("cap.UsedMemoryMB() = %d, want 0", got)
	}
	// The stopped containers are force-removed — no artifact left in `docker ps -a`.
	for _, cid := range []string{stopped1, stopped2} {
		if _, ok := fake.Container(cid); ok {
			t.Errorf("stopped container %q should have been removed by reconcile", cid)
		}
	}
}

// The realistic post-reboot mix: some labelled containers are still running
// (adopt), some were left Exited (drop and remove). Only the running ones are
// tracked and only they consume capacity — the stopped ones must not.
func TestReconcileMixedRunningAndStopped(t *testing.T) {
	ctx := context.Background()
	store := contract.NewStore()
	fake := runtime.NewFake()
	cap := capacity.NewAllocator(10, 100, 100_000)

	expiry := strconv.FormatInt(time.Unix(1_900_000_000, 0).Unix(), 10)
	labels := func(id, cpus, mem string) map[string]string {
		return map[string]string{
			contractLabel: id, expiresAtLabel: expiry,
			cpusLabel: cpus, memoryMBLabel: mem,
		}
	}
	runningCID, _ := fake.Create(ctx, "img", runtime.CreateOptions{Labels: labels("keep", "3", "1024")})
	if err := fake.Start(ctx, runningCID); err != nil {
		t.Fatalf("fake.Start running: %v", err)
	}
	stoppedCID, _ := fake.Create(ctx, "img", runtime.CreateOptions{Labels: labels("drop", "5", "2048")})
	// stoppedCID is not started, so ListLeased reports Running=false for it.

	Reconcile(ctx, store, fake, nil, cap, discardLogger())

	// Only the running one is adopted; the stopped one is not tracked.
	if got := len(store.List()); got != 1 {
		t.Fatalf("adopted contracts = %d, want 1 (only the running one)", got)
	}
	kept, err := store.Get("keep")
	if err != nil {
		t.Fatalf("running contract not adopted: %v", err)
	}
	if kept.ContainerID != runningCID {
		t.Errorf("adopted container id = %q, want %q", kept.ContainerID, runningCID)
	}
	if kept.ReservedCPUs != 3 || kept.ReservedMemoryMB != 1024 {
		t.Errorf("re-reserved capacity = (%v cpus, %d mem), want (3, 1024)", kept.ReservedCPUs, kept.ReservedMemoryMB)
	}
	if _, err := store.Get("drop"); !errors.Is(err, contract.ErrNotFound) {
		t.Errorf("stopped contract should not be adopted, got err=%v", err)
	}
	// Only the running one consumes budget — stopped containers must not reserve
	// any of it.
	if got := cap.ActiveContracts(); got != 1 {
		t.Errorf("cap.ActiveContracts() = %d, want 1", got)
	}
	if got := cap.UsedCPUs(); got != 3 {
		t.Errorf("cap.UsedCPUs() = %v, want 3 (only the running one)", got)
	}
	if got := cap.UsedMemoryMB(); got != 1024 {
		t.Errorf("cap.UsedMemoryMB() = %d, want 1024 (only the running one)", got)
	}
	// The running container survives; the stopped one is removed.
	if _, ok := fake.Container(runningCID); !ok {
		t.Errorf("running container %q should still be alive after reconcile", runningCID)
	}
	if _, ok := fake.Container(stoppedCID); ok {
		t.Errorf("stopped container %q should have been removed by reconcile", stoppedCID)
	}
}

// A failure to list leased containers must not adopt anything or panic — the
// reconcile is best-effort and must never keep the daemon from starting.
func TestReconcileToleratesListError(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	fake.ListLeasedErr = errors.New("docker unreachable")

	Reconcile(context.Background(), store, fake, nil, nil, discardLogger())

	if got := len(store.List()); got != 0 {
		t.Fatalf("tracked contracts = %d, want 0 on list error", got)
	}
}

// End-to-end: a reconciled container already past its expiry is reaped (killed
// and marked expired) on the reaper's first sweep, while a still-valid one stays
// tracked. This is the whole point of the reconcile — no orphans survive a
// restart. It uses real fake containers (created with the contract labels) so
// the reaper's Kill and the fake's liveness bookkeeping are exercised for real,
// via the derive-from-tracked-containers path of ListLeased.
func TestReconcileThenReaperExpiresStaleAndKeepsValid(t *testing.T) {
	ctx := context.Background()
	store := contract.NewStore()
	fake := runtime.NewFake()
	now := time.Unix(1_000_000_000, 0)

	labels := func(id, expiresAt string) map[string]string {
		return map[string]string{contractLabel: id, expiresAtLabel: expiresAt}
	}
	staleCID, _ := fake.Create(ctx, "img", runtime.CreateOptions{
		Labels: labels("contract-stale", strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)),
	})
	validCID, _ := fake.Create(ctx, "img", runtime.CreateOptions{
		Labels: labels("contract-valid", strconv.FormatInt(now.Add(time.Hour).Unix(), 10)),
	})
	// The pre-existing containers a prior wispd would have left behind were
	// running when it exited; Start them here so the reaper's liveness check
	// (Inspect) sees them alive rather than misreading a created-but-not-started
	// fake as a died-out-of-band container and reaping the still-valid one.
	if err := fake.Start(ctx, staleCID); err != nil {
		t.Fatalf("fake.Start stale: %v", err)
	}
	if err := fake.Start(ctx, validCID); err != nil {
		t.Fatalf("fake.Start valid: %v", err)
	}

	Reconcile(ctx, store, fake, nil, nil, discardLogger())

	// The reconcile learns each contract's runtime container id from ListLeased.
	staleC, err := store.Get("contract-stale")
	if err != nil {
		t.Fatalf("contract-stale not adopted: %v", err)
	}
	if staleC.ContainerID != staleCID {
		t.Fatalf("adopted container id = %q, want %q", staleC.ContainerID, staleCID)
	}

	rp := reaper.New(store, fake, reaper.Options{
		Now:    func() time.Time { return now },
		Logger: discardLogger(),
	})
	rp.Tick(ctx)
	rp.WaitForKills()

	stale, err := store.Get("contract-stale")
	if err != nil {
		t.Fatalf("contract-stale missing: %v", err)
	}
	if stale.State != contract.StateExpired {
		t.Errorf("stale state = %q, want expired", stale.State)
	}
	if _, ok := fake.Container(staleCID); ok {
		t.Error("stale container should have been killed by the reaper")
	}

	valid, err := store.Get("contract-valid")
	if err != nil {
		t.Fatalf("contract-valid missing: %v", err)
	}
	if valid.State == contract.StateExpired || valid.State == contract.StateReleased {
		t.Errorf("valid state = %q, want still tracked (non-terminal)", valid.State)
	}
	if _, ok := fake.Container(validCID); !ok {
		t.Error("valid container should still be alive")
	}
}
