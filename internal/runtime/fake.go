package runtime

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// FakeContainer records the observable state of a container in the Fake
// runtime. Tests inspect these to assert on lifecycle transitions.
type FakeContainer struct {
	ID      string
	Image   string
	Opts    CreateOptions
	Started bool
	Killed  bool
	// Runtime and Isolation are the Docker launch-mechanism values the isolation
	// level in Opts resolves to for this fake's OS (see launchMechanism), so a
	// test can assert the level→mechanism mapping without a real daemon. At most
	// one is non-empty: they are never both set.
	Runtime   string
	Isolation string
	// Execs is the ordered list of commands run against this container via
	// ExecSync or ExecStream.
	Execs [][]string
	// Resizes is the ordered list of {rows, cols} TTY dimensions forwarded to
	// the interactive shell via ShellStream.Resize, so tests can assert that a
	// client's resize control reached the runtime.
	Resizes [][2]uint16
}

// ExecFunc lets a test define how the Fake responds to an exec. It receives the
// target container id and command and returns the synthetic result. When nil,
// the Fake returns an empty, exit-code-0 result.
type ExecFunc func(id string, cmd []string) (ExecResult, error)

// StreamFunc lets a test define how the Fake responds to a streaming exec. It
// receives the target container id and command plus an emit callback to push
// output chunks, and returns the process exit code. When nil, ExecStream emits
// nothing and returns exit code 0.
type StreamFunc func(id string, cmd []string, emit func(ExecChunk) error) (int, error)

// ShellFunc lets a test define how the Fake responds to an interactive shell
// request. It receives the target container id and command and returns the
// duplex stream that stands in for the container's hijacked TTY. When nil,
// ExecShell returns an ErrNotFound-free empty stream that reports EOF on read
// and discards writes.
type ShellFunc func(id string, cmd []string) (io.ReadWriteCloser, error)

// Fake is an in-memory Runtime for tests. It never talks to a Docker daemon.
// The zero value is ready to use; it is safe for concurrent use.
type Fake struct {
	mu         sync.Mutex
	seq        int
	containers map[string]*FakeContainer

	// ensured records, in call order, every ref passed to EnsureImage so tests
	// can assert the pull-on-demand step ran with the expected image.
	ensured []string

	// ExecHandler, when set, produces exec results. Set it before use; it is
	// read under the Fake's lock so tests should not mutate it concurrently
	// with runtime calls.
	ExecHandler ExecFunc

	// StreamHandler, when set, produces streaming exec output. Set it before
	// use; it is read under the Fake's lock so tests should not mutate it
	// concurrently with runtime calls.
	StreamHandler StreamFunc

	// ShellHandler, when set, produces the duplex stream for an interactive
	// shell. Set it before use; it is read under the Fake's lock so tests
	// should not mutate it concurrently with runtime calls.
	ShellHandler ShellFunc

	// CreateErr, StartErr, KillErr, EnsureImageErr, ListLeasedErr, InspectErr,
	// when set, are returned by the corresponding method to let tests exercise
	// error paths.
	CreateErr      error
	StartErr       error
	KillErr        error
	EnsureImageErr error
	ListLeasedErr  error
	InspectErr     error

	// InspectOverrides lets a test force Inspect to report a specific
	// LivenessState for a given container id, regardless of the container's
	// tracked Started/Killed flags. It is the seam for the "container died out
	// of band" scenario a Fake otherwise can't reach - the reaper's liveness
	// sweep needs the runtime to report a container Wisp still thinks is alive
	// as stopped or gone, so a test sets that mapping here. An entry mapping to
	// LivenessGone stands in for a docker-rm.
	InspectOverrides map[string]LivenessState

	// LeasedContainers, when non-nil, is returned verbatim by ListLeased so
	// tests can simulate containers that pre-exist a restart (labels and ids of
	// their choosing) without going through Create. When nil, ListLeased derives
	// the result from the currently tracked containers carrying ContractLabel.
	LeasedContainers []LeasedContainer

	// OS is the container OS mode reported by ContainerOS. The zero value ("")
	// is treated as OSLinux, so a freshly constructed Fake defaults to linux;
	// tests set it to OSWindows to exercise Windows-container paths.
	OS ContainerOS

	// Runtimes is the set of registered OCI runtime names reported by DaemonInfo.
	// A nil value defaults to a permissive set (runc plus the gVisor and Kata
	// runtimes) so a test that does not care about isolation capability still sees
	// every level as available; capability tests set it explicitly (e.g.
	// []string{"runc"}) to model a host that can only run the shared baseline.
	Runtimes []string

	// DaemonInfoErr, when set, is returned by DaemonInfo so tests can exercise the
	// daemon-unreachable path and the caller's conservative fallback.
	DaemonInfoErr error

	// GPUDevices is the set of GPUs reported by GPUs. It is nil by default, so a
	// freshly constructed Fake models a GPU-less host (combined with the default
	// runtimes, which do not include NVIDIARuntimeName); GPU-capability tests set
	// it to model enumerated hardware.
	GPUDevices []GPUDevice

	// GPUErr, when set, is returned by GPUs so tests can drive the
	// enumeration-failure path (nvidia-smi absent or garbage), which the caller
	// degrades to "no GPU support".
	GPUErr error
}

// defaultFakeRuntimes is the permissive runtime set DaemonInfo reports when the
// Fake's Runtimes field is left nil: runc (the shared baseline), the gVisor
// runtime backing sandboxed, and the Kata runtime backing vm. It keeps tests that
// don't care about capability seeing every isolation level as available.
var defaultFakeRuntimes = []string{"runc", gVisorRuntimeName, kataRuntimeName}

// NewFake returns an initialized Fake runtime. It reports OSLinux by default;
// set OS to OSWindows to have it report a Windows daemon.
func NewFake() *Fake {
	return &Fake{containers: make(map[string]*FakeContainer), OS: OSLinux}
}

// ContainerOS implements Runtime, reporting the configured OS. An unset (empty)
// OS is reported as OSLinux so the zero-value Fake defaults to linux.
func (f *Fake) ContainerOS() ContainerOS {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.OS == "" {
		return OSLinux
	}
	return f.OS
}

// DaemonInfo implements Runtime, reporting the Fake's configured runtimes and OS.
// A nil Runtimes field defaults to the permissive defaultFakeRuntimes so a test
// that ignores isolation capability still sees every level as available; an unset
// OS reports linux, matching ContainerOS. When DaemonInfoErr is set it is
// returned instead, letting a test drive the daemon-unreachable fallback.
func (f *Fake) DaemonInfo(ctx context.Context) (DaemonInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.DaemonInfoErr != nil {
		return DaemonInfo{}, f.DaemonInfoErr
	}
	os := f.OS
	if os == "" {
		os = OSLinux
	}
	runtimes := f.Runtimes
	if runtimes == nil {
		runtimes = defaultFakeRuntimes
	}
	out := make([]string, len(runtimes))
	copy(out, runtimes)
	return DaemonInfo{Runtimes: out, OS: os}, nil
}

// GPUs implements Runtime, reporting the Fake's configured GPUDevices (or GPUErr
// when set). It never shells out - tests model an enumerated GPU host by setting
// GPUDevices and a GPU-less or broken host by leaving it nil or setting GPUErr.
func (f *Fake) GPUs(ctx context.Context) ([]GPUDevice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.GPUErr != nil {
		return nil, f.GPUErr
	}
	out := make([]GPUDevice, len(f.GPUDevices))
	copy(out, f.GPUDevices)
	return out, nil
}

// lazyInit ensures the container map exists so the zero value is usable.
func (f *Fake) lazyInit() {
	if f.containers == nil {
		f.containers = make(map[string]*FakeContainer)
	}
}

// EnsureImage implements Runtime. The Fake has no daemon, so it never pulls; it
// simply records the ref (in call order) and returns nil, letting tests assert
// that provisioning invoked pull-on-demand with the expected image.
func (f *Fake) EnsureImage(ctx context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured = append(f.ensured, ref)
	return f.EnsureImageErr
}

// Ensured returns a copy of the refs passed to EnsureImage, in call order.
func (f *Fake) Ensured() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.ensured))
	copy(out, f.ensured)
	return out
}

// Create implements Runtime.
func (f *Fake) Create(ctx context.Context, image string, opts CreateOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CreateErr != nil {
		return "", f.CreateErr
	}
	f.lazyInit()
	f.seq++
	id := fmt.Sprintf("fake-%d", f.seq)
	// Resolve the launch mechanism the same way the real runtime does so tests
	// can assert the isolation level→Runtime/Isolation mapping. The OS mirrors
	// ContainerOS's default (an empty OS is treated as linux); read directly here
	// because we already hold the lock. The Kata runtime name is picked from the
	// Fake's Runtimes (mirroring what the real DockerRuntime caches from
	// Info.Runtimes at construction), so a test that models a daemon registering
	// Kata only as the short alias "kata" sees the vm launch name "kata"
	// verbatim, matching the exact behavior the real DockerRuntime now has.
	os := f.OS
	if os == "" {
		os = OSLinux
	}
	runtimes := f.Runtimes
	if runtimes == nil {
		runtimes = defaultFakeRuntimes
	}
	rt, iso := launchMechanism(opts.Isolation, os, pickKataRuntimeName(runtimes))
	f.containers[id] = &FakeContainer{ID: id, Image: image, Opts: opts, Runtime: rt, Isolation: iso}
	return id, nil
}

// Start implements Runtime.
func (f *Fake) Start(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StartErr != nil {
		return f.StartErr
	}
	c, ok := f.containers[id]
	if !ok {
		return ErrNotFound
	}
	c.Started = true
	return nil
}

// Inspect implements Runtime. It reports LivenessRunning for a tracked
// container that has been Started (and not Killed), LivenessStopped for a
// tracked-but-not-started container, and LivenessGone for an unknown id. Tests
// can override the reported state per id via InspectOverrides - the seam that
// stands in for a container that died out of band (docker kill / rm) without
// the Fake having to model daemon-side death itself. InspectErr, when set,
// takes precedence and is returned so callers can exercise the transport-error
// path (e.g. a daemon-unreachable blip).
func (f *Fake) Inspect(ctx context.Context, id string) (LivenessState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.InspectErr != nil {
		return LivenessRunning, f.InspectErr
	}
	if s, ok := f.InspectOverrides[id]; ok {
		return s, nil
	}
	c, ok := f.containers[id]
	if !ok {
		return LivenessGone, nil
	}
	if c.Started && !c.Killed {
		return LivenessRunning, nil
	}
	return LivenessStopped, nil
}

// Kill implements Runtime. After Kill the id is removed and no longer valid.
func (f *Fake) Kill(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.KillErr != nil {
		return f.KillErr
	}
	c, ok := f.containers[id]
	if !ok {
		return ErrNotFound
	}
	c.Killed = true
	delete(f.containers, id)
	return nil
}

// ListLeased implements Runtime. When LeasedContainers is set it returns that
// injected list (letting a test model containers left over from a prior daemon
// run); otherwise it derives the list from the tracked containers that carry
// ContractLabel, mirroring the real runtime's label filter.
func (f *Fake) ListLeased(ctx context.Context) ([]LeasedContainer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ListLeasedErr != nil {
		return nil, f.ListLeasedErr
	}
	if f.LeasedContainers != nil {
		out := make([]LeasedContainer, len(f.LeasedContainers))
		copy(out, f.LeasedContainers)
		return out, nil
	}
	out := make([]LeasedContainer, 0, len(f.containers))
	for _, c := range f.containers {
		if _, ok := c.Opts.Labels[ContractLabel]; ok {
			out = append(out, LeasedContainer{ID: c.ID, Labels: c.Opts.Labels, Running: c.Started && !c.Killed})
		}
	}
	return out, nil
}

// ExecSync implements Runtime. The container must exist and be running
// (started, not killed). The configured ExecHandler produces the result.
func (f *Fake) ExecSync(ctx context.Context, id string, cmd []string) (ExecResult, error) {
	f.mu.Lock()
	c, ok := f.containers[id]
	if !ok {
		f.mu.Unlock()
		return ExecResult{}, ErrNotFound
	}
	if !c.Started || c.Killed {
		f.mu.Unlock()
		return ExecResult{}, ErrNotRunning
	}
	c.Execs = append(c.Execs, cmd)
	handler := f.ExecHandler
	f.mu.Unlock()

	if handler == nil {
		return ExecResult{}, nil
	}
	return handler(id, cmd)
}

// ExecStream implements Runtime. The container must exist and be running
// (started, not killed). The configured StreamHandler drives the emitted
// chunks; with no handler it emits nothing and reports exit code 0.
func (f *Fake) ExecStream(ctx context.Context, id string, cmd []string, emit func(ExecChunk) error) (int, error) {
	f.mu.Lock()
	c, ok := f.containers[id]
	if !ok {
		f.mu.Unlock()
		return 0, ErrNotFound
	}
	if !c.Started || c.Killed {
		f.mu.Unlock()
		return 0, ErrNotRunning
	}
	c.Execs = append(c.Execs, cmd)
	handler := f.StreamHandler
	f.mu.Unlock()

	if handler == nil {
		return 0, nil
	}
	return handler(id, cmd, emit)
}

// ExecShell implements Runtime. The container must exist and be running
// (started, not killed). The configured ShellHandler produces the duplex
// stream; with no handler it returns a stream that reports EOF on read and
// discards writes.
func (f *Fake) ExecShell(ctx context.Context, id string, cmd []string) (ShellStream, error) {
	f.mu.Lock()
	c, ok := f.containers[id]
	if !ok {
		f.mu.Unlock()
		return nil, ErrNotFound
	}
	if !c.Started || c.Killed {
		f.mu.Unlock()
		return nil, ErrNotRunning
	}
	c.Execs = append(c.Execs, cmd)
	handler := f.ShellHandler
	f.mu.Unlock()

	var inner io.ReadWriteCloser = nopStream{}
	if handler != nil {
		s, err := handler(id, cmd)
		if err != nil {
			return nil, err
		}
		inner = s
	}
	// Wrap the handler's duplex stream so the Fake can satisfy ShellStream and
	// record any forwarded resizes against the container (under the lock) for
	// test assertions. The ShellFunc contract stays a plain io.ReadWriteCloser.
	return &fakeShellStream{ReadWriteCloser: inner, onResize: func(rows, cols uint16) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if c, ok := f.containers[id]; ok {
			c.Resizes = append(c.Resizes, [2]uint16{rows, cols})
		}
	}}, nil
}

// Resizes returns a copy of the {rows, cols} dimensions forwarded to the given
// container's shell via ShellStream.Resize, in call order. It is safe to call
// concurrently with an active shell bridge.
func (f *Fake) Resizes(id string) [][2]uint16 {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[id]
	if !ok {
		return nil
	}
	out := make([][2]uint16, len(c.Resizes))
	copy(out, c.Resizes)
	return out
}

// fakeShellStream adapts a ShellFunc's io.ReadWriteCloser into a ShellStream,
// recording resizes via onResize while delegating the byte streams unchanged.
type fakeShellStream struct {
	io.ReadWriteCloser
	onResize func(rows, cols uint16)
}

func (s *fakeShellStream) Resize(ctx context.Context, rows, cols uint16) error {
	if s.onResize != nil {
		s.onResize(rows, cols)
	}
	return nil
}

// nopStream is the default duplex stream wrapped by ExecShell when no
// ShellHandler is set: reads report EOF immediately and writes are discarded.
type nopStream struct{}

func (nopStream) Read([]byte) (int, error)    { return 0, io.EOF }
func (nopStream) Write(p []byte) (int, error) { return len(p), nil }
func (nopStream) Close() error                { return nil }

// Container returns a snapshot-safe reference to a tracked container and whether
// it exists. Intended for test assertions; the returned pointer aliases the
// Fake's internal state, so read it while no runtime calls are in flight.
func (f *Fake) Container(id string) (*FakeContainer, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[id]
	return c, ok
}

// Count returns the number of live (created, not killed) containers.
func (f *Fake) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.containers)
}
