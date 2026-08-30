package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"

	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// shellUpgrader upgrades the shell endpoint's HTTP handshake to a WebSocket.
// The default (127.0.0.1-bound) deployment trusts any origin; cross-origin
// gating is the outer OS-user boundary's job, and the contract token gates the
// inner surface (see docs/DESIGN.md §8), so CheckOrigin is permissive here.
var shellUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

// wsConn is the minimal subset of *websocket.Conn the shell bridge needs. It
// keeps shellBridge free of the concrete gorilla type so the pump logic can be
// unit-tested against a fake WebSocket and a fake duplex stream with no daemon.
type wsConn interface {
	// ReadMessage blocks for the next client message, returning its type and
	// payload; a non-nil error (e.g. a closed connection) ends the read side.
	ReadMessage() (messageType int, p []byte, err error)
	// WriteMessage delivers one message of the given type to the client.
	WriteMessage(messageType int, data []byte) error
	// Close tears down the connection, unblocking a pending ReadMessage.
	Close() error
}

// shellStream is the container-side duplex stream the bridge drives: raw byte
// I/O plus a TTY resize. It mirrors runtime.ShellStream but is declared locally
// so the bridge logic stays testable against a fake stream with no daemon.
type shellStream interface {
	io.ReadWriteCloser
	Resize(ctx context.Context, rows, cols uint16) error
}

// shellControl is a client→server control frame sent as a WebSocket *text*
// message, distinguishing it from the raw keystroke bytes that ride binary
// messages. Only "resize" is defined; it carries the new TTY dimensions in
// character cells. Text messages that are not a recognized control frame fall
// through to the shell's stdin, so pre-resize clients (which send only binary
// messages, or occasional text keystrokes) keep their exact old behavior.
type shellControl struct {
	Type string `json:"type"`
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// parseShellControl decodes a WebSocket text frame as a shell control message.
// It reports ok only when data is a JSON object carrying a recognized control
// type; anything else (non-JSON, unknown type) is treated as raw stdin bytes by
// the caller, preserving backward compatibility.
func parseShellControl(data []byte) (shellControl, bool) {
	var ctrl shellControl
	if err := json.Unmarshal(data, &ctrl); err != nil {
		return shellControl{}, false
	}
	if ctrl.Type != "resize" {
		return shellControl{}, false
	}
	return ctrl, true
}

// shell handles WS /contracts/:id/shell: it upgrades the connection to a
// WebSocket and bridges it to an interactive shell (a TTY exec) inside the
// contract's container - client bytes → shell stdin, shell output → client (see
// docs/DESIGN.md "How the shell works").
//
// The handshake requires the contract's bearer token, supplied either as a
// ?token= query parameter or as a `bearer.<token>` WebSocket subprotocol
// (browsers cannot set request headers on a WebSocket handshake, so the token
// rides the URL or the subprotocol rather than an Authorization header). All
// pre-upgrade rejections use plain HTTP status codes so a client that fails the
// handshake sees a normal response: unknown contract (404), missing/invalid
// token (401), or a contract that is not usable (409). A contract in the
// expiring lead window is still usable: the whole point of that window is to
// give the client time to exfiltrate work, so shells are accepted through it.
func (b *broker) shell(w http.ResponseWriter, r *http.Request) {
	c, err := b.store.Get(r.PathValue("id"))
	if errors.Is(err, contract.ErrNotFound) {
		writeError(w, http.StatusNotFound, "contract not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read contract")
		return
	}

	if !shellTokenMatches(r, c.Token) {
		writeError(w, http.StatusUnauthorized, "missing or invalid contract token")
		return
	}

	if c.State != contract.StateReady && c.State != contract.StateExpiring {
		writeError(w, http.StatusConflict, "contract not ready")
		return
	}

	stream, err := b.rt.ExecShell(r.Context(), c.ContainerID, runtime.DefaultShell(b.rt.ContainerOS()))
	if err != nil {
		b.logger.Error("open shell in container", "contract_id", c.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not open shell")
		return
	}

	conn, err := shellUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response; just release the stream.
		_ = stream.Close()
		b.logger.Error("upgrade shell websocket", "contract_id", c.ID, "error", err)
		return
	}

	if err := shellBridge(r.Context(), conn, stream); err != nil &&
		!errors.Is(err, io.EOF) && !websocket.IsCloseError(err,
		websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		b.logger.Info("shell bridge closed", "contract_id", c.ID, "error", err)
	}
}

// shellBridge pumps bytes both directions between an interactive WebSocket and
// a container shell's duplex stream: client messages → shell stdin, shell
// output → client binary messages. It runs two pumps until either side closes
// or errors, then tears both ends down (so the surviving pump unblocks) and
// returns the first error observed.
//
// Client binary messages are forwarded verbatim to the shell's stdin. A client
// text message that decodes as a shell control frame (currently only "resize")
// is handled out of band - a resize forwards the new dimensions to the TTY via
// stream.Resize instead of being written to stdin. Any text message that is not
// a recognized control frame is still written to stdin, so a client that never
// sends a control frame behaves exactly as before. ctx scopes the resize calls.
//
// It deliberately takes interfaces, not the concrete gorilla/Docker types, so
// its read/write-loop logic is unit-testable against a fake duplex stream and a
// fake WebSocket with no Docker daemon.
func shellBridge(ctx context.Context, ws wsConn, stream shellStream) error {
	errc := make(chan error, 2)

	// Shell output → client.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					errc <- werr
					return
				}
			}
			if err != nil {
				errc <- err
				return
			}
		}
	}()

	// Client input → shell stdin (with out-of-band resize control frames).
	go func() {
		for {
			mt, data, err := ws.ReadMessage()
			if mt == websocket.TextMessage {
				if ctrl, ok := parseShellControl(data); ok {
					// A resize is out of band: forward the new TTY window size
					// and do not write the control frame to stdin. A resize
					// failure (e.g. the exec already gone) must not tear down a
					// working byte stream, so the error is intentionally dropped.
					_ = stream.Resize(ctx, ctrl.Rows, ctrl.Cols)
					if err != nil {
						errc <- err
						return
					}
					continue
				}
			}
			if len(data) > 0 {
				if _, werr := stream.Write(data); werr != nil {
					errc <- werr
					return
				}
			}
			if err != nil {
				errc <- err
				return
			}
		}
	}()

	// Wait for the first pump to finish, then close both ends so the other
	// pump's blocking Read/ReadMessage returns and its goroutine exits.
	err := <-errc
	_ = stream.Close()
	_ = ws.Close()
	return err
}

// shellTokenMatches reports whether the handshake carries the contract token,
// supplied as a ?token= query parameter or a `bearer.<token>` WebSocket
// subprotocol. The comparison is constant-time to avoid leaking the token
// through timing (see docs/DESIGN.md §8).
func shellTokenMatches(r *http.Request, want string) bool {
	if tok := r.URL.Query().Get("token"); tok != "" {
		return subtle.ConstantTimeCompare([]byte(tok), []byte(want)) == 1
	}
	const prefix = "bearer."
	for _, proto := range websocket.Subprotocols(r) {
		if strings.HasPrefix(proto, prefix) {
			tok := strings.TrimPrefix(proto, prefix)
			return subtle.ConstantTimeCompare([]byte(tok), []byte(want)) == 1
		}
	}
	return false
}
