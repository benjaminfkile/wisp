package policy

// Isolation capability detection. A host can only run an isolation level whose
// backing container runtime is actually installed, so the daemon must advertise
// and accept only the levels it can provide. This file maps a daemon's registered
// runtimes and container OS to the SUPPORTED set of levels, and intersects that
// with the operator's allow-list to get the EFFECTIVE posture the create path
// enforces and the read surface advertises.
//
// Policy stays free of a runtime import (mirroring how it compares the container
// OS as a plain string): the caller passes the daemon's runtime names and OS
// through as plain values.

// Runtime names that back the stronger isolation levels. gVisor registers its
// OCI runtime as "runsc"; Kata Containers registers as "kata-runtime", with the
// short alias "kata" also seen in the wild. These mirror the runtime package's
// launch-mechanism names but are kept here as plain strings so policy does not
// depend on the runtime package.
const (
	runscRuntime   = "runsc"
	kataRuntime    = "kata-runtime"
	kataRuntimeAlt = "kata"
)

// HostCapabilities describes what a container daemon can actually run: its
// registered OCI runtime names and its container OS mode ("linux" or "windows").
// It is the plain-value bridge from the runtime layer's daemon query into the
// isolation-capability computation.
type HostCapabilities struct {
	// Runtimes is the set of registered OCI runtime names (e.g. "runc", "runsc",
	// "kata-runtime"); order does not matter.
	Runtimes []string

	// OS is the daemon's container OS mode as a plain string ("linux"/"windows").
	OS string
}

// SupportedIsolations returns the set of isolation levels the host can actually
// provide, given the daemon's registered runtimes and container OS:
//
//   - shared is ALWAYS supported (the default OCI runtime, runc);
//   - sandboxed iff the gVisor runtime "runsc" is registered;
//   - vm iff a Kata runtime ("kata-runtime" or "kata") is registered on a Linux
//     daemon, OR the daemon runs Windows containers (Hyper-V isolation).
//
// confidential is never supported here — it parses for forward-compatibility but
// has no runtime backing yet. The returned map contains only the supported
// levels (each mapped to true).
func SupportedIsolations(h HostCapabilities) map[Isolation]bool {
	supported := map[Isolation]bool{IsolationShared: true}
	for _, r := range h.Runtimes {
		switch r {
		case runscRuntime:
			supported[IsolationSandboxed] = true
		case kataRuntime, kataRuntimeAlt:
			// A Kata runtime backs vm on a Linux daemon; on a Windows daemon vm is
			// instead Hyper-V isolation, handled below, so a registered Kata runtime
			// there does not add vm on its own.
			if h.OS != osWindows {
				supported[IsolationVM] = true
			}
		}
	}
	// A Windows-container daemon provides vm via Hyper-V isolation regardless of
	// which named runtimes are registered.
	if h.OS == osWindows {
		supported[IsolationVM] = true
	}
	return supported
}

// IsolationCapabilities is a host's effective isolation posture: the levels it
// will actually accept — the operator allow-list intersected with what the daemon
// can run — and the default level applied when a create omits one. It is computed
// once at startup (see Config.EffectiveIsolation), then consulted by the create
// path (Allows / Default) and advertised on the read surface (Levels / Default).
type IsolationCapabilities struct {
	order   []Isolation
	allowed map[Isolation]bool
	def     Isolation
}

// Levels returns the effective allowed levels in the operator's configured order.
// The result is a fresh, non-nil slice (empty when nothing is allowed) safe for
// the caller to retain or serialize.
func (ic IsolationCapabilities) Levels() []Isolation {
	out := make([]Isolation, len(ic.order))
	copy(out, ic.order)
	return out
}

// Allows reports whether level is in the effective allowed set — i.e. both
// permitted by the operator and runnable on this host.
func (ic IsolationCapabilities) Allows(level Isolation) bool {
	return ic.allowed[level]
}

// Default returns the effective default isolation level applied when a create
// omits one.
func (ic IsolationCapabilities) Default() Isolation {
	return ic.def
}

// EffectiveIsolation intersects the operator's isolation allow-list with the
// levels the host can actually provide (supported, from SupportedIsolations),
// returning the effective posture plus the policy-allowed levels the host cannot
// run (dropped), in allow-list order, so the caller can log a clear startup
// warning for each. The default level is preserved when it survives the
// intersection; otherwise it falls back to the always-available shared baseline
// (which is added to the effective set when it was itself dropped), so the host
// always has a runnable default and never advertises or accepts a level it cannot
// launch.
func (c *Config) EffectiveIsolation(supported map[Isolation]bool) (IsolationCapabilities, []Isolation) {
	ic := IsolationCapabilities{allowed: map[Isolation]bool{}}
	var dropped []Isolation
	for _, l := range c.Limits.Isolations {
		lvl := Isolation(l)
		if supported[lvl] {
			if !ic.allowed[lvl] {
				ic.allowed[lvl] = true
				ic.order = append(ic.order, lvl)
			}
		} else {
			dropped = append(dropped, lvl)
		}
	}
	def := c.DefaultIsolation()
	if !ic.allowed[def] {
		// The configured default was dropped (or never allowed) on this host; fall
		// back to the always-supported shared baseline so a create that omits a
		// level still resolves to a runnable one, adding shared to the effective set
		// if it was itself dropped.
		def = IsolationShared
		if !ic.allowed[IsolationShared] {
			ic.allowed[IsolationShared] = true
			ic.order = append([]Isolation{IsolationShared}, ic.order...)
		}
	}
	ic.def = def
	return ic, dropped
}
