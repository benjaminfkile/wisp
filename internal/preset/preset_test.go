package preset

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuiltinHasBareDefault(t *testing.T) {
	set := Builtin()
	p, err := set.Lookup(DefaultName)
	if err != nil {
		t.Fatalf("Lookup(default): %v", err)
	}
	if p.Image != baseImage {
		t.Errorf("default image = %q, want %q", p.Image, baseImage)
	}
	if p.MaxTTL <= 0 {
		t.Errorf("default MaxTTL = %v, want positive", p.MaxTTL)
	}
}

func TestLookupUnknown(t *testing.T) {
	set := Builtin()
	_, err := set.Lookup("nope")
	if !errors.Is(err, ErrUnknownPreset) {
		t.Fatalf("Lookup(nope) err = %v, want ErrUnknownPreset", err)
	}
}

func TestClampTTL(t *testing.T) {
	p := Preset{MaxTTL: time.Hour}
	tests := []struct {
		name      string
		requested time.Duration
		want      time.Duration
	}{
		{"under the cap is unchanged", 30 * time.Minute, 30 * time.Minute},
		{"at the cap is unchanged", time.Hour, time.Hour},
		{"over the cap is clamped", 2 * time.Hour, time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.ClampTTL(tt.requested); got != tt.want {
				t.Errorf("ClampTTL(%v) = %v, want %v", tt.requested, got, tt.want)
			}
		})
	}
}

func TestClampTTLNoCeiling(t *testing.T) {
	// A preset with a non-positive MaxTTL imposes no ceiling.
	p := Preset{MaxTTL: 0}
	if got := p.ClampTTL(5 * time.Hour); got != 5*time.Hour {
		t.Errorf("ClampTTL with no ceiling = %v, want unchanged", got)
	}
}

func TestLoadEmptyPathReturnsBuiltins(t *testing.T) {
	set, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if _, err := set.Lookup(DefaultName); err != nil {
		t.Errorf("built-in default missing: %v", err)
	}
}

func TestLoadOverlaysFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "presets.json")
	body := `{
	  "presets": {
	    "fast": {
	      "image": "custom-image",
	      "max_ttl_seconds": 120,
	      "cpus": 2,
	      "memory_mb": 2048,
	      "pids": 512,
	      "network": "open",
	      "session_flags": ["--dangerously-skip-permissions"]
	    }
	  }
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	set, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The file preset is added...
	fast, err := set.Lookup("fast")
	if err != nil {
		t.Fatalf("Lookup(fast): %v", err)
	}
	if fast.Image != "custom-image" {
		t.Errorf("fast image = %q, want custom-image", fast.Image)
	}
	if fast.MaxTTL != 2*time.Minute {
		t.Errorf("fast MaxTTL = %v, want 2m", fast.MaxTTL)
	}
	if fast.Network != NetworkOpen {
		t.Errorf("fast network = %q, want open", fast.Network)
	}
	if len(fast.SessionFlags) != 1 || fast.SessionFlags[0] != "--dangerously-skip-permissions" {
		t.Errorf("fast session flags = %v", fast.SessionFlags)
	}

	// ...and the built-ins are still present.
	if _, err := set.Lookup(DefaultName); err != nil {
		t.Errorf("built-in default lost after overlay: %v", err)
	}
}

func TestLoadOverridesBuiltin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "presets.json")
	body := `{"presets": {"default": {"max_ttl_seconds": 60}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	set, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, err := set.Lookup(DefaultName)
	if err != nil {
		t.Fatalf("Lookup(default): %v", err)
	}
	if p.MaxTTL != time.Minute {
		t.Errorf("overridden default MaxTTL = %v, want 1m", p.MaxTTL)
	}
	// An omitted image falls back to the bare base image.
	if p.Image != baseImage {
		t.Errorf("overridden default image = %q, want %q", p.Image, baseImage)
	}
}

func TestLoadRejectsBadNetwork(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "presets.json")
	body := `{"presets": {"weird": {"network": "sideways"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load with invalid network = nil, want error")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json")); err == nil {
		t.Fatal("Load of missing file = nil, want error")
	}
}
