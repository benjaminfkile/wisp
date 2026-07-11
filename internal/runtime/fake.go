package runtime

import (
	"context"
	"fmt"
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
	// ExecSync.
	Execs [][]string
}

// ExecFunc lets a test define how the Fake responds to an exec. It receives the
// target container id and command and returns the synthetic result. When nil,
// the Fake returns an empty, exit-code-0 result.
type ExecFunc func(id string, cmd []string) (ExecResult, error)

// Fake is an in-memory Runtime for tests. It never talks to a Docker daemon.
// The zero value is ready to use; it is safe for concurrent use.
type Fake struct {
	mu         sync.Mutex
	seq        int
	containers map[string]*FakeContainer

	// ExecHandler, when set, produces exec results. Set it before use; it is
	// read under the Fake's lock so tests should not mutate it concurrently
	// with runtime calls.
	ExecHandler ExecFunc

	// CreateErr, StartErr, KillErr, when set, are returned by the
	// corresponding method to let tests exercise error paths.
	CreateErr error
	StartErr  error
	KillErr   error
}

// NewFake returns an initialized Fake runtime.
func NewFake() *Fake {
	return &Fake{containers: make(map[string]*FakeContainer)}
}

// lazyInit ensures the container map exists so the zero value is usable.
func (f *Fake) lazyInit() {
	if f.containers == nil {
		f.containers = make(map[string]*FakeContainer)
	}
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
