package telemetry

import (
	"testing"

	"foci/internal/log"
	"foci/internal/turnevent"
)

// TestDisabled: without initWith ever having been called, the package is a
// no-op — NewTurnSink hands back the caller's own sink untouched and a nil
// *Turn, every Turn method is safe to call on that nil, and neither log
// hook is armed.
func TestDisabled(t *testing.T) {
	resetForTest() // belt-and-braces: guarantee disabled regardless of test order

	inner := turnevent.NopSink{}
	sink, turn := NewTurnSink(inner)
	if sink != inner {
		t.Error("NewTurnSink must return the inner sink unchanged when disabled")
	}
	if turn != nil {
		t.Error("NewTurnSink must return a nil *Turn when disabled")
	}

	// Every method must be safe on a nil receiver — none of these may panic.
	turn.Begin(TurnInfo{TurnID: "x", SessionKey: "s", AgentID: "a"})
	turn.SetInput("prompt", "model")
	turn.ToolStart("id", "name", []byte(`{}`))
	turn.ToolEnd("id", "name", "out", false)
	turn.Text("some text")
	turn.ThinkingDelta("some thinking")
	turn.ThinkingBlock("a whole thinking block")
	turn.Retry(1, "endpoint", nil)
	turn.SubagentStart("group", "label", "prompt", 1)
	turn.SubagentText("group", "text", 1)
	turn.SubagentPrompt("group", "prompt", 1)
	turn.SubagentEnd("group", 1)
	turn.Complete("final", "model", nil, 0, nil)
	if got := turn.TurnID(); got != "" {
		t.Errorf("nil Turn.TurnID() = %q, want \"\"", got)
	}

	if log.APIHook != nil {
		t.Error("log.APIHook must be nil when telemetry is disabled")
	}
	if log.CorrectionHook != nil {
		t.Error("log.CorrectionHook must be nil when telemetry is disabled")
	}
}
