package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benjaminfkile/wisp/internal/bus"
	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/policy"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// A Wisp container must stay alive for the whole contract so exec/shell can
// attach. createOptions must therefore always set a keep-alive command,
// regardless of the launch spec — otherwise a bare base image's default command
// exits, the container stops, and provisioning/exec fails with "container is
// not running". This guards against silently dropping the keep-alive.
func TestCreateOptionsAlwaysSetsKeepAliveCmd(t *testing.T) {
	// A zero-value spec (no image/limits/network) must still get the keep-alive.
	opts := createOptions(launchSpec{}, contract.Contract{ID: "contract-abc"}, runtime.OSLinux)

	if len(opts.Cmd) == 0 {
		t.Fatal("createOptions returned an empty Cmd; the container would exit immediately")
	}
	if !reflect.DeepEqual(opts.Cmd, keepAliveCmd(runtime.OSLinux)) {
		t.Fatalf("createOptions Cmd = %v, want keep-alive %v", opts.Cmd, keepAliveCmd(runtime.OSLinux))
	}
}

// The keep-alive command must be chosen for the daemon's container OS: the Linux
// `tail -f /dev/null` executable does not exist on Windows, so a Windows-mode
// runtime must launch a Windows-runnable idle command instead. Linux stays
// byte-identical to its long-standing command.
func TestCreateOptionsKeepAliveByOS(t *testing.T) {
	tests := []struct {
		name string
		os   runtime.ContainerOS
		want []string
	}{
		{"linux", runtime.OSLinux, []string{"tail", "-f", "/dev/null"}},
		{"windows", runtime.OSWindows, []string{"cmd", "/c", "ping -t localhost >NUL"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := createOptions(launchSpec{}, contract.Contract{ID: "contract-abc"}, tt.os)
			if !reflect.DeepEqual(opts.Cmd, tt.want) {
				t.Fatalf("createOptions(%s) Cmd = %v, want %v", tt.os, opts.Cmd, tt.want)
			}
			// WorkingDir, user, and entrypoint stay unset so each OS uses its
			// own container default rather than a POSIX-only path like /workspace.
			if opts.WorkingDir != "" {
				t.Fatalf("createOptions(%s) WorkingDir = %q, want empty (OS default)", tt.os, opts.WorkingDir)
			}
		})
	}
}

// End-to-end guard: provisioning a contract against a Windows-mode runtime must
// launch the container with the Windows keep-alive command, proving the broker
// wires the daemon's detected ContainerOS through to create. Uses the fake
// runtime with a stubbed OS — no real Windows Docker daemon.
func TestProvisionUsesRuntimeContainerOS(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	fake.OS = runtime.OSWindows
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, policy.Default(), bus.New(nil), "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/contracts", strings.NewReader(`{"ttl_seconds":3600}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp createResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	c, err := store.Get(resp.ContractID)
	if err != nil {
		t.Fatalf("contract %s not found: %v", resp.ContractID, err)
	}
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatalf("container %s not tracked", c.ContainerID)
	}
	want := []string{"cmd", "/c", "ping -t localhost >NUL"}
	if !reflect.DeepEqual(fc.Opts.Cmd, want) {
		t.Fatalf("windows-mode create Cmd = %v, want %v", fc.Opts.Cmd, want)
	}
}
