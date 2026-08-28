// Package runtime abstracts the container backend behind a small interface so
// the rest of Wisp never talks to Docker directly.
//
// The abstraction has two implementations:
//
//   - DockerRuntime — the real backend over the official Docker SDK for Go
//     (github.com/docker/docker/client). It requires a running Docker daemon.
//   - Fake — an in-memory implementation for tests. It requires no daemon and
//     lets tests drive deterministic container/exec behaviour.
//
// The broker, exec endpoint, shell endpoint, reaper, and startup reconcile all
// speak to the backend through this interface.
package runtime

import (
	"context"
	"errors"
	"io"
)

// EgressNetworkName is the stable name of the dedicated wisp-managed bridge
// network backing the "egress" network policy. A container attached to it
// reaches the outside world (the network is a plain NAT bridge, not an
// "internal" one, so outbound traffic still routes out) but cannot reach other
// containers on the same bridge — inter-container communication is disabled via
// EgressICCOption. The DockerRuntime creates this network on demand and reuses
// it if it already exists (see DockerRuntime.ensureEgressNetwork); Create
// treats a CreateOptions.NetworkMode equal to this name as the signal to do so.
const EgressNetworkName = "wisp-egress"

// EgressICCOption is the Docker bridge-network option that disables
// inter-container communication on the egress network. With ICC off the daemon
// installs iptables rules that drop traffic between containers on the bridge,
// isolating leases from one another while leaving their outbound (masqueraded)
// path intact.
const EgressICCOption = "com.docker.network.bridge.enable_icc"

// ContractLabel is the Docker label Wisp writes on every leased container to
// correlate it back to the contract that owns it. ListLeased filters on its
// presence so a wispd restart can rediscover leased containers whose in-memory
// tracking was lost, rather than orphaning them.
const ContractLabel = "wisp.contract"

// ExpiresAtLabel is the Docker label carrying a contract's absolute expiry as
// Unix seconds. It is written at create time alongside ContractLabel so the
// daemon can rebuild a contract's TTL tracking from the container's labels
// alone after a restart (see ListLeased). It is omitted when a contract has no
// TTL, since there is nothing to reap against.
const ExpiresAtLabel = "wisp.expires_at"

// GPUsLabel is the Docker label carrying the comma-joined GPU device IDs a lease
// was exclusively assigned. It is written at create time alongside ContractLabel
// and ExpiresAtLabel so a wispd restart can rebuild the device allocator's
// occupancy from surviving containers' labels alone and never double-assign a
// device already held by a pre-existing lease (see ListLeased and the startup
// reconcile). It is omitted when a lease holds no GPUs.
const GPUsLabel = "wisp.gpus"

// CPUsLabel and MemoryMBLabel are the Docker labels carrying a lease's POST-CLAMP
// reserved CPU (as a fraction of host cores) and reserved memory (mebibytes) —
// exactly the amounts the aggregate capacity allocator reserved for it (see
// internal/capacity). They are written at create time alongside ContractLabel so a
// wispd restart can rebuild aggregate capacity usage from surviving containers'
// labels alone and never oversubscribe the host by under-counting a live lease
// (see ListLeased and the startup reconcile). CPUsLabel is a decimal fraction;
// MemoryMBLabel is a whole mebibyte count.
const (
	CPUsLabel     = "wisp.cpus"
	MemoryMBLabel = "wisp.memory_mb"
)

// ExternalIDLabel is the Docker label carrying a caller-supplied opaque
// identifier the create endpoint accepted (e.g. an upstream lease id). It is
// written at create time alongside ContractLabel so a wispd restart can restore
// the mapping from surviving containers' labels alone (see the startup reconcile),
// which lets an upstream agent re-associate leases it owns after a wispd restart.
// It is omitted when the create call carried no external_id.
const ExternalIDLabel = "wisp.external_id"

// ErrNotFound is returned when an operation references a container id that the
// runtime does not know about (never created, or already destroyed).
var ErrNotFound = errors.New("runtime: container not found")

// LeasedContainer is a container the runtime reports as carrying Wisp's
// contract label, returned by ListLeased so the daemon can rebuild its
// in-memory tracking after a restart. Labels is the container's full label set;
// the reconcile reads ContractLabel and ExpiresAtLabel from it.
type LeasedContainer struct {
	// ID is the runtime container id.
	ID string

	// Labels is the container's complete label map as reported by the runtime.
	Labels map[string]string

	// Running reports whether the container was in the running state when
	// ListLeased observed it. ListLeased returns containers regardless of state
	// (so the startup reconcile can also see stopped orphans and clean them up),
	// but only running ones represent live leases whose capacity must be
	// re-reserved and whose TTL is still being enforced. A stopped container
	// carrying the contract label is an artifact of a prior wispd's exit (or a
	// host reboot) and must not be adopted as if it were live.
	Running bool
}

// ErrNotRunning is returned when an exec is attempted against a container that
// exists but is not in the running state (not started, or already killed).
var ErrNotRunning = errors.New("runtime: container not running")

// LivenessState is Inspect's coarse view of a container's runtime state, boiled
// down to the three cases the reaper acts on: still doing its job, or dead in a
// way that means its backing contract should be reaped and its capacity freed.
// Discrete non-running Docker statuses (created, exited, restarting, paused,
// removing, dead) collapse into LivenessStopped — from the lease's point of view
// they all mean the container is no longer serving execs.
type LivenessState int

const (
	// LivenessRunning means the container exists and is running.
	LivenessRunning LivenessState = iota

	// LivenessStopped means the container exists but is not running (created,
	// exited, dead, restarting, paused, or removing). The reaper treats it the
	// same as LivenessGone: the backing contract is dead and must be reaped.
	LivenessStopped

	// LivenessGone means the container no longer exists — it was removed out of
	// band (docker rm, or a Docker daemon's auto-remove after exit) or was never
	// there in the first place. The reaper treats it the same as LivenessStopped.
	LivenessGone
)

// CreateOptions carries the launch configuration for a container. All fields
// are optional; the zero value creates a container from the given image with
// the image's default command.
type CreateOptions struct {
	// Cmd overrides the image's default command/entrypoint arguments. When
	// empty the image default is used.
	Cmd []string

	// Env is a list of KEY=VALUE environment variables set in the container.
	Env []string

	// WorkingDir sets the container's initial working directory.
	WorkingDir string

	// Labels are opaque key/value tags attached to the container. Wisp uses
	// these to correlate containers back to contracts on reconciliation.
	Labels map[string]string

	// Resources caps the container's CPU, memory, and process count. The zero
	// value imposes no limits.
	Resources Resources

	// NetworkMode selects the container's network. Empty uses the runtime's
	// default network; "none" disconnects the container from all networks;
	// EgressNetworkName attaches the container to the dedicated wisp-managed
	// egress bridge, which the DockerRuntime creates on demand (outbound-only,
	// inter-container communication disabled).
	NetworkMode string

	// SecurityOpt carries Docker security options applied to the container's
	// HostConfig.SecurityOpt (e.g. "no-new-privileges:true"). Empty applies no
	// extra options.
	SecurityOpt []string

	// Isolation selects HOW the container is launched — the strength of the
	// boundary between it and the host. Its values mirror the contract's
	// isolation levels: "" or "shared" leaves the launch mechanism at the
	// default OCI runtime (runc), today's behavior; "sandboxed" selects the
	// gVisor runtime; "vm" selects a lightweight VM (the Kata runtime on a Linux
	// daemon, Hyper-V isolation on a Windows daemon). The DockerRuntime maps this
	// to HostConfig.Runtime / HostConfig.Isolation in Create (see
	// launchMechanism); other runtimes may ignore it.
	Isolation string
}

// The isolation levels recognized by CreateOptions.Isolation. They mirror the
// contract-spec / policy level names (shared < sandboxed < vm) but are kept as
// plain runtime constants so the runtime package does not depend on the policy
// package; the create path passes the resolved level string through.
const (
	IsolationShared    = "shared"
	IsolationSandboxed = "sandboxed"
	IsolationVM        = "vm"
)

// The Docker launch-mechanism names an isolation level maps to. These are the
// standard defaults for each backend and are not configurable: gVisor registers
// its OCI runtime as "runsc", Kata Containers as "kata-runtime", and Hyper-V
// isolation is selected via the container's Isolation value "hyperv" rather than
// a named runtime.
const (
	gVisorRuntimeName = "runsc"
	kataRuntimeName   = "kata-runtime"
	hyperVIsolation   = "hyperv"
)

// launchMechanism maps a requested isolation level and the daemon's container OS
// to the Docker launch mechanism: the HostConfig.Runtime name and the
// HostConfig.Isolation value. At most one is non-empty — they are NEVER both
// set. "" and "shared" yield neither (the default OCI runtime, runc, unchanged);
// "sandboxed" yields the gVisor runtime; "vm" yields the Kata runtime on a Linux
// daemon or Hyper-V isolation on a Windows daemon. Both DockerRuntime.Create and
// the Fake use it so their behavior matches and tests can assert the mapping
// against the Fake.
func launchMechanism(isolation string, os ContainerOS) (runtimeName, isolationMode string) {
	switch isolation {
	case IsolationSandboxed:
		return gVisorRuntimeName, ""
	case IsolationVM:
		if os == OSWindows {
			return "", hyperVIsolation
		}
		return kataRuntimeName, ""
	default:
		return "", ""
	}
}

// Resources caps a container's resource usage. A zero field means "no limit"
// for that dimension.
type Resources struct {
	// NanoCPUs limits CPU in units of 1e-9 CPUs (e.g. 1.5 cores = 1_500_000_000).
	NanoCPUs int64

	// MemoryBytes is the hard memory limit in bytes.
	MemoryBytes int64

	// PidsLimit is the maximum number of processes allowed in the container.
	PidsLimit int64

	// GPUDeviceIDs are the stable IDs of the whole GPUs to attach exclusively to
	// the container (see docs/DESIGN.md §7). Empty attaches no GPU — the common
	// case. Wisp owns the assignment: the allocator hands these specific IDs to the
	// create path, which passes them through here. The runtime maps a non-empty
	// list to the device-attachment strategy for its launch mechanism (see
	// gpuAttachment); for the shared/runc mechanism that is Docker DeviceRequests
	// with the nvidia driver and these explicit device IDs.
	GPUDeviceIDs []string
}

// ExecResult is the outcome of a synchronous exec: fully buffered stdout and
// stderr plus the process exit code.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// ExecChunk is one incremental piece of output from a streaming exec. Stream is
// the origin of the bytes, either "stdout" or "stderr"; Data is the bytes
// produced since the previous chunk. Data is owned by the receiver: streaming
// implementations must not retain or mutate it after emitting.
type ExecChunk struct {
	Stream string
	Data   []byte
}

// Stream names carried by ExecChunk.Stream.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
)

// ContainerOS identifies the container OS mode a Docker daemon runs in. A
// daemon is fixed in one mode (linux or windows) by the operator and never
// switches at runtime; Wisp only detects it so higher layers can adapt.
type ContainerOS string

// The container OS modes reported by the Docker daemon's Info.OSType.
const (
	OSLinux   ContainerOS = "linux"
	OSWindows ContainerOS = "windows"
)

// DaemonInfo carries the capability-relevant facts about the container backend
// the daemon exposes: the OCI runtimes it has registered (by name, e.g. "runc",
// "runsc", "kata-runtime") and its container OS mode. The startup
// isolation-capability detection reads this to decide which isolation levels the
// host can actually provide (see policy.SupportedIsolations). Unlike
// ContainerOS — a value cached once at construction — DaemonInfo is a live query
// and so can fail when the daemon is unreachable.
type DaemonInfo struct {
	// Runtimes is the set of registered OCI runtime names as reported by the
	// daemon, in no particular order (the DockerRuntime sorts them for stable
	// output). An empty slice means the daemon reported no named runtimes.
	Runtimes []string

	// OS is the daemon's container OS mode ("linux" or "windows").
	OS ContainerOS
}

// Runtime abstracts the container backend. Implementations must be safe for
// concurrent use by multiple goroutines.
type Runtime interface {
	// EnsureImage makes the image ref available locally so a subsequent Create
	// can use it. It inspects locally first: if the image is already present it
	// returns nil without pulling, so a bare local-only tag (e.g. "wisp-base")
	// that exists in no registry is never pulled and never errors. If the image
	// is absent it pulls ref from its registry and blocks until the pull
	// completes. If the pull fails and the image is still not present, it
	// returns a wrapped error. Safe for concurrent use.
	EnsureImage(ctx context.Context, ref string) error

	// Create provisions a container from image with the given options and
	// returns its id. The container is created but not started.
	Create(ctx context.Context, image string, opts CreateOptions) (string, error)

	// Start starts a previously created container. Starting an
	// already-running container is a no-op.
	Start(ctx context.Context, id string) error

	// Kill forcibly stops and removes the container. After Kill the id is no
	// longer valid.
	Kill(ctx context.Context, id string) error

	// Inspect reports whether the container is currently running, in the
	// LivenessState terms above. A container that has stopped (exited, OOM,
	// killed) reports LivenessStopped; a container that no longer exists
	// (removed out of band) reports LivenessGone. Those two are ordinary
	// observations, NOT errors, so the reaper's liveness sweep can treat them
	// uniformly. An error signals a transport-level failure (e.g. the daemon
	// is unreachable); the caller treats that as inconclusive and reads again
	// on the next tick rather than reaping a lease on a transient blip.
	Inspect(ctx context.Context, id string) (LivenessState, error)

	// ListLeased returns every container (running or stopped) the runtime knows
	// about that carries the Wisp contract label (ContractLabel), so the daemon
	// can rebuild its in-memory contract tracking after a restart and reap or
	// resume pre-existing leases rather than orphaning them. Each result carries
	// the container id and its full label set; the caller reads ContractLabel and
	// ExpiresAtLabel from the labels.
	ListLeased(ctx context.Context) ([]LeasedContainer, error)

	// ExecSync runs cmd to completion inside a running container and returns
	// its buffered stdout, stderr, and exit code. A non-zero exit code is
	// reported via ExecResult.ExitCode, not as an error.
	ExecSync(ctx context.Context, id string, cmd []string) (ExecResult, error)

	// ExecStream runs cmd inside a running container and delivers its output
	// incrementally: emit is called once per chunk as bytes are produced, so a
	// caller can watch a long-running command live. emit is invoked from a
	// single goroutine (chunks never overlap). If emit returns an error,
	// ExecStream stops and returns that error. On success it returns the
	// process exit code once the command completes; a non-zero exit code is not
	// an error.
	ExecStream(ctx context.Context, id string, cmd []string, emit func(ExecChunk) error) (int, error)

	// ExecShell starts an interactive exec with a TTY inside a running
	// container and returns its hijacked duplex byte stream: reads deliver the
	// shell's combined output, writes deliver keystrokes to the shell's stdin,
	// and Close tears the exec down. Because the exec has a TTY, the output is a
	// single raw stream (not the multiplexed stdout/stderr of ExecSync), exactly
	// what a terminal expects. The stream bridges directly onto an interactive
	// WebSocket; see server's shell endpoint. Resize adjusts the TTY window so a
	// client can forward terminal resize events.
	ExecShell(ctx context.Context, id string, cmd []string) (ShellStream, error)

	// ContainerOS reports the container OS mode of the underlying daemon
	// ("linux" or "windows"). The daemon is fixed in one mode by the operator,
	// so this is detected once and does not change while the runtime is live.
	// Server and policy layers read it to stay OS-aware.
	ContainerOS() ContainerOS

	// DaemonInfo reports the daemon's registered OCI runtimes and container OS so
	// the daemon can advertise only the isolation levels it can actually provide.
	// It queries the backend live (the DockerRuntime backs it with the Docker
	// client's Info call), so it may return an error when the daemon is
	// unreachable; callers fall back to a conservative, OS-derived capability set
	// in that case. See policy.SupportedIsolations for how the result maps to
	// isolation levels.
	DaemonInfo(ctx context.Context) (DaemonInfo, error)

	// GPUs enumerates the physical GPUs the host exposes (the DockerRuntime backs
	// it with nvidia-smi behind a CommandRunner). It is the hardware-side half of
	// GPU capability detection; the daemon-side half is the "nvidia" runtime in
	// DaemonInfo.Runtimes (see NVIDIARuntimeName). It returns an error when
	// enumeration fails (no NVIDIA tooling, or unparseable output), which callers
	// degrade to "no GPU support" rather than a fatal error; success with no GPUs
	// yields an empty slice. See policy.EffectiveGPU for how the devices combine
	// with the operator's config.
	GPUs(ctx context.Context) ([]GPUDevice, error)
}

// ShellStream is the duplex byte stream of an interactive shell exec (see
// ExecShell). Beyond the raw read/write/close of the TTY it exposes Resize so a
// caller can adjust the pseudo-terminal's window size in response to a client's
// terminal resize; rows and cols are in character cells and a zero dimension is
// left unchanged. Resize is a no-op-safe control channel: it is independent of
// the byte streams and may be called concurrently with reads and writes.
type ShellStream interface {
	io.ReadWriteCloser
	Resize(ctx context.Context, rows, cols uint16) error
}

// Ensure both implementations satisfy the interface at compile time.
var (
	_ Runtime = (*DockerRuntime)(nil)
	_ Runtime = (*Fake)(nil)
)
