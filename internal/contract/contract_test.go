package contract

import "testing"

// TestStateTransitions exhaustively checks the legal-transition table: every
// state pair is either explicitly allowed or rejected.
func TestStateTransitions(t *testing.T) {
	all := []State{
		StateRequested, StateProvisioning, StateReady,
		StateExpiring, StateReleasing, StateReleased, StateExpired,
	}

	legal := map[State]map[State]bool{
		StateRequested:    {StateProvisioning: true, StateReleasing: true, StateExpired: true},
		StateProvisioning: {StateReady: true, StateReleasing: true, StateExpired: true},
		StateReady:        {StateExpiring: true, StateReleasing: true, StateExpired: true},
		StateExpiring:     {StateReleasing: true, StateExpired: true},
		StateReleasing:    {StateReleased: true, StateExpired: true},
		StateReleased:     {},
		StateExpired:      {},
	}

	for _, from := range all {
		for _, to := range all {
			want := legal[from][to]
			if got := from.CanTransition(to); got != want {
				t.Errorf("CanTransition(%s → %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

// TestHappyPath walks the canonical lifecycle end to end.
func TestHappyPath(t *testing.T) {
	path := []State{
		StateRequested, StateProvisioning, StateReady, StateExpiring, StateReleasing, StateReleased,
	}
	for i := 0; i+1 < len(path); i++ {
		if !path[i].CanTransition(path[i+1]) {
			t.Errorf("expected legal transition %s → %s", path[i], path[i+1])
		}
	}
}

func TestTerminalStates(t *testing.T) {
	if !StateReleased.Terminal() {
		t.Error("released should be terminal")
	}
	if !StateExpired.Terminal() {
		t.Error("expired should be terminal")
	}
	if StateReady.Terminal() {
		t.Error("ready should not be terminal")
	}
	if StateReleasing.Terminal() {
		t.Error("releasing should not be terminal (the transient fence before released)")
	}

	// No transitions out of a terminal state.
	for _, to := range []State{StateRequested, StateProvisioning, StateReady, StateExpiring, StateReleasing, StateReleased, StateExpired} {
		if StateReleased.CanTransition(to) {
			t.Errorf("released → %s should be illegal", to)
		}
		if StateExpired.CanTransition(to) {
			t.Errorf("expired → %s should be illegal", to)
		}
	}
}

func TestNoSelfTransition(t *testing.T) {
	for _, s := range []State{StateRequested, StateProvisioning, StateReady, StateExpiring, StateReleasing} {
		if s.CanTransition(s) {
			t.Errorf("self-transition %s → %s should be illegal", s, s)
		}
	}
}

// StateReleasing has two legal successors: StateReleased (the DELETE handler's
// success path, serializing the terminal side effects to one caller) and
// StateExpired (the reaper's release-grace escape hatch: past the grace the
// release is presumed stuck and the reaper reclaims capacity by expiring the
// contract). Every other move is illegal so the state machine still admits at
// most one terminal transition per contract.
func TestReleasingOnlyGoesToReleasedOrExpired(t *testing.T) {
	for _, to := range []State{StateRequested, StateProvisioning, StateReady, StateExpiring, StateReleasing} {
		if StateReleasing.CanTransition(to) {
			t.Errorf("releasing → %s should be illegal", to)
		}
	}
	if !StateReleasing.CanTransition(StateReleased) {
		t.Error("releasing → released should be legal (the DELETE handler's success path)")
	}
	if !StateReleasing.CanTransition(StateExpired) {
		t.Error("releasing → expired should be legal (the reaper's stuck-release escape hatch)")
	}
}
