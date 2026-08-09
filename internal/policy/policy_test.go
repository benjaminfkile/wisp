package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	c := Default()
	// The built-in policy allows both base images so the same default serves a
	// Linux or a Windows daemon; the Linux base is the configured default image.
	if len(c.Allow) != 2 || c.Allow[0] != baseImage || c.Allow[1] != windowsBaseImage {
		t.Errorf("Allow = %v, want [%s %s]", c.Allow, baseImage, windowsBaseImage)
	}
	if c.DefaultImage != baseImage {
		t.Errorf("DefaultImage = %q, want %q", c.DefaultImage, baseImage)
	}
	if !c.AllowsImage(baseImage) || !c.AllowsImage(windowsBaseImage) {
		t.Errorf("Default should allow both %q and %q", baseImage, windowsBaseImage)
	}
	if !c.AllowsNetwork(NetworkNone) || !c.AllowsNetwork(NetworkOpen) {
		t.Errorf("Default networks = %v, want none+open", c.Limits.Networks)
	}
	if c.AllowsNetwork(NetworkEgress) {
		t.Errorf("Default should not allow egress")
	}
	// The built-in policy ships conservative non-zero ceilings so an
	// uncustomized lease cannot exhaust the host or run forever.
	if c.Limits.MaxTTLSeconds != 3600 {
		t.Errorf("MaxTTLSeconds = %d, want 3600", c.Limits.MaxTTLSeconds)
	}
	if c.Limits.MaxCPUs != 4 {
		t.Errorf("MaxCPUs = %v, want 4", c.Limits.MaxCPUs)
	}
	if c.Limits.MaxMemoryMB != 4096 {
		t.Errorf("MaxMemoryMB = %d, want 4096", c.Limits.MaxMemoryMB)
	}
	if c.Limits.PidsLimit != 512 {
		t.Errorf("PidsLimit = %d, want 512", c.Limits.PidsLimit)
	}
	// The host capacity budgets default to 0/unlimited so existing operator configs
	// keep their current behavior (enforcement lands in a later task).
	if c.Limits.MaxContracts != 0 || c.Limits.TotalCPUs != 0 || c.Limits.TotalMemoryMB != 0 {
		t.Errorf("capacity budgets = {%d, %v, %d}, want all 0/unlimited",
			c.Limits.MaxContracts, c.Limits.TotalCPUs, c.Limits.TotalMemoryMB)
	}
	// A request over a ceiling is clamped down to it.
	if got := c.ClampTTL(100 * time.Hour); got != time.Hour {
		t.Errorf("ClampTTL = %v, want 1h", got)
	}
	if got := c.ClampCPUs(64); got != 4 {
		t.Errorf("ClampCPUs = %v, want 4", got)
	}
	if got := c.ClampMemoryMB(1 << 20); got != 4096 {
		t.Errorf("ClampMemoryMB = %v, want 4096", got)
	}
	if got := c.ClampPids(9999); got != 512 {
		t.Errorf("ClampPids = %v, want 512", got)
	}
	// An unset (zero) resource request inherits the ceiling instead of running
	// uncapped, so a lease requesting resources:{} is bounded by the defaults.
	if got := c.ClampCPUs(0); got != 4 {
		t.Errorf("ClampCPUs(0) = %v, want 4 (inherits ceiling)", got)
	}
	if got := c.ClampMemoryMB(0); got != 4096 {
		t.Errorf("ClampMemoryMB(0) = %v, want 4096 (inherits ceiling)", got)
	}
	if got := c.ClampPids(0); got != 512 {
		t.Errorf("ClampPids(0) = %v, want 512 (inherits ceiling)", got)
	}
}

func TestDefaultImageForOS(t *testing.T) {
	// The built-in policy allow-lists both base images, so the default tracks the
	// daemon OS: the Windows base on a windows-mode host, the Linux base otherwise.
	c := Default()
	if got := c.DefaultImageFor("linux"); got != baseImage {
		t.Errorf("DefaultImageFor(linux) = %q, want %q", got, baseImage)
	}
	if got := c.DefaultImageFor(""); got != baseImage {
		t.Errorf("DefaultImageFor(\"\") = %q, want %q (linux fallback)", got, baseImage)
	}
	if got := c.DefaultImageFor("windows"); got != windowsBaseImage {
		t.Errorf("DefaultImageFor(windows) = %q, want %q", got, windowsBaseImage)
	}

	// A policy that does NOT allow-list the Windows base falls back to its
	// configured default even on a windows host (the result is always allowed).
	linuxOnly := &Config{Allow: []string{baseImage}, DefaultImage: baseImage}
	if got := linuxOnly.DefaultImageFor("windows"); got != baseImage {
		t.Errorf("DefaultImageFor(windows) with no windows image = %q, want %q", got, baseImage)
	}
}

func TestLoadEmptyPathReturnsDefault(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if c.DefaultImage != baseImage {
		t.Errorf("DefaultImage = %q, want %q", c.DefaultImage, baseImage)
	}
}

func TestDefaultNetwork(t *testing.T) {
	// "open" wins when allowed.
	c := &Config{Limits: Limits{Networks: []string{NetworkNone, NetworkOpen}}}
	if got := c.DefaultNetwork(); got != NetworkOpen {
		t.Errorf("DefaultNetwork = %q, want open", got)
	}
	// Otherwise the first configured network.
	c = &Config{Limits: Limits{Networks: []string{NetworkNone, NetworkEgress}}}
	if got := c.DefaultNetwork(); got != NetworkNone {
		t.Errorf("DefaultNetwork = %q, want none", got)
	}
}

func TestClampToLimits(t *testing.T) {
	c := &Config{Limits: Limits{
		MaxTTLSeconds: 900,
		MaxCPUs:       2,
		MaxMemoryMB:   1024,
		PidsLimit:     256,
	}}
	if got := c.ClampTTL(time.Hour); got != 15*time.Minute {
		t.Errorf("ClampTTL = %v, want 15m", got)
	}
	if got := c.ClampTTL(5 * time.Minute); got != 5*time.Minute {
		t.Errorf("ClampTTL under cap = %v, want unchanged", got)
	}
	if got := c.ClampCPUs(8); got != 2 {
		t.Errorf("ClampCPUs = %v, want 2", got)
	}
	if got := c.ClampMemoryMB(8192); got != 1024 {
		t.Errorf("ClampMemoryMB = %v, want 1024", got)
	}
	if got := c.ClampPids(9999); got != 256 {
		t.Errorf("ClampPids = %v, want 256", got)
	}
}

func TestParseIsolation(t *testing.T) {
	// Every known level parses, case-insensitively and space-trimmed, including
	// confidential (which Validate rejects separately).
	cases := map[string]Isolation{
		"shared":        IsolationShared,
		"SHARED":        IsolationShared,
		"  Sandboxed  ": IsolationSandboxed,
		"VM":            IsolationVM,
		"Confidential":  IsolationConfidential,
	}
	for in, want := range cases {
		got, err := ParseIsolation(in)
		if err != nil {
			t.Errorf("ParseIsolation(%q): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseIsolation(%q) = %q, want %q", in, got, want)
		}
	}
	// An unknown string is an error.
	if _, err := ParseIsolation("turtle"); err == nil {
		t.Error("ParseIsolation(turtle) = nil error, want error")
	}
	if _, err := ParseIsolation(""); err == nil {
		t.Error("ParseIsolation(\"\") = nil error, want error (empty is not defaulted here)")
	}
}

func TestIsolationOrdering(t *testing.T) {
	// The levels form a strict total order shared < sandboxed < vm < confidential.
	if !(IsolationShared.Rank() < IsolationSandboxed.Rank() &&
		IsolationSandboxed.Rank() < IsolationVM.Rank() &&
		IsolationVM.Rank() < IsolationConfidential.Rank()) {
		t.Errorf("ranks not strictly increasing: shared=%d sandboxed=%d vm=%d confidential=%d",
			IsolationShared.Rank(), IsolationSandboxed.Rank(), IsolationVM.Rank(), IsolationConfidential.Rank())
	}
	// An unknown level ranks below every recognized level.
	if Isolation("turtle").Rank() != -1 {
		t.Errorf("unknown Rank() = %d, want -1", Isolation("turtle").Rank())
	}
}

func TestIsolationValidate(t *testing.T) {
	// The launchable levels validate.
	for _, lvl := range []Isolation{IsolationShared, IsolationSandboxed, IsolationVM} {
		if err := lvl.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", lvl, err)
		}
	}
	// confidential is known (parses) but rejected as not supported yet.
	err := IsolationConfidential.Validate()
	if err == nil {
		t.Fatal("Validate(confidential) = nil, want not-supported error")
	}
	if !strings.Contains(err.Error(), "not supported yet") {
		t.Errorf("Validate(confidential) error = %q, want it to mention 'not supported yet'", err)
	}
	// An unknown level is rejected too.
	if err := Isolation("turtle").Validate(); err == nil {
		t.Error("Validate(turtle) = nil, want error")
	}
}

func TestDefaultAllowsOnlyShared(t *testing.T) {
	c := Default()
	if !c.AllowsIsolation(IsolationShared) {
		t.Error("Default should allow shared")
	}
	if c.AllowsIsolation(IsolationSandboxed) || c.AllowsIsolation(IsolationVM) || c.AllowsIsolation(IsolationConfidential) {
		t.Errorf("Default should allow ONLY shared, got %v", c.Limits.Isolations)
	}
	if got := c.DefaultIsolation(); got != IsolationShared {
		t.Errorf("DefaultIsolation = %q, want shared", got)
	}
}

func TestLoadIsolationConfig(t *testing.T) {
	// A config can widen the allowed set and set a default, wired through exactly
	// like networks.
	path := writeConfig(t, `{
	  "images": { "allow": ["wisp-base"] },
	  "limits": { "isolations": ["shared", "sandboxed", "vm"], "default_isolation": "sandboxed" }
	}`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.AllowsIsolation(IsolationShared) || !c.AllowsIsolation(IsolationSandboxed) || !c.AllowsIsolation(IsolationVM) {
		t.Errorf("allowed isolations = %v, want shared+sandboxed+vm", c.Limits.Isolations)
	}
	if got := c.DefaultIsolation(); got != IsolationSandboxed {
		t.Errorf("DefaultIsolation = %q, want sandboxed", got)
	}
}

func TestLoadIsolationDefaultsWhenOmitted(t *testing.T) {
	// A config that does not mention isolation falls back to shared-only, defaulting
	// to shared, so it preserves today's behavior.
	path := writeConfig(t, `{"images": {"allow": ["wisp-base"]}}`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Limits.Isolations) != 1 || c.Limits.Isolations[0] != string(IsolationShared) {
		t.Errorf("isolations = %v, want [shared] fallback", c.Limits.Isolations)
	}
	if got := c.DefaultIsolation(); got != IsolationShared {
		t.Errorf("DefaultIsolation = %q, want shared", got)
	}
}

func TestLoadRejectsBadIsolation(t *testing.T) {
	path := writeConfig(t, `{"images": {"allow": ["a"]}, "limits": {"isolations": ["sideways"]}}`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load with invalid isolation = nil, want error")
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wisp.config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadFile(t *testing.T) {
	path := writeConfig(t, `{
	  "images": { "allow": ["wisp-base", "custom"], "default": "custom" },
	  "limits": { "max_ttl_seconds": 600, "max_cpus": 1.5, "max_memory_mb": 512, "pids_limit": 128, "networks": ["none", "open", "egress"] }
	}`)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.AllowsImage("custom") || !c.AllowsImage("wisp-base") {
		t.Errorf("allow-list = %v, want both images", c.Allow)
	}
	if c.AllowsImage("nope") {
		t.Errorf("allow-list should not include nope")
	}
	if c.DefaultImage != "custom" {
		t.Errorf("DefaultImage = %q, want custom", c.DefaultImage)
	}
	if c.Limits.MaxTTLSeconds != 600 || c.Limits.MaxCPUs != 1.5 || c.Limits.MaxMemoryMB != 512 || c.Limits.PidsLimit != 128 {
		t.Errorf("limits = %+v, want the file values", c.Limits)
	}
	if !c.AllowsNetwork(NetworkEgress) {
		t.Errorf("egress should be allowed by this file")
	}
}

func TestLoadDefaultsOmittedFields(t *testing.T) {
	// Only the allow-list is specified: default image and networks fall back.
	path := writeConfig(t, `{"images": {"allow": ["only-image"]}}`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DefaultImage != "only-image" {
		t.Errorf("DefaultImage = %q, want only-image (first allowed)", c.DefaultImage)
	}
	if !c.AllowsNetwork(NetworkNone) || !c.AllowsNetwork(NetworkOpen) {
		t.Errorf("networks = %v, want fallback none+open", c.Limits.Networks)
	}
}

func TestLoadRejectsDefaultNotAllowed(t *testing.T) {
	path := writeConfig(t, `{"images": {"allow": ["a"], "default": "b"}}`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load with default not in allow-list = nil, want error")
	}
}

func TestLoadRejectsEmptyAllow(t *testing.T) {
	path := writeConfig(t, `{"images": {"allow": []}}`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load with empty allow-list = nil, want error")
	}
}

func TestLoadRejectsBadNetwork(t *testing.T) {
	path := writeConfig(t, `{"images": {"allow": ["a"]}, "limits": {"networks": ["sideways"]}}`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load with invalid network = nil, want error")
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, `{"images": {"allow": ["a"]}, "presets": {}}`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load with unknown field = nil, want error")
	}
}

func TestLoadCapacityBudgets(t *testing.T) {
	// The host capacity budgets parse from the limits block.
	path := writeConfig(t, `{
	  "images": { "allow": ["wisp-base"] },
	  "limits": { "max_contracts": 10, "total_cpus": 32, "total_memory_mb": 65536, "max_cpus": 4, "max_memory_mb": 4096 }
	}`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Limits.MaxContracts != 10 || c.Limits.TotalCPUs != 32 || c.Limits.TotalMemoryMB != 65536 {
		t.Errorf("capacity budgets = {%d, %v, %d}, want {10, 32, 65536}",
			c.Limits.MaxContracts, c.Limits.TotalCPUs, c.Limits.TotalMemoryMB)
	}
}

func TestLoadCapacityDefaultsUnlimited(t *testing.T) {
	// Omitted budgets are 0/unlimited, matching the zero-means-uncapped convention.
	path := writeConfig(t, `{"images": {"allow": ["wisp-base"]}}`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Limits.MaxContracts != 0 || c.Limits.TotalCPUs != 0 || c.Limits.TotalMemoryMB != 0 {
		t.Errorf("capacity budgets = {%d, %v, %d}, want all 0/unlimited",
			c.Limits.MaxContracts, c.Limits.TotalCPUs, c.Limits.TotalMemoryMB)
	}
}

func TestLoadRejectsNegativeCapacityBudgets(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"max_contracts", `{"images": {"allow": ["a"]}, "limits": {"max_contracts": -1}}`},
		{"total_cpus", `{"images": {"allow": ["a"]}, "limits": {"total_cpus": -0.5}}`},
		{"total_memory_mb", `{"images": {"allow": ["a"]}, "limits": {"total_memory_mb": -1}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.body)
			if _, err := Load(path); err == nil {
				t.Fatalf("Load with negative %s = nil, want error", tc.name)
			}
		})
	}
}

func TestLoadRejectsTotalBelowPerLease(t *testing.T) {
	// A total budget below its matching per-lease max could never admit one lease.
	cpuPath := writeConfig(t, `{"images": {"allow": ["a"]}, "limits": {"max_cpus": 4, "total_cpus": 2}}`)
	if _, err := Load(cpuPath); err == nil {
		t.Fatal("Load with total_cpus < max_cpus = nil, want error")
	}
	memPath := writeConfig(t, `{"images": {"allow": ["a"]}, "limits": {"max_memory_mb": 4096, "total_memory_mb": 2048}}`)
	if _, err := Load(memPath); err == nil {
		t.Fatal("Load with total_memory_mb < max_memory_mb = nil, want error")
	}
}

func TestLoadAllowsTotalEqualOrAbovePerLease(t *testing.T) {
	// total == per-lease admits exactly one lease; total > per-lease admits more.
	// Both are valid. A total set with no matching per-lease max is also fine.
	for _, body := range []string{
		`{"images": {"allow": ["a"]}, "limits": {"max_cpus": 4, "total_cpus": 4, "max_memory_mb": 4096, "total_memory_mb": 4096}}`,
		`{"images": {"allow": ["a"]}, "limits": {"max_cpus": 4, "total_cpus": 8}}`,
		`{"images": {"allow": ["a"]}, "limits": {"total_cpus": 2, "total_memory_mb": 1024}}`,
	} {
		if _, err := Load(writeConfig(t, body)); err != nil {
			t.Errorf("Load(%s) = %v, want nil", body, err)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("Load of missing file = nil, want error")
	}
}
