package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"

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

	// containerOS is the daemon's container OS mode, detected once at
	// construction (and re-detected on reconnect). The daemon never switches
	// modes at runtime, so a cached value stays correct for the runtime's life.
	containerOS ContainerOS
}

// NewDockerRuntime constructs a DockerRuntime using the ambient Docker
// environment (DOCKER_HOST etc.), negotiating the API version with the daemon.
func NewDockerRuntime() (*DockerRuntime, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("runtime: docker client: %w", err)
	}
	d := &DockerRuntime{cli: cli}
	d.detectContainerOS(context.Background())
	return d, nil
}

// NewDockerRuntimeWithClient wraps a pre-built Docker API client. It exists so
// callers (and tests that supply a stub client) can inject their own client.
func NewDockerRuntimeWithClient(cli client.APIClient) *DockerRuntime {
	d := &DockerRuntime{cli: cli}
	d.detectContainerOS(context.Background())
	return d
}

// detectContainerOS queries the daemon for its container OS mode via Info and
// caches it. Call it once at construction and again if the runtime reconnects
// its client. Detection is best-effort: if Info fails or reports an unknown
// OSType, the runtime falls back to OSLinux, the Docker default. It never
// switches the daemon's mode — the operator owns that.
func (d *DockerRuntime) detectContainerOS(ctx context.Context) {
	d.containerOS = OSLinux
	info, err := d.cli.Info(ctx)
	if err != nil {
		return
	}
	if ContainerOS(info.OSType) == OSWindows {
		d.containerOS = OSWindows
	}
}

// ContainerOS implements Runtime, returning the daemon's detected container OS
// mode ("linux" or "windows").
func (d *DockerRuntime) ContainerOS() ContainerOS {
	return d.containerOS
}

// Close releases the underlying Docker client resources.
func (d *DockerRuntime) Close() error {
	return d.cli.Close()
}

// EnsureImage implements Runtime. It inspects the image locally first and, only
// when it is absent, pulls it from its registry, draining the pull stream to
// completion so the pull has finished before returning. A local-only tag that
// inspects successfully is never pulled.
func (d *DockerRuntime) EnsureImage(ctx context.Context, ref string) error {
	if _, _, err := d.cli.ImageInspectWithRaw(ctx, ref); err == nil {
		// Already present locally; nothing to pull.
		return nil
	} else if !client.IsErrNotFound(err) {
		return fmt.Errorf("runtime: inspect image %s: %w", ref, err)
	}

	rc, err := d.cli.ImagePull(ctx, ref, types.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("runtime: pull image %s: %w", ref, err)
	}
	// Draining the reader to EOF blocks until the pull actually completes; the
	// payload is progress JSON we do not need.
	_, copyErr := io.Copy(io.Discard, rc)
	rc.Close()
	if copyErr != nil {
		return fmt.Errorf("runtime: pull image %s: %w", ref, copyErr)
	}

	// Confirm the pull produced a usable local image.
	if _, _, err := d.cli.ImageInspectWithRaw(ctx, ref); err != nil {
		return fmt.Errorf("runtime: image %s not present after pull: %w", ref, err)
	}
	return nil
}

// IdleCommand returns the no-op command Wisp runs as a leased container's main
// (PID 1) process to keep it alive for the whole contract, chosen for the
// container OS. A Wisp container does no work of its own — clients drive it via
// exec/shell — so its main process just blocks; without it a bare base image's
// default command exits and the container stops before any exec can attach.
//
// On Linux `tail -f /dev/null` blocks forever and exits on SIGTERM, so release
// or `docker stop` reaps the container promptly; the `tail` executable only
// exists on Linux. On a Windows-mode daemon that command is not runnable, so
// Windows containers instead ping the loopback forever with output discarded
// (`cmd /c ping -t localhost >NUL`), an idle process valid on the standard
// Windows base images that likewise terminates on stop. Callers pick the
// command from the daemon's detected ContainerOS (see DockerRuntime.ContainerOS)
// so a leased container's main process is always runnable on the daemon's OS.
func IdleCommand(os ContainerOS) []string {
	if os == OSWindows {
		return []string{"cmd", "/c", "ping -t localhost >NUL"}
	}
	return []string{"tail", "-f", "/dev/null"}
}

// ShellCommand wraps a command string in the container OS's shell so a client's
// single command (which may be a compound like `cd /repo && git diff`) runs
// through a shell interpreter rather than being exec'd directly. On Linux it is
// the historical `/bin/sh -c <cmd>`, byte-for-byte unchanged; a Windows-mode
// daemon has no `/bin/sh`, so the command runs through the Windows command
// processor (`cmd /c <cmd>`) instead. Callers pick the OS from the daemon's
// detected ContainerOS (see DockerRuntime.ContainerOS) so every exec is
// runnable on the daemon's OS.
func ShellCommand(os ContainerOS, cmd string) []string {
	if os == OSWindows {
		return []string{"cmd", "/c", cmd}
	}
	return []string{"/bin/sh", "-c", cmd}
}

// DefaultShell returns the default interactive shell binary launched for a
// container's PTY session, chosen for the container OS. On Linux it is
// `/bin/sh`, byte-for-byte unchanged; a Windows-mode daemon has no `/bin/sh`,
// so the interactive shell is `cmd.exe`, valid on the standard Windows base
// images. Callers pick the OS from the daemon's detected ContainerOS (see
// DockerRuntime.ContainerOS) so the interactive shell is always runnable on the
// daemon's OS. The exec is created with a TTY (see ExecShell), which Windows
// containers support alongside the hijacked attach.
func DefaultShell(os ContainerOS) []string {
	if os == OSWindows {
		return []string{"cmd.exe"}
	}
	return []string{"/bin/sh"}
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
	host := &container.HostConfig{
		NetworkMode: container.NetworkMode(opts.NetworkMode),
		Resources: container.Resources{
			NanoCPUs: opts.Resources.NanoCPUs,
			Memory:   opts.Resources.MemoryBytes,
		},
	}
	if opts.Resources.PidsLimit > 0 {
		pids := opts.Resources.PidsLimit
		host.Resources.PidsLimit = &pids
	}
	resp, err := d.cli.ContainerCreate(ctx, cfg, host, nil, nil, "")
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

// ExecStream implements Runtime. Like ExecSync it creates and attaches to an
// exec, but instead of buffering it demultiplexes the Docker stream into
// stdout/stderr chunks and forwards each to emit as it arrives, so the caller
// sees output live. It returns the exec's exit code once the command completes.
func (d *DockerRuntime) ExecStream(ctx context.Context, id string, cmd []string, emit func(ExecChunk) error) (int, error) {
	execID, err := d.cli.ContainerExecCreate(ctx, id, types.ExecConfig{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return 0, fmt.Errorf("runtime: exec create on %s: %w", id, err)
	}

	attach, err := d.cli.ContainerExecAttach(ctx, execID.ID, types.ExecStartCheck{})
	if err != nil {
		return 0, fmt.Errorf("runtime: exec attach on %s: %w", id, err)
	}
	defer attach.Close()

	stdout := &chunkWriter{stream: StreamStdout, emit: emit}
	stderr := &chunkWriter{stream: StreamStderr, emit: emit}
	// Docker multiplexes stdout and stderr onto one stream; stdcopy splits it,
	// writing each frame's payload to the matching writer as it is read. The
	// chunkWriters turn those writes into emitted chunks.
	if _, err := stdcopy.StdCopy(stdout, stderr, attach.Reader); err != nil {
		return 0, fmt.Errorf("runtime: exec read on %s: %w", id, err)
	}
	if stdout.err != nil {
		return 0, stdout.err
	}
	if stderr.err != nil {
		return 0, stderr.err
	}

	inspect, err := d.cli.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		return 0, fmt.Errorf("runtime: exec inspect on %s: %w", id, err)
	}
	return inspect.ExitCode, nil
}

// ExecShell implements Runtime. It creates an exec with a TTY and stdin/stdout
// attached, then hijacks the connection to obtain a raw duplex stream. The
// returned io.ReadWriteCloser reads the shell's TTY output and writes to its
// stdin; closing it detaches from the exec. With Tty:true Docker does not
// multiplex the streams, so the reader yields the terminal bytes verbatim.
func (d *DockerRuntime) ExecShell(ctx context.Context, id string, cmd []string) (ShellStream, error) {
	execID, err := d.cli.ContainerExecCreate(ctx, id, types.ExecConfig{
		Cmd:          cmd,
		Tty:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("runtime: exec create on %s: %w", id, err)
	}

	attach, err := d.cli.ContainerExecAttach(ctx, execID.ID, types.ExecStartCheck{Tty: true})
	if err != nil {
		return nil, fmt.Errorf("runtime: exec attach on %s: %w", id, err)
	}
	return &hijackStream{cli: d.cli, execID: execID.ID, attach: attach}, nil
}

// hijackStream adapts a Docker HijackedResponse into a ShellStream. The hijacked
// connection is full duplex: reads come from the multiplexed reader (raw TTY
// bytes, since the exec has a TTY) and writes go to the underlying conn (the
// shell's stdin). Resize issues an out-of-band ContainerExecResize for the exec
// so the pseudo-terminal tracks the client's window size. Close tears down both
// directions.
type hijackStream struct {
	cli    client.APIClient
	execID string
	attach types.HijackedResponse
}

func (h *hijackStream) Read(p []byte) (int, error)  { return h.attach.Reader.Read(p) }
func (h *hijackStream) Write(p []byte) (int, error) { return h.attach.Conn.Write(p) }

// Resize forwards a TTY window-size change to the exec. It is a separate Engine
// API call from the hijacked byte stream, so it can run while reads and writes
// are in flight.
func (h *hijackStream) Resize(ctx context.Context, rows, cols uint16) error {
	if err := h.cli.ContainerExecResize(ctx, h.execID, container.ResizeOptions{
		Height: uint(rows),
		Width:  uint(cols),
	}); err != nil {
		return fmt.Errorf("runtime: exec resize on %s: %w", h.execID, err)
	}
	return nil
}

func (h *hijackStream) Close() error {
	// CloseWrite signals stdin EOF where supported; Close tears down the conn.
	h.attach.CloseWrite()
	h.attach.Close()
	return nil
}

// chunkWriter is an io.Writer that turns each Write into an emitted ExecChunk.
// stdcopy reuses its read buffer across frames, so each Write copies its bytes
// before handing them off. The first emit error is captured in err and returned
// to stdcopy to halt the copy.
type chunkWriter struct {
	stream string
	emit   func(ExecChunk) error
	err    error
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	buf := make([]byte, len(p))
	copy(buf, p)
	if err := w.emit(ExecChunk{Stream: w.stream, Data: buf}); err != nil {
		w.err = err
		return 0, err
	}
	return len(p), nil
}
