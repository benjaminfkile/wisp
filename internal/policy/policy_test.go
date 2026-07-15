package policy

import (
	"os"
	"path/filepath"
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
	// No caps by default.
	if got := c.ClampTTL(100 * time.Hour); got != 100*time.Hour {
		t.Errorf("ClampTTL with no cap = %v, want unchanged", got)
	}
	if got := c.ClampCPUs(64); got != 64 {
		t.Errorf("ClampCPUs with no cap = %v, want unchanged", got)
	}
	if got := c.ClampMemoryMB(1 << 20); got != 1<<20 {
		t.Errorf("ClampMemoryMB with no cap = %v, want unchanged", got)
	}
	if got := c.ClampPids(9999); got != 9999 {
		t.Errorf("ClampPids with no cap = %v, want unchanged", got)
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

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("Load of missing file = nil, want error")
	}
}
