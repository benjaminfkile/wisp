// Package policy holds the operator's data-driven launch policy for Wisp
// contracts (see docs/DESIGN.md §7): an image allow-list, a default image, and
// optional TTL / resource / network limits.
//
// Wisp is domain-blind — it knows nothing about what runs inside a container
// (docs/DESIGN.md §2, §3). The operator owns the allow-list and the caps; the
// client picks an allowed image and shapes the network / resources on the create
// request; userdata owns the container's contents. There are no opinionated,
// tool-aware presets here.
//
// A Config is loaded from a JSON file (path from WISP_CONFIG) or falls back to
// safe built-in defaults. It exposes the allow-list, the default image, and the
// limits, plus helpers to check whether an image or network is allowed and to
// clamp a requested TTL / CPUs / memory / pids down to any configured maximum.
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// baseImage is the bare, domain-blind Linux base image (see docs/DESIGN.md §7):
// the generic default the built-in policy allows and boots from on a Linux
// daemon.
const baseImage = "wisp-base"

// windowsBaseImage is the bare Windows base image: the OS-appropriate default the
// built-in policy boots from on a windows-mode daemon (see DefaultImageFor). A
// Docker daemon is fixed in one container OS mode, so only one of the two base
// images is ever launchable on a given host; the allow-list may carry both so
// the same policy serves either OS, and the current OS picks the default.
const windowsBaseImage = "wisp-base-windows"

// osWindows is the container OS mode string a windows-mode daemon reports (it
// mirrors runtime.OSWindows). Policy stays free of a runtime import by comparing
// the mode as a plain string; callers pass the daemon's detected OS through.
const osWindows = "windows"

// Network policy names. A container's network is one of these; the operator's
// limits.networks list gates which a client may request at create time.
const (
	// NetworkNone disconnects the container from all networks.
	NetworkNone = "none"

	// NetworkEgress allows outbound traffic only. A lease requesting it is
	// attached to a dedicated wisp-managed bridge with inter-container
	// communication disabled, so it reaches the internet but not other leases or
	// host services on the default bridge (see runtime.EgressNetworkName).
	NetworkEgress = "egress"

	// NetworkOpen places the container on the runtime's default network.
	NetworkOpen = "open"
)

// validNetworks is the set of network names a config may reference.
var validNetworks = map[string]bool{
	NetworkNone:   true,
	NetworkEgress: true,
	NetworkOpen:   true,
}

// Limits caps a contract's TTL and resources and gates the networks a client
// may request. A zero/empty numeric limit imposes no cap on that dimension.
type Limits struct {
	// MaxTTLSeconds caps a contract's lease in seconds. Zero means no ceiling.
	MaxTTLSeconds int

	// MaxCPUs caps CPU as a fraction of host cores (e.g. 1.5). Zero means no cap.
	MaxCPUs float64

	// MaxMemoryMB caps memory in mebibytes. Zero means no cap.
	MaxMemoryMB int

	// PidsLimit caps the number of processes. Zero means no cap.
	PidsLimit int

	// Networks is the allow-list of network names a client may request.
	Networks []string
}

// Config is the operator's launch policy: an image allow-list, a default image,
// and limits. Construct it with Default or Load; treat it as read-only after
// construction (it is safe for concurrent reads).
type Config struct {
	// Allow is the non-empty list of images a client may request.
	Allow []string

	// DefaultImage is the image used when a create request omits one. Always a
	// member of Allow.
	DefaultImage string

	// Limits caps TTL / resources and gates networks.
	Limits Limits
}

// Conservative built-in resource / TTL ceilings applied by Default when
// WISP_CONFIG is unset. They are DEFAULTS, not hard limits: an operator config
// (loaded via Load) can raise or lower any of them. Their purpose is to ensure a
// lease created against the built-in defaults — including one requesting
// resources:{} — inherits a bounded container rather than an uncapped one that
// could exhaust host CPU / RAM / PIDs or run forever.
const (
	// defaultMaxTTLSeconds caps a default lease at one hour.
	defaultMaxTTLSeconds = 3600
	// defaultMaxCPUs caps a default lease at four host cores.
	defaultMaxCPUs = 4
	// defaultMaxMemoryMB caps a default lease at 4 GiB of memory.
	defaultMaxMemoryMB = 4096
	// defaultPidsLimit caps a default lease at 512 processes.
	defaultPidsLimit = 512
)

// Default returns the built-in policy used when WISP_CONFIG is unset: the bare
// Linux and Windows base images are both allowed so the same default serves
// either daemon OS, the Linux base is the configured default image (the current
// OS overrides it for a windows daemon via DefaultImageFor), "none" and "open"
// are the permitted networks, and conservative non-zero TTL / CPU / memory / PID
// ceilings are imposed so a default lease is always bounded (an operator
// WISP_CONFIG can raise or lower them).
func Default() *Config {
	return &Config{
		Allow:        []string{baseImage, windowsBaseImage},
		DefaultImage: baseImage,
		Limits: Limits{
			MaxTTLSeconds: defaultMaxTTLSeconds,
			MaxCPUs:       defaultMaxCPUs,
			MaxMemoryMB:   defaultMaxMemoryMB,
			PidsLimit:     defaultPidsLimit,
			Networks:      []string{NetworkNone, NetworkOpen},
		},
	}
}

// DefaultImageFor returns the image a create should boot when it omits one,
// chosen for the daemon's container OS (as reported by runtime.ContainerOS and
// passed through as a plain "linux"/"windows" string). On a windows-mode host it
// prefers the Windows base image when the allow-list carries it; otherwise (and
// always on Linux) it returns the configured DefaultImage. The result is always
// a member of the allow-list: a windows host without the Windows base image
// allow-listed still falls back to DefaultImage. Wisp never switches the daemon's
// mode — it only picks an OS-appropriate default to advertise and boot.
func (c *Config) DefaultImageFor(os string) string {
	if os == osWindows && c.AllowsImage(windowsBaseImage) {
		return windowsBaseImage
	}
	return c.DefaultImage
}

// AllowsImage reports whether image is in the allow-list.
func (c *Config) AllowsImage(image string) bool {
	for _, a := range c.Allow {
		if a == image {
			return true
		}
	}
	return false
}

// AllowsNetwork reports whether network is one the operator permits.
func (c *Config) AllowsNetwork(network string) bool {
	for _, n := range c.Limits.Networks {
		if n == network {
			return true
		}
	}
	return false
}

// DefaultNetwork is the network used when a create request omits one: "open"
// when allowed, otherwise the first configured network.
func (c *Config) DefaultNetwork() string {
	if c.AllowsNetwork(NetworkOpen) {
		return NetworkOpen
	}
	if len(c.Limits.Networks) > 0 {
		return c.Limits.Networks[0]
	}
	return ""
}

// ClampTTL caps requested at the configured maximum TTL. A zero max imposes no
// ceiling and returns requested unchanged.
func (c *Config) ClampTTL(requested time.Duration) time.Duration {
	if c.Limits.MaxTTLSeconds > 0 {
		if max := time.Duration(c.Limits.MaxTTLSeconds) * time.Second; requested > max {
			return max
		}
	}
	return requested
}

// ClampCPUs caps requested at the configured maximum. A zero max imposes no cap
// and returns requested unchanged. When a max is set, an unset request (<= 0)
// inherits the maximum so a lease that omits a CPU request is bounded by the
// ceiling instead of running uncapped.
func (c *Config) ClampCPUs(requested float64) float64 {
	if c.Limits.MaxCPUs > 0 && (requested <= 0 || requested > c.Limits.MaxCPUs) {
		return c.Limits.MaxCPUs
	}
	return requested
}

// ClampMemoryMB caps requested (mebibytes) at the configured maximum. A zero max
// imposes no cap and returns requested unchanged. When a max is set, an unset
// request (<= 0) inherits the maximum so a lease that omits a memory request is
// bounded by the ceiling instead of running uncapped.
func (c *Config) ClampMemoryMB(requested int) int {
	if c.Limits.MaxMemoryMB > 0 && (requested <= 0 || requested > c.Limits.MaxMemoryMB) {
		return c.Limits.MaxMemoryMB
	}
	return requested
}

// ClampPids caps requested at the configured maximum. A zero max imposes no cap
// and returns requested unchanged. When a max is set, an unset request (<= 0)
// inherits the maximum so a lease that omits a PID request is bounded by the
// ceiling instead of running uncapped.
func (c *Config) ClampPids(requested int) int {
	if c.Limits.PidsLimit > 0 && (requested <= 0 || requested > c.Limits.PidsLimit) {
		return c.Limits.PidsLimit
	}
	return requested
}

// fileConfig is the on-disk JSON representation of a policy config (see
// docs/DESIGN.md §7 and examples/wisp.config.json).
type fileConfig struct {
	Images fileImages `json:"images"`
	Limits fileLimits `json:"limits"`
}

type fileImages struct {
	Allow   []string `json:"allow"`
	Default string   `json:"default"`
}

type fileLimits struct {
	MaxTTLSeconds int      `json:"max_ttl_seconds"`
	MaxCPUs       float64  `json:"max_cpus"`
	MaxMemoryMB   int      `json:"max_memory_mb"`
	PidsLimit     int      `json:"pids_limit"`
	Networks      []string `json:"networks"`
}

// Load returns the policy at path, or the built-in Default when path is empty.
// The file is parsed strictly (unknown fields are rejected) and validated: the
// allow-list must be non-empty, the default image must be in it (an omitted
// default falls back to the first allowed image), and every network must be one
// of "none", "open", or "egress" (an omitted networks list falls back to the
// default set).
func Load(path string) (*Config, error) {
	if path == "" {
		return Default(), nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("policy: read config %s: %w", path, err)
	}

	var fc fileConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fc); err != nil {
		return nil, fmt.Errorf("policy: parse config %s: %w", path, err)
	}

	cfg := &Config{
		Allow:        fc.Images.Allow,
		DefaultImage: fc.Images.Default,
		Limits: Limits{
			MaxTTLSeconds: fc.Limits.MaxTTLSeconds,
			MaxCPUs:       fc.Limits.MaxCPUs,
			MaxMemoryMB:   fc.Limits.MaxMemoryMB,
			PidsLimit:     fc.Limits.PidsLimit,
			Networks:      fc.Limits.Networks,
		},
	}
	// An omitted networks list falls back to the default set so a config that
	// only tunes the allow-list still permits the usual networks.
	if len(cfg.Limits.Networks) == 0 {
		cfg.Limits.Networks = []string{NetworkNone, NetworkOpen}
	}
	// An omitted default image falls back to the first allowed image.
	if cfg.DefaultImage == "" && len(cfg.Allow) > 0 {
		cfg.DefaultImage = cfg.Allow[0]
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("policy: config %s: %w", path, err)
	}
	return cfg, nil
}

// validate enforces the config invariants: a non-empty allow-list, a default
// image within it, and only recognized network names.
func (c *Config) validate() error {
	if len(c.Allow) == 0 {
		return errors.New("images.allow must not be empty")
	}
	if !c.AllowsImage(c.DefaultImage) {
		return fmt.Errorf("images.default %q is not in the allow-list", c.DefaultImage)
	}
	for _, n := range c.Limits.Networks {
		if !validNetworks[n] {
			return fmt.Errorf("invalid network %q (want none, open, or egress)", n)
		}
	}
	return nil
}
