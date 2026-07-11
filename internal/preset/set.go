package preset

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"
)

// baseImage is the bare, domain-blind base image (see docs/DESIGN.md §7): the
// generic default every built-in preset boots from unless an operator points a
// preset at a pre-baked image.
const baseImage = "wisp-base"

// ErrUnknownPreset is returned by Set.Lookup when no preset matches the
// requested name.
var ErrUnknownPreset = errors.New("preset: unknown preset")

// Set is an immutable, name-keyed collection of presets. Construct it with
// Builtin or Load; it is safe for concurrent reads.
type Set struct {
	presets map[string]Preset
}

// builtins returns the built-in presets keyed by name. It always includes a
// bare "default" and the example presets named in docs/DESIGN.md §7 (a
// read-write "coding" preset and a locked-down "probe" preset).
func builtins() map[string]Preset {
	return map[string]Preset{
		DefaultName: {
			Name:      DefaultName,
			Image:     baseImage,
			MaxTTL:    time.Hour,
			CPUs:      1,
			MemoryMB:  1024,
			PidsLimit: 256,
			Network:   NetworkNone,
		},
		"coding": {
			Name:         "coding",
			Image:        baseImage,
			MaxTTL:       4 * time.Hour,
			CPUs:         2,
			MemoryMB:     4096,
			PidsLimit:    1024,
			Network:      NetworkOpen,
			SessionFlags: []string{"--dangerously-skip-permissions"},
		},
		"probe": {
			Name:      "probe",
			Image:     baseImage,
			MaxTTL:    15 * time.Minute,
			CPUs:      0.5,
			MemoryMB:  512,
			PidsLimit: 128,
			Network:   NetworkNone,
		},
	}
}

// Builtin returns a Set of the built-in presets.
func Builtin() *Set {
	return &Set{presets: builtins()}
}

// Lookup returns the preset registered under name. It returns ErrUnknownPreset
// when no preset matches, so callers can map the miss to a 400.
func (s *Set) Lookup(name string) (Preset, error) {
	p, ok := s.presets[name]
	if !ok {
		return Preset{}, fmt.Errorf("%w: %q", ErrUnknownPreset, name)
	}
	return p, nil
}

// Names returns the registered preset names in sorted order.
func (s *Set) Names() []string {
	out := make([]string, 0, len(s.presets))
	for name := range s.presets {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// fileSet is the on-disk representation of a presets config file. Each entry is
// keyed by name; fields left unset fall back to sensible defaults when the
// preset is materialized. Durations are expressed as whole seconds.
type fileSet struct {
	Presets map[string]filePreset `json:"presets"`
}

// filePreset is one preset as written in the config file. Pointer-free scalars
// keep the format terse; an omitted numeric field means "unset" (unlimited for
// limits, no ceiling for TTL).
type filePreset struct {
	Image         string   `json:"image"`
	MaxTTLSeconds int      `json:"max_ttl_seconds"`
	CPUs          float64  `json:"cpus"`
	MemoryMB      int      `json:"memory_mb"`
	PidsLimit     int      `json:"pids"`
	Network       string   `json:"network"`
	SessionFlags  []string `json:"session_flags"`
}

// Load returns a Set seeded with the built-in presets and overlaid with any
// presets defined in the file at path. A preset in the file replaces a built-in
// of the same name, letting operators retune "default" or add their own. An
// empty path skips the file and returns the built-ins unchanged.
func Load(path string) (*Set, error) {
	presets := builtins()
	if path == "" {
		return &Set{presets: presets}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("preset: read config %s: %w", path, err)
	}

	var fs fileSet
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fs); err != nil {
		return nil, fmt.Errorf("preset: parse config %s: %w", path, err)
	}

	for name, fp := range fs.Presets {
		p, err := fp.materialize(name)
		if err != nil {
			return nil, fmt.Errorf("preset: config %s: %w", path, err)
		}
		presets[name] = p
	}
	return &Set{presets: presets}, nil
}

// materialize converts a file entry into a Preset, applying defaults for
// omitted fields and validating the network policy.
func (fp filePreset) materialize(name string) (Preset, error) {
	if name == "" {
		return Preset{}, errors.New("preset name must not be empty")
	}
	image := fp.Image
	if image == "" {
		image = baseImage
	}
	network := NetworkPolicy(fp.Network)
	if fp.Network == "" {
		network = NetworkNone
	}
	switch network {
	case NetworkNone, NetworkEgress, NetworkOpen:
	default:
		return Preset{}, fmt.Errorf("preset %q: invalid network %q", name, fp.Network)
	}
	return Preset{
		Name:         name,
		Image:        image,
		MaxTTL:       time.Duration(fp.MaxTTLSeconds) * time.Second,
		CPUs:         fp.CPUs,
		MemoryMB:     fp.MemoryMB,
		PidsLimit:    fp.PidsLimit,
		Network:      network,
		SessionFlags: fp.SessionFlags,
	}, nil
}
