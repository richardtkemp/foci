package modelinfo

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Prompt-size price tiers (#2240). claude-haiku-5-5's rates are from the
// platform pricing page: up to 100K prompt tokens input 0.10 / output 0.50 /
// read 0.01 / 5m write 0.125 / 1h write 0.20; over 100K 0.50 / 2.50 / 0.05 /
// 0.625 / 1.
func TestCostAsOfPicksPromptTier(t *testing.T) {
	at := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	const m = "claude-haiku-5-5"
	for _, tc := range []struct {
		name string
		t    Tokens
		want float64
	}{
		{"under the threshold: base", Tokens{ClassInput: 50_000, ClassOutput: 1_000_000}, 0.05*0.10 + 0.50},
		{"exactly 100K is not OVER: base", Tokens{ClassInput: 100_000, ClassOutput: 1_000_000}, 0.10*0.10 + 0.50},
		{"one over: the whole call at tier rates", Tokens{ClassInput: 100_001, ClassOutput: 1_000_000}, 0.100001*0.50 + 2.50},
		{"cache reads count toward the prompt", Tokens{ClassInput: 10, ClassCacheRead: 150_000}, 0.00001*0.50 + 0.15*0.05},
		{"cache writes count toward the prompt", Tokens{ClassInput: 10, ClassCacheWrite1h: 200_000}, 0.00001*0.50 + 0.2*1},
		{"output does not count toward the prompt", Tokens{ClassInput: 10, ClassOutput: 500_000}, 0.00001*0.10 + 0.5*0.50},
	} {
		got, ok := CostAsOf(m, at, tc.t)
		if !ok || math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("%s: CostAsOf = %.12f (priced %v), want %.12f", tc.name, got, ok, tc.want)
		}
	}
}

func TestCostAsOfPromptZeroIsBase(t *testing.T) {
	// A sum of calls prices at base whatever its total: prompt 0.
	at := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	tok := Tokens{ClassInput: 500_000}
	got, ok := CostAsOfPrompt("claude-haiku-5-5", at, tok, 0)
	if want := 0.5 * 0.10; !ok || math.Abs(got-want) > 1e-12 {
		t.Fatalf("CostAsOfPrompt(prompt 0) = %v (priced %v), want base %v", got, ok, want)
	}
}

func TestTierRateModelRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	base, ok := ResolveRateModel("claude-haiku-5-5", at, 100_000)
	if !ok || strings.Contains(base, ">") {
		t.Fatalf("at 100K: rate model %q (ok %v), want the base group", base, ok)
	}
	tier, ok := ResolveRateModel("claude-haiku-5-5", at, 100_001)
	if !ok || tier != base+"|>100000" {
		t.Fatalf("over 100K: rate model %q, want %q", tier, base+"|>100000")
	}
	rates, ok := RatesAsOf(tier, at)
	if !ok || rates[ClassInput] != 0.5 || rates[ClassOutput] != 2.5 || rates[ClassCacheRead] != 0.05 ||
		rates[ClassCacheWrite5m] != 0.625 || rates[ClassCacheWrite1h] != 1 {
		t.Fatalf("RatesAsOf(%q) = %v, want the >100K tier", tier, rates)
	}
	// The SQL views price from RateTable: the tier group must be rendered.
	found := false
	for _, r := range RateTable() {
		if r.RateModel == tier && r.Class == ClassInput && r.USDPerUnit != nil && *r.USDPerUnit == 0.5 {
			found = true
		}
	}
	if !found {
		t.Fatalf("RateTable has no input row for %q", tier)
	}
	// A model without tiers never gets a tier group.
	if rm, _ := ResolveRateModel("claude-opus-5", at, 900_000); strings.Contains(rm, ">") {
		t.Fatalf("untiered model resolved to %q", rm)
	}
}

func TestParseRateGroupRejectsBadTier(t *testing.T) {
	for _, s := range []string{"a|p|d|100", "a|p|d|>x", "a|p|d|>0", "a|p|d|>5|x"} {
		if _, ok := parseRateGroup(s); ok {
			t.Errorf("parseRateGroup(%q) accepted a malformed tier", s)
		}
	}
	if g, ok := parseRateGroup("a|p|d|>5"); !ok || g.tier != 5 || g.String() != "a|p|d|>5" {
		t.Errorf("parseRateGroup round trip: %+v %v", g, ok)
	}
}

func TestHaiku55Registered(t *testing.T) {
	if cw := ContextWindow("claude-haiku-5-5"); cw != 1_000_000 {
		t.Errorf("ContextWindow = %d, want 1000000 (not the haiku-4-5 family fallback)", cw)
	}
	if effort, thinking, _ := Capabilities("claude-haiku-5-5"); !effort || !thinking {
		t.Errorf("Capabilities effort=%v thinking=%v, want both true (/v1/models)", effort, thinking)
	}
}
