package agent

import (
	"testing"

	"foci/internal/delegator"
)

// TestIsUnattendedTrigger pins THE list of triggers whose turns answer
// permission asks from foci's rules instead of prompting (Dick 2026-10-27).
// The unattended arm is the periodic self-maintenance passes, the memory
// passes, webhook and the batch purposes; every turn someone may be watching keeps prompting —
// notably redelivery (the user's own message re-sent after a crash) and the
// inter-session / async / branch / autonomous triggers.
func TestIsUnattendedTrigger(t *testing.T) {
	t.Parallel()
	for trigger, want := range map[string]bool{
		TriggerKeepalive:                      true,
		"background":                          true,
		"reflection":                          true,
		delegator.BatchPurposeConsolidation:   true,
		"compaction_memory":                   true,
		"session_end_memory":                  true,
		"webhook":                             true,
		delegator.BatchPurposeNudgeExtraction: true,
		delegator.BatchPurposeSummary:         true,
		delegator.BatchPurposePromptDiff:      true,

		"":                false,
		"user":            false,
		"voice":           false,
		"telegram":        false,
		"app":             false,
		TriggerRedelivery: false,
		"branch":          false,
		"autonomous":      false,
		"async_notify":    false,
		"session_notify":  false,
		"spawn":           false,
		"wake":            false,
	} {
		if got := isUnattendedTrigger(trigger); got != want {
			t.Errorf("isUnattendedTrigger(%q) = %v, want %v", trigger, got, want)
		}
	}
}
