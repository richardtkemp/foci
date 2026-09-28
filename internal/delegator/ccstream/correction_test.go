package ccstream

import (
	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/modelinfo"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func at(mins, secs int) time.Time {
	return time.Date(2026, 9, 13, 14, mins, secs, 0, time.UTC)
}

// TestPendingCorrection_CarriesBillingTimeAndSpawnTurn.
//
// A correction records WHEN the spend was billed and WHICH turn spawned the
// agent. It deliberately does NOT resolve the turn that absorbed it: api_calls
// already holds every turn's id beside its timestamp, so that lookup belongs at
// apply time against a durable, unbounded, indexed store — not against an
// in-memory ring with a retention bound to get wrong.
func TestPendingCorrection_CarriesBillingTimeAndSpawnTurn(t *testing.T) {
	t.Parallel()

	a := &usageAccumulator{}
	a.markResult(at(0, 0))
	a.beginTurn("T1")
	a.note("claude-opus-5", "agent-1", true, "m1", at(0, 30), true, usage(0, 0, 100, 0, 0, 0))
	a.markResult(at(1, 0))
	a.beginTurn("T2")
	a.markResult(at(2, 0))
	a.beginTurn("T3")
	// The tail finally delivers spend billed back during T2.
	a.note("claude-opus-5", "agent-1", true, "m3", at(1, 45), true, usage(0, 0, 5000, 0, 0, 0))

	got := a.drainCorrections()
	if len(got) != 1 {
		t.Fatalf("corrections = %d, want 1: %+v", len(got), got)
	}
	for k, u := range got {
		if !k.BilledAt.Equal(at(1, 45)) {
			t.Errorf("BilledAt = %v, want %v — the correction must carry when the "+
				"spend was BILLED, not when it was delivered", k.BilledAt, at(1, 45))
		}
		if k.Spawn != "T1" {
			t.Errorf("Spawn = %q, want T1 — phase C files every subagent row under "+
				"the spawning turn", k.Spawn)
		}
		if u.CacheRead != 5000 {
			t.Errorf("amount = %d, want 5000", u.CacheRead)
		}
	}
	if len(a.drainCorrections()) != 0 {
		t.Error("drain did not clear — a correction applied twice moves the spend twice")
	}
}

// TestPendingCorrection_SurvivesArbitrarilyOldSpend is the regression for what
// the ring got wrong.
//
// The first implementation resolved the absorbing turn against a 16-entry ring
// of turn windows and DROPPED any correction whose window had aged out.
// Measured over 26,836 live turns, 2.4% of 30-minute windows hold more than 16
// turns and the worst holds 155 — so that bound silently discarded corrections
// in one window in forty. Nothing may age out now.
func TestPendingCorrection_SurvivesArbitrarilyOldSpend(t *testing.T) {
	t.Parallel()

	a := &usageAccumulator{}
	a.markResult(at(0, 0))
	a.beginTurn("T1")
	a.note("claude-opus-5", "agent-1", true, "m1", at(0, 30), true, usage(0, 0, 100, 0, 0, 0))
	a.markResult(at(1, 0))
	for i := 0; i < 200; i++ {
		a.beginTurn("T-filler")
		a.markResult(at(2+i, 0))
	}
	a.beginTurn("T-last")
	a.drainCorrections()

	// Billed 200 turns ago.
	a.note("claude-opus-5", "agent-1", true, "m-late", at(0, 45), true, usage(0, 0, 9999, 0, 0, 0))

	got := a.drainCorrections()
	if len(got) != 1 {
		t.Fatalf("corrections = %d, want 1 — spend billed 200 turns ago is still "+
			"attributable, because api_calls retains every turn", len(got))
	}
	for k := range got {
		if !k.BilledAt.Equal(at(0, 45)) {
			t.Errorf("BilledAt = %v, want %v", k.BilledAt, at(0, 45))
		}
	}
}

// TestPendingCorrection_NotRaisedForOnTimeUsage: usage billed INSIDE the current
// window is attributed correctly already. Correcting it would move spend that
// was never misfiled.
func TestPendingCorrection_NotRaisedForOnTimeUsage(t *testing.T) {
	t.Parallel()

	a := &usageAccumulator{}
	a.markResult(at(0, 0))
	a.beginTurn("T1")
	a.note("claude-opus-5", "agent-1", true, "m1", at(0, 30), true, usage(0, 0, 100, 0, 0, 0))

	if got := a.drainCorrections(); len(got) != 0 {
		t.Errorf("corrections = %+v, want none for on-time usage", got)
	}
}

// TestPendingCorrection_CoalescesWithinOneSecond: a catch-up burst delivers many
// messages billed within the same second. They resolve to the same turn, so one
// correction is enough and one UPDATE per message is waste.
func TestPendingCorrection_CoalescesWithinOneSecond(t *testing.T) {
	t.Parallel()

	a := &usageAccumulator{}
	a.markResult(at(1, 0))
	a.beginTurn("T2")
	base := time.Date(2026, 9, 13, 14, 0, 30, 0, time.UTC)
	for i := 0; i < 5; i++ {
		a.note("claude-opus-5", "agent-1", true, "m"+string(rune('a'+i)),
			base.Add(time.Duration(i*100)*time.Millisecond), true, usage(0, 0, 100, 0, 0, 0))
	}
	// agentTurn is set on first sight, which happens above.
	got := a.drainCorrections()
	if len(got) != 1 {
		t.Fatalf("corrections = %d, want 1 — five messages in one second resolve to "+
			"one turn and should coalesce", len(got))
	}
	for _, u := range got {
		if u.CacheRead != 500 {
			t.Errorf("coalesced amount = %d, want 500", u.CacheRead)
		}
	}
}

// TestOnResult_CorrectionPricesEachSideAtItsOwnBasis (#1929).
//
// Driven through OnResult so it exercises the code that actually POPULATES the
// figures. A first version of this test asserted only modelinfo's rate
// arithmetic; the disconnected-test lint rejected it, correctly — it would have
// passed with handlers.go computing the figures wrongly, or not at all.
//
// A late subagent cache write is absorbed by the parent as an UNOBSERVED
// residue, which splitFor classes Unknown and Unknown prices at the 1h rate. The
// subagent observed it as 5m. So the parent must give up the 1h figure and the
// subagent gain the 5m one; debiting the parent at 5m (the pre-#1929 behaviour)
// stranded the difference on a row that no longer held the tokens.
func TestOnResult_CorrectionPricesEachSideAtItsOwnBasis(t *testing.T) {
	t.Parallel()

	const lateWrite = 1_000_000 // observed 5m
	b := &Backend{}
	b.beginTurn(&delegator.TurnEvents{TurnID: "T1"})
	// Close a window so there is a lastResultAt for "late" to be measured against.
	onResultWith(b, map[string]ModelUsage{
		"claude-opus-5": {InputTokens: 1, OutputTokens: 1, CacheCreationInputTokens: 1, CostUSD: 0.01},
	})
	b.beginTurn(&delegator.TurnEvents{TurnID: "T2"})

	// A subagent message COMPLETED before this window opened: late, so it does
	// not count here and raises a correction against the turn that absorbed it.
	b.noteSubagentTranscriptUsage("agent-x", "claude-opus-5", "msg_late",
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), true,
		usage(0, 0, 0, lateWrite, lateWrite, 0))

	got := onResultWith(b, map[string]ModelUsage{
		"claude-opus-5": {InputTokens: 1, OutputTokens: 1, CacheCreationInputTokens: 1, CostUSD: 0.01},
	})
	if got == nil || got.Usage == nil {
		t.Fatal("no usage")
	}
	if len(got.Usage.Corrections) != 1 {
		t.Fatalf("corrections = %d, want 1 — late spend must raise one", len(got.Usage.Corrections))
	}
	c := got.Usage.Corrections[0]
	if c.Counts.CacheWrite != lateWrite {
		t.Fatalf("correction cache-write = %d, want %d", c.Counts.CacheWrite, lateWrite)
	}
	// opus-5: 5m $6.25/MTok, Unknown prices with 1h at $10/MTok.
	if math.Abs(c.TTLSurchargeUSD-3.75) > 1e-9 {
		t.Errorf("surcharge = $%.6f, want $3.75 — the parent absorbed these writes in "+
			"the Unknown class at the 1h rate ($10), so it gives up $3.75 more than "+
			"the subagent's 5m credit (#1929)", c.TTLSurchargeUSD)
	}
	if math.Abs(c.CostUSD-6.25) > 1e-9 {
		t.Errorf("correction cost = $%.6f, want $6.25 — it moves the tokens at the "+
			"subagent's OWN observed split", c.CostUSD)
	}
}

// TestOnResult_NoSurchargeWhenTheSubagentWroteAt1h: the surcharge counts only
// 5m writes, so a subagent that genuinely wrote at 1h is debited and credited
// the same — the parent absorbed it at that same rate. Without this, a hard-coded (1h − 5m) difference would
// invent a surcharge on every 1h subagent and drop real money from the turn.
func TestOnResult_NoSurchargeWhenTheSubagentWroteAt1h(t *testing.T) {
	t.Parallel()

	const lateWrite = 1_000_000
	b := &Backend{}
	b.beginTurn(&delegator.TurnEvents{TurnID: "T1"})
	onResultWith(b, map[string]ModelUsage{
		"claude-opus-5": {InputTokens: 1, OutputTokens: 1, CacheCreationInputTokens: 1, CostUSD: 0.01},
	})
	b.beginTurn(&delegator.TurnEvents{TurnID: "T2"})

	// Same message, but its writes were observed as 1h.
	b.noteSubagentTranscriptUsage("agent-x", "claude-opus-5", "msg_late",
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), true,
		usage(0, 0, 0, lateWrite, 0, lateWrite))

	got := onResultWith(b, map[string]ModelUsage{
		"claude-opus-5": {InputTokens: 1, OutputTokens: 1, CacheCreationInputTokens: 1, CostUSD: 0.01},
	})
	if got == nil || got.Usage == nil || len(got.Usage.Corrections) != 1 {
		t.Fatalf("want exactly one correction, got %v", got.Usage.Corrections)
	}
	if s := got.Usage.Corrections[0].TTLSurchargeUSD; s != 0 {
		t.Errorf("surcharge = $%.6f, want exactly $0 — Unknown and 1h price identically, "+
			"so a subagent that wrote at 1h was not over-charged", s)
	}
}

// TestOnResult_CorrectedParentRowRePricesToItsOwnCost is #1929 D1, driven end
// to end: OnResult prices the turn that absorbed a late subagent cache write,
// the rows are written the way turn_delegated.go writes them, the next result
// raises the correction, and ApplyCostCorrections moves it.
//
// The parent absorbed the late writes as an UNOBSERVED residue — Unknown class,
// 1h rate. The subagent observed them as 5m. The #1854 identity says every row's
// stored counts must re-price to its stored cost; after the correction the
// parent holds none of those writes, so its cost must hold none of their price
// either. Debiting the parent at the subagent's 5m basis left
// W*(rate_1h - rate_5m) behind on a row with no tokens to justify it: $0.375 at
// W=100,000 on opus-5 ($10 vs $6.25 per MTok). The rates differ, so there is no
// W for which the old and new parent figures coincide.
//
// Not parallel: api.db is process-global in package log.
func TestOnResult_CorrectedParentRowRePricesToItsOwnCost(t *testing.T) {
	if err := log.InitAPIDB(filepath.Join(t.TempDir(), "api.db")); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	t.Cleanup(log.CloseAPIDB)

	const (
		model     = "claude-opus-5"
		lateWrite = 100_000 // observed 5m by the subagent
		spawn     = "sess@1"
	)
	b := &Backend{}
	// Close a window so there is a lastResultAt for "late" to be measured against.
	b.beginTurn(&delegator.TurnEvents{TurnID: "sess@0"})
	cum := ModelUsage{InputTokens: 1, OutputTokens: 1, CostUSD: 0.01}
	onResultWith(b, map[string]ModelUsage{model: cum})

	b.beginTurn(&delegator.TurnEvents{TurnID: spawn})
	t1Start := time.Now()
	// On time, so the agent has the subagent row the correction credits (a
	// late FIRST message is D2, not this).
	b.noteSubagentTranscriptUsage("agent-x", model, "msg_on_time", time.Now(), true,
		usage(0, 50, 0, 1000, 1000, 0))
	// Billed inside T1, so T1's ModelUsage (cumulative) includes it; delivered
	// only after T1's result.
	billed := time.Now()
	cum.InputTokens += 10
	cum.OutputTokens += 100 + 50
	cum.CacheCreationInputTokens += 1000 + lateWrite
	r1 := onResultWith(b, map[string]ModelUsage{model: cum})
	if r1 == nil || r1.Usage == nil || r1.Usage.CalculatedCostUSD == nil || r1.Usage.Turn == nil ||
		len(r1.Usage.Subagents) != 1 {
		t.Fatalf("T1 result lacks a priced parent and one subagent share: %+v", r1)
	}
	priceModel := r1.Usage.Subagents[0].Model

	// The rows turn_delegated.go writes for T1.
	log.API(log.APIEntry{
		Timestamp: t1Start, Session: "sess", Model: priceModel, CallType: "delegated_turn",
		TurnID: spawn, DurationMS: 60_000, Output: r1.Usage.Turn.Output,
		Turn: r1.Usage.Turn, CalculatedCostUSD: r1.Usage.CalculatedCostUSD,
	})
	for _, sc := range r1.Usage.Subagents {
		counts, cost := sc.Counts, sc.CostUSD
		log.AccumulateSubagentRow(log.APIEntry{
			Timestamp: t1Start, Session: "sess", Model: sc.Model, CallType: "subagent_turn",
			TurnID: sc.TurnID, SubagentID: sc.AgentID, Output: counts.Output,
			Turn: &counts, CalculatedCostUSD: &cost, DurationMS: 60_000,
		})
	}
	before := *r1.Usage.CalculatedCostUSD + r1.Usage.Subagents[0].CostUSD

	b.beginTurn(&delegator.TurnEvents{TurnID: "sess@2"})
	b.noteSubagentTranscriptUsage("agent-x", model, "msg_late", billed, true,
		usage(0, 0, 0, lateWrite, lateWrite, 0))
	r2 := onResultWith(b, map[string]ModelUsage{model: cum})
	if r2 == nil || r2.Usage == nil || len(r2.Usage.Corrections) != 1 {
		t.Fatalf("want exactly one correction from the late write, got %+v", r2)
	}
	log.ApplyCostCorrections(r2.Usage.Corrections)

	var parent, sub *log.APIEntry
	for _, e := range log.ReadAPIDBLog() {
		switch {
		case e.CallType == "delegated_turn" && e.TurnID == spawn:
			parent = &e
		case e.CallType == "subagent_turn" && e.TurnID == spawn:
			sub = &e
		}
	}
	if parent == nil || sub == nil || parent.Turn == nil || sub.Turn == nil ||
		parent.CalculatedCostUSD == nil || sub.CalculatedCostUSD == nil {
		t.Fatalf("rows missing after correction: parent=%+v sub=%+v", parent, sub)
	}
	if parent.Turn.CacheWrite != 0 {
		t.Fatalf("parent cache-write = %d, want 0 — the correction must move every late write",
			parent.Turn.CacheWrite)
	}
	now := time.Now()
	p := parent.Turn
	wantParent := modelinfo.CostAsOfSplit(priceModel, now, p.Input, p.Output, p.CacheRead,
		modelinfo.CacheWrites{})
	if d := *parent.CalculatedCostUSD - wantParent; math.Abs(d) > 1e-9 {
		t.Errorf("parent row = $%.6f, but its stored counts re-price to $%.6f: $%.6f of "+
			"cache-write surcharge stranded on a row that no longer holds the writes (#1929 D1, #1854)",
			*parent.CalculatedCostUSD, wantParent, d)
	}
	s := sub.Turn
	wantSub := modelinfo.CostAsOfSplit(priceModel, now, s.Input, s.Output, s.CacheRead,
		modelinfo.CacheWrites{Ephemeral5m: s.CacheWrite})
	if d := *sub.CalculatedCostUSD - wantSub; math.Abs(d) > 1e-9 {
		t.Errorf("subagent row = $%.6f, want $%.6f — every write it holds was observed 5m",
			*sub.CalculatedCostUSD, wantSub)
	}
	// The turn total FALLS by exactly the over-charge, priced both ways rather
	// than from rate constants: those were 5m writes billed at 1h.
	surcharge := modelinfo.CostAsOfSplit(priceModel, now, 0, 0, 0, modelinfo.CacheWrites{Unknown: lateWrite}) -
		modelinfo.CostAsOfSplit(priceModel, now, 0, 0, 0, modelinfo.CacheWrites{Ephemeral5m: lateWrite})
	if surcharge <= 0 {
		t.Fatalf("surcharge = $%.6f — the fixture needs a model whose 1h and 5m rates differ", surcharge)
	}
	after := *parent.CalculatedCostUSD + *sub.CalculatedCostUSD
	if d := (before - after) - surcharge; math.Abs(d) > 1e-9 {
		t.Errorf("turn total fell by $%.6f, want $%.6f (the 1h-vs-5m surcharge on %d late writes)",
			before-after, surcharge, lateWrite)
	}
}
