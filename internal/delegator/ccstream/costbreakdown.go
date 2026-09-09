package ccstream

import (
	"fmt"
	"strings"
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

	// Cache-write tokens split by TTL, top-level and subagent separately
	// (#1866 phase 1). Reported but NOT yet priced differently: phase 1 exists
	// to make the disagreement legible before anything about the money moves.
	writeTop cacheWriteSplit
	writeSub cacheWriteSplit

	// Every model this turn actually used. More than one means the turn's cost
	// cannot be read off ModelUsage[resultModel], which is keyed by a single
	// model — naming them here is what makes that visible in the log rather
	// than only in a ticket (#1872).
	models []string
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
	) + b.writeSplitSuffix()
}

// writeSplitSuffix names the cache-write TTL mix behind the single
// cache_write figure above, and how much of it a subagent produced.
//
// This is the line that turns "foci and CC disagree by 18.8%" into a readable
// cause: a turn whose writes are mostly 5m is a turn foci over-prices, because
// every write is currently charged at the 1h rate. Printed only when there is a
// mix worth reading — an all-1h turn (the main thread's normal shape) adds
// nothing but noise.
//
// Deliberately reports the OBSERVED split rather than deriving it from
// top-level-vs-subagent. The mapping happens to be clean today; asserting it
// here would rebuild, one layer down, the assumption that caused the bug.
func (b costBreakdown) writeSplitSuffix() string {
	tot := b.writeTop.addSplit(b.writeSub)
	if tot.total() == 0 {
		return ""
	}
	s := fmt.Sprintf(" | cache_write_ttl 5m=%d 1h=%d", tot.Ephemeral5m, tot.Ephemeral1h)
	if tot.Unknown > 0 {
		s += fmt.Sprintf(" unknown=%d", tot.Unknown)
	}
	if b.writeSub.total() > 0 {
		s += fmt.Sprintf(" (subagent %d of %d)", b.writeSub.total(), tot.total())
	}
	if len(b.models) > 1 {
		s += fmt.Sprintf(" | models %s", strings.Join(b.models, ","))
	}
	return s
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
	b.turnUsageAcc.reset()
}
