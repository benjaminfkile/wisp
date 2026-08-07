package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/benjaminfkile/wisp/internal/bus"
	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/gpu"
	"github.com/benjaminfkile/wisp/internal/policy"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// gpuFake builds a Fake runtime that advertises the NVIDIA runtime and the given
// GPU devices, so a broker's startup GPU detection seeds its allocator with them.
func gpuFake(ids ...string) *runtime.Fake {
	f := runtime.NewFake()
	f.Runtimes = []string{"runc", runtime.NVIDIARuntimeName}
	for i, id := range ids {
		f.GPUDevices = append(f.GPUDevices, runtime.GPUDevice{ID: id, Class: "test-gpu", VRAMMB: 1024 * (i + 1)})
	}
	return f
}

// gpuBroker builds a broker over a GPU-capable fake plus a mux serving its
// routes, returning both so a test can drive the HTTP surface and inspect the
// shared allocator the handlers free into.
func gpuBroker(t *testing.T, ids ...string) (*broker, http.Handler, *contract.Store, *runtime.Fake) {
	t.Helper()
	store := contract.NewStore()
	fake := gpuFake(ids...)
	b := newBroker(store, fake, policy.Default(), bus.New(nil), discardLogger(), "")
	mux := http.NewServeMux()
	b.routes(mux)
	return b, mux, store, fake
}

// The broker's allocator is seeded from the detected GPU inventory in detection
// order, so the create path allocates from real devices.
func TestBrokerAllocatorSeededFromDetectedGPUs(t *testing.T) {
	b, _, _, _ := gpuBroker(t, "GPU-0", "GPU-1", "GPU-2")
	if got := b.alloc.Devices(); !reflect.DeepEqual(got, []string{"GPU-0", "GPU-1", "GPU-2"}) {
		t.Fatalf("allocator devices = %v, want [GPU-0 GPU-1 GPU-2]", got)
	}
	if b.alloc.Available() != 3 {
		t.Errorf("Available() = %d, want 3", b.alloc.Available())
	}
}

// A GPU-holding contract's status surfaces its assigned device IDs under the
// "gpus" wire field; a GPU-less contract carries no "gpus" key at all.
func TestStatusSurfacesGPUs(t *testing.T) {
	b, h, store, _ := gpuBroker(t, "GPU-0", "GPU-1")

	ids, err := b.alloc.Allocate(2)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	withGPUs, err := store.Create(contract.CreateParams{TTL: time.Hour, GPUDeviceIDs: ids})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	noGPUs, err := store.Create(contract.CreateParams{TTL: time.Hour})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rec := do(t, h, http.MethodGet, "/contracts/"+withGPUs.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw, ok := body["gpus"]
	if !ok {
		t.Fatalf("status body missing \"gpus\" field: %s", rec.Body.String())
	}
	list, ok := raw.([]any)
	if !ok || len(list) != 2 || list[0] != "GPU-0" || list[1] != "GPU-1" {
		t.Fatalf("gpus = %v, want [GPU-0 GPU-1]", raw)
	}

	rec = do(t, h, http.MethodGet, "/contracts/"+noGPUs.ID, "")
	var noGPUsBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &noGPUsBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := noGPUsBody["gpus"]; ok {
		t.Errorf("GPU-less contract must omit \"gpus\", got: %s", rec.Body.String())
	}
}

// Releasing a GPU-holding contract via DELETE returns its devices to the
// allocator, and a second DELETE (idempotent release) does not double-free.
func TestReleaseFreesGPUs(t *testing.T) {
	b, h, store, fake := gpuBroker(t, "GPU-0", "GPU-1")

	ids, err := b.alloc.Allocate(2)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	c := readyGPUContract(t, store, fake, ids)
	if b.alloc.Available() != 0 {
		t.Fatalf("Available() = %d before release, want 0", b.alloc.Available())
	}

	rec := do(t, h, http.MethodDelete, "/contracts/"+c.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("release status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got, _ := store.Get(c.ID); got.State != contract.StateReleased {
		t.Fatalf("state = %q, want released", got.State)
	}
	if b.alloc.Available() != 2 {
		t.Fatalf("Available() = %d after release, want 2", b.alloc.Available())
	}

	// A second DELETE is a no-op and must not corrupt occupancy.
	do(t, h, http.MethodDelete, "/contracts/"+c.ID, "")
	if b.alloc.Available() != 2 {
		t.Fatalf("Available() = %d after redundant release, want 2", b.alloc.Available())
	}
}

// A provisioning failure frees the lease's GPU devices so a failed create never
// strands a device as permanently occupied.
func TestProvisionFailureFreesGPUs(t *testing.T) {
	ctx := context.Background()
	b, _, store, fake := gpuBroker(t, "GPU-0", "GPU-1")

	ids, err := b.alloc.Allocate(1)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	c, err := store.Create(contract.CreateParams{TTL: time.Hour, GPUDeviceIDs: ids})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if b.alloc.Available() != 1 {
		t.Fatalf("Available() = %d, want 1 (one device held)", b.alloc.Available())
	}

	// Force the container create to fail so provision drives the contract through
	// fail(), which must free the held device.
	fake.CreateErr = context.DeadlineExceeded
	if _, err := b.provision(ctx, c, launchSpec{image: "wisp-base", isolation: policy.IsolationShared}, ""); err == nil {
		t.Fatal("provision succeeded, want failure")
	}
	if got, _ := store.Get(c.ID); got.State != contract.StateExpired {
		t.Fatalf("state = %q, want expired", got.State)
	}
	if b.alloc.Available() != 2 {
		t.Fatalf("Available() = %d after provision failure, want 2 (device freed)", b.alloc.Available())
	}
}

// createOptions writes the assigned device IDs as the comma-joined wisp.gpus
// label, and the reconcile reads them back to rebuild allocator occupancy after
// a simulated restart — the full label round-trip that keeps exclusivity across
// a restart. A GPU-less lease writes no label.
func TestGPUsLabelRoundTripThroughReconcile(t *testing.T) {
	ctx := context.Background()

	// Write side: a create records the assigned devices on the container label.
	c := contract.Contract{ID: "contract-1", TTL: time.Hour, ExpiresAt: time.Unix(1_900_000_000, 0), GPUDeviceIDs: []string{"GPU-0", "GPU-1"}}
	opts := createOptions(launchSpec{image: "wisp-base"}, c, runtime.OSLinux)
	if got := opts.Labels[gpusLabel]; got != "GPU-0,GPU-1" {
		t.Fatalf("wisp.gpus label = %q, want \"GPU-0,GPU-1\"", got)
	}
	if _, ok := createOptions(launchSpec{}, contract.Contract{ID: "x", TTL: time.Hour, ExpiresAt: time.Unix(1_900_000_000, 0)}, runtime.OSLinux).Labels[gpusLabel]; ok {
		t.Errorf("GPU-less lease must not write a wisp.gpus label")
	}

	// Read side: a fresh wispd restarts, sees the surviving container, and rebuilds
	// its allocator occupancy from that label.
	store := contract.NewStore()
	alloc := gpu.NewAllocator([]string{"GPU-0", "GPU-1", "GPU-2"})
	fake := runtime.NewFake()
	fake.LeasedContainers = []runtime.LeasedContainer{{ID: "cont-1", Labels: opts.Labels}}

	Reconcile(ctx, store, fake, alloc, discardLogger())

	got, err := store.Get("contract-1")
	if err != nil {
		t.Fatalf("contract-1 not adopted: %v", err)
	}
	if !reflect.DeepEqual(got.GPUDeviceIDs, []string{"GPU-0", "GPU-1"}) {
		t.Fatalf("adopted GPUDeviceIDs = %v, want [GPU-0 GPU-1]", got.GPUDeviceIDs)
	}
	if !reflect.DeepEqual(alloc.Occupied(), []string{"GPU-0", "GPU-1"}) {
		t.Fatalf("allocator occupied = %v, want [GPU-0 GPU-1]", alloc.Occupied())
	}
	// A new create after the restart can only get the one remaining free device.
	if _, err := alloc.Allocate(2); err == nil {
		t.Fatal("Allocate(2) after restart succeeded, want insufficient-devices error")
	}
	free, err := alloc.Allocate(1)
	if err != nil || !reflect.DeepEqual(free, []string{"GPU-2"}) {
		t.Fatalf("Allocate(1) = %v, %v, want [GPU-2], nil", free, err)
	}
}

// Reconcile must not double-assign a device a surviving lease holds, and must
// log-and-skip a label naming a device the host no longer detects rather than
// crashing.
func TestReconcileRebuildsOccupancyAcrossRestart(t *testing.T) {
	ctx := context.Background()
	store := contract.NewStore()
	alloc := gpu.NewAllocator([]string{"GPU-0", "GPU-1", "GPU-2"})
	fake := runtime.NewFake()

	expiry := strconv.FormatInt(time.Unix(1_900_000_000, 0).Unix(), 10)
	fake.LeasedContainers = []runtime.LeasedContainer{
		{ID: "cont-a", Labels: map[string]string{contractLabel: "contract-a", expiresAtLabel: expiry, gpusLabel: "GPU-0,GPU-1"}},
		// contract-b references GPU-2 (still present) and GPU-gone (removed since
		// launch): the reconcile reserves GPU-2 and logs/skips GPU-gone.
		{ID: "cont-b", Labels: map[string]string{contractLabel: "contract-b", expiresAtLabel: expiry, gpusLabel: "GPU-2,GPU-gone"}},
	}

	Reconcile(ctx, store, fake, alloc, discardLogger())

	// Both leases are tracked, each carrying the device IDs from its label verbatim.
	if got, _ := store.Get("contract-a"); !reflect.DeepEqual(got.GPUDeviceIDs, []string{"GPU-0", "GPU-1"}) {
		t.Fatalf("contract-a gpus = %v, want [GPU-0 GPU-1]", got.GPUDeviceIDs)
	}
	if got, _ := store.Get("contract-b"); !reflect.DeepEqual(got.GPUDeviceIDs, []string{"GPU-2", "GPU-gone"}) {
		t.Fatalf("contract-b gpus = %v, want [GPU-2 GPU-gone]", got.GPUDeviceIDs)
	}

	// Every detected device a surviving lease holds is occupied; the phantom
	// GPU-gone was skipped (not tracked as occupied, and no crash).
	if got := alloc.Occupied(); !reflect.DeepEqual(got, []string{"GPU-0", "GPU-1", "GPU-2"}) {
		t.Fatalf("occupied = %v, want [GPU-0 GPU-1 GPU-2]", got)
	}
	if alloc.Available() != 0 {
		t.Fatalf("Available() = %d, want 0 (every detected device held)", alloc.Available())
	}
	// A post-restart create cannot be handed any of those held devices.
	if _, err := alloc.Allocate(1); err == nil {
		t.Fatal("Allocate(1) succeeded after restart, want insufficient-devices error")
	}
}

// readyGPUContract creates a contract carrying the given device IDs and drives it
// to ready with a live fake container, so a release/reap path can exercise the
// GPU free.
func readyGPUContract(t *testing.T, store *contract.Store, fake *runtime.Fake, ids []string) contract.Contract {
	t.Helper()
	ctx := context.Background()
	c, err := store.Create(contract.CreateParams{TTL: time.Hour, GPUDeviceIDs: ids})
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
	c, err = store.UpdateState(c.ID, contract.StateReady)
	if err != nil {
		t.Fatalf("UpdateState ready: %v", err)
	}
	return c
}
