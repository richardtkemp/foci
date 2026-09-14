package ccstream

import (
	"testing"
	"time"
)

func at(mins, secs int) time.Time {
	return time.Date(2026, 9, 13, 14, mins, secs, 0, time.UTC)
}

// TestPendingCorrection_NamesBothTurns: late spend must name the turn that
// ABSORBED it (whose ModelUsage included it) and the turn that SPAWNED the
// agent (under which phase C files its rows). They differ whenever a background
// subagent outlives its parent, which is why both are carried.
func TestPendingCorrection_NamesBothTurns(t *testing.T) {
	t.Parallel()

	a := &usageAccumulator{}
	// T1 spawns the agent and sees a little of its spend.
	a.markResult(at(0, 0))
	a.beginTurn("T1")
	a.note("claude-opus-5", "agent-1", true, "m1", at(0, 30), usage(0, 0, 100, 0, 0, 0))
	a.markResult(at(1, 0))
	// T2 runs and closes.
	a.beginTurn("T2")
	a.note("claude-opus-5", "agent-1", true, "m2", at(1, 30), usage(0, 0, 200, 0, 0, 0))
	a.markResult(at(2, 0))
	// T3 opens, and the tail finally delivers spend BILLED during T2.
	a.beginTurn("T3")
	a.note("claude-opus-5", "agent-1", true, "m3", at(1, 45), usage(0, 0, 5000, 0, 0, 0))

	got := a.drainCorrections()
	if len(got) != 1 {
		t.Fatalf("corrections = %d, want 1: %+v", len(got), got)
	}
	for k, u := range got {
		if k.Bill != "T2" {
			t.Errorf("Bill = %q, want T2 — the turn whose ModelUsage already "+
				"included the spend, not the turn it was delivered in", k.Bill)
		}
		if k.Spawn != "T1" {
			t.Errorf("Spawn = %q, want T1 — phase C files every subagent row "+
				"under the spawning turn", k.Spawn)
		}
		if u.CacheRead != 5000 {
			t.Errorf("amount = %d, want 5000", u.CacheRead)
		}
	}
	if len(a.drainCorrections()) != 0 {
		t.Error("drain did not clear — a correction applied twice moves the spend twice")
	}
}

// TestPendingCorrection_NoneWhenTheWindowHasAgedOut: if the billing window is no
// longer in the ring we cannot name the row that absorbed the spend, and a
// correction that credits the subagent without debiting anyone INFLATES the
// session total. Losing the attribution is the safe failure.
func TestPendingCorrection_NoneWhenTheWindowHasAgedOut(t *testing.T) {
	t.Parallel()

	a := &usageAccumulator{}
	a.markResult(at(0, 0))
	a.beginTurn("T1")
	a.note("claude-opus-5", "agent-1", true, "m1", at(0, 30), usage(0, 0, 100, 0, 0, 0))
	a.markResult(at(1, 0))
	for i := 0; i < maxTurnWindows+4; i++ {
		a.beginTurn("T-filler")
		a.markResult(at(2+i, 0))
	}
	a.beginTurn("T-last")
	a.drainCorrections() // clear anything the fillers produced

	// Billed back in T1's window, long since evicted.
	a.note("claude-opus-5", "agent-1", true, "m-late", at(0, 45), usage(0, 0, 9999, 0, 0, 0))

	if got := a.drainCorrections(); len(got) != 0 {
		t.Errorf("corrections = %+v, want none — the billing window aged out, so "+
			"there is no row to debit and crediting alone inflates the total", got)
	}
}

// TestPendingCorrection_NotRaisedForOnTimeUsage: usage billed INSIDE the current
// window is attributed correctly already. Emitting a correction for it would
// move spend that was never misfiled.
func TestPendingCorrection_NotRaisedForOnTimeUsage(t *testing.T) {
	t.Parallel()

	a := &usageAccumulator{}
	a.markResult(at(0, 0))
	a.beginTurn("T1")
	a.note("claude-opus-5", "agent-1", true, "m1", at(0, 30), usage(0, 0, 100, 0, 0, 0))

	if got := a.drainCorrections(); len(got) != 0 {
		t.Errorf("corrections = %+v, want none for on-time usage", got)
	}
}

// TestTurnWindow_OneWindowPerTurnNotPerCycle: rows are written per TURN, so a
// correction can only name a turn. A multi-cycle turn must therefore extend its
// single window rather than opening a second one, or late spend billed in
// cycle 1 would resolve to a window with no row behind it.
func TestTurnWindow_OneWindowPerTurnNotPerCycle(t *testing.T) {
	t.Parallel()

	a := &usageAccumulator{}
	a.markResult(at(0, 0))
	a.beginTurn("T1")
	a.markResult(at(1, 0)) // cycle 1
	a.markResult(at(2, 0)) // cycle 2

	if n := len(a.windows); n != 1 {
		t.Fatalf("windows = %d, want 1 — a turn writes one row set", n)
	}
	if got, ok := a.turnAt(at(0, 30)); !ok || got != "T1" {
		t.Errorf("turnAt(cycle 1) = %q/%v, want T1/true", got, ok)
	}
	if got, ok := a.turnAt(at(1, 30)); !ok || got != "T1" {
		t.Errorf("turnAt(cycle 2) = %q/%v, want T1/true — the window must have "+
			"extended to the turn's last result", got, ok)
	}
}
