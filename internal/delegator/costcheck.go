package delegator

import (
	"math"
	"sync"
	"time"
)

// Cost-divergence checking, shared by every delegated backend (#1674).
//
// foci prices every delegated turn itself, from the modelinfo table applied to
// real token counts, and that figure is authoritative. Backends still report a
// cost of their own, and this is the only thing that still reads it: if the two
// disagree, our pricing table has gone stale, a model has started resolving to
// the wrong family, or a rate changed upstream.
//
// Nothing else would notice any of those, because our number is the one that
// gets stored. This check is the alarm — it is the mechanism that makes the
// pricing table's maintenance cost visible instead of silent.
//
// Deliberately one implementation across backends rather than one per backend,
// including for opencode whose per-message cost is not cumulative and could
// arguably be trusted: a single rule is what makes a warning mean exactly one
// thing wherever it fires.

const (
	// CostDivergenceTolerance is the fraction by which foci's own priced figure
	// may differ from the backend's before it is worth saying so.
	//
	// Raised 1% -> 3% on 2026-08-06, once the check had done its job. Its first
	// 47 fires were all claude-opus-5 at ~2.2x, and they were RIGHT: the model
	// had no registry entry and fell back to a stale claude-opus-4-6 row priced
	// at the Opus-4.1 rates. With that corrected the residual gap is small and
	// structural — per-turn rounding — not a stale table. (This comment
	// originally also blamed the unmodelled 5m/1h cache-write split; that was
	// true when written and is no longer: the split was the dominant residual,
	// worth 11.4% of a measured session, and modelinfo now prices cache writes
	// at the 1h rate. 3% therefore has MORE headroom than it needs, which is
	// the safe direction.) 1% sat
	// close enough to that noise floor to risk training everyone to ignore the
	// warning, which costs the whole mechanism. 3% still catches the failure it
	// exists for: a wrong-family fallback or an upstream rate change moves the
	// price by tens of percent, never by two.
	CostDivergenceTolerance = 0.03

	// costDivergenceFloorUSD suppresses the check for turns too cheap to carry
	// signal: 1% of a fraction of a cent is rounding, and warning on it would
	// train everyone to ignore the warning.
	costDivergenceFloorUSD = 0.01

	// costWarnInterval rate-limits per model. A stale price or a genuinely new
	// model diverges on EVERY turn, so an unthrottled warning floods the log and
	// buries the first occurrence, which is the informative one.
	costWarnInterval = 10 * time.Minute
)

// CostDivergenceChecker rate-limits the divergence warning per model. The zero
// value is ready to use.
type CostDivergenceChecker struct {
	mu       sync.Mutex
	lastWarn map[string]time.Time
}

// Check compares foci's calculated cost against the backend-provided one and
// calls warnf when they disagree by more than CostDivergenceTolerance.
//
// warnf is passed in rather than a logger so this can sit in the delegator
// package without depending on any backend's logging setup. Callers must not
// hold a lock that warnf could contend on.
func (c *CostDivergenceChecker) Check(model string, calculated, provided float64, warnf func(string, ...any)) {
	// A zero on either side is not a disagreement worth reporting. Calculated
	// is 0 for CC's synthetic/no-op sentinel, which modelinfo prices at 0 by
	// design; an unknown model does NOT land here (it falls back to a family or
	// haiku rate) and is already reported by modelinfo's own unpriced warning.
	// Provided is 0 when the backend simply reported nothing to compare with.
	if calculated <= 0 || provided <= 0 {
		return
	}
	if provided < costDivergenceFloorUSD && calculated < costDivergenceFloorUSD {
		return
	}
	offBy := math.Abs(calculated-provided) / provided
	if offBy <= CostDivergenceTolerance {
		return
	}

	c.mu.Lock()
	if c.lastWarn == nil {
		c.lastWarn = make(map[string]time.Time)
	}
	last, seen := c.lastWarn[model]
	throttled := seen && time.Since(last) < costWarnInterval
	if !throttled {
		c.lastWarn[model] = time.Now()
	}
	c.mu.Unlock()
	if throttled || warnf == nil {
		return
	}

	warnf("cost divergence: foci priced this %s turn at $%.6f but the backend reported $%.6f "+
		"(%.1f%% off, tolerance %.0f%%) — the modelinfo pricing table is likely stale for this model (#1674)",
		model, calculated, provided, 100*offBy, 100*CostDivergenceTolerance)
}
