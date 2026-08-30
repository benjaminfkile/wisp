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
// ReapInterval and warns contracts ExpiringLead before their TTL. ReleaseGrace
// bounds how long a contract may sit in StateReleasing before the reaper stops
// skipping it and expires the contract like any other non-terminal one.
// KillTimeout bounds a single reaper Kill call so a hung Docker daemon cannot
// stall the sweep; the reaper runs the bounded Kill off the tick, so on
// timeout the contract is left for a later attempt while the tick proceeds.
const (
	defaultReapInterval = time.Second
	defaultExpiringLead = time.Minute
	defaultReleaseGrace = 30 * time.Second
	defaultKillTimeout  = 30 * time.Second
)

// defaultMaxFileReadBytes is the default download cap for GET
// /contracts/{id}/files: 16 MiB. WISP_MAX_FILE_READ_BYTES overrides it and
// must be a positive integer, matching the other WISP_* numeric env vars.
const defaultMaxFileReadBytes int64 = 16 * 1024 * 1024

// Config holds runtime configuration for the daemon.
type Config struct {
	// Addr is the TCP address the HTTP server listens on, e.g. "127.0.0.1:8080".
	Addr string

	// ReapInterval is how often the TTL reaper scans tracked contracts.
	ReapInterval time.Duration

	// ExpiringLead is how long before a contract's TTL it is moved to the
	// expiring warning state, giving the client time to exfiltrate work.
	ExpiringLead time.Duration

	// ReleaseGrace is how long a contract may sit in StateReleasing before the
	// reaper stops skipping it. Inside the window the DELETE handler owns the
	// tear down; past it the release is presumed stuck (the handler died
	// mid-release, or its request was cancelled before the final mark-released
	// transition) and the reaper expires the contract like any other
	// non-terminal one so its capacity and GPUs return to the allocators.
	ReleaseGrace time.Duration

	// KillTimeout bounds a single reaper Kill call so a hung Docker daemon
	// cannot stall the sweep. The reaper launches the bounded Kill off the tick,
	// so on timeout the contract is left in place for a later kill attempt
	// while the tick proceeds with the remaining contracts.
	KillTimeout time.Duration

	// MaxFileReadBytes caps the size of a single file GET /contracts/{id}/files
	// will stream back. A file larger than this is rejected with 413 and a
	// clear file_too_large error body. Defaults to defaultMaxFileReadBytes
	// (16 MiB); overridden by WISP_MAX_FILE_READ_BYTES.
	MaxFileReadBytes int64

	// ImageConfigFile is an optional path to a JSON policy config file defining
	// the image allow-list, default image, and limits (see docs/DESIGN.md §7).
	// Empty means the built-in defaults (allow-list of just the bare base image,
	// no resource/TTL caps).
	ImageConfigFile string

	// AppToken is the app-level bearer credential gating contract creation (and
	// the event bus). When empty, the app-level gate is disabled: any caller may
	// create contracts - the localhost-friendly default, since the OS user
	// boundary is the outer defense (see docs/DESIGN.md §8). Set it to require an
	// Authorization: Bearer <token> on POST /contracts.
	AppToken string
}

// Load reads configuration from the process environment, applying defaults.
//
// Supported variables:
//
//	WISP_ADDR                  full listen address (host:port). Takes precedence.
//	WISP_PORT                  port only; combined with 127.0.0.1 when WISP_ADDR is unset.
//	WISP_REAP_INTERVAL_SECONDS  reaper scan interval in seconds (positive integer).
//	WISP_EXPIRING_LEAD_SECONDS  expiring-warning lead time in seconds (positive integer).
//	WISP_RELEASE_GRACE_SECONDS  release-grace window in seconds (positive integer); how
//	                            long a contract may sit in StateReleasing before the
//	                            reaper expires it. Defaults to 30.
//	WISP_KILL_TIMEOUT_SECONDS   reaper Kill timeout in seconds (positive integer); a
//	                            hung Docker daemon on a single Kill cannot stall the
//	                            sweep past this. Defaults to 30.
//	WISP_MAX_FILE_READ_BYTES    per-file download cap for GET /contracts/{id}/files
//	                            in bytes (positive integer). Defaults to 16777216
//	                            (16 MiB); a file larger than this is rejected with
//	                            413 file_too_large.
//	WISP_CONFIG                 path to a JSON image allow-list + limits config (optional).
//	WISP_APP_TOKEN              app-level bearer token gating contract creation (optional).
func Load() (Config, error) {
	cfg := Config{
		Addr:             defaultAddr,
		ReapInterval:     defaultReapInterval,
		ExpiringLead:     defaultExpiringLead,
		ReleaseGrace:     defaultReleaseGrace,
		KillTimeout:      defaultKillTimeout,
		MaxFileReadBytes: defaultMaxFileReadBytes,
		ImageConfigFile:  os.Getenv("WISP_CONFIG"),
		AppToken:         os.Getenv("WISP_APP_TOKEN"),
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
	if cfg.ReleaseGrace, err = positiveSecondsEnv("WISP_RELEASE_GRACE_SECONDS", defaultReleaseGrace); err != nil {
		return Config{}, err
	}
	if cfg.KillTimeout, err = positiveSecondsEnv("WISP_KILL_TIMEOUT_SECONDS", defaultKillTimeout); err != nil {
		return Config{}, err
	}
	if cfg.MaxFileReadBytes, err = positiveInt64Env("WISP_MAX_FILE_READ_BYTES", defaultMaxFileReadBytes); err != nil {
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

// positiveInt64Env reads name as a positive 64-bit integer (used for byte-count
// env vars like WISP_MAX_FILE_READ_BYTES so a caller can raise the download
// cap past the 32-bit int range). Returns def when the variable is unset and
// an error when it is not a positive integer.
func positiveInt64Env(name string, def int64) (int64, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid %s %q: must be a positive integer", name, v)
	}
	return n, nil
}
