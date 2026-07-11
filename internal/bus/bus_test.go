package bus

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"
)

// quietBus returns a bus whose logs are discarded, keeping test output clean.
func quietBus() *Bus {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// recv reads one event from sub within a short deadline, failing the test if
// none arrives.
func recv(t *testing.T, sub *Subscription) Event {
	t.Helper()
	select {
	case e, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription channel closed unexpectedly")
		}
		return e
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}

// mustNotRecv asserts no event arrives on sub within a short window.
func mustNotRecv(t *testing.T, sub *Subscription) {
	t.Helper()
	select {
	case e := <-sub.Events():
		t.Fatalf("unexpected event %q", e.Type)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestPublishReachesSubscriber: an event published after subscribing is
// delivered verbatim, payload intact.
func TestPublishReachesSubscriber(t *testing.T) {
	b := quietBus()
	sub := b.Subscribe()
	defer sub.Close()

	b.Publish(Event{Type: "task.start", Data: json.RawMessage(`{"id":7}`)})

	got := recv(t, sub)
	if got.Type != "task.start" {
		t.Errorf("type = %q, want task.start", got.Type)
	}
	if string(got.Data) != `{"id":7}` {
		t.Errorf("data = %s, want {\"id\":7}", got.Data)
	}
}

// TestPublishFanOut: every subscriber receives every unfiltered event.
func TestPublishFanOut(t *testing.T) {
	b := quietBus()
	a := b.Subscribe()
	defer a.Close()
	c := b.Subscribe()
	defer c.Close()

	b.Publish(Event{Type: "ping"})

	if got := recv(t, a); got.Type != "ping" {
		t.Errorf("subscriber a got %q, want ping", got.Type)
	}
	if got := recv(t, c); got.Type != "ping" {
		t.Errorf("subscriber c got %q, want ping", got.Type)
	}
}

// TestTypeFilterHonored: a subscriber with a type filter receives only matching
// events; non-matching events are not delivered.
func TestTypeFilterHonored(t *testing.T) {
	b := quietBus()
	sub := b.Subscribe("contract.ready")
	defer sub.Close()

	b.Publish(Event{Type: "contract.created"}) // filtered out
	b.Publish(Event{Type: "contract.ready"})   // delivered

	got := recv(t, sub)
	if got.Type != "contract.ready" {
		t.Fatalf("type = %q, want contract.ready", got.Type)
	}
	mustNotRecv(t, sub)
}

// TestMultiTypeFilter: a subscriber may name several types and receives any of
// them.
func TestMultiTypeFilter(t *testing.T) {
	b := quietBus()
	sub := b.Subscribe("contract.expiring", "contract.expired")
	defer sub.Close()

	b.Publish(Event{Type: "contract.ready"}) // not in filter
	b.Publish(Event{Type: "contract.expired"})

	if got := recv(t, sub); got.Type != "contract.expired" {
		t.Fatalf("type = %q, want contract.expired", got.Type)
	}
}

// TestCloseUnsubscribes: after Close a subscriber's channel is closed and later
// publishes are not delivered to it.
func TestCloseUnsubscribes(t *testing.T) {
	b := quietBus()
	sub := b.Subscribe()

	sub.Close()

	// The channel is closed: a receive returns the zero value with ok=false.
	select {
	case _, ok := <-sub.Events():
		if ok {
			t.Fatal("expected channel to be closed after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for closed channel")
	}

	// Publishing after Close must not panic (no send on the closed channel).
	b.Publish(Event{Type: "after.close"})

	// Close is idempotent.
	sub.Close()
}

// TestCloseIsConcurrencySafe exercises Close racing with Publish under the race
// detector: no send on a closed channel, no double close.
func TestCloseIsConcurrencySafe(t *testing.T) {
	b := quietBus()
	for i := 0; i < 100; i++ {
		sub := b.Subscribe()
		done := make(chan struct{})
		go func() {
			b.Publish(Event{Type: "race"})
			close(done)
		}()
		sub.Close()
		<-done
	}
}
