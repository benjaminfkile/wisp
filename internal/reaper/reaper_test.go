package reaper

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// discardLogger keeps test output quiet.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// readyContract creates a contract, boots it through the fake runtime, and
// drives it to ready with a live container. It returns the ready contract copy
// (so callers can read ExpiresAt/ContainerID) and the backing container id.
func readyContract(t *testing.T, store *contract.Store, fake *runtime.Fake, ttl time.Duration) (contract.Contract, string) {
	t.Helper()
	ctx := context.Background()

	c, err := store.Create(contract.CreateParams{TTL: ttl})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.UpdateState(c.ID, contract.StateProvisioning); err != nil {
		t.Fatalf("UpdateState provisioning: %v", err)
	}
	cid, err := fake.Create(ctx, "wisp-base", runtime.CreateOptions{})
	if err != nil {
		t.Fatalf("fake.Create: %v", err)
	}
	if _, err := store.SetContainerID(c.ID, cid); err != nil {
		t.Fatalf("SetContainerID: %v", err)
	}
	if err := fake.Start(ctx, cid); err != nil {
		t.Fatalf("fake.Start: %v", err)
	}
	if _, err := store.UpdateState(c.ID, contract.StateReady); err != nil {
		t.Fatalf("UpdateState ready: %v", err)
	}

	c, err = store.Get(c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return c, cid
}

// When the reaper expires a lease holding GPU devices, it invokes ReleaseGPUs
// with that lease's exact device IDs so they return to the allocator. A lease
// with no GPUs never triggers the hook.
func TestReaperReleasesGPUsOnExpiry(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	ctx := context.Background()

	// A GPU-holding lease already past its TTL, adopted with a live container.
	past := time.Unix(1_000_000_000, 0)
	cid, err := fake.Create(ctx, "wisp-base", runtime.CreateOptions{})
	if err != nil {
		t.Fatalf("fake.Create: %v", err)
	}
	if _, err := store.Adopt(contract.AdoptParams{
		ID:           "gpu-contract",
		ContainerID:  cid,
		ExpiresAt:    past,
		GPUDeviceIDs: []string{"GPU-0", "GPU-1"},
	}); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	// A GPU-less lease also past its TTL, so both are expired in the same sweep.
	if _, err := store.Adopt(contract.AdoptParams{ID: "plain-contract", ExpiresAt: past}); err != nil {
		t.Fatalf("Adopt plain: %v", err)
	}

	var freed [][]string
	rp := New(store, fake, Options{
		Logger:      discardLogger(),
		Now:         func() time.Time { return past.Add(time.Hour) },
		ReleaseGPUs: func(ids []string) { freed = append(freed, ids) },
	})
	rp.Tick(ctx)

	if c, _ := store.Get("gpu-contract"); c.State != contract.StateExpired {
		t.Fatalf("gpu-contract state = %q, want expired", c.State)
	}
	if len(freed) != 1 {
		t.Fatalf("ReleaseGPUs called %d times, want 1 (only the GPU-holding lease)", len(freed))
	}
	if want := []string{"GPU-0", "GPU-1"}; !equalStrings(freed[0], want) {
		t.Fatalf("freed = %v, want %v", freed[0], want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestReaperExpiringWarning: a ready contract inside the lead window is moved to
// expiring, its container is left running, and the hook fires.
func TestReaperExpiringWarning(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	c, cid := readyContract(t, store, fake, time.Hour)

	var events []Event
	rp := New(store, fake, Options{
		Lead:   time.Minute,
		Logger: discardLogger(),
		// 30s before TTL: inside the one-minute lead window, before the hard kill.
		Now:    func() time.Time { return c.ExpiresAt.Add(-30 * time.Second) },
		Notify: func(e Event) { events = append(events, e) },
	})

	rp.Tick(context.Background())

	got, _ := store.Get(c.ID)
	if got.State != contract.StateExpiring {
		t.Fatalf("State = %q, want expiring", got.State)
	}
	if _, ok := fake.Container(cid); !ok {
		t.Error("container was killed during the expiring warning; want it left running")
	}
	if len(events) != 1 || events[0].To != contract.StateExpiring || events[0].From != contract.StateReady {
		t.Fatalf("events = %+v, want one ready→expiring event", events)
	}
	if events[0].ContractID != c.ID {
		t.Errorf("event ContractID = %q, want %q", events[0].ContractID, c.ID)
	}
}

// TestReaperExpiringThenExpired covers AC 477: the reaper transitions
// expiring→expired and kills the container, driven entirely by the injected
// clock.
func TestReaperExpiringThenExpired(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	c, cid := readyContract(t, store, fake, time.Hour)

	var events []Event
	now := c.ExpiresAt.Add(-30 * time.Second) // start inside the lead window
	rp := New(store, fake, Options{
		Lead:   time.Minute,
		Logger: discardLogger(),
		Now:    func() time.Time { return now },
		Notify: func(e Event) { events = append(events, e) },
	})

	// First tick: warning only.
	rp.Tick(context.Background())
	if got, _ := store.Get(c.ID); got.State != contract.StateExpiring {
		t.Fatalf("after warning tick State = %q, want expiring", got.State)
	}
	if _, ok := fake.Container(cid); !ok {
		t.Fatal("container killed during warning; want alive")
	}

	// Advance the clock past the TTL and tick again: hard kill.
	now = c.ExpiresAt.Add(time.Second)
	rp.Tick(context.Background())

	got, _ := store.Get(c.ID)
	if got.State != contract.StateExpired {
		t.Fatalf("after expiry tick State = %q, want expired", got.State)
	}
	if _, ok := fake.Container(cid); ok {
		t.Error("container survived expiry; want it killed")
	}
	if fake.Count() != 0 {
		t.Errorf("live container count = %d, want 0", fake.Count())
	}

	if len(events) != 2 {
		t.Fatalf("events = %+v, want two (expiring then expired)", events)
	}
	if events[0].To != contract.StateExpiring {
		t.Errorf("events[0].To = %q, want expiring", events[0].To)
	}
	if events[1].From != contract.StateExpiring || events[1].To != contract.StateExpired {
		t.Errorf("events[1] = %+v, want expiring→expired", events[1])
	}
}

// TestReaperExpiresReadyPastTTL: a ready contract already past its TTL (the lead
// window was missed) goes straight to expired and is killed.
func TestReaperExpiresReadyPastTTL(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	c, cid := readyContract(t, store, fake, time.Hour)

	rp := New(store, fake, Options{
		Lead:   time.Minute,
		Logger: discardLogger(),
		Now:    func() time.Time { return c.ExpiresAt.Add(time.Second) },
	})

	rp.Tick(context.Background())

	got, _ := store.Get(c.ID)
	if got.State != contract.StateExpired {
		t.Fatalf("State = %q, want expired", got.State)
	}
	if _, ok := fake.Container(cid); ok {
		t.Error("container survived; want killed")
	}
}

// TestReaperRunStartupReconcile covers AC 478's startup reconcile: Run's initial
// sweep reaps a contract already past its TTL before the ticker ever fires. The
// test blocks on the notify hook rather than sleeping, so it is deterministic.
func TestReaperRunStartupReconcile(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	c, cid := readyContract(t, store, fake, time.Hour)

	events := make(chan Event, 4)
	rp := New(store, fake, Options{
		Lead: time.Minute,
		// A long interval guarantees only the startup reconcile runs during the
		// test; the periodic ticker never fires.
		Interval: time.Hour,
		Logger:   discardLogger(),
		Now:      func() time.Time { return c.ExpiresAt.Add(time.Second) },
		Notify:   func(e Event) { events <- e },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rp.Run(ctx)

	select {
	case e := <-events:
		if e.To != contract.StateExpired || e.ContractID != c.ID {
			t.Fatalf("reconcile event = %+v, want expired for %q", e, c.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("startup reconcile did not fire")
	}

	got, _ := store.Get(c.ID)
	if got.State != contract.StateExpired {
		t.Errorf("State = %q, want expired", got.State)
	}
	if _, ok := fake.Container(cid); ok {
		t.Error("container survived startup reconcile; want killed")
	}
}

// TestReaperSkipsTerminalAndUntimely: contracts that are terminal or not yet due
// are left untouched, and repeated ticks are idempotent.
func TestReaperSkipsTerminalAndUntimely(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()

	// Released (terminal) contract past its TTL: must be left alone.
	released, releasedCID := readyContract(t, store, fake, time.Hour)
	if _, err := store.UpdateState(released.ID, contract.StateReleased); err != nil {
		t.Fatalf("UpdateState released: %v", err)
	}

	// Ready contract well before its lead window: must stay ready.
	fresh, freshCID := readyContract(t, store, fake, time.Hour)

	rp := New(store, fake, Options{
		Lead:   time.Minute,
		Logger: discardLogger(),
		// Far from either TTL: 30 minutes out, outside the one-minute lead.
		Now: func() time.Time { return fresh.ExpiresAt.Add(-30 * time.Minute) },
		Notify: func(e Event) {
			t.Errorf("unexpected transition: %+v", e)
		},
	})

	rp.Tick(context.Background())
	rp.Tick(context.Background()) // idempotent

	if got, _ := store.Get(released.ID); got.State != contract.StateReleased {
		t.Errorf("released contract State = %q, want released", got.State)
	}
	if got, _ := store.Get(fresh.ID); got.State != contract.StateReady {
		t.Errorf("fresh contract State = %q, want ready", got.State)
	}
	// The released contract's container was never touched by the reaper.
	_ = releasedCID
	if _, ok := fake.Container(freshCID); !ok {
		t.Error("fresh container killed; want alive")
	}
}

// TestReaperExpiresEvenIfKillFails: a runtime Kill error is logged but does not
// stop the reaper from marking the contract expired.
func TestReaperExpiresEvenIfKillFails(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	c, _ := readyContract(t, store, fake, time.Hour)
	fake.KillErr = errors.New("boom")

	rp := New(store, fake, Options{
		Lead:   time.Minute,
		Logger: discardLogger(),
		Now:    func() time.Time { return c.ExpiresAt.Add(time.Second) },
	})

	rp.Tick(context.Background())

	if got, _ := store.Get(c.ID); got.State != contract.StateExpired {
		t.Errorf("State = %q, want expired despite kill failure", got.State)
	}
}

// TestReaperSetNotify: the hook can be swapped after construction, as the event
// bus will do once it is wired up.
func TestReaperSetNotify(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	c, _ := readyContract(t, store, fake, time.Hour)

	var got []Event
	rp := New(store, fake, Options{
		Lead:   time.Minute,
		Logger: discardLogger(),
		Now:    func() time.Time { return c.ExpiresAt.Add(time.Second) },
	})
	rp.SetNotify(func(e Event) { got = append(got, e) })

	rp.Tick(context.Background())

	if len(got) != 1 || got[0].To != contract.StateExpired {
		t.Fatalf("events = %+v, want one expired event", got)
	}

	// Disabling the hook stops further notifications.
	rp.SetNotify(nil)
	rp.Tick(context.Background()) // no-op: contract already terminal
	if len(got) != 1 {
		t.Errorf("events = %+v, want no new events after SetNotify(nil)", got)
	}
}

// TestReaperDefaults: New fills in sane defaults for a zero Options.
func TestReaperDefaults(t *testing.T) {
	rp := New(contract.NewStore(), runtime.NewFake(), Options{})
	if rp.lead != defaultLead {
		t.Errorf("lead = %v, want %v", rp.lead, defaultLead)
	}
	if rp.interval != defaultInterval {
		t.Errorf("interval = %v, want %v", rp.interval, defaultInterval)
	}
	if rp.now == nil {
		t.Error("now is nil; want default time.Now")
	}
	if rp.logger == nil {
		t.Error("logger is nil; want default")
	}
}
