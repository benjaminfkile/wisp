package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/reaper"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// leased builds a LeasedContainer with the given contract id and expiry label.
func leased(containerID, contractID, expiresAt string) runtime.LeasedContainer {
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

	Reconcile(context.Background(), store, fake, nil, discardLogger())

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

// A container whose expiry label is missing or non-numeric cannot have its TTL
// enforced, so Reconcile must log and skip it (leave it untracked) rather than
// adopt a lease that would never expire or fail the whole reconcile. A container
// with a valid label in the same batch must still be adopted.
func TestReconcileSkipsMalformedLabels(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	fake.LeasedContainers = []runtime.LeasedContainer{
		leased("cont-missing", "contract-missing", ""),          // no expiry label
		leased("cont-bad", "contract-bad", "not-a-number"),      // malformed expiry
		{ID: "cont-noid", Labels: map[string]string{expiresAtLabel: "123"}}, // empty contract id
		leased("cont-good", "contract-good", "1900000000"),      // valid
	}

	Reconcile(context.Background(), store, fake, nil, discardLogger())

	for _, skipped := range []string{"contract-missing", "contract-bad"} {
		if _, err := store.Get(skipped); !errors.Is(err, contract.ErrNotFound) {
			t.Errorf("contract %q should have been skipped, got err=%v", skipped, err)
		}
	}
	if got := len(store.List()); got != 1 {
		t.Fatalf("tracked contracts = %d, want 1 (only the valid one)", got)
	}
	if _, err := store.Get("contract-good"); err != nil {
		t.Errorf("valid contract not adopted: %v", err)
	}
}

// A failure to list leased containers must not adopt anything or panic — the
// reconcile is best-effort and must never keep the daemon from starting.
func TestReconcileToleratesListError(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	fake.ListLeasedErr = errors.New("docker unreachable")

	Reconcile(context.Background(), store, fake, nil, discardLogger())

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

	Reconcile(ctx, store, fake, nil, discardLogger())

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
