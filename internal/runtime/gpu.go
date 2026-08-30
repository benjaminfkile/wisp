package runtime

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// GPU discovery. Wisp advertises GPU capability the same data-driven way it
// advertises isolation capability (see policy.SupportedIsolations): the daemon
// query reports the registered runtimes (DaemonInfo.Runtimes - the "nvidia"
// runtime gates GPU support) and this file enumerates the physical devices via
// nvidia-smi. Both are plain facts the policy layer intersects with the
// operator's config to get the effective posture the read surface advertises
// (see policy.EffectiveGPU and docs/DESIGN.md §7).
//
// nvidia-smi is the only vendor tool wired in v1; it sits behind CommandRunner
// so the enumeration+parse logic is unit-testable with canned output and nothing
// shells out for real in tests - the CI/runner container has no GPU and no NVIDIA
// tooling.

// NVIDIARuntimeName is the OCI runtime name the NVIDIA Container Runtime
// registers with the Docker daemon. Its presence in DaemonInfo.Runtimes is the
// necessary daemon-side signal that this host can attach GPUs to a container;
// enumeration via nvidia-smi is the sufficient hardware-side signal. It mirrors
// the isolation runtime-name constants (gVisorRuntimeName, kataRuntimeName) but
// is consulted by the policy layer as a plain string, exactly like those.
const NVIDIARuntimeName = "nvidia"

// The nvidia-smi invocation used to enumerate GPUs. The machine-readable query
// flags emit one CSV line per device with no header and no unit suffixes, so
// memory.total is a bare integer count of mebibytes - see enumerateGPUs for the
// parse. Keeping the binary and flags as constants documents the exact contract
// the fake stands in for.
const (
	nvidiaSMIBinary   = "nvidia-smi"
	nvidiaSMIQueryGPU = "--query-gpu=uuid,name,memory.total"
	nvidiaSMIFormat   = "--format=csv,noheader,nounits"
)

// GPUDevice is one physical GPU enumerated on the host: its stable UUID, its
// normalized product class, and its total video memory in mebibytes. It is the
// runtime-layer fact the policy layer intersects with the operator's GPU config
// and the read surface advertises (see policy.GPUDevice and the /images "gpu"
// block).
type GPUDevice struct {
	// ID is the GPU's globally-unique identifier as reported by nvidia-smi (e.g.
	// "GPU-1b2c3d4e-..."). It is the handle the allocator pins a lease to.
	ID string

	// Class is the normalized product name: lowercased with whitespace runs
	// collapsed to single hyphens (e.g. "nvidia-geforce-rtx-4090"). It is an
	// opaque string to consumers - a coarse "what kind of GPU" label, not a spec.
	Class string

	// VRAMMB is the device's total video memory in mebibytes.
	VRAMMB int
}

// CommandRunner is the narrow seam behind which GPU enumeration shells out to
// nvidia-smi. It exists so the enumeration+parse logic can be exercised with
// canned output in unit tests without a GPU or NVIDIA tooling present, and so
// nothing runs a real binary during tests. The real implementation (execRunner)
// runs the command; tests supply a fake that returns fixed bytes/errors.
type CommandRunner interface {
	// Output runs name with args and returns its standard output, or an error if
	// the command cannot be run or exits non-zero.
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

// execRunner is the production CommandRunner: it runs the command for real via
// os/exec. A missing nvidia-smi (the common case on a GPU-less host) surfaces as
// an error from Output, which the caller degrades to "no GPU support".
type execRunner struct{}

func (execRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// enumerateGPUs runs nvidia-smi through runner and parses its CSV output into
// the enumerated devices. A runner error (nvidia-smi absent or exiting non-zero)
// or an unparseable line is returned as an error so the caller degrades to
// supported=false; enumeration succeeding with zero devices returns an empty,
// non-nil slice and no error. The parse is deliberately strict - garbage lines
// are an error rather than being silently skipped - so a host only advertises
// GPUs it could actually describe. See normalizeGPUClass for the class rule.
func enumerateGPUs(ctx context.Context, runner CommandRunner) ([]GPUDevice, error) {
	out, err := runner.Output(ctx, nvidiaSMIBinary, nvidiaSMIQueryGPU, nvidiaSMIFormat)
	if err != nil {
		return nil, fmt.Errorf("runtime: nvidia-smi enumerate: %w", err)
	}
	devices := make([]GPUDevice, 0)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Each row is "uuid, name, memory.total" (nounits → memory is a bare MiB
		// integer). SplitN on the first two commas so a product name that itself
		// contains a comma is not split apart.
		fields := strings.SplitN(line, ",", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("runtime: nvidia-smi line %q: want 3 CSV fields, got %d", line, len(fields))
		}
		id := strings.TrimSpace(fields[0])
		class := normalizeGPUClass(fields[1])
		vram, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err != nil {
			return nil, fmt.Errorf("runtime: nvidia-smi line %q: memory.total not an integer: %w", line, err)
		}
		if id == "" || class == "" {
			return nil, fmt.Errorf("runtime: nvidia-smi line %q: empty uuid or name", line)
		}
		devices = append(devices, GPUDevice{ID: id, Class: class, VRAMMB: vram})
	}
	return devices, nil
}

// normalizeGPUClass turns a raw nvidia-smi product name into the wire "class":
// lowercase with whitespace runs collapsed to single hyphens (e.g. "NVIDIA
// GeForce RTX 4090" → "nvidia-geforce-rtx-4090"). strings.Fields collapses any
// run of whitespace and trims the ends, so double spaces never produce empty
// segments. The result is an opaque label for consumers, not a parseable spec.
func normalizeGPUClass(name string) string {
	return strings.Join(strings.Fields(strings.ToLower(name)), "-")
}
