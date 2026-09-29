package modelinfo

import (
	"testing"
	"time"
)

// flatCost prices four flat counts now, cache writes as TTL-unknown — the shape
// the removed Cost(model, in, out, cr, cw) had, kept so these tests read as
// before.
func flatCost(model string, in, out, cr, cw int) float64 {
	usd, _ := CostAsOf(model, time.Now(), Tokens{
		ClassInput: in, ClassOutput: out, ClassCacheRead: cr, ClassCacheWrite: cw,
	})
	return usd
}

func TestUnpricedModelWarnsOnce(t *testing.T) {
	var got []string
	UnpricedModelHook = func(m string) { got = append(got, m) }
	t.Cleanup(func() {
		UnpricedModelHook = nil
		unpricedMu.Lock()
		unpricedSeen = map[string]bool{}
		unpricedMu.Unlock()
	})

	flatCost("mystery-model-x", 100, 0, 0, 0)
	flatCost("mystery-model-x", 200, 0, 0, 0) // same model again
	flatCost("gpt-7", 100, 0, 0, 0)           // an OpenAI-looking id is no exception
	// claude-opus-4-8 has no hyphen row, but models.jsonl carries the dot
	// spelling (claude-opus-4.8) with real rates — the punctuation-wildcard
	// retry (#1966) resolves it as an exact hit by another spelling, so this
	// must NOT warn either as unpriced or (see TestPunctuationWildcard*) as a
	// family fallback.
	flatCost("claude-opus-4-8", 100, 0, 0, 0)

	want := []string{"mystery-model-x", "gpt-7"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("unpriced warnings = %v, want %v", got, want)
	}
}

// #1833: a model version with no exact registry row that inherits its
// family's rates (e.g. a brand-new "claude-fable-5-2") must warn once via
// FamilyPricedModelHook — distinct from UnpricedModelHook, which only fires
// when there's no family match either. An exact registry hit must NOT warn.
func TestFamilyPricedModelWarnsOnceAndOnlyOnFamilyFallback(t *testing.T) {
	var familyWarned, unpriced []string
	FamilyPricedModelHook = func(m string) { familyWarned = append(familyWarned, m) }
	UnpricedModelHook = func(m string) { unpriced = append(unpriced, m) }
	t.Cleanup(func() {
		FamilyPricedModelHook = nil
		UnpricedModelHook = nil
		familyPricedMu.Lock()
		familyPricedSeen = map[string]bool{}
		familyPricedMu.Unlock()
		unpricedMu.Lock()
		unpricedSeen = map[string]bool{}
		unpricedMu.Unlock()
	})

	flatCost("claude-fable-5-2", 100, 0, 0, 0) // no exact row → family fallback → warn
	flatCost("claude-fable-5-2", 200, 0, 0, 0) // same model again → no repeat warn
	flatCost("claude-haiku-4-5", 100, 0, 0, 0) // exact registry hit → no warn at all
	flatCost("mystery-model-x", 100, 0, 0, 0)  // no family match → unpriced, not family

	wantFamily := []string{"claude-fable-5-2"}
	if len(familyWarned) != len(wantFamily) || familyWarned[0] != wantFamily[0] {
		t.Fatalf("family-priced warnings = %v, want %v", familyWarned, wantFamily)
	}
	wantUnpriced := []string{"mystery-model-x"}
	if len(unpriced) != len(wantUnpriced) || unpriced[0] != wantUnpriced[0] {
		t.Fatalf("unpriced warnings = %v, want %v", unpriced, wantUnpriced)
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
	if cost := flatCost("<synthetic>", 1_000_000, 1_000_000, 1_000_000, 1_000_000); cost != 0 {
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
		if cost := flatCost(model, 1_000_000, 1_000_000, 1_000_000, 1_000_000); cost != 0 {
			t.Errorf("prefixed synthetic %q cost = %f, want 0", model, cost)
		}
	}
	if len(got) != 0 {
		t.Errorf("prefixed synthetic tripped unpriced warning: %v", got)
	}
}

// TestUnknownModelIsUnpricedNotGuessed pins #2111 R9: a model that resolves
// to no rate group is UNPRICED. There used to be a guessed fallback (OpenAI-
// looking ids at $5/$15, everything else at haiku's rates); a guess booked as a
// cost is exactly the silent absorption R9 forbids.
func TestUnknownModelIsUnpricedNotGuessed(t *testing.T) {
	for _, model := range []string{"gpt-synthetic-999", "o4-synthetic-999", "mystery-model-x"} {
		usd, priced := CostAsOf(model, time.Now(), Tokens{ClassInput: 1_000_000})
		if priced || usd != 0 {
			t.Errorf("CostAsOf(%q) = $%v priced=%v, want $0 unpriced", model, usd, priced)
		}
		if _, ok := ResolveRateModel(model, time.Now()); ok {
			t.Errorf("ResolveRateModel(%q) resolved; want not in table", model)
		}
	}
	// No tokens billed: nothing to price, so the call is fully priced at $0.
	if usd, priced := CostAsOf("mystery-model-x", time.Now(), Tokens{}); !priced || usd != 0 {
		t.Errorf("empty tokens on unknown model = $%v priced=%v, want $0 priced", usd, priced)
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
	got := flatCost("claude/claude-opus-5", 0, 0, 0, oneMillion)

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
	r := Prices{Input: 1, Output: 2, CacheRead: 3, CacheWrite5m: 7}.Rates()
	for _, c := range []Class{ClassCacheWrite, ClassCacheWrite1h, ClassCacheWrite5m} {
		if got := r[c]; got != 7 {
			t.Errorf("%s rate = %v with no 1h figure, want the 5m rate 7", c, got)
		}
	}
	r = Prices{Input: 1, Output: 2, CacheRead: 3, CacheWrite5m: 7, CacheWrite1h: 11}.Rates()
	if r[ClassCacheWrite] != 11 || r[ClassCacheWrite1h] != 11 || r[ClassCacheWrite5m] != 7 {
		t.Errorf("rates with a 1h figure = %v, want unknown and 1h at 11, 5m at 7", r)
	}
	// A 1h figure BELOW the 5m one is a data error; it must not lower the
	// unknown-TTL rate below 5m, which is what keeps the #1929 surcharge >= 0.
	r = Prices{CacheWrite5m: 6.25, CacheWrite1h: 3}.Rates()
	if r[ClassCacheWrite] != 6.25 {
		t.Errorf("unknown-TTL rate with 1h < 5m = %v, want 6.25", r[ClassCacheWrite])
	}
}
