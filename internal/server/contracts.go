package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/benjaminfkile/wisp/internal/bus"
	"github.com/benjaminfkile/wisp/internal/capacity"
	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/gpu"
	"github.com/benjaminfkile/wisp/internal/policy"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// contractLabel is the container label Wisp uses to correlate a container back
// to the contract that owns it (see runtime.CreateOptions.Labels). It is defined
// in the runtime package because the runtime filters on it during the startup
// reconcile; the alias keeps this package's create path referring to it locally.
const contractLabel = runtime.ContractLabel

// expiresAtLabel is the container label carrying a contract's absolute expiry as
// Unix seconds, written at create time next to contractLabel so a wispd restart
// can rebuild the contract's TTL tracking from the container's labels alone (see
// reconcile). Defined in the runtime package for the same reason as contractLabel.
const expiresAtLabel = runtime.ExpiresAtLabel

// gpusLabel is the container label carrying the comma-joined GPU device IDs a
// lease was exclusively assigned, written at create time next to contractLabel
// so a wispd restart can rebuild the device allocator's occupancy from the
// container's labels alone (see reconcile). Defined in the runtime package for
// the same reason as contractLabel.
const gpusLabel = runtime.GPUsLabel

// cpusLabel and memoryMBLabel are the container labels carrying a lease's
// POST-CLAMP reserved CPU and memory — exactly the amounts reserved in the
// aggregate capacity allocator — written at create time next to contractLabel so
// a wispd restart can rebuild aggregate capacity usage from the container's labels
// alone and never oversubscribe the host by under-counting a live lease (see
// reconcile). Defined in the runtime package for the same reason as contractLabel.
const cpusLabel = runtime.CPUsLabel
const memoryMBLabel = runtime.MemoryMBLabel

// externalIDLabel is the container label carrying a caller-supplied opaque
// identifier (e.g. an upstream lease id) accepted at create time, written next
// to contractLabel so a wispd restart's reconcile can restore the mapping from
// the container's labels alone. Defined in the runtime package for the same
// reason as contractLabel.
const externalIDLabel = runtime.ExternalIDLabel

// maxExternalIDLen bounds the opaque external_id a create call may attach so an
// arbitrarily long identifier cannot be smuggled into a container label.
const maxExternalIDLen = 128

// noNewPrivilegesOpt is the Docker security option applied to every leased
// container so a process inside it cannot gain privileges via setuid binaries
// (e.g. sudo/su). It is safe, workload-agnostic hardening: it neither drops
// Linux capabilities nor blocks root-in-container provisioning/userdata.
const noNewPrivilegesOpt = "no-new-privileges:true"

// bytesPerMB converts a request's memory_mb (mebibytes) to the byte limit the
// runtime expects.
const bytesPerMB = 1024 * 1024

// errAtCapacity is the stable 409 message returned when a create would exceed one
// of the host's AGGREGATE capacity budgets (concurrent contracts, total CPU, or
// total memory). It carries the stable "at capacity" token upstream matches on and
// is deliberately DISTINCT from the GPU allocator's "no GPU devices currently
// available" 409, so a caller can tell host-budget exhaustion from GPU exhaustion.
// The ask itself was within the per-lease caps — the HOST is full — which is why
// this is a 409 conflict, not a 400 rejection.
const errAtCapacity = "host at capacity: contract, cpu, or memory budget exhausted"

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

	// iso is the host's effective isolation posture — the operator allow-list
	// intersected with the levels the daemon can actually run — computed once at
	// construction (see detectIsolation). The create path validates a requested
	// level against it so the host never accepts an isolation it cannot launch,
	// and the read surface advertises it.
	iso policy.IsolationCapabilities

	// gpu is the host's effective GPU posture — the operator's GPU config
	// intersected with the daemon's NVIDIA runtime and the enumerated devices —
	// computed once at construction (see detectGPU). The read surface advertises it
	// (the /images "gpu" block) and the create path enforces it (see allocateGPUs).
	gpu policy.GPUCapabilities

	// alloc assigns whole GPU devices exclusively from the detected inventory. It
	// is built once at construction from the effective GPU posture's device IDs and
	// is the single source of truth for which device is held by which lease: the
	// create path allocates from it (see allocateGPUs), and every terminal path
	// (release, provision failure, and the reaper's expiry) frees back to it. The
	// startup reconcile rebuilds its occupancy from surviving containers' labels.
	alloc *gpu.Allocator

	// cap enforces the host's AGGREGATE capacity budgets (max_contracts /
	// total_cpus / total_memory_mb) across all live leases. Like alloc it is a
	// single shared instance built once at construction and threaded through every
	// path: the create path reserves against it (a 409 when a budget is exhausted),
	// and release, provision failure, and the reaper's expiry free back to it. The
	// read surface advertises its live usage (the /images "capacity" block).
	cap *capacity.Allocator

	// now is the clock used to compute time remaining; injectable for tests.
	now func() time.Time
}

// newBroker constructs a broker over the given store, runtime, launch policy,
// and event bus. appToken gates contract creation and the bus; an empty value
// disables that gate.
func newBroker(store *contract.Store, rt runtime.Runtime, pol *policy.Config, b *bus.Bus, logger *slog.Logger, appToken string) *broker {
	br := &broker{store: store, rt: rt, pol: pol, bus: b, logger: logger, appToken: appToken, now: time.Now}
	br.iso = br.detectIsolation(context.Background())
	br.gpu = br.detectGPU(context.Background())
	// Build the exclusive device allocator over the effective GPU inventory so the
	// create path can assign whole devices and every terminal path can free them.
	// A GPU-less host yields an empty allocator whose Free/Allocate are harmless
	// no-ops.
	br.alloc = gpu.NewAllocator(gpuDeviceIDs(br.gpu.Devices()))
	// Build the aggregate capacity allocator over the operator's host budgets so the
	// create path can admit leases only within them and every terminal path frees
	// back. A zero budget on any dimension leaves it unbudgeted (never checked).
	br.cap = capacity.NewAllocator(pol.Limits.MaxContracts, pol.Limits.TotalCPUs, pol.Limits.TotalMemoryMB)
	return br
}

// gpuDeviceIDs extracts the stable device IDs from the effective GPU posture's
// devices, in detection order, to seed the allocator's inventory.
func gpuDeviceIDs(devices []policy.GPUDevice) []string {
	ids := make([]string, 0, len(devices))
	for _, d := range devices {
		ids = append(ids, d.ID)
	}
	return ids
}

// detectIsolation queries the daemon for its registered runtimes and container OS,
// computes the isolation levels the host can actually provide, and intersects them
// with the operator's allow-list to get the effective posture the create path
// enforces and the read surface advertises. Any policy-allowed level the host
// cannot run is dropped and logged as a startup WARNING. When NONE of the
// configured levels are runnable and the allow-list explicitly excluded shared,
// the host is treated as degraded — it logs an ERROR and advertises an empty
// isolation set rather than silently downgrading to the weakest tier. Detection
// is best-effort: if the daemon info call fails, it falls back to the levels
// derivable from the already-detected container OS (shared always, plus vm on a
// windows daemon) and warns.
func (b *broker) detectIsolation(ctx context.Context) policy.IsolationCapabilities {
	info, err := b.rt.DaemonInfo(ctx)
	caps := policy.HostCapabilities{Runtimes: info.Runtimes, OS: string(info.OS)}
	if err != nil {
		// Fall back to the cached container OS so at least the OS-derived levels
		// (shared, and vm on a windows daemon) remain available when the daemon
		// info query fails.
		caps = policy.HostCapabilities{OS: string(b.rt.ContainerOS())}
		b.logger.Warn("isolation capability detection: daemon info unavailable, using conservative OS-derived defaults", "error", err)
	}
	supported := policy.SupportedIsolations(caps)
	ic, dropped := b.pol.EffectiveIsolation(supported)
	for _, lvl := range dropped {
		b.logger.Warn("isolation level allowed by policy but not available on this host; dropping it from the accepted set", "isolation", lvl.String())
	}
	if len(dropped) > 0 && len(ic.Levels()) == 0 {
		// All configured isolation levels are unavailable on this host and the
		// allow-list explicitly excluded shared. Refuse to silently downgrade to
		// the weakest tier — that is the opposite of the operator's security
		// intent. Advertise an empty isolation set (host degraded) so no images
		// are offered under a weaker tier than the operator authorised.
		b.logger.Error("configured isolation levels unavailable on this host; host will advertise no isolation for images (refusing silent downgrade to shared)",
			"configured", b.pol.Limits.Isolations,
			"unavailable", dropped)
		return ic
	}
	if configured := b.pol.DefaultIsolation(); ic.Default() != configured {
		b.logger.Warn("default isolation level not available on this host; using effective default",
			"configured_default", configured.String(), "effective_default", ic.Default().String())
	}
	return ic
}

// detectGPU computes the host's effective GPU posture once at startup, the same
// data-driven way detectIsolation computes the isolation posture. GPU support has
// two halves: the daemon advertising the NVIDIA runtime (from DaemonInfo) and
// nvidia-smi enumerating at least one device (from Runtime.GPUs). Only when the
// daemon reports the NVIDIA runtime does it bother enumerating — a GPU-less host
// has no nvidia-smi, so a needless shell-out and scary log line are avoided; the
// policy layer still re-derives support authoritatively from the runtimes+devices
// data (see policy.EffectiveGPU). Detection is best-effort: any failure (daemon
// info unavailable, enumeration erroring) degrades to supported=false with a
// startup log line, never an error. Capacity the operator withheld (leasing
// disabled, or a cap below the device count) is logged, mirroring
// EffectiveIsolation's dropped-levels warning.
func (b *broker) detectGPU(ctx context.Context) policy.GPUCapabilities {
	info, err := b.rt.DaemonInfo(ctx)
	if err != nil {
		// Without daemon info the NVIDIA runtime cannot be confirmed; degrade to no
		// GPU support rather than probing hardware the daemon may not even expose.
		b.logger.Warn("gpu detection: daemon info unavailable; advertising no GPU support", "error", err)
	}
	var devices []policy.GPUDevice
	if err == nil && hasRuntime(info.Runtimes, runtime.NVIDIARuntimeName) {
		rtDevices, gerr := b.rt.GPUs(ctx)
		if gerr != nil {
			b.logger.Warn("gpu detection: nvidia-smi enumeration failed; advertising no GPU support", "error", gerr)
		}
		for _, d := range rtDevices {
			devices = append(devices, policy.GPUDevice{ID: d.ID, Class: d.Class, VRAMMB: d.VRAMMB})
		}
	}

	caps, drop := b.pol.EffectiveGPU(policy.GPUHostCapabilities{Runtimes: info.Runtimes, Devices: devices})
	if drop.Disabled {
		b.logger.Warn("GPU leasing disabled by operator policy though the host has usable GPUs; advertising no GPU support", "detected_devices", drop.Detected)
	}
	if drop.Capped {
		b.logger.Warn("operator GPU cap is below the detected device count; capping per-lease GPUs", "detected_devices", drop.Detected, "max_gpus", drop.Cap)
	}
	if caps.Supported() {
		b.logger.Info("GPU support enabled", "devices", drop.Detected, "max_gpus", caps.MaxGPUs())
	} else {
		b.logger.Info("GPU support not available on this host", "detected_devices", drop.Detected)
	}
	return caps
}

// hasRuntime reports whether name is in the daemon's registered runtime list. It
// is the local equivalent of the plain-string runtime-name match the policy layer
// does; the broker uses it only to decide whether to bother enumerating GPUs.
func hasRuntime(runtimes []string, name string) bool {
	for _, r := range runtimes {
		if r == name {
			return true
		}
	}
	return false
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
	// The list endpoint is app-token gated (like create): it enumerates every
	// live contract with its current bearer token so the local wisp-agent can
	// rebuild its lease map after a restart, so it must never be reachable to
	// an unauthenticated caller (see docs/DESIGN.md §8).
	mux.HandleFunc("GET /contracts", b.requireAppToken(b.list))
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

// authorizedForContract reports whether r may act on a per-contract endpoint
// (today: GET /contracts/{id} and DELETE /contracts/{id}) — the app token OR
// the contract's own bearer token authorizes it, letting the local agent drive
// any lease it created while a satellite that only holds a contract token can
// still read/release its own. When the configured app token is empty the app
// gate is disabled (localhost-friendly default, see docs/DESIGN.md §8) and any
// request passes; otherwise a mismatched Authorization header is rejected with
// 401. Both comparisons are constant-time (see bearerMatches).
func (b *broker) authorizedForContract(r *http.Request, c contract.Contract) bool {
	if b.appToken == "" {
		return true
	}
	if bearerMatches(r, b.appToken) {
		return true
	}
	if c.Token != "" && bearerMatches(r, c.Token) {
		return true
	}
	return false
}

// resourcesRequest is the optional per-request resource shaping. Each value is
// clamped down to the operator's configured maximum when that maximum is set; an
// unset (zero) value inherits that maximum so a lease that omits a dimension is
// still bounded by the ceiling. When no maximum is configured for a dimension, a
// zero value means "no request" and the runtime imposes no cap for it.
type resourcesRequest struct {
	CPUs     float64 `json:"cpus"`
	MemoryMB int     `json:"memory_mb"`
	Pids     int     `json:"pids"`

	// Gpus is the count of whole, exclusively-assigned GPUs the lease requests
	// (default 0). Unlike the other dimensions it is REJECTED, never clamped, when
	// it exceeds what the host allows: gpus is billing-meaningful upstream, so
	// silently reducing it would misprice a lease. Wisp turns this count into a set
	// of specific device IDs via the exclusive allocator (see create).
	Gpus int `json:"gpus"`
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

	// Isolation is the optional, ordered isolation level for the lease (see
	// policy.Isolation): "shared" (runc, the safe baseline), "sandboxed" (gVisor),
	// or "vm" (Kata on a Linux daemon, Hyper-V on a Windows daemon). Omitted/empty
	// resolves to the host's effective default. It is validated against the host's
	// effective posture, recorded on the contract, and threaded through the
	// runtime to select the launch mechanism (see runtime.launchMechanism). A host
	// whose effective isolation set is empty (all configured tiers unrunnable and
	// shared excluded) rejects every create with a 409, whether or not this field
	// is set, rather than silently downgrading to runc.
	Isolation string `json:"isolation"`

	// Env is an optional, opaque KEY->VALUE map injected as the container's
	// environment (see docs/DESIGN.md §8). Wisp is domain-blind about its
	// contents; it just carries them into Config.Env so every exec/shell
	// inherits them. It is WRITE-ONLY: never echoed on GET status, never logged.
	Env map[string]string `json:"env"`

	// ExternalID is an optional, opaque caller-supplied identifier (e.g. an
	// upstream lease id) that wisp persists on the container as the
	// wisp.external_id label and echoes back on list/status reads so an upstream
	// agent can re-associate leases it owns after a wispd restart. It is capped
	// at maxExternalIDLen so a malicious caller cannot smuggle an unbounded
	// identifier into a container label.
	ExternalID string `json:"external_id"`
}

// Env injection limits. These cap the blast radius of the opaque env map so a
// create call cannot smuggle an unbounded environment into a container.
const (
	// maxEnvEntries caps the number of KEY=VALUE pairs a create may inject.
	maxEnvEntries = 128
	// maxEnvTotalBytes caps the total size of the injected environment,
	// summed over "KEY=VALUE" for every entry.
	maxEnvTotalBytes = 256 * 1024
)

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

	// Gpus is the whole GPU devices this contract was exclusively assigned, by
	// their stable device IDs — the wire field name the dashboard reads. Omitted
	// when the lease holds no GPUs (a GPU-less lease carries no "gpus" key).
	Gpus []string `json:"gpus,omitempty"`

	Meta map[string]any `json:"meta,omitempty"`

	// ExternalID is the opaque caller-supplied identifier attached at create
	// time (see createRequest.ExternalID); an empty string when the caller
	// supplied none. Always present so a consumer sees the same field name it
	// gets from GET /contracts.
	ExternalID string `json:"external_id"`
}

// contractInfo is one entry in the GET /contracts list response. Its field
// names and shape are a cross-repo wire contract (see docs/DESIGN.md §7 and the
// wisp-agent counterpart), so keep them exactly in lockstep with that
// specification. Only non-terminal contracts appear here (see listContracts).
type contractInfo struct {
	// ID is the contract id.
	ID string `json:"id"`

	// ExternalID is the opaque caller-supplied identifier (empty string when
	// the caller supplied none).
	ExternalID string `json:"external_id"`

	// Token is the contract's CURRENT bearer token, valid for /exec and /shell.
	// The endpoint is app-token-gated and local-only, so returning tokens to the
	// authenticated local agent is acceptable for v1 (see docs/DESIGN.md §8).
	Token string `json:"token"`

	// Status is the current lifecycle state, exactly one of
	// provisioning|ready|expiring. Terminal contracts (released|expired) are
	// excluded from the list, and the transient pre-provision state (requested)
	// is also excluded so the wire never leaks a status outside this enum (see
	// the list handler); consumers may treat those three as an exhaustive set.
	Status string `json:"status"`

	// ExpiresAt is the contract's absolute expiry as Unix seconds.
	ExpiresAt int64 `json:"expires_at"`

	// TTLSecondsRemaining is time.Until(ExpiresAt) rounded down to whole seconds,
	// clamped at zero.
	TTLSecondsRemaining int `json:"ttl_seconds_remaining"`

	// ReservedCPUs is the POST-CLAMP CPU reservation held by the lease (see
	// Contract.ReservedCPUs).
	ReservedCPUs float64 `json:"reserved_cpus"`

	// ReservedMemoryMB is the POST-CLAMP memory reservation held by the lease
	// (see Contract.ReservedMemoryMB).
	ReservedMemoryMB int `json:"reserved_memory_mb"`

	// Gpus is the whole GPU devices this contract was exclusively assigned; a
	// non-null array (empty for a GPU-less lease) so a consumer can iterate
	// unconditionally.
	Gpus []string `json:"gpus"`
}

// contractsListResponse is the GET /contracts response body — a single
// "contracts" array of contractInfo entries. The array is always present
// (empty when no non-terminal contracts exist) so a consumer can decode
// unconditionally.
type contractsListResponse struct {
	Contracts []contractInfo `json:"contracts"`
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
	// isolation is the resolved, validated isolation level for the lease (see
	// policy.Isolation). It is recorded on the contract and threaded into the
	// runtime through CreateOptions.Isolation, which the runtime maps to the
	// launch mechanism (see runtime.launchMechanism): shared/empty → runc,
	// sandboxed → gVisor, vm → Kata on Linux or Hyper-V on Windows.
	isolation policy.Isolation
	// env is the validated, KEY=VALUE-form environment injected into the
	// container's Config.Env; nil when the create carried no env.
	env []string
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

	// Fail loud on a host with no runnable isolation tier BEFORE any other
	// per-request work. detectIsolation deliberately advertises an empty effective
	// set when every configured tier is unrunnable and shared was excluded from
	// the allow-list (see the host-degraded branch there); admitting any create in
	// that state, even one that omits isolation (defaulting to the empty string
	// would silently downgrade to runc via launchMechanism("")), would undo the
	// whole point of the fail-loud posture. Refuse the create with a 409 conflict
	// that names the missing capability, matching the discovery document's
	// Isolation.Supported=[] advertisement.
	if len(b.iso.Levels()) == 0 {
		writeError(w, http.StatusConflict, "no runnable isolation tier: host advertises no isolation")
		return
	}

	// Resolve the image: an omitted image selects the policy default for the
	// daemon's container OS (the Windows base on a windows-mode host, the Linux
	// base otherwise); any other image must be in the allow-list, else it is a
	// client error (400).
	image := req.Image
	if image == "" {
		image = b.pol.DefaultImageFor(string(b.rt.ContainerOS()))
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

	// Resolve the isolation level: an omitted level selects the effective default;
	// any other level must parse, be a level Wisp supports today (confidential is
	// known but rejected), and be in the host's EFFECTIVE allowed set — the
	// operator allow-list intersected with the levels this daemon can actually run
	// (see detectIsolation) — else a client error (400), mirroring the
	// image/network validation shape. The host never accepts a level it cannot
	// launch, even if the operator allow-listed it. The empty-set (host-degraded)
	// case is already rejected above with a 409 before we reach this point.
	isolation := b.iso.Default()
	if req.Isolation != "" {
		level, err := policy.ParseIsolation(req.Isolation)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := level.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !b.iso.Allows(level) {
			writeError(w, http.StatusBadRequest, "isolation not allowed: "+string(level))
			return
		}
		isolation = level
	}

	// Validate and convert the optional env map before recording anything, so a
	// malformed env is a clean 400 with no contract created. Values are never
	// echoed in the error.
	env, err := envList(req.Env)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Cap the opaque external_id so a caller cannot smuggle an unbounded
	// identifier into a container label. Empty is fine — the caller may omit it.
	if len(req.ExternalID) > maxExternalIDLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("external_id too long (max %d bytes)", maxExternalIDLen))
		return
	}

	// The policy caps the contract: the requested TTL and resources are clamped
	// down to any configured maximum (see docs/DESIGN.md §7). Clamping happens
	// BEFORE the capacity reservation so the reserved amounts are the POST-CLAMP
	// cpus / memory actually applied to the container.
	ttl := b.pol.ClampTTL(time.Duration(req.TTLSeconds) * time.Second)
	spec := launchSpec{
		image:     image,
		cpus:      b.pol.ClampCPUs(req.Resources.CPUs),
		memoryMB:  b.pol.ClampMemoryMB(req.Resources.MemoryMB),
		pids:      b.pol.ClampPids(req.Resources.Pids),
		network:   network,
		isolation: isolation,
		env:       env,
	}

	// Reserve host capacity against the aggregate budgets FIRST, then GPU devices.
	// The reserved amounts are exactly spec.cpus / spec.memoryMB — the post-clamp
	// values applied to the container: a request that omits a dimension has already
	// inherited the per-lease max when one is configured (ClampCPUs / ClampMemoryMB),
	// or reserves 0 when that dimension has no per-lease cap, so an unbounded
	// container on an unbudgeted dimension stays uncounted (it still takes one
	// contract slot). An exhausted budget is a 409 — the ask was within the per-lease
	// caps, the HOST is full — distinct from the 400 over-asks above.
	if !b.cap.TryReserve(spec.cpus, spec.memoryMB) {
		writeError(w, http.StatusConflict, errAtCapacity)
		return
	}

	// Resolve GPUs: resources.gpus is a count of whole, exclusively-assigned GPUs.
	// Unlike cpus/memory/pids it is REJECTED, never clamped — gpus is
	// billing-meaningful upstream, so silently reducing the count would misprice a
	// lease. A positive count is rejected with a 400 when the host supports no GPUs,
	// when it exceeds the effective per-lease cap, or when GPU attach is not
	// available at the resolved isolation level (the task-1 attach map; v1: only
	// shared). Only after those checks pass does it consume real capacity: the
	// allocator hands out that many free device IDs, and an exhausted allocator (the
	// host advertises GPUs but every device is held by another live lease) maps to a
	// 409 capacity conflict, not a 400. Allocation happens after the capacity
	// reservation and immediately before the contract records the IDs, so a rejected
	// GPU request frees the capacity reservation and a rejected create never strands
	// either resource ("allocation happens last" discipline).
	gpuDeviceIDs, code, msg := b.allocateGPUs(req.Resources.Gpus, isolation)
	if code != 0 {
		// The capacity reserved just above would leak if we returned now; free it so
		// a GPU rejection strands nothing.
		b.cap.Free(spec.cpus, spec.memoryMB)
		writeError(w, code, msg)
		return
	}

	c, err := b.store.Create(contract.CreateParams{
		TTL:              ttl,
		Image:            image,
		Isolation:        string(spec.isolation),
		GPUDeviceIDs:     gpuDeviceIDs,
		ReservedCPUs:     spec.cpus,
		ReservedMemoryMB: spec.memoryMB,
		Meta:             req.Meta,
		ExternalID:       req.ExternalID,
	})
	if err != nil {
		// The only error Create returns for a positive TTL is ErrInvalidTTL,
		// already guarded above; treat anything here as a server fault. Return the
		// capacity and any devices reserved above so a store failure strands neither:
		// the contract that would own (and later free) them was never recorded.
		b.alloc.Free(gpuDeviceIDs)
		b.cap.Free(spec.cpus, spec.memoryMB)
		b.logger.Error("create contract", "error", err)
		writeError(w, http.StatusInternalServerError, "could not create contract")
		return
	}

	// contract.created announces the new lease before provisioning begins, so a
	// satellite can start watching for its contract.ready (see docs/DESIGN.md §6).
	b.publishLifecycle(eventContractCreated, c)

	c, err = b.provision(r.Context(), c, spec, req.Userdata)
	if err != nil {
		// An image whose OS does not match this host's container mode is a client
		// choice we cannot validate up front, so it surfaces here as a clear 400
		// (the requested image is not compatible) rather than an opaque 500. Wisp
		// never switches the daemon's mode to accommodate it.
		var mismatch *imageOSMismatchError
		if errors.As(err, &mismatch) {
			b.logger.Warn("image OS mismatch on create", "contract_id", c.ID, "image", spec.image, "os", mismatch.os)
			writeError(w, http.StatusBadRequest, mismatch.Error())
			return
		}
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

// allocateGPUs validates a lease's requested GPU count against the host's
// effective posture and, when valid, exclusively reserves that many device IDs
// from the allocator. It returns the assigned device IDs on success (nil for a
// zero request) with code 0; on rejection it returns a zero-length slice, the HTTP
// status to write, and a client-facing message.
//
// The requested count is REJECTED, never clamped (unlike cpus/memory/pids): gpus
// is billing-meaningful upstream, so silently reducing it would misprice a lease.
// A positive count is a 400 when the host has no GPU support, when it exceeds the
// effective per-lease cap (min of operator cap and detected devices), or when GPU
// attach is unavailable at the resolved isolation level — the task-1 policy attach
// map, which advertises only shared in v1, so a GPU request at any stronger level
// is rejected here rather than reaching the runtime's (unreachable-in-v1) vm attach
// seam. Only after those checks does it consume capacity; an exhausted allocator
// (every advertised device already held by another live lease) is a 409 conflict,
// distinct from the 400 over-ask, since the ask itself was within limits.
func (b *broker) allocateGPUs(count int, isolation policy.Isolation) (ids []string, code int, msg string) {
	if count <= 0 {
		return nil, 0, ""
	}
	if !b.gpu.Supported() {
		return nil, http.StatusBadRequest, "gpus requested but this host has no GPU support"
	}
	if count > b.gpu.MaxGPUs() {
		return nil, http.StatusBadRequest, fmt.Sprintf("gpus requested (%d) exceeds the maximum allowed (%d)", count, b.gpu.MaxGPUs())
	}
	if !b.gpu.Attach(isolation) {
		return nil, http.StatusBadRequest, fmt.Sprintf("gpus not available at isolation level %q", isolation.String())
	}
	assigned, err := b.alloc.Allocate(count)
	if err != nil {
		// The only error Allocate returns is insufficient free devices: the host
		// advertises GPU capacity but every device is currently held by another live
		// lease. That is a transient capacity conflict, not a malformed request.
		return nil, http.StatusConflict, "no GPU devices currently available"
	}
	return assigned, 0, ""
}

// images handles GET /images: the unauthenticated discovery document any
// consumer can read to learn what it may request (see docs/DESIGN.md §7).
func (b *broker) images(w http.ResponseWriter, r *http.Request) {
	os := b.rt.ContainerOS()
	writeJSON(w, http.StatusOK, imagesResponse{
		OS:      string(os),
		Images:  b.pol.Allow,
		Default: b.pol.DefaultImageFor(string(os)),
		Limits: limitsResponse{
			MaxTTLSeconds: b.pol.Limits.MaxTTLSeconds,
			MaxCPUs:       b.pol.Limits.MaxCPUs,
			MaxMemoryMB:   b.pol.Limits.MaxMemoryMB,
			PidsLimit:     b.pol.Limits.PidsLimit,
			Networks:      b.pol.Limits.Networks,
		},
		Isolation: isolationResponse{
			Supported: isolationStrings(b.iso.Levels()),
			Default:   b.iso.Default().String(),
		},
		GPU:      gpuBlock(b.gpu),
		Capacity: b.capacityBlock(),
	})
}

// capacityBlock renders the host's capacity posture as the /images "capacity"
// block, field-for-field per the wire contract (see docs/DESIGN.md §7). The
// budgets (max_contracts / total_cpus / total_memory_mb) come from the operator
// policy — 0 means unlimited. used_cpus / used_memory_mb are the LIVE aggregates
// the capacity allocator reserves across active leases. active_contracts is the
// count of non-terminal contracts in the store, the authoritative registry (it
// also covers leases rediscovered by the startup reconcile, which the capacity
// allocator now re-Reserves too); it equals the allocator's reserved-contract count
// for every lease admitted through create or rebuilt by the reconcile.
func (b *broker) capacityBlock() capacityResponse {
	active := 0
	for _, c := range b.store.List() {
		if !c.State.Terminal() {
			active++
		}
	}
	return capacityResponse{
		MaxContracts:    b.pol.Limits.MaxContracts,
		ActiveContracts: active,
		TotalCPUs:       b.pol.Limits.TotalCPUs,
		UsedCPUs:        b.cap.UsedCPUs(),
		TotalMemoryMB:   b.pol.Limits.TotalMemoryMB,
		UsedMemoryMB:    b.cap.UsedMemoryMB(),
	}
}

// gpuBlock renders the host's effective GPU posture as the /images "gpu" block,
// field-for-field per the wire contract (see docs/DESIGN.md §7). The block is
// ALWAYS present: an unsupported host advertises supported=false with an empty
// devices array and max_gpus=0. devices and isolations are always non-nil so the
// JSON carries arrays (never null). "isolations" is the levels at which GPU
// attach is available — at most ["shared"] in v1, computed as data so a future
// vm/kata backend needs no change here (see policy.gpuAttachByIsolation).
func gpuBlock(g policy.GPUCapabilities) gpuResponse {
	devices := g.Devices()
	out := gpuResponse{
		Supported:  g.Supported(),
		Devices:    make([]gpuDeviceResponse, 0, len(devices)),
		MaxGPUs:    g.MaxGPUs(),
		Isolations: make([]string, 0),
	}
	for _, d := range devices {
		out.Devices = append(out.Devices, gpuDeviceResponse{ID: d.ID, Class: d.Class, VRAMMB: d.VRAMMB})
	}
	for _, lvl := range g.Isolations() {
		out.Isolations = append(out.Isolations, lvl.String())
	}
	return out
}

// isolationStrings renders the effective isolation levels as their canonical
// lowercase names for the discovery document, always returning a non-nil slice so
// the JSON is an array (never null) even when nothing is allowed.
func isolationStrings(levels []policy.Isolation) []string {
	out := make([]string, 0, len(levels))
	for _, l := range levels {
		out = append(out, l.String())
	}
	return out
}

// imagesResponse is the GET /images discovery document.
type imagesResponse struct {
	// OS is the daemon's detected container OS mode ("linux" or "windows") so a
	// consumer knows what this host serves; the daemon is fixed in one mode and
	// Wisp never switches it (see docs/DESIGN.md §7).
	OS      string         `json:"os"`
	Images  []string       `json:"images"`
	Default string         `json:"default"`
	Limits  limitsResponse `json:"limits"`

	// Isolation advertises the host's EFFECTIVE isolation posture — the levels it
	// will actually accept (operator allow-list intersected with what the daemon
	// can run) and the default applied when a create omits one — so an agent can
	// report the host's real capabilities upward rather than the raw policy list.
	Isolation isolationResponse `json:"isolation"`

	// GPU advertises the host's EFFECTIVE GPU posture (see gpuResponse). It is the
	// surface wisp-agent scrapes to report GPU capacity upward, so its field names
	// match the wire contract exactly. The block is always present, even on a
	// GPU-less host (supported=false, devices=[], max_gpus=0).
	GPU gpuResponse `json:"gpu"`

	// Capacity advertises the host's aggregate capacity posture — the operator's
	// host budgets (max_contracts / total_cpus / total_memory_mb) and current usage
	// — so an agent can report and schedule against real host headroom rather than
	// per-lease caps alone. The block is always present (see capacityResponse).
	Capacity capacityResponse `json:"capacity"`
}

// limitsResponse mirrors policy.Limits in the discovery document's JSON shape.
type limitsResponse struct {
	MaxTTLSeconds int      `json:"max_ttl_seconds"`
	MaxCPUs       float64  `json:"max_cpus"`
	MaxMemoryMB   int      `json:"max_memory_mb"`
	PidsLimit     int      `json:"pids_limit"`
	Networks      []string `json:"networks"`
}

// isolationResponse is the isolation section of the discovery document: the
// effective set of levels this host accepts and the default level.
type isolationResponse struct {
	// Supported is the effective set of isolation levels this host will accept —
	// the operator allow-list intersected with the levels the daemon can actually
	// run — in the operator's configured order.
	Supported []string `json:"supported"`

	// Default is the isolation level applied when a create omits one.
	Default string `json:"default"`
}

// gpuResponse is the "gpu" section of the discovery document — the host's
// effective GPU posture, per the wire contract (authoritative across repos):
// whether GPUs may be leased, the enumerated devices, the effective per-lease
// cap (min of operator cap and detected count), and the isolation levels at which
// GPU attach is available (at most ["shared"] in v1).
type gpuResponse struct {
	// Supported reports whether GPUs may be leased on this host at all.
	Supported bool `json:"supported"`

	// Devices is the enumerated GPUs; an empty (but non-null) array when
	// unsupported.
	Devices []gpuDeviceResponse `json:"devices"`

	// MaxGPUs is the effective per-lease GPU cap; 0 when unsupported.
	MaxGPUs int `json:"max_gpus"`

	// Isolations lists the isolation levels at which GPU attach is available on
	// this host (a non-null array; at most ["shared"] in v1).
	Isolations []string `json:"isolations"`
}

// capacityResponse is the "capacity" section of the discovery document — the
// host's aggregate capacity posture, per the wire contract (authoritative across
// repos: wisp-agent forwards it verbatim, wisper-api consumes it). Field names
// are snake_case and exact; keep them in lockstep with the other repos, exactly
// as the "gpu" block's field names are a cross-repo wire contract. The budgets
// come from operator policy (0 = unlimited); active_contracts is the live
// non-terminal contract count. used_cpus / used_memory_mb are the live aggregates
// the capacity allocator reserves across active leases.
type capacityResponse struct {
	// MaxContracts is the operator's cap on concurrent contracts (0 = unlimited).
	MaxContracts int `json:"max_contracts"`

	// ActiveContracts is the count of non-terminal contracts currently in the
	// store.
	ActiveContracts int `json:"active_contracts"`

	// TotalCPUs is the operator's aggregate CPU budget across all contracts (0 =
	// unlimited).
	TotalCPUs float64 `json:"total_cpus"`

	// UsedCPUs is the aggregate CPU currently reserved across live contracts (the
	// capacity allocator's running total).
	UsedCPUs float64 `json:"used_cpus"`

	// TotalMemoryMB is the operator's aggregate memory budget in mebibytes across
	// all contracts (0 = unlimited).
	TotalMemoryMB int `json:"total_memory_mb"`

	// UsedMemoryMB is the aggregate memory (mebibytes) currently reserved across
	// live contracts (the capacity allocator's running total).
	UsedMemoryMB int `json:"used_memory_mb"`
}

// gpuDeviceResponse is one enumerated GPU in the discovery document, matching the
// wire contract's device shape exactly.
type gpuDeviceResponse struct {
	// ID is the GPU's globally-unique identifier ("GPU-<uuid>").
	ID string `json:"id"`

	// Class is the normalized product name (opaque to consumers).
	Class string `json:"class"`

	// VRAMMB is the device's total video memory in mebibytes.
	VRAMMB int `json:"vram_mb"`
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
		return c, b.mapOSMismatch(err)
	}

	cid, err := b.rt.Create(ctx, spec.image, createOptions(spec, c, b.rt.ContainerOS()))
	if err != nil {
		b.fail(ctx, c.ID, "")
		// wisp can't know an arbitrary image's OS up front, so it attempts the
		// create and translates the daemon's OS/platform rejection into a clear,
		// OS-aware contract error (see mapOSMismatch). It never switches modes.
		return c, b.mapOSMismatch(err)
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
		res, err := b.rt.ExecSync(ctx, cid, runtime.ShellCommand(b.rt.ContainerOS(), userdata))
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
// has the originating failure to report. On the winning terminal transition it
// also announces contract.expired on the bus with reason=provisioning_failed so
// subscribers learn about the failure through the same channel a TTL/container
// death expiry would surface it (see docs/DESIGN.md §6).
func (b *broker) fail(ctx context.Context, id, containerID string) {
	if containerID != "" {
		if err := b.rt.Kill(ctx, containerID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			b.logger.Error("kill container after failed provision", "contract_id", id, "error", err)
		}
	}
	// Free any GPU devices the failed lease was assigned so a provisioning failure
	// never strands a device as permanently occupied. Read the contract for its
	// authoritative device set (and its reserved capacity); Free is idempotent and a
	// no-op for a GPU-less lease.
	c, cerr := b.store.Get(id)
	if cerr == nil {
		b.alloc.Free(c.GPUDeviceIDs)
	}
	if _, err := b.store.UpdateState(id, contract.StateExpired); err != nil {
		b.logger.Error("mark contract expired", "contract_id", id, "error", err)
		return
	}
	// Return the lease's reserved host capacity now that the contract is
	// authoritatively expired. Gated on the successful transition above so it frees
	// exactly once even if a concurrent reaper expiry races this failure path.
	if cerr == nil {
		b.cap.Free(c.ReservedCPUs, c.ReservedMemoryMB)
	}
	// Announce the terminal transition on the bus so subscribers learn about a
	// provisioning failure through the same lifecycle channel a TTL / container
	// death expiry uses. Emitted only after the winning UpdateState above so a
	// race with the reaper does not double-emit; the reason field lets a
	// subscriber tell this apart from a natural TTL expiry (see events.go).
	b.publishExpired(id, ExpiredReasonProvisioningFailed)
}

// userdataError reports a userdata script that exited non-zero.
type userdataError struct {
	ExitCode int
	Stderr   string
}

func (e *userdataError) Error() string {
	return "userdata script failed with exit code " + strconv.Itoa(e.ExitCode)
}

// imageOSMismatchError reports that the requested image is incompatible with
// this host's container mode. os is the daemon's detected container OS; the
// message names it so the client understands why the image was rejected and that
// Wisp will not switch modes to run it.
//
// The daemon's "no matching manifest ... in the manifest list entries" rejection
// (see runtime.IsImageOSMismatch) can also stem from an ARCHITECTURE mismatch —
// e.g. an arm64-only image on an amd64 daemon — not strictly an OS mismatch, and
// Wisp does not parse the OS out of the daemon message. The wording is kept
// deliberately general ("not compatible") rather than claiming an OS-only cause,
// so it stays honest for both the OS-mode and the architecture case.
type imageOSMismatchError struct {
	os runtime.ContainerOS
}

func (e *imageOSMismatchError) Error() string {
	return fmt.Sprintf("this host is in %s container mode; the requested image is not compatible", e.os)
}

// mapOSMismatch translates a runtime error that is the Docker daemon rejecting an
// image for an OS/platform mismatch into a clear imageOSMismatchError naming this
// host's container mode; any other error (including nil) is returned unchanged.
// Wisp cannot know an arbitrary image's OS before launch, so this recognizes the
// daemon's rejection after the fact rather than pre-validating — and never
// switches the daemon's mode (see docs/DESIGN.md §7).
func (b *broker) mapOSMismatch(err error) error {
	if runtime.IsImageOSMismatch(err) {
		return &imageOSMismatchError{os: b.rt.ContainerOS()}
	}
	return err
}

// get handles GET /contracts/:id: it reports the current status and the
// seconds remaining until the TTL elapses. It requires either the app token or
// the contract's own bearer token so a local process without either credential
// cannot enumerate lease state (see authorizedForContract).
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
	if !b.authorizedForContract(r, c) {
		writeError(w, http.StatusUnauthorized, "missing or invalid app or contract token")
		return
	}
	writeJSON(w, http.StatusOK, b.statusOf(c))
}

// list handles GET /contracts: it returns every non-terminal contract with its
// current bearer token so a restarted local agent can rebuild its lease map
// (see docs/DESIGN.md §7). The endpoint is app-token-gated (see routes) and
// local-only, so surfacing tokens to the authenticated caller is acceptable
// for v1. Terminal contracts are excluded so a lease already reaped or released
// never appears in the list. StateRequested contracts are ALSO excluded: it is
// the transient pre-provision state a create call passes through between
// store.Create and the provision path's UpdateState(StateProvisioning), so an
// observer catching a contract mid-flight would otherwise see a status outside
// the documented list enum (provisioning|ready|expiring) that consumers were
// built against. A requested contract has no container id yet either, so it is
// useless to a resyncing agent — skipping it costs nothing.
func (b *broker) list(w http.ResponseWriter, r *http.Request) {
	all := b.store.List()
	now := b.now()
	out := contractsListResponse{Contracts: make([]contractInfo, 0, len(all))}
	for _, c := range all {
		if c.State.Terminal() {
			continue
		}
		// Skip the transient pre-provision state so the list never emits a
		// "requested" status the wire contract does not document.
		if c.State == contract.StateRequested {
			continue
		}
		remaining := 0
		if d := c.ExpiresAt.Sub(now); d > 0 {
			remaining = int(d / time.Second)
		}
		gpus := c.GPUDeviceIDs
		if gpus == nil {
			// Emit an empty array (never null) so the consumer can iterate
			// unconditionally.
			gpus = []string{}
		}
		out.Contracts = append(out.Contracts, contractInfo{
			ID:                  c.ID,
			ExternalID:          c.ExternalID,
			Token:               c.Token,
			Status:              string(c.State),
			ExpiresAt:           c.ExpiresAt.Unix(),
			TTLSecondsRemaining: remaining,
			ReservedCPUs:        c.ReservedCPUs,
			ReservedMemoryMB:    c.ReservedMemoryMB,
			Gpus:                gpus,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// release handles DELETE /contracts/:id: it kills the container and marks the
// contract released. Releasing an already-terminal contract is an IDEMPOTENT
// success — the contract IS released in every sense the caller cares about — so
// a DELETE against an already expired or released lease returns the normal
// success response, and a reaper that wins the terminal transition between our
// Get and our UpdateState (the tiny race window live-repro'd on 2026-08-15)
// resolves the same way rather than 500'ing. It requires either the app token
// or the contract's own bearer token so a local process without either
// credential cannot destroy a lease (see authorizedForContract).
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
	if !b.authorizedForContract(r, c) {
		writeError(w, http.StatusUnauthorized, "missing or invalid app or contract token")
		return
	}

	// A DELETE against an already-terminal contract is an idempotent no-op: the
	// contract IS released in every sense the caller cares about, and the terminal
	// transition that took it here already freed capacity and GPUs (the reaper and
	// the release path both gate their frees on winning the state-machine transition),
	// so this path must NOT free again.
	if c.State.Terminal() {
		writeJSON(w, http.StatusOK, b.statusOf(c))
		return
	}

	if c.ContainerID != "" {
		if err := b.rt.Kill(r.Context(), c.ContainerID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			b.logger.Error("kill container on release", "contract_id", c.ID, "error", err)
		}
	}

	updated, err := b.store.UpdateState(c.ID, contract.StateReleased)
	if err != nil {
		// The reaper raced us to a terminal transition between our Get above and
		// this UpdateState (the tiny window observed in the 2026-08-15 failure
		// drill). Treat it as an idempotent success — the contract IS released as
		// far as the caller is concerned — and DO NOT double-free capacity or
		// GPUs: the reaper's winning transition already freed both (its expire
		// path gates ReleaseCapacity on the winning transition and Free-releases
		// GPUs idempotently). Mirror the response back from the store so the
		// caller sees the authoritative terminal state, not our stale copy.
		var illegal *contract.IllegalTransitionError
		if errors.As(err, &illegal) {
			if cur, gerr := b.store.Get(c.ID); gerr == nil && cur.State.Terminal() {
				writeJSON(w, http.StatusOK, b.statusOf(cur))
				return
			}
		}
		b.logger.Error("mark contract released", "contract_id", c.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not release contract")
		return
	}
	// Return any exclusively-held GPU devices to the allocator. Gated on the winning
	// state-machine transition above so a released-then-reaped lease never
	// double-frees; Free is also idempotent as a belt-and-braces safety net.
	b.alloc.Free(c.GPUDeviceIDs)
	// Return the lease's reserved host capacity now that the release is
	// authoritative. The state-machine transition above serializes against a
	// concurrent reaper expiry — only one side wins — so capacity frees exactly once
	// per contract (see internal/capacity.Allocator.Free).
	b.cap.Free(c.ReservedCPUs, c.ReservedMemoryMB)
	// contract.released announces the client-initiated teardown (see
	// docs/DESIGN.md §6). The reaper announces contract.expiring / contract.expired.
	b.publishLifecycle(eventContractReleased, updated)
	writeJSON(w, http.StatusOK, b.statusOf(updated))
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
// Each exec is a fresh process: Wisp runs the command through a new OS-shell
// invocation (`/bin/sh -c` on Linux, `cmd /c` on a Windows daemon; see
// runtime.ShellCommand), so there is no shared cwd or environment between calls
// (see docs/DESIGN.md §5, "Each exec is a fresh process"). Clients that need
// state to persist across steps send a single compound command.
//
// The call requires the contract's bearer token (Authorization: Bearer <token>)
// and rejects execs against a contract that is not usable (409), unknown (404),
// or presented without a valid token (401). A contract in the expiring lead
// window is still usable: the whole point of that window is to give the client
// time to exfiltrate work, so execs are accepted through it. These checks are
// identical in both modes: the stream flag only changes how a successful
// exec's output is framed.
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

	if c.State != contract.StateReady && c.State != contract.StateExpiring {
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

	res, err := b.rt.ExecSync(r.Context(), c.ContainerID, runtime.ShellCommand(b.rt.ContainerOS(), req.Command))
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

	exitCode, err := b.rt.ExecStream(r.Context(), c.ContainerID, runtime.ShellCommand(b.rt.ContainerOS(), command), emit)
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
		Gpus:                c.GPUDeviceIDs,
		Meta:                c.Meta,
		ExternalID:          c.ExternalID,
	}
}

// keepAliveCmd returns the command Wisp runs as a container's main (PID 1)
// process, chosen for the daemon's container OS. A Wisp container is a
// persistent sandbox that clients drive via exec/shell for the contract's
// lifetime — the main process does no work, it just blocks so the container
// stays running. Without it, a bare base image's own default command (e.g.
// Alpine's /bin/sh) exits immediately and the container stops before any exec
// can attach, which surfaces as "container is not running". The Linux idle
// command (`tail -f /dev/null`) only exists on Linux, so the runtime layer owns
// the OS→command mapping (see runtime.IdleCommand); Windows containers get a
// Windows-runnable idle command instead. Both terminate on SIGTERM/`docker
// stop`, so release reaps the container promptly.
func keepAliveCmd(os runtime.ContainerOS) []string {
	return runtime.IdleCommand(os)
}

// envList converts the opaque create-time env map into Docker's []string
// "KEY=VALUE" form after light validation. A nil/empty map yields a nil slice
// so a create without env is byte-for-byte unchanged. Keys must be non-empty
// and free of '=' and NUL; the entry count and total size are capped. The
// returned slice is sorted by key for deterministic output. Validation errors
// carry a client-facing message suitable for a 400 and never include env values.
func envList(m map[string]string) ([]string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	if len(m) > maxEnvEntries {
		return nil, fmt.Errorf("env has too many entries (max %d)", maxEnvEntries)
	}
	keys := make([]string, 0, len(m))
	total := 0
	for k, v := range m {
		if k == "" {
			return nil, errors.New("env key must not be empty")
		}
		if strings.ContainsAny(k, "=\x00") {
			return nil, errors.New("env key must not contain '=' or NUL")
		}
		if strings.IndexByte(v, 0) >= 0 {
			return nil, errors.New("env value must not contain NUL")
		}
		total += len(k) + 1 + len(v) // KEY=VALUE
		keys = append(keys, k)
	}
	if total > maxEnvTotalBytes {
		return nil, fmt.Errorf("env too large (max %d bytes)", maxEnvTotalBytes)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out, nil
}

// createOptions translates a resolved launch spec into the runtime's
// CreateOptions: the resource caps, network policy, and security options
// applied to the container, plus the labels correlating it back to its
// contract. Labels written: wisp.contract carries the contract id; wisp.expires_at
// carries the contract's absolute expiry as Unix seconds so a wispd restart can
// rebuild the TTL tracking from its labels alone (omitted when the contract has no
// TTL, since there is nothing to reap against); wisp.gpus carries the exclusively
// assigned device IDs (omitted for a GPU-less lease); and wisp.cpus / wisp.memory_mb
// carry the POST-CLAMP reserved capacity so the restart's reconcile re-Reserves the
// same aggregate usage (see reconcile). Every leased container is hardened
// with no-new-privileges so a process inside it cannot escalate via setuid
// binaries. It always sets the keep-alive command — chosen for the daemon's
// container OS — so the container outlives its provisioning step. WorkingDir,
// entrypoint, and user are left unset so each OS uses its own container default
// (Linux the image default, Windows C:\ with the image's user); no POSIX-only
// default is imposed.
func createOptions(spec launchSpec, c contract.Contract, os runtime.ContainerOS) runtime.CreateOptions {
	labels := map[string]string{contractLabel: c.ID}
	// Record the absolute expiry so the startup reconcile can reap or resume this
	// container from its labels alone after a restart. Omitted for a contract with
	// no TTL (none in the current create path, which requires a positive TTL, but
	// kept defensive) since there would be nothing to reap against.
	if c.TTL > 0 && !c.ExpiresAt.IsZero() {
		labels[expiresAtLabel] = strconv.FormatInt(c.ExpiresAt.Unix(), 10)
	}
	// Record the exclusively-assigned GPU device IDs so the startup reconcile can
	// rebuild the allocator's occupancy from this container's labels alone and
	// never double-assign a device this live lease still holds (see reconcile).
	// Omitted when the lease holds no GPUs.
	if v := gpusLabelValue(c.GPUDeviceIDs); v != "" {
		labels[gpusLabel] = v
	}
	// Record the POST-CLAMP reserved cpus / memory — exactly what was reserved in
	// the capacity allocator — so the startup reconcile can re-Reserve the same
	// aggregate usage from this container's labels alone and never oversubscribe the
	// host by under-counting this live lease (see reconcile). Always written (even a
	// "0" reservation on an unbudgeted dimension) so the round-trip restores exactly
	// what create took.
	labels[cpusLabel] = strconv.FormatFloat(c.ReservedCPUs, 'f', -1, 64)
	labels[memoryMBLabel] = strconv.Itoa(c.ReservedMemoryMB)
	// Record the caller-supplied opaque identifier so a wispd restart's reconcile
	// can restore the mapping from surviving containers' labels alone. Omitted
	// when the create carried no external_id, so the container carries no such
	// label unless the caller explicitly asked for one.
	if c.ExternalID != "" {
		labels[externalIDLabel] = c.ExternalID
	}
	opts := runtime.CreateOptions{
		Labels: labels,
		// Run the keep-alive as the container's main process so it stays up for
		// the whole contract; all real work happens via exec/shell (see keepAliveCmd).
		Cmd: keepAliveCmd(os),
		// Env is set on Config.Env at container create, so every subsequent
		// docker exec (sync, streaming, and the shell PTY) inherits it.
		Env: spec.env,
		Resources: runtime.Resources{
			NanoCPUs:    int64(spec.cpus * 1e9),
			MemoryBytes: int64(spec.memoryMB) * bytesPerMB,
			PidsLimit:   int64(spec.pids),
			// Pass the exclusively-assigned GPU devices through to the runtime, which
			// maps them to its launch mechanism's attachment strategy (nvidia
			// DeviceRequests for shared/runc; see runtime.gpuAttachment). Empty for a
			// GPU-less lease, leaving no device attachment.
			GPUDeviceIDs: c.GPUDeviceIDs,
		},
		// Always harden leased containers against setuid privilege escalation.
		SecurityOpt: []string{noNewPrivilegesOpt},
		// Carry the resolved isolation level so the runtime selects the launch
		// mechanism from it (shared→runc, sandboxed→gVisor, vm→Kata/Hyper-V); see
		// runtime.launchMechanism. "shared" leaves today's behavior unchanged.
		Isolation: string(spec.isolation),
	}
	// Map the network policy to a Docker network mode. "none" disconnects the
	// container entirely; "egress" attaches it to the dedicated wisp-managed
	// egress bridge (outbound-only, no inter-container reachability), which the
	// runtime creates on demand; "open" (the default) leaves NetworkMode empty
	// so the container boots on the runtime's default bridge.
	switch spec.network {
	case policy.NetworkNone:
		opts.NetworkMode = "none"
	case policy.NetworkEgress:
		opts.NetworkMode = runtime.EgressNetworkName
	}
	return opts
}
