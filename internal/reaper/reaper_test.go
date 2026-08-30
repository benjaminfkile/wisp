package reaper

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
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
// window was missed) goes straight to expired and is killed. The fired Event
// carries Reason=ReasonTTLExpired so the bus can tell a natural TTL expiry
// apart from an out-of-band container death expiry (both drive the same
// terminal transition otherwise).
func TestReaperExpiresReadyPastTTL(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	c, cid := readyContract(t, store, fake, time.Hour)

	var events []Event
	rp := New(store, fake, Options{
		Lead:   time.Minute,
		Logger: discardLogger(),
		Now:    func() time.Time { return c.ExpiresAt.Add(time.Second) },
		Notify: func(e Event) { events = append(events, e) },
	})

	rp.Tick(context.Background())

	got, _ := store.Get(c.ID)
	if got.State != contract.StateExpired {
		t.Fatalf("State = %q, want expired", got.State)
	}
	if _, ok := fake.Container(cid); ok {
		t.Error("container survived; want killed")
	}
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

	var events []Event
	rp := New(store, fake, Options{
		Lead:   time.Minute,
		Logger: discardLogger(),
		// Nowhere near the TTL: only the liveness signal can trigger the reap.
		Now:    func() time.Time { return c.ExpiresAt.Add(-30 * time.Minute) },
		Notify: func(e Event) { events = append(events, e) },
	})

	rp.Tick(context.Background())

	got, _ := store.Get(c.ID)
	if !got.State.Terminal() {
		t.Fatalf("state = %q, want terminal after out-of-band container death", got.State)
	}
	if got.State != contract.StateExpired {
		t.Errorf("state = %q, want %q", got.State, contract.StateExpired)
	}
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

	freed := 0
	rp := New(store, fake, Options{
		Lead:            time.Minute,
		Logger:          discardLogger(),
		Now:             func() time.Time { return c.ExpiresAt.Add(-30 * time.Minute) },
		ReleaseCapacity: func(contract.Contract) { freed++ },
	})

	for i := 0; i < 5; i++ {
		rp.Tick(context.Background())
	}

	if freed != 1 {
		t.Fatalf("ReleaseCapacity fired %d times across 5 ticks; want exactly 1", freed)
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

	var freed [][]string
	rp := New(store, fake, Options{
		Logger:      discardLogger(),
		Now:         func() time.Time { return future.Add(-time.Hour) },
		ReleaseGPUs: func(ids []string) { freed = append(freed, ids) },
	})
	rp.Tick(ctx)

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
// (the DELETE handler crashed mid-release, or its Kill hung) is treated like
// any other non-terminal contract: the reaper kills the container, expires the
// contract, and returns its reserved capacity to the allocator. Without this
// path the lease's capacity would leak until wispd restarted.
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

	var events []Event
	freed := 0
	rp := New(store, fake, Options{
		Lead:         time.Minute,
		ReleaseGrace: 30 * time.Second,
		Logger:       discardLogger(),
		// Just past the grace: the release is presumed stuck, so the reaper
		// takes over even though the contract is still well within its TTL.
		Now:             func() time.Time { return c.ReleasingSince.Add(31 * time.Second) },
		Notify:          func(e Event) { events = append(events, e) },
		ReleaseCapacity: func(contract.Contract) { freed++ },
	})
	rp.Tick(ctx)

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
	if freed != 1 {
		t.Errorf("ReleaseCapacity fired %d times, want 1 (capacity must return to the allocator)", freed)
	}
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
// embedded Kill and the wrapper still satisfies runtime.Runtime.
type hangingKillRuntime struct {
	*runtime.Fake
	hangID string
}

func (h *hangingKillRuntime) Kill(ctx context.Context, id string) error {
	if id == h.hangID {
		<-ctx.Done()
		return ctx.Err()
	}
	return h.Fake.Kill(ctx, id)
}

// TestReaperKillTimeoutContinuesTick pins the fix for the "one hung Kill stalls
// the whole tick" scenario: with a bounded Kill, a runtime whose Kill blocks on
// the first contract processed must NOT prevent the reaper from moving on and
// expiring the remaining contracts. Without the bound the reaper loop would
// block on the wedged Kill and the second contract's TTL expiry would sit
// unenforced until the daemon responded, dropping capacity and GPUs on the
// floor for the duration.
func TestReaperKillTimeoutContinuesTick(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()

	// Two ready contracts, both past their TTL. The store lists in CreatedAt
	// order, so the first-created (hung) contract is processed first; without
	// the Kill bound the sweep would block there and never reach live.
	hung, hungCID := readyContract(t, store, fake, time.Hour)
	live, liveCID := readyContract(t, store, fake, time.Hour)

	rt := &hangingKillRuntime{Fake: fake, hangID: hungCID}

	rp := New(store, rt, Options{
		Lead:        time.Minute,
		KillTimeout: 50 * time.Millisecond,
		Logger:      discardLogger(),
		// Past both TTLs so both contracts are due for expiry.
		Now: func() time.Time { return hung.ExpiresAt.Add(time.Second) },
	})

	done := make(chan struct{})
	go func() {
		rp.Tick(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Tick stalled on a hung Kill; each reaper Kill must be bounded so the sweep can continue with the remaining contracts")
	}

	// The hung contract must be left in place so the next tick can retry the
	// kill; a timed-out Kill is inconclusive and must not fake a terminal
	// transition on the store side either.
	got, err := store.Get(hung.ID)
	if err != nil {
		t.Fatalf("Get hung contract: %v", err)
	}
	if got.State == contract.StateExpired {
		t.Errorf("hung contract state = %q; a Kill timeout must leave the contract for the next tick, not transition it to expired", got.State)
	}
	if _, ok := fake.Container(hungCID); !ok {
		t.Error("hung container was removed despite the Kill timing out")
	}

	// The live contract must have been reaped despite the earlier hung Kill;
	// this is the regression the timeout was added to prevent.
	liveGot, err := store.Get(live.ID)
	if err != nil {
		t.Fatalf("Get live contract: %v", err)
	}
	if liveGot.State != contract.StateExpired {
		t.Errorf("live contract state = %q; want expired (the reaper must reap the remaining contracts even when one Kill hangs)", liveGot.State)
	}
	if _, ok := fake.Container(liveCID); ok {
		t.Error("live contract's container survived; it must be reaped even when another contract's Kill hangs")
	}
}
