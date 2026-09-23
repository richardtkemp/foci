package modelinfo

import (
	"math"
	"testing"
	"time"
)

// #1913: CC reports its searches under the dated haiku id its search sub-call
// ran on. That id must resolve to a row that carries a per-call rate — the
// hand-added CC-id rows originally had none, so wiring the consumer alone would
// have priced every real search at $0.
func TestWebSearchCostAsOf_PricesTheModelCCReportsSearchesUnder(t *testing.T) {
	t.Parallel()
	for _, model := range []string{"claude-haiku-4-5-20251001", "claude-opus-5", "claude-opus-5-5", "claude-sonnet-5"} {
		got, priced := WebSearchCostAsOf(model, time.Now(), 3)
		if !priced || math.Abs(got-0.03) > 1e-12 {
			t.Errorf("%s: WebSearchCostAsOf(3) = $%.6f priced=%v, want $0.030000 priced", model, got, priced)
		}
	}
}

// A row with no per-call rate must say so rather than pass $0 off as "free".
func TestWebSearchCostAsOf_ReportsMissingRate(t *testing.T) {
	t.Parallel()
	if got, priced := WebSearchCostAsOf("claude-code", time.Now(), 1); priced || got != 0 {
		t.Errorf("claude-code (no rate): got $%.6f priced=%v, want $0 unpriced", got, priced)
	}
	// No searches is nothing to price, not a missing rate.
	if _, priced := WebSearchCostAsOf("claude-code", time.Now(), 0); !priced {
		t.Error("zero searches reported as unpriced")
	}
}

// The #1854 identity: a stored row re-priced from its counts must land on the
// cost it was recorded at, so the counts must carry the searches too.
func TestTokenCountsCostAsOf_IncludesWebSearches(t *testing.T) {
	t.Parallel()
	now := time.Now()
	base := TokenCounts{Input: 1000, Output: 100}
	with := base
	with.WebSearches = 2
	if diff := with.CostAsOf("claude-haiku-4-5", now) - base.CostAsOf("claude-haiku-4-5", now); math.Abs(diff-0.02) > 1e-12 {
		t.Errorf("two searches added $%.6f, want $0.020000", diff)
	}
	if sum := base.Add(with); sum.WebSearches != 2 {
		t.Errorf("Add dropped WebSearches: %+v", sum)
	}
	if parent, ok := with.SubClamped(base); !ok || parent.WebSearches != 2 {
		t.Errorf("SubClamped = %+v ok=%v, want WebSearches kept on the parent", parent, ok)
	}
}
