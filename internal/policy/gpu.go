package policy

// GPU capability detection. Wisp advertises GPU capability the same data-driven
// way it advertises isolation capability (see capability.go): the runtime layer
// reports plain facts — the daemon's registered runtimes and the enumerated
// devices — and this file intersects them with the operator's GPU config to get
// the EFFECTIVE posture the read surface advertises (the /images "gpu" block) and
// the create path enforces (see docs/DESIGN.md §7).
//
// Policy stays free of a runtime import, exactly as the isolation capability does:
// the caller passes the daemon's runtime names and the enumerated devices through
// as plain values.

// nvidiaRuntime is the OCI runtime name the NVIDIA Container Runtime registers
// with the daemon; its presence in the daemon's runtime list is the necessary
// daemon-side signal that GPUs can be attached. It mirrors runtime.NVIDIARuntimeName
// but is kept here as a plain string so policy does not depend on the runtime
// package, exactly as runscRuntime / kataRuntime are (see capability.go).
const nvidiaRuntime = "nvidia"

// GPUDevice is one enumerated GPU as plain policy-layer data: its stable UUID,
// its normalized product class, and its total video memory in mebibytes. It is
// the value bridge from runtime.GPUDevice into the capability computation and out
// to the /images "gpu" block (field-for-field the wire contract's device shape).
type GPUDevice struct {
	// ID is the GPU's globally-unique identifier (a "GPU-<uuid>" string).
	ID string

	// Class is the normalized product name (lowercase, whitespace → hyphens);
	// opaque to consumers (see runtime.normalizeGPUClass).
	Class string

	// VRAMMB is the device's total video memory in mebibytes.
	VRAMMB int
}

// GPUHostCapabilities describes what the host physically offers for GPU leasing:
// the daemon's registered OCI runtime names (the "nvidia" runtime gates support)
// and the devices nvidia-smi enumerated. It is the plain-value bridge from the
// runtime layer's daemon query + GPU enumeration into EffectiveGPU, mirroring how
// HostCapabilities bridges into SupportedIsolations.
type GPUHostCapabilities struct {
	// Runtimes is the set of registered OCI runtime names as reported by the
	// daemon; order does not matter. GPU support requires "nvidia" to be present.
	Runtimes []string

	// Devices is the set of GPUs nvidia-smi enumerated. Empty means the host
	// exposed no GPUs (or enumeration failed and the caller degraded to empty).
	Devices []GPUDevice
}

// hasNVIDIARuntime reports whether the daemon advertises the NVIDIA runtime — the
// daemon-side half of GPU support. It scans the runtime names as plain strings,
// mirroring how SupportedIsolations scans them for runsc / kata.
func hasNVIDIARuntime(runtimes []string) bool {
	for _, r := range runtimes {
		if r == nvidiaRuntime {
			return true
		}
	}
	return false
}

// gpuAttachByIsolation computes, per isolation level, whether a GPU can be
// attached to a lease AT THAT LEVEL on this host. This is DATA, not a hard-coded
// check: the kata/vm slot is present in the map but currently false, so adding a
// GPU passthrough backend later (e.g. Kata + VFIO for vm) is implementing that
// backend and flipping its entry to true here — with ZERO call-site changes (the
// KATA-INTENT rule; see docs/DESIGN.md §7).
//
// v1 ships exactly one GPU backend: runc/shared, which attaches devices via
// Docker DeviceRequests. So shared is true whenever the host supports GPUs at
// all, and vm — which would need a Kata + VFIO backend that does not exist yet —
// is present but always false. The advertised "isolations" list is the keys that
// map to true (at most ["shared"] in v1).
func gpuAttachByIsolation(supported bool) map[Isolation]bool {
	return map[Isolation]bool{
		IsolationShared: supported,
		IsolationVM:     false,
	}
}

// GPUCapabilities is a host's effective GPU posture: whether GPUs may be leased
// at all, the enumerated devices, the effective per-lease cap, and the
// per-isolation attach map. It is computed once at startup (see
// Config.EffectiveGPU), then advertised on the read surface (the /images "gpu"
// block) and consulted by the create path.
type GPUCapabilities struct {
	supported bool
	devices   []GPUDevice
	maxGPUs   int
	attach    map[Isolation]bool
}

// Supported reports whether GPUs may be leased on this host: the daemon
// advertises the NVIDIA runtime, nvidia-smi enumerated at least one device, and
// the operator did not disable GPU leasing.
func (g GPUCapabilities) Supported() bool { return g.supported }

// Devices returns the enumerated GPUs, in enumeration order. The result is a
// fresh, non-nil slice (empty when GPUs are unsupported, since the wire contract
// requires devices=[] then) safe for the caller to retain or serialize.
func (g GPUCapabilities) Devices() []GPUDevice {
	out := make([]GPUDevice, len(g.devices))
	copy(out, g.devices)
	return out
}

// MaxGPUs returns the effective per-lease GPU cap: min(operator cap, detected
// device count), or 0 when GPUs are unsupported (as the wire contract requires).
func (g GPUCapabilities) MaxGPUs() int { return g.maxGPUs }

// Attach reports whether a GPU can be attached to a lease at the given isolation
// level on this host. It reads the computed attach map, so the vm/kata slot is
// answered from data (false in v1), not a hard-coded check.
func (g GPUCapabilities) Attach(level Isolation) bool { return g.attach[level] }

// Isolations returns the isolation levels at which GPU attach is available on
// this host — the attach map's true entries — in isolation-rank order (shared
// before vm). The result is a fresh, non-nil slice; in v1 it is at most
// ["shared"]. This is the wire contract's "isolations" list.
func (g GPUCapabilities) Isolations() []Isolation {
	out := make([]Isolation, 0, len(g.attach))
	// Iterate the known levels in rank order so the advertised list is stable and
	// independent of Go's map iteration order.
	for _, lvl := range []Isolation{IsolationShared, IsolationSandboxed, IsolationVM, IsolationConfidential} {
		if g.attach[lvl] {
			out = append(out, lvl)
		}
	}
	return out
}

// GPUDrop describes GPU capacity the operator's policy withheld from what the
// host physically offers, so the caller can log it at startup — mirroring the
// dropped-levels warning EffectiveIsolation returns. A zero GPUDrop means the
// operator withheld nothing (or the host has no GPUs to withhold).
type GPUDrop struct {
	// Detected is the number of GPUs the host enumerated, regardless of policy.
	Detected int

	// Disabled is true when the host has usable GPUs but the operator disabled
	// GPU leasing outright, so every detected device is withheld.
	Disabled bool

	// Capped is true when the operator's per-lease cap is below the detected
	// device count, so a single lease cannot use every device. Cap is that
	// configured cap.
	Capped bool
	Cap    int
}

// EffectiveGPU intersects the operator's GPU config with what the host physically
// offers (the NVIDIA runtime plus the enumerated devices), returning the
// effective posture plus a GPUDrop describing any capacity the policy withheld so
// the caller can log a clear startup warning (mirroring EffectiveIsolation).
//
// A host supports GPUs iff the daemon advertises the "nvidia" runtime AND at
// least one device was enumerated; the operator may additionally disable leasing
// outright (Limits.GPUsDisabled). When supported, the effective per-lease cap is
// min(operator cap, detected count) — a zero operator cap (the unconfigured
// default) means "no operator cap", i.e. all detected devices. When unsupported,
// the posture is the wire contract's empty shape (devices=[], max_gpus=0) and the
// attach map is all-false. The per-isolation attach map is always present with
// the vm slot computed as data (see gpuAttachByIsolation).
func (c *Config) EffectiveGPU(host GPUHostCapabilities) (GPUCapabilities, GPUDrop) {
	drop := GPUDrop{Detected: len(host.Devices)}

	// The host physically supports GPUs iff both halves of detection agree.
	hostSupported := hasNVIDIARuntime(host.Runtimes) && len(host.Devices) > 0
	// The operator can veto GPU leasing regardless of hardware.
	enabled := !c.Limits.GPUsDisabled
	supported := hostSupported && enabled
	if hostSupported && !enabled {
		drop.Disabled = true
	}

	g := GPUCapabilities{
		supported: supported,
		attach:    gpuAttachByIsolation(supported),
	}
	if supported {
		g.devices = append([]GPUDevice(nil), host.Devices...)
		// Effective per-lease cap = min(operator cap, detected count). A zero
		// operator cap imposes no operator ceiling → all detected devices, mirroring
		// how a zero numeric limit means "no cap" elsewhere in Limits.
		g.maxGPUs = len(host.Devices)
		if c.Limits.MaxGPUs > 0 && c.Limits.MaxGPUs < g.maxGPUs {
			g.maxGPUs = c.Limits.MaxGPUs
			drop.Capped = true
			drop.Cap = c.Limits.MaxGPUs
		}
	}
	return g, drop
}
