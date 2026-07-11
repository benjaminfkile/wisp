package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
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
	mux.HandleFunc("POST /contracts/{id}/exec", b.exec)
	mux.HandleFunc("GET /contracts/{id}/shell", b.shell)
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

// execRequest is the POST /contracts/:id/exec body (see docs/DESIGN.md §5). The
// command is run through a shell so clients can send compound commands like
// "cd /repo && git diff"; per docs/DESIGN.md each exec is a fresh process with
// no shared cwd/env between calls.
type execRequest struct {
	Command string `json:"command"`
}

// execResponse is the machine-readable outcome of a sync exec: fully buffered
// stdout and stderr plus the process exit code (a non-zero exit code is a
// successful HTTP response, not an error).
type execResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// execStreamChunk is one Server-Sent Event payload carrying a slice of live
// output. Stream is "stdout" or "stderr"; Data is the bytes produced since the
// previous chunk (JSON-escaped, so embedded newlines survive SSE framing).
type execStreamChunk struct {
	Stream string `json:"stream"`
	Data   string `json:"data"`
}

// execStreamExit is the terminal SSE payload of a streaming exec: the process
// exit code, sent once the command completes.
type execStreamExit struct {
	ExitCode int `json:"exit_code"`
}

// exec handles POST /contracts/:id/exec: it runs {command} inside the
// contract's container. By default (sync mode) it runs the command to
// completion via Runtime.ExecSync and returns stdout/stderr/exit as one JSON
// body. With ?stream=1 it instead streams output live as Server-Sent Events
// (see execStream), so a caller can watch a long-running command's output as it
// is produced.
//
// Each exec is a fresh process: Wisp runs the command with a new `/bin/sh -c`
// invocation, so there is no shared cwd or environment between calls (see
// docs/DESIGN.md §5, "Each exec is a fresh process"). Clients that need state to
// persist across steps send a single compound command.
//
// The call requires the contract's bearer token (Authorization: Bearer <token>)
// and rejects execs against a contract that is not ready (409), unknown (404),
// or presented without a valid token (401). These checks are identical in both
// modes: the stream flag only changes how a successful exec's output is framed.
func (b *broker) exec(w http.ResponseWriter, r *http.Request) {
	c, err := b.store.Get(r.PathValue("id"))
	if errors.Is(err, contract.ErrNotFound) {
		writeError(w, http.StatusNotFound, "contract not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read contract")
		return
	}

	if !bearerMatches(r, c.Token) {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}

	if c.State != contract.StateReady {
		writeError(w, http.StatusConflict, "contract not ready")
		return
	}

	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Command == "" {
		writeError(w, http.StatusBadRequest, "command must not be empty")
		return
	}

	if r.URL.Query().Get("stream") == "1" {
		b.execStream(w, r, c, req.Command)
		return
	}

	res, err := b.rt.ExecSync(r.Context(), c.ContainerID, []string{"/bin/sh", "-c", req.Command})
	if err != nil {
		b.logger.Error("exec in container", "contract_id", c.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "exec failed")
		return
	}

	writeJSON(w, http.StatusOK, execResponse{
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
	})
}

// execStream runs command in streaming mode and writes the output live to w as
// Server-Sent Events. Each output chunk is a `chunk` event whose data is an
// execStreamChunk; the command's exit code arrives as a final `exit` event with
// an execStreamExit payload. Every event is flushed immediately so the client
// sees output as it is produced rather than all at once at the end.
//
// The 200 status and SSE headers are committed before the first byte of output,
// so a runtime failure mid-stream cannot change the status code; it is instead
// surfaced as an `error` event and logged. A client that disconnects makes a
// flush write fail, which propagates back through emit to stop the exec.
func (b *broker) execStream(w http.ResponseWriter, r *http.Request, c contract.Contract, command string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// No streaming transport available (should not happen for net/http).
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	emit := func(chunk runtime.ExecChunk) error {
		if err := writeSSE(w, "chunk", execStreamChunk{
			Stream: chunk.Stream,
			Data:   string(chunk.Data),
		}); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	exitCode, err := b.rt.ExecStream(r.Context(), c.ContainerID, []string{"/bin/sh", "-c", command}, emit)
	if err != nil {
		b.logger.Error("stream exec in container", "contract_id", c.ID, "error", err)
		_ = writeSSE(w, "error", map[string]string{"error": "exec failed"})
		flusher.Flush()
		return
	}

	_ = writeSSE(w, "exit", execStreamExit{ExitCode: exitCode})
	flusher.Flush()
}

// writeSSE encodes payload as JSON and writes it as a single named Server-Sent
// Event. JSON escaping keeps the payload on one `data:` line even when it
// contains newlines, so multi-line output survives SSE framing intact.
func writeSSE(w http.ResponseWriter, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}

// bearerMatches reports whether the request carries an Authorization: Bearer
// header whose token equals want. The comparison is constant-time to avoid
// leaking the token through timing (see docs/DESIGN.md §8).
func bearerMatches(r *http.Request, want string) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	got := h[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
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
