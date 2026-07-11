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

	"github.com/benjaminfkile/wisp/internal/bus"
	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/policy"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// contractLabel is the container label Wisp uses to correlate a container back
// to the contract that owns it (see runtime.CreateOptions.Labels).
const contractLabel = "wisp.contract"

// bytesPerMB converts a request's memory_mb (mebibytes) to the byte limit the
// runtime expects.
const bytesPerMB = 1024 * 1024

// broker wires the contract model to the Runtime over HTTP. It owns the
// lifecycle of a contract: create + boot + run userdata on POST, report status
// on GET, and release (kill + mark released) on DELETE.
type broker struct {
	store  *contract.Store
	rt     runtime.Runtime
	pol    *policy.Config
	logger *slog.Logger

	// bus is the event bus Wisp publishes contract lifecycle events on and
	// which the /events endpoints ingest into and subscribe from (see
	// docs/DESIGN.md §6).
	bus *bus.Bus

	// appToken is the app-level bearer credential gating contract creation and
	// the event bus. An empty value disables the gate (see docs/DESIGN.md §8).
	appToken string

	// now is the clock used to compute time remaining; injectable for tests.
	now func() time.Time
}

// newBroker constructs a broker over the given store, runtime, launch policy,
// and event bus. appToken gates contract creation and the bus; an empty value
// disables that gate.
func newBroker(store *contract.Store, rt runtime.Runtime, pol *policy.Config, b *bus.Bus, logger *slog.Logger, appToken string) *broker {
	return &broker{store: store, rt: rt, pol: pol, bus: b, logger: logger, appToken: appToken, now: time.Now}
}

// routes registers the contract lifecycle endpoints on mux. Creating a contract
// is gated by the app-level token; the per-contract token gates /exec and
// /shell inside their handlers (see docs/DESIGN.md §8).
func (b *broker) routes(mux *http.ServeMux) {
	// GET /images is a public discovery document (like /healthz): any consumer
	// may learn the allow-list, default image, and limits without a token, so it
	// is registered without the app-token gate (see docs/DESIGN.md §7, §8).
	mux.HandleFunc("GET /images", b.images)
	mux.HandleFunc("POST /contracts", b.requireAppToken(b.create))
	mux.HandleFunc("GET /contracts/{id}", b.get)
	mux.HandleFunc("DELETE /contracts/{id}", b.release)
	mux.HandleFunc("POST /contracts/{id}/exec", b.exec)
	mux.HandleFunc("GET /contracts/{id}/shell", b.shell)
	mux.HandleFunc("POST /events", b.requireAppToken(b.publishEvent))
	mux.HandleFunc("GET /events", b.subscribeEvents)
}

// requireAppToken wraps next so it runs only when the request carries the
// app-level bearer token (Authorization: Bearer <token>). When the configured
// app token is empty the gate is disabled and every request passes through —
// the localhost-friendly default (see docs/DESIGN.md §8). Otherwise a missing or
// mismatched token is rejected with 401 before next runs. The comparison is
// constant-time (see bearerMatches).
func (b *broker) requireAppToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if b.appToken != "" && !bearerMatches(r, b.appToken) {
			writeError(w, http.StatusUnauthorized, "missing or invalid app token")
			return
		}
		next(w, r)
	}
}

// resourcesRequest is the optional per-request resource shaping. Each value is
// clamped down to the operator's configured maximum when that maximum is set; a
// zero value means "no request" (the runtime imposes no cap for that dimension).
type resourcesRequest struct {
	CPUs     float64 `json:"cpus"`
	MemoryMB int     `json:"memory_mb"`
	Pids     int     `json:"pids"`
}

// createRequest is the POST /contracts body (see docs/DESIGN.md §4, §7). The
// client picks an allowed image and shapes the network / resources; userdata
// owns the container's contents. There is no preset — wisp is domain-blind.
type createRequest struct {
	TTLSeconds int              `json:"ttl_seconds"`
	Image      string           `json:"image"`
	Network    string           `json:"network"`
	Resources  resourcesRequest `json:"resources"`
	Userdata   string           `json:"userdata"`
	Meta       map[string]any   `json:"meta"`
}

// createResponse is returned on a successful create.
type createResponse struct {
	ContractID string `json:"contract_id"`
	Token      string `json:"token"`
	Status     string `json:"status"`
}

// statusResponse is returned by GET /contracts/:id and DELETE /contracts/:id.
type statusResponse struct {
	ContractID          string         `json:"contract_id"`
	Status              string         `json:"status"`
	TTLSecondsRemaining int            `json:"ttl_seconds_remaining"`
	Meta                map[string]any `json:"meta,omitempty"`
}

// launchSpec is the launch configuration resolved from a create request against
// the operator policy: the image to boot, the clamped resources, and the network
// the container runs on.
type launchSpec struct {
	image    string
	cpus     float64
	memoryMB int
	pids     int
	network  string
}

// create handles POST /contracts: it validates the requested image and network
// against the operator policy, clamps the TTL and resources to the configured
// limits, records a contract, boots a container from the resolved image, runs
// the userdata script, transitions provisioning→ready, and returns the id,
// token, and final status (see docs/DESIGN.md §4, §7).
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

	// Resolve the image: an omitted image selects the policy default; any other
	// image must be in the allow-list, else it is a client error (400).
	image := req.Image
	if image == "" {
		image = b.pol.DefaultImage
	}
	if !b.pol.AllowsImage(image) {
		writeError(w, http.StatusBadRequest, "image not allowed: "+image)
		return
	}

	// Resolve the network: an omitted network selects the policy default; any
	// other network must be one the operator permits, else a client error (400).
	network := req.Network
	if network == "" {
		network = b.pol.DefaultNetwork()
	}
	if !b.pol.AllowsNetwork(network) {
		writeError(w, http.StatusBadRequest, "network not allowed: "+network)
		return
	}

	// The policy caps the contract: the requested TTL and resources are clamped
	// down to any configured maximum (see docs/DESIGN.md §7).
	ttl := b.pol.ClampTTL(time.Duration(req.TTLSeconds) * time.Second)
	spec := launchSpec{
		image:    image,
		cpus:     b.pol.ClampCPUs(req.Resources.CPUs),
		memoryMB: b.pol.ClampMemoryMB(req.Resources.MemoryMB),
		pids:     b.pol.ClampPids(req.Resources.Pids),
		network:  network,
	}

	c, err := b.store.Create(contract.CreateParams{
		TTL:   ttl,
		Image: image,
		Meta:  req.Meta,
	})
	if err != nil {
		// The only error Create returns for a positive TTL is ErrInvalidTTL,
		// already guarded above; treat anything here as a server fault.
		b.logger.Error("create contract", "error", err)
		writeError(w, http.StatusInternalServerError, "could not create contract")
		return
	}

	// contract.created announces the new lease before provisioning begins, so a
	// satellite can start watching for its contract.ready (see docs/DESIGN.md §6).
	b.publishLifecycle(eventContractCreated, c)

	c, err = b.provision(r.Context(), c, spec, req.Userdata)
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

// images handles GET /images: the unauthenticated discovery document any
// consumer can read to learn what it may request (see docs/DESIGN.md §7).
func (b *broker) images(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, imagesResponse{
		Images:  b.pol.Allow,
		Default: b.pol.DefaultImage,
		Limits: limitsResponse{
			MaxTTLSeconds: b.pol.Limits.MaxTTLSeconds,
			MaxCPUs:       b.pol.Limits.MaxCPUs,
			MaxMemoryMB:   b.pol.Limits.MaxMemoryMB,
			PidsLimit:     b.pol.Limits.PidsLimit,
			Networks:      b.pol.Limits.Networks,
		},
	})
}

// imagesResponse is the GET /images discovery document.
type imagesResponse struct {
	Images  []string       `json:"images"`
	Default string         `json:"default"`
	Limits  limitsResponse `json:"limits"`
}

// limitsResponse mirrors policy.Limits in the discovery document's JSON shape.
type limitsResponse struct {
	MaxTTLSeconds int      `json:"max_ttl_seconds"`
	MaxCPUs       float64  `json:"max_cpus"`
	MaxMemoryMB   int      `json:"max_memory_mb"`
	PidsLimit     int      `json:"pids_limit"`
	Networks      []string `json:"networks"`
}

// provision boots the container for c, runs its userdata, and drives the
// contract from requested through provisioning to ready. On any failure it
// destroys the container and marks the contract expired, returning the error.
func (b *broker) provision(ctx context.Context, c contract.Contract, spec launchSpec, userdata string) (contract.Contract, error) {
	if _, err := b.store.UpdateState(c.ID, contract.StateProvisioning); err != nil {
		return c, err
	}

	// Pull the resolved image on demand so a contract is never blocked from using
	// an allowed image just because it is not in `docker images` yet.
	if err := b.rt.EnsureImage(ctx, spec.image); err != nil {
		b.fail(ctx, c.ID, "")
		return c, err
	}

	cid, err := b.rt.Create(ctx, spec.image, createOptions(spec, c.ID))
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
	// contract.ready tells clients the container is provisioned and they may
	// exec / open shells freely (see docs/DESIGN.md §4, §6).
	b.publishLifecycle(eventContractReady, c)
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
	// contract.released announces the client-initiated teardown (see
	// docs/DESIGN.md §6). The reaper announces contract.expiring / contract.expired.
	b.publishLifecycle(eventContractReleased, c)
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
		Meta:                c.Meta,
	}
}

// keepAliveCmd is the command Wisp runs as a container's main (PID 1) process.
// A Wisp container is a persistent sandbox that clients drive via exec/shell for
// the contract's lifetime — the main process does no work, it just blocks so the
// container stays running. Without it, a bare base image's own default command
// (e.g. Alpine's /bin/sh) exits immediately and the container stops before any
// exec can attach, which surfaces as "container is not running". `tail` exits on
// SIGTERM, so release / `docker stop` reaps the container promptly.
var keepAliveCmd = []string{"tail", "-f", "/dev/null"}

// createOptions translates a resolved launch spec into the runtime's
// CreateOptions: the resource caps and network policy applied to the container,
// plus the label correlating it back to its contract. It always sets the
// keep-alive command so the container outlives its provisioning step.
func createOptions(spec launchSpec, contractID string) runtime.CreateOptions {
	opts := runtime.CreateOptions{
		Labels: map[string]string{contractLabel: contractID},
		// Run the keep-alive as the container's main process so it stays up for
		// the whole contract; all real work happens via exec/shell (see keepAliveCmd).
		Cmd: keepAliveCmd,
		Resources: runtime.Resources{
			NanoCPUs:    int64(spec.cpus * 1e9),
			MemoryBytes: int64(spec.memoryMB) * bytesPerMB,
			PidsLimit:   int64(spec.pids),
		},
	}
	// Only "none" maps to an explicit Docker network mode today; "egress" and
	// "open" both boot on the runtime's default network. Egress is not yet
	// separately enforced (see policy.NetworkEgress).
	if spec.network == policy.NetworkNone {
		opts.NetworkMode = "none"
	}
	return opts
}
