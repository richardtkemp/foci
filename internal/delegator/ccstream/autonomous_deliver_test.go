package ccstream

import (
	"bytes"
	"testing"

	"foci/internal/delegator"
)

// TestAutonomousResultDelivered pins #1261: a CC-initiated run (one foci did not
// open — e.g. a task-notification run after a backgrounded sub-agent completes)
// is adopted as a first-class turn via onAutonomousOpen, and its reply completes
// through OnTurnComplete (not the old whole-message late-delivery fallback) — so
// it gets streaming, accounting, and meta like any turn. Replays the 2026-07-07
// incident (a report generated but never reaching the chat) under the new model.
func TestAutonomousResultDelivered(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}

	var completed []*delegator.TurnResult
	// The agent wires onAutonomousOpen to adopt the run as a first-class turn.
	b.SetOnAutonomousOpen(func() {
		b.AdoptRunningTurn(&delegator.TurnEvents{
			OnTurnComplete: func(r *delegator.TurnResult) { completed = append(completed, r) },
		})
	})

	stateEvent(b, "running") // no foci turn open → adopted as a first-class turn
	if !b.IsTurnInFlight() {
		t.Fatal("an adopted autonomous run must be in flight (turnActive=true)")
	}
	// Reply arrives only via the result message (turnText empty → uses Result),
	// exactly as a task-notification run does.
	b.OnResult(&ResultMessage{Subtype: "success", Result: "the autonomous reply", ModelUsage: map[string]ModelUsage{}})
	stateEvent(b, "idle") // → completeTurn → OnTurnComplete

	if len(completed) != 1 || completed[0].Text != "the autonomous reply" {
		t.Fatalf("adopted autonomous turn must complete once via OnTurnComplete; got %v", completed)
	}
}

// TestNormalTurnUnaffectedByAutonomousDelivery confirms the fix is scoped to
// autonomous runs: a normal foci turn still completes via OnTurnComplete and the
// autonomous-delivery branch never fires for it.
func TestNormalTurnUnaffectedByAutonomousDelivery(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}

	var delivered []string
	var completed []*delegator.TurnResult
	handler := &testHandler{
		OnText:         func(text string) { delivered = append(delivered, text) },
		OnTurnComplete: func(r *delegator.TurnResult) { completed = append(completed, r) },
	}
	applyHandler(b, handler) // opens a real foci turn (turnActive=true)
	stateEvent(b, "running")

	b.turnMu.Lock()
	b.turnText.WriteString("normal reply")
	b.turnMu.Unlock()
	b.OnResult(&ResultMessage{Subtype: "success", ModelUsage: map[string]ModelUsage{}})
	stateEvent(b, "idle")

	if len(completed) != 1 || completed[0].Text != "normal reply" {
		t.Fatalf("normal turn should complete via OnTurnComplete with its text; got %v", completed)
	}
}
