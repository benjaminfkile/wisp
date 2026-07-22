package contract

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned when an operation references a contract id the store
// does not know about (never created, or deleted).
var ErrNotFound = errors.New("contract: not found")

// CreateParams carries the client-supplied fields for a new contract. The
// store fills in the id, token, timestamps, and initial state.
type CreateParams struct {
	// TTL is the requested lease duration. Must be positive.
	TTL time.Duration

	// Image is the base image the container will boot from. Opaque to the store.
	Image string

	// Meta is arbitrary client-supplied metadata echoed back on status reads.
	// Opaque to the store.
	Meta map[string]any
}

// ErrInvalidTTL is returned by Create when the requested TTL is not positive.
var ErrInvalidTTL = errors.New("contract: ttl must be positive")

// Store is a thread-safe in-memory collection of contracts. The zero value is
// not usable; call NewStore. All methods are safe for concurrent use.
//
// Accessors return Contract values (copies), never pointers into the store, so
// callers cannot mutate stored state without going through UpdateState/Delete.
type Store struct {
	mu        sync.RWMutex
	contracts map[string]*Contract

	// now and newID/newToken are injectable for deterministic tests; they
	// default to wall-clock time and crypto/rand-backed generators.
	now      func() time.Time
	newID    func() string
	newToken func() string
}

// NewStore returns an initialized, empty Store.
func NewStore() *Store {
	return &Store{
		contracts: make(map[string]*Contract),
		now:       time.Now,
		newID:     func() string { return randHex(16) },
		newToken:  func() string { return randHex(32) },
	}
}

// randHex returns a hex-encoded string of n random bytes. It panics only if the
// system CSPRNG fails, which is treated as unrecoverable.
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("contract: cannot read random bytes: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// Create records a new contract in StateRequested and returns a copy of it. The
// store assigns the id, bearer token, and created/expires timestamps.
func (s *Store) Create(p CreateParams) (Contract, error) {
	if p.TTL <= 0 {
		return Contract{}, ErrInvalidTTL
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	created := s.now()
	c := &Contract{
		ID:        s.newID(),
		TTL:       p.TTL,
		Image:     p.Image,
		Meta:      p.Meta,
		State:     StateRequested,
		CreatedAt: created,
		ExpiresAt: created.Add(p.TTL),
		Token:     s.newToken(),
	}
	s.contracts[c.ID] = c
	return *c, nil
}

// AdoptParams carries the fields needed to rebuild a tracking entry for a
// container that already exists at startup — one launched by a previous wispd
// process whose in-memory state was lost on restart (see the Docker-label
// reconcile). The id and expiry come from the container's labels; the store
// fills in the remaining bookkeeping so the reaper can enforce the TTL.
type AdoptParams struct {
	// ID is the contract id, recovered from the container's wisp.contract label.
	ID string

	// ContainerID is the runtime container backing the contract, so the reaper
	// can kill it when the TTL elapses.
	ContainerID string

	// ExpiresAt is the contract's absolute expiry, recovered from the container's
	// wisp.expires_at label.
	ExpiresAt time.Time
}

// Adopt records a tracking entry for a pre-existing container discovered during
// startup reconciliation. The contract is rebuilt in StateReady so the reaper
// treats it like any live lease: still-valid ones keep being tracked, and ones
// already past ExpiresAt are reaped (their container killed) on the reaper's
// first sweep. It has no bearer token — a reconciled lease cannot be driven via
// exec/shell, only tracked to expiry — and no client Meta, which did not survive
// the restart. A no-op if the id is already tracked, so re-running the reconcile
// never clobbers a live contract. Returns a copy of the tracked contract.
func (s *Store) Adopt(p AdoptParams) (Contract, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if c, ok := s.contracts[p.ID]; ok {
		return *c, nil
	}
	c := &Contract{
		ID:          p.ID,
		State:       StateReady,
		ContainerID: p.ContainerID,
		ExpiresAt:   p.ExpiresAt,
		CreatedAt:   s.now(),
	}
	s.contracts[p.ID] = c
	return *c, nil
}

// Get returns a copy of the contract with the given id, or ErrNotFound.
func (s *Store) Get(id string) (Contract, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	c, ok := s.contracts[id]
	if !ok {
		return Contract{}, ErrNotFound
	}
	return *c, nil
}

// List returns copies of all contracts, ordered by creation time (oldest
// first) and then by id for a stable order.
func (s *Store) List() []Contract {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Contract, 0, len(s.contracts))
	for _, c := range s.contracts {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// UpdateState transitions the contract to next, enforcing the lifecycle state
// machine. It returns the updated contract copy, ErrNotFound if the id is
// unknown, or an *IllegalTransitionError (wrapping ErrIllegalTransition) if the
// move is not permitted.
func (s *Store) UpdateState(id string, next State) (Contract, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.contracts[id]
	if !ok {
		return Contract{}, ErrNotFound
	}
	if !c.State.CanTransition(next) {
		return Contract{}, &IllegalTransitionError{From: c.State, To: next}
	}
	c.State = next
	return *c, nil
}

// SetContainerID records the runtime container backing the contract. It returns
// ErrNotFound if the id is unknown.
func (s *Store) SetContainerID(id, containerID string) (Contract, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.contracts[id]
	if !ok {
		return Contract{}, ErrNotFound
	}
	c.ContainerID = containerID
	return *c, nil
}

// Delete removes the contract from the store. It returns ErrNotFound if the id
// is unknown.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.contracts[id]; !ok {
		return ErrNotFound
	}
	delete(s.contracts, id)
	return nil
}
