package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/benjaminfkile/wisp/internal/bus"
	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/policy"
	"github.com/benjaminfkile/wisp/internal/reaper"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// testServer builds a handler wired to a fresh store and fake runtime, returning
// all three so tests can drive the API and inspect backend state. It uses the
// built-in default policy (allow-list of just the bare base image, no caps).
func testServer(t *testing.T) (http.Handler, *contract.Store, *runtime.Fake) {
	t.Helper()
	store := contract.NewStore()
	fake := runtime.NewFake()
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, policy.Default(), bus.New(nil), "")
	return h, store, fake
}

// do performs an HTTP request against h and returns the recorder.
func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// doAuth performs an HTTP request with an Authorization: Bearer header.
func doAuth(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// createContract creates a ready contract and returns its create response.
func createContract(t *testing.T, h http.Handler, body string) createResponse {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/contracts", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp createResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	return resp
}

func TestExecReturnsResult(t *testing.T) {
	h, store, fake := testServer(t)
	fake.ExecHandler = func(id string, cmd []string) (runtime.ExecResult, error) {
		return runtime.ExecResult{Stdout: "hello\n", Stderr: "warn\n", ExitCode: 3}, nil
	}

	created := createContract(t, h, `{"ttl_seconds":3600}`)

	rec := doAuth(t, h, http.MethodPost, "/contracts/"+created.ContractID+"/exec",
		created.Token, `{"command":"echo hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp execResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Stdout != "hello\n" || resp.Stderr != "warn\n" || resp.ExitCode != 3 {
		t.Errorf("exec response = %+v, want {hello warn 3}", resp)
	}

	// The command was run through a fresh shell against the contract's container.
	c, _ := store.Get(created.ContractID)
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatalf("container not tracked")
	}
	last := fc.Execs[len(fc.Execs)-1]
	if len(last) != 3 || last[0] != "/bin/sh" || last[1] != "-c" || last[2] != "echo hello" {
		t.Errorf("exec cmd = %v, want [/bin/sh -c echo hello]", last)
	}
}

// A sync exec against a Windows-mode runtime must wrap the command with the
// Windows command processor (`cmd /c <cmd>`) rather than the POSIX `/bin/sh -c`,
// since a Windows container has no /bin/sh. Uses the fake runtime with a stubbed
// OS — no real Windows Docker daemon.
func TestExecWrapsCommandForWindows(t *testing.T) {
	h, store, fake := testServer(t)
	fake.OS = runtime.OSWindows

	created := createContract(t, h, `{"ttl_seconds":3600}`)

	rec := doAuth(t, h, http.MethodPost, "/contracts/"+created.ContractID+"/exec",
		created.Token, `{"command":"dir"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	c, _ := store.Get(created.ContractID)
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatalf("container not tracked")
	}
	last := fc.Execs[len(fc.Execs)-1]
	if len(last) != 3 || last[0] != "cmd" || last[1] != "/c" || last[2] != "dir" {
		t.Errorf("windows exec cmd = %v, want [cmd /c dir]", last)
	}
}

// A streaming exec against a Windows-mode runtime must wrap the command with the
// Windows command processor too, matching the sync path.
func TestExecStreamWrapsCommandForWindows(t *testing.T) {
	h, store, fake := testServer(t)
	fake.OS = runtime.OSWindows

	created := createContract(t, h, `{"ttl_seconds":3600}`)

	rec := doAuth(t, h, http.MethodPost, "/contracts/"+created.ContractID+"/exec?stream=1",
		created.Token, `{"command":"dir"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	c, _ := store.Get(created.ContractID)
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatalf("container not tracked")
	}
	last := fc.Execs[len(fc.Execs)-1]
	if len(last) != 3 || last[0] != "cmd" || last[1] != "/c" || last[2] != "dir" {
		t.Errorf("windows stream exec cmd = %v, want [cmd /c dir]", last)
	}
}

// flushRecorder is a ResponseWriter that records a snapshot of the body written
// so far on every Flush(), letting a test assert output was flushed
// incrementally rather than all at once at the end.
type flushRecorder struct {
	header    http.Header
	status    int
	buf       bytes.Buffer
	flushes   []string // body snapshot at each Flush
	failWrite bool     // when true, Write returns an error (simulates a dead client)
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{header: make(http.Header), status: http.StatusOK}
}

func (f *flushRecorder) Header() http.Header { return f.header }

func (f *flushRecorder) Write(p []byte) (int, error) {
	if f.failWrite {
		return 0, io.ErrClosedPipe
	}
	return f.buf.Write(p)
}

func (f *flushRecorder) WriteHeader(status int) { f.status = status }

func (f *flushRecorder) Flush() {
	f.flushes = append(f.flushes, f.buf.String())
}

// sseEvents splits an SSE body into (event, data) pairs.
func sseEvents(t *testing.T, body string) []struct{ Event, Data string } {
	t.Helper()
	var out []struct{ Event, Data string }
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var ev struct{ Event, Data string }
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.Data = strings.TrimPrefix(line, "data: ")
			}
		}
		out = append(out, ev)
	}
	return out
}

func TestExecStreamFlushesIncrementally(t *testing.T) {
	h, _, fake := testServer(t)
	fake.StreamHandler = func(id string, cmd []string, emit func(runtime.ExecChunk) error) (int, error) {
		if err := emit(runtime.ExecChunk{Stream: runtime.StreamStdout, Data: []byte("line one\n")}); err != nil {
			return 0, err
		}
		if err := emit(runtime.ExecChunk{Stream: runtime.StreamStderr, Data: []byte("a warning\n")}); err != nil {
			return 0, err
		}
		if err := emit(runtime.ExecChunk{Stream: runtime.StreamStdout, Data: []byte("line two\n")}); err != nil {
			return 0, err
		}
		return 5, nil
	}

	created := createContract(t, h, `{"ttl_seconds":3600}`)

	req := httptest.NewRequest(http.MethodPost, "/contracts/"+created.ContractID+"/exec?stream=1",
		strings.NewReader(`{"command":"run"}`))
	req.Header.Set("Authorization", "Bearer "+created.Token)
	rec := newFlushRecorder()
	h.ServeHTTP(rec, req)

	if rec.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.status)
	}
	if ct := rec.header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Output was flushed incrementally: the header flush plus one flush per
	// chunk plus the terminal exit flush, each snapshot strictly longer than the
	// last (the body grew between flushes rather than being emitted all at once).
	if len(rec.flushes) < 5 {
		t.Fatalf("flushes = %d, want at least 5 (header + 3 chunks + exit)", len(rec.flushes))
	}
	for i := 1; i < len(rec.flushes); i++ {
		if len(rec.flushes[i]) < len(rec.flushes[i-1]) {
			t.Errorf("flush %d snapshot shorter than previous; body did not grow monotonically", i)
		}
	}
	// The first chunk was visible to the client before the last chunk was written.
	firstChunkFlush := -1
	for i, snap := range rec.flushes {
		if strings.Contains(snap, "line one") {
			firstChunkFlush = i
			break
		}
	}
	if firstChunkFlush < 0 {
		t.Fatal("first chunk never appeared in a flush")
	}
	if strings.Contains(rec.flushes[firstChunkFlush], "line two") {
		t.Error("first and last chunks appeared in the same flush; output was not streamed incrementally")
	}

	events := sseEvents(t, rec.buf.String())
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4 (3 chunks + exit): %q", len(events), rec.buf.String())
	}
	wantChunks := []execStreamChunk{
		{Stream: "stdout", Data: "line one\n"},
		{Stream: "stderr", Data: "a warning\n"},
		{Stream: "stdout", Data: "line two\n"},
	}
	for i, want := range wantChunks {
		if events[i].Event != "chunk" {
			t.Errorf("event %d = %q, want chunk", i, events[i].Event)
		}
		var got execStreamChunk
		if err := json.Unmarshal([]byte(events[i].Data), &got); err != nil {
			t.Fatalf("decode chunk %d: %v", i, err)
		}
		if got != want {
			t.Errorf("chunk %d = %+v, want %+v", i, got, want)
		}
	}
	last := events[len(events)-1]
	if last.Event != "exit" {
		t.Errorf("final event = %q, want exit", last.Event)
	}
	var exit execStreamExit
	if err := json.Unmarshal([]byte(last.Data), &exit); err != nil {
		t.Fatalf("decode exit: %v", err)
	}
	if exit.ExitCode != 5 {
		t.Errorf("exit_code = %d, want 5", exit.ExitCode)
	}
}

func TestExecStreamRequiresToken(t *testing.T) {
	h, _, fake := testServer(t)
	called := false
	fake.StreamHandler = func(id string, cmd []string, emit func(runtime.ExecChunk) error) (int, error) {
		called = true
		return 0, nil
	}
	created := createContract(t, h, `{"ttl_seconds":3600}`)

	// Missing token: rejected before streaming begins.
	rec := do(t, h, http.MethodPost, "/contracts/"+created.ContractID+"/exec?stream=1", `{"command":"ls"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no-token stream status = %d, want 401", rec.Code)
	}
	// Bad token: also rejected.
	rec = doAuth(t, h, http.MethodPost, "/contracts/"+created.ContractID+"/exec?stream=1",
		created.Token+"x", `{"command":"ls"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad-token stream status = %d, want 401", rec.Code)
	}
	if called {
		t.Error("stream handler ran despite missing/invalid token")
	}
}

func TestExecStreamClientDisconnect(t *testing.T) {
	h, _, fake := testServer(t)
	emitCount := 0
	fake.StreamHandler = func(id string, cmd []string, emit func(runtime.ExecChunk) error) (int, error) {
		for i := 0; i < 5; i++ {
			if err := emit(runtime.ExecChunk{Stream: runtime.StreamStdout, Data: []byte("x")}); err != nil {
				return 0, err
			}
			emitCount++
		}
		return 0, nil
	}
	created := createContract(t, h, `{"ttl_seconds":3600}`)

	req := httptest.NewRequest(http.MethodPost, "/contracts/"+created.ContractID+"/exec?stream=1",
		strings.NewReader(`{"command":"run"}`))
	req.Header.Set("Authorization", "Bearer "+created.Token)
	rec := newFlushRecorder()
	rec.failWrite = true // simulate a client that has gone away
	h.ServeHTTP(rec, req)

	// The failed write propagates through emit, so the exec stops early rather
	// than draining all five chunks.
	if emitCount != 0 {
		t.Errorf("emitCount = %d, want 0 (stopped on first failed write)", emitCount)
	}
}

func TestExecStreamNotReady(t *testing.T) {
	h, store, _ := testServer(t)
	created := createContract(t, h, `{"ttl_seconds":3600}`)
	if _, err := store.UpdateState(created.ContractID, contract.StateReleased); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	rec := doAuth(t, h, http.MethodPost, "/contracts/"+created.ContractID+"/exec?stream=1",
		created.Token, `{"command":"ls"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("not-ready stream status = %d, want 409", rec.Code)
	}
}

func TestExecUnknownContract(t *testing.T) {
	h, _, _ := testServer(t)
	rec := doAuth(t, h, http.MethodPost, "/contracts/nope/exec", "anytoken", `{"command":"ls"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestExecMissingToken(t *testing.T) {
	h, _, _ := testServer(t)
	created := createContract(t, h, `{"ttl_seconds":3600}`)

	// No Authorization header at all.
	rec := do(t, h, http.MethodPost, "/contracts/"+created.ContractID+"/exec", `{"command":"ls"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing-token status = %d, want 401", rec.Code)
	}
}

func TestExecBadToken(t *testing.T) {
	h, _, _ := testServer(t)
	created := createContract(t, h, `{"ttl_seconds":3600}`)

	rec := doAuth(t, h, http.MethodPost, "/contracts/"+created.ContractID+"/exec",
		created.Token+"garbage", `{"command":"ls"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad-token status = %d, want 401", rec.Code)
	}
}

func TestExecNotReady(t *testing.T) {
	h, store, _ := testServer(t)
	created := createContract(t, h, `{"ttl_seconds":3600}`)

	// Release the contract so it is no longer ready.
	if _, err := store.UpdateState(created.ContractID, contract.StateReleased); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}

	rec := doAuth(t, h, http.MethodPost, "/contracts/"+created.ContractID+"/exec",
		created.Token, `{"command":"ls"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("not-ready status = %d, want 409", rec.Code)
	}
}

func TestExecEmptyCommand(t *testing.T) {
	h, _, _ := testServer(t)
	created := createContract(t, h, `{"ttl_seconds":3600}`)

	rec := doAuth(t, h, http.MethodPost, "/contracts/"+created.ContractID+"/exec",
		created.Token, `{"command":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty-command status = %d, want 400", rec.Code)
	}
}

func TestCreateBootsAndReturns(t *testing.T) {
	h, store, fake := testServer(t)

	rec := do(t, h, http.MethodPost, "/contracts",
		`{"ttl_seconds":3600,"userdata":"#!/bin/sh\necho hi"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp createResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ContractID == "" {
		t.Error("empty contract_id")
	}
	if resp.Token == "" {
		t.Error("empty token")
	}
	if resp.Status != string(contract.StateReady) {
		t.Errorf("status = %q, want ready", resp.Status)
	}

	// The contract is persisted, ready, and bound to a container.
	c, err := store.Get(resp.ContractID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if c.State != contract.StateReady {
		t.Errorf("stored state = %q, want ready", c.State)
	}
	if c.ContainerID == "" {
		t.Fatal("contract has no container id")
	}

	// The fake booted exactly one container, started it, and ran userdata.
	if n := fake.Count(); n != 1 {
		t.Errorf("live containers = %d, want 1", n)
	}
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatalf("container %q not tracked by fake", c.ContainerID)
	}
	if !fc.Started {
		t.Error("container not started")
	}
	if fc.Image != "wisp-base" {
		t.Errorf("image = %q, want %q", fc.Image, "wisp-base")
	}
	if fc.Opts.Labels[contractLabel] != c.ID {
		t.Errorf("label %s = %q, want %q", contractLabel, fc.Opts.Labels[contractLabel], c.ID)
	}
	// The absolute expiry is stamped as a Unix-seconds label so a wispd restart
	// can rebuild this container's TTL tracking from its labels alone.
	if got, want := fc.Opts.Labels[expiresAtLabel], strconv.FormatInt(c.ExpiresAt.Unix(), 10); got != want {
		t.Errorf("label %s = %q, want %q", expiresAtLabel, got, want)
	}
	if len(fc.Execs) != 1 {
		t.Fatalf("execs = %d, want 1 (userdata)", len(fc.Execs))
	}
	if got := fc.Execs[0]; len(got) != 3 || got[0] != "/bin/sh" || got[1] != "-c" {
		t.Errorf("userdata exec = %v, want [/bin/sh -c <script>]", got)
	}
}

func TestCreateWithEnvReachesContainerAndExec(t *testing.T) {
	h, store, fake := testServer(t)

	created := createContract(t, h,
		`{"ttl_seconds":3600,"env":{"API_KEY":"abc","FOO":"bar"}}`)

	c, _ := store.Get(created.ContractID)
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatal("container not tracked")
	}
	// The env map reached the container's Config.Env in KEY=VALUE form (sorted).
	want := []string{"API_KEY=abc", "FOO=bar"}
	if len(fc.Opts.Env) != len(want) {
		t.Fatalf("Config.Env = %v, want %v", fc.Opts.Env, want)
	}
	for i, w := range want {
		if fc.Opts.Env[i] != w {
			t.Errorf("Config.Env[%d] = %q, want %q", i, fc.Opts.Env[i], w)
		}
	}

	// docker exec inherits the container's Config.Env, so an exec after create
	// sees the same variables. The fake records what the container was created
	// with; assert the exec runs against that same container.
	rec := doAuth(t, h, http.MethodPost, "/contracts/"+created.ContractID+"/exec",
		created.Token, `{"command":"env"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("exec status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	fc, _ = fake.Container(c.ContainerID)
	if len(fc.Opts.Env) != len(want) {
		t.Errorf("Config.Env after exec = %v, want %v (exec inherits it)", fc.Opts.Env, want)
	}
	if len(fc.Execs) == 0 {
		t.Fatal("exec was not recorded against the env-carrying container")
	}
}

func TestCreateWithoutEnvLeavesConfigEnvNil(t *testing.T) {
	h, store, fake := testServer(t)

	created := createContract(t, h, `{"ttl_seconds":3600}`)
	c, _ := store.Get(created.ContractID)
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatal("container not tracked")
	}
	if fc.Opts.Env != nil {
		t.Errorf("Config.Env = %v, want nil when no env requested", fc.Opts.Env)
	}
}

func TestCreateEnvNotEchoedOnStatus(t *testing.T) {
	h, _, _ := testServer(t)
	created := createContract(t, h,
		`{"ttl_seconds":60,"env":{"SECRET":"do-not-leak"}}`)

	rec := do(t, h, http.MethodGet, "/contracts/"+created.ContractID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	// env is write-only: it must not appear anywhere in the status response.
	if body := rec.Body.String(); strings.Contains(body, "SECRET") || strings.Contains(body, "do-not-leak") || strings.Contains(body, "env") {
		t.Errorf("status response leaked env: %s", body)
	}
}

func TestCreateInvalidEnv(t *testing.T) {
	h, store, _ := testServer(t)
	bad := []string{
		`{"ttl_seconds":60,"env":{"":"v"}}`,         // empty key
		`{"ttl_seconds":60,"env":{"A=B":"v"}}`,      // '=' in key
		`{"ttl_seconds":60,"env":{"A\u0000B":"v"}}`, // NUL in key
		`{"ttl_seconds":60,"env":{"A":"v\u0000w"}}`, // NUL in value
	}
	for _, body := range bad {
		rec := do(t, h, http.MethodPost, "/contracts", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s status = %d, want 400", body, rec.Code)
		}
	}
	// Rejected env creates no contract.
	if n := len(store.List()); n != 0 {
		t.Errorf("stored contracts = %d, want 0 after rejected env", n)
	}
}

func TestGetReflectsState(t *testing.T) {
	h, _, _ := testServer(t)

	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":3600}`)
	var created createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	rec = do(t, h, http.MethodGet, "/contracts/"+created.ContractID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	var got statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ContractID != created.ContractID {
		t.Errorf("contract_id = %q, want %q", got.ContractID, created.ContractID)
	}
	if got.Status != string(contract.StateReady) {
		t.Errorf("status = %q, want ready", got.Status)
	}
	if got.TTLSecondsRemaining <= 0 || got.TTLSecondsRemaining > 3600 {
		t.Errorf("ttl_seconds_remaining = %d, want in (0,3600]", got.TTLSecondsRemaining)
	}
}

func TestCreateNoUserdataSkipsExec(t *testing.T) {
	h, store, fake := testServer(t)

	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":60}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	var resp createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	c, _ := store.Get(resp.ContractID)
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatalf("container not tracked")
	}
	if len(fc.Execs) != 0 {
		t.Errorf("execs = %d, want 0 (no userdata)", len(fc.Execs))
	}
}

func TestDeleteReleases(t *testing.T) {
	h, store, fake := testServer(t)

	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":3600}`)
	var created createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	c, _ := store.Get(created.ContractID)

	rec = do(t, h, http.MethodDelete, "/contracts/"+created.ContractID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var got statusResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Status != string(contract.StateReleased) {
		t.Errorf("status = %q, want released", got.Status)
	}
	if got.TTLSecondsRemaining != 0 {
		t.Errorf("ttl_seconds_remaining = %d, want 0 for released", got.TTLSecondsRemaining)
	}

	// The container was killed and the contract marked released.
	if _, ok := fake.Container(c.ContainerID); ok {
		t.Error("container still tracked after release")
	}
	if n := fake.Count(); n != 0 {
		t.Errorf("live containers = %d, want 0", n)
	}
	final, _ := store.Get(created.ContractID)
	if final.State != contract.StateReleased {
		t.Errorf("stored state = %q, want released", final.State)
	}
}

func TestDeleteIdempotent(t *testing.T) {
	h, _, _ := testServer(t)

	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":3600}`)
	var created createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	if rec = do(t, h, http.MethodDelete, "/contracts/"+created.ContractID, ""); rec.Code != http.StatusOK {
		t.Fatalf("first DELETE status = %d, want 200", rec.Code)
	}
	// A second DELETE of an already-released contract still succeeds.
	rec = do(t, h, http.MethodDelete, "/contracts/"+created.ContractID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("second DELETE status = %d, want 200", rec.Code)
	}
	var got statusResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Status != string(contract.StateReleased) {
		t.Errorf("status = %q, want released", got.Status)
	}
}

func TestCreateInvalidTTL(t *testing.T) {
	h, _, _ := testServer(t)
	for _, body := range []string{`{"ttl_seconds":0}`, `{"ttl_seconds":-5}`, `{"image":"wisp-base"}`} {
		rec := do(t, h, http.MethodPost, "/contracts", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s status = %d, want 400", body, rec.Code)
		}
	}
}

func TestCreateInvalidBody(t *testing.T) {
	h, _, _ := testServer(t)
	rec := do(t, h, http.MethodPost, "/contracts", `not json`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestCreateUserdataFailure(t *testing.T) {
	h, store, fake := testServer(t)
	fake.ExecHandler = func(id string, cmd []string) (runtime.ExecResult, error) {
		return runtime.ExecResult{ExitCode: 1, Stderr: "boom"}, nil
	}

	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":60,"userdata":"exit 1"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}

	// The container was destroyed and the contract marked expired.
	if n := fake.Count(); n != 0 {
		t.Errorf("live containers = %d, want 0 after failed userdata", n)
	}
	all := store.List()
	if len(all) != 1 {
		t.Fatalf("stored contracts = %d, want 1", len(all))
	}
	if all[0].State != contract.StateExpired {
		t.Errorf("state = %q, want expired after failed userdata", all[0].State)
	}
}

func TestCreateEnsuresImageBeforeCreate(t *testing.T) {
	h, store, fake := testServer(t)

	created := createContract(t, h, `{"ttl_seconds":3600}`)

	c, err := store.Get(created.ContractID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}

	// Provisioning pulled the resolved image on demand before booting.
	ensured := fake.Ensured()
	if len(ensured) != 1 || ensured[0] != "wisp-base" {
		t.Errorf("ensured images = %v, want [wisp-base]", ensured)
	}
	// The image ensured is the one the container was created from.
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatal("container not tracked")
	}
	if fc.Image != ensured[0] {
		t.Errorf("created image = %q, ensured image = %q; want equal", fc.Image, ensured[0])
	}
}

func TestCreateEnsureImageError(t *testing.T) {
	h, store, fake := testServer(t)
	fake.EnsureImageErr = errors.New("pull failed")

	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":60}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}

	// EnsureImage runs before Create: a pull failure fails the contract and no
	// container is ever booted.
	if n := fake.Count(); n != 0 {
		t.Errorf("live containers = %d, want 0 after failed pull", n)
	}
	all := store.List()
	if len(all) != 1 || all[0].State != contract.StateExpired {
		t.Errorf("contract state = %v, want a single expired contract", all)
	}
}

func TestCreateRuntimeCreateError(t *testing.T) {
	h, store, fake := testServer(t)
	fake.CreateErr = runtime.ErrNotFound // any non-nil error

	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":60}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	all := store.List()
	if len(all) != 1 || all[0].State != contract.StateExpired {
		t.Errorf("contract state = %v, want a single expired contract", all)
	}
}

// policyServer builds a handler wired to a fresh store and fake runtime over an
// explicit policy, returning the handler plus the backend so a test can drive
// the API and inspect launched containers.
func policyServer(t *testing.T, pol *policy.Config) (http.Handler, *contract.Store, *runtime.Fake) {
	t.Helper()
	store := contract.NewStore()
	fake := runtime.NewFake()
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, pol, bus.New(nil), "")
	return h, store, fake
}

func TestCreateImageNotAllowed(t *testing.T) {
	h, store, _ := testServer(t)
	// The default policy allows only "wisp-base"; any other image is a 400.
	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":60,"image":"not-allowed"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	// A disallowed image is rejected before any contract is recorded.
	if n := len(store.List()); n != 0 {
		t.Errorf("stored contracts = %d, want 0 after rejected image", n)
	}
}

func TestCreateNetworkNotAllowed(t *testing.T) {
	h, store, _ := testServer(t)
	// The default policy allows only none+open; egress is not permitted -> 400.
	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":60,"network":"egress"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if n := len(store.List()); n != 0 {
		t.Errorf("stored contracts = %d, want 0 after rejected network", n)
	}
}

func TestCreateDefaultImageAndNetwork(t *testing.T) {
	h, store, fake := testServer(t)
	// Omitting image and network resolves to the policy defaults: the base image
	// and "open" (which leaves Docker's network mode at the runtime default).
	created := createContract(t, h, `{"ttl_seconds":60}`)

	c, _ := store.Get(created.ContractID)
	if c.Image != "wisp-base" {
		t.Errorf("image = %q, want wisp-base", c.Image)
	}
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatal("container not tracked")
	}
	if fc.Image != "wisp-base" {
		t.Errorf("container image = %q, want wisp-base", fc.Image)
	}
	// The default network is "open", so no explicit Docker network mode is set.
	if fc.Opts.NetworkMode != "" {
		t.Errorf("network mode = %q, want empty for open", fc.Opts.NetworkMode)
	}
}

func TestCreateNoneNetworkMapsToDockerNone(t *testing.T) {
	h, store, fake := testServer(t)
	// Explicitly requesting "none" maps to Docker's "none" network mode.
	created := createContract(t, h, `{"ttl_seconds":60,"network":"none"}`)
	c, _ := store.Get(created.ContractID)
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatal("container not tracked")
	}
	if fc.Opts.NetworkMode != "none" {
		t.Errorf("network mode = %q, want none", fc.Opts.NetworkMode)
	}
}

func TestCreateEgressNetworkMapsToDedicatedBridge(t *testing.T) {
	// A policy permitting egress lets a lease request outbound-only isolation.
	pol := &policy.Config{
		Allow:        []string{"wisp-base"},
		DefaultImage: "wisp-base",
		Limits:       policy.Limits{Networks: []string{"none", "open", "egress"}},
	}
	h, store, fake := policyServer(t, pol)

	// "egress" attaches the container to the dedicated wisp-managed bridge rather
	// than falling through to the default network, so the runtime can isolate it.
	created := createContract(t, h, `{"ttl_seconds":60,"network":"egress"}`)
	c, _ := store.Get(created.ContractID)
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatal("container not tracked")
	}
	if fc.Opts.NetworkMode != runtime.EgressNetworkName {
		t.Errorf("network mode = %q, want %q for egress", fc.Opts.NetworkMode, runtime.EgressNetworkName)
	}
}

func TestCreateOmittedIsolationDefaultsToShared(t *testing.T) {
	h, store, fake := testServer(t)
	// Omitting isolation resolves to the policy default (shared) and preserves
	// today's behavior exactly: nothing about the launch changes yet.
	created := createContract(t, h, `{"ttl_seconds":60}`)

	c, _ := store.Get(created.ContractID)
	if c.Isolation != string(policy.IsolationShared) {
		t.Errorf("stored isolation = %q, want shared", c.Isolation)
	}
	// A create with no isolation still launches a normal container: no network
	// mode override and the usual no-new-privileges hardening (unchanged behavior).
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatal("container not tracked")
	}
	if fc.Opts.NetworkMode != "" {
		t.Errorf("network mode = %q, want empty (behavior preserved)", fc.Opts.NetworkMode)
	}
}

func TestCreateSharedIsolationAccepted(t *testing.T) {
	h, store, _ := testServer(t)
	// The default policy allows shared, so an explicit shared request succeeds and
	// is recorded on the contract.
	created := createContract(t, h, `{"ttl_seconds":60,"isolation":"shared"}`)
	c, _ := store.Get(created.ContractID)
	if c.Isolation != string(policy.IsolationShared) {
		t.Errorf("stored isolation = %q, want shared", c.Isolation)
	}
}

func TestCreateUnknownIsolationRejected(t *testing.T) {
	h, store, _ := testServer(t)
	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":60,"isolation":"turtle"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if n := len(store.List()); n != 0 {
		t.Errorf("stored contracts = %d, want 0 after rejected isolation", n)
	}
}

func TestCreateConfidentialIsolationRejected(t *testing.T) {
	// confidential is a known level, so it parses, but it is not supported yet and
	// is rejected with a clear error before any contract is recorded — even if an
	// operator has (mistakenly) allow-listed it.
	pol := &policy.Config{
		Allow:        []string{"wisp-base"},
		DefaultImage: "wisp-base",
		Limits: policy.Limits{
			Networks:         []string{"none", "open"},
			Isolations:       []string{"shared", "confidential"},
			DefaultIsolation: "shared",
		},
	}
	h, store, _ := policyServer(t, pol)
	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":60,"isolation":"confidential"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not supported yet") {
		t.Errorf("body = %q, want it to mention 'not supported yet'", rec.Body.String())
	}
	if n := len(store.List()); n != 0 {
		t.Errorf("stored contracts = %d, want 0 after rejected isolation", n)
	}
}

func TestCreateIsolationNotAllowedRejected(t *testing.T) {
	h, store, _ := testServer(t)
	// The default policy allows only shared; a valid-but-not-permitted level is a
	// 400, mirroring the network allow-list rejection.
	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":60,"isolation":"vm"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not allowed") {
		t.Errorf("body = %q, want it to mention 'not allowed'", rec.Body.String())
	}
	if n := len(store.List()); n != 0 {
		t.Errorf("stored contracts = %d, want 0 after rejected isolation", n)
	}
}

func TestCreateAllowedIsolationRecorded(t *testing.T) {
	// A policy widening the allowed set lets a lease request a stronger level; it
	// is recorded on the contract (a later task maps it to a runtime — launch
	// behavior is unchanged for now, so the container still boots normally).
	pol := &policy.Config{
		Allow:        []string{"wisp-base"},
		DefaultImage: "wisp-base",
		Limits: policy.Limits{
			Networks:         []string{"none", "open"},
			Isolations:       []string{"shared", "sandboxed", "vm"},
			DefaultIsolation: "shared",
		},
	}
	h, store, fake := policyServer(t, pol)
	created := createContract(t, h, `{"ttl_seconds":60,"isolation":"VM"}`)
	c, _ := store.Get(created.ContractID)
	if c.Isolation != string(policy.IsolationVM) {
		t.Errorf("stored isolation = %q, want vm (case-normalized)", c.Isolation)
	}
	// The resolved level flows from the create path into the runtime's
	// CreateOptions.Isolation, and the runtime maps it to a launch mechanism: vm
	// on this linux-mode fake selects the Kata runtime.
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatal("container not tracked")
	}
	if fc.Opts.Isolation != string(policy.IsolationVM) {
		t.Errorf("CreateOptions.Isolation = %q, want vm", fc.Opts.Isolation)
	}
	if fc.Runtime != "kata-runtime" || fc.Isolation != "" {
		t.Errorf("launch mechanism = {Runtime:%q Isolation:%q}, want {kata-runtime, }", fc.Runtime, fc.Isolation)
	}
}

// TestCreateIsolationUnavailableDroppedAndRejected verifies the host advertises
// and accepts only isolation levels it can actually run: a policy that allows vm
// on a host whose daemon reports only runc drops vm from the effective set (with a
// startup WARNING), rejects a vm create request, and advertises only shared on the
// discovery document.
func TestCreateIsolationUnavailableDroppedAndRejected(t *testing.T) {
	pol := &policy.Config{
		Allow:        []string{"wisp-base"},
		DefaultImage: "wisp-base",
		Limits: policy.Limits{
			Networks:         []string{"none", "open"},
			Isolations:       []string{"shared", "vm"},
			DefaultIsolation: "shared",
		},
	}
	store := contract.NewStore()
	fake := runtime.NewFake()
	// This daemon can only run the shared baseline: no gVisor/Kata runtimes.
	fake.Runtimes = []string{"runc"}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	h := New(logger, store, fake, pol, bus.New(nil), "")

	// A clear startup warning names the dropped level.
	if out := logs.String(); !strings.Contains(out, "vm") || !strings.Contains(out, "not available") {
		t.Errorf("startup logs = %q, want a warning that vm is not available", out)
	}

	// A create requesting the dropped level is rejected before any contract exists.
	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":60,"isolation":"vm"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not allowed") {
		t.Errorf("body = %q, want it to mention 'not allowed'", rec.Body.String())
	}
	if n := len(store.List()); n != 0 {
		t.Errorf("stored contracts = %d, want 0 after rejected isolation", n)
	}

	// The discovery document advertises only the effective (runnable) levels.
	rec = do(t, h, http.MethodGet, "/images", "")
	var got imagesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got.Isolation.Supported, []string{"shared"}) {
		t.Errorf("advertised isolation.supported = %v, want [shared]", got.Isolation.Supported)
	}
	if got.Isolation.Default != "shared" {
		t.Errorf("advertised isolation.default = %q, want shared", got.Isolation.Default)
	}

	// shared (the effective level) is still accepted.
	if rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":60,"isolation":"shared"}`); rec.Code != http.StatusCreated {
		t.Errorf("shared create status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestCreateSandboxedRequiresRunsc verifies sandboxed is only accepted when the
// daemon has the gVisor runtime "runsc" registered: with runsc present the level
// is advertised and a create succeeds; without it the level is dropped and the
// create is rejected.
func TestCreateSandboxedRequiresRunsc(t *testing.T) {
	pol := &policy.Config{
		Allow:        []string{"wisp-base"},
		DefaultImage: "wisp-base",
		Limits: policy.Limits{
			Networks:         []string{"none", "open"},
			Isolations:       []string{"shared", "sandboxed"},
			DefaultIsolation: "shared",
		},
	}

	t.Run("runsc present", func(t *testing.T) {
		store := contract.NewStore()
		fake := runtime.NewFake()
		fake.Runtimes = []string{"runc", "runsc"}
		h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, pol, bus.New(nil), "")

		created := createContract(t, h, `{"ttl_seconds":60,"isolation":"sandboxed"}`)
		c, _ := store.Get(created.ContractID)
		fc, ok := fake.Container(c.ContainerID)
		if !ok {
			t.Fatal("container not tracked")
		}
		// sandboxed maps to the gVisor runtime on the launched container.
		if fc.Runtime != "runsc" {
			t.Errorf("launch runtime = %q, want runsc", fc.Runtime)
		}
	})

	t.Run("runsc absent", func(t *testing.T) {
		store := contract.NewStore()
		fake := runtime.NewFake()
		fake.Runtimes = []string{"runc"}
		h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, pol, bus.New(nil), "")

		rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":60,"isolation":"sandboxed"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
		}
		if n := len(store.List()); n != 0 {
			t.Errorf("stored contracts = %d, want 0", n)
		}
	})
}

// TestCreateVMAvailableOnWindowsDaemon verifies a windows-mode daemon provides vm
// via Hyper-V even without a Kata runtime registered: the level is accepted and
// maps to Hyper-V isolation on the launched container.
func TestCreateVMAvailableOnWindowsDaemon(t *testing.T) {
	pol := &policy.Config{
		Allow:        []string{"wisp-base", "wisp-base-windows"},
		DefaultImage: "wisp-base",
		Limits: policy.Limits{
			Networks:         []string{"none", "open"},
			Isolations:       []string{"shared", "vm"},
			DefaultIsolation: "shared",
		},
	}
	store := contract.NewStore()
	fake := runtime.NewFake()
	fake.OS = runtime.OSWindows
	// Only runc registered — vm comes from the windows daemon (Hyper-V), not Kata.
	fake.Runtimes = []string{"runc"}
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, pol, bus.New(nil), "")

	created := createContract(t, h, `{"ttl_seconds":60,"isolation":"vm"}`)
	c, _ := store.Get(created.ContractID)
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatal("container not tracked")
	}
	// vm on a windows daemon selects Hyper-V isolation, not a named runtime.
	if fc.Isolation != "hyperv" || fc.Runtime != "" {
		t.Errorf("launch mechanism = {Runtime:%q Isolation:%q}, want {\"\", hyperv}", fc.Runtime, fc.Isolation)
	}
}

func TestCreateAppliesNoNewPrivileges(t *testing.T) {
	h, store, fake := testServer(t)
	// Every leased container is hardened with the no-new-privileges security
	// option regardless of image, network, or resource request.
	created := createContract(t, h, `{"ttl_seconds":60}`)
	c, _ := store.Get(created.ContractID)
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatal("container not tracked")
	}
	if got := fc.Opts.SecurityOpt; len(got) != 1 || got[0] != "no-new-privileges:true" {
		t.Errorf("SecurityOpt = %v, want [no-new-privileges:true]", got)
	}
}

func TestCreateClampsTTLToLimit(t *testing.T) {
	// A policy with a 15-minute TTL cap clamps a longer requested lease.
	pol := &policy.Config{
		Allow:        []string{"wisp-base"},
		DefaultImage: "wisp-base",
		Limits:       policy.Limits{MaxTTLSeconds: 900, Networks: []string{"none", "open"}},
	}
	h, store, _ := policyServer(t, pol)

	created := createContract(t, h, `{"ttl_seconds":3600}`)
	c, err := store.Get(created.ContractID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if want := 15 * time.Minute; c.TTL != want {
		t.Errorf("clamped TTL = %v, want %v", c.TTL, want)
	}
	if got := c.ExpiresAt.Sub(c.CreatedAt); got != 15*time.Minute {
		t.Errorf("lease window = %v, want 15m", got)
	}
}

func TestCreateBelowTTLLimitKeepsRequested(t *testing.T) {
	pol := &policy.Config{
		Allow:        []string{"wisp-base"},
		DefaultImage: "wisp-base",
		Limits:       policy.Limits{MaxTTLSeconds: 900, Networks: []string{"none", "open"}},
	}
	h, store, _ := policyServer(t, pol)
	// 5 minutes is under the cap, so it survives unchanged.
	created := createContract(t, h, `{"ttl_seconds":300}`)
	c, _ := store.Get(created.ContractID)
	if want := 5 * time.Minute; c.TTL != want {
		t.Errorf("TTL = %v, want %v", c.TTL, want)
	}
}

func TestCreateAppliesRequestedImageAndResources(t *testing.T) {
	// A policy allowing a second image with resource caps lets us assert that the
	// requested image and (clamped) resources reach the runtime.
	pol := &policy.Config{
		Allow:        []string{"wisp-base", "tiny-image"},
		DefaultImage: "wisp-base",
		Limits: policy.Limits{
			MaxCPUs:     0.5,
			MaxMemoryMB: 256,
			PidsLimit:   64,
			Networks:    []string{"none", "open"},
		},
	}
	h, store, fake := policyServer(t, pol)

	// Request more than the caps allow; each dimension is clamped down.
	created := createContract(t, h,
		`{"ttl_seconds":600,"image":"tiny-image","network":"open","resources":{"cpus":4,"memory_mb":8192,"pids":9999}}`)
	c, _ := store.Get(created.ContractID)
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatal("container not tracked")
	}
	if fc.Image != "tiny-image" {
		t.Errorf("image = %q, want tiny-image", fc.Image)
	}
	if want := int64(0.5 * 1e9); fc.Opts.Resources.NanoCPUs != want {
		t.Errorf("NanoCPUs = %d, want %d (clamped)", fc.Opts.Resources.NanoCPUs, want)
	}
	if want := int64(256 * 1024 * 1024); fc.Opts.Resources.MemoryBytes != want {
		t.Errorf("MemoryBytes = %d, want %d (clamped)", fc.Opts.Resources.MemoryBytes, want)
	}
	if fc.Opts.Resources.PidsLimit != 64 {
		t.Errorf("PidsLimit = %d, want 64 (clamped)", fc.Opts.Resources.PidsLimit)
	}
	// An "open" network leaves NetworkMode empty (the runtime default).
	if fc.Opts.NetworkMode != "" {
		t.Errorf("network mode = %q, want empty for open", fc.Opts.NetworkMode)
	}
}

func TestCreateEchoesMetaOnStatus(t *testing.T) {
	h, _, _ := testServer(t)
	created := createContract(t, h, `{"ttl_seconds":60,"meta":{"job":"build-42"}}`)

	rec := do(t, h, http.MethodGet, "/contracts/"+created.ContractID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	var got statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Meta["job"] != "build-42" {
		t.Errorf("meta = %v, want job=build-42 echoed back", got.Meta)
	}
}

func TestImagesDiscovery(t *testing.T) {
	pol := &policy.Config{
		Allow:        []string{"wisp-base", "custom"},
		DefaultImage: "wisp-base",
		Limits: policy.Limits{
			MaxTTLSeconds: 900,
			MaxCPUs:       2,
			MaxMemoryMB:   1024,
			PidsLimit:     256,
			Networks:      []string{"none", "open", "egress"},
		},
	}
	h, _, _ := policyServer(t, pol)

	// GET /images is unauthenticated even when an app token is set.
	rec := do(t, h, http.MethodGet, "/images", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var got imagesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Images) != 2 || got.Images[0] != "wisp-base" || got.Images[1] != "custom" {
		t.Errorf("images = %v, want [wisp-base custom]", got.Images)
	}
	if got.Default != "wisp-base" {
		t.Errorf("default = %q, want wisp-base", got.Default)
	}
	if got.Limits.MaxTTLSeconds != 900 || got.Limits.MaxCPUs != 2 || got.Limits.MaxMemoryMB != 1024 || got.Limits.PidsLimit != 256 {
		t.Errorf("limits = %+v, want the policy limits", got.Limits)
	}
	if len(got.Limits.Networks) != 3 {
		t.Errorf("networks = %v, want none+open+egress", got.Limits.Networks)
	}
}

// GET /images advertises the daemon's detected container OS so a consumer knows
// what this host serves. A default (linux) fake reports "linux".
func TestImagesAdvertisesOS(t *testing.T) {
	h, _, _ := testServer(t)
	rec := do(t, h, http.MethodGet, "/images", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var got imagesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.OS != string(runtime.OSLinux) {
		t.Errorf("os = %q, want linux", got.OS)
	}
	// The Linux default is advertised on a linux host.
	if got.Default != "wisp-base" {
		t.Errorf("default = %q, want wisp-base on a linux host", got.Default)
	}
}

// On a windows-mode host, GET /images reports os "windows" and advertises the
// Windows base image as the default (the built-in policy allow-lists both).
// Uses the fake runtime with a stubbed OS — no real Windows Docker daemon.
func TestImagesAdvertisesWindowsDefault(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	fake.OS = runtime.OSWindows
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, policy.Default(), bus.New(nil), "")

	rec := do(t, h, http.MethodGet, "/images", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var got imagesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.OS != string(runtime.OSWindows) {
		t.Errorf("os = %q, want windows", got.OS)
	}
	if got.Default != "wisp-base-windows" {
		t.Errorf("default = %q, want wisp-base-windows on a windows host", got.Default)
	}
}

// gpuServer builds a handler over a fake runtime carrying the given registered
// runtimes and enumerated GPUs, so the GPU-block advertising can be exercised
// without a real daemon or NVIDIA tooling. The GPU posture is computed once at
// broker construction (inside New), so the fake must be configured before the
// call — hence a dedicated helper rather than the shared testServer.
func gpuServer(t *testing.T, pol *policy.Config, runtimes []string, devices []runtime.GPUDevice) http.Handler {
	t.Helper()
	store := contract.NewStore()
	fake := runtime.NewFake()
	fake.Runtimes = runtimes
	fake.GPUDevices = devices
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, pol, bus.New(nil), "")
}

// getImages GETs /images and decodes it, failing the test on a non-200.
func getImages(t *testing.T, h http.Handler) imagesResponse {
	t.Helper()
	rec := do(t, h, http.MethodGet, "/images", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var got imagesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// On an NVIDIA host with devices enumerated, GET /images advertises a supported
// GPU block with the enumerated devices (field-for-field the wire contract), the
// effective per-lease cap, and shared-only attach.
func TestImagesGPUBlockSupported(t *testing.T) {
	devices := []runtime.GPUDevice{
		{ID: "GPU-1111", Class: "nvidia-geforce-rtx-4090", VRAMMB: 24564},
		{ID: "GPU-2222", Class: "nvidia-a100-sxm4-80gb", VRAMMB: 81920},
	}
	h := gpuServer(t, policy.Default(), []string{"runc", "nvidia"}, devices)
	got := getImages(t, h)

	if !got.GPU.Supported {
		t.Fatalf("gpu.supported = false, want true (body has %+v)", got.GPU)
	}
	if got.GPU.MaxGPUs != 2 {
		t.Errorf("gpu.max_gpus = %d, want 2 (all detected, no operator cap)", got.GPU.MaxGPUs)
	}
	if !reflect.DeepEqual(got.GPU.Isolations, []string{"shared"}) {
		t.Errorf("gpu.isolations = %v, want [shared]", got.GPU.Isolations)
	}
	want := []gpuDeviceResponse{
		{ID: "GPU-1111", Class: "nvidia-geforce-rtx-4090", VRAMMB: 24564},
		{ID: "GPU-2222", Class: "nvidia-a100-sxm4-80gb", VRAMMB: 81920},
	}
	if !reflect.DeepEqual(got.GPU.Devices, want) {
		t.Errorf("gpu.devices = %+v, want %+v", got.GPU.Devices, want)
	}
}

// An operator per-lease cap below the detected count is reflected in the
// advertised max_gpus while still listing every enumerated device.
func TestImagesGPUBlockOperatorCap(t *testing.T) {
	pol := policy.Default()
	pol.Limits.MaxGPUs = 1
	devices := []runtime.GPUDevice{
		{ID: "GPU-1111", Class: "nvidia-l4", VRAMMB: 24564},
		{ID: "GPU-2222", Class: "nvidia-l4", VRAMMB: 24564},
	}
	got := getImages(t, gpuServer(t, pol, []string{"runc", "nvidia"}, devices))

	if !got.GPU.Supported {
		t.Fatal("gpu.supported = false, want true")
	}
	if got.GPU.MaxGPUs != 1 {
		t.Errorf("gpu.max_gpus = %d, want 1 (operator cap below detected count)", got.GPU.MaxGPUs)
	}
	if len(got.GPU.Devices) != 2 {
		t.Errorf("gpu.devices len = %d, want 2 (the cap limits per-lease use, not enumeration)", len(got.GPU.Devices))
	}
}

// TestImagesGPUBlockGPULessHost verifies the block is ALWAYS present: a host
// without the NVIDIA runtime advertises supported=false with an empty (non-null)
// devices array, max_gpus 0, and no attach isolations. The default fake reports
// runc/runsc/kata but never nvidia.
func TestImagesGPUBlockGPULessHost(t *testing.T) {
	got := getImages(t, gpuServer(t, policy.Default(), nil, nil))

	if got.GPU.Supported {
		t.Error("gpu.supported = true, want false on a GPU-less host")
	}
	if got.GPU.MaxGPUs != 0 {
		t.Errorf("gpu.max_gpus = %d, want 0", got.GPU.MaxGPUs)
	}
	if got.GPU.Devices == nil {
		t.Error("gpu.devices = null, want an empty array (the block is always present)")
	}
	if len(got.GPU.Devices) != 0 {
		t.Errorf("gpu.devices = %+v, want empty", got.GPU.Devices)
	}
	if got.GPU.Isolations == nil || len(got.GPU.Isolations) != 0 {
		t.Errorf("gpu.isolations = %v, want an empty array", got.GPU.Isolations)
	}
}

// TestImagesGPUBlockRawJSON verifies the wire-contract field NAMES exactly (the
// surface wisp-agent scrapes), independent of the Go struct tags, and that the
// block is always present even on a GPU-less host.
func TestImagesGPUBlockRawJSON(t *testing.T) {
	rec := do(t, gpuServer(t, policy.Default(), nil, nil), http.MethodGet, "/images", "")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	block, ok := raw["gpu"]
	if !ok {
		t.Fatal("/images response has no top-level \"gpu\" object")
	}
	var gpu map[string]json.RawMessage
	if err := json.Unmarshal(block, &gpu); err != nil {
		t.Fatalf("decode gpu block: %v", err)
	}
	for _, field := range []string{"supported", "devices", "max_gpus", "isolations"} {
		if _, ok := gpu[field]; !ok {
			t.Errorf("gpu block missing wire-contract field %q", field)
		}
	}
	// devices must be [] (not null) so a consumer can iterate unconditionally.
	if string(gpu["devices"]) != "[]" {
		t.Errorf("gpu.devices raw = %s, want []", gpu["devices"])
	}
}

// TestImagesGPUEnumerationFailureDegrades verifies a GPU-detection failure (the
// NVIDIA runtime present but nvidia-smi enumeration erroring) degrades to
// supported=false rather than failing startup or the request.
func TestImagesGPUEnumerationFailureDegrades(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	fake.Runtimes = []string{"runc", "nvidia"}
	fake.GPUErr = errors.New("nvidia-smi: garbage output")
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, policy.Default(), bus.New(nil), "")

	got := getImages(t, h)
	if got.GPU.Supported {
		t.Error("gpu.supported = true, want false when enumeration fails")
	}
	if len(got.GPU.Devices) != 0 {
		t.Errorf("gpu.devices = %+v, want empty on enumeration failure", got.GPU.Devices)
	}
}

// GET /images advertises the operator's host capacity budgets and the live
// non-terminal contract count in the "capacity" block; used_* are 0 until the
// capacity allocator lands (sibling #566).
func TestImagesCapacityBlockBudgets(t *testing.T) {
	pol := policy.Default()
	pol.Limits.MaxContracts = 5
	pol.Limits.TotalCPUs = 16
	pol.Limits.TotalMemoryMB = 32768
	store := contract.NewStore()
	fake := runtime.NewFake()
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, pol, bus.New(nil), "")

	got := getImages(t, h)
	if got.Capacity.MaxContracts != 5 || got.Capacity.TotalCPUs != 16 || got.Capacity.TotalMemoryMB != 32768 {
		t.Errorf("capacity budgets = %+v, want max_contracts=5 total_cpus=16 total_memory_mb=32768", got.Capacity)
	}
	if got.Capacity.ActiveContracts != 0 {
		t.Errorf("capacity.active_contracts = %d, want 0 (empty store)", got.Capacity.ActiveContracts)
	}
	if got.Capacity.UsedCPUs != 0 || got.Capacity.UsedMemoryMB != 0 {
		t.Errorf("capacity used_* = {%v, %d}, want 0 (allocator lands in #566)", got.Capacity.UsedCPUs, got.Capacity.UsedMemoryMB)
	}
}

// capacity.active_contracts counts only non-terminal contracts: a released or
// expired contract still in the store is excluded.
func TestImagesCapacityActiveContracts(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, policy.Default(), bus.New(nil), "")

	// Two live contracts and one driven to a terminal state.
	if _, err := store.Create(contract.CreateParams{TTL: time.Minute, Image: "wisp-base"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Create(contract.CreateParams{TTL: time.Minute, Image: "wisp-base"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	term, err := store.Create(contract.CreateParams{TTL: time.Minute, Image: "wisp-base"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.UpdateState(term.ID, contract.StateReleased); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}

	got := getImages(t, h)
	if got.Capacity.ActiveContracts != 2 {
		t.Errorf("capacity.active_contracts = %d, want 2 (terminal contract excluded)", got.Capacity.ActiveContracts)
	}
}

// TestImagesCapacityBlockRawJSON verifies the wire-contract field NAMES exactly
// (the surface wisp-agent forwards and wisper-api consumes), independent of the
// Go struct tags, and that the block is always present.
func TestImagesCapacityBlockRawJSON(t *testing.T) {
	h, _, _ := testServer(t)
	rec := do(t, h, http.MethodGet, "/images", "")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	block, ok := raw["capacity"]
	if !ok {
		t.Fatal("/images response has no top-level \"capacity\" object")
	}
	var capacity map[string]json.RawMessage
	if err := json.Unmarshal(block, &capacity); err != nil {
		t.Fatalf("decode capacity block: %v", err)
	}
	for _, field := range []string{"max_contracts", "active_contracts", "total_cpus", "used_cpus", "total_memory_mb", "used_memory_mb"} {
		if _, ok := capacity[field]; !ok {
			t.Errorf("capacity block missing wire-contract field %q", field)
		}
	}
}

// On a windows-mode host, an omitted image on create boots the Windows base
// image (the OS-aware default) rather than the Linux base.
func TestCreateDefaultsToWindowsBaseImage(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	fake.OS = runtime.OSWindows
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, policy.Default(), bus.New(nil), "")

	created := createContract(t, h, `{"ttl_seconds":60}`)
	c, _ := store.Get(created.ContractID)
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatal("container not tracked")
	}
	if fc.Image != "wisp-base-windows" {
		t.Errorf("image = %q, want wisp-base-windows on a windows host", fc.Image)
	}
}

// When the daemon rejects the image for an OS/platform mismatch, create surfaces
// a clear, mapped error naming this host's container mode (a 400), and does NOT
// switch modes. Uses the fake runtime with a create error that mimics Docker's
// rejection message.
func TestCreateImageOSMismatch(t *testing.T) {
	h, store, fake := testServer(t)
	fake.CreateErr = errors.New(`Error response from daemon: image operating system "windows" cannot be used on this platform`)

	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":60}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	// The error is the clear, OS-aware message — not an opaque "provisioning failed".
	body := rec.Body.String()
	if !strings.Contains(body, "this host is in linux container mode") || !strings.Contains(body, "not compatible") {
		t.Errorf("error body = %q, want the clear OS-mismatch message", body)
	}
	// No container survives and the contract is marked expired (no mode switching,
	// just a failed create).
	if n := fake.Count(); n != 0 {
		t.Errorf("live containers = %d, want 0 after mismatch", n)
	}
	all := store.List()
	if len(all) != 1 || all[0].State != contract.StateExpired {
		t.Errorf("contract state = %v, want a single expired contract", all)
	}
}

// The most common real mismatch surfaces on the on-demand pull path, not
// ContainerCreate: the modern daemon rejects an incompatible registry image at
// pull time with "no matching manifest ... in the manifest list entries",
// arriving wrapped as `runtime: pull image <name>: Error response from daemon:
// ...`. That wrapped chain must still map to the clear 400, not a generic 500.
func TestCreateImageOSMismatchOnPull(t *testing.T) {
	h, store, fake := testServer(t)
	fake.EnsureImageErr = fmt.Errorf("runtime: pull image ghcr.io/x/y: %w",
		errors.New(`Error response from daemon: no matching manifest for windows(10.0.26200)/amd64 in the manifest list entries`))

	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":60}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "this host is in linux container mode") || !strings.Contains(body, "not compatible") {
		t.Errorf("error body = %q, want the clear OS-mismatch message", body)
	}
	// The pull never produced a container, and the contract is marked expired.
	if n := fake.Count(); n != 0 {
		t.Errorf("live containers = %d, want 0 after mismatch", n)
	}
	all := store.List()
	if len(all) != 1 || all[0].State != contract.StateExpired {
		t.Errorf("contract state = %v, want a single expired contract", all)
	}
}

func TestImagesUnauthenticatedWithAppToken(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, policy.Default(), bus.New(nil), "app-secret")

	// No Authorization header: /images is public like /healthz.
	rec := do(t, h, http.MethodGet, "/images", "")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 without a token", rec.Code)
	}
}

func TestGetNotFound(t *testing.T) {
	h, _, _ := testServer(t)
	rec := do(t, h, http.MethodGet, "/contracts/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestDeleteNotFound(t *testing.T) {
	h, _, _ := testServer(t)
	rec := do(t, h, http.MethodDelete, "/contracts/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// GET /contracts returns every non-terminal contract with the documented JSON
// shape, including the current token and any caller-supplied external_id.
// Covers acceptance criterion 158.
func TestListContractsReturnsNonTerminalShape(t *testing.T) {
	h, store, _ := testServer(t)

	// One contract with a caller-supplied external id...
	first := createContract(t, h, `{"ttl_seconds":3600,"external_id":"lease-abc","resources":{"cpus":0,"memory_mb":0}}`)
	// ...and one without.
	second := createContract(t, h, `{"ttl_seconds":1800}`)

	// A terminal contract must never appear.
	terminal := createContract(t, h, `{"ttl_seconds":3600}`)
	if _, err := store.UpdateState(terminal.ContractID, contract.StateReleased); err != nil {
		t.Fatalf("UpdateState released: %v", err)
	}

	rec := do(t, h, http.MethodGet, "/contracts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var got contractsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, rec.Body.String())
	}

	if len(got.Contracts) != 2 {
		t.Fatalf("contracts len = %d, want 2 (terminal excluded)", len(got.Contracts))
	}

	byID := map[string]contractInfo{}
	for _, c := range got.Contracts {
		byID[c.ID] = c
	}
	firstInfo, ok := byID[first.ContractID]
	if !ok {
		t.Fatalf("first contract missing from list")
	}
	if firstInfo.ExternalID != "lease-abc" {
		t.Errorf("first external_id = %q, want lease-abc", firstInfo.ExternalID)
	}
	if firstInfo.Token != first.Token {
		t.Errorf("first token = %q, want %q (current create token)", firstInfo.Token, first.Token)
	}
	if firstInfo.Status != string(contract.StateReady) {
		t.Errorf("first status = %q, want ready", firstInfo.Status)
	}
	if firstInfo.ExpiresAt <= 0 {
		t.Errorf("first expires_at = %d, want > 0", firstInfo.ExpiresAt)
	}
	if firstInfo.TTLSecondsRemaining <= 0 || firstInfo.TTLSecondsRemaining > 3600 {
		t.Errorf("first ttl_seconds_remaining = %d, want (0,3600]", firstInfo.TTLSecondsRemaining)
	}
	if firstInfo.Gpus == nil {
		t.Errorf("first gpus = null, want empty array (always present in the wire contract)")
	}

	secondInfo, ok := byID[second.ContractID]
	if !ok {
		t.Fatalf("second contract missing from list")
	}
	if secondInfo.ExternalID != "" {
		t.Errorf("second external_id = %q, want empty for a caller that supplied none", secondInfo.ExternalID)
	}
	if secondInfo.Token != second.Token {
		t.Errorf("second token = %q, want %q", secondInfo.Token, second.Token)
	}
}

// GET /contracts's raw JSON matches the cross-repo wire contract exactly —
// wisp-agent scrapes these snake_case names — and the "contracts" array is
// always present (empty when no non-terminal contracts exist).
func TestListContractsRawJSONShape(t *testing.T) {
	h, _, _ := testServer(t)

	// Empty case: still an array, never null.
	rec := do(t, h, http.MethodGet, "/contracts", "")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if _, ok := raw["contracts"]; !ok {
		t.Fatal("response has no top-level \"contracts\" field")
	}
	if string(raw["contracts"]) != "[]" {
		t.Errorf("empty contracts = %s, want []", raw["contracts"])
	}

	// Populated case: each entry carries every wire-contract field.
	createContract(t, h, `{"ttl_seconds":60,"external_id":"lease-1"}`)
	rec = do(t, h, http.MethodGet, "/contracts", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode populated: %v", err)
	}
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(raw["contracts"], &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}
	for _, field := range []string{"id", "external_id", "token", "status", "expires_at", "ttl_seconds_remaining", "reserved_cpus", "reserved_memory_mb", "gpus"} {
		if _, ok := list[0][field]; !ok {
			t.Errorf("contract entry missing wire-contract field %q (entry: %v)", field, list[0])
		}
	}
	if string(list[0]["gpus"]) != "[]" {
		t.Errorf("gpus raw = %s, want [] (never null)", list[0]["gpus"])
	}
}

// TestListContractsExcludesRequested covers AC 216: a contract observed in the
// transient pre-provision state (store.Create returns StateRequested, provision
// then flips it to StateProvisioning) MUST NOT appear in GET /contracts, so the
// wire never emits a "status":"requested" outside the documented enum
// (provisioning|ready|expiring) that consumers were built against. The window is
// sub-millisecond in production; a store.Adopt with no state advance is used to
// pin a contract in StateRequested deterministically, and every non-Requested
// state is also walked through to prove the list surfaces those. A requested
// contract carries no container id yet either, so a resyncing agent loses
// nothing.
func TestListContractsExcludesRequested(t *testing.T) {
	h, store, _ := testServer(t)

	// A ready contract that MUST appear.
	ready := createContract(t, h, `{"ttl_seconds":3600}`)

	// A contract pinned in StateRequested. store.Create leaves a contract in
	// exactly that state (the provision path is what flips it to
	// StateProvisioning), so calling it directly recreates the sub-millisecond
	// window a caller might observe between store.Create and
	// UpdateState(StateProvisioning) inside broker.provision.
	pending, err := store.Create(contract.CreateParams{TTL: time.Hour})
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	if pending.State != contract.StateRequested {
		t.Fatalf("pending state = %q, want %q (test premise)", pending.State, contract.StateRequested)
	}

	rec := do(t, h, http.MethodGet, "/contracts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var got contractsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The list contains only the ready contract; the requested one is skipped.
	if len(got.Contracts) != 1 {
		t.Fatalf("contracts len = %d, want 1 (requested contract must be excluded)", len(got.Contracts))
	}
	if got.Contracts[0].ID != ready.ContractID {
		t.Errorf("contracts[0].ID = %q, want %q", got.Contracts[0].ID, ready.ContractID)
	}
	// Every surfaced status must fall inside the documented enum.
	allowed := map[string]bool{
		string(contract.StateProvisioning): true,
		string(contract.StateReady):        true,
		string(contract.StateExpiring):     true,
	}
	for _, c := range got.Contracts {
		if !allowed[c.Status] {
			t.Errorf("status = %q, want one of provisioning|ready|expiring", c.Status)
		}
	}

	// Advance the pinned contract through the remaining non-terminal states and
	// confirm each surfaces cleanly (the enum is exhaustive from the wire's
	// perspective).
	for _, next := range []contract.State{contract.StateProvisioning, contract.StateReady, contract.StateExpiring} {
		if _, err := store.UpdateState(pending.ID, next); err != nil {
			t.Fatalf("UpdateState %q: %v", next, err)
		}
		rec := do(t, h, http.MethodGet, "/contracts", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status after %q = %d, want 200", next, rec.Code)
		}
		var listed contractsListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
			t.Fatalf("decode after %q: %v", next, err)
		}
		found := false
		for _, c := range listed.Contracts {
			if c.ID == pending.ID {
				found = true
				if c.Status != string(next) {
					t.Errorf("status for %q = %q, want %q", pending.ID, c.Status, next)
				}
			}
			if !allowed[c.Status] {
				t.Errorf("status %q outside documented enum after %q", c.Status, next)
			}
		}
		if !found {
			t.Errorf("pending contract missing from list after %q transition", next)
		}
	}
}

// GET /contracts without the app token returns 401; with the token it succeeds.
// Covers acceptance criterion 159 (list-side).
func TestListContractsRequiresAppToken(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, policy.Default(), bus.New(nil), "app-secret")

	// No token: 401.
	if rec := do(t, h, http.MethodGet, "/contracts", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no-token GET /contracts status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}
	// Wrong token: 401.
	if rec := doAuth(t, h, http.MethodGet, "/contracts", "wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad-token GET /contracts status = %d, want 401", rec.Code)
	}
	// Correct token: 200.
	if rec := doAuth(t, h, http.MethodGet, "/contracts", "app-secret", ""); rec.Code != http.StatusOK {
		t.Errorf("good-token GET /contracts status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

// GET/DELETE /contracts/{id} require either the app token OR the per-contract
// token — a random local process without either credential cannot read or
// destroy the lease. Covers acceptance criterion 159 (per-contract side).
func TestContractByIDRequiresAppOrContractToken(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, policy.Default(), bus.New(nil), "app-secret")

	rec := doAuth(t, h, http.MethodPost, "/contracts", "app-secret", `{"ttl_seconds":3600}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var created createResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	url := "/contracts/" + created.ContractID

	// GET without any token: 401.
	if r := do(t, h, http.MethodGet, url, ""); r.Code != http.StatusUnauthorized {
		t.Errorf("GET no-token status = %d, want 401", r.Code)
	}
	// GET with the app token: 200.
	if r := doAuth(t, h, http.MethodGet, url, "app-secret", ""); r.Code != http.StatusOK {
		t.Errorf("GET app-token status = %d, want 200 (body: %s)", r.Code, r.Body.String())
	}
	// GET with the contract token: 200.
	if r := doAuth(t, h, http.MethodGet, url, created.Token, ""); r.Code != http.StatusOK {
		t.Errorf("GET contract-token status = %d, want 200 (body: %s)", r.Code, r.Body.String())
	}
	// GET with a random token: 401.
	if r := doAuth(t, h, http.MethodGet, url, "bogus", ""); r.Code != http.StatusUnauthorized {
		t.Errorf("GET bogus-token status = %d, want 401", r.Code)
	}

	// DELETE with the contract token succeeds (and simultaneously releases).
	if r := doAuth(t, h, http.MethodDelete, url, created.Token, ""); r.Code != http.StatusOK {
		t.Errorf("DELETE contract-token status = %d, want 200 (body: %s)", r.Code, r.Body.String())
	}

	// A fresh contract for the DELETE-without-token / app-token cases.
	rec = doAuth(t, h, http.MethodPost, "/contracts", "app-secret", `{"ttl_seconds":3600}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("second create status = %d, want 201", rec.Code)
	}
	var second createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &second)
	url = "/contracts/" + second.ContractID

	// DELETE without any token: 401.
	if r := do(t, h, http.MethodDelete, url, ""); r.Code != http.StatusUnauthorized {
		t.Errorf("DELETE no-token status = %d, want 401", r.Code)
	}
	// DELETE with a random token: 401.
	if r := doAuth(t, h, http.MethodDelete, url, "bogus", ""); r.Code != http.StatusUnauthorized {
		t.Errorf("DELETE bogus-token status = %d, want 401", r.Code)
	}
	// DELETE with the app token: 200.
	if r := doAuth(t, h, http.MethodDelete, url, "app-secret", ""); r.Code != http.StatusOK {
		t.Errorf("DELETE app-token status = %d, want 200 (body: %s)", r.Code, r.Body.String())
	}
}

// A create carrying external_id persists it as a container label, echoes it on
// GET /contracts/{id}, and surfaces it in GET /contracts. The bookkeeping is
// the same round-trip a wispd restart's reconcile relies on.
// Covers acceptance criterion 160 (create side).
func TestCreateExternalIDRoundTrip(t *testing.T) {
	h, store, fake := testServer(t)
	created := createContract(t, h, `{"ttl_seconds":3600,"external_id":"lease-xyz-42"}`)

	c, err := store.Get(created.ContractID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if c.ExternalID != "lease-xyz-42" {
		t.Errorf("stored ExternalID = %q, want lease-xyz-42", c.ExternalID)
	}

	// The container label carries the exact value so a wispd restart can recover it.
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatal("container not tracked")
	}
	if got := fc.Opts.Labels[externalIDLabel]; got != "lease-xyz-42" {
		t.Errorf("container label %s = %q, want lease-xyz-42", externalIDLabel, got)
	}

	// GET /contracts/{id} echoes it back.
	rec := doAuth(t, h, http.MethodGet, "/contracts/"+created.ContractID, created.Token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var status statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.ExternalID != "lease-xyz-42" {
		t.Errorf("status external_id = %q, want lease-xyz-42", status.ExternalID)
	}
}

// A create without external_id writes no wisp.external_id label — the presence
// of that label distinguishes a caller-supplied identifier from an absent one.
func TestCreateWithoutExternalIDWritesNoLabel(t *testing.T) {
	h, store, fake := testServer(t)
	created := createContract(t, h, `{"ttl_seconds":60}`)

	c, _ := store.Get(created.ContractID)
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatal("container not tracked")
	}
	if _, present := fc.Opts.Labels[externalIDLabel]; present {
		t.Errorf("container carries %s label without a caller-supplied external_id: %v",
			externalIDLabel, fc.Opts.Labels)
	}
}

// A create with an external_id longer than the cap is rejected up front — no
// contract is recorded, no container is booted.
func TestCreateExternalIDTooLongRejected(t *testing.T) {
	h, store, _ := testServer(t)
	tooLong := strings.Repeat("x", maxExternalIDLen+1)
	body := `{"ttl_seconds":60,"external_id":"` + tooLong + `"}`

	rec := do(t, h, http.MethodPost, "/contracts", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if n := len(store.List()); n != 0 {
		t.Errorf("stored contracts = %d, want 0 after rejected external_id", n)
	}
}

// After a wispd restart's reconcile, an adopted contract's external_id is
// recovered from the container label and appears on both GET /contracts and
// GET /contracts/{id} — the mapping the local wisp-agent needs to re-associate
// its leases. Covers acceptance criterion 160 (reconcile side).
func TestReconcileRestoresExternalIDFromLabel(t *testing.T) {
	ctx := context.Background()
	store := contract.NewStore()
	fake := runtime.NewFake()

	expiry := strconv.FormatInt(time.Unix(2_000_000_000, 0).Unix(), 10)
	cid, err := fake.Create(ctx, "wisp-base", runtime.CreateOptions{
		Labels: map[string]string{
			contractLabel:   "kept-contract",
			expiresAtLabel:  expiry,
			externalIDLabel: "lease-carried-across",
		},
	})
	if err != nil {
		t.Fatalf("fake.Create: %v", err)
	}
	if err := fake.Start(ctx, cid); err != nil {
		t.Fatalf("fake.Start: %v", err)
	}

	Reconcile(ctx, store, fake, nil, nil, discardLogger())

	c, err := store.Get("kept-contract")
	if err != nil {
		t.Fatalf("adopted contract missing: %v", err)
	}
	if c.ExternalID != "lease-carried-across" {
		t.Errorf("recovered ExternalID = %q, want lease-carried-across", c.ExternalID)
	}
}

// Adopt generates a fresh bearer token so an exec against a reconciled
// contract succeeds with the token returned by GET /contracts.
// Covers acceptance criterion 161.
func TestReconciledContractGetsFreshWorkingToken(t *testing.T) {
	ctx := context.Background()
	store := contract.NewStore()
	fake := runtime.NewFake()

	expiry := strconv.FormatInt(time.Unix(2_000_000_000, 0).Unix(), 10)
	cid, err := fake.Create(ctx, "wisp-base", runtime.CreateOptions{
		Labels: map[string]string{contractLabel: "adopted", expiresAtLabel: expiry},
	})
	if err != nil {
		t.Fatalf("fake.Create: %v", err)
	}
	if err := fake.Start(ctx, cid); err != nil {
		t.Fatalf("fake.Start: %v", err)
	}

	// Now build the HTTP surface over the same store/fake so an app-token-gated
	// GET /contracts + exec via the returned token exercises the fresh credential.
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, policy.Default(), bus.New(nil), "app-secret")
	Reconcile(ctx, store, fake, nil, nil, discardLogger())

	c, err := store.Get("adopted")
	if err != nil {
		t.Fatalf("adopted contract missing: %v", err)
	}
	if c.Token == "" {
		t.Fatal("adopted contract has empty token; want a fresh one")
	}

	// The token surfaced by GET /contracts matches the stored one.
	rec := doAuth(t, h, http.MethodGet, "/contracts", "app-secret", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /contracts status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var list contractsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Contracts) != 1 {
		t.Fatalf("list len = %d, want 1", len(list.Contracts))
	}
	if list.Contracts[0].Token != c.Token {
		t.Errorf("list token = %q, want %q (matches stored)", list.Contracts[0].Token, c.Token)
	}

	// Exec against the adopted contract with the fresh token succeeds — the whole
	// point of re-issuing on Adopt.
	rec = doAuth(t, h, http.MethodPost, "/contracts/adopted/exec", list.Contracts[0].Token, `{"command":"echo hi"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("exec status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

// Terminal contracts never appear in GET /contracts, and the reaper's cleanup
// sweep removes them from the store on the tick after they become terminal so
// storage does not grow unboundedly. Covers acceptance criterion 162.
func TestListExcludesTerminalAndReaperSweepDeletes(t *testing.T) {
	h, store, _ := testServer(t)
	live := createContract(t, h, `{"ttl_seconds":3600}`)
	terminal := createContract(t, h, `{"ttl_seconds":3600}`)

	// Drive the second one to a terminal state directly. GET /contracts must
	// filter it out even though it is still in the store.
	if _, err := store.UpdateState(terminal.ContractID, contract.StateReleased); err != nil {
		t.Fatalf("UpdateState released: %v", err)
	}

	rec := do(t, h, http.MethodGet, "/contracts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /contracts status = %d, want 200", rec.Code)
	}
	var list contractsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Contracts) != 1 || list.Contracts[0].ID != live.ContractID {
		t.Fatalf("list = %+v, want only the live contract", list.Contracts)
	}

	// One reaper tick is enough to remove a contract that was terminal at start of
	// tick — it drops from the store, so a subsequent Get returns not-found.
	rp := reaper.New(store, runtime.NewFake(), reaper.Options{
		Logger: discardLogger(),
		Now:    func() time.Time { return time.Unix(1_000_000_000, 0) },
	})
	rp.Tick(context.Background())

	if _, err := store.Get(terminal.ContractID); !errors.Is(err, contract.ErrNotFound) {
		t.Errorf("terminal contract still in store after cleanup tick: err=%v", err)
	}
	// The live contract is untouched by the cleanup sweep.
	if _, err := store.Get(live.ContractID); err != nil {
		t.Errorf("live contract missing after cleanup tick: %v", err)
	}
}
