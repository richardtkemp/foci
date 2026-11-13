package ccstream

import (
	"testing"

	"foci/internal/delegator"
)

// TestTryBeginTurn_FiresOnBeginOnlyWhenClaimed pins #3067: the backend tells
// the agent the moment it claims a turn (TurnEvents.OnBegin), so the agent can
// register a non-delivered turn's sink before the turn's input is written. A
// refused begin must not fire it, or a waiting system turn would take the
// running turn's sink.
func TestTryBeginTurn_FiresOnBeginOnlyWhenClaimed(t *testing.T) {
	t.Parallel()

	b, _ := newGateTestBackend()
	b.beginTurn(nil) // a turn is active
	refused := 0
	if err := b.tryBeginTurn(&delegator.TurnEvents{OnBegin: func() { refused++ }}); err != delegator.ErrTurnInFlight {
		t.Fatalf("begin while a turn is active = %v, want ErrTurnInFlight", err)
	}
	if refused != 0 {
		t.Fatalf("OnBegin fired %d time(s) for a refused begin, want 0", refused)
	}

	b2, _ := newGateTestBackend()
	claimed := 0
	if err := b2.tryBeginTurn(&delegator.TurnEvents{OnBegin: func() {
		claimed++
		if !b2.turnActive { // turnMu is held by the caller
			t.Error("OnBegin fired before the turn was claimed")
		}
	}}); err != nil {
		t.Fatalf("begin at idle = %v, want nil", err)
	}
	if claimed != 1 {
		t.Fatalf("OnBegin fired %d time(s) for a claimed begin, want 1", claimed)
	}
}
