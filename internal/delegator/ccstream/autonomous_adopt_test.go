package ccstream

import (
	"bytes"
	"testing"

	"foci/internal/delegator"
)

// TestAutonomousLifecycleCallbacks pins #1070 part 1: the backend fires
// onAutonomousStart exactly when it detects CC has begun an autonomous run
// (session_state:running with no foci turn open) and onAutonomousEnd exactly
// once when that run ends (idle). The agent wires these to markInFlight so the
// run is adopted as an in-flight delivering turn.
func TestAutonomousLifecycleCallbacks(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}

	var starts, ends int
	b.SetOnAutonomousStart(func() { starts++ })
	b.SetOnAutonomousEnd(func() { ends++ })

	// No foci turn open → running is an autonomous run → onAutonomousStart fires.
	stateEvent(b, "running")
	if starts != 1 || ends != 0 {
		t.Fatalf("after autonomous running: starts=%d ends=%d, want 1/0", starts, ends)
	}

	// Idle ends the run → onAutonomousEnd fires exactly once.
	stateEvent(b, "idle")
	if starts != 1 || ends != 1 {
		t.Fatalf("after idle: starts=%d ends=%d, want 1/1", starts, ends)
	}
}

// TestAutonomousLifecycle_NoCallbackForFociTurn confirms the callbacks are
// scoped to autonomous runs: a normal foci turn (turnActive=true) must NOT fire
// onAutonomousStart/End — its in-flight tracking is owned by OrchestrateFullTurn.
func TestAutonomousLifecycle_NoCallbackForFociTurn(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}

	var starts, ends int
	b.SetOnAutonomousStart(func() { starts++ })
	b.SetOnAutonomousEnd(func() { ends++ })

	handler := &testHandler{OnTurnComplete: func(*delegator.TurnResult) {}}
	applyHandler(b, handler) // opens a real foci turn (turnActive=true)
	stateEvent(b, "running")
	stateEvent(b, "idle")

	if starts != 0 || ends != 0 {
		t.Fatalf("foci turn must not fire autonomous callbacks; starts=%d ends=%d", starts, ends)
	}
}

// TestAutonomousLifecycle_AdoptionByFociTurnFiresEnd proves the adoption edge:
// an autonomous run is in flight (onAutonomousStart fired), then a foci turn
// begins (a user message adopts the run via beginTurn). onAutonomousEnd MUST
// fire so the agent releases the autonomous adoption — otherwise the in-flight
// counter leaks and the session's injection gate wedges (#1070).
func TestAutonomousLifecycle_AdoptionByFociTurnFiresEnd(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}

	var starts, ends int
	b.SetOnAutonomousStart(func() { starts++ })
	b.SetOnAutonomousEnd(func() { ends++ })

	stateEvent(b, "running") // autonomous run begins
	if starts != 1 || ends != 0 {
		t.Fatalf("after autonomous running: starts=%d ends=%d, want 1/0", starts, ends)
	}

	// A foci turn begins mid-run (adoption). beginTurn clears autonomousActive.
	b.beginTurn(&delegator.TurnEvents{})
	if starts != 1 || ends != 1 {
		t.Fatalf("adoption by a foci turn must fire onAutonomousEnd once; starts=%d ends=%d", starts, ends)
	}
}
