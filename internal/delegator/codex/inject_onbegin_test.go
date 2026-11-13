package codex

import (
	"testing"

	"foci/internal/delegator"
)

// TestBeginTurn_FiresOnBeginOnlyWhenClaimed pins #3067 for codex: see the
// ccstream test of the same name. The claimed case uses a backend with no
// thread, so beginTurn claims the turn and then fails before any RPC.
func TestBeginTurn_FiresOnBeginOnlyWhenClaimed(t *testing.T) {
	t.Parallel()

	b, _ := newInjectTestBackend(false)
	openTurn(b, nil) // a turn is active
	refused := 0
	if err := b.beginTurn("hi", &delegator.TurnEvents{OnBegin: func() { refused++ }}); err != delegator.ErrTurnInFlight {
		t.Fatalf("begin while a turn is active = %v, want ErrTurnInFlight", err)
	}
	if refused != 0 {
		t.Fatalf("OnBegin fired %d time(s) for a refused begin, want 0", refused)
	}

	b2, _ := newInjectTestBackend(false)
	b2.mu.Lock()
	b2.threadID = ""
	b2.mu.Unlock()
	b2.threadMapMu.Lock()
	delete(b2.sessionThreads, b2.startOpts.SessionKey)
	b2.threadMapMu.Unlock()
	claimed := 0
	_ = b2.beginTurn("hi", &delegator.TurnEvents{
		OnBegin: func() {
			claimed++
			if !b2.turnActive { // turnMu is held by the caller
				t.Error("OnBegin fired before the turn was claimed")
			}
		},
		OnTurnComplete: func(*delegator.TurnResult) {},
	})
	if claimed != 1 {
		t.Fatalf("OnBegin fired %d time(s) for a claimed begin, want 1", claimed)
	}
}
