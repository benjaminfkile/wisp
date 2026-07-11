package config

import (
	"testing"
	"time"
)

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

func TestLoadReaperDefaults(t *testing.T) {
	t.Setenv("WISP_REAP_INTERVAL_SECONDS", "")
	t.Setenv("WISP_EXPIRING_LEAD_SECONDS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ReapInterval != defaultReapInterval {
		t.Errorf("ReapInterval = %v, want %v", cfg.ReapInterval, defaultReapInterval)
	}
	if cfg.ExpiringLead != defaultExpiringLead {
		t.Errorf("ExpiringLead = %v, want %v", cfg.ExpiringLead, defaultExpiringLead)
	}
}

func TestLoadReaperOverride(t *testing.T) {
	t.Setenv("WISP_REAP_INTERVAL_SECONDS", "5")
	t.Setenv("WISP_EXPIRING_LEAD_SECONDS", "120")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ReapInterval != 5*time.Second {
		t.Errorf("ReapInterval = %v, want 5s", cfg.ReapInterval)
	}
	if cfg.ExpiringLead != 120*time.Second {
		t.Errorf("ExpiringLead = %v, want 120s", cfg.ExpiringLead)
	}
}

func TestLoadInvalidReaperEnv(t *testing.T) {
	for _, name := range []string{"WISP_REAP_INTERVAL_SECONDS", "WISP_EXPIRING_LEAD_SECONDS"} {
		for _, val := range []string{"notanint", "0", "-3"} {
			t.Run(name+"="+val, func(t *testing.T) {
				t.Setenv("WISP_REAP_INTERVAL_SECONDS", "")
				t.Setenv("WISP_EXPIRING_LEAD_SECONDS", "")
				t.Setenv(name, val)
				if _, err := Load(); err == nil {
					t.Fatalf("Load() error = nil, want error for %s=%q", name, val)
				}
			})
		}
	}
}
