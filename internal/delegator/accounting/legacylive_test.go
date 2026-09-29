package accounting

import (
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/log"
	"foci/internal/modelinfo"
)

// The #1918/#1922 tests, ported from internal/log's pre-ledger writer to the
// legacy path the not-yet-switched delegated backends book through
// (legacylive.go). The mechanism is the same; the rows are legacy calls.

// seedLegacy books a parent row for parentTurn and a subagent share for
// (spawnTurn, agentID), with the counts and cost a correction moves between
// them. The parent's turn starts at startedAt and runs 15,648ms: the correction
// resolves by a turn's CLOSE, and a zero-length turn could not tell that from
// resolving by its start (#1924).
func seedLegacy(t *testing.T, l *Ledger, parentTurn, spawnTurn, agentID, model string,
	parent modelinfo.TokenCounts, parentCost float64,
	sub modelinfo.TokenCounts, subCost float64, startedAt time.Time) {
	t.Helper()
	pc, sc := parentCost, subCost
	p, s := parent, sub
	sess, _, _ := strings.Cut(parentTurn, "@")
	if err := l.BookLegacy(LegacyRow{
		At: startedAt, DurationMS: 15648, Session: sess, Model: model, TurnID: parentTurn,
		Fill: LegacyFill{Output: parent.Output}, Turn: &p, CalculatedCostUSD: &pc,
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.BookLegacy(LegacyRow{
		At: startedAt, Session: sess, Model: model, TurnID: spawnTurn,
		Subagent: true, SubagentID: agentID, Turn: &s, CalculatedCostUSD: &sc,
	}); err != nil {
		t.Fatal(err)
	}
}

// bookParent books one legacy parent row on turn, starting at startedAt.
func bookParent(t *testing.T, l *Ledger, turn string, startedAt time.Time, durMS int64, cost float64) {
	t.Helper()
	c := modelinfo.TokenCounts{Input: 100, Output: 50, CacheRead: 1000, CacheWrite: 200}
	if err := l.BookLegacy(LegacyRow{
		At: startedAt, DurationMS: durMS, Session: "sess", Model: "claude-opus-5", TurnID: turn,
		Turn: &c, CalculatedCostUSD: &cost,
	}); err != nil {
		t.Fatal(err)
	}
}

// bookShare books one legacy subagent share of agent-1 on turn.
func bookShare(t *testing.T, l *Ledger, turn string, startedAt time.Time, cost float64) {
	t.Helper()
	c := modelinfo.TokenCounts{Input: 10, Output: 5, CacheRead: 100, CacheWrite: 20}
	if err := l.BookLegacy(LegacyRow{
		At: startedAt, Session: "sess", Model: "claude-opus-5", TurnID: turn,
		Subagent: true, SubagentID: "agent-1", Turn: &c, CalculatedCostUSD: &cost,
	}); err != nil {
		t.Fatal(err)
	}
}

// readLegacy reads the legacy parent call (sub false) or the subagent share of
// turn: its counts, every cache-write class as one, and its recorded cost.
func readLegacy(t *testing.T, l *Ledger, turn string, sub bool) (modelinfo.TokenCounts, float64) {
	t.Helper()
	op := "="
	if sub {
		op = "<>"
	}
	var id int64
	var cost sql.NullFloat64
	if err := l.db.QueryRow(`SELECT id, legacy_calculated_cost_usd FROM api_calls
		WHERE kind = 'legacy' AND turn_id = ? AND actor `+op+` ''`, turn).Scan(&id, &cost); err != nil {
		t.Fatalf("read %s (sub=%v): %v", turn, sub, err)
	}
	tok := map[string]int{}
	rows, err := l.db.Query(`SELECT class, count FROM call_tokens WHERE call_id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var c string
		var n int
		if err := rows.Scan(&c, &n); err != nil {
			t.Fatal(err)
		}
		tok[c] = n
	}
	return modelinfo.TokenCounts{
		Input: tok["input"], Output: tok["output"], CacheRead: tok["cache_read"],
		CacheWrite: tok["cache_write_5m"] + tok["cache_write_1h"] + tok["cache_write"],
	}, cost.Float64
}

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-9 }

func sessTurn(name string) string { return "sess@" + name }

var (
	startT1 = time.Date(2026, 9, 13, 14, 41, 35, 0, time.UTC)
	billed  = time.Date(2026, 9, 13, 14, 41, 30, 0, time.UTC)
	moveAll = modelinfo.TokenCounts{Input: 40, Output: 20, CacheRead: 400, CacheWrite: 80}
)

// TestLegacyCorrectionMovesSpendAndConservesTheTotal is the core of #1918: a
// subagent's spend reached foci after the turn that paid for it closed, so its
// parent absorbed it; the correction moves it onto the subagent's call, and
// with no TTL surcharge the total does not change. The cache writes move from
// the parent's 1h class (the kind rule for a CC parent) to the subagent's 5m.
func TestLegacyCorrectionMovesSpendAndConservesTheTotal(t *testing.T) {
	l, _ := openLedger(t)
	parent := modelinfo.TokenCounts{Input: 100, Output: 50, CacheRead: 1000, CacheWrite: 200}
	sub := modelinfo.TokenCounts{Input: 10, Output: 5, CacheRead: 100, CacheWrite: 20}
	seedLegacy(t, l, sessTurn("T1"), sessTurn("T1"), "agent-1", "claude-opus-5", parent, 1.00, sub, 0.10, startT1)

	l.ApplyLegacyCorrections([]modelinfo.CostCorrection{{
		BilledAt: billed, SubagentTurnID: sessTurn("T1"), AgentID: "agent-1",
		Model: "claude-opus-5", Counts: moveAll, CostUSD: 0.30,
	}})

	gotParent, parentCost := readLegacy(t, l, sessTurn("T1"), false)
	gotSub, subCost := readLegacy(t, l, sessTurn("T1"), true)
	if want := (modelinfo.TokenCounts{Input: 60, Output: 30, CacheRead: 600, CacheWrite: 120}); gotParent != want {
		t.Errorf("parent counts = %+v, want %+v", gotParent, want)
	}
	if want := (modelinfo.TokenCounts{Input: 50, Output: 25, CacheRead: 500, CacheWrite: 100}); gotSub != want {
		t.Errorf("subagent counts = %+v, want %+v", gotSub, want)
	}
	if !near(parentCost+subCost, 1.10) || !near(parentCost, 0.70) {
		t.Errorf("parent $%.6f + subagent $%.6f, want $0.70 + $0.40 — a correction MOVES spend", parentCost, subCost)
	}
	var w5m int
	if err := l.db.QueryRow(`SELECT count FROM call_tokens k JOIN api_calls c ON c.id = k.call_id
		WHERE c.actor = 'agent-1' AND k.class = 'cache_write_5m'`).Scan(&w5m); err != nil || w5m != 100 {
		t.Errorf("subagent 5m writes = %d (%v), want 100: every write it holds was observed 5m", w5m, err)
	}
	// The views read the moved figures.
	var view float64
	if err := l.db.QueryRow(`SELECT TOTAL(cost_usd) FROM call_costs`).Scan(&view); err != nil || !near(view, 1.10) {
		t.Errorf("call_costs total = $%.6f (%v), want $1.10", view, err)
	}
}

// TestLegacyCorrectionDebitsParentAtItsOwnBasis is #1929 D1: the parent gives
// up what it was CHARGED (CostUSD + TTLSurchargeUSD), the subagent gains what
// it OBSERVED (CostUSD). Tokens move one-for-one; the total falls by the
// over-charge.
func TestLegacyCorrectionDebitsParentAtItsOwnBasis(t *testing.T) {
	l, _ := openLedger(t)
	parent := modelinfo.TokenCounts{Input: 100, Output: 50, CacheRead: 1000, CacheWrite: 200}
	sub := modelinfo.TokenCounts{Input: 10, Output: 5, CacheRead: 100, CacheWrite: 20}
	seedLegacy(t, l, sessTurn("T1"), sessTurn("T1"), "agent-1", "claude-opus-5", parent, 1.00, sub, 0.10, startT1)

	l.ApplyLegacyCorrections([]modelinfo.CostCorrection{{
		BilledAt: billed, SubagentTurnID: sessTurn("T1"), AgentID: "agent-1",
		Model: "claude-opus-5", Counts: modelinfo.TokenCounts{CacheWrite: 80}, CostUSD: 0.25, TTLSurchargeUSD: 0.15,
	}})

	gotParent, parentCost := readLegacy(t, l, sessTurn("T1"), false)
	gotSub, subCost := readLegacy(t, l, sessTurn("T1"), true)
	if gotParent.CacheWrite != 120 || gotSub.CacheWrite != 100 {
		t.Errorf("cache-write parent=%d sub=%d, want 120 and 100", gotParent.CacheWrite, gotSub.CacheWrite)
	}
	if !near(parentCost, 0.60) || !near(subCost, 0.35) {
		t.Errorf("parent $%.6f sub $%.6f, want $0.60 (debited at its basis) and $0.35 (credited at its own)", parentCost, subCost)
	}
}

// TestLegacyCorrectionIsAllOrNothing: half a correction is worse than none.
// A missing subagent call, a parent that cannot cover the move, and several
// candidate subagent calls (pre-#1922 history) each leave everything as it
// was.
func TestLegacyCorrectionIsAllOrNothing(t *testing.T) {
	parent := modelinfo.TokenCounts{Input: 100, Output: 50, CacheRead: 1000, CacheWrite: 200}
	sub := modelinfo.TokenCounts{Input: 10, Output: 5, CacheRead: 100, CacheWrite: 20}
	for _, tc := range []struct {
		name  string
		agent string
		move  modelinfo.TokenCounts
		cost  float64
		dups  bool
	}{
		{"missing subagent call", "agent-NONE", moveAll, 0.30, false},
		{"parent cannot cover", "agent-1", modelinfo.TokenCounts{Input: 999, Output: 999, CacheRead: 9999, CacheWrite: 999}, 9.99, false},
		{"parent cannot cover the money", "agent-1", modelinfo.TokenCounts{Input: 1}, 5.00, false},
		{"several candidate subagent calls", "agent-1", moveAll, 0.30, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := openLedger(t)
			seedLegacy(t, l, sessTurn("T1"), sessTurn("T1"), "agent-1", "claude-opus-5", parent, 1.00, sub, 0.10, startT1)
			if tc.dups {
				bookShare(t, l, sessTurn("T1"), startT1, 0.10)
			}
			l.ApplyLegacyCorrections([]modelinfo.CostCorrection{{
				BilledAt: billed, SubagentTurnID: sessTurn("T1"), AgentID: tc.agent,
				Model: "claude-opus-5", Counts: tc.move, CostUSD: tc.cost,
			}})
			gotParent, parentCost := readLegacy(t, l, sessTurn("T1"), false)
			if gotParent != parent || !near(parentCost, 1.00) {
				t.Errorf("parent = %+v $%.6f, want %+v $1.00 UNCHANGED", gotParent, parentCost, parent)
			}
			var subTotal float64
			if err := l.db.QueryRow(`SELECT TOTAL(legacy_calculated_cost_usd) FROM api_calls WHERE actor <> ''`).Scan(&subTotal); err != nil {
				t.Fatal(err)
			}
			want := 0.10
			if tc.dups {
				want = 0.20
			}
			if !near(subTotal, want) {
				t.Errorf("subagent total = $%.6f, want $%.2f unchanged", subTotal, want)
			}
		})
	}
}

// TestLegacyCorrectionCrossTurn: the turn that ABSORBED the spend and the one
// that SPAWNED the agent differ when the subagent outlives its parent; the
// move shifts per-turn totals and conserves the session's.
func TestLegacyCorrectionCrossTurn(t *testing.T) {
	l, _ := openLedger(t)
	parent := modelinfo.TokenCounts{Input: 100, Output: 50, CacheRead: 1000, CacheWrite: 200}
	sub := modelinfo.TokenCounts{Input: 10, Output: 5, CacheRead: 100, CacheWrite: 20}
	seedLegacy(t, l, sessTurn("T3"), sessTurn("T1"), "agent-1", "claude-opus-5", parent, 1.00, sub, 0.10, startT1)

	l.ApplyLegacyCorrections([]modelinfo.CostCorrection{{
		BilledAt: billed, SubagentTurnID: sessTurn("T1"), AgentID: "agent-1",
		Model: "claude-opus-5", Counts: moveAll, CostUSD: 0.30,
	}})
	_, t3 := readLegacy(t, l, sessTurn("T3"), false)
	_, t1 := readLegacy(t, l, sessTurn("T1"), true)
	if !near(t3, 0.70) || !near(t1, 0.40) {
		t.Errorf("T3 parent $%.6f, T1 subagent $%.6f; want $0.70 and $0.40", t3, t1)
	}
}

// TestLegacyCorrectionResolvesByClose is the tiling rule, with the real turn
// timings of 2026-09-13: a turn is priced from the previous result, so the
// turn that absorbed spend billed at t is the first of the session to CLOSE at
// or after t — the one RUNNING at t (#1922: resolving by start debited the
// next turn), or, for spend billed in an idle gap, the one that follows it.
func TestLegacyCorrectionResolvesByClose(t *testing.T) {
	for _, tc := range []struct {
		name   string
		billed time.Time
		want   map[string]float64
	}{
		// 14:41:40 is INSIDE MID's run (14:41:35 .. 14:41:50).
		{"billed while MID ran", time.Date(2026, 9, 13, 14, 41, 40, 0, time.UTC),
			map[string]float64{"EARLY": 1.00, "MID": 0.70, "LATE": 1.00}},
		// 14:41:00 is after EARLY closed (14:40:16), before MID started.
		{"billed in the idle gap", time.Date(2026, 9, 13, 14, 41, 0, 0, time.UTC),
			map[string]float64{"EARLY": 1.00, "MID": 0.70, "LATE": 1.00}},
		// The same instant stated in another zone: stored times are UTC, so a
		// zone can never reorder turns (#1896's lexical trap).
		{"billed in another zone", time.Date(2026, 9, 13, 15, 41, 40, 0, time.FixedZone("BST", 3600)),
			map[string]float64{"EARLY": 1.00, "MID": 0.70, "LATE": 1.00}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := openLedger(t)
			bookParent(t, l, sessTurn("EARLY"), time.Date(2026, 9, 13, 14, 39, 47, 0, time.UTC), 29919, 1.00)
			// MID is booked with its start stated in BST: 15:41:35+01:00 is 14:41:35Z.
			bookParent(t, l, sessTurn("MID"), time.Date(2026, 9, 13, 15, 41, 35, 0, time.FixedZone("BST", 3600)), 15648, 1.00)
			bookParent(t, l, sessTurn("LATE"), time.Date(2026, 9, 13, 15, 6, 29, 0, time.UTC), 87910, 1.00)
			bookShare(t, l, sessTurn("EARLY"), time.Date(2026, 9, 13, 14, 39, 47, 0, time.UTC), 0.10)

			l.ApplyLegacyCorrections([]modelinfo.CostCorrection{{
				BilledAt: tc.billed, SubagentTurnID: sessTurn("EARLY"), AgentID: "agent-1",
				Model: "claude-opus-5", Counts: moveAll, CostUSD: 0.30,
			}})
			for turn, want := range tc.want {
				if _, got := readLegacy(t, l, sessTurn(turn), false); !near(got, want) {
					t.Errorf("%s parent = $%.6f, want $%.2f", turn, got, want)
				}
			}
			if _, got := readLegacy(t, l, sessTurn("EARLY"), true); !near(got, 0.40) {
				t.Errorf("subagent = $%.6f, want $0.40", got)
			}
		})
	}
}

// TestLegacyCorrectionIgnoresOtherSessions: a neighbouring session's turn
// closing between the billing and our turn's close must not be debited.
func TestLegacyCorrectionIgnoresOtherSessions(t *testing.T) {
	l, _ := openLedger(t)
	oc := 5.00
	o := modelinfo.TokenCounts{Input: 100, Output: 50, CacheRead: 1000, CacheWrite: 200}
	if err := l.BookLegacy(LegacyRow{
		At: time.Date(2026, 9, 13, 14, 41, 32, 0, time.UTC), Session: "OTHER", Model: "claude-opus-5",
		TurnID: "OTHER@X", Turn: &o, CalculatedCostUSD: &oc,
	}); err != nil {
		t.Fatal(err)
	}
	parent := modelinfo.TokenCounts{Input: 100, Output: 50, CacheRead: 1000, CacheWrite: 200}
	sub := modelinfo.TokenCounts{Input: 10, Output: 5, CacheRead: 100, CacheWrite: 20}
	seedLegacy(t, l, sessTurn("T1"), sessTurn("T1"), "agent-1", "claude-opus-5", parent, 1.00, sub, 0.10, startT1)

	l.ApplyLegacyCorrections([]modelinfo.CostCorrection{{
		BilledAt: billed, SubagentTurnID: sessTurn("T1"), AgentID: "agent-1",
		Model: "claude-opus-5", Counts: moveAll, CostUSD: 0.30,
	}})
	if _, other := readLegacy(t, l, "OTHER@X", false); !near(other, 5.00) {
		t.Errorf("neighbouring session's turn = $%.6f, want $5.00 untouched", other)
	}
	if _, ours := readLegacy(t, l, sessTurn("T1"), false); !near(ours, 0.70) {
		t.Errorf("our parent = $%.6f, want $0.70", ours)
	}
}

// TestAccumulateLegacySubagentKeepsOneCall is #1922: a subagent that outlives
// its parent is booked once per turn it straddles, all under the SPAWNING
// turn; folding keeps one call per delegation, so a lookup answers "what did
// it cost" and a correction has exactly one target. The share must not
// rewrite the spawning turn's end either, which the correction resolves by.
func TestAccumulateLegacySubagentKeepsOneCall(t *testing.T) {
	l, _ := openLedger(t)
	bookParent(t, l, sessTurn("T1"), startT1, 15648, 1.00)
	var ended string
	if err := l.db.QueryRow(`SELECT ended_at FROM turns WHERE turn_id = ?`, sessTurn("T1")).Scan(&ended); err != nil {
		t.Fatal(err)
	}

	for i, c := range []modelinfo.TokenCounts{
		{Input: 10, Output: 100, CacheRead: 1000, CacheWrite: 100},
		{Input: 5, Output: 50, CacheRead: 500, CacheWrite: 50},
		{Input: 1, Output: 10, CacheRead: 100, CacheWrite: 10},
	} {
		counts, cost := c, []float64{0.10, 0.05, 0.01}[i]
		merged, err := l.AccumulateLegacySubagent(LegacyRow{
			// Each instalment is written at a LATER turn's end.
			At: startT1.Add(time.Duration(i) * time.Hour), DurationMS: 1000, Session: "sess",
			Model: "claude-opus-5", TurnID: sessTurn("T1"), Subagent: true, SubagentID: "agent-1",
			Turn: &counts, CalculatedCostUSD: &cost,
		})
		if err != nil || merged != (i > 0) {
			t.Fatalf("instalment %d: merged=%v err=%v, want merged=%v", i, merged, err, i > 0)
		}
	}
	var n int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM api_calls WHERE actor = 'agent-1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("calls for one delegation = %d (%v), want 1", n, err)
	}
	got, cost := readLegacy(t, l, sessTurn("T1"), true)
	if want := (modelinfo.TokenCounts{Input: 16, Output: 160, CacheRead: 1600, CacheWrite: 160}); got != want || !near(cost, 0.16) {
		t.Errorf("accumulated = %+v $%.6f, want %+v $0.16 — folding must SUM", got, cost, want)
	}
	var after string
	if err := l.db.QueryRow(`SELECT ended_at FROM turns WHERE turn_id = ?`, sessTurn("T1")).Scan(&after); err != nil || after != ended {
		t.Errorf("spawning turn's end moved %s -> %s (%v): a share must not rewrite its turn", ended, after, err)
	}

	// The folded call is creditable.
	l.ApplyLegacyCorrections([]modelinfo.CostCorrection{{
		BilledAt: time.Date(2026, 9, 13, 14, 41, 40, 0, time.UTC), SubagentTurnID: sessTurn("T1"),
		AgentID: "agent-1", Model: "claude-opus-5", Counts: moveAll, CostUSD: 0.30,
	}})
	_, parent := readLegacy(t, l, sessTurn("T1"), false)
	_, sub := readLegacy(t, l, sessTurn("T1"), true)
	if !near(parent, 0.70) || !near(sub, 0.46) {
		t.Errorf("parent $%.6f sub $%.6f, want $0.70 and $0.46", parent, sub)
	}
}

// TestAccumulateLegacySubagentObservesEveryInstalment: api.db folds a
// delegation into one call; api.jsonl and the trace exporter are append-only,
// so each instalment reaches them on its own, marked, and their sums agree
// with the ledger.
func TestAccumulateLegacySubagentObservesEveryInstalment(t *testing.T) {
	l, _ := openLedger(t)
	path := filepath.Join(t.TempDir(), "api.jsonl")
	f := openJSONL(t, path)
	var seen []Booking
	BookedHook = func(b Booking) { seen = append(seen, b) }
	t.Cleanup(func() { BookedHook = nil })

	for i := 0; i < 3; i++ {
		c := modelinfo.TokenCounts{Input: 5, Output: 5, CacheRead: 50, CacheWrite: 5}
		cost := 0.05
		if _, err := l.AccumulateLegacySubagent(LegacyRow{
			At: startT1, Session: "sess", Model: "claude-opus-5", TurnID: sessTurn("T1"),
			Subagent: true, SubagentID: "agent-1", Turn: &c, CalculatedCostUSD: &cost,
		}); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	if len(seen) != 3 || seen[0].Instalment || !seen[1].Instalment || !seen[2].Instalment {
		t.Fatalf("hook saw %d bookings (instalment flags %v %v %v), want the first plain and two instalments",
			len(seen), seen[0].Instalment, seen[1].Instalment, seen[2].Instalment)
	}
	var jsonlCost float64
	lines := ReadJSONL(path)
	for _, r := range lines {
		jsonlCost += r.Cost()
	}
	var dbCost float64
	if err := l.db.QueryRow(`SELECT TOTAL(cost_usd) FROM call_costs`).Scan(&dbCost); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || !near(jsonlCost, dbCost) || !near(dbCost, 0.15) {
		t.Errorf("api.jsonl %d lines $%.6f vs ledger $%.6f; want 3 lines agreeing on $0.15", len(lines), jsonlCost, dbCost)
	}
}

// TestBookLegacyUnnamedSubagent: a share whose usage arrived before anything
// named its subagent is still a subagent's call, never the session's own
// thread (actor ""), and is booked on its own rather than folded.
func TestBookLegacyUnnamedSubagent(t *testing.T) {
	l, _ := openLedger(t)
	for i := 0; i < 2; i++ {
		c := modelinfo.TokenCounts{Output: 10}
		cost := 0.5
		if merged, err := l.AccumulateLegacySubagent(LegacyRow{
			At: startT1, Session: "sess", Model: "claude-opus-5", TurnID: sessTurn("T1"),
			Subagent: true, Turn: &c, CalculatedCostUSD: &cost,
		}); err != nil || merged {
			t.Fatalf("merged=%v err=%v, want a fresh call each time", merged, err)
		}
	}
	var n int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM api_calls WHERE actor = ?`, UnnamedSubagent).Scan(&n); err != nil || n != 2 {
		t.Errorf("unnamed subagent calls = %d (%v), want 2", n, err)
	}
}

// TestBookLegacyStoresTheBackendReport: a CC parent row's provided cost is
// CC's cumulative per-process figure; it is kept as a backend report (R5),
// never as the call's cost.
func TestBookLegacyStoresTheBackendReport(t *testing.T) {
	l, _ := openLedger(t)
	provided, calc := 12.5, 0.25
	c := modelinfo.TokenCounts{Input: 10, Output: 20}
	if err := l.BookLegacy(LegacyRow{
		At: startT1, Backend: BackendCCStream, Session: "sess", Model: "claude-opus-5", TurnID: sessTurn("T1"),
		Turn: &c, ProvidedCostUSD: &provided, CalculatedCostUSD: &calc,
	}); err != nil {
		t.Fatal(err)
	}
	var grain string
	var reported float64
	if err := l.db.QueryRow(`SELECT grain, cost_usd FROM backend_reports WHERE backend = 'ccstream'`).Scan(&grain, &reported); err != nil ||
		grain != GrainCumulative || reported != provided {
		t.Errorf("report = %s $%v (%v), want cumulative $12.5", grain, reported, err)
	}
	var cost float64
	var basis string
	if err := l.db.QueryRow(`SELECT cost_usd, cost_basis FROM call_costs`).Scan(&cost, &basis); err != nil ||
		cost != calc || basis != CostBasisRecorded {
		t.Errorf("call cost = $%v on %s (%v), want the recorded $0.25", cost, basis, err)
	}
}

// openJSONL makes path the api.jsonl for the test.
func openJSONL(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	log.SetAPIWriter(f)
	t.Cleanup(func() { log.SetAPIWriter(nil) })
	return f
}
