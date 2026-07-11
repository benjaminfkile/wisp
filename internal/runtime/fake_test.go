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
