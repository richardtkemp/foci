package ccstream

import (
	"math"
	"testing"

	"foci/internal/delegator"
	"foci/internal/modelinfo"
)

// #1866 P3: a turn is priced PER MODEL, with cache writes charged at the TTL
// they were actually written at.
//
// These fixtures set Message.ID deliberately. The accumulator drops messages
// with an empty id, and NO existing fixture in this package sets one — so every
// pre-existing end-to-end cost test runs against an EMPTY accumulator and
// cannot distinguish per-model pricing from single-model pricing, nor the 5m
// rate from the 1h rate. They pass either way. These do not.

func onResultWith(b *Backend, mu map[string]ModelUsage) *delegator.TurnResult {
	var got *delegator.TurnResult
	applyHandler(b, &testHandler{OnTurnComplete: func(r *delegator.TurnResult) { got = r }})
	b.mu.Lock()
	b.lastModel = "claude-opus-5"
	b.mu.Unlock()
	b.OnResult(&ResultMessage{Subtype: "success", Result: "ok", ModelUsage: mu})
	return got
}

// TestOnResult_PricesEveryModelNotJustTheResultModel is the #1870 regression.
//
// The predecessor read ModelUsage[resultModel] and dropped every other model's
// spend. Measured on a live turn: $0.54 charged against a true $1.68, and the
// divergence check stayed silent because BOTH its sides omitted the same model.
func TestOnResult_PricesEveryModelNotJustTheResultModel(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	got := onResultWith(b, map[string]ModelUsage{
		"claude-opus-5":   {OutputTokens: 1000, CostUSD: 0.5},
		"claude-sonnet-5": {OutputTokens: 2000, CostUSD: 0.2},
	})
	if got == nil || got.Usage == nil || got.Usage.CalculatedCostUSD == nil {
		t.Fatal("no calculated cost")
	}
	// opus-5 output $25/MTok, sonnet-5 $10/MTok.
	want := 1000*25.0/1e6 + 2000*10.0/1e6
	if math.Abs(*got.Usage.CalculatedCostUSD-want) > 1e-9 {
		t.Errorf("cost = %.9f, want %.9f — the subagent model's spend must be priced too",
			*got.Usage.CalculatedCostUSD, want)
	}
	// And the token totals must include it, or api.db under-reports the turn.
	if got.Usage.Turn == nil || got.Usage.Turn.Output != 3000 {
		t.Errorf("Turn.Output = %v, want 3000 across both models", got.Usage.Turn)
	}
}

// TestOnResult_SubagentCacheWritesPriceAtTheFiveMinuteRate is the #1866
// regression: the bug that started all of this.
func TestOnResult_SubagentCacheWritesPriceAtTheFiveMinuteRate(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	// A subagent message observed on the stream: 100k cache writes, all 5m.
	b.noteAssistantUsage(msgWith("msg_sub", "tool_1", 100000, 100000, 0))

	got := onResultWith(b, map[string]ModelUsage{
		"claude-opus-5": {CacheCreationInputTokens: 100000, CostUSD: 0.6},
	})
	if got == nil || got.Usage == nil || got.Usage.CalculatedCostUSD == nil {
		t.Fatal("no calculated cost")
	}
	want5m := 100000 * 6.25 / 1e6  // $0.625
	want1h := 100000 * 10.00 / 1e6 // $1.000 — what the bug charged
	if math.Abs(*got.Usage.CalculatedCostUSD-want5m) > 1e-9 {
		t.Errorf("cost = %.9f, want %.9f (5m rate). The 1h rate would give %.9f — a 60%% overcharge.",
			*got.Usage.CalculatedCostUSD, want5m, want1h)
	}
}

// TestOnResult_UnobservedTTLPricesAtTheOneHourRate pins the safe fallback.
// An empty accumulator means the TTL was NOT OBSERVED, which is different from
// "it was 5m" — assuming the cheaper rate is how money goes missing.
func TestOnResult_UnobservedTTLPricesAtTheOneHourRate(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	got := onResultWith(b, map[string]ModelUsage{
		"claude-opus-5": {CacheCreationInputTokens: 100000, CostUSD: 0.6},
	})
	if got == nil || got.Usage == nil || got.Usage.CalculatedCostUSD == nil {
		t.Fatal("no calculated cost")
	}
	want := 100000 * 10.00 / 1e6
	if math.Abs(*got.Usage.CalculatedCostUSD-want) > 1e-9 {
		t.Errorf("cost = %.9f, want %.9f (1h rate) — an unobserved TTL must not be assumed cheap",
			*got.Usage.CalculatedCostUSD, want)
	}
}

// TestSplitFor_AllocatesTheAuthoritativeTotalExactly: whatever the accumulator
// saw, the priced classes must sum to ModelUsage's figure, or pricing stops
// reconciling against the number the divergence check compares to.
func TestSplitFor_AllocatesTheAuthoritativeTotalExactly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		observed cacheWriteSplit
		total    int
		want     modelinfo.CacheWrites
	}{
		{"exact coverage — the split is used verbatim",
			cacheWriteSplit{Ephemeral5m: 300, Ephemeral1h: 700}, 1000,
			modelinfo.CacheWrites{Ephemeral5m: 300, Ephemeral1h: 700}},
		{"nothing observed — all unknown, priced at 1h",
			cacheWriteSplit{}, 1000,
			modelinfo.CacheWrites{Unknown: 1000}},
		{"partial coverage — the shortfall is unknown, NOT scaled up",
			cacheWriteSplit{Ephemeral5m: 200}, 1000,
			modelinfo.CacheWrites{Ephemeral5m: 200, Unknown: 800}},
		{"accumulator read high — trimmed from the cheaper class first",
			cacheWriteSplit{Ephemeral5m: 900, Ephemeral1h: 400}, 1000,
			modelinfo.CacheWrites{Ephemeral5m: 600, Ephemeral1h: 400}},
		{"no writes at all",
			cacheWriteSplit{Ephemeral5m: 50}, 0, modelinfo.CacheWrites{}},
	}
	for _, c := range cases {
		got := splitFor(c.observed, c.total)
		if got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
		if c.total > 0 && got.Ephemeral5m+got.Ephemeral1h+got.Unknown != c.total {
			t.Errorf("%s: classes sum to %d, want %d — must reconcile to ModelUsage",
				c.name, got.Ephemeral5m+got.Ephemeral1h+got.Unknown, c.total)
		}
	}
}

// TestSplitFor_NeverScalesProportionally is the explicit refusal.
// Scaling a partial observation onto an authoritative total manufactures a
// number nobody measured. Dick rejected that twice; this pins the refusal so a
// future "improvement" has to argue with a test.
func TestSplitFor_NeverScalesProportionally(t *testing.T) {
	t.Parallel()
	// Observed 100 tokens, 100% of them 5m, against an authoritative 1000.
	// A ratio would say "100% 5m, so all 1000 are 5m". The honest answer is
	// that 900 tokens' TTL was never seen.
	got := splitFor(cacheWriteSplit{Ephemeral5m: 100}, 1000)
	if got.Ephemeral5m == 1000 {
		t.Fatal("proportional scaling: a 10% sample was extrapolated to the whole total")
	}
	if got.Ephemeral5m != 100 || got.Unknown != 900 {
		t.Errorf("got %+v, want 5m=100 unknown=900 — only what was observed is claimed", got)
	}
}
