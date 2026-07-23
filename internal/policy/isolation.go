package policy

import (
	"fmt"
	"strings"
)

// Isolation is an ordered isolation level: the strength of the boundary between
// a leased container and the host (and other leases). Levels form a total order
// by rank — shared < sandboxed < vm — so a policy can reason about "at least
// this strong" and a later task can pick the container runtime that satisfies a
// level. It is modeled on how the network policy works (a gated, operator-owned
// dimension of a lease).
//
// This is spec + policy only: a level is parsed, validated, gated by the
// operator allow-list, and recorded on the contract, but nothing here changes
// how a container is launched yet — a later task maps a level to a Docker
// runtime, so today every accepted level still launches under the default OCI
// runtime (runc), i.e. shared.
type Isolation string

const (
	// IsolationShared runs the container under the shared host kernel via the
	// default OCI runtime (runc). It is the safe baseline and preserves today's
	// behavior exactly.
	IsolationShared Isolation = "shared"

	// IsolationSandboxed runs the container under a stronger user-space /
	// syscall-intercepting runtime (e.g. gVisor) that isolates it from the host
	// kernel without a full virtual machine.
	IsolationSandboxed Isolation = "sandboxed"

	// IsolationVM runs the container inside a lightweight virtual machine (e.g.
	// Kata Containers) for hardware-backed isolation from the host kernel.
	IsolationVM Isolation = "vm"

	// IsolationConfidential is a known but not-yet-supported level above vm:
	// hardware confidential-computing isolation (e.g. encrypted-memory VMs). It
	// PARSES so the spec stays forward-compatible and the ordering is complete,
	// but Validate rejects it as not supported yet until a later task adds
	// runtime support.
	IsolationConfidential Isolation = "confidential"
)

// isolationRanks gives each known level its position in the total order; a
// higher rank is a stronger boundary. confidential ranks above vm so the order
// stays meaningful even though Validate does not yet accept it.
var isolationRanks = map[Isolation]int{
	IsolationShared:       0,
	IsolationSandboxed:    1,
	IsolationVM:           2,
	IsolationConfidential: 3,
}

// ParseIsolation parses a level string case-insensitively (leading/trailing
// space trimmed) into an Isolation. It recognizes every known level — including
// confidential, which Validate later rejects as not-yet-supported — so the spec
// is forward-compatible; any other string is an error. An empty string is NOT
// defaulted here: callers apply the policy default before parsing.
func ParseIsolation(s string) (Isolation, error) {
	lvl := Isolation(strings.ToLower(strings.TrimSpace(s)))
	if _, ok := isolationRanks[lvl]; !ok {
		return "", fmt.Errorf("unknown isolation level %q (want shared, sandboxed, or vm)", s)
	}
	return lvl, nil
}

// String returns the level's canonical lowercase name.
func (i Isolation) String() string { return string(i) }

// Rank returns the level's position in the total order (higher is stronger). An
// unknown level ranks -1 so it sorts below every recognized level.
func (i Isolation) Rank() int {
	if r, ok := isolationRanks[i]; ok {
		return r
	}
	return -1
}

// Validate reports whether the level is one Wisp can launch today: a known level
// at or below vm is valid; confidential parses but is rejected as not yet
// supported; anything else is unknown. It does NOT consult the operator's
// allow-list — the create path checks membership separately, mirroring how the
// network policy validates a name and then gates it against Limits.Networks.
func (i Isolation) Validate() error {
	if _, ok := isolationRanks[i]; !ok {
		return fmt.Errorf("unknown isolation level %q (want shared, sandboxed, or vm)", i)
	}
	if i == IsolationConfidential {
		return fmt.Errorf("isolation level %q not supported yet", i)
	}
	return nil
}
