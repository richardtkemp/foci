package ccstream

import (
	"foci/internal/delegator"
	"math"
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

// TestOnResult_CorrectionCarriesItsStrandedSurcharge (#1929).
//
// Driven through OnResult so it exercises the code that actually POPULATES the
// figure. A first version of this test asserted only modelinfo's rate
// arithmetic; the disconnected-test lint rejected it, correctly — it would have
// passed with handlers.go computing the surcharge wrongly, or not at all.
//
// A late subagent cache write is absorbed by the parent as an UNOBSERVED
// residue, which splitFor classes Unknown and Unknown prices at the 1h rate. The
// correction removes it at the subagent's own observed 5m rate, so the
// difference stays on a row that no longer holds the tokens. Reported, not
// repaired — see CostCorrection.StrandedUSD.
func TestOnResult_CorrectionCarriesItsStrandedSurcharge(t *testing.T) {
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
	if math.Abs(c.StrandedUSD-3.75) > 1e-9 {
		t.Errorf("stranded = $%.6f, want $3.75 per MTok — the parent absorbed these "+
			"writes in the Unknown class at the 1h rate while the correction removes "+
			"them at the subagent's observed 5m rate (#1929)", c.StrandedUSD)
	}
	if math.Abs(c.CostUSD-6.25) > 1e-9 {
		t.Errorf("correction cost = $%.6f, want $6.25 — it moves the tokens at the "+
			"subagent's OWN observed split", c.CostUSD)
	}
}

// TestOnResult_NoSurchargeWhenTheSubagentWroteAt1h: the figure is priced BOTH
// ways rather than derived by subtracting rate constants, so a subagent that
// genuinely wrote at 1h strands nothing — the parent absorbed it at that same
// rate. Without this, a hard-coded (1h − 5m) difference would invent a surcharge
// on every 1h subagent.
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
	if s := got.Usage.Corrections[0].StrandedUSD; math.Abs(s) > 1e-9 {
		t.Errorf("stranded = $%.6f, want $0 — Unknown and 1h price identically, so a "+
			"subagent that wrote at 1h leaves nothing behind", s)
	}
}
