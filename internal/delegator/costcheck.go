package delegator

import (
	"fmt"
	"math"
)

// Cost-divergence checking for the delegated backends that report a cost (#1674).
//
// foci prices every delegated turn itself, from the modelinfo table applied to
// real token counts, and that figure is authoritative. Backends still report a
// cost of their own. If the two disagree, our pricing table has gone stale, a
// model has started resolving to the wrong family, or a rate changed upstream.
//
// Nothing else would notice any of those, because the cost views report our
// number. This check is the alarm — it is the mechanism that makes the pricing
// table's maintenance cost visible instead of silent.
//
// The alarm is not the only reader of the backend's figure (#2111). Each report
// is stored verbatim in backend_reports. In ccstream, ccbook.go also solves a
// remainder's 5m/1h cache-write split from CC's cost (splitRemainderTTL, the one
// reader that moves foci's own price), records it on a refusal (cc_cost_usd) and
// bounds overhead against it (checkOverhead), and expectations.go alarms if CC's
// cumulative cost falls.
//
// One tolerance and one alarm (invCostDivergence) across backends, so a warning
// means one thing wherever it fires. The comparison differs by what the backend
// reports: CostDivergence below is per call, for opencode's per-message cost;
// ccBook.checkDivergence is per model and per process, for CC's cumulative cost.
// codex reports no cost, so it has no check.

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
)

// CostDivergence compares foci's calculated cost of one call against the
// backend-provided one, and returns the report to raise and true when they
// disagree by more than CostDivergenceTolerance. The caller raises it as a
// ledger alarm (invCostDivergence), whose expectation guard rate-limits it:
// a stale price diverges on EVERY call.
//
// details supplies the per-class token/price breakdown to print WHEN IT
// FIRES. It is a closure, not a string, so the cost of building it is paid
// only on the rare firing path rather than on every call. nil is allowed and
// prints nothing extra.
//
// The breakdown is the whole diagnostic value of this report. Without it the
// line reports two scalars, and identifying WHICH token class disagrees then
// requires reconstructing the turn from CC's transcript against a database that
// stores the final cycle's context fill rather than the tokens actually priced
// — an exercise that on 2026-08-31 produced two confident wrong answers before
// a correct one, because turn boundaries had to be inferred. Everything needed
// is already in the caller's hand at this moment; it was simply discarded.
func CostDivergence(model string, calculated, provided float64, details func() string) (string, bool) {
	// A zero on either side is not a disagreement worth reporting. Calculated
	// is 0 for CC's synthetic/no-op sentinel, which modelinfo prices at 0 by
	// design; an unknown model does NOT land here (it falls back to a family or
	// haiku rate) and is already reported by modelinfo's own unpriced warning.
	// Provided is 0 when the backend simply reported nothing to compare with.
	if calculated <= 0 || provided <= 0 {
		return "", false
	}
	if provided < costDivergenceFloorUSD && calculated < costDivergenceFloorUSD {
		return "", false
	}
	offBy := math.Abs(calculated-provided) / provided
	if offBy <= CostDivergenceTolerance {
		return "", false
	}

	extra := ""
	if details != nil {
		if d := details(); d != "" {
			extra = " | " + d
		}
	}
	// "likely stale table" is deliberately NOT asserted any more. Measured over
	// 1,827 opus-5 turns on 2026-08-31, ~95% priced correctly — a stale rate
	// cannot do that, so the old wording sent every reader down the wrong path
	// first. State the disagreement; let the breakdown name the cause.
	return fmt.Sprintf("foci priced this %s call at $%.6f but the backend reported $%.6f "+
		"(%.1f%% off, tolerance %.0f%%)%s",
		model, calculated, provided, 100*offBy, 100*CostDivergenceTolerance, extra), true
}
