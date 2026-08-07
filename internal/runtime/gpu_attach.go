package runtime

import (
	"errors"

	"github.com/docker/docker/api/types/container"
)

// GPU device attachment — the KATA SEAM.
//
// Attaching a GPU to a leased container is done DIFFERENTLY by each launch
// mechanism, exactly like launchMechanism selects the container runtime from the
// isolation level. This file is the mapping from isolation level to a GPU
// attachment STRATEGY, kept structurally parallel to launchMechanism so the two
// dimensions of a launch (its runtime and its device attachment) are selected the
// same data-driven way.
//
// v1 ships exactly one GPU backend: shared/runc, which attaches whole devices via
// Docker DeviceRequests (the SDK equivalent of `docker run --gpus device=<id>`).
// The vm/kata slot EXISTS but has no backend yet, so it returns the typed
// ErrGPUAttachUnsupported. That slot is the FUTURE Kata + VFIO passthrough
// insertion point: adding VM-backed GPU passthrough is implementing that strategy
// HERE and flipping the policy attach map's vm entry to true (see
// policy.gpuAttachByIsolation) — with no create-path change. GPU attachment is
// therefore never gated on isolation == "shared" inline anywhere; the selection
// lives only in this seam.
//
// In v1 the create path already rejects a GPU request at any non-shared isolation
// level (the policy attach map advertises only shared), so the vm slot's error is
// unreachable in practice — but it is the honest, typed answer for a mechanism
// that cannot attach a GPU, and it is exercised directly by unit tests.

// nvidiaGPUDriver is the device-driver name the NVIDIA Container Runtime
// registers for Docker DeviceRequests. A request naming this driver, with
// explicit DeviceIDs and the "gpu" capability, is the SDK equivalent of
// `docker run --gpus device=<id>` — an EXCLUSIVE attach of exactly those devices,
// not a count.
const nvidiaGPUDriver = "nvidia"

// gpuCapability is the device capability Wisp requests: a plain GPU. Docker's
// Capabilities field is an OR-list of AND-lists, so requesting the single "gpu"
// capability is [][]string{{"gpu"}}.
var gpuCapability = [][]string{{"gpu"}}

// ErrGPUAttachUnsupported is the typed error the GPU-attach seam returns for a
// launch mechanism that has no GPU backend yet (v1: anything but shared/runc, i.e.
// the vm/kata slot). The create path converts it to the same capability rejection
// its policy attach-map check produces; because that check already rejects a GPU
// request at a non-shared level, this error is unreachable through the create path
// in v1, but it is the honest typed answer for the seam and is tested directly.
var ErrGPUAttachUnsupported = errors.New("runtime: GPU attach not supported for this launch mechanism")

// gpuAttachment maps a requested isolation level to the Docker DeviceRequests that
// attach the given whole GPU devices under that level's launch mechanism. It is
// the GPU counterpart of launchMechanism: shared/runc attaches via the nvidia
// DeviceRequests strategy; the vm/kata slot exists but has no backend yet and
// returns ErrGPUAttachUnsupported (the future Kata + VFIO insertion point). Any
// other level likewise has no GPU backend and returns the typed error.
//
// Callers invoke it only when there are devices to attach, so an empty deviceIDs
// slice is not special-cased here. Both DockerRuntime.Create and the Fake go
// through this seam so their behavior matches and the mapping is testable without
// a real daemon.
func gpuAttachment(isolation string, deviceIDs []string) ([]container.DeviceRequest, error) {
	switch isolation {
	case IsolationShared, "":
		return sharedDeviceRequests(deviceIDs), nil
	case IsolationVM:
		// FUTURE: Kata + VFIO GPU passthrough goes here. Implement the strategy and
		// flip policy.gpuAttachByIsolation's vm entry to true; no other change.
		return nil, ErrGPUAttachUnsupported
	default:
		return nil, ErrGPUAttachUnsupported
	}
}

// sharedDeviceRequests builds the Docker DeviceRequests that attach exactly the
// given devices to a shared/runc container: the nvidia driver, the explicit
// device IDs, and the "gpu" capability — the SDK equivalent of
// `docker run --gpus device=<id>,<id>`. Naming explicit DeviceIDs (rather than a
// Count) is what makes the attach exclusive to the allocator-chosen devices.
func sharedDeviceRequests(deviceIDs []string) []container.DeviceRequest {
	return []container.DeviceRequest{{
		Driver:       nvidiaGPUDriver,
		DeviceIDs:    deviceIDs,
		Capabilities: gpuCapability,
	}}
}
