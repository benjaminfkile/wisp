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
	// default network; "none" disconnects the container from all networks.
	NetworkMode string
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
	// WebSocket; see server's shell endpoint.
	ExecShell(ctx context.Context, id string, cmd []string) (io.ReadWriteCloser, error)
}

// Ensure both implementations satisfy the interface at compile time.
var (
	_ Runtime = (*DockerRuntime)(nil)
	_ Runtime = (*Fake)(nil)
)
