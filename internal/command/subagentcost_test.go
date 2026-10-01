package command

import (
	"math"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/modelinfo"
)

// costEntry is a call on one delegated turn costing cost. actor is the
// subagent that made it (the Agent tool_use id), "" for the session's own
// thread: every call shares one owning AgentID ("clutch", the session's
// agent), while the actor distinguishes one subagent's calls from another's
// (#1946).
func costEntry(actor string, cost float64) apiRow {
	return apiRow{
		Timestamp: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC),
		Session:   "clutch/c1",
		Model:     "claude/claude-opus-5",
		AgentID:   "clutch",
		Actor:     actor,
		TurnID:    "clutch/c1@1",
		CostUSD:   &cost,
	}
}

// legacyShare is a migrated pre-ledger row as Ledger.Calls reads it: priced
// from its recorded figure, and a subagent's share when actor is set.
func legacyShare(actor string, cost float64) accounting.CallRow {
	return accounting.CallRow{Kind: accounting.KindLegacy, CostBasis: accounting.CostBasisRecorded,
		Session: "clutch/c1", TurnID: "clutch/c1@1", Actor: actor, CostUSD: &cost,
		Classes: map[modelinfo.Class]accounting.ClassCost{modelinfo.ClassOutput: {Count: 10}}}
}

// TestSumCosts_SubagentRowsCostButDoNotCount is the invariant that keeps #1880
// phase C honest on the read side, for the migrated history.
//
// A legacy subagent row's cost was SUBTRACTED from the parent row beside it, so
// the money only adds up if every row is summed. The CALL count is the
// opposite: a turn that spawned three subagents was one call and four rows,
// and counting rows inflates every "N calls" figure. A call booked since the
// ledger is a call of its own, a subagent's included.
func TestSumCosts_SubagentRowsCostButDoNotCount(t *testing.T) {
	t.Parallel()
	total, count := sumCosts([]accounting.CallRow{
		legacyShare("", 1.00), legacyShare("agent-a", 0.25), legacyShare("agent-b", 0.75),
	})
	if total != 2.00 {
		t.Errorf("total = %.4f, want 2.00 — every row's cost counts, the parent's was reduced by the others", total)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1 — three rows, one call", count)
	}

	total, count = sumCosts(callRows(t, costEntry("", 1.00), costEntry("agent-a", 0.25), costEntry("agent-b", 0.75)))
	if math.Abs(total-2.00) > 1e-9 || count != 3 {
		t.Errorf("ledger calls: total = %.4f over %d calls, want 2.00 over 3", total, count)
	}
}

// TestCategoryCosts_PricesClassCounts is the #1854 defect on the read side:
// categoryCosts once priced a delegated turn's final-cycle context fill, not
// what the call was priced from, so the category table sat beside a Total it
// could not add up to. The split now comes from the views' own per-class
// prices of the call's counts.
func TestCategoryCosts_PricesClassCounts(t *testing.T) {
	t.Parallel()
	e := apiRow{Timestamp: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC), Model: "claude/claude-opus-5", CacheWrite: 100000}
	got := categoryCosts(callRows(t, e))
	want, _ := modelinfo.CostAsOf(e.Model, e.Timestamp, modelinfo.Tokens{modelinfo.ClassCacheWrite1h: 100000})
	if math.Abs(got.cacheWrite-want) > 1e-12 {
		t.Errorf("cacheWrite = %.6f, want %.6f — the call's 100000 1h writes, priced", got.cacheWrite, want)
	}

	// And the categories must reconcile to the total the header prints.
	e2 := apiRow{Timestamp: e.Timestamp, Model: e.Model, Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40}
	rows := callRows(t, e2)
	c := categoryCosts(rows)
	total, _ := sumCosts(rows)
	if got := c.cacheRead + c.cacheWrite + c.input + c.output + c.search + c.recorded; math.Abs(got-total) > 1e-12 {
		t.Errorf("categories sum to %.12f but the total is %.12f — the table must add up to its own total", got, total)
	}

	// A legacy call priced from its recorded figure has no class split: it is
	// one line of its own, and the table still adds up.
	if c := categoryCosts([]accounting.CallRow{legacyShare("", 1.25)}); c.recorded != 1.25 || c.output != 0 {
		t.Errorf("recorded-basis split = %+v, want the whole $1.25 under recorded", c)
	}
}

// TestSubagentBreakdown_NamesEachAgent covers #1863's third bullet: the
// question "what did that delegation cost", which had no answer at all.
func TestSubagentBreakdown_NamesEachAgent(t *testing.T) {
	t.Parallel()
	if got := subagentBreakdown(callRows(t,
		costEntry("", 1.00),
	)); got != "" {
		t.Errorf("breakdown with no subagents = %q, want empty", got)
	}

	entries := callRows(t,
		costEntry("", 1.00),
		costEntry("agent-a", 0.25),
		costEntry("agent-a", 0.75),
		costEntry("agent-b", 4.2578),
	)
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

// TestSubagentBreakdown_UnnamedAgentStillShows pins the unnamed actor. Usage that
// arrives before task_started names the agent still belongs to some subagent,
// and hiding it would leave this table short against the total above it.
func TestSubagentBreakdown_UnnamedAgentStillShows(t *testing.T) {
	t.Parallel()
	got := subagentBreakdown(callRows(t,
		costEntry(accounting.UnnamedSubagent, 0.5),
	))
	if !strings.Contains(got, "(unnamed)") || !strings.Contains(got, "0.5000") {
		t.Errorf("breakdown = %q, want an (unnamed) row carrying $0.5000", got)
	}
}
