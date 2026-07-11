package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// defaultBaseImage is the bare base image booted when a preset does not resolve
// to a more specific image. Permission presets (a later task, see
// docs/DESIGN.md §7) will map preset names to images and resource limits; until
// then every contract boots from this single bare base.
const defaultBaseImage = "wisp-base"

// contractLabel is the container label Wisp uses to correlate a container back
// to the contract that owns it (see runtime.CreateOptions.Labels).
const contractLabel = "wisp.contract"

// broker wires the contract model to the Runtime over HTTP. It owns the
// lifecycle of a contract: create + boot + run userdata on POST, report status
// on GET, and release (kill + mark released) on DELETE.
type broker struct {
	store  *contract.Store
	rt     runtime.Runtime
	logger *slog.Logger

	// now is the clock used to compute time remaining; injectable for tests.
	now func() time.Time
}

// newBroker constructs a broker over the given store and runtime.
func newBroker(store *contract.Store, rt runtime.Runtime, logger *slog.Logger) *broker {
	return &broker{store: store, rt: rt, logger: logger, now: time.Now}
}

// routes registers the contract lifecycle endpoints on mux.
func (b *broker) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /contracts", b.create)
	mux.HandleFunc("GET /contracts/{id}", b.get)
	mux.HandleFunc("DELETE /contracts/{id}", b.release)
}

// createRequest is the POST /contracts body (see docs/DESIGN.md §4).
type createRequest struct {
	TTLSeconds int    `json:"ttl_seconds"`
	Preset     string `json:"preset"`
	Userdata   string `json:"userdata"`
}

// createResponse is returned on a successful create.
type createResponse struct {
	ContractID string `json:"contract_id"`
	Token      string `json:"token"`
	Status     string `json:"status"`
}

// statusResponse is returned by GET /contracts/:id and DELETE /contracts/:id.
type statusResponse struct {
	ContractID          string `json:"contract_id"`
	Status              string `json:"status"`
	TTLSecondsRemaining int    `json:"ttl_seconds_remaining"`
}

// create handles POST /contracts: it records a contract, boots a container from
// the preset's base image, runs the userdata script, transitions
// provisioning→ready, and returns the id, token, and final status.
func (b *broker) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.TTLSeconds <= 0 {
		writeError(w, http.StatusBadRequest, "ttl_seconds must be positive")
		return
	}

	c, err := b.store.Create(contract.CreateParams{
		TTL:    time.Duration(req.TTLSeconds) * time.Second,
		Preset: req.Preset,
	})
	if err != nil {
		// The only error Create returns for a positive TTL is ErrInvalidTTL,
		// already guarded above; treat anything here as a server fault.
		b.logger.Error("create contract", "error", err)
		writeError(w, http.StatusInternalServerError, "could not create contract")
		return
	}

	c, err = b.provision(r.Context(), c, req.Userdata)
	if err != nil {
		b.logger.Error("provision contract", "contract_id", c.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "provisioning failed")
		return
	}

	writeJSON(w, http.StatusCreated, createResponse{
		ContractID: c.ID,
		Token:      c.Token,
		Status:     string(c.State),
	})
}

// provision boots the container for c, runs its userdata, and drives the
// contract from requested through provisioning to ready. On any failure it
// destroys the container and marks the contract expired, returning the error.
func (b *broker) provision(ctx context.Context, c contract.Contract, userdata string) (contract.Contract, error) {
	if _, err := b.store.UpdateState(c.ID, contract.StateProvisioning); err != nil {
		return c, err
	}

	image := b.imageFor(c.Preset)
	cid, err := b.rt.Create(ctx, image, runtime.CreateOptions{
		Labels: map[string]string{contractLabel: c.ID},
	})
	if err != nil {
		b.fail(ctx, c.ID, "")
		return c, err
	}
	if _, err := b.store.SetContainerID(c.ID, cid); err != nil {
		b.fail(ctx, c.ID, cid)
		return c, err
	}
	if err := b.rt.Start(ctx, cid); err != nil {
		b.fail(ctx, c.ID, cid)
		return c, err
	}

	if userdata != "" {
		res, err := b.rt.ExecSync(ctx, cid, []string{"/bin/sh", "-c", userdata})
		if err != nil {
			b.fail(ctx, c.ID, cid)
			return c, err
		}
		if res.ExitCode != 0 {
			b.fail(ctx, c.ID, cid)
			return c, &userdataError{ExitCode: res.ExitCode, Stderr: res.Stderr}
		}
	}

	c, err = b.store.UpdateState(c.ID, contract.StateReady)
	if err != nil {
		b.fail(ctx, c.ID, cid)
		return c, err
	}
	return c, nil
}

// fail destroys the container (if one was created) and marks the contract
// expired. Errors from the cleanup are logged, not returned: the caller already
// has the originating failure to report.
func (b *broker) fail(ctx context.Context, id, containerID string) {
	if containerID != "" {
		if err := b.rt.Kill(ctx, containerID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			b.logger.Error("kill container after failed provision", "contract_id", id, "error", err)
		}
	}
	if _, err := b.store.UpdateState(id, contract.StateExpired); err != nil {
		b.logger.Error("mark contract expired", "contract_id", id, "error", err)
	}
}

// userdataError reports a userdata script that exited non-zero.
type userdataError struct {
	ExitCode int
	Stderr   string
}

func (e *userdataError) Error() string {
	return "userdata script failed with exit code " + strconv.Itoa(e.ExitCode)
}

// get handles GET /contracts/:id: it reports the current status and the
// seconds remaining until the TTL elapses.
func (b *broker) get(w http.ResponseWriter, r *http.Request) {
	c, err := b.store.Get(r.PathValue("id"))
	if errors.Is(err, contract.ErrNotFound) {
		writeError(w, http.StatusNotFound, "contract not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read contract")
		return
	}
	writeJSON(w, http.StatusOK, b.statusOf(c))
}

// release handles DELETE /contracts/:id: it kills the container and marks the
// contract released. Releasing an already-terminal contract is a no-op that
// echoes the current status, so DELETE is safe to retry.
func (b *broker) release(w http.ResponseWriter, r *http.Request) {
	c, err := b.store.Get(r.PathValue("id"))
	if errors.Is(err, contract.ErrNotFound) {
		writeError(w, http.StatusNotFound, "contract not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read contract")
		return
	}

	if c.State.Terminal() {
		writeJSON(w, http.StatusOK, b.statusOf(c))
		return
	}

	if c.ContainerID != "" {
		if err := b.rt.Kill(r.Context(), c.ContainerID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			b.logger.Error("kill container on release", "contract_id", c.ID, "error", err)
		}
	}

	c, err = b.store.UpdateState(c.ID, contract.StateReleased)
	if err != nil {
		b.logger.Error("mark contract released", "contract_id", c.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not release contract")
		return
	}
	writeJSON(w, http.StatusOK, b.statusOf(c))
}

// statusOf builds the status response for c, clamping time remaining at zero for
// terminal or already-expired contracts.
func (b *broker) statusOf(c contract.Contract) statusResponse {
	remaining := 0
	if !c.State.Terminal() {
		if d := c.ExpiresAt.Sub(b.now()); d > 0 {
			remaining = int(d / time.Second)
		}
	}
	return statusResponse{
		ContractID:          c.ID,
		Status:              string(c.State),
		TTLSecondsRemaining: remaining,
	}
}

// imageFor resolves a preset name to its base image. Until presets land (a
// later task) every preset resolves to the bare default base image.
func (b *broker) imageFor(preset string) string {
	return defaultBaseImage
}
