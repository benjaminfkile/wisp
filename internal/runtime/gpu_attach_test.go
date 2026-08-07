package runtime

import (
	"errors"
	"reflect"
	"testing"

	"github.com/docker/docker/api/types/container"
)

// TestGPUAttachmentShared verifies the shared/runc slot of the GPU-attach seam
// builds the nvidia DeviceRequests exactly: the "nvidia" driver, the explicit
// device IDs in order, and the single "gpu" capability — the SDK equivalent of
// `docker run --gpus device=...`.
func TestGPUAttachmentShared(t *testing.T) {
	for _, iso := range []string{IsolationShared, ""} {
		reqs, err := gpuAttachment(iso, []string{"GPU-0", "GPU-1"})
		if err != nil {
			t.Fatalf("gpuAttachment(%q): unexpected error %v", iso, err)
		}
		want := []container.DeviceRequest{{
			Driver:       "nvidia",
			DeviceIDs:    []string{"GPU-0", "GPU-1"},
			Capabilities: [][]string{{"gpu"}},
		}}
		if !reflect.DeepEqual(reqs, want) {
			t.Fatalf("gpuAttachment(%q) = %+v, want %+v", iso, reqs, want)
		}
	}
}

// TestGPUAttachmentUnsupportedMechanisms verifies the vm/kata slot — and any other
// launch mechanism with no GPU backend yet — returns the typed
// ErrGPUAttachUnsupported and no DeviceRequests. This is the load-bearing seam: a
// future Kata + VFIO backend is added by implementing that slot, not by touching
// the create path.
func TestGPUAttachmentUnsupportedMechanisms(t *testing.T) {
	for _, iso := range []string{IsolationVM, IsolationSandboxed, "bogus"} {
		reqs, err := gpuAttachment(iso, []string{"GPU-0"})
		if !errors.Is(err, ErrGPUAttachUnsupported) {
			t.Fatalf("gpuAttachment(%q) error = %v, want ErrGPUAttachUnsupported", iso, err)
		}
		if reqs != nil {
			t.Fatalf("gpuAttachment(%q) reqs = %+v, want nil", iso, reqs)
		}
	}
}
