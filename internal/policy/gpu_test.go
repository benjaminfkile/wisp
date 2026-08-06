package policy

import (
	"reflect"
	"testing"
)

// gpuHost is a small helper building a GPUHostCapabilities with the NVIDIA
// runtime present and n synthetic devices, so the capability tests read cleanly.
func gpuHost(nvidia bool, n int) GPUHostCapabilities {
	var runtimes []string
	if nvidia {
		runtimes = []string{"runc", nvidiaRuntime}
	} else {
		runtimes = []string{"runc"}
	}
	devices := make([]GPUDevice, 0, n)
	for i := 0; i < n; i++ {
		devices = append(devices, GPUDevice{ID: "GPU-" + string(rune('a'+i)), Class: "nvidia-test", VRAMMB: 8192})
	}
	return GPUHostCapabilities{Runtimes: runtimes, Devices: devices}
}

// TestEffectiveGPUSupportedDefaults verifies the unconfigured default: with the
// NVIDIA runtime present and devices enumerated, GPUs are supported, the effective
// cap is all detected devices (zero operator cap = no cap), and the attach map
// advertises shared only.
func TestEffectiveGPUSupportedDefaults(t *testing.T) {
	cfg := &Config{} // no GPU limits configured -> enabled, uncapped
	g, drop := cfg.EffectiveGPU(gpuHost(true, 3))

	if !g.Supported() {
		t.Fatal("Supported() = false, want true on an NVIDIA host with devices")
	}
	if g.MaxGPUs() != 3 {
		t.Errorf("MaxGPUs() = %d, want 3 (all detected, no operator cap)", g.MaxGPUs())
	}
	if len(g.Devices()) != 3 {
		t.Errorf("Devices() len = %d, want 3", len(g.Devices()))
	}
	if got := isoNames(g.Isolations()); !reflect.DeepEqual(got, []string{"shared"}) {
		t.Errorf("Isolations() = %v, want [shared]", got)
	}
	if drop.Capped || drop.Disabled {
		t.Errorf("drop = %+v, want nothing withheld", drop)
	}
}

// TestEffectiveGPUCap verifies an operator per-lease cap below the detected count
// clamps the effective cap and is reported as a drop; a cap at or above the count
// leaves it at the detected count.
func TestEffectiveGPUCap(t *testing.T) {
	cfg := &Config{Limits: Limits{MaxGPUs: 1}}
	g, drop := cfg.EffectiveGPU(gpuHost(true, 4))
	if g.MaxGPUs() != 1 {
		t.Errorf("MaxGPUs() = %d, want 1 (operator cap)", g.MaxGPUs())
	}
	if !drop.Capped || drop.Cap != 1 || drop.Detected != 4 {
		t.Errorf("drop = %+v, want capped at 1 of 4 detected", drop)
	}

	// A cap >= detected count is not a drop and does not lower the effective cap.
	cfg = &Config{Limits: Limits{MaxGPUs: 8}}
	g, drop = cfg.EffectiveGPU(gpuHost(true, 4))
	if g.MaxGPUs() != 4 {
		t.Errorf("MaxGPUs() = %d, want 4 (cap above detected count)", g.MaxGPUs())
	}
	if drop.Capped {
		t.Errorf("drop.Capped = true, want false when the cap is above the detected count")
	}
}

// TestEffectiveGPUDisabled verifies the operator can disable GPU leasing outright
// even with hardware present: supported=false, the wire-empty shape, and a drop
// flagging the withheld hardware.
func TestEffectiveGPUDisabled(t *testing.T) {
	cfg := &Config{Limits: Limits{GPUsDisabled: true}}
	g, drop := cfg.EffectiveGPU(gpuHost(true, 2))
	if g.Supported() {
		t.Error("Supported() = true, want false when the operator disabled GPU leasing")
	}
	if g.MaxGPUs() != 0 || len(g.Devices()) != 0 {
		t.Errorf("disabled posture = {max:%d devices:%d}, want the empty shape (0 / [])", g.MaxGPUs(), len(g.Devices()))
	}
	if len(g.Isolations()) != 0 {
		t.Errorf("Isolations() = %v, want empty when unsupported", isoNames(g.Isolations()))
	}
	if !drop.Disabled || drop.Detected != 2 {
		t.Errorf("drop = %+v, want disabled with 2 detected", drop)
	}
}

// TestEffectiveGPUUnsupported verifies the two ways a host lacks GPU support —
// no NVIDIA runtime, or the runtime present but no device enumerated — both yield
// the wire-contract empty shape and no drop (nothing was withheld, there was
// nothing to withhold).
func TestEffectiveGPUUnsupported(t *testing.T) {
	cases := []struct {
		name string
		host GPUHostCapabilities
	}{
		{"no nvidia runtime", gpuHost(false, 2)}, // devices but runtime absent
		{"nvidia runtime, no devices", gpuHost(true, 0)},
		{"neither", gpuHost(false, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, drop := (&Config{}).EffectiveGPU(tc.host)
			if g.Supported() {
				t.Error("Supported() = true, want false")
			}
			if g.MaxGPUs() != 0 {
				t.Errorf("MaxGPUs() = %d, want 0", g.MaxGPUs())
			}
			if len(g.Devices()) != 0 {
				t.Errorf("Devices() len = %d, want 0", len(g.Devices()))
			}
			if drop.Disabled || drop.Capped {
				t.Errorf("drop = %+v, want nothing withheld", drop)
			}
		})
	}
}

// TestGPUAttachIsDataWithVMSlot verifies the KATA-INTENT rule: GPU-attach per
// isolation level is a computed map with the vm/kata slot PRESENT and false in
// v1, not a hard-coded shared-only check. Flipping the vm entry to true later is
// the only change needed to advertise vm — the Isolations() list and Attach()
// read straight from the map.
func TestGPUAttachIsDataWithVMSlot(t *testing.T) {
	// The map itself carries the vm slot regardless of host support.
	m := gpuAttachByIsolation(true)
	if _, ok := m[IsolationVM]; !ok {
		t.Fatal("attach map is missing the vm slot; it must be present as data")
	}
	if m[IsolationVM] {
		t.Error("attach map vm = true, want false (no VM GPU backend in v1)")
	}
	if !m[IsolationShared] {
		t.Error("attach map shared = false, want true when the host supports GPUs")
	}

	// On a supported host, attach is available at shared and not vm...
	g, _ := (&Config{}).EffectiveGPU(gpuHost(true, 1))
	if !g.Attach(IsolationShared) {
		t.Error("Attach(shared) = false, want true on a supported host")
	}
	if g.Attach(IsolationVM) {
		t.Error("Attach(vm) = true, want false (vm slot is data, currently false)")
	}

	// ...and unavailable everywhere on an unsupported host, vm slot still present.
	un, _ := (&Config{}).EffectiveGPU(gpuHost(false, 0))
	if un.Attach(IsolationShared) || un.Attach(IsolationVM) {
		t.Error("unsupported host advertises GPU attach, want none")
	}
}

// TestGPUDevicesAndIsolationsAreCopies verifies the accessor slices are fresh
// non-nil copies safe for the caller to retain or serialize (mirroring the
// isolation accessors).
func TestGPUDevicesAndIsolationsAreCopies(t *testing.T) {
	g, _ := (&Config{}).EffectiveGPU(gpuHost(true, 2))
	d1 := g.Devices()
	d1[0].ID = "mutated"
	if g.Devices()[0].ID == "mutated" {
		t.Error("Devices() returned an aliased slice; a caller's mutation leaked back")
	}
	if g.Isolations() == nil {
		t.Error("Isolations() = nil, want a non-nil slice")
	}
}
