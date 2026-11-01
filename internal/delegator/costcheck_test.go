package delegator

import (
	"strings"
	"testing"
)

// TestCostDivergence_ReportsBeyondTolerance: the whole point of still reading
// the backend's cost. Our figure is authoritative, so nothing else would notice
// a stale pricing table — this report is the only alarm.
func TestCostDivergence_ReportsBeyondTolerance(t *testing.T) {
	t.Parallel()
	msg, off := CostDivergence("claude/claude-opus-5", 1.00, 1.50, nil)
	if !off {
		t.Fatal("a 50% divergence must be reported")
	}
	for _, want := range []string{"claude/claude-opus-5", "1.00", "1.50"} {
		if !strings.Contains(msg, want) {
			t.Errorf("report missing %q — it must carry both figures and the model to be actionable:\n%s", want, msg)
		}
	}
}

// TestCostDivergence_SilentWithinTolerance: small disagreement is expected
// (rounding, a fractional rate difference) and must not cry wolf.
func TestCostDivergence_SilentWithinTolerance(t *testing.T) {
	t.Parallel()
	if msg, off := CostDivergence("m", 1.000, 1.005, nil); off { // 0.5%, inside the 3% tolerance
		t.Errorf("0.5%% is within tolerance, got: %s", msg)
	}
}

// TestCostDivergence_SilentBelowFloor: 1% of a fraction of a cent is rounding.
// Reporting it would train everyone to ignore the report.
func TestCostDivergence_SilentBelowFloor(t *testing.T) {
	t.Parallel()
	if msg, off := CostDivergence("m", 0.000_1, 0.000_5, nil); off { // 400% off, but both trivial
		t.Errorf("both figures are below the floor, got: %s", msg)
	}
}

// TestCostDivergence_SilentWhenEitherSideIsZero: calculated==0 is the synthetic
// / no-op sentinel (modelinfo prices those at 0 by design), and provided==0
// means the backend reported nothing to compare against. Neither is a pricing
// disagreement.
func TestCostDivergence_SilentWhenEitherSideIsZero(t *testing.T) {
	t.Parallel()
	for _, c := range [][2]float64{{0, 1.50}, {1.50, 0}} {
		if msg, off := CostDivergence("m", c[0], c[1], nil); off {
			t.Errorf("%v: want silence, got: %s", c, msg)
		}
	}
}

// TestCostDivergence_SilentInTheRaisedBand pins the 2026-08-06 widening 1% -> 3%
// specifically. The SilentWithinTolerance case (0.5%) is silent under BOTH
// values, so it cannot detect the change; this one sits in the band that used
// to report and must not any more.
func TestCostDivergence_SilentInTheRaisedBand(t *testing.T) {
	t.Parallel()
	if msg, off := CostDivergence("m", 1.00, 1.02, nil); off { // 2%: reported at the old 1%, silent at 3%
		t.Errorf("2%% is inside the raised 3%% tolerance, got: %s", msg)
	}
}

// TestCostDivergence_StillReportsJustBeyondTheBand is the other half: widening
// the band must not blunt it. A gap the far side of 3% still has to be
// reported, or the constant could be raised to anything and every test would
// stay green.
func TestCostDivergence_StillReportsJustBeyondTheBand(t *testing.T) {
	t.Parallel()
	if _, off := CostDivergence("m", 1.00, 1.05, nil); !off { // 5%: outside 3%
		t.Fatal("5% is beyond the 3% tolerance and must be reported")
	}
}

// The breakdown is the whole diagnostic value of this report: without it the
// line reports two scalars and the question it always provokes — WHICH token
// class disagrees — needs the call reconstructed by hand.
func TestCostDivergence_ReportCarriesTheBreakdown(t *testing.T) {
	t.Parallel()
	msg, off := CostDivergence("m", 1.00, 1.50, func() string { return "cycles=3 cache_read=8586842 ($4.293421)" })
	if !off {
		t.Fatal("want a report")
	}
	if !strings.Contains(msg, "cache_read=8586842") {
		t.Errorf("report dropped the breakdown, leaving only two scalars to diagnose from:\n%s", msg)
	}
}

// The details closure must only be EVALUATED when the report fires — pricing
// the classes on the common path would be pure waste, and the closure exists
// specifically to avoid it.
func TestCostDivergence_BreakdownNotBuiltWhenSilent(t *testing.T) {
	t.Parallel()
	built := 0
	details := func() string { built++; return "x" }
	CostDivergence("m", 1.000, 1.005, details)     // inside tolerance
	CostDivergence("m", 0.000_1, 0.000_5, details) // below floor
	CostDivergence("m", 0, 1.50, details)          // zero side
	if built != 0 {
		t.Errorf("details closure evaluated %d times on the silent path — it must be paid for only when the report fires", built)
	}
}

// An empty breakdown must not leave a dangling separator in the report.
func TestCostDivergence_EmptyBreakdownAddsNothing(t *testing.T) {
	t.Parallel()
	msg, off := CostDivergence("m", 1.00, 1.50, func() string { return "" })
	if !off {
		t.Fatal("want a report")
	}
	if strings.Contains(msg, "| ") {
		t.Errorf("empty breakdown still emitted a separator:\n%s", msg)
	}
}
