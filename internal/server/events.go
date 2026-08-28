package server

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"

	"github.com/benjaminfkile/wisp/internal/bus"
	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/reaper"
)

// Wisp's own contract lifecycle event types (see docs/DESIGN.md §6). These are
// the only event types Wisp itself publishes; every other type on the bus is an
// opaque satellite string Wisp never interprets. contract.created / .ready /
// .released come from the contract flow; .expiring / .expired come from the
// reaper (see LifecycleNotify).
const (
	eventContractCreated  = "contract.created"
	eventContractReady    = "contract.ready"
	eventContractExpiring = "contract.expiring"
	eventContractReleased = "contract.released"
	eventContractExpired  = "contract.expired"
)

// eventsUpgrader upgrades the subscribe endpoint's HTTP handshake to a
// WebSocket. Like the shell, the loopback-bound default trusts any origin: the
// OS user is the outer boundary and the app token gates the inner surface (see
// docs/DESIGN.md §8), so CheckOrigin is permissive.
var eventsUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

// Reasons carried on a contract.expired payload's `reason` field, so a
// subscriber can distinguish a provisioning failure from a TTL expiry or an
// out-of-band container death. Only contract.expired currently carries a
// reason; the other lifecycle events omit the field.
const (
	// ExpiredReasonProvisioningFailed is set when the create path's fail()
	// destroys the container after a failed image pull, container create/start,
	// or userdata run. Distinguishes an authentic provisioning failure from a
	// lease that ran successfully and then expired.
	ExpiredReasonProvisioningFailed = "provisioning_failed"

	// ExpiredReasonTTLExpired is set when the reaper expires a lease whose TTL
	// has elapsed (the ordinary lifecycle end).
	ExpiredReasonTTLExpired = "ttl_expired"

	// ExpiredReasonContainerDied is set when the reaper expires a lease whose
	// backing container has stopped or been removed out of band (docker kill /
	// docker rm / OOM), detected by the liveness sweep before the TTL.
	ExpiredReasonContainerDied = "container_died"
)

// lifecyclePayload is the opaque body Wisp attaches to a contract lifecycle
// event. The bus does not interpret it; subscribers do. Reason is set only on
// contract.expired so subscribers can tell a provisioning failure apart from a
// TTL expiry or an out-of-band container death; other lifecycle events omit it.
type lifecyclePayload struct {
	ContractID string `json:"contract_id"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
}

// publishLifecycle emits a contract lifecycle event for c onto the bus. A
// marshal failure is logged and the event is dropped rather than propagated:
// lifecycle emission is best-effort and must never break the request path.
func (b *broker) publishLifecycle(eventType string, c contract.Contract) {
	publishLifecycle(b.bus, b.logger, eventType, c.ID, c.State, "")
}

// publishExpired is the contract.expired counterpart of publishLifecycle that
// carries the reason the lease reached the expired state so subscribers can
// distinguish provisioning failure from TTL/container death.
func (b *broker) publishExpired(contractID, reason string) {
	publishLifecycle(b.bus, b.logger, eventContractExpired, contractID, contract.StateExpired, reason)
}

// publishLifecycle marshals a lifecycle payload and publishes it under
// eventType. It is a package function (not just a broker method) so the reaper
// wiring (LifecycleNotify) can share it without a broker. reason is included on
// the payload only when non-empty (contract.expired carries it; the other
// lifecycle events do not).
func publishLifecycle(b *bus.Bus, logger *slog.Logger, eventType, contractID string, state contract.State, reason string) {
	data, err := json.Marshal(lifecyclePayload{ContractID: contractID, Status: string(state), Reason: reason})
	if err != nil {
		logger.Error("marshal lifecycle event", "type", eventType, "contract_id", contractID, "error", err)
		return
	}
	b.Publish(bus.Event{Type: eventType, Data: data})
}

// LifecycleNotify returns a reaper.Notify hook that republishes the reaper's
// time-based transitions as contract lifecycle events on the bus:
// expiring → contract.expiring, expired → contract.expired. Transitions with no
// corresponding lifecycle event are ignored. main wires this into the reaper so
// the bus announces the full lifecycle (see docs/DESIGN.md §6). A
// contract.expired carries the reaper Event's Reason (ttl_expired or
// container_died) so subscribers can tell a natural TTL expiry from an
// out-of-band container death, matching the provisioning-failure reason the
// broker publishes on the same event type.
func LifecycleNotify(b *bus.Bus, logger *slog.Logger) func(reaper.Event) {
	if logger == nil {
		logger = slog.Default()
	}
	return func(e reaper.Event) {
		eventType := lifecycleTypeFor(e.To)
		if eventType == "" {
			return
		}
		publishLifecycle(b, logger, eventType, e.ContractID, e.To, e.Reason)
	}
}

// lifecycleTypeFor maps a contract state to the lifecycle event type announced
// when a contract enters it, or "" if entering the state has no bus event.
func lifecycleTypeFor(s contract.State) string {
	switch s {
	case contract.StateExpiring:
		return eventContractExpiring
	case contract.StateExpired:
		return eventContractExpired
	case contract.StateReleased:
		return eventContractReleased
	case contract.StateReady:
		return eventContractReady
	default:
		return ""
	}
}

// publishRequest is the POST /events body: an opaque event to fan out to
// subscribers. Type routes the event (subscribers filter on it) and Data is the
// verbatim payload; Wisp interprets neither (see docs/DESIGN.md §6).
type publishRequest struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// publishEvent handles POST /events: it publishes an opaque event onto the bus.
// The call is gated by the app-level token (see docs/DESIGN.md §8, wired in
// routes). A missing or empty type is a client error; anything else is accepted
// and fanned out to matching subscribers.
func (b *broker) publishEvent(w http.ResponseWriter, r *http.Request) {
	var req publishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Type == "" {
		writeError(w, http.StatusBadRequest, "type must not be empty")
		return
	}
	b.bus.Publish(bus.Event{Type: req.Type, Data: req.Data})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "published"})
}

// subscribeEvents handles WS /events: it upgrades to a WebSocket and streams
// matching bus events to the client as JSON text messages. An optional ?type=
// query filters the stream to one or more comma-separated event types; with no
// filter the client receives every event.
//
// The handshake is gated by the app-level token (see docs/DESIGN.md §8),
// supplied either as an Authorization: Bearer header or a ?token= query
// parameter (browsers cannot set headers on a WebSocket handshake). A missing
// or invalid token is rejected with 401 before the upgrade.
func (b *broker) subscribeEvents(w http.ResponseWriter, r *http.Request) {
	if !b.appTokenOK(r) {
		writeError(w, http.StatusUnauthorized, "missing or invalid app token")
		return
	}

	var types []string
	if filter := r.URL.Query().Get("type"); filter != "" {
		for _, t := range strings.Split(filter, ",") {
			if t = strings.TrimSpace(t); t != "" {
				types = append(types, t)
			}
		}
	}

	sub := b.bus.Subscribe(types...)

	conn, err := eventsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response; just release the subscription.
		sub.Close()
		b.logger.Error("upgrade events websocket", "error", err)
		return
	}

	eventsBridge(conn, sub)
}

// eventsBridge streams a subscription's events to a WebSocket client until
// either side goes away. It writes each event as a JSON text message; a
// concurrent read pump drains client frames and detects disconnect, closing the
// subscription so the write loop unblocks. On return both the subscription and
// the connection are closed.
func eventsBridge(conn *websocket.Conn, sub *bus.Subscription) {
	defer conn.Close()
	defer sub.Close()

	// Reader pump: the subscribe stream is server→client only, but the client
	// may still send control frames (ping/close). Any read error means the
	// client is gone — close the subscription so the range loop below ends.
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				sub.Close()
				return
			}
		}
	}()

	for e := range sub.Events() {
		data, err := json.Marshal(e)
		if err != nil {
			continue
		}
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			return
		}
	}
}

// appTokenOK reports whether r carries the app-level token, or the gate is
// disabled (empty appToken). It accepts the token as an Authorization: Bearer
// header or a ?token= query parameter, so the bus WebSocket handshake works
// from clients that cannot set headers. The comparison is constant-time.
func (b *broker) appTokenOK(r *http.Request) bool {
	if b.appToken == "" {
		return true
	}
	if bearerMatches(r, b.appToken) {
		return true
	}
	if tok := r.URL.Query().Get("token"); tok != "" {
		return subtle.ConstantTimeCompare([]byte(tok), []byte(b.appToken)) == 1
	}
	return false
}
