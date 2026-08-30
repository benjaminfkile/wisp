package policy

import (
	"reflect"
	"sort"
	"testing"
)

// levelSet renders a supported-set map as a sorted slice of level names for
// stable comparison in table tests.
func levelSet(m map[Isolation]bool) []string {
	out := make([]string, 0, len(m))
	for lvl, ok := range m {
		if ok {
			out = append(out, lvl.String())
		}
	}
	sort.Strings(out)
	return out
}

// TestSupportedIsolations verifies the runtime/OS → supported-level mapping:
// shared is always supported; sandboxed needs the gVisor runtime "runsc"; vm
// needs a Kata runtime ("kata-runtime" or "kata") on a Linux daemon, or a Windows
// daemon (Hyper-V). A Kata runtime on a Windows daemon does not by itself add vm
// (Hyper-V does); confidential is never supported.
func TestSupportedIsolations(t *testing.T) {
	tests := []struct {
		name     string
		runtimes []string
		os       string
		want     []string
	}{
		{"only runc", []string{"runc"}, "linux", []string{"shared"}},
		{"no runtimes reported", nil, "linux", []string{"shared"}},
		{"runsc adds sandboxed", []string{"runc", "runsc"}, "linux", []string{"sandboxed", "shared"}},
		{"kata-runtime on linux adds vm", []string{"runc", "kata-runtime"}, "linux", []string{"shared", "vm"}},
		{"kata alias on linux adds vm", []string{"runc", "kata"}, "linux", []string{"shared", "vm"}},
		{"windows daemon adds vm", []string{"runc"}, "windows", []string{"shared", "vm"}},
		{"runsc plus kata on linux", []string{"runc", "runsc", "kata-runtime"}, "linux", []string{"sandboxed", "shared", "vm"}},
		{"runsc on windows", []string{"runc", "runsc"}, "windows", []string{"sandboxed", "shared", "vm"}},
		// A Kata runtime registered on a windows daemon does not add vm via Kata,
		// but the windows daemon still provides vm via Hyper-V - so vm is present.
		{"kata on windows still vm via hyperv", []string{"runc", "kata-runtime"}, "windows", []string{"shared", "vm"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := levelSet(SupportedIsolations(HostCapabilities{Runtimes: tt.runtimes, OS: tt.os}))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SupportedIsolations(%v, %q) = %v, want %v", tt.runtimes, tt.os, got, tt.want)
			}
		})
	}
}

// TestSupportedIsolationsNeverConfidential verifies confidential is never in the
// supported set regardless of runtimes/OS - it parses for forward-compatibility
// but has no runtime backing.
func TestSupportedIsolationsNeverConfidential(t *testing.T) {
	for _, os := range []string{"linux", "windows"} {
		s := SupportedIsolations(HostCapabilities{Runtimes: []string{"runc", "runsc", "kata-runtime"}, OS: os})
		if s[IsolationConfidential] {
			t.Errorf("confidential reported supported on %s daemon, want never", os)
		}
	}
}

// TestEffectiveIsolationIntersects verifies the effective set is the operator
// allow-list intersected with the supported set, in allow-list order, and that
// unavailable levels are returned as dropped.
func TestEffectiveIsolationIntersects(t *testing.T) {
	cfg := &Config{
		Limits: Limits{
			Isolations:       []string{"shared", "sandboxed", "vm"},
			DefaultIsolation: "shared",
		},
	}
	// Host can only run shared: sandboxed and vm are dropped.
	supported := SupportedIsolations(HostCapabilities{Runtimes: []string{"runc"}, OS: "linux"})
	ic, dropped := cfg.EffectiveIsolation(supported)

	if got := isoNames(ic.Levels()); !reflect.DeepEqual(got, []string{"shared"}) {
		t.Errorf("effective levels = %v, want [shared]", got)
	}
	if got := isoNames(dropped); !reflect.DeepEqual(got, []string{"sandboxed", "vm"}) {
		t.Errorf("dropped = %v, want [sandboxed vm]", got)
	}
	if !ic.Allows(IsolationShared) {
		t.Error("Allows(shared) = false, want true")
	}
	if ic.Allows(IsolationVM) {
		t.Error("Allows(vm) = true, want false (host cannot run it)")
	}
	if ic.Default() != IsolationShared {
		t.Errorf("Default() = %q, want shared", ic.Default())
	}
}

// TestEffectiveIsolationKeepsAvailable verifies a level both allowed and
// supported survives the intersection with nothing dropped.
func TestEffectiveIsolationKeepsAvailable(t *testing.T) {
	cfg := &Config{
		Limits: Limits{
			Isolations:       []string{"shared", "vm"},
			DefaultIsolation: "vm",
		},
	}
	// A windows daemon provides vm (Hyper-V), so both survive.
	supported := SupportedIsolations(HostCapabilities{Runtimes: []string{"runc"}, OS: "windows"})
	ic, dropped := cfg.EffectiveIsolation(supported)

	if len(dropped) != 0 {
		t.Errorf("dropped = %v, want none", isoNames(dropped))
	}
	if got := isoNames(ic.Levels()); !reflect.DeepEqual(got, []string{"shared", "vm"}) {
		t.Errorf("effective levels = %v, want [shared vm]", got)
	}
	if !ic.Allows(IsolationVM) {
		t.Error("Allows(vm) = false on a windows daemon, want true")
	}
	if ic.Default() != IsolationVM {
		t.Errorf("Default() = %q, want vm (preserved when supported)", ic.Default())
	}
}

// TestEffectiveIsolationAllDropped verifies the fail-closed behaviour: when the
// operator's allow-list is non-empty but none of the configured levels are
// available on this host, the effective set is EMPTY and shared is NOT silently
// injected. The caller is expected to log an ERROR and advertise no isolation.
func TestEffectiveIsolationAllDropped(t *testing.T) {
	cfg := &Config{
		Limits: Limits{
			// Operator wants vm-only security; shared is explicitly excluded.
			Isolations:       []string{"vm"},
			DefaultIsolation: "vm",
		},
	}
	// Host has no Kata runtime - vm is unavailable.
	supported := SupportedIsolations(HostCapabilities{Runtimes: []string{"runc"}, OS: "linux"})
	ic, dropped := cfg.EffectiveIsolation(supported)

	if got := isoNames(dropped); !reflect.DeepEqual(got, []string{"vm"}) {
		t.Errorf("dropped = %v, want [vm]", got)
	}
	// All configured levels unavailable: effective set must be empty.
	if got := isoNames(ic.Levels()); len(got) != 0 {
		t.Errorf("effective levels = %v, want empty (no silent downgrade to shared)", got)
	}
	// shared must NOT be injected - the operator excluded it.
	if ic.Allows(IsolationShared) {
		t.Error("Allows(shared) = true, want false (operator excluded shared from allow-list)")
	}
	// No runnable default.
	if ic.Default() != "" {
		t.Errorf("Default() = %q, want empty (no runnable default)", ic.Default())
	}
}

// TestEffectiveIsolationDefaultDroppedOthersSurvive verifies that when the
// operator's allow-list produces a non-empty intersection but the configured
// default is not in it, the fallback uses the first remaining runnable level -
// NOT shared - when shared was excluded from the allow-list.
func TestEffectiveIsolationDefaultDroppedOthersSurvive(t *testing.T) {
	cfg := &Config{
		Limits: Limits{
			// Operator allows sandboxed and vm (no shared), prefers vm as default.
			// This host has gVisor (sandboxed) but no Kata (vm).
			Isolations:       []string{"sandboxed", "vm"},
			DefaultIsolation: "vm",
		},
	}
	// Host supports shared + sandboxed; vm requires kata which is absent.
	supported := SupportedIsolations(HostCapabilities{Runtimes: []string{"runc", "runsc"}, OS: "linux"})
	ic, dropped := cfg.EffectiveIsolation(supported)

	if got := isoNames(dropped); !reflect.DeepEqual(got, []string{"vm"}) {
		t.Errorf("dropped = %v, want [vm]", got)
	}
	if got := isoNames(ic.Levels()); !reflect.DeepEqual(got, []string{"sandboxed"}) {
		t.Errorf("effective levels = %v, want [sandboxed]", got)
	}
	// shared excluded from allow-list: must not be injected as a fallback default.
	if ic.Allows(IsolationShared) {
		t.Error("Allows(shared) = true, want false (shared excluded from allow-list)")
	}
	// Default falls to the first remaining runnable level, not shared.
	if ic.Default() != IsolationSandboxed {
		t.Errorf("Default() = %q, want sandboxed (first remaining runnable, not shared)", ic.Default())
	}
}

// TestEffectiveIsolationSharedExplicitlyExcluded verifies that when the
// operator's allow-list omits shared and the implicit default (empty
// DefaultIsolation → shared) is therefore not in the allow-list, shared is not
// injected into the effective set or used as the default.
func TestEffectiveIsolationSharedExplicitlyExcluded(t *testing.T) {
	cfg := &Config{
		Limits: Limits{
			// Operator listed only sandboxed - shared is explicitly excluded.
			// DefaultIsolation is empty, which Config.DefaultIsolation() maps to
			// shared, but shared is not in the allow-list.
			Isolations:       []string{"sandboxed"},
			DefaultIsolation: "",
		},
	}
	// Host supports shared and sandboxed (gVisor present).
	supported := SupportedIsolations(HostCapabilities{Runtimes: []string{"runc", "runsc"}, OS: "linux"})
	ic, dropped := cfg.EffectiveIsolation(supported)

	if len(dropped) != 0 {
		t.Errorf("dropped = %v, want none", isoNames(dropped))
	}
	if got := isoNames(ic.Levels()); !reflect.DeepEqual(got, []string{"sandboxed"}) {
		t.Errorf("effective levels = %v, want [sandboxed]", got)
	}
	// shared was not in the allow-list: it must not appear in the effective set.
	if ic.Allows(IsolationShared) {
		t.Error("Allows(shared) = true, want false (shared excluded from allow-list)")
	}
	// Default is the only allowed runnable level, not the implicit shared baseline.
	if ic.Default() != IsolationSandboxed {
		t.Errorf("Default() = %q, want sandboxed (not the implicit shared baseline)", ic.Default())
	}
}

// isoNames renders isolation levels as their canonical names for comparison.
func isoNames(levels []Isolation) []string {
	out := make([]string, 0, len(levels))
	for _, l := range levels {
		out = append(out, l.String())
	}
	return out
}
