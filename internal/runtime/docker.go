package runtime

import (
	"bytes"
	"context"
	"fmt"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// DockerRuntime is the real Runtime backed by the Docker Engine via the
// official Docker SDK for Go. It requires a reachable Docker daemon and is
// therefore never exercised by the unit tests, which use Fake instead.
type DockerRuntime struct {
	cli client.APIClient
}

// NewDockerRuntime constructs a DockerRuntime using the ambient Docker
// environment (DOCKER_HOST etc.), negotiating the API version with the daemon.
func NewDockerRuntime() (*DockerRuntime, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("runtime: docker client: %w", err)
	}
	return &DockerRuntime{cli: cli}, nil
}

// NewDockerRuntimeWithClient wraps a pre-built Docker API client. It exists so
// callers (and tests that supply a stub client) can inject their own client.
func NewDockerRuntimeWithClient(cli client.APIClient) *DockerRuntime {
	return &DockerRuntime{cli: cli}
}

// Close releases the underlying Docker client resources.
func (d *DockerRuntime) Close() error {
	return d.cli.Close()
}

// Create implements Runtime.
func (d *DockerRuntime) Create(ctx context.Context, image string, opts CreateOptions) (string, error) {
	cfg := &container.Config{
		Image:      image,
		Cmd:        opts.Cmd,
		Env:        opts.Env,
		WorkingDir: opts.WorkingDir,
		Labels:     opts.Labels,
	}
	resp, err := d.cli.ContainerCreate(ctx, cfg, &container.HostConfig{}, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("runtime: create container: %w", err)
	}
	return resp.ID, nil
}

// Start implements Runtime.
func (d *DockerRuntime) Start(ctx context.Context, id string) error {
	if err := d.cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return fmt.Errorf("runtime: start container %s: %w", id, err)
	}
	return nil
}

// Kill implements Runtime. It force-removes the container (stopping it first),
// so the id is no longer valid afterwards.
func (d *DockerRuntime) Kill(ctx context.Context, id string) error {
	if err := d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("runtime: kill container %s: %w", id, err)
	}
	return nil
}

// ExecSync implements Runtime. It creates an exec, attaches to capture output,
// demultiplexes the Docker stdout/stderr stream, and inspects the exec for the
// exit code. A non-zero exit code is not treated as an error.
func (d *DockerRuntime) ExecSync(ctx context.Context, id string, cmd []string) (ExecResult, error) {
	execID, err := d.cli.ContainerExecCreate(ctx, id, types.ExecConfig{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return ExecResult{}, fmt.Errorf("runtime: exec create on %s: %w", id, err)
	}

	attach, err := d.cli.ContainerExecAttach(ctx, execID.ID, types.ExecStartCheck{})
	if err != nil {
		return ExecResult{}, fmt.Errorf("runtime: exec attach on %s: %w", id, err)
	}
	defer attach.Close()

	var stdout, stderr bytes.Buffer
	// Docker multiplexes stdout and stderr onto one stream; stdcopy splits it.
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attach.Reader); err != nil {
		return ExecResult{}, fmt.Errorf("runtime: exec read on %s: %w", id, err)
	}

	inspect, err := d.cli.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		return ExecResult{}, fmt.Errorf("runtime: exec inspect on %s: %w", id, err)
	}

	return ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: inspect.ExitCode,
	}, nil
}
