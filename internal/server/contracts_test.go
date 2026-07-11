package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// testServer builds a handler wired to a fresh store and fake runtime, returning
// all three so tests can drive the API and inspect backend state.
func testServer(t *testing.T) (http.Handler, *contract.Store, *runtime.Fake) {
	t.Helper()
	store := contract.NewStore()
	fake := runtime.NewFake()
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake)
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

	created := createContract(t, h, `{"ttl_seconds":3600,"preset":"coding"}`)

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
		`{"ttl_seconds":3600,"preset":"coding","userdata":"#!/bin/sh\necho hi"}`)

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
	if fc.Image != defaultBaseImage {
		t.Errorf("image = %q, want %q", fc.Image, defaultBaseImage)
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

func TestGetReflectsState(t *testing.T) {
	h, _, _ := testServer(t)

	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":3600,"preset":"coding"}`)
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

	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":60,"preset":"probe"}`)
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

	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":3600,"preset":"coding"}`)
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
	for _, body := range []string{`{"ttl_seconds":0}`, `{"ttl_seconds":-5}`, `{"preset":"coding"}`} {
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
