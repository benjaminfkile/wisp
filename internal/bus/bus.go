// Package bus is Wisp's in-process pub/sub event bus: a dumb coordination layer
// between satellites and the channel Wisp announces its own contract lifecycle
// on (see docs/DESIGN.md §6). Clients publish opaque events and subscribe to a
// live fan-out, optionally filtered by event type.
//
// The bus is deliberately separate from the live session layer (exec/shell):
// per docs/DESIGN.md §2, the bus is async coordination between apps (control
// plane) and is never what a contract's session is built on. It interprets
// neither an event's type string nor its payload - both are opaque.
//
// Delivery is fire-and-forget in-process fan-out. Each subscriber has a
// buffered channel; a subscriber that cannot keep up has events dropped rather
// than stalling the publisher or other subscribers. Optional persistence /
// replay (docs/DESIGN.md §6, §14) is a later concern.
package bus

import (
	"encoding/json"
	"log/slog"
	"sync"
)

// defaultBuffer is the per-subscriber channel depth. A subscriber that falls
// this far behind has further events dropped (see Publish).
const defaultBuffer = 64

// Event is a single message on the bus. Both fields are opaque to Wisp: Type is
// used only for subscriber filtering (never interpreted), and Data is the raw
// payload carried through untouched.
type Event struct {
	// Type names the event, e.g. "contract.ready" or a satellite's own string.
	// The bus routes on it but never interprets it.
	Type string `json:"type"`

	// Data is the opaque payload. It is preserved verbatim as raw JSON so the
	// bus neither parses nor re-shapes what publishers send.
	Data json.RawMessage `json:"data,omitempty"`
}

// Subscription is a live feed of matching events. Read from Events until it is
// closed; call Close to unsubscribe (idempotent). The channel is closed by
// Close, so a range over Events terminates cleanly on unsubscribe.
type Subscription struct {
	id    int
	types map[string]bool // nil/empty means "every type"
	ch    chan Event
	bus   *Bus

	closeOnce sync.Once
}

// Events returns the receive-only channel this subscription delivers on. It is
// closed when the subscription is closed.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Close unsubscribes and closes the Events channel. It is safe to call more
// than once and from any goroutine.
func (s *Subscription) Close() {
	s.closeOnce.Do(func() {
		s.bus.remove(s.id)
		close(s.ch)
	})
}

// matches reports whether an event of the given type should be delivered to
// this subscription. An empty type filter matches everything.
func (s *Subscription) matches(t string) bool {
	if len(s.types) == 0 {
		return true
	}
	return s.types[t]
}

// Bus is an in-process pub/sub hub. The zero value is not usable; call New. All
// methods are safe for concurrent use.
type Bus struct {
	mu     sync.Mutex
	subs   map[int]*Subscription
	nextID int
	logger *slog.Logger
}

// New returns an empty bus. logger receives operational logs (e.g. drop
// warnings); a nil logger falls back to slog.Default.
func New(logger *slog.Logger) *Bus {
	if logger == nil {
		logger = slog.Default()
	}
	return &Bus{
		subs:   make(map[int]*Subscription),
		logger: logger,
	}
}

// Subscribe registers a new subscriber and returns its Subscription. If types
// is non-empty the subscriber receives only events whose Type is in the set;
// with no types it receives every event. Empty strings in types are ignored.
// Call Subscription.Close to unsubscribe.
func (b *Bus) Subscribe(types ...string) *Subscription {
	b.mu.Lock()
	defer b.mu.Unlock()

	var filter map[string]bool
	if len(types) > 0 {
		filter = make(map[string]bool, len(types))
		for _, t := range types {
			if t != "" {
				filter[t] = true
			}
		}
	}

	s := &Subscription{
		id:    b.nextID,
		types: filter,
		ch:    make(chan Event, defaultBuffer),
		bus:   b,
	}
	b.subs[s.id] = s
	b.nextID++
	return s
}

// Publish fans e out to every subscriber whose filter matches e.Type. Delivery
// is non-blocking: a subscriber whose buffer is full has the event dropped (and
// logged) so one slow consumer cannot stall the publisher or other consumers.
func (b *Bus) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, s := range b.subs {
		if !s.matches(e.Type) {
			continue
		}
		select {
		case s.ch <- e:
		default:
			b.logger.Warn("bus: dropped event for slow subscriber", "type", e.Type, "subscriber", s.id)
		}
	}
}

// remove deletes a subscription from the fan-out set. Called by
// Subscription.Close; safe if the id is already gone.
func (b *Bus) remove(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, id)
}
