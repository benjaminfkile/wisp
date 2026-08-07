package server

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/gpu"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// Reconcile rebuilds the in-memory contract store from the Docker labels of
// containers a previous wispd process left behind, so a crash or restart does
// not orphan leased containers — which would otherwise keep running with the
// consumer's secrets in their environment and no daemon to enforce their TTL.
//
// It queries the runtime for every container carrying the wisp.contract label
// and, for each, recovers the contract id (wisp.contract) and absolute expiry
// (wisp.expires_at, Unix seconds) and re-adopts a tracking entry in StateReady.
// The reaper then treats them like any live lease on its next sweep: containers
// already past their expiry are killed and marked expired, and still-valid ones
// resume being tracked to their TTL. Callers MUST run this before the reaper
// starts so no orphan is missed on the first sweep.
//
// Reconcile is tolerant of a container whose labels are missing or malformed: it
// logs and skips that container rather than failing the whole reconcile, so one
// bad label never blocks recovery of the rest. A failure to even list containers
// is logged and returns without adopting anything — a best-effort recovery must
// never keep the daemon from starting.
//
// alloc, when non-nil, is the exclusive GPU device allocator: for each adopted
// lease carrying GPU devices (the wisp.gpus label), Reconcile reserves those
// devices in the allocator so a restarted wispd never re-hands a device a
// surviving lease still holds. A reserved device id the host no longer detects
// (e.g. a GPU removed since the container launched) is logged and skipped rather
// than crashing the reconcile. Pass nil on a host with no GPU allocator wired.
func Reconcile(ctx context.Context, store *contract.Store, rt runtime.Runtime, alloc *gpu.Allocator, logger *slog.Logger) {
	containers, err := rt.ListLeased(ctx)
	if err != nil {
		logger.Error("reconcile: list leased containers", "error", err)
		return
	}

	adopted := 0
	for _, lc := range containers {
		id := lc.Labels[contractLabel]
		if id == "" {
			// The runtime filtered on the label's presence, so an empty value here
			// means a malformed label; skip it rather than track an id-less lease.
			logger.Warn("reconcile: container has missing contract label; skipping", "container_id", lc.ID)
			continue
		}
		expiresAt, ok := parseExpiresAt(lc.Labels[expiresAtLabel])
		if !ok {
			// Without a trustworthy expiry the reaper cannot enforce a TTL, so skip
			// the container rather than adopt a lease that would never (or wrongly)
			// expire. This is logged so an operator can see the orphan was left.
			logger.Warn("reconcile: contract has missing or malformed expiry label; skipping",
				"contract_id", id, "container_id", lc.ID, "expires_at", lc.Labels[expiresAtLabel])
			continue
		}
		gpuIDs := parseGPUsLabel(lc.Labels[gpusLabel])
		if _, err := store.Adopt(contract.AdoptParams{ID: id, ContainerID: lc.ID, ExpiresAt: expiresAt, GPUDeviceIDs: gpuIDs}); err != nil {
			logger.Error("reconcile: adopt contract", "contract_id", id, "container_id", lc.ID, "error", err)
			continue
		}
		// Rebuild allocator occupancy so a device a surviving lease still holds is
		// never re-assigned. A device id the host no longer detects is logged and
		// skipped — the reconcile must not crash on stale hardware.
		if alloc != nil && len(gpuIDs) > 0 {
			if unknown := alloc.Reserve(gpuIDs); len(unknown) > 0 {
				logger.Warn("reconcile: lease references GPU device(s) no longer detected; skipping those",
					"contract_id", id, "container_id", lc.ID, "devices", strings.Join(unknown, ","))
			}
		}
		adopted++
		logger.Info("reconcile: adopted pre-existing leased container",
			"contract_id", id, "container_id", lc.ID, "expires_at", expiresAt, "gpus", strings.Join(gpuIDs, ","))
	}
	if adopted > 0 {
		logger.Info("reconcile: rebuilt tracking for pre-existing leases", "count", adopted)
	}
}

// gpusLabelValue joins a lease's assigned GPU device IDs into the comma-separated
// wisp.gpus label value. It returns "" for a lease with no GPUs so the caller can
// omit the label entirely. Device IDs are opaque "GPU-<uuid>" strings that never
// contain a comma, so the join is unambiguous (see parseGPUsLabel for the split).
func gpusLabelValue(ids []string) string {
	return strings.Join(ids, ",")
}

// parseGPUsLabel splits a wisp.gpus label value back into device IDs, preserving
// the written order and dropping empty segments so a trailing comma or an empty
// value yields no IDs. It is the read half of the wisp.gpus round-trip the
// startup reconcile relies on.
func parseGPUsLabel(v string) []string {
	if v == "" {
		return nil
	}
	out := make([]string, 0)
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseExpiresAt parses a wisp.expires_at label value (a contract's absolute
// expiry as Unix seconds) into a time. It reports ok=false for an empty or
// non-numeric value so the caller can log and skip a container whose expiry
// cannot be trusted, keeping the reconcile tolerant of malformed labels.
func parseExpiresAt(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}
