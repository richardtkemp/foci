package log

import (
	"path/filepath"
	"testing"

	"foci/internal/modelinfo"
)

// seedTurn writes a parent row for turnID and a subagent row for (spawnTurn,
// agentID), with the counts and cost a correction will move between them.
func seedTurn(t *testing.T, parentTurn, spawnTurn, agentID, model string,
	parent modelinfo.TokenCounts, parentCost float64,
	sub modelinfo.TokenCounts, subCost float64) {
	t.Helper()
	pc, sc := parentCost, subCost
	p, s := parent, sub
	API(APIEntry{
		Session: "sess", Model: model, CallType: "delegated_turn", TurnID: parentTurn,
		Output: parent.Output, Turn: &p, CalculatedCostUSD: &pc,
	})
	API(APIEntry{
		Session: "sess", Model: model, CallType: "subagent_turn", TurnID: spawnTurn,
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
	seedTurn(t, "T1", "T1", "agent-1", "claude-opus-5", parent, 1.00, sub, 0.10)

	move := modelinfo.TokenCounts{Input: 40, Output: 20, CacheRead: 400, CacheWrite: 80}
	ApplyCostCorrections([]modelinfo.CostCorrection{{
		ParentTurnID: "T1", SubagentTurnID: "T1", AgentID: "agent-1",
		Model: "claude-opus-5", Counts: move, CostUSD: 0.30,
	}})

	gotParent, parentCost := readRow(t, "turn_id = ? AND call_type = 'delegated_turn'", "T1")
	gotSub, subCost := readRow(t, "turn_id = ? AND call_type = 'subagent_turn'", "T1")

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
	seedTurn(t, "T1", "T1", "agent-1", "claude-opus-5", parent, 1.00, sub, 0.10)

	// Target a subagent that has no row at all.
	ApplyCostCorrections([]modelinfo.CostCorrection{{
		ParentTurnID: "T1", SubagentTurnID: "T1", AgentID: "agent-NONE",
		Model:   "claude-opus-5",
		Counts:  modelinfo.TokenCounts{Input: 40, Output: 20, CacheRead: 400, CacheWrite: 80},
		CostUSD: 0.30,
	}})

	gotParent, parentCost := readRow(t, "turn_id = ? AND call_type = 'delegated_turn'", "T1")
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
	seedTurn(t, "T1", "T1", "agent-1", "claude-opus-5", parent, 0.10, sub, 0.01)

	ApplyCostCorrections([]modelinfo.CostCorrection{{
		ParentTurnID: "T1", SubagentTurnID: "T1", AgentID: "agent-1",
		Model:   "claude-opus-5",
		Counts:  modelinfo.TokenCounts{Input: 999, Output: 999, CacheRead: 9999, CacheWrite: 999},
		CostUSD: 9.99,
	}})

	gotParent, parentCost := readRow(t, "turn_id = ? AND call_type = 'delegated_turn'", "T1")
	gotSub, subCost := readRow(t, "turn_id = ? AND call_type = 'subagent_turn'", "T1")
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
	seedTurn(t, "T3", "T1", "agent-1", "claude-opus-5", parent, 1.00, sub, 0.10)

	ApplyCostCorrections([]modelinfo.CostCorrection{{
		ParentTurnID: "T3", SubagentTurnID: "T1", AgentID: "agent-1",
		Model:   "claude-opus-5",
		Counts:  modelinfo.TokenCounts{Input: 40, Output: 20, CacheRead: 400, CacheWrite: 80},
		CostUSD: 0.30,
	}})

	_, t3Cost := readRow(t, "turn_id = ? AND call_type = 'delegated_turn'", "T3")
	_, t1Cost := readRow(t, "turn_id = ? AND call_type = 'subagent_turn'", "T1")
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
