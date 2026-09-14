package log

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/modelinfo"
)

// seedTurn writes a parent row for turnID and a subagent row for (spawnTurn,
// agentID), with the counts and cost a correction will move between them.
func seedTurn(t *testing.T, parentTurn, spawnTurn, agentID, model string,
	parent modelinfo.TokenCounts, parentCost float64,
	sub modelinfo.TokenCounts, subCost float64, closedAt time.Time) {
	t.Helper()
	pc, sc := parentCost, subCost
	p, s := parent, sub
	// session is the part of a turn id before '@' — the resolution keys on it,
	// so the row's session column and the turn id must agree or nothing matches.
	sess, _, _ := strings.Cut(parentTurn, "@")
	API(APIEntry{
		Timestamp: closedAt,
		Session:   sess, Model: model, CallType: "delegated_turn", TurnID: parentTurn,
		Output: parent.Output, Turn: &p, CalculatedCostUSD: &pc,
	})
	API(APIEntry{
		Timestamp: closedAt,
		Session:   sess, Model: model, CallType: "subagent_turn", TurnID: spawnTurn,
		AgentID: agentID, Output: sub.Output, Turn: &s, CalculatedCostUSD: &sc,
	})
}

func readRow(t *testing.T, where string, args ...any) (modelinfo.TokenCounts, float64) {
	t.Helper()
	var c modelinfo.TokenCounts
	var cost float64
	err := apiLog.db.QueryRow(`SELECT turn_input_tokens, turn_output_tokens,
		turn_cache_read_tokens, turn_cache_write_tokens, calculated_cost_usd
		FROM api_calls WHERE `+where, args...).
		Scan(&c.Input, &c.Output, &c.CacheRead, &c.CacheWrite, &cost)
	if err != nil {
		t.Fatalf("read row (%s): %v", where, err)
	}
	return c, cost
}

// sessTurn builds a turn id in the real format, "<session>@<StartedAt nanos>",
// because the parent-turn resolution splits on '@' to recover the session.
func sessTurn(name string) string { return "sess@" + name }

var (
	closeT1 = time.Date(2026, 9, 13, 15, 41, 35, 0, time.UTC)
	billed  = time.Date(2026, 9, 13, 15, 41, 30, 0, time.UTC)
)

func withAPIDB(t *testing.T) {
	t.Helper()
	resetGlobal()
	t.Cleanup(resetGlobal)
	if err := InitAPIDB(filepath.Join(t.TempDir(), "api.db")); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	t.Cleanup(CloseAPIDB)
}

// TestApplyCostCorrections_MovesSpendAndConservesTheTotal is the core of #1918.
//
// A subagent's spend reached foci after the turn that paid for it had closed,
// so that turn's parent row absorbed it. The correction moves it onto the
// subagent row. What must NOT change is the sum.
func TestApplyCostCorrections_MovesSpendAndConservesTheTotal(t *testing.T) {
	withAPIDB(t)

	parent := modelinfo.TokenCounts{Input: 100, Output: 50, CacheRead: 1000, CacheWrite: 200}
	sub := modelinfo.TokenCounts{Input: 10, Output: 5, CacheRead: 100, CacheWrite: 20}
	seedTurn(t, sessTurn("T1"), sessTurn("T1"), "agent-1", "claude-opus-5", parent, 1.00, sub, 0.10, closeT1)

	move := modelinfo.TokenCounts{Input: 40, Output: 20, CacheRead: 400, CacheWrite: 80}
	ApplyCostCorrections([]modelinfo.CostCorrection{{
		BilledAt: billed, SubagentTurnID: sessTurn("T1"), AgentID: "agent-1",
		Model: "claude-opus-5", Counts: move, CostUSD: 0.30,
	}})

	gotParent, parentCost := readRow(t, "turn_id = ? AND call_type = 'delegated_turn'", sessTurn("T1"))
	gotSub, subCost := readRow(t, "turn_id = ? AND call_type = 'subagent_turn'", sessTurn("T1"))

	wantParent := modelinfo.TokenCounts{Input: 60, Output: 30, CacheRead: 600, CacheWrite: 120}
	wantSub := modelinfo.TokenCounts{Input: 50, Output: 25, CacheRead: 500, CacheWrite: 100}
	if gotParent != wantParent {
		t.Errorf("parent counts = %+v, want %+v", gotParent, wantParent)
	}
	if gotSub != wantSub {
		t.Errorf("subagent counts = %+v, want %+v", gotSub, wantSub)
	}
	if d := (parentCost + subCost) - 1.10; d > 1e-9 || d < -1e-9 {
		t.Errorf("turn total = $%.6f, want $1.100000 — a correction MOVES spend, "+
			"it never creates or destroys any", parentCost+subCost)
	}
	if d := parentCost - 0.70; d > 1e-9 || d < -1e-9 {
		t.Errorf("parent cost = $%.6f, want $0.700000", parentCost)
	}
}

// TestApplyCostCorrections_MissingSubagentRowLeavesParentUntouched is the test
// this whole design exists for.
//
// Half a correction is worse than none: subtracting from the parent without
// crediting the subagent DESTROYS money from the record, and adding without
// subtracting inflates it — the exact failure #1909 closed. The parent update
// runs FIRST, so a missing subagent row is the case where a non-transactional
// implementation silently loses spend.
func TestApplyCostCorrections_MissingSubagentRowLeavesParentUntouched(t *testing.T) {
	withAPIDB(t)

	parent := modelinfo.TokenCounts{Input: 100, Output: 50, CacheRead: 1000, CacheWrite: 200}
	sub := modelinfo.TokenCounts{Input: 10, Output: 5, CacheRead: 100, CacheWrite: 20}
	seedTurn(t, sessTurn("T1"), sessTurn("T1"), "agent-1", "claude-opus-5", parent, 1.00, sub, 0.10, closeT1)

	// Target a subagent that has no row at all.
	ApplyCostCorrections([]modelinfo.CostCorrection{{
		BilledAt: billed, SubagentTurnID: sessTurn("T1"), AgentID: "agent-NONE",
		Model:   "claude-opus-5",
		Counts:  modelinfo.TokenCounts{Input: 40, Output: 20, CacheRead: 400, CacheWrite: 80},
		CostUSD: 0.30,
	}})

	gotParent, parentCost := readRow(t, "turn_id = ? AND call_type = 'delegated_turn'", sessTurn("T1"))
	if gotParent != parent {
		t.Errorf("parent counts = %+v, want %+v UNCHANGED — the subagent row was "+
			"missing, so the parent's half must have rolled back", gotParent, parent)
	}
	if d := parentCost - 1.00; d > 1e-9 || d < -1e-9 {
		t.Errorf("parent cost = $%.6f, want $1.000000 unchanged — spend was "+
			"subtracted with nowhere to put it, i.e. destroyed", parentCost)
	}
}

// TestApplyCostCorrections_RefusesWhatTheParentCannotCover guards the direction
// that prices as a CREDIT. A parent row that cannot give up the amount means the
// correction is wrong; clamping would hide that and still move a wrong figure.
func TestApplyCostCorrections_RefusesWhatTheParentCannotCover(t *testing.T) {
	withAPIDB(t)

	parent := modelinfo.TokenCounts{Input: 10, Output: 5, CacheRead: 100, CacheWrite: 20}
	sub := modelinfo.TokenCounts{Input: 1, Output: 1, CacheRead: 10, CacheWrite: 2}
	seedTurn(t, sessTurn("T1"), sessTurn("T1"), "agent-1", "claude-opus-5", parent, 0.10, sub, 0.01, closeT1)

	ApplyCostCorrections([]modelinfo.CostCorrection{{
		BilledAt: billed, SubagentTurnID: sessTurn("T1"), AgentID: "agent-1",
		Model:   "claude-opus-5",
		Counts:  modelinfo.TokenCounts{Input: 999, Output: 999, CacheRead: 9999, CacheWrite: 999},
		CostUSD: 9.99,
	}})

	gotParent, parentCost := readRow(t, "turn_id = ? AND call_type = 'delegated_turn'", sessTurn("T1"))
	gotSub, subCost := readRow(t, "turn_id = ? AND call_type = 'subagent_turn'", sessTurn("T1"))
	if gotParent != parent || gotSub != sub {
		t.Errorf("rows moved on an uncoverable correction: parent %+v sub %+v", gotParent, gotSub)
	}
	if parentCost < 0 {
		t.Errorf("parent cost went negative (%.6f) — that prices as a credit", parentCost)
	}
	if d := (parentCost + subCost) - 0.11; d > 1e-9 || d < -1e-9 {
		t.Errorf("turn total = $%.6f, want $0.110000 unchanged", parentCost+subCost)
	}
}

// TestApplyCostCorrections_CrossTurnMovesBetweenDifferentTurns covers the case
// the original ticket got wrong: the turn that ABSORBED the spend and the turn
// that SPAWNED the agent are the same only when the subagent does not outlive
// its parent. Phase C files every subagent row under the spawning turn, so a
// cross-turn correction shifts per-turn totals while conserving the session's.
func TestApplyCostCorrections_CrossTurnMovesBetweenDifferentTurns(t *testing.T) {
	withAPIDB(t)

	parent := modelinfo.TokenCounts{Input: 100, Output: 50, CacheRead: 1000, CacheWrite: 200}
	sub := modelinfo.TokenCounts{Input: 10, Output: 5, CacheRead: 100, CacheWrite: 20}
	// Agent spawned in T1; the spend was billed inside T3's window.
	seedTurn(t, sessTurn("T3"), sessTurn("T1"), "agent-1", "claude-opus-5", parent, 1.00, sub, 0.10, closeT1)

	ApplyCostCorrections([]modelinfo.CostCorrection{{
		BilledAt: billed, SubagentTurnID: sessTurn("T1"), AgentID: "agent-1",
		Model:   "claude-opus-5",
		Counts:  modelinfo.TokenCounts{Input: 40, Output: 20, CacheRead: 400, CacheWrite: 80},
		CostUSD: 0.30,
	}})

	_, t3Cost := readRow(t, "turn_id = ? AND call_type = 'delegated_turn'", sessTurn("T3"))
	_, t1Cost := readRow(t, "turn_id = ? AND call_type = 'subagent_turn'", sessTurn("T1"))
	if d := t3Cost - 0.70; d > 1e-9 || d < -1e-9 {
		t.Errorf("T3 parent = $%.6f, want $0.700000", t3Cost)
	}
	if d := t1Cost - 0.40; d > 1e-9 || d < -1e-9 {
		t.Errorf("T1 subagent = $%.6f, want $0.400000", t1Cost)
	}
	if d := (t3Cost + t1Cost) - 1.10; d > 1e-9 || d < -1e-9 {
		t.Errorf("session total = $%.6f, want $1.100000", t3Cost+t1Cost)
	}
}

// TestApplyCostCorrections_ResolvesTheTurnWhoseWindowContainsTheBilling is the
// test for the mechanism that replaced the ring buffer.
//
// A turn is priced from the PREVIOUS result, so its window runs from the
// previous turn's close to its own and the timeline TILES — idle time belongs
// to the turn that follows it, not to the one before. Spend billed in a gap must
// therefore debit the NEXT turn to close, not the previous one.
//
// Verified against the live incident of 2026-09-13: spend billed at 15:41:40
// belongs to the turn closing 16:06:29, whose window opened at the 15:41:35
// result — twenty-five minutes of it idle.
func TestApplyCostCorrections_ResolvesTheTurnWhoseWindowContainsTheBilling(t *testing.T) {
	withAPIDB(t)

	mk := func(turn string, closedAt time.Time, cost float64) {
		c := modelinfo.TokenCounts{Input: 100, Output: 50, CacheRead: 1000, CacheWrite: 200}
		cc := cost
		API(APIEntry{
			Timestamp: closedAt, Session: "sess", Model: "claude-opus-5",
			CallType: "delegated_turn", TurnID: turn, Output: c.Output,
			Turn: &c, CalculatedCostUSD: &cc,
		})
	}
	early := time.Date(2026, 9, 13, 15, 39, 47, 0, time.UTC)
	mid := time.Date(2026, 9, 13, 15, 41, 35, 0, time.UTC)
	late := time.Date(2026, 9, 13, 16, 6, 29, 0, time.UTC)
	mk(sessTurn("EARLY"), early, 1.00)
	mk(sessTurn("MID"), mid, 1.00)
	mk(sessTurn("LATE"), late, 1.00)

	sub := modelinfo.TokenCounts{Input: 10, Output: 5, CacheRead: 100, CacheWrite: 20}
	sc := 0.10
	s := sub
	API(APIEntry{
		Timestamp: early, Session: "sess", Model: "claude-opus-5",
		CallType: "subagent_turn", TurnID: sessTurn("EARLY"), AgentID: "agent-1",
		Output: sub.Output, Turn: &s, CalculatedCostUSD: &sc,
	})

	// Billed at 15:41:40 — AFTER MID closed, inside LATE's window.
	ApplyCostCorrections([]modelinfo.CostCorrection{{
		BilledAt:       time.Date(2026, 9, 13, 15, 41, 40, 0, time.UTC),
		SubagentTurnID: sessTurn("EARLY"), AgentID: "agent-1", Model: "claude-opus-5",
		Counts:  modelinfo.TokenCounts{Input: 40, Output: 20, CacheRead: 400, CacheWrite: 80},
		CostUSD: 0.30,
	}})

	for _, tc := range []struct {
		turn string
		want float64
	}{
		{"LATE", 0.70},  // debited: its window contains 15:41:40
		{"MID", 1.00},   // untouched: it had already closed
		{"EARLY", 1.00}, // untouched
	} {
		_, cost := readRow(t, "turn_id = ? AND call_type = 'delegated_turn'", sessTurn(tc.turn))
		if d := cost - tc.want; d > 1e-9 || d < -1e-9 {
			t.Errorf("%s parent = $%.6f, want $%.6f — spend billed in the gap after "+
				"MID belongs to the turn that FOLLOWS it, because a turn is priced "+
				"from the previous result", tc.turn, cost, tc.want)
		}
	}
	_, subCost := readRow(t, "turn_id = ? AND call_type = 'subagent_turn'", sessTurn("EARLY"))
	if d := subCost - 0.40; d > 1e-9 || d < -1e-9 {
		t.Errorf("subagent = $%.6f, want $0.400000", subCost)
	}
}

// TestApplyCostCorrections_IgnoresOtherSessions: turn ids are globally unique but
// the resolution scans by SESSION and time, so a busy neighbour must not be able
// to supply the turn this correction debits.
func TestApplyCostCorrections_IgnoresOtherSessions(t *testing.T) {
	withAPIDB(t)

	other := modelinfo.TokenCounts{Input: 100, Output: 50, CacheRead: 1000, CacheWrite: 200}
	oc := 5.00
	o := other
	API(APIEntry{
		// Closes BETWEEN the billing time and our own turn's close. Without the
		// session filter this row wins the ORDER BY, which is exactly the bug
		// this test exists to catch — an earlier version of it put the
		// neighbour AFTER our turn, so ours won on time regardless and the
		// test could not fail.
		Timestamp: time.Date(2026, 9, 13, 15, 41, 32, 0, time.UTC),
		Session:   "OTHER", Model: "claude-opus-5", CallType: "delegated_turn",
		TurnID: "OTHER@X", Output: other.Output, Turn: &o, CalculatedCostUSD: &oc,
	})

	parent := modelinfo.TokenCounts{Input: 100, Output: 50, CacheRead: 1000, CacheWrite: 200}
	sub := modelinfo.TokenCounts{Input: 10, Output: 5, CacheRead: 100, CacheWrite: 20}
	seedTurn(t, sessTurn("T1"), sessTurn("T1"), "agent-1", "claude-opus-5",
		parent, 1.00, sub, 0.10, closeT1)

	ApplyCostCorrections([]modelinfo.CostCorrection{{
		BilledAt: billed, SubagentTurnID: sessTurn("T1"), AgentID: "agent-1",
		Model:   "claude-opus-5",
		Counts:  modelinfo.TokenCounts{Input: 40, Output: 20, CacheRead: 400, CacheWrite: 80},
		CostUSD: 0.30,
	}})

	_, otherCost := readRow(t, "turn_id = ?", "OTHER@X")
	if d := otherCost - 5.00; d > 1e-9 || d < -1e-9 {
		t.Errorf("neighbouring session's turn = $%.6f, want $5.000000 untouched", otherCost)
	}
	_, ours := readRow(t, "turn_id = ? AND call_type = 'delegated_turn'", sessTurn("T1"))
	if d := ours - 0.70; d > 1e-9 || d < -1e-9 {
		t.Errorf("our parent = $%.6f, want $0.700000", ours)
	}
}

// TestApplyCostCorrections_OrdersChronologicallyNotLexically.
//
// api_calls.ts is stored as local ISO WITH OFFSET (timeutil.Format). Across a
// DST boundary two rows carry different offsets, and then STRING order and time
// order disagree: "2026-10-25T01:15:00+00:00" sorts before
// "2026-10-25T01:30:00+01:00" but happens 45 minutes LATER. A bare `ts >= ?`
// therefore resolves the wrong turn, or none at all.
//
// This is the same trap that made the morning cost report read $172.06 against
// a true $161.17 — 6.8% — by letting SQLite convert a local-with-offset value
// (#1896). Rows are inserted with literal timestamps here so the offsets are
// fixed regardless of the host's timezone.
func TestApplyCostCorrections_OrdersChronologicallyNotLexically(t *testing.T) {
	withAPIDB(t)

	ins := func(turn, ts, callType, agent string, cost float64) {
		t.Helper()
		_, err := apiLog.db.Exec(`INSERT INTO api_calls
			(ts, session, model, call_type, turn_id, agent_id, output_tokens,
			 turn_input_tokens, turn_output_tokens, turn_cache_read_tokens,
			 turn_cache_write_tokens, calculated_cost_usd)
			VALUES (?, 'sess', 'claude-opus-5', ?, ?, ?, 50, 100, 50, 1000, 200, ?)`,
			ts, callType, turn, agent, cost)
		if err != nil {
			t.Fatalf("insert %s: %v", turn, err)
		}
	}
	// 01:30+01:00 is 00:30Z. 01:15+00:00 is 01:15Z — 45 minutes LATER, yet it
	// sorts FIRST as a string.
	ins(sessTurn("EARLIER"), "2026-10-25T01:30:00+01:00", "delegated_turn", "", 1.00)
	ins(sessTurn("LATER"), "2026-10-25T01:15:00+00:00", "delegated_turn", "", 1.00)
	ins(sessTurn("EARLIER"), "2026-10-25T01:30:00+01:00", "subagent_turn", "agent-1", 0.10)

	// Billed 00:45Z — after EARLIER closed, inside LATER's window.
	ApplyCostCorrections([]modelinfo.CostCorrection{{
		BilledAt:       time.Date(2026, 10, 25, 0, 45, 0, 0, time.UTC),
		SubagentTurnID: sessTurn("EARLIER"), AgentID: "agent-1", Model: "claude-opus-5",
		Counts:  modelinfo.TokenCounts{Input: 40, Output: 20, CacheRead: 400, CacheWrite: 80},
		CostUSD: 0.30,
	}})

	_, later := readRow(t, "turn_id = ? AND call_type = 'delegated_turn'", sessTurn("LATER"))
	_, earlier := readRow(t, "turn_id = ? AND call_type = 'delegated_turn'", sessTurn("EARLIER"))
	if d := later - 0.70; d > 1e-9 || d < -1e-9 {
		t.Errorf("LATER parent = $%.6f, want $0.700000 — it is the turn whose window "+
			"contains the billing, and only a chronological compare finds it", later)
	}
	if d := earlier - 1.00; d > 1e-9 || d < -1e-9 {
		t.Errorf("EARLIER parent = $%.6f, want $1.000000 untouched", earlier)
	}
}

// TestApplyCostCorrections_DebitsTheTurnThatWasRUNNING is the defect the first
// attempt at #1918 shipped, written before the fix (#1922).
//
// api_calls.ts is the turn's START (turn_delegated.go passes ts.StartedAt, and
// the turn_id nanos match ts exactly). Resolving "first turn at or after t" by
// ts therefore means "first turn to START at or after t" — so spend billed while
// a turn is RUNNING skips that turn and debits the NEXT one, which never
// absorbed it. Every subagent a turn spawns bills while that turn runs, so this
// is the whole target population.
//
// The close is ts + duration_ms; verified against the log's own
// turn_lifecycle event=complete lines (start 15:41:35 + 15648ms -> 15:41:50,
// logged 15:41:50).
func TestApplyCostCorrections_DebitsTheTurnThatWasRUNNING(t *testing.T) {
	withAPIDB(t)

	ins := func(turn, ts string, durMS int, callType, agent string, cost float64) {
		t.Helper()
		if _, err := apiLog.db.Exec(`INSERT INTO api_calls
			(ts, session, model, call_type, turn_id, agent_id, duration_ms, output_tokens,
			 turn_input_tokens, turn_output_tokens, turn_cache_read_tokens,
			 turn_cache_write_tokens, calculated_cost_usd)
			VALUES (?, 'sess', 'claude-opus-5', ?, ?, ?, ?, 50, 100, 50, 1000, 200, ?)`,
			ts, callType, turn, agent, durMS, cost); err != nil {
			t.Fatalf("insert %s: %v", turn, err)
		}
	}
	// B runs 14:20:46 -> 14:27:43. C starts later.
	ins(sessTurn("B"), "2026-09-14T14:20:46+01:00", 416846, "delegated_turn", "", 1.00)
	ins(sessTurn("C"), "2026-09-14T14:28:36+01:00", 33295, "delegated_turn", "", 1.00)
	ins(sessTurn("B"), "2026-09-14T14:20:46+01:00", 416846, "subagent_turn", "agent-1", 0.10)

	// Billed 14:25:00 — INSIDE B's run. B's ModelUsage contains it; B's parent
	// absorbed it.
	ApplyCostCorrections([]modelinfo.CostCorrection{{
		BilledAt:       time.Date(2026, 9, 14, 13, 25, 0, 0, time.UTC), // 14:25:00 +01:00
		SubagentTurnID: sessTurn("B"), AgentID: "agent-1", Model: "claude-opus-5",
		Counts:  modelinfo.TokenCounts{Input: 40, Output: 20, CacheRead: 400, CacheWrite: 80},
		CostUSD: 0.30,
	}})

	_, bCost := readRow(t, "turn_id = ? AND call_type = 'delegated_turn'", sessTurn("B"))
	_, cCost := readRow(t, "turn_id = ? AND call_type = 'delegated_turn'", sessTurn("C"))
	if d := bCost - 0.70; d > 1e-9 || d < -1e-9 {
		t.Errorf("B parent = $%.6f, want $0.700000 — the spend was billed while B was "+
			"RUNNING, so B absorbed it; resolving by ts (the START) skips B entirely", bCost)
	}
	if d := cCost - 1.00; d > 1e-9 || d < -1e-9 {
		t.Errorf("C parent = $%.6f, want $1.000000 untouched — C had not started when "+
			"the spend was billed and cannot have absorbed it", cCost)
	}
}

// TestAccumulateSubagentRow_KeepsOneRowPerDelegation is defect 2 (#1922).
//
// A subagent that outlives its parent is written once per turn it straddles,
// all under the SPAWNING turn id. Phase C INSERTed each time, so live data holds
// keys with 2 and 3 rows — meaning "what did that delegation cost" needed a SUM,
// and the #1918 correction (which requires exactly one match) could never apply
// to the very population it exists for.
func TestAccumulateSubagentRow_KeepsOneRowPerDelegation(t *testing.T) {
	withAPIDB(t)

	write := func(in, out, cr, cw int, cost float64) {
		c := modelinfo.TokenCounts{Input: in, Output: out, CacheRead: cr, CacheWrite: cw}
		cc := cost
		AccumulateSubagentRow(APIEntry{
			Timestamp: closeT1, Session: "sess", Model: "claude-opus-5",
			CallType: "subagent_turn", TurnID: sessTurn("T1"), AgentID: "agent-1",
			Output: out, Turn: &c, CalculatedCostUSD: &cc, DurationMS: 1000,
		})
	}
	// Three turns' worth of the same delegation's spend.
	write(10, 100, 1000, 100, 0.10)
	write(5, 50, 500, 50, 0.05)
	write(1, 10, 100, 10, 0.01)

	var n int
	if err := apiLog.db.QueryRow(`SELECT COUNT(*) FROM api_calls
		WHERE call_type='subagent_turn' AND turn_id=? AND agent_id=? AND model=?`,
		sessTurn("T1"), "agent-1", "claude-opus-5").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rows for one delegation = %d, want 1 — several rows sharing the key "+
			"is what made the #1918 credit unapplicable", n)
	}

	got, cost := readRow(t, "turn_id=? AND call_type='subagent_turn'", sessTurn("T1"))
	want := modelinfo.TokenCounts{Input: 16, Output: 160, CacheRead: 1600, CacheWrite: 160}
	if got != want {
		t.Errorf("accumulated counts = %+v, want %+v — collapsing must SUM, not replace", got, want)
	}
	if d := cost - 0.16; d > 1e-9 || d < -1e-9 {
		t.Errorf("accumulated cost = $%.6f, want $0.160000", cost)
	}
	var outTok int
	if err := apiLog.db.QueryRow(`SELECT output_tokens FROM api_calls
		WHERE call_type='subagent_turn' AND turn_id=?`, sessTurn("T1")).Scan(&outTok); err != nil {
		t.Fatal(err)
	}
	if outTok != 160 {
		t.Errorf("output_tokens = %d, want 160 — it must track turn_output_tokens on a "+
			"subagent row or that row stops re-pricing to its own cost (#1854)", outTok)
	}
}

// TestApplyCostCorrections_CollapsedRowIsCreditable closes the loop: a
// delegation written through AccumulateSubagentRow has exactly one row, so the
// correction that could never apply now does.
func TestApplyCostCorrections_CollapsedRowIsCreditable(t *testing.T) {
	withAPIDB(t)

	if _, err := apiLog.db.Exec(`INSERT INTO api_calls
		(ts, session, model, call_type, turn_id, duration_ms, output_tokens,
		 turn_input_tokens, turn_output_tokens, turn_cache_read_tokens,
		 turn_cache_write_tokens, calculated_cost_usd)
		VALUES ('2026-09-13T15:41:35+01:00','sess','claude-opus-5','delegated_turn',?,
		        15648, 50, 100, 50, 1000, 200, 1.00)`, sessTurn("T1")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		c := modelinfo.TokenCounts{Input: 5, Output: 5, CacheRead: 50, CacheWrite: 5}
		cc := 0.05
		AccumulateSubagentRow(APIEntry{
			Timestamp: closeT1, Session: "sess", Model: "claude-opus-5",
			CallType: "subagent_turn", TurnID: sessTurn("T1"), AgentID: "agent-1",
			Output: 5, Turn: &c, CalculatedCostUSD: &cc, DurationMS: 1000,
		})
	}

	ApplyCostCorrections([]modelinfo.CostCorrection{{
		BilledAt:       time.Date(2026, 9, 13, 14, 41, 40, 0, time.UTC), // inside T1's run
		SubagentTurnID: sessTurn("T1"), AgentID: "agent-1", Model: "claude-opus-5",
		Counts:  modelinfo.TokenCounts{Input: 40, Output: 20, CacheRead: 400, CacheWrite: 80},
		CostUSD: 0.30,
	}})

	_, parent := readRow(t, "turn_id=? AND call_type='delegated_turn'", sessTurn("T1"))
	_, sub := readRow(t, "turn_id=? AND call_type='subagent_turn'", sessTurn("T1"))
	if d := parent - 0.70; d > 1e-9 || d < -1e-9 {
		t.Errorf("parent = $%.6f, want $0.700000 — with one row per delegation the "+
			"credit has a unique target and the correction applies", parent)
	}
	if d := sub - 0.45; d > 1e-9 || d < -1e-9 {
		t.Errorf("subagent = $%.6f, want $0.450000 ($0.15 accumulated + $0.30 moved)", sub)
	}
}

// TestApplyCostCorrections_LegacyDuplicateRowsAreRefused: rows written before
// #1922 keep their duplicates. The correction must refuse those rather than pick
// one arbitrarily — guessing which of three rows should carry the spend is not
// better than leaving the attribution alone, and the parent's debit must roll
// back with it.
func TestApplyCostCorrections_LegacyDuplicateRowsAreRefused(t *testing.T) {
	withAPIDB(t)

	if _, err := apiLog.db.Exec(`INSERT INTO api_calls
		(ts, session, model, call_type, turn_id, duration_ms, output_tokens,
		 turn_input_tokens, turn_output_tokens, turn_cache_read_tokens,
		 turn_cache_write_tokens, calculated_cost_usd)
		VALUES ('2026-09-13T15:41:35+01:00','sess','claude-opus-5','delegated_turn',?,
		        15648, 50, 100, 50, 1000, 200, 1.00)`, sessTurn("T1")); err != nil {
		t.Fatal(err)
	}
	// The pre-#1922 shape: three rows sharing one key, as live data holds.
	for i := 0; i < 3; i++ {
		if _, err := apiLog.db.Exec(`INSERT INTO api_calls
			(ts, session, model, call_type, turn_id, agent_id, duration_ms, output_tokens,
			 turn_input_tokens, turn_output_tokens, turn_cache_read_tokens,
			 turn_cache_write_tokens, calculated_cost_usd)
			VALUES ('2026-09-13T15:41:35+01:00','sess','claude-opus-5','subagent_turn',?,
			        'agent-1', 1000, 5, 5, 5, 50, 5, 0.05)`, sessTurn("T1")); err != nil {
			t.Fatal(err)
		}
	}

	ApplyCostCorrections([]modelinfo.CostCorrection{{
		BilledAt:       time.Date(2026, 9, 13, 14, 41, 40, 0, time.UTC),
		SubagentTurnID: sessTurn("T1"), AgentID: "agent-1", Model: "claude-opus-5",
		Counts:  modelinfo.TokenCounts{Input: 40, Output: 20, CacheRead: 400, CacheWrite: 80},
		CostUSD: 0.30,
	}})

	_, parent := readRow(t, "turn_id=? AND call_type='delegated_turn'", sessTurn("T1"))
	if d := parent - 1.00; d > 1e-9 || d < -1e-9 {
		t.Errorf("parent = $%.6f, want $1.000000 UNCHANGED — three candidate credit "+
			"rows means the whole correction rolls back, debit included", parent)
	}
	var total float64
	if err := apiLog.db.QueryRow(`SELECT SUM(calculated_cost_usd) FROM api_calls
		WHERE call_type='subagent_turn' AND turn_id=?`, sessTurn("T1")).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if d := total - 0.15; d > 1e-9 || d < -1e-9 {
		t.Errorf("subagent total = $%.6f, want $0.150000 unchanged", total)
	}
}

// TestAccumulateSubagentRow_JSONLKeepsEveryInstalment: the db collapses a
// delegation to ONE row so a lookup is correct; the JSONL cannot be updated, so
// it must keep every instalment as its own line. Every JSONL reader sums rows,
// so both stores then agree on the total by different means. Writing only the
// first instalment would make the fallback under-report.
func TestAccumulateSubagentRow_JSONLKeepsEveryInstalment(t *testing.T) {
	resetGlobal()
	t.Cleanup(resetGlobal)
	dir := t.TempDir()
	jsonlPath := filepath.Join(dir, "api.jsonl")
	Init(Config{APIFile: jsonlPath})
	if err := InitAPIDB(filepath.Join(dir, "api.db")); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	t.Cleanup(CloseAPIDB)

	for i := 0; i < 3; i++ {
		c := modelinfo.TokenCounts{Input: 5, Output: 5, CacheRead: 50, CacheWrite: 5}
		cc := 0.05
		AccumulateSubagentRow(APIEntry{
			Timestamp: closeT1, Session: "sess", Model: "claude-opus-5",
			CallType: "subagent_turn", TurnID: sessTurn("T1"), AgentID: "agent-1",
			Output: 5, Turn: &c, CalculatedCostUSD: &cc, DurationMS: 1000,
		})
	}

	var dbRows int
	var dbCost float64
	if err := apiLog.db.QueryRow(`SELECT COUNT(*), SUM(calculated_cost_usd) FROM api_calls
		WHERE call_type='subagent_turn'`).Scan(&dbRows, &dbCost); err != nil {
		t.Fatal(err)
	}
	if dbRows != 1 {
		t.Errorf("db rows = %d, want 1 (collapsed)", dbRows)
	}

	entries := ReadAPILog(jsonlPath)
	var jsonlCost float64
	n := 0
	for _, e := range entries {
		if e.IsSubagent() {
			n++
			jsonlCost += e.EffectiveCost()
		}
	}
	if n != 3 {
		t.Errorf("JSONL subagent lines = %d, want 3 — the JSONL is append-only, so "+
			"every instalment must appear or the fallback under-reports", n)
	}
	if d := jsonlCost - dbCost; d > 1e-9 || d < -1e-9 {
		t.Errorf("JSONL total $%.6f != db total $%.6f — the two stores must agree on "+
			"the money even though only one of them collapses", jsonlCost, dbCost)
	}
}
