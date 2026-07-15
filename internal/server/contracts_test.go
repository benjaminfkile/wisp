package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benjaminfkile/wisp/internal/bus"
	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/policy"
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
		`{"ttl_seconds":3600,"env":{"CLAUDE_CODE_OAUTH_TOKEN":"abc","FOO":"bar"}}`)

	c, _ := store.Get(created.ContractID)
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatal("container not tracked")
	}
	// The env map reached the container's Config.Env in KEY=VALUE form (sorted).
	want := []string{"CLAUDE_CODE_OAUTH_TOKEN=abc", "FOO=bar"}
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
		`{"ttl_seconds":60,"env":{"":"v"}}`,        // empty key
		`{"ttl_seconds":60,"env":{"A=B":"v"}}`,     // '=' in key
		`{"ttl_seconds":60,"env":{"A\u0000B":"v"}}`, // NUL in key
		`{"ttl_seconds":60,"env":{"A":"v\u0000w"}}`,  // NUL in value
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
