package ccstream

import (
	"bytes"
	"testing"

	"foci/internal/delegator"
)

// TestAutonomousResultDelivered pins the #1063 fix: an autonomous run (one foci
// opened no turn for — e.g. a task-notification run after a backgrounded
// sub-agent completes) whose reply arrives ONLY in CC's result message (no
// streamed assistant text) must still be delivered to the session sink on idle,
// not silently dropped. This replays the 2026-07-07 incident where a 2.5k-char
// report was generated but never reached the chat.
func TestAutonomousResultDelivered(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}

	var delivered []string
	// Attach session-scoped delivery only — NO beginTurn, so turnActive stays
	// false and the run is autonomous.
	b.AttachSessionEvents(&delegator.SessionEvents{
		OnText: func(text string) { delivered = append(delivered, text) },
	})

	stateEvent(b, "running") // running with no foci turn open → autonomous run
	// Reply arrives only via the result message (turnText empty → uses Result),
	// exactly as a task-notification run does.
	b.OnResult(&ResultMessage{Subtype: "success", Result: "the autonomous reply", ModelUsage: map[string]ModelUsage{}})
	stateEvent(b, "idle") // → onSessionIdle delivers the stashed result

	if len(delivered) != 1 || delivered[0] != "the autonomous reply" {
		t.Fatalf("autonomous result must be delivered once via the session sink; got %v", delivered)
	}
}

// TestAutonomousStreamedNotRedelivered guards the other side: when an autonomous
// run DID stream its text via OnAssistant (se.OnText already sent it through the
// late-delivery fallback), onSessionIdle must NOT re-deliver the stashed result
// — otherwise the reply lands twice.
func TestAutonomousStreamedNotRedelivered(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}

	var delivered []string
	b.AttachSessionEvents(&delegator.SessionEvents{
		OnText: func(text string) { delivered = append(delivered, text) },
	})

	stateEvent(b, "running") // autonomous run
	// The run streams a top-level assistant text block: se.OnText fires (path 1
	// delivery) and the run is marked streamed.
	b.OnAssistant(&AssistantMessage{
		Message: BetaMessage{
			Model:   "claude-opus-4-8",
			Content: []ContentBlock{{Type: "text", Text: "streamed reply"}},
			Usage:   TokenUsage{InputTokens: 10, OutputTokens: 5},
		},
	})
	b.OnResult(&ResultMessage{Subtype: "success", ModelUsage: map[string]ModelUsage{}})
	stateEvent(b, "idle")

	// Exactly one delivery (the stream), not a second copy from onSessionIdle.
	if len(delivered) != 1 || delivered[0] != "streamed reply" {
		t.Fatalf("streamed autonomous text must not be re-delivered on idle; got %v", delivered)
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
