package ccstream

import (
	"testing"

	"foci/internal/delegator"
	"foci/internal/modelinfo"
	"time"
)

// The #1848 regression. turnCalcCostUSD was cleared at each turn boundary but
// the four token counters that EXPLAIN it were not, so they accumulated for the
// life of the CC process while the cost they annotate reset every turn. The
// divergence warning then printed a per-session breakdown beside a per-turn
// total, which is worse than printing nothing: it reads as a diagnosis.
//
// Observed 2026-09-03 in one session, five consecutive warnings — in, out and
// cache_write climbing monotonically (826 -> 2469 -> 2577 -> 2786 -> 2801 output
// tokens) while the totals swung by 60x. At 18:22:01 the printed classes summed
// to $1.669364 beside a stated turn total of $0.052267.

// TestBeginTurn_ResetsEveryTurnScopedCostAccumulator is the direct arm: the
// counters must be cleared by the same boundary that clears the cost.
func TestBeginTurn_ResetsEveryTurnScopedCostAccumulator(t *testing.T) {
	t.Parallel()
	b := &Backend{}

	// Residue from a completed turn, using the real 18:22:01 figures.
	b.turnCalcCostUSD = 0.052267
	b.turnProvidedUSD = 0.054929
	b.turnProvidedSeen = true
	b.turnCalcInput = 12
	b.turnCalcOutput = 2786
	b.turnCalcCacheRead = 144976
	b.turnCalcCacheWrite = 74685

	b.beginTurnLocked(&delegator.TurnEvents{})

	for _, c := range []struct {
		name string
		got  int
	}{
		{"turnCalcInput", b.turnCalcInput},
		{"turnCalcOutput", b.turnCalcOutput},
		{"turnCalcCacheRead", b.turnCalcCacheRead},
		{"turnCalcCacheWrite", b.turnCalcCacheWrite},
	} {
		if c.got != 0 {
			t.Errorf("%s = %d after beginTurnLocked, want 0 — it carried into the next turn", c.name, c.got)
		}
	}
	if b.turnCalcCostUSD != 0 {
		t.Errorf("turnCalcCostUSD = %v, want 0", b.turnCalcCostUSD)
	}
	if b.turnProvidedUSD != 0 {
		t.Errorf("turnProvidedUSD = %v, want 0", b.turnProvidedUSD)
	}
	if b.turnProvidedSeen {
		t.Error("turnProvidedSeen = true, want false")
	}
}

// TestBreakdownClassesSumToTheTurnTotal is the invariant arm, and the one that
// states WHY the reset matters. The warning shows a total and four classes; if
// those classes cannot be re-priced back to that total the line is misleading at
// exactly the moment it is trusted. Two turns are run so the residue from turn 1
// has somewhere to leak to.
func TestBreakdownClassesSumToTheTurnTotal(t *testing.T) {
	t.Parallel()
	const model = "claude/claude-fable-5-1"
	priceAt := time.Now()
	b := &Backend{}

	accumulate := func(in, out, cr, cw int) {
		b.turnCalcCostUSD += modelinfo.CostAsOf(model, priceAt, in, out, cr, cw)
		b.turnCalcInput += in
		b.turnCalcOutput += out
		b.turnCalcCacheRead += cr
		b.turnCalcCacheWrite += cw
	}

	b.beginTurnLocked(&delegator.TurnEvents{})
	accumulate(6, 826, 80470, 41243) // turn 1
	b.beginTurnLocked(&delegator.TurnEvents{})
	accumulate(2, 15, 31292, 271) // turn 2 — small, so leaked residue is obvious

	bd := costBreakdown{
		model:      model,
		cycles:     1,
		input:      b.turnCalcInput,
		output:     b.turnCalcOutput,
		cacheRead:  b.turnCalcCacheRead,
		cacheWrite: b.turnCalcCacheWrite,
	}
	classes := modelinfo.CostAsOf(model, priceAt, bd.input, bd.output, bd.cacheRead, bd.cacheWrite)

	if diff := classes - b.turnCalcCostUSD; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("breakdown classes price to $%.6f but the turn total is $%.6f (diff $%.6f)\n"+
			"the printed breakdown does not explain the printed total",
			classes, b.turnCalcCostUSD, diff)
	}
}
