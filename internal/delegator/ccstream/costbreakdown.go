package ccstream

import (
	"fmt"
	"sort"
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
	model string
	// cycles counts RESULT messages — ask cycles — not API calls. It is 1 for a
	// normal turn and rises only on a steer or a pre-answer re-dispatch. It was
	// read as an API-call count and reported as broken (#1877 saw cycles=1 on a
	// turn with 16.0M cache-read against a 1M context window); msgs is the
	// counter that answers that question, and both are printed under names that
	// say which is which (#1866 P5).
	cycles int
	msgs   int
	counts modelinfo.TokenCounts

	// Cache-write tokens split by TTL, top-level and subagent separately
	// (#1866 phase 1). Reported but NOT yet priced differently: phase 1 exists
	// to make the disagreement legible before anything about the money moves.
	writeTop cacheWriteSplit
	writeSub cacheWriteSplit

	// turnDur is how long the turn itself ran; pricedDur is the span the
	// ModelUsage delta being priced actually covers, measured from the
	// previous snapshot of the SAME model. They are equal only when nothing
	// outlived the turn.
	//
	// Printed together because either alone is misleading. On 2026-09-10 a
	// 3.5-minute turn was priced over a 34-minute window — a background
	// subagent had outlived its parent by half an hour — and the row was
	// indistinguishable from a genuinely expensive turn (#1880). A reader who
	// can see both can tell those apart at a glance; a reader who cannot will
	// reconstruct the span by hand from turn_lifecycle, which is how most of a
	// session went missing that morning.
	turnDur   time.Duration
	pricedDur time.Duration

	// This turn's cache-write tokens per SUBAGENT, keyed by the Agent tool_use
	// id (#1880 phase C). Named individually because "subagent 344112 of
	// 369913" says a subagent spent it and not WHICH — and with several running
	// concurrently, which one is the question a reader actually has.
	subagents map[string]turnUsage

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
		"ask_cycles=%d msgs=%d in=%d ($%.6f) out=%d ($%.6f) cache_read=%d ($%.6f) cache_write=%d ($%.6f)",
		b.cycles, b.msgs,
		c.Input, price(c.Input, 0, 0, 0),
		c.Output, price(0, c.Output, 0, 0),
		c.CacheRead, price(0, 0, c.CacheRead, 0),
		c.CacheWrite, price(0, 0, 0, c.CacheWrite),
	) + b.spanSuffix() + b.writeSplitSuffix() + b.subagentSuffix()
}

// spanSuffix names the turn's own duration and the window the priced delta
// covers, and flags them when they disagree.
//
// Silent when pricedDur is unknown (the first snapshot for a model has no
// previous one to measure from) — a zero would read as "instantaneous" rather
// than "not measured", and inventing a span is exactly the class of guess this
// field exists to remove.
func (b costBreakdown) spanSuffix() string {
	if b.pricedDur <= 0 {
		return ""
	}
	s := fmt.Sprintf(" | turn=%s priced_span=%s", b.turnDur.Round(time.Second), b.pricedDur.Round(time.Second))
	// A priced window materially longer than the turn means spend from outside
	// this turn is being booked to it. Named explicitly rather than left for
	// the reader to divide, because the whole failure mode is that nobody
	// divides.
	if b.turnDur > 0 && b.pricedDur > 2*b.turnDur {
		s += fmt.Sprintf(" (SPAN %.1fx TURN — includes work from outside this turn)", b.pricedDur.Seconds()/b.turnDur.Seconds())
	}
	return s
}

// subagentSuffix names each subagent's cache-write tokens, largest first.
//
// Printed only when more than one subagent contributed: with a single one the
// existing "(subagent N of M)" already says everything, and the id adds noise.
func (b costBreakdown) subagentSuffix() string {
	type ent struct {
		id string
		n  int
	}
	var es []ent
	for id, u := range b.subagents {
		if n := u.Write.total(); n > 0 {
			es = append(es, ent{id, n})
		}
	}
	if len(es) < 2 {
		return ""
	}
	sort.Slice(es, func(i, j int) bool {
		if es[i].n != es[j].n {
			return es[i].n > es[j].n
		}
		return es[i].id < es[j].id // stable for equal counts
	})
	parts := make([]string, 0, len(es))
	for _, e := range es {
		parts = append(parts, fmt.Sprintf("%s=%d", e.id, e.n))
	}
	return " | by_subagent " + strings.Join(parts, " ")
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
	b.turnParentCostUSD = 0
	b.turnParentCalc = modelinfo.TokenCounts{}
	b.turnSubagents = nil
	// beginTurn, NOT reset: the accumulator's totals are cumulative and the
	// window a turn is priced over starts at the PREVIOUS RESULT, before this
	// turn opened. Wiping here is what lost 85-99% of the cache-write tokens
	// (#1880).
	b.turnUsageAcc.beginTurn(b.turnRowID)
}
