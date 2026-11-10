package agent

import (
	"context"
	"testing"

	"foci/internal/delegator"
)

// TestBackgroundCall (#3019): background agent turns are the lowest tier on a
// local endpoint; the user's turns and everything unlisted are not.
func TestBackgroundCall(t *testing.T) {
	for _, tr := range []string{TriggerKeepalive, "background", "reflection", delegator.BatchPurposeConsolidation,
		"compaction_memory", "session_end_memory", delegator.BatchPurposeNudgeExtraction} {
		if !BackgroundCall(WithTrigger(context.Background(), tr)) {
			t.Errorf("%q: not a background call", tr)
		}
	}
	for _, tr := range []string{"", "user", "voice", "telegram", "ask_grader", "branch", "webhook", "scheduled_wake",
		"session_notify", delegator.BatchPurposeSummary, delegator.BatchPurposePromptDiff} {
		if BackgroundCall(WithTrigger(context.Background(), tr)) {
			t.Errorf("%q: a background call", tr)
		}
	}
	if BackgroundCall(context.Background()) {
		t.Error("no trigger: a background call")
	}
}
