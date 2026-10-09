package modelinfo

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRegisterAndLookup(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	ctx := 300_000
	m := Model{
		ContextWindow: ctx,
		Caching:       true,
		Rates:         Prices{Input: 2.00, Output: 10.00, CacheRead: 0.20, CacheWrite5m: 2.50}.Rates(),
	}
	Register("", "test-register-model", m)

	got, ok := Lookup("", "test-register-model")
	if !ok {
		t.Fatal("Lookup failed for registered model")
	}
	if got.ContextWindow != ctx {
		t.Errorf("ContextWindow = %d, want %d", got.ContextWindow, ctx)
	}
	if got.Provider != "" {
		t.Errorf("Provider = %q, want empty for providerless entry", got.Provider)
	}
}

func TestRegisterWithProvider(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	Register("zai-coding-plan", "syn-solo-model", Model{
		ContextWindow: 1_000_000,
		Rates:         Prices{Input: 0.0, Output: 0.0}.Rates(),
	})

	// Provider-specific lookup hits.
	m, ok := Lookup("zai-coding-plan", "syn-solo-model")
	if !ok {
		t.Fatal("Lookup with provider failed")
	}
	if m.Provider != "zai-coding-plan" {
		t.Errorf("Provider = %q, want %q", m.Provider, "zai-coding-plan")
	}

	// Providerless lookup now hits via the sole-provider fallback: syn-solo-model
	// has exactly one provider entry, so a providerless lookup matches it.
	if pm, ok := Lookup("", "syn-solo-model"); !ok {
		t.Error("Lookup without provider should hit via sole-provider fallback")
	} else if pm.Provider != "zai-coding-plan" {
		t.Errorf("Provider = %q, want %q", pm.Provider, "zai-coding-plan")
	}

	// Cost with provider prefix hits the provider-specific entry.
	cost := flatCost("zai-coding-plan/syn-solo-model", 1_000_000, 500_000, 0, 0)
	if cost != 0 {
		t.Errorf("Cost = %v, want 0 (all prices zero)", cost)
	}

	// Cost without provider prefix also hits now (sole-provider fallback),
	// so it uses the registered zero pricing rather than family/default fallback.
	if cost := flatCost("syn-solo-model", 100, 50, 0, 0); cost != 0 {
		t.Errorf("Cost = %v, want 0 (sole-provider entry has zero pricing)", cost)
	}
}

func TestProviderFallbackToProviderless(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	// Register a providerless entry.
	Register("", "test-fb", Model{
		ContextWindow: 200_000,
		Rates:         Prices{Input: 1.00}.Rates(),
	})

	// Provider-specific lookup falls back to providerless.
	m, ok := Lookup("some-provider", "test-fb")
	if !ok {
		t.Fatal("Lookup with unknown provider should fall back to providerless")
	}
	if m.Rates[ClassInput] != 1.00 {
		t.Errorf("InputPer1M = %v, want 1.0 (providerless fallback)", m.Rates[ClassInput])
	}

	// Provider-specific entry takes priority when it exists.
	Register("specific", "test-fb", Model{
		ContextWindow: 400_000,
		Rates:         Prices{Input: 2.00}.Rates(),
	})
	m, ok = Lookup("specific", "test-fb")
	if !ok {
		t.Fatal("Lookup with specific provider failed")
	}
	if m.Rates[ClassInput] != 2.00 {
		t.Errorf("InputPer1M = %v, want 2.0 (provider-specific)", m.Rates[ClassInput])
	}
}

func TestRegisterOverridesExisting(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	original, _ := Lookup("", "claude-haiku-4-5")
	newPrice := 99.99
	Register("", "claude-haiku-4-5", Model{
		ContextWindow: original.ContextWindow,
		Caching:       original.Caching,
		Rates:         Prices{Input: newPrice, Output: original.Rates[ClassOutput], CacheRead: original.Rates[ClassCacheRead], CacheWrite5m: original.Prices().CacheWrite5m}.Rates(),
	})

	got, ok := Lookup("", "claude-haiku-4-5")
	if !ok {
		t.Fatal("Lookup failed")
	}
	if got.Rates[ClassInput] != newPrice {
		t.Errorf("InputPer1M = %v, want %v (override did not take effect)", got.Rates[ClassInput], newPrice)
	}

	cost := flatCost("claude-haiku-4-5", 1_000_000, 0, 0, 0)
	if cost != newPrice {
		t.Errorf("Cost with 1M input = %v, want %v", cost, newPrice)
	}
}

func TestResetToBuiltIn(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	Register("", "temp-model", Model{ContextWindow: 500_000})
	if _, ok := Lookup("", "temp-model"); !ok {
		t.Fatal("Register did not create entry")
	}

	ResetToBuiltIn()

	if _, ok := Lookup("", "temp-model"); ok {
		t.Error("temp-model still in registry after ResetToBuiltIn")
	}
	if _, ok := Lookup("", "claude-haiku-4-5"); !ok {
		t.Error("built-in claude-haiku-4-5 missing after ResetToBuiltIn")
	}
}

func TestRegisterLowercasesKey(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	Register("", "My-Model", Model{ContextWindow: 100_000})

	if _, ok := Lookup("", "my-model"); !ok {
		t.Error("Lookup with lowercase key failed after Register with mixed-case key")
	}
	if _, ok := Lookup("", "My-Model"); !ok {
		t.Error("Lookup with original-case key failed after Register")
	}
}

func TestConcurrentAccessNoRace(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			Register("", "concurrent-test", Model{ContextWindow: i})
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = ContextWindow("claude-haiku-4-5")
			_ = flatCost("claude-haiku-4-5", 100, 50, 0, 0)
			_, _, _ = Capabilities("claude-haiku-4-5")
			_ = Caching("claude-haiku-4-5")
		}
	}()

	wg.Wait()
}

func TestAccessorsWithProviderPrefix(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	Register("my-provider", "acc-test", Model{
		ContextWindow: 333_000,
		Effort:        true,
		Thinking:      true,
		Caching:       true,
		Rates:         Prices{Input: 3.00, Output: 9.00}.Rates(),
	})

	// ContextWindow with provider prefix.
	if got := ContextWindow("my-provider/acc-test"); got != 333_000 {
		t.Errorf("ContextWindow(\"my-provider/acc-test\") = %d, want 333000", got)
	}

	// Capabilities with provider prefix.
	effort, thinking, speed := Capabilities("my-provider/acc-test")
	if !effort || !thinking || speed {
		t.Errorf("Capabilities(\"my-provider/acc-test\") = (%v, %v, %v), want (true, true, false)", effort, thinking, speed)
	}

	// Caching with provider prefix.
	if got := Caching("my-provider/acc-test"); !got {
		t.Errorf("Caching(\"my-provider/acc-test\") = false, want true")
	}

	// Without provider prefix, now hits via the sole-provider fallback since
	// acc-test has exactly one registered provider entry.
	if got := ContextWindow("acc-test"); got != 333_000 {
		t.Errorf("ContextWindow(\"acc-test\") = %d, want 333000 (sole-provider fallback)", got)
	}
}

func TestDateSuffixWithProvider(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	Register("prov", "dated-model", Model{
		ContextWindow: 128_000,
		Rates:         Prices{Input: 1.00}.Rates(),
	})

	// Lookup with date suffix on the bare part should still match.
	m, ok := Lookup("prov", "dated-model-20260715")
	if !ok {
		t.Fatal("Lookup with date suffix should match after stripDateSuffix")
	}
	if m.ContextWindow != 128_000 {
		t.Errorf("ContextWindow = %d, want 128000", m.ContextWindow)
	}

	// Cost with date suffix in the full model string.
	cost := flatCost("prov/dated-model-20260715", 1_000_000, 0, 0, 0)
	if cost != 1.00 {
		t.Errorf("Cost = %v, want 1.0", cost)
	}

	// ContextWindow with date suffix.
	if got := ContextWindow("prov/dated-model-20260715"); got != 128_000 {
		t.Errorf("ContextWindow with date suffix = %d, want 128000", got)
	}
}

func TestTwoProvidersSameModel(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	Register("provider-a", "shared-model", Model{
		ContextWindow: 200_000,
		Rates:         Prices{Input: 1.00, Output: 5.00}.Rates(),
	})
	Register("provider-b", "shared-model", Model{
		ContextWindow: 400_000,
		Rates:         Prices{Input: 2.00, Output: 10.00}.Rates(),
	})

	// Each provider gets its own entry.
	mA, ok := Lookup("provider-a", "shared-model")
	if !ok || mA.Rates[ClassInput] != 1.00 || mA.ContextWindow != 200_000 {
		t.Errorf("provider-a entry = %+v, want InputPer1M=1.0 ContextWindow=200000", mA)
	}

	mB, ok := Lookup("provider-b", "shared-model")
	if !ok || mB.Rates[ClassInput] != 2.00 || mB.ContextWindow != 400_000 {
		t.Errorf("provider-b entry = %+v, want InputPer1M=2.0 ContextWindow=400000", mB)
	}

	// Cost routes to the right provider.
	costA := flatCost("provider-a/shared-model", 1_000_000, 0, 0, 0)
	costB := flatCost("provider-b/shared-model", 1_000_000, 0, 0, 0)
	if costA != 1.00 {
		t.Errorf("Cost provider-a = %v, want 1.0", costA)
	}
	if costB != 2.00 {
		t.Errorf("Cost provider-b = %v, want 2.0", costB)
	}

	// ContextWindow also routes correctly.
	if got := ContextWindow("provider-a/shared-model"); got != 200_000 {
		t.Errorf("ContextWindow provider-a = %d, want 200000", got)
	}
	if got := ContextWindow("provider-b/shared-model"); got != 400_000 {
		t.Errorf("ContextWindow provider-b = %d, want 400000", got)
	}
}

func TestStripPrefix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"claude/claude-sonnet-5", "claude-sonnet-5"},
		{"anthropic/claude-opus-4-6", "claude-opus-4-6"},
		{"google/gemini-2.5-flash", "gemini-2.5-flash"},
		{"claude-sonnet-5", "claude-sonnet-5"}, // no prefix → unchanged
		{"sonnet", "sonnet"},                   // bare alias → unchanged
		{"", ""},                               // empty → unchanged
		{"/weird", "/weird"},                   // leading slash (i>0 guard) → unchanged
	}
	for _, tt := range tests {
		if got := StripPrefix(tt.input); got != tt.want {
			t.Errorf("StripPrefix(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestSoleProviderFallback proves that a model registered under a single
// provider matches lookups regardless of which provider the query carries
// (or none). Provider only distinguishes the preferred entry when the model
// already matches.
func TestSoleProviderFallback(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)

	Register("zai-coding-plan", "syn-solo-model", Model{
		ContextWindow: 1_000_000,
		Rates:         Prices{Input: 0.0, Output: 0.0}.Rates(),
	})

	// No provider → sole entry matches.
	m, ok := Lookup("", "syn-solo-model")
	if !ok || m.ContextWindow != 1_000_000 {
		t.Errorf("Lookup(\"\", \"syn-solo-model\") = %+v ok=%v, want ContextWindow=1000000", m, ok)
	}

	// Different provider → sole entry matches.
	m, ok = Lookup("openrouter", "syn-solo-model")
	if !ok || m.ContextWindow != 1_000_000 {
		t.Errorf("Lookup(\"openrouter\", \"syn-solo-model\") = %+v ok=%v, want ContextWindow=1000000", m, ok)
	}

	// Cost also resolves via the bare model name.
	c := flatCost("syn-solo-model", 1_000_000, 0, 0, 0)
	if c != 0.0 {
		t.Errorf("flatCost(\"syn-solo-model\") = %v, want 0.0", c)
	}
}

// models.jsonl is append-only history; the live registry must key to the row
// with the latest `fetched`, regardless of file order, with an empty `fetched`
// (baseline) treated as oldest.
func TestBuildRegistryLatestFetchedWins(t *testing.T) {
	// Rows deliberately out of chronological order in the file, plus a baseline
	// row with no `fetched`. Newest date (3.0) must win.
	data := []byte(`{"id":"hist-model","provider":"openrouter","input_per_1m":1.0}
{"id":"hist-model","provider":"openrouter","input_per_1m":3.0,"fetched":"2026-03-01"}
{"id":"hist-model","provider":"openrouter","input_per_1m":2.0,"fetched":"2026-01-01"}`)
	reg, _, _, err := parseModelsJSONL(data)
	if err != nil {
		t.Fatalf("parseModelsJSONL: %v", err)
	}
	if got := reg["hist-model"][provKey("openrouter", "")].Rates[ClassInput]; got != 3.0 {
		t.Errorf("latest InputPer1M = %v, want 3.0 (max fetched wins regardless of file order)", got)
	}
}

// On equal `fetched`, the later file line wins (append order).
func TestBuildRegistryTieBreaksByFileOrder(t *testing.T) {
	data := []byte(`{"id":"tie","provider":"","input_per_1m":1.0,"fetched":"2026-05-01"}
{"id":"tie","provider":"","input_per_1m":9.0,"fetched":"2026-05-01"}`)
	reg, _, _, err := parseModelsJSONL(data)
	if err != nil {
		t.Fatalf("parseModelsJSONL: %v", err)
	}
	if got := reg["tie"][provKey("", "")].Rates[ClassInput]; got != 9.0 {
		t.Errorf("tie InputPer1M = %v, want 9.0 (later line wins on equal fetched)", got)
	}
}

// TestLookupAsOfPicksHistoricalPrice is the core as-of-request-time pricing
// case for foci_todo #1407: a model with multiple dated rows (price changed
// over time) must resolve to whichever row was in effect at a GIVEN past
// timestamp, not the latest.
func TestLookupAsOfPicksHistoricalPrice(t *testing.T) {
	data := []byte(`{"id":"asof-model","provider":"openrouter","input_per_1m":1.0,"fetched":"2026-01-01"}
{"id":"asof-model","provider":"openrouter","input_per_1m":2.0,"fetched":"2026-03-01"}
{"id":"asof-model","provider":"openrouter","input_per_1m":4.0,"fetched":"2026-06-01"}`)
	reg, hist, known, err := parseModelsJSONL(data)
	if err != nil {
		t.Fatalf("parseModelsJSONL: %v", err)
	}
	// Sanity: registry (latest-only) picks the newest row.
	if got := reg["asof-model"][provKey("openrouter", "")].Rates[ClassInput]; got != 4.0 {
		t.Fatalf("registry InputPer1M = %v, want 4.0 (latest)", got)
	}

	swapRegistry(t, reg, hist, known)

	tests := []struct {
		name string
		at   time.Time
		want float64
	}{
		{"before any row falls back to earliest", time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC), 1.0},
		{"exactly on a fetched date uses that row", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), 2.0},
		{"between two rows uses the earlier (preceding) one", time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC), 2.0},
		{"on/after the latest row uses it", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), 4.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := lookupAsOf("openrouter", "asof-model", tt.at)
			if !ok {
				t.Fatal("LookupAsOf: not found")
			}
			if m.Rates[ClassInput] != tt.want {
				t.Errorf("lookupAsOf(%v).Rates[ClassInput] = %v, want %v", tt.at, m.Rates[ClassInput], tt.want)
			}
		})
	}
}

// TestCostAsOfUsesHistoricalPrice mirrors CostAsOf against the same
// multi-row history, verifying the computed dollar figure (not just the raw
// per-1M rate) reflects the price at the given timestamp.
func TestCostAsOfUsesHistoricalPrice(t *testing.T) {
	data := []byte(`{"id":"asof-cost-model","provider":"","input_per_1m":1.0,"output_per_1m":2.0,"fetched":"2026-01-01"}
{"id":"asof-cost-model","provider":"","input_per_1m":3.0,"output_per_1m":6.0,"fetched":"2026-05-01"}`)
	reg, hist, known, err := parseModelsJSONL(data)
	if err != nil {
		t.Fatalf("parseModelsJSONL: %v", err)
	}
	swapRegistry(t, reg, hist, known)

	// At a timestamp before the price change: 1M input @ $1.0 + 1M output @ $2.0 = $3.0.
	early := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if got := flatCostAt("asof-cost-model", early, 1_000_000, 1_000_000); got != 3.0 {
		t.Errorf("CostAsOf(early) = %v, want 3.0 (pre-change price)", got)
	}

	// At a timestamp after the price change: 1M input @ $3.0 + 1M output @ $6.0 = $9.0.
	late := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if got := flatCostAt("asof-cost-model", late, 1_000_000, 1_000_000); got != 9.0 {
		t.Errorf("CostAsOf(late) = %v, want 9.0 (post-change price)", got)
	}

	// Priced now, the latest row applies.
	if got := flatCost("asof-cost-model", 1_000_000, 1_000_000, 0, 0); got != 9.0 {
		t.Errorf("CostAsOf(now) = %v, want 9.0", got)
	}
}

// flatCostAt prices input and output tokens on model at `at`.
func flatCostAt(model string, at time.Time, in, out int) float64 {
	usd, _ := CostAsOf(model, at, Tokens{ClassInput: in, ClassOutput: out})
	return usd
}

// lookupAsOf is the test's view of the as-of history lookup that
// ResolveRateModel/RatesAsOf price from: the model attributes effective AT THE
// GIVEN TIME `at` —
// the latest models.jsonl row for (provider, modelID) whose `fetched` date is
// on or before at's UTC date — rather than Lookup's always-latest-known
// price. Falls back to the earliest available row if `at` predates every
// dated row (baseline/no-fetched rows always qualify, per the `history` var
// doc). ok is false if there is no history at all under this (provider,
// bare) key. See the `history` var doc for the day-granularity/
// observation-date caveats: this is a best-effort reconstruction from the
// data models.jsonl actually records, not an exact historical price.
func lookupAsOf(provider, modelID string, at time.Time) (Model, bool) {
	segs, bare := splitSegs(modelID)
	if p := strings.ToLower(provider); p != "" {
		segs[p] = true
	}
	historyMu.RLock()
	defer historyMu.RUnlock()
	r, ok := historyLookupAsOfSegs(segs, bare, at)
	return r.row.model, ok
}

// TestProviderQualifiedOpencodeIDResolvesToBareOverride: the opencode backend
// reports "zai-coding-plan/glm-5.3" (#2264). The operator's $0 override is a
// providerless [[modelinfo]] row keyed "glm-5.3", alongside the built-in
// openrouter/z-ai row. The qualified id must resolve to the override exactly as
// the bare id does: priced at $0 (not unpriced, cf. #2172, nor the OpenRouter
// rate) with the override's context window.
func TestProviderQualifiedOpencodeIDResolvesToBareOverride(t *testing.T) {
	t.Cleanup(ResetToBuiltIn)
	Register("", "glm-5.3", Model{
		ContextWindow: 1_000_000,
		Rates:         Prices{CacheReadSet: true, CacheWriteSet: true}.Rates(),
	})

	for _, id := range []string{"glm-5.3", "zai-coding-plan/glm-5.3"} {
		usd, priced := CostAsOf(id, time.Now(), Tokens{
			ClassInput: 1_000_000, ClassOutput: 1_000_000, ClassCacheRead: 1_000_000, ClassCacheWrite: 1_000_000,
		})
		if !priced || usd != 0 {
			t.Errorf("CostAsOf(%q) = $%v priced=%v, want $0 priced", id, usd, priced)
		}
		if got := ContextWindow(id); got != 1_000_000 {
			t.Errorf("ContextWindow(%q) = %d, want 1000000", id, got)
		}
	}
}
