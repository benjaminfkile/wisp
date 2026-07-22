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
// Nothing here is wired into the HTTP surface yet; later tasks (contract model,
// exec endpoint, shell) consume this interface.
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

// ErrNotFound is returned when an operation references a container id that the
// runtime does not know about (never created, or already destroyed).
var ErrNotFound = errors.New("runtime: container not found")

// ErrNotRunning is returned when an exec is attempted against a container that
// exists but is not in the running state (not started, or already killed).
var ErrNotRunning = errors.New("runtime: container not running")

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
