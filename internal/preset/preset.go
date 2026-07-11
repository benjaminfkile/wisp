// Package preset defines named launch-configuration bundles (see
// docs/DESIGN.md §7). A preset is a bundle of the knobs used to boot a
// contract's container — base image, resource limits, network policy, the
// maximum TTL allowed, and the session flags the Claude session starts with —
// referenced by name at contract-creation time so clients never pass raw Docker
// knobs.
//
// Presets are the cap on a contract: the base image comes from the preset and a
// requested TTL is clamped down to the preset's maximum. Wisp ships built-in
// presets (including a bare "default") and can overlay operator-supplied presets
// loaded from a config file.
package preset

import "time"

// DefaultName is the preset used when a contract is created without naming one.
const DefaultName = "default"

// NetworkPolicy names a container's network exposure (see docs/DESIGN.md §7).
type NetworkPolicy string

const (
	// NetworkNone disconnects the container from all networks.
	NetworkNone NetworkPolicy = "none"

	// NetworkEgress allows outbound traffic only. Wisp does not yet enforce a
	// distinct egress-only mode, so it currently boots on the default network;
	// the policy is recorded so a later task can tighten it.
	NetworkEgress NetworkPolicy = "egress"

	// NetworkOpen places the container on the runtime's default network.
	NetworkOpen NetworkPolicy = "open"
)

// Preset is a named bundle of launch configuration. The zero value is not a
// valid preset; construct presets through Builtin or Load.
type Preset struct {
	// Name is the identifier clients reference at contract creation.
	Name string

	// Image is the base image the container boots from. It may be the bare
	// wisp base or a client pre-baked image FROM wisp-base (the cold-start
	// escape hatch, see docs/DESIGN.md §7): that image is just configuration,
	// Wisp never knows what is inside it.
	Image string

	// MaxTTL is the longest lease this preset permits. A requested TTL longer
	// than MaxTTL is clamped down to it (see ClampTTL). A non-positive MaxTTL
	// imposes no ceiling.
	MaxTTL time.Duration

	// CPUs caps the container's CPU as a fraction of host cores (e.g. 1.5).
	// Zero means unlimited.
	CPUs float64

	// MemoryMB caps the container's memory in mebibytes. Zero means unlimited.
	MemoryMB int

	// PidsLimit caps the number of processes in the container. Zero means
	// unlimited.
	PidsLimit int

	// Network selects the container's network exposure.
	Network NetworkPolicy

	// SessionFlags are the flags the Claude session starts with, e.g.
	// "--dangerously-skip-permissions". Opaque to Wisp; surfaced to whatever
	// drives the session inside the container.
	SessionFlags []string
}

// ClampTTL returns requested capped at the preset's MaxTTL. A preset with a
// non-positive MaxTTL imposes no ceiling and returns requested unchanged.
func (p Preset) ClampTTL(requested time.Duration) time.Duration {
	if p.MaxTTL > 0 && requested > p.MaxTTL {
		return p.MaxTTL
	}
	return requested
}
