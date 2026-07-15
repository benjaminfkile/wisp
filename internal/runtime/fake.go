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

	// CreateErr, StartErr, KillErr, EnsureImageErr, when set, are returned by
	// the corresponding method to let tests exercise error paths.
	CreateErr      error
	StartErr       error
	KillErr        error
	EnsureImageErr error

	// OS is the container OS mode reported by ContainerOS. The zero value ("")
	// is treated as OSLinux, so a freshly constructed Fake defaults to linux;
	// tests set it to OSWindows to exercise Windows-container paths.
	OS ContainerOS
}

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
	f.containers[id] = &FakeContainer{ID: id, Image: image, Opts: opts}
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
