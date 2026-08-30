package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/benjaminfkile/wisp/internal/bus"
	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/policy"
	"github.com/benjaminfkile/wisp/internal/reaper"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// wsEvent is the shape a subscriber reads off WS /events: a bus event framed as
// JSON. Data is left raw so tests can assert on the opaque payload.
type wsEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// dialEvents opens a WS /events subscription against srv, applying an optional
// ?type= filter (empty means no filter). The caller closes the returned conn.
func dialEvents(t *testing.T, srv *httptest.Server, filter string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/events"
	if filter != "" {
		url += "?type=" + filter
	}
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	return conn
}

// readEvent reads and decodes the next event from conn within a deadline.
func readEvent(t *testing.T, conn *websocket.Conn) wsEvent {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	var e wsEvent
	if err := json.Unmarshal(msg, &e); err != nil {
		t.Fatalf("decode event %q: %v", msg, err)
	}
	return e
}

// TestEventsPublishSubscribe: an event POSTed to /events is delivered to a WS
// /events subscriber verbatim - acceptance criterion "publish -> subscribe
// delivery works".
func TestEventsPublishSubscribe(t *testing.T) {
	h, _, _ := testServer(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialEvents(t, srv, "")
	defer conn.Close()

	// The subscription is registered before Dial returns, so this publish is seen.
	resp, err := http.Post(srv.URL+"/events", "application/json",
		strings.NewReader(`{"type":"task.start","data":{"repo":"acme"}}`))
	if err != nil {
		t.Fatalf("post event: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("publish status = %d, want 202", resp.StatusCode)
	}

	got := readEvent(t, conn)
	if got.Type != "task.start" {
		t.Errorf("type = %q, want task.start", got.Type)
	}
	if string(got.Data) != `{"repo":"acme"}` {
		t.Errorf("data = %s, want {\"repo\":\"acme\"}", got.Data)
	}
}

// TestEventsTypeFilterHonored: a subscriber with ?type= receives only matching
// events - acceptance criterion "type filter honored".
func TestEventsTypeFilterHonored(t *testing.T) {
	h, _, _ := testServer(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialEvents(t, srv, "wanted.type")
	defer conn.Close()

	post := func(body string) {
		resp, err := http.Post(srv.URL+"/events", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post event: %v", err)
		}
		resp.Body.Close()
	}
	post(`{"type":"other.type"}`)  // filtered out
	post(`{"type":"wanted.type"}`) // delivered

	got := readEvent(t, conn)
	if got.Type != "wanted.type" {
		t.Fatalf("type = %q, want wanted.type (filter leaked other.type?)", got.Type)
	}
}

// TestEventsPublishRequiresAppToken: POST /events is gated by the app token.
func TestEventsPublishRequiresAppToken(t *testing.T) {
	h := tokenServer(t, "app-secret")
	body := `{"type":"x"}`

	if rec := do(t, h, http.MethodPost, "/events", body); rec.Code != http.StatusUnauthorized {
		t.Errorf("no-token publish status = %d, want 401", rec.Code)
	}
	if rec := doAuth(t, h, http.MethodPost, "/events", "wrong", body); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad-token publish status = %d, want 401", rec.Code)
	}
	if rec := doAuth(t, h, http.MethodPost, "/events", "app-secret", body); rec.Code != http.StatusAccepted {
		t.Errorf("good-token publish status = %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestEventsPublishValidatesType: an event without a type is a 400.
func TestEventsPublishValidatesType(t *testing.T) {
	h, _, _ := testServer(t)
	if rec := do(t, h, http.MethodPost, "/events", `{"data":{}}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty-type publish status = %d, want 400", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/events", `not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad-json publish status = %d, want 400", rec.Code)
	}
}

// TestEventsSubscribeRequiresAppToken: the WS /events handshake is gated by the
// app token, accepted here via ?token= since a WebSocket cannot set headers.
func TestEventsSubscribeRequiresAppToken(t *testing.T) {
	h := tokenServer(t, "app-secret")

	// Pre-upgrade rejection: no token → 401 before any WebSocket upgrade.
	if rec := do(t, h, http.MethodGet, "/events", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no-token subscribe status = %d, want 401", rec.Code)
	}

	srv := httptest.NewServer(h)
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http") + "/events"

	// Wrong token: handshake fails.
	if _, resp, err := websocket.DefaultDialer.Dial(base+"?token=nope", nil); err == nil {
		t.Error("expected handshake with bad token to fail")
	} else if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad-token handshake status = %v, want 401", resp)
	}

	// Correct token: handshake succeeds.
	conn, _, err := websocket.DefaultDialer.Dial(base+"?token=app-secret", nil)
	if err != nil {
		t.Fatalf("dial with valid token: %v", err)
	}
	conn.Close()
}

// TestLifecycleEventsEmitted: creating a contract emits contract.created then
// contract.ready, and releasing it emits contract.released - acceptance
// criterion "lifecycle events emitted on create/ready/release".
func TestLifecycleEventsEmitted(t *testing.T) {
	h, _, _ := testServer(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialEvents(t, srv, "")
	defer conn.Close()

	created := createContract(t, h, `{"ttl_seconds":3600}`)

	if got := readEvent(t, conn); got.Type != eventContractCreated {
		t.Fatalf("first event = %q, want %q", got.Type, eventContractCreated)
	}
	ready := readEvent(t, conn)
	if ready.Type != eventContractReady {
		t.Fatalf("second event = %q, want %q", ready.Type, eventContractReady)
	}
	// The lifecycle payload carries the contract id and its new status.
	var payload lifecyclePayload
	if err := json.Unmarshal(ready.Data, &payload); err != nil {
		t.Fatalf("decode ready payload: %v", err)
	}
	if payload.ContractID != created.ContractID {
		t.Errorf("ready payload contract_id = %q, want %q", payload.ContractID, created.ContractID)
	}
	if payload.Status != string(contract.StateReady) {
		t.Errorf("ready payload status = %q, want %q", payload.Status, contract.StateReady)
	}

	// Release emits contract.released.
	rec := doAuth(t, h, http.MethodDelete, "/contracts/"+created.ContractID, created.Token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("release status = %d, want 200", rec.Code)
	}
	if got := readEvent(t, conn); got.Type != eventContractReleased {
		t.Fatalf("release event = %q, want %q", got.Type, eventContractReleased)
	}
}

// TestLifecycleNotify: the reaper hook maps time-based transitions to the right
// lifecycle events on the bus, covering the expiring/expired criterion without
// driving a real clock. The two expired paths (TTL and container death) each
// carry the matching reason on the payload so a subscriber can tell them apart.
func TestLifecycleNotify(t *testing.T) {
	b := bus.New(nil)
	sub := b.Subscribe()
	defer sub.Close()

	notify := LifecycleNotify(b, nil)

	notify(reaper.Event{ContractID: "c1", From: contract.StateReady, To: contract.StateExpiring})
	notify(reaper.Event{ContractID: "c1", From: contract.StateExpiring, To: contract.StateExpired, Reason: reaper.ReasonTTLExpired})
	notify(reaper.Event{ContractID: "c2", From: contract.StateReady, To: contract.StateExpired, Reason: reaper.ReasonContainerDied})
	// A transition with no lifecycle event (e.g. into provisioning) is ignored.
	notify(reaper.Event{ContractID: "c1", From: contract.StateRequested, To: contract.StateProvisioning})

	want := []struct {
		eventType string
		reason    string
	}{
		{eventContractExpiring, ""},
		{eventContractExpired, ExpiredReasonTTLExpired},
		{eventContractExpired, ExpiredReasonContainerDied},
	}
	for i, w := range want {
		select {
		case e := <-sub.Events():
			if e.Type != w.eventType {
				t.Errorf("event %d = %q, want %q", i, e.Type, w.eventType)
			}
			var p lifecyclePayload
			if err := json.Unmarshal(e.Data, &p); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			if p.Reason != w.reason {
				t.Errorf("event %d reason = %q, want %q", i, p.Reason, w.reason)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %d (%s)", i, w.eventType)
		}
	}

	// The ignored transition produced no event.
	select {
	case e := <-sub.Events():
		t.Fatalf("unexpected event %q", e.Type)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestLifecycleEventOmitsReasonForNonExpired: contract.created / .ready /
// .released / .expiring payloads never carry a reason field so their wire shape
// is byte-for-byte unchanged for existing subscribers.
func TestLifecycleEventOmitsReasonForNonExpired(t *testing.T) {
	h, _, _ := testServer(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialEvents(t, srv, "")
	defer conn.Close()

	created := createContract(t, h, `{"ttl_seconds":3600}`)

	// The first two events are contract.created and contract.ready.
	for i := 0; i < 2; i++ {
		e := readEvent(t, conn)
		if strings.Contains(string(e.Data), `"reason"`) {
			t.Errorf("event %d (%s) carries reason: %s", i, e.Type, e.Data)
		}
	}

	// Release emits contract.released, also without a reason.
	rec := doAuth(t, h, http.MethodDelete, "/contracts/"+created.ContractID, created.Token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("release status = %d, want 200", rec.Code)
	}
	rel := readEvent(t, conn)
	if strings.Contains(string(rel.Data), `"reason"`) {
		t.Errorf("released event carries reason: %s", rel.Data)
	}
}

// TestProvisioningFailurePublishesExpired: a create whose provisioning fails
// (e.g. userdata exiting non-zero) publishes contract.expired on the bus with
// reason=provisioning_failed, so a subscriber learns about the failure through
// the same lifecycle channel a TTL / container death expiry uses.
func TestProvisioningFailurePublishesExpired(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	// userdata exits non-zero: provisioning fails, the container is destroyed,
	// and the contract is marked expired.
	fake.ExecHandler = func(id string, cmd []string) (runtime.ExecResult, error) {
		return runtime.ExecResult{ExitCode: 1, Stderr: "boom"}, nil
	}
	eventBus := bus.New(nil)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, policy.Default(), eventBus, "")

	// Subscribe BEFORE the create so no event races the reader.
	sub := eventBus.Subscribe(eventContractExpired)
	defer sub.Close()

	rec := do(t, h, http.MethodPost, "/contracts",
		`{"ttl_seconds":60,"userdata":"exit 1"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("create status = %d, want 500 on failed userdata (body: %s)", rec.Code, rec.Body.String())
	}

	select {
	case e := <-sub.Events():
		if e.Type != eventContractExpired {
			t.Fatalf("event type = %q, want %q", e.Type, eventContractExpired)
		}
		var p lifecyclePayload
		if err := json.Unmarshal(e.Data, &p); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if p.Status != string(contract.StateExpired) {
			t.Errorf("status = %q, want %q", p.Status, contract.StateExpired)
		}
		if p.Reason != ExpiredReasonProvisioningFailed {
			t.Errorf("reason = %q, want %q", p.Reason, ExpiredReasonProvisioningFailed)
		}
		// The contract id in the payload matches the one that failed.
		all := store.List()
		if len(all) != 1 {
			t.Fatalf("stored contracts = %d, want 1", len(all))
		}
		if p.ContractID != all[0].ID {
			t.Errorf("payload contract_id = %q, want %q", p.ContractID, all[0].ID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for contract.expired after provisioning failure")
	}
}

// TestProvisioningFailurePublishesExpiredOnImagePull: the same expired event
// with reason=provisioning_failed fires when the image pull itself fails,
// covering the other broker.fail() entry points besides userdata.
func TestProvisioningFailurePublishesExpiredOnImagePull(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	fake.EnsureImageErr = errors.New("pull failed")
	eventBus := bus.New(nil)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, policy.Default(), eventBus, "")

	sub := eventBus.Subscribe(eventContractExpired)
	defer sub.Close()

	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":60}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("create status = %d, want 500 on failed image pull (body: %s)", rec.Code, rec.Body.String())
	}

	select {
	case e := <-sub.Events():
		var p lifecyclePayload
		if err := json.Unmarshal(e.Data, &p); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if p.Reason != ExpiredReasonProvisioningFailed {
			t.Errorf("reason = %q, want %q", p.Reason, ExpiredReasonProvisioningFailed)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for contract.expired after failed image pull")
	}
}
