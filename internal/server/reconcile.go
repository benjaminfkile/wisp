package server

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/benjaminfkile/wisp/internal/capacity"
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
// (running or stopped, so orphans from a host reboot are visible too) and, for
// each RUNNING container with well-formed labels, recovers the contract id
// (wisp.contract) and absolute expiry (wisp.expires_at, Unix seconds) and
// re-adopts a tracking entry in StateReady. The reaper then treats them like
// any live lease on its next sweep: containers already past their expiry are
// killed and marked expired, and still-valid ones resume being tracked to their
// TTL. Callers MUST run this before the reaper starts so no orphan is missed on
// the first sweep.
//
// A non-running container carrying the contract label is a dead lease from a
// prior wispd — its process is no longer running and no capacity is truly held
// by it. Reconcile does NOT adopt it (which would reserve full capacity for a
// container that isn't consuming any and could report the host at_capacity while
// idle) and instead force-removes it (rt.Kill), so a `docker ps -a` after
// startup no longer shows stale wisp containers.
//
// A container whose labels are missing or malformed cannot be tracked (there is
// no trustworthy id or expiry to enforce a TTL against) but carries wisp.contract
// so it is unambiguously ours; leaving it running would be worse than removing it,
// so Reconcile force-removes it with a clear warn log rather than skipping it and
// letting an unaccounted-for container linger forever. A failure to even list
// containers is logged and returns without adopting anything — a best-effort
// recovery must never keep the daemon from starting.
//
// alloc, when non-nil, is the exclusive GPU device allocator: for each adopted
// lease carrying GPU devices (the wisp.gpus label), Reconcile reserves those
// devices in the allocator so a restarted wispd never re-hands a device a
// surviving lease still holds. A reserved device id the host no longer detects
// (e.g. a GPU removed since the container launched) is logged and skipped rather
// than crashing the reconcile. Pass nil on a host with no GPU allocator wired.
//
// cap, when non-nil, is the aggregate capacity allocator: for each adopted lease
// Reconcile re-Reserves its POST-CLAMP cpus and memory (the wisp.cpus and
// wisp.memory_mb labels) plus its one contract slot, exactly parallel to the GPU
// Reserve, so a restarted wispd rebuilds aggregate usage from surviving containers
// and never oversubscribes the host by under-counting a live lease. A missing or
// malformed capacity label is logged and treated as 0 for that dimension; the
// contract is still adopted and still counts toward max_contracts, so one bad
// label never drops a lease from the budget. Pass nil when no capacity allocator
// is wired.
func Reconcile(ctx context.Context, store *contract.Store, rt runtime.Runtime, alloc *gpu.Allocator, cap *capacity.Allocator, logger *slog.Logger) {
	containers, err := rt.ListLeased(ctx)
	if err != nil {
		logger.Error("reconcile: list leased containers", "error", err)
		return
	}

	adopted := 0
	for _, lc := range containers {
		// A non-running container is a dead lease from a prior wispd (a host
		// reboot left it Exited, or the daemon killed it out of band). It holds
		// no live process, and adopting it would falsely reserve its full
		// capacity for a container that isn't consuming any. Force-remove it so
		// `docker ps -a` no longer shows the stale artifact, and never reserve
		// capacity for it.
		if !lc.Running {
			if err := rt.Kill(ctx, lc.ID); err != nil {
				logger.Warn("reconcile: remove non-running leased container failed",
					"container_id", lc.ID, "contract_id", lc.Labels[contractLabel], "error", err)
			} else {
				logger.Info("reconcile: removed non-running leased container (not adopted, no capacity reserved)",
					"container_id", lc.ID, "contract_id", lc.Labels[contractLabel])
			}
			continue
		}
		id := lc.Labels[contractLabel]
		if id == "" {
			// The runtime filtered on the label's presence, so an empty value here
			// means a malformed label. The container is unambiguously ours but has
			// no trustworthy id to track it by; leaving it running unaccounted-for
			// is worse than removing it, so force-remove it.
			logger.Warn("reconcile: container has missing contract label; removing", "container_id", lc.ID)
			if err := rt.Kill(ctx, lc.ID); err != nil {
				logger.Warn("reconcile: remove malformed-label container failed", "container_id", lc.ID, "error", err)
			}
			continue
		}
		expiresAt, ok := parseExpiresAt(lc.Labels[expiresAtLabel])
		if !ok {
			// Without a trustworthy expiry the reaper cannot enforce a TTL, and
			// leaving the container running unaccounted-for would let a lease
			// outlive its intended TTL forever. Force-remove it instead.
			logger.Warn("reconcile: contract has missing or malformed expiry label; removing",
				"contract_id", id, "container_id", lc.ID, "expires_at", lc.Labels[expiresAtLabel])
			if err := rt.Kill(ctx, lc.ID); err != nil {
				logger.Warn("reconcile: remove malformed-label container failed",
					"contract_id", id, "container_id", lc.ID, "error", err)
			}
			continue
		}
		gpuIDs := parseGPUsLabel(lc.Labels[gpusLabel])
		// Recover the POST-CLAMP reserved capacity so the re-Reserve below restores
		// exactly what create took. A missing or malformed label yields 0 for that
		// dimension (the contract still counts one slot) and a single warning, so one
		// bad label never drops the lease from the aggregate budget.
		cpus, cpusOK := parseReservedCPUs(lc.Labels[cpusLabel])
		memoryMB, memOK := parseReservedMemoryMB(lc.Labels[memoryMBLabel])
		if !cpusOK || !memOK {
			logger.Warn("reconcile: contract has missing or malformed capacity label(s); reserving what parsed",
				"contract_id", id, "container_id", lc.ID,
				"cpus", lc.Labels[cpusLabel], "memory_mb", lc.Labels[memoryMBLabel])
		}
		externalID := lc.Labels[externalIDLabel]
		if _, err := store.Adopt(contract.AdoptParams{ID: id, ContainerID: lc.ID, ExpiresAt: expiresAt, GPUDeviceIDs: gpuIDs, ReservedCPUs: cpus, ReservedMemoryMB: memoryMB, ExternalID: externalID}); err != nil {
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
		// Rebuild aggregate capacity usage so a restarted wispd never oversubscribes
		// the host: re-Reserve this lease's contract slot plus its post-clamp cpus and
		// memory, unconditionally (a reconciled lease already holds its capacity, even
		// if a since-lowered budget would now reject it — see capacity.Allocator.Reserve).
		if cap != nil {
			cap.Reserve(cpus, memoryMB)
		}
		adopted++
		logger.Info("reconcile: adopted pre-existing leased container",
			"contract_id", id, "container_id", lc.ID, "expires_at", expiresAt, "gpus", strings.Join(gpuIDs, ","),
			"cpus", cpus, "memory_mb", memoryMB)
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

// parseReservedCPUs parses a wisp.cpus label value (a lease's post-clamp reserved
// CPU as a decimal fraction of host cores) into a float. It reports ok=false for an
// empty, non-numeric, or negative value so the reconcile can warn and re-Reserve 0
// on that dimension rather than corrupt aggregate usage with a bad figure — the
// read half of the wisp.cpus round-trip create writes.
func parseReservedCPUs(v string) (float64, bool) {
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return f, true
}

// parseReservedMemoryMB parses a wisp.memory_mb label value (a lease's post-clamp
// reserved memory in whole mebibytes) into an int, with the same tolerance as
// parseReservedCPUs: an empty, non-numeric, or negative value reports ok=false so
// the reconcile re-Reserves 0 on that dimension. It is the read half of the
// wisp.memory_mb round-trip create writes.
func parseReservedMemoryMB(v string) (int, bool) {
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
