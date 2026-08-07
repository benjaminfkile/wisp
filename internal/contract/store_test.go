package contract

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestStoreCreateGet(t *testing.T) {
	s := NewStore()

	c, err := s.Create(CreateParams{TTL: time.Hour, Image: "wisp-base"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.ID == "" {
		t.Error("Create returned empty id")
	}
	if c.Token == "" {
		t.Error("Create returned empty token")
	}
	if c.State != StateRequested {
		t.Errorf("State = %q, want requested", c.State)
	}
	if c.Image != "wisp-base" {
		t.Errorf("Image = %q, want wisp-base", c.Image)
	}
	if c.TTL != time.Hour {
		t.Errorf("TTL = %v, want 1h", c.TTL)
	}
	if got := c.ExpiresAt.Sub(c.CreatedAt); got != time.Hour {
		t.Errorf("ExpiresAt-CreatedAt = %v, want 1h", got)
	}

	got, err := s.Get(c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Errorf("Get = %+v, want %+v", got, c)
	}
}

// Create persists the exclusively-assigned GPU device IDs, Get surfaces them,
// and the stored slice does not alias the caller's input (mutating the input
// after Create must not change stored state).
func TestStoreCreatePersistsGPUDeviceIDs(t *testing.T) {
	s := NewStore()
	input := []string{"GPU-aaa", "GPU-bbb"}

	c, err := s.Create(CreateParams{TTL: time.Hour, GPUDeviceIDs: input})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !reflect.DeepEqual(c.GPUDeviceIDs, []string{"GPU-aaa", "GPU-bbb"}) {
		t.Fatalf("GPUDeviceIDs = %v, want [GPU-aaa GPU-bbb]", c.GPUDeviceIDs)
	}

	// Mutating the caller's slice must not affect the stored contract.
	input[0] = "GPU-tampered"
	got, err := s.Get(c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GPUDeviceIDs[0] != "GPU-aaa" {
		t.Errorf("stored GPUDeviceIDs aliased caller slice: %v", got.GPUDeviceIDs)
	}

	// A GPU-less create carries no device slice.
	none, err := s.Create(CreateParams{TTL: time.Hour})
	if err != nil {
		t.Fatalf("Create (no gpus): %v", err)
	}
	if none.GPUDeviceIDs != nil {
		t.Errorf("GPUDeviceIDs = %v, want nil for a GPU-less contract", none.GPUDeviceIDs)
	}
}

// Adopt recovers the GPU device IDs from a reconciled lease so the rebuilt
// contract surfaces them and can free them when reaped.
func TestStoreAdoptRecoversGPUDeviceIDs(t *testing.T) {
	s := NewStore()
	c, err := s.Adopt(AdoptParams{
		ID:           "contract-1",
		ContainerID:  "cont-1",
		ExpiresAt:    time.Unix(2_000_000_000, 0),
		GPUDeviceIDs: []string{"GPU-xyz"},
	})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if !reflect.DeepEqual(c.GPUDeviceIDs, []string{"GPU-xyz"}) {
		t.Errorf("GPUDeviceIDs = %v, want [GPU-xyz]", c.GPUDeviceIDs)
	}
}

func TestStoreCreateInvalidTTL(t *testing.T) {
	s := NewStore()
	for _, ttl := range []time.Duration{0, -time.Second} {
		if _, err := s.Create(CreateParams{TTL: ttl}); !errors.Is(err, ErrInvalidTTL) {
			t.Errorf("Create(ttl=%v) err = %v, want ErrInvalidTTL", ttl, err)
		}
	}
}

func TestStoreUniqueIDsAndTokens(t *testing.T) {
	s := NewStore()
	ids := map[string]bool{}
	tokens := map[string]bool{}
	for i := 0; i < 100; i++ {
		c, err := s.Create(CreateParams{TTL: time.Minute})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if ids[c.ID] {
			t.Fatalf("duplicate id %q", c.ID)
		}
		if tokens[c.Token] {
			t.Fatalf("duplicate token %q", c.Token)
		}
		ids[c.ID] = true
		tokens[c.Token] = true
	}
}

func TestStoreGetNotFound(t *testing.T) {
	s := NewStore()
	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestStoreUpdateStateLegal(t *testing.T) {
	s := NewStore()
	c, _ := s.Create(CreateParams{TTL: time.Hour})

	for _, next := range []State{StateProvisioning, StateReady, StateExpiring, StateReleased} {
		got, err := s.UpdateState(c.ID, next)
		if err != nil {
			t.Fatalf("UpdateState(%s): %v", next, err)
		}
		if got.State != next {
			t.Errorf("State = %q, want %q", got.State, next)
		}
	}

	// The update must persist.
	final, _ := s.Get(c.ID)
	if final.State != StateReleased {
		t.Errorf("persisted State = %q, want released", final.State)
	}
}

func TestStoreUpdateStateIllegal(t *testing.T) {
	s := NewStore()
	c, _ := s.Create(CreateParams{TTL: time.Hour})

	// requested → ready skips provisioning: illegal.
	_, err := s.UpdateState(c.ID, StateReady)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("UpdateState illegal err = %v, want ErrIllegalTransition", err)
	}
	var ite *IllegalTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("err = %v, want *IllegalTransitionError", err)
	}
	if ite.From != StateRequested || ite.To != StateReady {
		t.Errorf("IllegalTransitionError = %+v, want requested→ready", ite)
	}

	// State must be unchanged after a rejected transition.
	got, _ := s.Get(c.ID)
	if got.State != StateRequested {
		t.Errorf("State = %q after illegal transition, want requested", got.State)
	}
}

func TestStoreUpdateStateNotFound(t *testing.T) {
	s := NewStore()
	if _, err := s.UpdateState("nope", StateProvisioning); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateState(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestStoreSetContainerID(t *testing.T) {
	s := NewStore()
	c, _ := s.Create(CreateParams{TTL: time.Hour})

	got, err := s.SetContainerID(c.ID, "fake-1")
	if err != nil {
		t.Fatalf("SetContainerID: %v", err)
	}
	if got.ContainerID != "fake-1" {
		t.Errorf("ContainerID = %q, want fake-1", got.ContainerID)
	}

	if _, err := s.SetContainerID("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetContainerID(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestStoreDelete(t *testing.T) {
	s := NewStore()
	c, _ := s.Create(CreateParams{TTL: time.Hour})

	if err := s.Delete(c.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete err = %v, want ErrNotFound", err)
	}
	if err := s.Delete(c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete(twice) err = %v, want ErrNotFound", err)
	}
}

func TestStoreList(t *testing.T) {
	s := NewStore()
	// Inject a monotonic clock so creation order is deterministic.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var n int
	s.now = func() time.Time { d := base.Add(time.Duration(n) * time.Second); n++; return d }

	var want []string
	for i := 0; i < 3; i++ {
		c, _ := s.Create(CreateParams{TTL: time.Minute})
		want = append(want, c.ID)
	}

	got := s.List()
	if len(got) != 3 {
		t.Fatalf("List returned %d, want 3", len(got))
	}
	for i, c := range got {
		if c.ID != want[i] {
			t.Errorf("List[%d].ID = %q, want %q (order)", i, c.ID, want[i])
		}
	}

	// List returns copies: mutating one must not affect the store.
	got[0].State = StateExpired
	fresh, _ := s.Get(want[0])
	if fresh.State != StateRequested {
		t.Errorf("List returned an aliased pointer; store mutated to %q", fresh.State)
	}
}

// TestStoreConcurrent exercises the store from many goroutines; run with -race.
func TestStoreConcurrent(t *testing.T) {
	s := NewStore()
	const n = 50
	var wg sync.WaitGroup
	ids := make([]string, n)

	for i := 0; i < n; i++ {
		c, _ := s.Create(CreateParams{TTL: time.Hour})
		ids[i] = c.ID
	}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, _ = s.UpdateState(id, StateProvisioning)
			_, _ = s.UpdateState(id, StateReady)
			_, _ = s.Get(id)
			_ = s.List()
			_, _ = s.SetContainerID(id, "c-"+id)
		}(ids[i])
	}
	wg.Wait()

	for _, id := range ids {
		c, err := s.Get(id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if c.State != StateReady {
			t.Errorf("State = %q, want ready", c.State)
		}
	}
}
