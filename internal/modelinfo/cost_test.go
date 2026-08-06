package modelinfo

import (
	"testing"
)

func TestUnpricedModelWarnsOnce(t *testing.T) {
	var got []string
	UnpricedModelHook = func(m string) { got = append(got, m) }
	t.Cleanup(func() {
		UnpricedModelHook = nil
		unpricedMu.Lock()
		unpricedSeen = map[string]bool{}
		unpricedMu.Unlock()
	})

	Cost("mystery-model-x", 100, 0, 0, 0)
	Cost("mystery-model-x", 200, 0, 0, 0) // same model again
	Cost("gpt-7", 100, 0, 0, 0)           // openai fallback also counts
	Cost("claude-opus-4-8", 100, 0, 0, 0) // family match → NO warn

	want := []string{"mystery-model-x", "gpt-7"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("unpriced warnings = %v, want %v", got, want)
	}
}

func TestSyntheticModelIsFreeAndNotUnpriced(t *testing.T) {
	var got []string
	UnpricedModelHook = func(m string) { got = append(got, m) }
	t.Cleanup(func() {
		UnpricedModelHook = nil
		unpricedMu.Lock()
		unpricedSeen = map[string]bool{}
		unpricedMu.Unlock()
	})

	// The synthetic sentinel prices at $0 regardless of token counts...
	if cost := Cost("<synthetic>", 1_000_000, 1_000_000, 1_000_000, 1_000_000); cost != 0 {
		t.Errorf("synthetic cost = %f, want 0", cost)
	}
	// ...and must NOT trip the unpriced-model warning.
	if len(got) != 0 {
		t.Errorf("synthetic tripped unpriced warning: %v", got)
	}
	if !IsSynthetic("<synthetic>") || IsSynthetic("claude-haiku-4-5") {
		t.Error("IsSynthetic misclassified a model")
	}
}

// A PROVIDER-PREFIXED synthetic sentinel (e.g. "openrouter/<synthetic>" from a
// non-ccstream caller) must also price at $0 and not trip the unpriced warning.
// The exact-string IsSynthetic guard alone misses it — normalizeParts strips the
// prefix to a bare "<synthetic>", which Cost now catches via IsSynthetic(bare).
// Regression for #1331 (bare "<synthetic>" warn from a prefixed callsite).
func TestPrefixedSyntheticIsFreeAndNotUnpriced(t *testing.T) {
	var got []string
	UnpricedModelHook = func(m string) { got = append(got, m) }
	t.Cleanup(func() {
		UnpricedModelHook = nil
		unpricedMu.Lock()
		unpricedSeen = map[string]bool{}
		unpricedMu.Unlock()
	})

	for _, model := range []string{"openrouter/<synthetic>", "claude/<synthetic>", "anthropic/<synthetic>"} {
		if cost := Cost(model, 1_000_000, 1_000_000, 1_000_000, 1_000_000); cost != 0 {
			t.Errorf("prefixed synthetic %q cost = %f, want 0", model, cost)
		}
	}
	if len(got) != 0 {
		t.Errorf("prefixed synthetic tripped unpriced warning: %v", got)
	}
}

// TestCalculateCostOpenAIFallback pins the $5/$15 approximation that Cost()
// applies in code (not from the registry) to OpenAI-looking models with no
// registry entry. It is a code constant, not sync-churned map data.
func TestCalculateCostOpenAIFallback(t *testing.T) {
	// 1M input tokens on unknown OpenAI model = $5.00. Synthetic names (not in
	// the OpenRouter catalogue, so absent from the built-in registry) that still
	// look like OpenAI models (gpt-/o4- prefix → IsOpenAI) to hit the fallback.
	cost := Cost("gpt-synthetic-999", 1_000_000, 0, 0, 0)
	if cost != 5.0 {
		t.Errorf("1M input unknown openai = %f, want 5.0", cost)
	}

	// 1M output tokens on unknown OpenAI model = $15.00
	cost = Cost("o4-synthetic-999", 0, 1_000_000, 0, 0)
	if cost != 15.0 {
		t.Errorf("1M output unknown openai = %f, want 15.0", cost)
	}
}

// TestCost_PrefersOneHourCacheWriteRate pins the 2026-08-06 reversal of the
// "one cache-write rate is enough" ruling.
//
// Claude Code caches at 1h EXCLUSIVELY — measured on a live helen session:
// 273,094 ephemeral_1h cache-write tokens and 0 ephemeral_5m. Pricing those at
// the 5m rate understated opus-5 by $3.75/M, which was 11.4% of that session's
// bill and 16.6% of the turn that tripped the divergence warning.
func TestCost_PrefersOneHourCacheWriteRate(t *testing.T) {
	// claude-opus-5: input $5/M, so Anthropic's 1h write (2x input) is $10/M
	// against the 5m rate of $6.25/M.
	const oneMillion = 1_000_000
	got := Cost("claude/claude-opus-5", 0, 0, 0, oneMillion)

	if want := 10.00; got != want {
		t.Errorf("1M cache-write tokens priced at $%.4f, want $%.2f (the 1h rate).\n"+
			"$6.25 means the 5m rate is still winning — CC writes are 100%% 1h, so that "+
			"understates every cached turn.", got, want)
	}
}

// TestCost_FallsBackToFiveMinuteRateWhenNoOneHourFigure guards the other half:
// only 26 of 477 registry rows carry a 1h rate, so preferring it must not zero
// out pricing for the 70 rows that have a 5m rate and no 1h one. A fix that
// simply swapped the field would pass the test above and silently price those
// models' cache writes at $0.
func TestCost_FallsBackToFiveMinuteRateWhenNoOneHourFigure(t *testing.T) {
	m := Model{InputPer1M: 1, OutputPer1M: 2, CacheReadPer1M: 3, CacheWritePer1M: 7}
	if got := m.cacheWriteRate(); got != 7 {
		t.Errorf("cacheWriteRate() = %v with no 1h figure, want the 5m rate 7", got)
	}
	m.CacheWrite1hPer1M = 11
	if got := m.cacheWriteRate(); got != 11 {
		t.Errorf("cacheWriteRate() = %v with a 1h figure set, want 11", got)
	}
}
