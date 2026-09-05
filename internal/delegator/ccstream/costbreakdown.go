package ccstream

import (
	"fmt"
	"time"

	"foci/internal/modelinfo"
)

// costBreakdown renders the per-class token counts and prices behind one turn's
// calculated cost, for the divergence warning (#1695).
//
// It exists because the warning previously reported only two scalars, and the
// question it always provokes — WHICH class disagrees — was then answerable only
// by reconstructing the turn from CC's transcript. api.db cannot answer it: its
// token columns hold the final cycle's context fill, not the deltas that were
// priced, so a single-cycle turn whose row prices at $0.74 can legitimately
// carry a calculated cost of $4.39 and look like a pricing bug.
type costBreakdown struct {
	model  string
	cycles int
	counts modelinfo.TokenCounts
}

// String prices each class through modelinfo.CostAsOf with the other classes
// zeroed. Deliberately the same function that produced the total rather than a
// local rate lookup: a second copy of the rate logic could disagree with the
// figure under investigation, which would make this line actively misleading at
// exactly the moment it is being trusted.
func (b costBreakdown) String() string {
	now := time.Now()
	price := func(in, out, cr, cw int) float64 {
		return modelinfo.CostAsOf(b.model, now, in, out, cr, cw)
	}
	c := b.counts
	return fmt.Sprintf(
		"cycles=%d in=%d ($%.6f) out=%d ($%.6f) cache_read=%d ($%.6f) cache_write=%d ($%.6f)",
		b.cycles,
		c.Input, price(c.Input, 0, 0, 0),
		c.Output, price(0, c.Output, 0, 0),
		c.CacheRead, price(0, 0, c.CacheRead, 0),
		c.CacheWrite, price(0, 0, 0, c.CacheWrite),
	)
}

// resetTurnCostAccumulatorsLocked clears every turn-scoped cost accumulator as
// ONE group. Caller must hold turnMu.
//
// It exists because the cost and the token counts that explain it were reset at
// two call sites and drifted apart: #1848 shipped with turnCalcCostUSD cleared
// at both boundaries and the four counters cleared at neither, so the warning
// printed a per-SESSION breakdown beside a per-TURN total. Splitting the group
// across call sites is what let one half be forgotten, so the group now has one
// name and adding a field to it cannot miss a boundary.
func (b *Backend) resetTurnCostAccumulatorsLocked() {
	b.turnCalcCostUSD = 0
	b.turnProvidedUSD = 0
	b.turnProvidedSeen = false
	b.turnCalc = modelinfo.TokenCounts{}
}
