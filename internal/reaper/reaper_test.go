package reaper

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/runtime"
	dockertypes "github.com/docker/docker/api/types"
	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/system"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
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

	var freedMu sync.Mutex
	var freed [][]string
	rp := New(store, fake, Options{
		Logger: discardLogger(),
		Now:    func() time.Time { return past.Add(time.Hour) },
		ReleaseGPUs: func(ids []string) {
			freedMu.Lock()
			defer freedMu.Unlock()
			freed = append(freed, ids)
		},
	})
	rp.Tick(ctx)
	rp.WaitForKills()

	if c, _ := store.Get("gpu-contract"); c.State != contract.StateExpired {
		t.Fatalf("gpu-contract state = %q, want expired", c.State)
	}
	freedMu.Lock()
	defer freedMu.Unlock()
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

	var eventsMu sync.Mutex
	var events []Event
	now := c.ExpiresAt.Add(-30 * time.Second) // start inside the lead window
	rp := New(store, fake, Options{
		Lead:   time.Minute,
		Logger: discardLogger(),
		Now:    func() time.Time { return now },
		Notify: func(e Event) {
			eventsMu.Lock()
			defer eventsMu.Unlock()
			events = append(events, e)
		},
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
	rp.WaitForKills()

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

	eventsMu.Lock()
	defer eventsMu.Unlock()
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
// window was missed) goes straight to expired and is killed. The fired Event
// carries Reason=ReasonTTLExpired so the bus can tell a natural TTL expiry
// apart from an out-of-band container death expiry (both drive the same
// terminal transition otherwise).
func TestReaperExpiresReadyPastTTL(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	c, cid := readyContract(t, store, fake, time.Hour)

	var eventsMu sync.Mutex
	var events []Event
	rp := New(store, fake, Options{
		Lead:   time.Minute,
		Logger: discardLogger(),
		Now:    func() time.Time { return c.ExpiresAt.Add(time.Second) },
		Notify: func(e Event) {
			eventsMu.Lock()
			defer eventsMu.Unlock()
			events = append(events, e)
		},
	})

	rp.Tick(context.Background())
	rp.WaitForKills()

	got, _ := store.Get(c.ID)
	if got.State != contract.StateExpired {
		t.Fatalf("State = %q, want expired", got.State)
	}
	if _, ok := fake.Container(cid); ok {
		t.Error("container survived; want killed")
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	if len(events) != 1 || events[0].Reason != ReasonTTLExpired {
		t.Fatalf("events = %+v, want one expired event with Reason=%q", events, ReasonTTLExpired)
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

	// Released (terminal) contract past its TTL: must be left alone. The
	// contract lifecycle routes releases through StateReleasing before
	// StateReleased, so the test walks the same fence.
	released, releasedCID := readyContract(t, store, fake, time.Hour)
	if _, err := store.UpdateState(released.ID, contract.StateReleasing); err != nil {
		t.Fatalf("UpdateState releasing: %v", err)
	}
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

	// The released contract is a terminal-at-start-of-tick lease that the reaper's
	// cleanup sweep removes so the store does not grow unboundedly. What matters
	// for "skips terminal" is that no unexpected transition fired (asserted via
	// the Notify hook above) and that the container was not touched — the removal
	// itself is expected.
	if _, err := store.Get(released.ID); !errors.Is(err, contract.ErrNotFound) {
		t.Errorf("released contract lookup err = %v, want ErrNotFound after cleanup sweep", err)
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
	rp.WaitForKills()

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

	var gotMu sync.Mutex
	var got []Event
	rp := New(store, fake, Options{
		Lead:   time.Minute,
		Logger: discardLogger(),
		Now:    func() time.Time { return c.ExpiresAt.Add(time.Second) },
	})
	rp.SetNotify(func(e Event) {
		gotMu.Lock()
		defer gotMu.Unlock()
		got = append(got, e)
	})

	rp.Tick(context.Background())
	rp.WaitForKills()

	gotMu.Lock()
	if len(got) != 1 || got[0].To != contract.StateExpired {
		gotMu.Unlock()
		t.Fatalf("events = %+v, want one expired event", got)
	}
	gotMu.Unlock()

	// Disabling the hook stops further notifications.
	rp.SetNotify(nil)
	rp.Tick(context.Background()) // no-op: contract already terminal
	rp.WaitForKills()
	gotMu.Lock()
	defer gotMu.Unlock()
	if len(got) != 1 {
		t.Errorf("events = %+v, want no new events after SetNotify(nil)", got)
	}
}

// TestReaperReapsDeadContainerBeforeTTL covers ACs 149/150: a container killed
// out of band well before its TTL (docker kill / rm / OOM) is detected on the
// reaper's next tick and drives its contract to a terminal state. The container
// slot is freed too, so a follow-up create is not blocked by a "ready" contract
// with no living backing.
func TestReaperReapsDeadContainerBeforeTTL(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	c, cid := readyContract(t, store, fake, time.Hour)

	// Simulate an out-of-band docker kill / rm: the container is gone but the
	// contract is still ready with a well-in-the-future TTL. Wisp only learns via
	// the liveness check on the next tick — well before the lead window.
	fake.InspectOverrides = map[string]runtime.LivenessState{cid: runtime.LivenessGone}

	var eventsMu sync.Mutex
	var events []Event
	rp := New(store, fake, Options{
		Lead:   time.Minute,
		Logger: discardLogger(),
		// Nowhere near the TTL: only the liveness signal can trigger the reap.
		Now: func() time.Time { return c.ExpiresAt.Add(-30 * time.Minute) },
		Notify: func(e Event) {
			eventsMu.Lock()
			defer eventsMu.Unlock()
			events = append(events, e)
		},
	})

	rp.Tick(context.Background())
	rp.WaitForKills()

	got, _ := store.Get(c.ID)
	if !got.State.Terminal() {
		t.Fatalf("state = %q, want terminal after out-of-band container death", got.State)
	}
	if got.State != contract.StateExpired {
		t.Errorf("state = %q, want %q", got.State, contract.StateExpired)
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	if len(events) != 1 || events[0].To != contract.StateExpired {
		t.Fatalf("events = %+v, want one ready→expired event", events)
	}
	// The container-death path tags its Reason so the bus can distinguish it
	// from a natural TTL expiry (see LifecycleNotify).
	if events[0].Reason != ReasonContainerDied {
		t.Errorf("events[0].Reason = %q, want %q", events[0].Reason, ReasonContainerDied)
	}
}

// TestReaperReapsStoppedContainerBeforeTTL is the LivenessStopped sibling of the
// gone case above: a container that exited (OOM, crash, docker stop) but still
// exists is likewise reaped through the same terminal path.
func TestReaperReapsStoppedContainerBeforeTTL(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	c, cid := readyContract(t, store, fake, time.Hour)

	fake.InspectOverrides = map[string]runtime.LivenessState{cid: runtime.LivenessStopped}

	rp := New(store, fake, Options{
		Lead:   time.Minute,
		Logger: discardLogger(),
		Now:    func() time.Time { return c.ExpiresAt.Add(-30 * time.Minute) },
	})
	rp.Tick(context.Background())
	rp.WaitForKills()

	if got, _ := store.Get(c.ID); got.State != contract.StateExpired {
		t.Errorf("state = %q, want %q after out-of-band container stop", got.State, contract.StateExpired)
	}
}

// TestReaperDeadContainerFreesCapacityExactlyOnce covers AC 151: ReleaseCapacity
// fires EXACTLY ONCE for a contract whose container died out of band, even when
// the reaper observes the dead container on multiple ticks. The
// state-machine gate on the winning expired transition is what enforces this —
// second and subsequent ticks see a terminal contract and skip early.
func TestReaperDeadContainerFreesCapacityExactlyOnce(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	c, cid := readyContract(t, store, fake, time.Hour)

	fake.InspectOverrides = map[string]runtime.LivenessState{cid: runtime.LivenessGone}

	var freed atomic.Int64
	rp := New(store, fake, Options{
		Lead:            time.Minute,
		Logger:          discardLogger(),
		Now:             func() time.Time { return c.ExpiresAt.Add(-30 * time.Minute) },
		ReleaseCapacity: func(contract.Contract) { freed.Add(1) },
	})

	for i := 0; i < 5; i++ {
		rp.Tick(context.Background())
		// Each tick may launch an off-tick Kill goroutine; wait for it so a
		// subsequent tick sees the winning terminal transition (and skips) rather
		// than racing on the in-flight mark, which would let the assertion below
		// depend on the goroutine's timing.
		rp.WaitForKills()
	}

	if got := freed.Load(); got != 1 {
		t.Fatalf("ReleaseCapacity fired %d times across 5 ticks; want exactly 1", got)
	}
	// After the first tick the contract is expired; a subsequent tick's cleanup
	// sweep removes it (it was terminal at start of tick), so five ticks in it is
	// gone from the store. What matters here is exactly-once capacity release
	// across those ticks, which is asserted above.
	if _, err := store.Get(c.ID); !errors.Is(err, contract.ErrNotFound) {
		t.Errorf("Get after 5 ticks err = %v, want ErrNotFound (terminal contract cleaned up)", err)
	}
}

// TestReaperDeadContainerFreesGPUs covers AC 152: a contract holding GPU devices
// whose container dies out of band still has its GPUs freed through the same
// path a TTL expiry would. Uses Adopt to attach GPU device IDs, then simulates
// the container going away via InspectOverrides.
func TestReaperDeadContainerFreesGPUs(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	ctx := context.Background()

	cid, err := fake.Create(ctx, "wisp-base", runtime.CreateOptions{})
	if err != nil {
		t.Fatalf("fake.Create: %v", err)
	}
	if err := fake.Start(ctx, cid); err != nil {
		t.Fatalf("fake.Start: %v", err)
	}
	future := time.Unix(2_000_000_000, 0)
	if _, err := store.Adopt(contract.AdoptParams{
		ID:           "gpu-contract",
		ContainerID:  cid,
		ExpiresAt:    future,
		GPUDeviceIDs: []string{"GPU-0", "GPU-1"},
	}); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	// Simulate the container being killed by docker kill / OOM.
	fake.InspectOverrides = map[string]runtime.LivenessState{cid: runtime.LivenessGone}

	var freedMu sync.Mutex
	var freed [][]string
	rp := New(store, fake, Options{
		Logger: discardLogger(),
		Now:    func() time.Time { return future.Add(-time.Hour) },
		ReleaseGPUs: func(ids []string) {
			freedMu.Lock()
			defer freedMu.Unlock()
			freed = append(freed, ids)
		},
	})
	rp.Tick(ctx)
	rp.WaitForKills()

	freedMu.Lock()
	defer freedMu.Unlock()
	if len(freed) != 1 {
		t.Fatalf("ReleaseGPUs fired %d times, want 1", len(freed))
	}
	if want := []string{"GPU-0", "GPU-1"}; !equalStrings(freed[0], want) {
		t.Errorf("freed = %v, want %v", freed[0], want)
	}
	if got, _ := store.Get("gpu-contract"); got.State != contract.StateExpired {
		t.Errorf("state = %q, want %q", got.State, contract.StateExpired)
	}
}

// TestReaperInspectTransportErrorDoesNotReap: a transient error from Inspect
// (e.g. daemon unreachable) is inconclusive — the reaper leaves the contract
// alone rather than reaping a live lease on a blip.
func TestReaperInspectTransportErrorDoesNotReap(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	c, _ := readyContract(t, store, fake, time.Hour)

	fake.InspectErr = errors.New("daemon unreachable")

	rp := New(store, fake, Options{
		Lead:   time.Minute,
		Logger: discardLogger(),
		Now:    func() time.Time { return c.ExpiresAt.Add(-30 * time.Minute) },
		Notify: func(Event) {
			t.Error("no transition should fire on a transient Inspect error")
		},
	})
	rp.Tick(context.Background())

	if got, _ := store.Get(c.ID); got.State != contract.StateReady {
		t.Errorf("state = %q, want still ready after transient Inspect error", got.State)
	}
}

// TestReaperInspectSkipsProvisioning: a container mid-provision (created but not
// yet Started) reports LivenessStopped from the runtime, but the reaper must
// leave a provisioning contract alone — it is expected to not be running yet.
// Only ready and expiring contracts are candidates for the liveness reap.
func TestReaperInspectSkipsProvisioning(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	ctx := context.Background()

	c, err := store.Create(contract.CreateParams{TTL: time.Hour})
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
	// Deliberately do NOT Start the container — mirror the mid-provision moment
	// between Create and Start when Inspect would report LivenessStopped.

	rp := New(store, fake, Options{
		Lead:   time.Minute,
		Logger: discardLogger(),
		Now:    func() time.Time { return c.CreatedAt.Add(time.Minute) },
	})
	rp.Tick(ctx)

	got, _ := store.Get(c.ID)
	if got.State != contract.StateProvisioning {
		t.Errorf("state = %q, want still provisioning", got.State)
	}
}

// dockerNotFoundStubClient embeds the full Docker APIClient (left nil) and
// overrides Info (for construction) plus ContainerInspect and ContainerRemove
// so the reaper can drive a DockerRuntime whose backing daemon reports both the
// inspect and the remove as docker-style not-found — the exact shape produced
// by a live daemon after a container is removed out of band (docker rm, an
// auto-remove after exit, etc.). Anything else would panic, which is fine: the
// reaper's expire path only calls Inspect and Kill on the runtime.
type dockerNotFoundStubClient struct {
	dockerclient.APIClient
}

func (dockerNotFoundStubClient) Info(context.Context) (system.Info, error) {
	return system.Info{OSType: "linux"}, nil
}

func (dockerNotFoundStubClient) ContainerInspect(context.Context, string) (dockertypes.ContainerJSON, error) {
	return dockertypes.ContainerJSON{}, errdefs.NotFound(errors.New("No such container"))
}

func (dockerNotFoundStubClient) ContainerRemove(context.Context, string, dockercontainer.RemoveOptions) error {
	return errdefs.NotFound(errors.New("No such container"))
}

// dockerConflictStubClient is the "removal already in progress" shape a live
// daemon returns when two Kill calls race on the same container (typically the
// release handler and the reaper's container-died sweep). Reaper's Kill must
// tolerate the conflict as success (the container IS being torn down, which is
// what Kill wanted) so it does not log a spurious ERROR every time the release
// path wins the race.
type dockerConflictStubClient struct {
	dockerclient.APIClient
}

func (dockerConflictStubClient) Info(context.Context) (system.Info, error) {
	return system.Info{OSType: "linux"}, nil
}

func (dockerConflictStubClient) ContainerInspect(context.Context, string) (dockertypes.ContainerJSON, error) {
	// Report the container as stopped so the reaper's containerDied check picks
	// it up and drives the expire path (which calls Kill).
	return dockertypes.ContainerJSON{ContainerJSONBase: &dockertypes.ContainerJSONBase{State: &dockertypes.ContainerState{Running: false}}}, nil
}

func (dockerConflictStubClient) ContainerRemove(context.Context, string, dockercontainer.RemoveOptions) error {
	return errdefs.Conflict(errors.New("removal of container abc is already in progress"))
}

// TestReaperNoErrorLogOnDockerConflictKill: when the release handler and the
// reaper's container-died sweep race on the same container, the daemon replies
// to the second Kill with a 409 conflict ("removal already in progress"). Kill
// treats that as success so the reaper never logs a spurious ERROR on the
// race. Uses a real DockerRuntime backed by a stub client so the mapping is
// exercised end to end (the fake runtime never returns conflicts).
func TestReaperNoErrorLogOnDockerConflictKill(t *testing.T) {
	store := contract.NewStore()
	rt := runtime.NewDockerRuntimeWithClient(dockerConflictStubClient{})

	future := time.Unix(2_000_000_000, 0)
	adopted, err := store.Adopt(contract.AdoptParams{
		ID:          "conflicting-contract",
		ContainerID: "abc",
		ExpiresAt:   future,
	})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rp := New(store, rt, Options{
		Lead:   time.Minute,
		Logger: logger,
		// Well before the TTL: only the container-died signal (via Inspect) can
		// trigger the reap.
		Now: func() time.Time { return future.Add(-time.Hour) },
	})

	rp.Tick(context.Background())
	rp.WaitForKills()

	if got, _ := store.Get(adopted.ID); got.State != contract.StateExpired {
		t.Fatalf("state = %q, want expired after container-died reap", got.State)
	}
	out := logs.String()
	if strings.Contains(out, "level=ERROR") {
		t.Errorf("reaper logged an ERROR on a racing Kill; the docker conflict must map to success. logs:\n%s", out)
	}
	if strings.Contains(out, "reaper: kill container") {
		t.Errorf("reaper logged a kill-container error message; want silent tolerance on a conflict race. logs:\n%s", out)
	}
}

// TestReaperSkipsReleasingContract pins the release-fence contract from the
// reaper side: a contract in StateReleasing INSIDE the release grace must never
// be reaped (no Kill, no transition) because the DELETE handler owns that fence
// and is in the middle of tearing the container down. Without this skip the
// reaper would double-kill the container and then purge the contract from under
// the DELETE handler, turning it into a 500 (the 2026-08-29 wisp-log failure).
func TestReaperSkipsReleasingContract(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	ctx := context.Background()

	// A live container behind a contract just moved into StateReleasing whose
	// TTL has already elapsed. Both reap-triggering signals (TTL past +
	// container stopped) are simulated below so the test can catch either path
	// if the in-grace skip fails.
	c, cid := readyContract(t, store, fake, time.Hour)
	if _, err := store.UpdateState(c.ID, contract.StateReleasing); err != nil {
		t.Fatalf("UpdateState releasing: %v", err)
	}
	// Reload the contract to read the ReleasingSince the store stamped on the
	// transition, so the reaper's clock is anchored to a real timestamp and the
	// grace check is exercised rather than accidentally satisfied.
	c, err := store.Get(c.ID)
	if err != nil {
		t.Fatalf("Get after UpdateState: %v", err)
	}
	if c.ReleasingSince.IsZero() {
		t.Fatal("UpdateState(StateReleasing) did not stamp ReleasingSince")
	}
	// Also force the container into a stopped shape via the fake, so the reap
	// path would fire on either signal (TTL past OR container died) if the
	// in-grace skip were removed.
	fake.InspectOverrides = map[string]runtime.LivenessState{cid: runtime.LivenessStopped}

	rp := New(store, fake, Options{
		Lead:         time.Minute,
		ReleaseGrace: 30 * time.Second,
		Logger:       discardLogger(),
		// Well past the TTL but well inside the release grace so the reaper is
		// facing both reap-triggering signals AND the in-grace fence at once.
		Now: func() time.Time { return c.ReleasingSince.Add(time.Second) },
		Notify: func(e Event) {
			t.Errorf("reaper transitioned a releasing contract: %+v", e)
		},
		ReleaseCapacity: func(contract.Contract) {
			t.Errorf("reaper freed capacity for a releasing contract")
		},
	})
	rp.Tick(ctx)

	got, err := store.Get(c.ID)
	if err != nil {
		t.Fatalf("Get after tick: %v", err)
	}
	if got.State != contract.StateReleasing {
		t.Errorf("state = %q, want releasing (fence must hold across a tick inside the grace)", got.State)
	}
	if _, ok := fake.Container(cid); !ok {
		t.Error("reaper killed the container of a releasing contract inside the grace; the fence must forbid it")
	}
}

// TestReaperExpiresStuckReleasingContract pins the release-grace escape hatch:
// a contract that has sat in StateReleasing past the reaper's release grace
// (the DELETE handler crashed mid-release, or its request was cancelled
// before the final mark-released transition) is treated like any other
// non-terminal contract: the reaper kills the container, expires the
// contract, and returns its reserved capacity to the allocator. A hung Docker
// daemon on the reaper's own Kill is bounded separately by killTimeout and
// runs off the tick, not by this grace. Without this path the lease's
// capacity would leak until wispd restarted.
func TestReaperExpiresStuckReleasingContract(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	ctx := context.Background()

	c, cid := readyContract(t, store, fake, time.Hour)
	if _, err := store.UpdateState(c.ID, contract.StateReleasing); err != nil {
		t.Fatalf("UpdateState releasing: %v", err)
	}
	c, err := store.Get(c.ID)
	if err != nil {
		t.Fatalf("Get after UpdateState: %v", err)
	}
	if c.ReleasingSince.IsZero() {
		t.Fatal("UpdateState(StateReleasing) did not stamp ReleasingSince")
	}

	var eventsMu sync.Mutex
	var events []Event
	var freed atomic.Int64
	rp := New(store, fake, Options{
		Lead:         time.Minute,
		ReleaseGrace: 30 * time.Second,
		Logger:       discardLogger(),
		// Just past the grace: the release is presumed stuck, so the reaper
		// takes over even though the contract is still well within its TTL.
		Now: func() time.Time { return c.ReleasingSince.Add(31 * time.Second) },
		Notify: func(e Event) {
			eventsMu.Lock()
			defer eventsMu.Unlock()
			events = append(events, e)
		},
		ReleaseCapacity: func(contract.Contract) { freed.Add(1) },
	})
	rp.Tick(ctx)
	rp.WaitForKills()

	got, err := store.Get(c.ID)
	if err != nil {
		t.Fatalf("Get after tick: %v", err)
	}
	if got.State != contract.StateExpired {
		t.Fatalf("state = %q, want expired (release stuck past grace must be reaped)", got.State)
	}
	if _, ok := fake.Container(cid); ok {
		t.Error("reaper did not kill the container of a stuck releasing contract past grace")
	}
	if got := freed.Load(); got != 1 {
		t.Errorf("ReleaseCapacity fired %d times, want 1 (capacity must return to the allocator)", got)
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	if len(events) != 1 || events[0].To != contract.StateExpired || events[0].From != contract.StateReleasing {
		t.Fatalf("events = %+v, want one releasing→expired transition", events)
	}
}

// TestReaperDefaultReleaseGraceSkipsFreshReleasing pins that the default
// release-grace window (see defaultReleaseGrace) is long enough that a
// just-transitioned StateReleasing contract is skipped even without an
// explicit ReleaseGrace on the Options.
func TestReaperDefaultReleaseGraceSkipsFreshReleasing(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	ctx := context.Background()

	c, cid := readyContract(t, store, fake, time.Hour)
	if _, err := store.UpdateState(c.ID, contract.StateReleasing); err != nil {
		t.Fatalf("UpdateState releasing: %v", err)
	}
	c, err := store.Get(c.ID)
	if err != nil {
		t.Fatalf("Get after UpdateState: %v", err)
	}

	rp := New(store, fake, Options{
		Lead:   time.Minute,
		Logger: discardLogger(),
		// One second after the fence went up: well inside the default 30 s grace.
		Now: func() time.Time { return c.ReleasingSince.Add(time.Second) },
		Notify: func(e Event) {
			t.Errorf("reaper transitioned a fresh releasing contract under the default grace: %+v", e)
		},
	})
	rp.Tick(ctx)

	if got, _ := store.Get(c.ID); got.State != contract.StateReleasing {
		t.Errorf("state = %q, want releasing (default grace must skip a fresh releasing contract)", got.State)
	}
	if _, ok := fake.Container(cid); !ok {
		t.Error("reaper killed the container of a fresh releasing contract under the default grace")
	}
}

// TestReaperNoErrorLogOnDockerNotFoundKill covers AC 215: a container the
// daemon reports as gone (docker rm out of band, auto-remove after exit) is
// reaped through the LivenessGone path without an ERROR log. The regression it
// pins is a Kill implementation that wrapped the docker not-found error and
// never returned runtime.ErrNotFound, so reaper.expire logged
// "reaper: kill container" on every out-of-band removal — the exact scenario
// the container-death detection was built for. The test uses a real
// DockerRuntime backed by a stub client returning docker-style not-found (NOT
// the fake runtime, whose Kill already returns the pre-mapped ErrNotFound and
// therefore masks the bug).
func TestReaperNoErrorLogOnDockerNotFoundKill(t *testing.T) {
	store := contract.NewStore()
	rt := runtime.NewDockerRuntimeWithClient(dockerNotFoundStubClient{})

	// A ready contract with a container id the daemon does not know about — the
	// LivenessGone shape. Adopt sets the fields directly so we do not need to
	// drive a Fake through create/start.
	future := time.Unix(2_000_000_000, 0)
	adopted, err := store.Adopt(contract.AdoptParams{
		ID:          "gone-contract",
		ContainerID: "vanished-cid",
		ExpiresAt:   future,
	})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rp := New(store, rt, Options{
		Lead:   time.Minute,
		Logger: logger,
		// Well before the TTL: only the LivenessGone signal (via Inspect) can
		// trigger the reap.
		Now: func() time.Time { return future.Add(-time.Hour) },
	})

	rp.Tick(context.Background())
	rp.WaitForKills()

	if got, _ := store.Get(adopted.ID); got.State != contract.StateExpired {
		t.Fatalf("state = %q, want expired after out-of-band container removal", got.State)
	}
	out := logs.String()
	if strings.Contains(out, "level=ERROR") {
		t.Errorf("reaper logged an ERROR on the LivenessGone reap path; the docker not-found from Kill must map to runtime.ErrNotFound and be suppressed. logs:\n%s", out)
	}
	if strings.Contains(out, "reaper: kill container") {
		t.Errorf("reaper logged a kill-container error message; want silent suppression on LivenessGone. logs:\n%s", out)
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
	if rp.releaseGrace != defaultReleaseGrace {
		t.Errorf("releaseGrace = %v, want %v", rp.releaseGrace, defaultReleaseGrace)
	}
	if rp.killTimeout != defaultKillTimeout {
		t.Errorf("killTimeout = %v, want %v", rp.killTimeout, defaultKillTimeout)
	}
	if rp.now == nil {
		t.Error("now is nil; want default time.Now")
	}
	if rp.logger == nil {
		t.Error("logger is nil; want default")
	}
}

// hangingKillRuntime wraps a Fake so a targeted container id hangs Kill until
// the caller's context is cancelled, standing in for a Docker daemon whose
// ContainerRemove has wedged. Any other id delegates to the fake's normal Kill.
// Embedding the concrete *runtime.Fake means Kill defined here shadows the
// embedded Kill and the wrapper still satisfies runtime.Runtime. started is
// closed the first time Kill enters the hang so tests can synchronize on the
// hung goroutine actually being in flight, and calls counts every entry into
// Kill for the hangID so a test can assert the in-flight mark prevents the
// next tick from launching a second Kill.
type hangingKillRuntime struct {
	*runtime.Fake
	hangID    string
	started   chan struct{}
	startedMu sync.Mutex
	calls     atomic.Int64
}

func (h *hangingKillRuntime) Kill(ctx context.Context, id string) error {
	if id == h.hangID {
		h.calls.Add(1)
		h.startedMu.Lock()
		select {
		case <-h.started:
			// already closed by an earlier call
		default:
			close(h.started)
		}
		h.startedMu.Unlock()
		<-ctx.Done()
		return ctx.Err()
	}
	return h.Fake.Kill(ctx, id)
}

// TestReaperKillDoesNotStallTick pins the async Kill behavior: with the
// reaper's Kill running OFF the tick in its own goroutine, a hung Docker
// daemon on one contract's Kill must add no latency to the sweep, so Tick
// returns well under the KillTimeout while the remaining contracts are still
// reaped. Without the off-tick change the sweep would block on the wedged
// Kill for the whole KillTimeout and a co-tick expiry would sit unenforced
// until the daemon responded (measured: a TTL-expired contract reaped 17 s
// late).
func TestReaperKillDoesNotStallTick(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()

	// Two ready contracts, both past their TTL. The store lists in CreatedAt
	// order, so the first-created (hung) contract is processed first; without
	// the off-tick change the sweep would block there and never reach live.
	hung, hungCID := readyContract(t, store, fake, time.Hour)
	live, liveCID := readyContract(t, store, fake, time.Hour)

	rt := &hangingKillRuntime{Fake: fake, hangID: hungCID, started: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		// Let the hung goroutine unwind before the test returns so the -race
		// detector does not flag its lingering write to killWG under teardown.
	}()

	rp := New(store, rt, Options{
		Lead: time.Minute,
		// A generous KillTimeout so any inline-Kill regression is obvious: even
		// a "slow" inline Kill would stall the tick for the full hour, not the
		// sub-second bound below.
		KillTimeout: time.Hour,
		Logger:      discardLogger(),
		Now:         func() time.Time { return hung.ExpiresAt.Add(time.Second) },
	})

	start := time.Now()
	rp.Tick(ctx)
	elapsed := time.Since(start)
	// Off-the-tick Kill: Tick returns as soon as it has fanned out the
	// goroutines, regardless of a hung one. The bound is orders of magnitude
	// below KillTimeout so a stall regression is unambiguous.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Tick took %v with a hung Kill in flight; the reaper Kill must run off the tick (KillTimeout = %v)", elapsed, time.Hour)
	}

	// The hung Kill goroutine must actually be in flight while the live
	// contract is being reaped, otherwise the "off the tick" claim is
	// meaningless. Wait for it before making the live-side assertions.
	select {
	case <-rt.started:
	case <-time.After(2 * time.Second):
		t.Fatal("hung Kill goroutine never entered Kill; the reaper did not launch it")
	}

	// The live contract's Kill goroutine completes quickly against the fake
	// runtime; poll for its terminal transition rather than sleeping.
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := store.Get(live.ID)
		if err == nil && got.State == contract.StateExpired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("live contract not expired within 2s; got state = %q err = %v", got.State, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := fake.Container(liveCID); ok {
		t.Error("live contract's container survived; it must be reaped while another Kill hangs")
	}

	// The hung contract must be left in place: its Kill is still in flight
	// (blocked on ctx.Done), so no terminal transition has fired and the
	// in-flight mark still holds.
	got, err := store.Get(hung.ID)
	if err != nil {
		t.Fatalf("Get hung contract: %v", err)
	}
	if got.State == contract.StateExpired {
		t.Errorf("hung contract state = %q; the terminal transition must not fire while Kill is still in flight", got.State)
	}
	if !rp.isKilling(hung.ID) {
		t.Error("hung contract is not marked in-flight; the mark must hold across the sweep")
	}
	if _, ok := fake.Container(hungCID); !ok {
		t.Error("hung container was removed despite the Kill still hanging")
	}

	// Cancel the outer context so the hung goroutine's killCtx unblocks and
	// the goroutine cleans up before the test returns.
	cancel()
	rp.WaitForKills()
}

// TestReaperKillTimeoutLeavesContractInPlace pins the fix for a bounded Kill:
// when the Kill goroutine's killCtx timeout fires with the outer context
// still alive, the contract is left in place for a later kill attempt (no
// terminal transition, no capacity free). The reaper's tick sweep is
// unaffected; this test focuses on the goroutine's completion path.
func TestReaperKillTimeoutLeavesContractInPlace(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	c, cid := readyContract(t, store, fake, time.Hour)

	rt := &hangingKillRuntime{Fake: fake, hangID: cid, started: make(chan struct{})}

	var freed atomic.Int64
	rp := New(store, rt, Options{
		Lead: time.Minute,
		// Short timeout so the goroutine exits via the killCtx deadline path
		// quickly under the test's outer context (still alive).
		KillTimeout:     50 * time.Millisecond,
		Logger:          discardLogger(),
		Now:             func() time.Time { return c.ExpiresAt.Add(time.Second) },
		ReleaseCapacity: func(contract.Contract) { freed.Add(1) },
	})

	rp.Tick(context.Background())
	rp.WaitForKills()

	// The goroutine exited via killCtx.DeadlineExceeded with the outer ctx
	// still alive: no terminal transition, no capacity free.
	got, err := store.Get(c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State == contract.StateExpired {
		t.Errorf("state = %q; a Kill timeout under a live outer ctx must leave the contract for a later attempt", got.State)
	}
	if n := freed.Load(); n != 0 {
		t.Errorf("ReleaseCapacity fired %d times; want 0 on a Kill timeout (nothing was actually freed)", n)
	}
	// The in-flight mark must be cleared so a later tick can retry.
	if rp.isKilling(c.ID) {
		t.Error("in-flight mark still held after Kill timeout; the mark must clear so a later tick can retry")
	}
}

// TestReaperInFlightContractNotDoubleKilled pins that the in-flight mark
// prevents a subsequent tick from launching a second Kill goroutine on a
// contract whose previous Kill is still outstanding. Without this the reaper
// would fan out a new Kill every tick while the daemon was wedged, all
// competing on the same container.
func TestReaperInFlightContractNotDoubleKilled(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	c, cid := readyContract(t, store, fake, time.Hour)

	rt := &hangingKillRuntime{Fake: fake, hangID: cid, started: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
	}()

	rp := New(store, rt, Options{
		Lead:        time.Minute,
		KillTimeout: time.Hour,
		Logger:      discardLogger(),
		Now:         func() time.Time { return c.ExpiresAt.Add(time.Second) },
	})

	// First tick: launches the hung Kill goroutine.
	rp.Tick(ctx)
	select {
	case <-rt.started:
	case <-time.After(2 * time.Second):
		t.Fatal("hung Kill goroutine did not start on the first tick")
	}

	// A second tick with the previous Kill still in flight must skip the
	// contract entirely (no new goroutine, no new Kill call).
	rp.Tick(ctx)
	// Give a stray second goroutine a moment to enter Kill if the in-flight
	// mark did not stop it, so the assertion catches the regression.
	time.Sleep(50 * time.Millisecond)

	if n := rt.calls.Load(); n != 1 {
		t.Fatalf("Kill called %d times; want 1 (an in-flight contract must not be double-killed by the next tick)", n)
	}

	cancel()
	rp.WaitForKills()
}

// TestReaperAsyncExpiryFreesCapacityExactlyOnce: the completion of an off-tick
// Kill goroutine that transitions a contract to expired fires
// ReleaseCapacity exactly once per contract, even across multiple ticks that
// would each try to expire it if not for the in-flight mark and the terminal
// state-machine gate.
func TestReaperAsyncExpiryFreesCapacityExactlyOnce(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	c, _ := readyContract(t, store, fake, time.Hour)

	var freed atomic.Int64
	rp := New(store, fake, Options{
		Lead:            time.Minute,
		Logger:          discardLogger(),
		Now:             func() time.Time { return c.ExpiresAt.Add(time.Second) },
		ReleaseCapacity: func(contract.Contract) { freed.Add(1) },
	})

	// Two ticks in quick succession before the first goroutine's completion
	// has a chance to fire on a slow scheduler; the in-flight mark must gate
	// the second tick's would-be expire.
	rp.Tick(context.Background())
	rp.Tick(context.Background())
	rp.WaitForKills()
	// A third tick after completion sees the contract as terminal and reap()
	// short-circuits.
	rp.Tick(context.Background())
	rp.WaitForKills()

	if got := freed.Load(); got != 1 {
		t.Fatalf("ReleaseCapacity fired %d times across three ticks; want exactly 1", got)
	}
}
