package accounting

import (
	"math"
	"testing"
	"time"

	"foci/internal/modelinfo"
)

// TestTieredCallPricesAtItsPromptTier: a haiku-5-5 call whose prompt is over
// 100K tokens is booked under the tier rate group and the view prices it at the
// tier rates; the same counts booked as a remainder (a sum of calls) price at
// base, because a sum has no single prompt size (#2240).
func TestTieredCallPricesAtItsPromptTier(t *testing.T) {
	l, _ := openLedger(t)
	mustUpdate(t, l, func(tx *Tx) error { return tx.RecordTurn(turn("T")) })
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	tok := modelinfo.Tokens{modelinfo.ClassInput: 120_000, modelinfo.ClassOutput: 1_000_000}

	one := call("one", tok)
	one.Model, one.BilledAt = "claude-haiku-5-5", at
	rem := call("rem", tok)
	rem.Model, rem.BilledAt, rem.Finality = "claude-haiku-5-5", at, FinalityDerived
	b := book(t, l, one, rem)

	for _, tc := range []struct {
		name string
		id   int64
		want float64
	}{
		{"one call over 100K: tier 0.50/2.50", b[0].ID, 0.12*0.50 + 2.50},
		{"remainder: base 0.10/0.50", b[1].ID, 0.12*0.10 + 0.50},
	} {
		got := viewCost(t, l, tc.id)
		if !got.Valid || math.Abs(got.Float64-tc.want) > 1e-9 {
			t.Errorf("%s: view cost %+v, want $%.6f", tc.name, got, tc.want)
		}
	}
	for i, c := range []Call{one, rem} {
		got := c.cost()
		want := viewCost(t, l, b[i].ID)
		if got == nil || !want.Valid || math.Abs(*got-want.Float64) > 1e-9 {
			t.Errorf("call %q: Go cost %v, view %+v — must agree", c.Key, got, want)
		}
	}
}
