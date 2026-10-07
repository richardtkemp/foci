package accounting

import (
	"testing"
	"time"

	"foci/internal/modelinfo"
)

// TestSessionStats: one session's totals for /status and /context. Turns
// count the conversation's own turns — not a compaction, nor the system turn
// a summary is booked on; calls count calls (a legacy subagent share is part
// of its parent's); cost sums the priced calls and says how many were not;
// the context is the latest turn's fill, skipping a synthetic turn that left
// none.
func TestSessionStats(t *testing.T) {
	l, _ := openLedger(t)
	useLive(t, l)
	s := "gil/c1"
	book := func(turnID, source string, at time.Time, resp APIResponse) {
		t.Helper()
		resp.Session, resp.TurnID, resp.Start = s, turnID, at
		tt := Turn{TurnID: turnID, Session: s, Backend: BackendAPI, Source: source, StartedAt: at}
		if err := Record(tt, resp.Call()); err != nil {
			t.Fatal(err)
		}
	}
	tok := func(in, read int) modelinfo.Tokens {
		return modelinfo.Tokens{modelinfo.ClassInput: in, modelinfo.ClassCacheRead: read, modelinfo.ClassOutput: 5}
	}
	book("gil/c1@1", SourceUser, t0, APIResponse{ID: "m1", Kind: KindCall, Model: "claude-opus-5", Tokens: tok(100, 5000)})
	book("gil/c1@1", SourceUser, t0.Add(time.Second), APIResponse{ID: "m2", Kind: KindCall, Model: "claude-opus-5", Tokens: tok(50, 6000)})
	book("gil/c1@2", SourceUser, t0.Add(time.Minute), APIResponse{ID: "m3", Kind: KindCall, Model: "<synthetic>"})
	book("gil/c1@3:compaction", SourceCompaction, t0.Add(2*time.Minute),
		APIResponse{ID: "m4", Kind: KindCompaction, Model: "claude-opus-5", Tokens: tok(90000, 0)})
	book("gil/c1@4:summary", SourceSystem, t0.Add(3*time.Minute),
		APIResponse{ID: "m5", Kind: KindSummary, Model: "no-such-model-anywhere", Tokens: tok(10, 0)})

	st, err := l.SessionStats(s)
	if err != nil {
		t.Fatal(err)
	}
	if st.TurnCount != 2 {
		t.Errorf("turns = %d, want 2 (the compaction and the summary's system turn are not the session's turns)", st.TurnCount)
	}
	if st.TotalCalls != 5 || st.UnpricedCalls != 1 {
		t.Errorf("calls = %d (%d unpriced), want 5 (1)", st.TotalCalls, st.UnpricedCalls)
	}
	if st.ContextTokens != 50+6000 {
		t.Errorf("context = %d, want %d: the last call of the latest turn that had a fill", st.ContextTokens, 50+6000)
	}
	if !st.CreatedAt.Equal(t0) || !st.LastActivity.Equal(t0.Add(3*time.Minute)) {
		t.Errorf("span = %v .. %v", st.CreatedAt, st.LastActivity)
	}
	var want float64
	for _, tk := range []modelinfo.Tokens{tok(100, 5000), tok(50, 6000), tok(90000, 0)} {
		c, _ := modelinfo.CostAsOf("claude-opus-5", t0, tk)
		want += c
	}
	if !near(st.TotalCost, want) {
		t.Errorf("cost = $%.6f, want $%.6f (the unpriced call adds nothing)", st.TotalCost, want)
	}

	if id, err := l.LastTurnID(s); err != nil || id != "gil/c1@2" {
		t.Errorf("LastTurnID = %q (%v), want gil/c1@2 — the newest conversation turn", id, err)
	}
	if id, err := l.LastTurnID("nobody/c1"); err != nil || id != "" {
		t.Errorf("LastTurnID(unknown) = %q (%v)", id, err)
	}
	if st, err := l.SessionStats("nobody/c1"); err != nil || st.TotalCalls != 0 || !st.CreatedAt.IsZero() {
		t.Errorf("empty session stats = %+v (%v)", st, err)
	}
}

// TestCallsSplitsCostByClass: each call's class costs are the views' own, so
// on the counts basis they add up to the call's cost; a recorded-basis call
// keeps its counts but no class costs, since its figure was never split.
func TestCallsSplitsCostByClass(t *testing.T) {
	l, _ := openLedger(t)
	tokens := modelinfo.Tokens{modelinfo.ClassInput: 10, modelinfo.ClassOutput: 20,
		modelinfo.ClassCacheRead: 300, modelinfo.ClassCacheWrite5m: 40, modelinfo.ClassWebSearch: 2}
	mustUpdate(t, l, func(tx *Tx) error {
		if err := tx.RecordTurn(turn("T")); err != nil {
			return err
		}
		_, err := tx.Book(call("k1", tokens))
		return err
	})
	calc := 3.5
	if err := l.BookLegacy(LegacyRow{At: t0, Session: "agent/c1", Model: "claude-opus-5", TurnID: "T2",
		Turn: &modelinfo.TokenCounts{Input: 7}, CalculatedCostUSD: &calc}); err != nil {
		t.Fatal(err)
	}
	rows, err := l.Calls(t0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("calls = %d (%v)", len(rows), err)
	}
	var sum float64
	for class, cc := range rows[0].Classes {
		if cc.CostUSD == nil || cc.Count != tokens[class] {
			t.Errorf("class %s = %+v, want count %d priced", class, cc, tokens[class])
			continue
		}
		sum += *cc.CostUSD
	}
	if !near(sum, rows[0].Cost()) {
		t.Errorf("class costs sum to $%.9f, call costs $%.9f", sum, rows[0].Cost())
	}
	legacy := rows[1]
	if legacy.Cost() != 3.5 || legacy.CostBasis != CostBasisRecorded || legacy.Count(modelinfo.ClassInput) != 7 ||
		legacy.Classes[modelinfo.ClassInput].CostUSD != nil {
		t.Errorf("recorded call = %+v", legacy)
	}
	if later, err := l.Calls(t0.Add(time.Second)); err != nil || len(later) != 0 {
		t.Errorf("Calls(after) = %d (%v), want none", len(later), err)
	}
}

// TestCurrentContextFill (#2235): the fill a caller may ACT on. It is the
// latest real turn's fill, but a compaction booked after that fill makes it
// stale (the compaction turn leaves no fill of its own), so it reads as 0 —
// unknown — until the next real turn books a fresh one. SessionStats keeps
// its display behaviour (the last fill seen).
func TestCurrentContextFill(t *testing.T) {
	l, _ := openLedger(t)
	useLive(t, l)
	s := "gil/c9"
	book := func(turnID, source string, at time.Time, resp APIResponse) {
		t.Helper()
		resp.Session, resp.TurnID, resp.Start = s, turnID, at
		tt := Turn{TurnID: turnID, Session: s, Backend: BackendAPI, Source: source, StartedAt: at}
		if err := Record(tt, resp.Call()); err != nil {
			t.Fatal(err)
		}
	}
	tok := func(in, read int) modelinfo.Tokens {
		return modelinfo.Tokens{modelinfo.ClassInput: in, modelinfo.ClassCacheRead: read, modelinfo.ClassOutput: 5}
	}
	fill := func() int {
		t.Helper()
		f, err := l.CurrentContextFill(s)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	if f := fill(); f != 0 {
		t.Fatalf("empty session fill = %d, want 0", f)
	}
	book("gil/c9@1", SourceUser, t0, APIResponse{ID: "n1", Kind: KindCall, Model: "claude-opus-5", Tokens: tok(100, 120000)})
	if f := fill(); f != 100+120000 {
		t.Fatalf("fill after a user turn = %d, want %d", f, 100+120000)
	}
	book("gil/c9@2:compaction", SourceCompaction, t0.Add(time.Minute),
		APIResponse{ID: "n2", Kind: KindCompaction, Model: "claude-opus-5", Tokens: tok(120100, 0)})
	if f := fill(); f != 0 {
		t.Errorf("fill after a compaction = %d, want 0: the latest fill predates the compaction and is stale", f)
	}
	if st, err := l.SessionStats(s); err != nil || st.ContextTokens != 100+120000 {
		t.Errorf("SessionStats context = %+v (%v), want the last fill seen (%d) — display is unchanged", st, err, 100+120000)
	}
	book("gil/c9@3", SourceKeepalive, t0.Add(2*time.Minute), APIResponse{ID: "n3", Kind: KindCall, Model: "claude-opus-5", Tokens: tok(10, 20000)})
	if f := fill(); f != 10+20000 {
		t.Errorf("fill after a post-compaction turn = %d, want %d", f, 10+20000)
	}
}
