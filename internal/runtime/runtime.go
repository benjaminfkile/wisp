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
}

// ExecResult is the outcome of a synchronous exec: fully buffered stdout and
// stderr plus the process exit code.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runtime abstracts the container backend. Implementations must be safe for
// concurrent use by multiple goroutines.
type Runtime interface {
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
}

// Ensure both implementations satisfy the interface at compile time.
var (
	_ Runtime = (*DockerRuntime)(nil)
	_ Runtime = (*Fake)(nil)
)
