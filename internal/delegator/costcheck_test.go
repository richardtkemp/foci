package delegator

import (
	"fmt"
	"strings"
	"testing"
)

// capture returns a warnf that appends formatted messages to msgs.
func capture(msgs *[]string) func(string, ...any) {
	return func(format string, args ...any) {
		*msgs = append(*msgs, fmt.Sprintf(format, args...))
	}
}

// TestCostDivergence_WarnsBeyondTolerance: the whole point of still reading the
// backend's cost. Our figure is authoritative, so nothing else would notice a
// stale pricing table — this warning is the only alarm.
func TestCostDivergence_WarnsBeyondTolerance(t *testing.T) {
	t.Parallel()
	var msgs []string
	var c CostDivergenceChecker

	c.Check("claude/claude-opus-5", 1.00, 1.50, nil, capture(&msgs))

	if len(msgs) != 1 {
		t.Fatalf("got %d warnings, want 1 — a 50%% divergence must be reported", len(msgs))
	}
	for _, want := range []string{"claude/claude-opus-5", "1.00", "1.50"} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("warning missing %q — it must carry both figures and the model to be actionable:\n%s", want, msgs[0])
		}
	}
}

// TestCostDivergence_SilentWithinTolerance: small disagreement is expected
// (rounding, a fractional rate difference) and must not cry wolf.
func TestCostDivergence_SilentWithinTolerance(t *testing.T) {
	t.Parallel()
	var msgs []string
	var c CostDivergenceChecker

	c.Check("m", 1.000, 1.005, nil, capture(&msgs)) // 0.5%, inside the 3% tolerance

	if len(msgs) != 0 {
		t.Errorf("got %d warnings, want 0 — 0.5%% is within tolerance:\n%s", len(msgs), strings.Join(msgs, "\n"))
	}
}

// TestCostDivergence_SilentBelowFloor: 1% of a fraction of a cent is rounding.
// Warning on it would train everyone to ignore the warning.
func TestCostDivergence_SilentBelowFloor(t *testing.T) {
	t.Parallel()
	var msgs []string
	var c CostDivergenceChecker

	c.Check("m", 0.000_1, 0.000_5, nil, capture(&msgs)) // 400% off, but both trivial

	if len(msgs) != 0 {
		t.Errorf("got %d warnings, want 0 — both figures are below the floor:\n%s", len(msgs), strings.Join(msgs, "\n"))
	}
}

// TestCostDivergence_SilentWhenEitherSideIsZero: calculated==0 is the synthetic
// / no-op sentinel (modelinfo prices those at 0 by design), and provided==0
// means the backend reported nothing to compare against. Neither is a pricing
// disagreement.
func TestCostDivergence_SilentWhenEitherSideIsZero(t *testing.T) {
	t.Parallel()
	var msgs []string
	var c CostDivergenceChecker

	c.Check("m", 0, 1.50, nil, capture(&msgs))
	c.Check("m", 1.50, 0, nil, capture(&msgs))

	if len(msgs) != 0 {
		t.Errorf("got %d warnings, want 0:\n%s", len(msgs), strings.Join(msgs, "\n"))
	}
}

// TestCostDivergence_ThrottledPerModel: a stale price diverges on EVERY turn.
// Unthrottled, the first occurrence — the informative one — is buried by its
// own repeats.
func TestCostDivergence_ThrottledPerModel(t *testing.T) {
	t.Parallel()
	var msgs []string
	var c CostDivergenceChecker

	for range 5 {
		c.Check("opus", 1.00, 1.50, nil, capture(&msgs))
	}

	if len(msgs) != 1 {
		t.Errorf("got %d warnings for one model, want 1 (rate-limited):\n%s", len(msgs), strings.Join(msgs, "\n"))
	}
}

// TestCostDivergence_ThrottleIsPerModelNotGlobal: a second model diverging is
// new information and must not be swallowed by the first model's throttle.
func TestCostDivergence_ThrottleIsPerModelNotGlobal(t *testing.T) {
	t.Parallel()
	var msgs []string
	var c CostDivergenceChecker

	c.Check("opus", 1.00, 1.50, nil, capture(&msgs))
	c.Check("haiku", 1.00, 1.50, nil, capture(&msgs))

	if len(msgs) != 2 {
		t.Errorf("got %d warnings, want 2 (one per model):\n%s", len(msgs), strings.Join(msgs, "\n"))
	}
}

// TestCostDivergence_ZeroValueUsable guards the zero-value contract — the
// checker is embedded by value in each Backend and never explicitly
// constructed, so a nil map must not panic on first use.
func TestCostDivergence_ZeroValueUsable(t *testing.T) {
	t.Parallel()
	var c CostDivergenceChecker
	c.Check("m", 1.00, 1.50, nil, func(string, ...any) {})
}

// TestCostDivergence_NilWarnfSafe: callers pass a logger method; a nil one must
// not panic the turn path this is reporting on.
func TestCostDivergence_NilWarnfSafe(t *testing.T) {
	t.Parallel()
	var c CostDivergenceChecker
	c.Check("m", 1.00, 1.50, nil, nil)
}

// TestCostDivergence_SilentInTheRaisedBand pins the 2026-08-06 widening 1% -> 3%
// specifically. The existing SilentWithinTolerance case (0.5%) is silent under
// BOTH values, so it cannot detect the change; this one sits in the band that
// used to warn and must not any more.
//
// 2% is where the residual lives once the pricing table is right: per-turn
// rounding plus the 5m/1h cache-write split foci deliberately does not model.
// Warning on it trains everyone to ignore the warning, which costs the whole
// mechanism — and this check is the ONLY thing that would ever notice a stale
// table, since foci's own figure is the stored one.
func TestCostDivergence_SilentInTheRaisedBand(t *testing.T) {
	t.Parallel()
	var msgs []string
	var c CostDivergenceChecker

	c.Check("m", 1.00, 1.02, nil, capture(&msgs)) // 2%: warned at the old 1%, silent at 3%

	if len(msgs) != 0 {
		t.Errorf("got %d warnings, want 0 — 2%% is inside the raised 3%% tolerance:\n%s",
			len(msgs), strings.Join(msgs, "\n"))
	}
}

// TestCostDivergence_StillWarnsJustBeyondTheBand is the other half: widening the
// band must not blunt it. A gap the far side of 3% still has to be reported, or
// the constant could be raised to anything and every test would stay green.
func TestCostDivergence_StillWarnsJustBeyondTheBand(t *testing.T) {
	t.Parallel()
	var msgs []string
	var c CostDivergenceChecker

	c.Check("m", 1.00, 1.05, nil, capture(&msgs)) // 5%: outside 3%

	if len(msgs) != 1 {
		t.Fatalf("got %d warnings, want 1 — 5%% is beyond the 3%% tolerance", len(msgs))
	}
}

// The breakdown is the whole diagnostic value of this warning: without it the
// line reports two scalars and the question it always provokes — WHICH token
// class disagrees — needs the turn reconstructed from CC's transcript, because
// api.db stores the final cycle's context fill rather than the tokens priced.
func TestCostDivergence_WarningCarriesTheBreakdown(t *testing.T) {
	t.Parallel()
	var msgs []string
	var c CostDivergenceChecker

	c.Check("m", 1.00, 1.50, func() string { return "cycles=3 cache_read=8586842 ($4.293421)" }, capture(&msgs))

	if len(msgs) != 1 {
		t.Fatalf("got %d warnings, want 1", len(msgs))
	}
	if !strings.Contains(msgs[0], "cache_read=8586842") {
		t.Errorf("warning dropped the breakdown, leaving only two scalars to diagnose from:\n%s", msgs[0])
	}
}

// The details closure runs on EVERY turn's worth of arguments but must only be
// EVALUATED when the warning actually fires — pricing four classes on the
// common path would be pure waste, and the closure exists specifically to avoid
// it. A plain string parameter would have quietly lost this.
func TestCostDivergence_BreakdownNotBuiltWhenSilent(t *testing.T) {
	t.Parallel()
	var msgs []string
	var c CostDivergenceChecker
	built := 0
	details := func() string { built++; return "x" }

	c.Check("m", 1.000, 1.005, details, capture(&msgs))     // inside tolerance
	c.Check("m", 0.000_1, 0.000_5, details, capture(&msgs)) // below floor
	c.Check("m", 0, 1.50, details, capture(&msgs))          // zero side

	if len(msgs) != 0 {
		t.Fatalf("expected silence, got %d warnings", len(msgs))
	}
	if built != 0 {
		t.Errorf("details closure evaluated %d times on the silent path — it must be paid for only when the warning fires", built)
	}
}

// An empty breakdown must not leave a dangling separator in the message.
func TestCostDivergence_EmptyBreakdownAddsNothing(t *testing.T) {
	t.Parallel()
	var msgs []string
	var c CostDivergenceChecker
	c.Check("m", 1.00, 1.50, func() string { return "" }, capture(&msgs))
	if len(msgs) != 1 {
		t.Fatalf("got %d warnings, want 1", len(msgs))
	}
	if strings.Contains(msgs[0], "| ") {
		t.Errorf("empty breakdown still emitted a separator:\n%s", msgs[0])
	}
}
