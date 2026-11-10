package agent

import (
	"context"
	"testing"

	"foci/internal/delegator"
	"foci/internal/epdispatch"
)

// TestCallTier (#3019, #3023): background agent turns are the lowest tier on
// a local endpoint; turns that automation started (HTTP /send, HTTP /branch,
// webhook, agent message, reminder wake) are Medium; the user's turns and
// everything unlisted are High.
func TestCallTier(t *testing.T) {
	bg := context.Background()
	for _, tr := range []string{TriggerKeepalive, "background", "reflection", delegator.BatchPurposeConsolidation,
		"compaction_memory", "session_end_memory", delegator.BatchPurposeNudgeExtraction} {
		if got := CallTier(WithTrigger(bg, tr)); got != epdispatch.Low {
			t.Errorf("%q: tier %s, want low", tr, got)
		}
	}
	for _, tr := range []string{"user", "branch", "webhook", "session_notify", "scheduled_wake"} {
		if got := CallTier(WithTrigger(bg, tr)); got != epdispatch.Medium {
			t.Errorf("%q: tier %s, want medium", tr, got)
		}
	}
	for _, tr := range []string{"", "voice", "telegram", "app", "ask_grader", "async_notify",
		TriggerRedelivery, "depth", "autonomous", "restart",
		delegator.BatchPurposeSummary, delegator.BatchPurposePromptDiff} {
		if got := CallTier(WithTrigger(bg, tr)); got != epdispatch.High {
			t.Errorf("%q: tier %s, want high", tr, got)
		}
	}
	if got := CallTier(bg); got != epdispatch.High {
		t.Errorf("no trigger: tier %s, want high", got)
	}
	// A /send that declares a human sender (--human) is the user's turn.
	if got := CallTier(WithHumanSource(WithTrigger(bg, "user"))); got != epdispatch.High {
		t.Errorf("human /send: tier %s, want high", got)
	}
	// A /branch that declares a human sender (--human, #3025) is High too.
	if got := CallTier(WithHumanSource(WithTrigger(bg, "branch"))); got != epdispatch.High {
		t.Errorf("human /branch: tier %s, want high", got)
	}
	// A /send with a delivery choice runs with trigger "" and the HTTP-send
	// marker (RunTurn, SendDriver): Medium, unless a human sent it.
	if got := CallTier(withHTTPSend(bg)); got != epdispatch.Medium {
		t.Errorf("choice /send: tier %s, want medium", got)
	}
	if got := CallTier(WithHumanSource(withHTTPSend(bg))); got != epdispatch.High {
		t.Errorf("human choice /send: tier %s, want high", got)
	}
}
