package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/benjaminfkile/wisp/internal/runtime"
)

// fakeDuplex is an in-memory stand-in for a container's hijacked TTY stream. The
// bridge reads container output from reads and writes shell stdin into wrote; no
// Docker daemon is involved.
type fakeDuplex struct {
	reads   chan []byte    // test → bridge: bytes the shell "produces"
	wrote   chan []byte    // bridge → test: bytes sent to the shell's stdin
	resizes chan [2]uint16 // bridge → test: {rows, cols} forwarded via Resize
	closed  chan struct{}  // closed by Close, unblocking a pending Read
	once    sync.Once
}

func newFakeDuplex() *fakeDuplex {
	return &fakeDuplex{
		reads:   make(chan []byte),
		wrote:   make(chan []byte, 16),
		resizes: make(chan [2]uint16, 16),
		closed:  make(chan struct{}),
	}
}

func (s *fakeDuplex) Resize(_ context.Context, rows, cols uint16) error {
	select {
	case s.resizes <- [2]uint16{rows, cols}:
		return nil
	case <-s.closed:
		return io.ErrClosedPipe
	}
}

func (s *fakeDuplex) Read(p []byte) (int, error) {
	select {
	case b := <-s.reads:
		return copy(p, b), nil
	case <-s.closed:
		return 0, io.EOF
	}
}

func (s *fakeDuplex) Write(p []byte) (int, error) {
	b := append([]byte(nil), p...)
	select {
	case s.wrote <- b:
		return len(p), nil
	case <-s.closed:
		return 0, io.ErrClosedPipe
	}
}

func (s *fakeDuplex) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// fakeWS is an in-memory stand-in for the WebSocket side of the bridge.
type fakeWS struct {
	reads   chan []byte   // test → bridge: client keystrokes (binary messages)
	texts   chan []byte   // test → bridge: client text messages (control frames)
	written chan []byte   // bridge → test: bytes delivered to the client
	closed  chan struct{} // closed by Close, unblocking a pending ReadMessage
	once    sync.Once
}

func newFakeWS() *fakeWS {
	return &fakeWS{
		reads:   make(chan []byte),
		texts:   make(chan []byte),
		written: make(chan []byte, 16),
		closed:  make(chan struct{}),
	}
}

func (w *fakeWS) ReadMessage() (int, []byte, error) {
	select {
	case b, ok := <-w.reads:
		if !ok {
			return 0, nil, io.EOF
		}
		return websocket.BinaryMessage, b, nil
	case b, ok := <-w.texts:
		if !ok {
			return 0, nil, io.EOF
		}
		return websocket.TextMessage, b, nil
	case <-w.closed:
		return 0, nil, io.EOF
	}
}

func (w *fakeWS) WriteMessage(_ int, data []byte) error {
	b := append([]byte(nil), data...)
	select {
	case w.written <- b:
		return nil
	case <-w.closed:
		return io.ErrClosedPipe
	}
}

func (w *fakeWS) Close() error {
	w.once.Do(func() { close(w.closed) })
	return nil
}

// TestShellBridgePumpsBothDirections drives shellBridge against a fake duplex
// stream and a fake WebSocket — no Docker daemon — asserting bytes flow shell →
// client and client → shell, and that a client disconnect tears the bridge down.
func TestShellBridgePumpsBothDirections(t *testing.T) {
	ws := newFakeWS()
	stream := newFakeDuplex()

	done := make(chan error, 1)
	go func() { done <- shellBridge(context.Background(), ws, stream) }()

	// Shell output → client.
	stream.reads <- []byte("motd\n")
	select {
	case got := <-ws.written:
		if string(got) != "motd\n" {
			t.Fatalf("client received %q, want %q", got, "motd\n")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for shell output to reach client")
	}

	// Client input → shell stdin.
	ws.reads <- []byte("ls -la\n")
	select {
	case got := <-stream.wrote:
		if string(got) != "ls -la\n" {
			t.Fatalf("shell stdin got %q, want %q", got, "ls -la\n")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for client input to reach shell stdin")
	}

	// Client disconnects: the read pump sees EOF and the bridge tears down.
	close(ws.reads)
	select {
	case err := <-done:
		if err != io.EOF {
			t.Fatalf("bridge returned %v, want io.EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("bridge did not return after client disconnect")
	}

	// Both ends were closed so the surviving pump could unblock.
	select {
	case <-stream.closed:
	default:
		t.Error("bridge did not close the container stream")
	}
}

// TestShellBridgeResizeControlFrame checks the resize control channel: a text
// message carrying a resize control frame is forwarded to the stream's Resize
// (not written to stdin), while an unrecognized text message and binary
// messages still flow to stdin as raw bytes — the backward-compatible path.
func TestShellBridgeResizeControlFrame(t *testing.T) {
	ws := newFakeWS()
	stream := newFakeDuplex()

	done := make(chan error, 1)
	go func() { done <- shellBridge(context.Background(), ws, stream) }()

	// A resize control frame → stream.Resize, not stdin.
	ws.texts <- []byte(`{"type":"resize","rows":40,"cols":120}`)
	select {
	case got := <-stream.resizes:
		if got != [2]uint16{40, 120} {
			t.Fatalf("resize got %v, want {40 120}", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for resize to reach the stream")
	}

	// A text message that is not a control frame falls through to stdin,
	// preserving the pre-resize behavior for raw text keystrokes.
	ws.texts <- []byte("plain text\n")
	select {
	case got := <-stream.wrote:
		if string(got) != "plain text\n" {
			t.Fatalf("stdin got %q, want %q", got, "plain text\n")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for non-control text to reach stdin")
	}

	// Binary input is unaffected: it still reaches stdin verbatim.
	ws.reads <- []byte("echo hi\n")
	select {
	case got := <-stream.wrote:
		if string(got) != "echo hi\n" {
			t.Fatalf("stdin got %q, want %q", got, "echo hi\n")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for binary input to reach stdin")
	}

	// A resize must not appear on the stdin stream.
	select {
	case got := <-stream.wrote:
		t.Fatalf("resize control frame leaked to stdin as %q", got)
	default:
	}

	stream.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bridge did not return after stream close")
	}
}

// TestShellHandshakeRequiresToken checks the pre-upgrade token gate directly:
// a handshake without a valid contract token is rejected before any upgrade.
func TestShellHandshakeRequiresToken(t *testing.T) {
	h, _, _ := testServer(t)
	created := createContract(t, h, `{"ttl_seconds":3600}`)

	// No token.
	rec := do(t, h, http.MethodGet, "/contracts/"+created.ContractID+"/shell", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", rec.Code)
	}

	// Wrong token.
	rec = do(t, h, http.MethodGet, "/contracts/"+created.ContractID+"/shell?token=nope", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-token status = %d, want 401", rec.Code)
	}

	// Unknown contract.
	rec = do(t, h, http.MethodGet, "/contracts/does-not-exist/shell?token=x", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown-contract status = %d, want 404", rec.Code)
	}
}

// TestShellEndToEnd drives a real WebSocket handshake through the server and the
// bridge, backed by the fake runtime's shell stream (net.Pipe, no daemon): the
// valid token is accepted, an invalid one is refused, and bytes flow both ways.
func TestShellEndToEnd(t *testing.T) {
	h, _, fake := testServer(t)

	containerEnd, testEnd := net.Pipe()
	defer testEnd.Close()
	var containerID string
	fake.ShellHandler = func(id string, cmd []string) (io.ReadWriteCloser, error) {
		containerID = id
		return containerEnd, nil
	}

	created := createContract(t, h, `{"ttl_seconds":3600}`)

	srv := httptest.NewServer(h)
	defer srv.Close()
	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http")
	shellURL := wsBase + "/contracts/" + created.ContractID + "/shell"

	// Invalid token: the handshake fails with 401.
	if _, resp, err := websocket.DefaultDialer.Dial(shellURL+"?token=wrong", nil); err == nil {
		t.Fatal("expected handshake with bad token to fail")
	} else if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad-token handshake status = %v, want 401", resp)
	}

	// Valid token: the handshake succeeds.
	conn, _, err := websocket.DefaultDialer.Dial(shellURL+"?token="+created.Token, nil)
	if err != nil {
		t.Fatalf("dial with valid token: %v", err)
	}
	defer conn.Close()

	// Shell output → client.
	go func() { _, _ = testEnd.Write([]byte("hello-from-shell")) }()
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read shell output: %v", err)
	}
	if string(msg) != "hello-from-shell" {
		t.Fatalf("client got %q, want %q", msg, "hello-from-shell")
	}

	// Client input → shell stdin.
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("id\n")); err != nil {
		t.Fatalf("write to shell: %v", err)
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(testEnd, buf); err != nil {
		t.Fatalf("read shell stdin: %v", err)
	}
	if string(buf) != "id\n" {
		t.Fatalf("shell stdin got %q, want %q", buf, "id\n")
	}

	// A resize control frame (a text message) reaches the runtime's TTY resize
	// and does not leak into stdin. The fake records forwarded resizes on the
	// container; poll briefly since the bridge processes the frame async.
	if err := conn.WriteMessage(websocket.TextMessage,
		[]byte(`{"type":"resize","rows":50,"cols":132}`)); err != nil {
		t.Fatalf("write resize control frame: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var resizes [][2]uint16
	for time.Now().Before(deadline) {
		if resizes = fake.Resizes(containerID); len(resizes) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(resizes) != 1 || resizes[0] != [2]uint16{50, 132} {
		t.Fatalf("recorded resizes = %v, want [{50 132}]", resizes)
	}
}

// The interactive shell must launch the OS-appropriate default binary: `/bin/sh`
// on a Linux daemon (unchanged) and `cmd.exe` on a Windows daemon, which has no
// /bin/sh. Uses the fake runtime with a stubbed OS — no real Windows Docker.
func TestShellDefaultBinaryByOS(t *testing.T) {
	tests := []struct {
		name string
		os   runtime.ContainerOS
		want []string
	}{
		{"linux", runtime.OSLinux, []string{"/bin/sh"}},
		{"windows", runtime.OSWindows, []string{"cmd.exe"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, fake := testServer(t)
			fake.OS = tt.os

			gotCmd := make(chan []string, 1)
			containerEnd, testEnd := net.Pipe()
			defer testEnd.Close()
			fake.ShellHandler = func(id string, cmd []string) (io.ReadWriteCloser, error) {
				gotCmd <- cmd
				return containerEnd, nil
			}

			created := createContract(t, h, `{"ttl_seconds":3600}`)

			srv := httptest.NewServer(h)
			defer srv.Close()
			wsBase := "ws" + strings.TrimPrefix(srv.URL, "http")
			shellURL := wsBase + "/contracts/" + created.ContractID + "/shell?token=" + created.Token

			conn, _, err := websocket.DefaultDialer.Dial(shellURL, nil)
			if err != nil {
				t.Fatalf("dial shell: %v", err)
			}
			defer conn.Close()

			select {
			case cmd := <-gotCmd:
				if len(cmd) != len(tt.want) || cmd[0] != tt.want[0] {
					t.Fatalf("shell cmd = %v, want %v", cmd, tt.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("ExecShell was not invoked")
			}
		})
	}
}
