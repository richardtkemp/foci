package command

import (
	"math"
	"strings"
	"testing"
	"time"

	"foci/internal/log"
	"foci/internal/modelinfo"
)

func costEntry(callType, agentID string, cost float64, turn modelinfo.TokenCounts) log.APIEntry {
	c := cost
	tc := turn
	return log.APIEntry{
		Timestamp:         time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC),
		Session:           "clutch/c1",
		Model:             "claude/claude-opus-5",
		CallType:          callType,
		AgentID:           agentID,
		TurnID:            "clutch/c1@1",
		CalculatedCostUSD: &c,
		Turn:              &tc,
	}
}

// TestSumCosts_SubagentRowsCostButDoNotCount is the invariant that keeps #1880
// phase C honest on the read side.
//
// A subagent row's cost was SUBTRACTED from the parent row beside it, so the
// money only adds up if every row is summed. The CALL count is the opposite: a
// turn that spawned three subagents is one call and four rows, and counting
// rows inflates every "N calls" figure the moment the split ships.
func TestSumCosts_SubagentRowsCostButDoNotCount(t *testing.T) {
	t.Parallel()
	entries := []log.APIEntry{
		costEntry("delegated_turn", "", 1.00, modelinfo.TokenCounts{Output: 100}),
		costEntry("subagent_turn", "agent-a", 0.25, modelinfo.TokenCounts{Output: 50}),
		costEntry("subagent_turn", "agent-b", 0.75, modelinfo.TokenCounts{Output: 70}),
	}
	total, count := sumCosts(entries)
	if total != 2.00 {
		t.Errorf("total = %.4f, want 2.00 — every row's cost counts, the parent's was reduced by the others", total)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1 — three rows, one call", count)
	}
}

// TestCategoryCosts_PricesTurnCountsNotContextFill is the #1854 defect on the
// read side.
//
// categoryCosts priced the un-suffixed columns, which for a delegated turn are
// the FINAL ask cycle's context fill, not what the row was priced from. So the
// category table sat beside a Total it could not add up to. Measured on live
// data 2026-09-11 over 810 rows since 2026-09-04: 3,845,629 cache-write tokens
// in those columns against 55,728,618 in Turn — 14.5x short.
func TestCategoryCosts_PricesTurnCountsNotContextFill(t *testing.T) {
	t.Parallel()
	e := costEntry("delegated_turn", "", 0, modelinfo.TokenCounts{CacheWrite: 100000})
	// The context fill is deliberately tiny and unlike the turn total, exactly
	// as a real multi-cycle row's is.
	e.CacheWrite = 300
	entries := []log.APIEntry{e}

	_, cacheWrite, _, _ := categoryCosts(entries)
	want := modelinfo.CostAsOf(e.Model, e.Timestamp, 0, 0, 0, 100000)
	if cacheWrite != want {
		t.Errorf("cacheWrite = %.6f, want %.6f — priced from Turn (100000), not the 300-token context fill",
			cacheWrite, want)
	}

	// And the categories must reconcile to the total the header prints. Summed
	// in a different order from EffectiveCost, so compared with a tolerance
	// rather than for bit equality.
	e2 := costEntry("delegated_turn", "", 0, modelinfo.TokenCounts{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40})
	e2.CalculatedCostUSD = nil // no recorded cost → EffectiveCost re-prices
	cr, cw, in, out := categoryCosts([]log.APIEntry{e2})
	if got, want := cr+cw+in+out, e2.EffectiveCost(); math.Abs(got-want) > 1e-12 {
		t.Errorf("categories sum to %.12f but EffectiveCost is %.12f — the table must add up to its own total", got, want)
	}
}

// TestSubagentBreakdown_NamesEachAgent covers #1863's third bullet: the
// question "what did that delegation cost", which had no answer at all.
func TestSubagentBreakdown_NamesEachAgent(t *testing.T) {
	t.Parallel()
	if got := subagentBreakdown([]log.APIEntry{
		costEntry("delegated_turn", "", 1.00, modelinfo.TokenCounts{Output: 100}),
	}); got != "" {
		t.Errorf("breakdown with no subagents = %q, want empty", got)
	}

	entries := []log.APIEntry{
		costEntry("delegated_turn", "", 1.00, modelinfo.TokenCounts{Output: 100}),
		costEntry("subagent_turn", "agent-a", 0.25, modelinfo.TokenCounts{Output: 50}),
		costEntry("subagent_turn", "agent-a", 0.75, modelinfo.TokenCounts{Output: 70}),
		costEntry("subagent_turn", "agent-b", 4.2578, modelinfo.TokenCounts{Output: 900}),
	}
	got := subagentBreakdown(entries)
	for _, want := range []string{"agent-a", "agent-b", "$5.2578", "claude-opus-5"} {
		if !strings.Contains(got, want) {
			t.Errorf("breakdown missing %q:\n%s", want, got)
		}
	}
	// agent-b outspent agent-a, so it sorts first — the expensive delegation is
	// the one a reader is looking for.
	if strings.Index(got, "agent-b") > strings.Index(got, "agent-a") {
		t.Errorf("agent-b ($4.2578) must sort above agent-a ($1.00):\n%s", got)
	}
	// agent-a contributed two rows and must appear once, with both summed.
	if strings.Count(got, "agent-a") != 1 {
		t.Errorf("agent-a listed %d times, want 1 (its rows sum into one line):\n%s",
			strings.Count(got, "agent-a"), got)
	}
}

// TestSubagentBreakdown_UnnamedAgentStillShows pins the "" case. Usage that
// arrives before task_started names the agent still belongs to some subagent,
// and hiding it would leave this table short against the total above it.
func TestSubagentBreakdown_UnnamedAgentStillShows(t *testing.T) {
	t.Parallel()
	got := subagentBreakdown([]log.APIEntry{
		costEntry("subagent_turn", "", 0.5, modelinfo.TokenCounts{Output: 10}),
	})
	if !strings.Contains(got, "(unnamed)") || !strings.Contains(got, "0.5000") {
		t.Errorf("breakdown = %q, want an (unnamed) row carrying $0.5000", got)
	}
}
