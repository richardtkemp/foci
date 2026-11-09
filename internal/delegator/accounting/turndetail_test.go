package accounting

import (
	"testing"
	"time"

	"foci/internal/modelinfo"
)

// TestTurnDetail (#2992): one turn's facts for the app's Message info. The
// thread's own tokens and calls are apart from its subagents'; the cost is the
// views' sum and names an unpriced call rather than pricing it at $0; the
// compaction the turn caused counts, its cost kept apart; the fill is the last
// own call's in-context tokens.
func TestTurnDetail(t *testing.T) {
	l, _ := openLedger(t)
	s, turn := "gil/c1", "gil/c1@1"
	end := t0.Add(time.Minute)
	book := func(c Call) {
		t.Helper()
		c.Backend, c.Session, c.Finality, c.ClassMethod = BackendCCStream, s, FinalityCompleted, ClassMethodObserved
		if c.Kind == "" {
			c.Kind = KindCall
		}
		tt := Turn{TurnID: c.TurnID, Session: s, Backend: BackendCCStream, Source: SourceUser, StartedAt: t0, EndedAt: end,
			StopReason: "end_turn", FinalModel: "claude-opus-5", ActivityClosedAt: end}
		if c.Kind == KindCompaction {
			tt.Source = SourceCompaction
		}
		if _, err := l.RecordCall(tt, c, nil); err != nil {
			t.Fatal(err)
		}
	}
	tok := func(in, read, out int) modelinfo.Tokens {
		return modelinfo.Tokens{modelinfo.ClassInput: in, modelinfo.ClassCacheRead: read, modelinfo.ClassOutput: out}
	}
	book(Call{Key: "m1", TurnID: turn, Model: "claude-opus-5", BilledAt: t0.Add(time.Second), Tokens: tok(10, 1000, 50)})
	book(Call{Key: "m2", TurnID: turn, Model: "claude-opus-5", BilledAt: t0.Add(2 * time.Second), Tokens: tok(20, 2000, 70)})
	book(Call{Key: "s1", TurnID: turn, Actor: "agent-x", Model: "claude-opus-5", BilledAt: t0.Add(3 * time.Second), Tokens: tok(5, 0, 9)})
	book(Call{Key: "u1", TurnID: turn, Model: "no-such-model-anywhere", BilledAt: t0.Add(4 * time.Second), Tokens: tok(1, 0, 1)})
	book(Call{Key: "c1", TurnID: turn + ":compaction", Kind: KindCompaction, Model: "claude-opus-5", BilledAt: t0.Add(5 * time.Second), Tokens: tok(9000, 0, 300)})

	d, err := l.TurnDetail(turn)
	if err != nil || d == nil {
		t.Fatalf("TurnDetail = %v, %v", d, err)
	}
	if d.Calls != 3 || d.SubagentCalls != 1 || d.Compactions != 1 {
		t.Errorf("calls = %d own, %d subagent, %d compactions; want 3, 1, 1", d.Calls, d.SubagentCalls, d.Compactions)
	}
	if got := d.Tokens[modelinfo.ClassOutput]; got != 50+70+1 {
		t.Errorf("own output = %d, want %d", got, 50+70+1)
	}
	if got := d.SubagentTokens[modelinfo.ClassOutput]; got != 9 {
		t.Errorf("subagent output = %d, want 9", got)
	}
	if d.UnpricedCalls != 1 {
		t.Errorf("unpriced = %d, want 1", d.UnpricedCalls)
	}
	var want, sub float64
	for _, tk := range []modelinfo.Tokens{tok(10, 1000, 50), tok(20, 2000, 70), tok(5, 0, 9)} {
		c, _ := modelinfo.CostAsOf("claude-opus-5", t0, tk)
		want += c
	}
	sub, _ = modelinfo.CostAsOf("claude-opus-5", t0, tok(5, 0, 9))
	if !near(d.CostUSD, want) || !near(d.SubagentCostUSD, sub) {
		t.Errorf("cost = $%.6f (sub $%.6f), want $%.6f ($%.6f)", d.CostUSD, d.SubagentCostUSD, want, sub)
	}
	if comp, _ := modelinfo.CostAsOf("claude-opus-5", t0, tok(9000, 0, 300)); !near(d.CompactionCostUSD, comp) {
		t.Errorf("compaction cost = $%.6f, want $%.6f", d.CompactionCostUSD, comp)
	}
	if d.ContextFill != 1 {
		t.Errorf("fill = %d, want 1: the last own call's in-context tokens", d.ContextFill)
	}
	if d.StopReason != "end_turn" || d.Backend != BackendCCStream || !d.StartedAt.Equal(t0) || !d.EndedAt.Equal(end) || d.Running {
		t.Errorf("turn facts = %+v", d)
	}

	if d, err := l.TurnDetail("nobody@1"); err != nil || d != nil {
		t.Errorf("unknown turn = %+v, %v; want nil, nil", d, err)
	}
}

// TestTurnSpawns (#2992): a turn's spawns are the ones RECORDED against it,
// each priced from where its spend was booked — its own session, its own turn
// — or marked as inside the parent turn, or as not attributable.
func TestTurnSpawns(t *testing.T) {
	l, _ := openLedger(t)
	tok := modelinfo.Tokens{modelinfo.ClassInput: 10, modelinfo.ClassOutput: 10}
	book := func(s, turnID string, at time.Time) {
		t.Helper()
		r := APIResponse{ID: turnID, Kind: KindCall, Model: "claude-opus-5", Session: s, TurnID: turnID, Start: at, Tokens: tok}
		tt := Turn{TurnID: turnID, Session: s, Backend: BackendAPI, Source: SourceUser, StartedAt: at}
		if _, err := l.RecordCall(tt, r.Call(), nil); err != nil {
			t.Fatal(err)
		}
	}
	parent := "gil/c1@1"
	book("gil/c1", parent, t0)
	book("gil/c1/b1", "gil/c1/b1@1", t0.Add(time.Second))   // the batch spawn's own session
	book("gil/c1/b1", "gil/c1/b1@2", t0.Add(2*time.Second)) // its later turn counts too
	book("gil/c1", "gil/c1@3:spawn", t0.Add(3*time.Second)) // an API one-shot's own turn
	book("gil/c1/b9", "gil/c1/b9@1", t0.Add(4*time.Second)) // a branch nobody recorded: not listed

	for i, s := range []SpawnLink{
		{SpawnID: "s1", ParentTurnID: parent, ParentSession: "gil/c1", Name: "batch", Mode: "character", StartedAt: t0.Add(time.Second)},
		{SpawnID: "s2", ParentTurnID: parent, ParentSession: "gil/c1", Mode: "explore", StartedAt: t0.Add(3 * time.Second)},
		{SpawnID: "s3", ParentTurnID: parent, ParentSession: "gil/c1", Mode: "raw", StartedAt: t0.Add(5 * time.Second)},
		{SpawnID: "s4", ParentTurnID: parent, ParentSession: "gil/c1", Mode: "raw", StartedAt: t0.Add(6 * time.Second)},
		{SpawnID: "s5", ParentSession: "gil/c1", StartedAt: t0}, // no parent turn: not stored
	} {
		if err := l.RecordSpawn(s); err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
	}
	for _, b := range []struct {
		id, sess, turn string
		in             bool
	}{{"s1", "gil/c1/b1", "", false}, {"s2", "", "gil/c1@3:spawn", false}, {"s3", "", "", true}} {
		if err := l.SetSpawnBooking(b.id, b.sess, b.turn, b.in); err != nil {
			t.Fatal(err)
		}
	}

	got, err := l.TurnSpawns(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("spawns = %+v, want s1..s4", got)
	}
	one, _ := modelinfo.CostAsOf("claude-opus-5", t0, tok)
	if g := got[0]; g.SpawnID != "s1" || !g.Priced || g.ChildSession != "gil/c1/b1" || !near(g.CostUSD, 2*one) {
		t.Errorf("s1 = %+v, want its session's two calls ($%.6f)", g, 2*one)
	}
	if g := got[1]; g.SpawnID != "s2" || !g.Priced || !near(g.CostUSD, one) {
		t.Errorf("s2 = %+v, want its own turn's call", g)
	}
	if g := got[2]; g.SpawnID != "s3" || g.Priced || !g.InParentTurn {
		t.Errorf("s3 = %+v, want in the parent turn", g)
	}
	if g := got[3]; g.SpawnID != "s4" || g.Priced || g.InParentTurn {
		t.Errorf("s4 = %+v, want not attributable", g)
	}
	if got, err := l.TurnSpawns("nobody@1"); err != nil || len(got) != 0 {
		t.Errorf("unknown turn spawns = %v, %v", got, err)
	}
}
