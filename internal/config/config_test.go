package config

import "testing"

func TestLoadDefault(t *testing.T) {
	t.Setenv("WISP_ADDR", "")
	t.Setenv("WISP_PORT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Addr != defaultAddr {
		t.Errorf("Addr = %q, want %q", cfg.Addr, defaultAddr)
	}
}

func TestLoadAddrOverride(t *testing.T) {
	t.Setenv("WISP_ADDR", "0.0.0.0:9000")
	t.Setenv("WISP_PORT", "1234")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Addr != "0.0.0.0:9000" {
		t.Errorf("Addr = %q, want WISP_ADDR to win", cfg.Addr)
	}
}

func TestLoadPortOnly(t *testing.T) {
	t.Setenv("WISP_ADDR", "")
	t.Setenv("WISP_PORT", "9999")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Addr != "127.0.0.1:9999" {
		t.Errorf("Addr = %q, want 127.0.0.1:9999", cfg.Addr)
	}
}

func TestLoadInvalidPort(t *testing.T) {
	t.Setenv("WISP_ADDR", "")
	t.Setenv("WISP_PORT", "notaport")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error for invalid WISP_PORT")
	}
}
