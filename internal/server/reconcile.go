package server

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/benjaminfkile/wisp/internal/contract"
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
func Reconcile(ctx context.Context, store *contract.Store, rt runtime.Runtime, logger *slog.Logger) {
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
		if _, err := store.Adopt(contract.AdoptParams{ID: id, ContainerID: lc.ID, ExpiresAt: expiresAt}); err != nil {
			logger.Error("reconcile: adopt contract", "contract_id", id, "container_id", lc.ID, "error", err)
			continue
		}
		adopted++
		logger.Info("reconcile: adopted pre-existing leased container",
			"contract_id", id, "container_id", lc.ID, "expires_at", expiresAt)
	}
	if adopted > 0 {
		logger.Info("reconcile: rebuilt tracking for pre-existing leases", "count", adopted)
	}
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
