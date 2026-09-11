package ccstream

import (
	"strings"
	"testing"
)

// TestAccumulator_MessagesCountsThisTurnOnly is #1866 P5.
//
// The breakdown's cycles= counts RESULT messages — ask cycles — and #1877 read
// it as an API-call count, reporting it broken because a turn with 16.0M
// cache-read against a 1M context window (many calls) printed cycles=1. The
// counter was right and its name was wrong. msgs= is the number that answers
// the question actually being asked, so it has to be a per-TURN count from a
// map that is cumulative for the Backend's life.
func TestAccumulator_MessagesCountsThisTurnOnly(t *testing.T) {
	t.Parallel()
	var a usageAccumulator
	a.beginTurn("t1")

	a.note("claude-opus-5", "", false, "msg_a", TokenUsage{OutputTokens: 1})
	// The same message redelivered — CC emits one line per content block, each
	// repeating the whole message's usage. A repeat is not another call.
	a.note("claude-opus-5", "", false, "msg_a", TokenUsage{OutputTokens: 5})
	a.note("claude-opus-5", "", false, "msg_b", TokenUsage{OutputTokens: 2})
	a.note("claude-sonnet-5", "agent-x", true, "msg_c", TokenUsage{OutputTokens: 3})

	if got := a.messages(); got != 3 {
		t.Errorf("messages() = %d, want 3 — distinct ids, counting a content-block repeat once", got)
	}

	// A new turn starts from zero even though the map keeps every id.
	a.beginTurn("t2")
	if got := a.messages(); got != 0 {
		t.Errorf("messages() after beginTurn = %d, want 0 — the map is cumulative, the COUNT is per turn", got)
	}
	a.note("claude-opus-5", "", false, "msg_d", TokenUsage{OutputTokens: 1})
	if got := a.messages(); got != 1 {
		t.Errorf("messages() = %d, want 1 — turn 1's three messages must not carry over", got)
	}
}

// TestCostBreakdown_NamesBothCounters pins the rename. The two numbers answer
// different questions and were conflated for months; a line printing one of
// them under a name that suggests the other is the defect P5 exists to remove.
func TestCostBreakdown_NamesBothCounters(t *testing.T) {
	t.Parallel()
	got := costBreakdown{model: "claude/claude-opus-5", cycles: 1, msgs: 12}.String()
	if !strings.Contains(got, "ask_cycles=1") {
		t.Errorf("breakdown = %q, want ask_cycles=1 — 'cycles' alone reads as an API-call count", got)
	}
	if !strings.Contains(got, "msgs=12") {
		t.Errorf("breakdown = %q, want msgs=12", got)
	}
}
