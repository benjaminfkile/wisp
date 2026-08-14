package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestFakeCreateStartExecKill(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.ExecHandler = func(id string, cmd []string) (ExecResult, error) {
		return ExecResult{Stdout: "hello\n", Stderr: "", ExitCode: 0}, nil
	}

	id, err := f.Create(ctx, "debian:slim", CreateOptions{
		Cmd:    []string{"sleep", "infinity"},
		Env:    []string{"FOO=bar"},
		Labels: map[string]string{"wisp.contract": "c1"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("Create returned empty id")
	}

	c, ok := f.Container(id)
	if !ok {
		t.Fatalf("container %s not tracked", id)
	}
	if c.Image != "debian:slim" {
		t.Errorf("Image = %q, want debian:slim", c.Image)
	}
	if c.Opts.Labels["wisp.contract"] != "c1" {
		t.Errorf("label not recorded: %+v", c.Opts.Labels)
	}
	if c.Started {
		t.Error("container Started before Start called")
	}

	// Exec before Start must fail: not running.
	if _, err := f.ExecSync(ctx, id, []string{"echo", "hi"}); !errors.Is(err, ErrNotRunning) {
		t.Errorf("ExecSync before Start err = %v, want ErrNotRunning", err)
	}

	if err := f.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if c, _ := f.Container(id); !c.Started {
		t.Error("container not marked Started")
	}

	res, err := f.ExecSync(ctx, id, []string{"echo", "hello"})
	if err != nil {
		t.Fatalf("ExecSync: %v", err)
	}
	if res.Stdout != "hello\n" || res.ExitCode != 0 {
		t.Errorf("ExecResult = %+v, want stdout=hello exit=0", res)
	}
	if c, _ := f.Container(id); !reflect.DeepEqual(c.Execs, [][]string{{"echo", "hello"}}) {
		t.Errorf("recorded execs = %v", c.Execs)
	}

	if err := f.Kill(ctx, id); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if _, ok := f.Container(id); ok {
		t.Error("container still tracked after Kill")
	}
	if f.Count() != 0 {
		t.Errorf("Count = %d, want 0", f.Count())
	}
}

func TestFakeEnsureImageRecordsRefs(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	// No calls yet: nothing recorded.
	if got := f.Ensured(); len(got) != 0 {
		t.Fatalf("Ensured before any call = %v, want empty", got)
	}

	if err := f.EnsureImage(ctx, "debian:slim"); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	if err := f.EnsureImage(ctx, "alpine:3"); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}

	want := []string{"debian:slim", "alpine:3"}
	if got := f.Ensured(); !reflect.DeepEqual(got, want) {
		t.Errorf("Ensured = %v, want %v", got, want)
	}

	// Ensured returns a copy: mutating it must not affect the Fake's record.
	f.Ensured()[0] = "tampered"
	if got := f.Ensured(); !reflect.DeepEqual(got, want) {
		t.Errorf("Ensured after mutation = %v, want %v", got, want)
	}
}

func TestFakeExecStreamEmitsChunks(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.StreamHandler = func(id string, cmd []string, emit func(ExecChunk) error) (int, error) {
		if err := emit(ExecChunk{Stream: StreamStdout, Data: []byte("one\n")}); err != nil {
			return 0, err
		}
		if err := emit(ExecChunk{Stream: StreamStderr, Data: []byte("warn\n")}); err != nil {
			return 0, err
		}
		if err := emit(ExecChunk{Stream: StreamStdout, Data: []byte("two\n")}); err != nil {
			return 0, err
		}
		return 7, nil
	}
	id, _ := f.Create(ctx, "img", CreateOptions{})
	if err := f.Start(ctx, id); err != nil {
		t.Fatal(err)
	}

	var got []ExecChunk
	code, err := f.ExecStream(ctx, id, []string{"/bin/sh", "-c", "run"}, func(c ExecChunk) error {
		// Copy the data: the chunk's bytes are owned by the receiver.
		got = append(got, ExecChunk{Stream: c.Stream, Data: append([]byte(nil), c.Data...)})
		return nil
	})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	want := []ExecChunk{
		{Stream: StreamStdout, Data: []byte("one\n")},
		{Stream: StreamStderr, Data: []byte("warn\n")},
		{Stream: StreamStdout, Data: []byte("two\n")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chunks = %v, want %v", got, want)
	}

	// The streamed command was recorded like any other exec.
	c, _ := f.Container(id)
	if !reflect.DeepEqual(c.Execs, [][]string{{"/bin/sh", "-c", "run"}}) {
		t.Errorf("recorded execs = %v", c.Execs)
	}
}

func TestFakeExecStreamEmitErrorStops(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("client gone")
	f := NewFake()
	emitted := 0
	f.StreamHandler = func(id string, cmd []string, emit func(ExecChunk) error) (int, error) {
		for i := 0; i < 5; i++ {
			if err := emit(ExecChunk{Stream: StreamStdout, Data: []byte("x")}); err != nil {
				return 0, err
			}
		}
		return 0, nil
	}
	id, _ := f.Create(ctx, "img", CreateOptions{})
	_ = f.Start(ctx, id)

	_, err := f.ExecStream(ctx, id, []string{"run"}, func(c ExecChunk) error {
		emitted++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("ExecStream err = %v, want sentinel", err)
	}
	if emitted != 1 {
		t.Errorf("emit called %d times, want 1 (stop on first error)", emitted)
	}
}

func TestFakeExecStreamNilHandler(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	id, _ := f.Create(ctx, "img", CreateOptions{})
	_ = f.Start(ctx, id)
	code, err := f.ExecStream(ctx, id, []string{"true"}, func(ExecChunk) error {
		t.Fatal("emit called with nil handler")
		return nil
	})
	if err != nil || code != 0 {
		t.Errorf("ExecStream = (%d, %v), want (0, nil)", code, err)
	}
}

func TestFakeExecStreamNotRunning(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	// Unknown container.
	if _, err := f.ExecStream(ctx, "nope", []string{"x"}, func(ExecChunk) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Errorf("ExecStream unknown err = %v, want ErrNotFound", err)
	}

	// Created but not started.
	id, _ := f.Create(ctx, "img", CreateOptions{})
	if _, err := f.ExecStream(ctx, id, []string{"x"}, func(ExecChunk) error { return nil }); !errors.Is(err, ErrNotRunning) {
		t.Errorf("ExecStream before Start err = %v, want ErrNotRunning", err)
	}
}

func TestFakeExecHandlerExitCodeAndStderr(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.ExecHandler = func(id string, cmd []string) (ExecResult, error) {
		if len(cmd) > 0 && cmd[0] == "false" {
			return ExecResult{Stderr: "boom\n", ExitCode: 1}, nil
		}
		return ExecResult{Stdout: "ok\n"}, nil
	}
	id, err := f.Create(ctx, "img", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Start(ctx, id); err != nil {
		t.Fatal(err)
	}

	res, err := f.ExecSync(ctx, id, []string{"false"})
	if err != nil {
		t.Fatalf("ExecSync: %v", err)
	}
	if res.ExitCode != 1 || res.Stderr != "boom\n" {
		t.Errorf("ExecResult = %+v, want exit=1 stderr=boom", res)
	}
}

func TestFakeNilHandlerReturnsEmptyResult(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	id, _ := f.Create(ctx, "img", CreateOptions{})
	_ = f.Start(ctx, id)
	res, err := f.ExecSync(ctx, id, []string{"true"})
	if err != nil {
		t.Fatalf("ExecSync: %v", err)
	}
	if res != (ExecResult{}) {
		t.Errorf("ExecResult = %+v, want zero value", res)
	}
}

func TestFakeUnknownContainer(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	if err := f.Start(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Start unknown err = %v, want ErrNotFound", err)
	}
	if err := f.Kill(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Kill unknown err = %v, want ErrNotFound", err)
	}
	if _, err := f.ExecSync(ctx, "nope", []string{"echo"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("ExecSync unknown err = %v, want ErrNotFound", err)
	}
}

// TestFakeInspect verifies Inspect reports LivenessRunning for a started
// container, LivenessStopped for a created-but-not-started one and for a killed
// one, and LivenessGone for an unknown id. InspectOverrides forces a specific
// state regardless of the tracked flags — the seam that stands in for a
// container that died out of band without the Fake having to model daemon-side
// death. InspectErr, when set, surfaces the transport-error path.
func TestFakeInspect(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	if got, err := f.Inspect(ctx, "nope"); err != nil || got != LivenessGone {
		t.Errorf("Inspect unknown = (%v, %v), want (LivenessGone, nil)", got, err)
	}

	id, _ := f.Create(ctx, "img", CreateOptions{})
	if got, err := f.Inspect(ctx, id); err != nil || got != LivenessStopped {
		t.Errorf("Inspect created-not-started = (%v, %v), want (LivenessStopped, nil)", got, err)
	}
	if err := f.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got, err := f.Inspect(ctx, id); err != nil || got != LivenessRunning {
		t.Errorf("Inspect started = (%v, %v), want (LivenessRunning, nil)", got, err)
	}

	// InspectOverrides wins over the tracked liveness.
	f.InspectOverrides = map[string]LivenessState{id: LivenessGone}
	if got, err := f.Inspect(ctx, id); err != nil || got != LivenessGone {
		t.Errorf("Inspect with override = (%v, %v), want (LivenessGone, nil)", got, err)
	}
	f.InspectOverrides = nil

	// Killing the container removes it, so Inspect reports Gone (not Stopped).
	if err := f.Kill(ctx, id); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if got, err := f.Inspect(ctx, id); err != nil || got != LivenessGone {
		t.Errorf("Inspect after Kill = (%v, %v), want (LivenessGone, nil)", got, err)
	}

	// InspectErr takes precedence and surfaces the transport-level failure path.
	f.InspectErr = errors.New("daemon unreachable")
	if _, err := f.Inspect(ctx, "anything"); !errors.Is(err, f.InspectErr) {
		t.Errorf("Inspect with InspectErr err = %v, want daemon unreachable", err)
	}
}

func TestFakeExecAfterKill(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	id, _ := f.Create(ctx, "img", CreateOptions{})
	_ = f.Start(ctx, id)
	if err := f.Kill(ctx, id); err != nil {
		t.Fatal(err)
	}
	// Killed container is removed, so exec reports not found.
	if _, err := f.ExecSync(ctx, id, []string{"echo"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("ExecSync after Kill err = %v, want ErrNotFound", err)
	}
}

func TestFakeInjectedErrors(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("sentinel")

	f := NewFake()
	f.CreateErr = sentinel
	if _, err := f.Create(ctx, "img", CreateOptions{}); !errors.Is(err, sentinel) {
		t.Errorf("Create err = %v, want sentinel", err)
	}

	f = NewFake()
	f.StartErr = sentinel
	id, _ := f.Create(ctx, "img", CreateOptions{})
	if err := f.Start(ctx, id); !errors.Is(err, sentinel) {
		t.Errorf("Start err = %v, want sentinel", err)
	}

	f = NewFake()
	f.KillErr = sentinel
	id, _ = f.Create(ctx, "img", CreateOptions{})
	if err := f.Kill(ctx, id); !errors.Is(err, sentinel) {
		t.Errorf("Kill err = %v, want sentinel", err)
	}
}

// TestFakeZeroValueUsable verifies the zero-value Fake works without NewFake.
func TestFakeZeroValueUsable(t *testing.T) {
	ctx := context.Background()
	var f Fake
	id, err := f.Create(ctx, "img", CreateOptions{})
	if err != nil {
		t.Fatalf("Create on zero value: %v", err)
	}
	if err := f.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := f.ExecSync(ctx, id, []string{"echo"}); err != nil {
		t.Fatalf("ExecSync: %v", err)
	}
}

// TestFakeSatisfiesRuntime is a compile-time-ish check exercised at runtime that
// Fake is usable through the Runtime interface.
func TestFakeSatisfiesRuntime(t *testing.T) {
	var rt Runtime = NewFake()
	ctx := context.Background()
	id, err := rt.Create(ctx, "img", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := rt.Kill(ctx, id); err != nil {
		t.Fatal(err)
	}
}

// TestFakeContainerOS verifies the Fake defaults to linux and honors a
// configured OS, including the zero-value ("") Fake defaulting to linux.
func TestFakeContainerOS(t *testing.T) {
	if got := NewFake().ContainerOS(); got != OSLinux {
		t.Fatalf("NewFake().ContainerOS() = %q, want %q", got, OSLinux)
	}
	if got := (&Fake{}).ContainerOS(); got != OSLinux {
		t.Fatalf("zero-value Fake ContainerOS() = %q, want %q", got, OSLinux)
	}

	f := NewFake()
	f.OS = OSWindows
	if got := f.ContainerOS(); got != OSWindows {
		t.Fatalf("ContainerOS() after OS=windows = %q, want %q", got, OSWindows)
	}
}

// TestFakeDaemonInfo verifies DaemonInfo is stubbable: a nil Runtimes field
// reports the permissive default set (so capability-agnostic tests see every
// level), an explicit Runtimes value is reported verbatim, the reported OS
// follows the Fake's OS (defaulting to linux), and DaemonInfoErr surfaces the
// daemon-unreachable path.
func TestFakeDaemonInfo(t *testing.T) {
	ctx := context.Background()

	t.Run("default permissive runtimes", func(t *testing.T) {
		f := NewFake()
		got, err := f.DaemonInfo(ctx)
		if err != nil {
			t.Fatalf("DaemonInfo: %v", err)
		}
		want := []string{"runc", "runsc", "kata-runtime"}
		if !reflect.DeepEqual(got.Runtimes, want) {
			t.Errorf("Runtimes = %v, want %v", got.Runtimes, want)
		}
		if got.OS != OSLinux {
			t.Errorf("OS = %q, want linux", got.OS)
		}
		// The returned slice is a copy: mutating it must not affect the Fake.
		got.Runtimes[0] = "tampered"
		if again, _ := f.DaemonInfo(ctx); again.Runtimes[0] != "runc" {
			t.Errorf("DaemonInfo returned an aliased slice: %v", again.Runtimes)
		}
	})

	t.Run("stubbed runtimes and windows os", func(t *testing.T) {
		f := NewFake()
		f.Runtimes = []string{"runc"}
		f.OS = OSWindows
		got, err := f.DaemonInfo(ctx)
		if err != nil {
			t.Fatalf("DaemonInfo: %v", err)
		}
		if !reflect.DeepEqual(got.Runtimes, []string{"runc"}) {
			t.Errorf("Runtimes = %v, want [runc]", got.Runtimes)
		}
		if got.OS != OSWindows {
			t.Errorf("OS = %q, want windows", got.OS)
		}
	})

	t.Run("injected error", func(t *testing.T) {
		sentinel := errors.New("no daemon")
		f := NewFake()
		f.DaemonInfoErr = sentinel
		if _, err := f.DaemonInfo(ctx); !errors.Is(err, sentinel) {
			t.Errorf("DaemonInfo err = %v, want sentinel", err)
		}
	})
}

// TestFakeCreateIsolationMechanism verifies the Fake resolves the isolation
// level in CreateOptions to the same Docker launch mechanism the real runtime
// would, recording the Runtime and Isolation values on the captured container so
// tests can assert the full level→mechanism mapping without a daemon. The full
// matrix: shared (and the empty default) → neither set; sandboxed → Runtime
// "runsc"; vm on a linux daemon → Runtime "kata-runtime"; vm on a windows daemon
// → Isolation "hyperv". Runtime and Isolation are never both set.
func TestFakeCreateIsolationMechanism(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name          string
		os            ContainerOS
		isolation     string
		wantRuntime   string
		wantIsolation string
	}{
		{"shared linux", OSLinux, IsolationShared, "", ""},
		{"empty defaults to shared", OSLinux, "", "", ""},
		{"shared windows", OSWindows, IsolationShared, "", ""},
		{"sandboxed linux", OSLinux, IsolationSandboxed, "runsc", ""},
		{"sandboxed windows", OSWindows, IsolationSandboxed, "runsc", ""},
		{"vm linux", OSLinux, IsolationVM, "kata-runtime", ""},
		{"vm windows", OSWindows, IsolationVM, "", "hyperv"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewFake()
			f.OS = tt.os
			id, err := f.Create(ctx, "img", CreateOptions{Isolation: tt.isolation})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			c, ok := f.Container(id)
			if !ok {
				t.Fatalf("container %s not tracked", id)
			}
			if c.Runtime != tt.wantRuntime {
				t.Errorf("Runtime = %q, want %q", c.Runtime, tt.wantRuntime)
			}
			if c.Isolation != tt.wantIsolation {
				t.Errorf("Isolation = %q, want %q", c.Isolation, tt.wantIsolation)
			}
			if c.Runtime != "" && c.Isolation != "" {
				t.Errorf("both Runtime (%q) and Isolation (%q) set; must never both be set", c.Runtime, c.Isolation)
			}
		})
	}
}
