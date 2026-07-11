// Package config loads Wisp daemon configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Default listen address. Wisp binds loopback by default (see docs/DESIGN.md §8):
// the OS user boundary is the outer defense; tokens gate the cross-app surface.
const defaultAddr = "127.0.0.1:8080"

// Config holds runtime configuration for the daemon.
type Config struct {
	// Addr is the TCP address the HTTP server listens on, e.g. "127.0.0.1:8080".
	Addr string
}

// Load reads configuration from the process environment, applying defaults.
//
// Supported variables:
//
//	WISP_ADDR  full listen address (host:port). Takes precedence when set.
//	WISP_PORT  port only; combined with 127.0.0.1 when WISP_ADDR is unset.
func Load() (Config, error) {
	cfg := Config{Addr: defaultAddr}

	if addr := os.Getenv("WISP_ADDR"); addr != "" {
		cfg.Addr = addr
		return cfg, nil
	}

	if port := os.Getenv("WISP_PORT"); port != "" {
		if _, err := strconv.Atoi(port); err != nil {
			return Config{}, fmt.Errorf("invalid WISP_PORT %q: %w", port, err)
		}
		cfg.Addr = "127.0.0.1:" + port
	}

	return cfg, nil
}
