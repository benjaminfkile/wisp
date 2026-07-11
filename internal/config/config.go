// Package config loads Wisp daemon configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Default listen address. Wisp binds loopback by default (see docs/DESIGN.md §8):
// the OS user boundary is the outer defense; tokens gate the cross-app surface.
const defaultAddr = "127.0.0.1:8080"

// TTL-reaper defaults (see docs/DESIGN.md §4, §9). The reaper scans every
// ReapInterval and warns contracts ExpiringLead before their TTL.
const (
	defaultReapInterval = time.Second
	defaultExpiringLead = time.Minute
)

// Config holds runtime configuration for the daemon.
type Config struct {
	// Addr is the TCP address the HTTP server listens on, e.g. "127.0.0.1:8080".
	Addr string

	// ReapInterval is how often the TTL reaper scans tracked contracts.
	ReapInterval time.Duration

	// ExpiringLead is how long before a contract's TTL it is moved to the
	// expiring warning state, giving the client time to exfiltrate work.
	ExpiringLead time.Duration

	// PresetsFile is an optional path to a JSON presets config file overlaid on
	// the built-in presets (see docs/DESIGN.md §7). Empty means built-ins only.
	PresetsFile string
}

// Load reads configuration from the process environment, applying defaults.
//
// Supported variables:
//
//	WISP_ADDR                  full listen address (host:port). Takes precedence.
//	WISP_PORT                  port only; combined with 127.0.0.1 when WISP_ADDR is unset.
//	WISP_REAP_INTERVAL_SECONDS reaper scan interval in seconds (positive integer).
//	WISP_EXPIRING_LEAD_SECONDS expiring-warning lead time in seconds (positive integer).
//	WISP_PRESETS_FILE          path to a JSON presets config file (optional).
func Load() (Config, error) {
	cfg := Config{
		Addr:         defaultAddr,
		ReapInterval: defaultReapInterval,
		ExpiringLead: defaultExpiringLead,
		PresetsFile:  os.Getenv("WISP_PRESETS_FILE"),
	}

	switch {
	case os.Getenv("WISP_ADDR") != "":
		cfg.Addr = os.Getenv("WISP_ADDR")
	case os.Getenv("WISP_PORT") != "":
		port := os.Getenv("WISP_PORT")
		if _, err := strconv.Atoi(port); err != nil {
			return Config{}, fmt.Errorf("invalid WISP_PORT %q: %w", port, err)
		}
		cfg.Addr = "127.0.0.1:" + port
	}

	var err error
	if cfg.ReapInterval, err = positiveSecondsEnv("WISP_REAP_INTERVAL_SECONDS", defaultReapInterval); err != nil {
		return Config{}, err
	}
	if cfg.ExpiringLead, err = positiveSecondsEnv("WISP_EXPIRING_LEAD_SECONDS", defaultExpiringLead); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// positiveSecondsEnv reads name as a positive integer count of seconds,
// returning def when the variable is unset and an error when it is not a
// positive integer.
func positiveSecondsEnv(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid %s %q: must be a positive integer", name, v)
	}
	return time.Duration(n) * time.Second, nil
}
