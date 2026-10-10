package config

import "testing"

func TestNewFallbackResolver_NilOnEmptyMaps(t *testing.T) {
	// Proves that NewFallbackResolver returns nil when both global and
	// per-agent maps are empty, enabling a fast no-op check.
	fr := NewFallbackResolver(nil, nil, nil)
	if fr != nil {
		t.Fatal("expected nil resolver for empty maps")
	}
	fr = NewFallbackResolver(map[string]string{}, map[string]string{}, nil)
	if fr != nil {
		t.Fatal("expected nil resolver for empty (non-nil) maps")
	}
}

func TestFallbackResolver_BasicResolution(t *testing.T) {
	// Proves that a simple global fallback entry resolves correctly,
	// returning a ResolvedModel with the right developer, model ID,
	// and format.
	fr := NewFallbackResolver(
		map[string]string{"anthropic/claude-opus-4-6": "anthropic/claude-sonnet-4-6"},
		nil, nil,
	)
	if fr == nil {
		t.Fatal("expected non-nil resolver")
	}
	got := fr.Resolve("anthropic/claude-opus-4-6")
	if got == nil {
		t.Fatal("expected fallback for opus")
	}
	if got.Developer != "anthropic" || got.ModelID != "claude-sonnet-4-6" {
		t.Errorf("got %s/%s, want anthropic/claude-sonnet-4-6", got.Developer, got.ModelID)
	}
	if got.Format != "anthropic" {
		t.Errorf("got format %q, want anthropic", got.Format)
	}
}

func TestFallbackResolver_FullModelStrings(t *testing.T) {
	// Proves that fallback entries using full developer/model_id strings
	// resolve correctly in both keys and values.
	fr := NewFallbackResolver(
		map[string]string{"anthropic/claude-opus-4-6": "anthropic/claude-sonnet-4-6"},
		nil, nil,
	)
	if fr == nil {
		t.Fatal("expected non-nil resolver")
	}
	got := fr.Resolve("anthropic/claude-opus-4-6")
	if got == nil {
		t.Fatal("expected fallback for opus")
	}
	if got.Developer != "anthropic" || got.ModelID != "claude-sonnet-4-6" {
		t.Errorf("got %s/%s, want anthropic/claude-sonnet-4-6", got.Developer, got.ModelID)
	}
}

func TestFallbackResolver_ChainWalk(t *testing.T) {
	// Proves that fallback chains work: opus -> sonnet -> haiku,
	// where each Resolve returns the next hop (not the full chain).
	fr := NewFallbackResolver(
		map[string]string{
			"anthropic/claude-opus-4-6":   "anthropic/claude-sonnet-4-6",
			"anthropic/claude-sonnet-4-6": "anthropic/claude-haiku-4-5",
		},
		nil, nil,
	)
	if fr == nil {
		t.Fatal("expected non-nil resolver")
	}

	// First hop: opus -> sonnet
	got := fr.Resolve("anthropic/claude-opus-4-6")
	if got == nil || got.ModelID != "claude-sonnet-4-6" {
		t.Fatalf("first hop: got %v, want sonnet", got)
	}

	// Second hop: sonnet -> haiku
	got = fr.Resolve("anthropic/claude-sonnet-4-6")
	if got == nil || got.ModelID != "claude-haiku-4-5" {
		t.Fatalf("second hop: got %v, want haiku", got)
	}

	// Third hop: haiku -> nil (end of chain)
	got = fr.Resolve("anthropic/claude-haiku-4-5")
	if got != nil {
		t.Fatalf("third hop: got %v, want nil", got)
	}
}

func TestFallbackResolver_CycleDetection(t *testing.T) {
	// Proves that cycles in fallback maps are broken during construction
	// so that Resolve never enters an infinite loop.
	fr := NewFallbackResolver(
		map[string]string{
			"anthropic/claude-opus-4-6":   "anthropic/claude-sonnet-4-6",
			"anthropic/claude-sonnet-4-6": "anthropic/claude-opus-4-6",
		},
		nil, nil,
	)
	if fr == nil {
		t.Fatal("expected non-nil resolver (cycle should be broken, not discarded)")
	}

	// Walk the chain -- must terminate within MaxFallbackDepth
	model := "anthropic/claude-opus-4-6"
	for i := 0; i < MaxFallbackDepth+1; i++ {
		got := fr.Resolve(model)
		if got == nil {
			return // chain terminated -- no cycle
		}
		model = got.Developer + "/" + got.ModelID
	}
	t.Fatal("chain did not terminate -- cycle was not broken")
}

func TestFallbackResolver_PerAgentOverride(t *testing.T) {
	// Proves that per-agent fallback entries override global entries
	// for the same key, and global entries for other keys are preserved.
	global := map[string]string{
		"anthropic/claude-opus-4-6":   "anthropic/claude-sonnet-4-6",
		"anthropic/claude-sonnet-4-6": "anthropic/claude-haiku-4-5",
	}
	perAgent := map[string]string{
		"anthropic/claude-opus-4-6": "anthropic/claude-haiku-4-5", // override: skip sonnet
	}
	fr := NewFallbackResolver(global, perAgent, nil)
	if fr == nil {
		t.Fatal("expected non-nil resolver")
	}

	// Per-agent override: opus -> haiku (not sonnet)
	got := fr.Resolve("anthropic/claude-opus-4-6")
	if got == nil || got.ModelID != "claude-haiku-4-5" {
		t.Fatalf("opus fallback: got %v, want haiku", got)
	}

	// Global preserved: sonnet -> haiku
	got = fr.Resolve("anthropic/claude-sonnet-4-6")
	if got == nil || got.ModelID != "claude-haiku-4-5" {
		t.Fatalf("sonnet fallback: got %v, want haiku", got)
	}
}

func TestFallbackResolver_CrossEndpoint(t *testing.T) {
	// Proves that fallback works across different developers/endpoints,
	// e.g. Google model falling back to Anthropic model.
	fr := NewFallbackResolver(
		map[string]string{
			"google/gemini-2.5-pro": "anthropic/claude-sonnet-4-6",
		},
		nil, nil,
	)
	if fr == nil {
		t.Fatal("expected non-nil resolver")
	}
	got := fr.Resolve("google/gemini-2.5-pro")
	if got == nil {
		t.Fatal("expected fallback")
	}
	if got.Developer != "anthropic" || got.ModelID != "claude-sonnet-4-6" {
		t.Errorf("got %s/%s, want anthropic/claude-sonnet-4-6", got.Developer, got.ModelID)
	}
	if got.Format != "anthropic" {
		t.Errorf("got format %q, want anthropic", got.Format)
	}
	if got.Endpoint != "anthropic" {
		t.Errorf("got endpoint %q, want anthropic", got.Endpoint)
	}
}

func TestFallbackResolver_NoMatch(t *testing.T) {
	// Proves that Resolve returns nil for models without a fallback entry.
	fr := NewFallbackResolver(
		map[string]string{"anthropic/claude-opus-4-6": "anthropic/claude-sonnet-4-6"},
		nil, nil,
	)
	got := fr.Resolve("anthropic/claude-haiku-4-5")
	if got != nil {
		t.Fatalf("expected nil for unconfigured model, got %v", got)
	}
}

func TestFallbackResolver_NilReceiver(t *testing.T) {
	// Proves that calling Resolve on a nil *FallbackResolver returns nil
	// safely, supporting the nil-means-disabled pattern.
	var fr *FallbackResolver
	got := fr.Resolve("anthropic/claude-opus-4-6")
	if got != nil {
		t.Fatalf("expected nil from nil resolver, got %v", got)
	}
}

func TestFallbackResolver_InvalidKeysIgnored(t *testing.T) {
	// Proves that fallback entries with unparseable keys or values
	// (no slash, empty) are silently ignored rather than causing errors.
	fr := NewFallbackResolver(
		map[string]string{
			"badkey":                    "anthropic/claude-sonnet-4-6",
			"anthropic/claude-opus-4-6": "badvalue",
			"":                          "anthropic/claude-sonnet-4-6",
		},
		nil, nil,
	)
	// All entries had invalid keys or values -- resolver should be nil
	if fr != nil {
		t.Fatalf("expected nil resolver for all-invalid entries, got %+v", fr)
	}
}

func TestFallbackResolver_SelfCycleDetection(t *testing.T) {
	// Proves that a model mapping to itself is treated as a cycle and removed.
	fr := NewFallbackResolver(
		map[string]string{
			"anthropic/claude-opus-4-6": "anthropic/claude-opus-4-6",
		},
		nil, nil,
	)
	if fr != nil {
		t.Fatal("expected nil resolver for self-cycle")
	}
}

func TestFallbackResolver_CaseInsensitiveKeys(t *testing.T) {
	// Proves a [fallbacks] key spelled with different case still matches:
	// matching folds the whole developer/model_id, so "openrouter/Qwen/X"
	// in config fires for a session running "openrouter/qwen/x". Before
	// the fold the key never matched and the fallback silently never ran.
	fr := NewFallbackResolver(
		map[string]string{"openrouter/Qwen/X": "openrouter/qwen/y"},
		nil, nil,
	)
	if fr == nil {
		t.Fatal("expected non-nil resolver")
	}
	got := fr.Resolve("openrouter/qwen/x")
	if got == nil {
		t.Fatal("expected fallback for the lowercase spelling of a mixed-case key")
	}
	if got.Developer != "openrouter" || got.ModelID != "qwen/y" {
		t.Errorf("got %s/%s, want openrouter/qwen/y", got.Developer, got.ModelID)
	}
}

func TestFallbackResolver_ValueKeepsConfigCase(t *testing.T) {
	// Proves folding is for MATCHING only: the resolved fallback keeps the
	// value's config spelling — some providers' model ids are
	// case-sensitive, so the wire model must be exactly as written.
	fr := NewFallbackResolver(
		map[string]string{"openrouter/Qwen/X": "openrouter/DeepSeek/V4-Pro"},
		nil, nil,
	)
	got := fr.Resolve("openrouter/qwen/x")
	if got == nil {
		t.Fatal("expected fallback for the lowercase spelling of a mixed-case key")
	}
	if got.ModelID != "DeepSeek/V4-Pro" {
		t.Errorf("ModelID = %q, want the config spelling %q", got.ModelID, "DeepSeek/V4-Pro")
	}
}

func TestModelKey(t *testing.T) {
	// Proves ModelKey is the one match key used on both sides of every
	// model-config match: the canonical developer/model_id lowercased in
	// full, and the raw string lowercased when it does not resolve — so
	// two spellings differing only in case always share a key.
	if got := ModelKey("OpenRouter/Qwen/X"); got != "openrouter/qwen/x" {
		t.Errorf("ModelKey(mixed-case resolvable) = %q, want %q", got, "openrouter/qwen/x")
	}
	if got := ModelKey("OpenRouter-Qwen"); got != "openrouter-qwen" {
		t.Errorf("ModelKey(mixed-case unresolvable) = %q, want the raw string lowercased", got)
	}
}

func TestFallbackResolver_CaseFoldCycleBroken(t *testing.T) {
	// Proves a cycle that exists only under case folding (a/X → a/y,
	// a/Y → a/x) is broken at construction — the cycle walk must fold its
	// visited set and edge lookups like the resolver folds its keys, or
	// the two mixed-case spellings walk as distinct nodes and loop forever.
	fr := NewFallbackResolver(
		map[string]string{
			"a/X": "a/y",
			"a/Y": "a/x",
		},
		nil, nil,
	)
	if fr == nil {
		t.Fatal("expected non-nil resolver (cycle should be broken, not discarded)")
	}
	model := "a/X"
	for i := 0; i < MaxFallbackDepth+1; i++ {
		got := fr.Resolve(model)
		if got == nil {
			return // chain terminated — no cycle
		}
		model = got.Developer + "/" + got.ModelID
	}
	t.Fatal("chain did not terminate — case-fold cycle was not broken")
}

func TestFallbackResolver_CaseFoldCycleUppercaseValuesBroken(t *testing.T) {
	// Proves the cycle walk follows an edge through ModelKey, not the raw
	// value: here the VALUES carry the config case (a/x → a/Y, a/y → a/X),
	// so a raw walk stops at "a/Y" — not a key — and leaves the fold-only
	// cycle in place forever. The folded walk sees it and breaks it.
	fr := NewFallbackResolver(
		map[string]string{
			"a/x": "a/Y",
			"a/y": "a/X",
		},
		nil, nil,
	)
	if fr == nil {
		t.Fatal("expected non-nil resolver (cycle should be broken, not discarded)")
	}
	model := "a/x"
	for i := 0; i < MaxFallbackDepth+1; i++ {
		got := fr.Resolve(model)
		if got == nil {
			return // chain terminated — no cycle
		}
		model = got.Developer + "/" + got.ModelID
	}
	t.Fatal("chain did not terminate — case-fold cycle through uppercase values was not broken")
}

func TestFallbackResolver_FoldCollidingKeysDeterministic(t *testing.T) {
	// Proves two keys in ONE fallback table that differ only by case fold
	// to a single match key and the resolver picks a DETERMINISTIC winner —
	// the value of the key that sorts first — instead of whichever entry Go
	// map iteration happens to yield. Validate rejects such tables; this
	// keeps resolvers built outside validation from flipping between runs.
	const want = "openrouter/p/one" // value of "openrouter/a/X", which sorts before "openrouter/a/x"
	for i := 0; i < 20; i++ {
		fr := NewFallbackResolver(map[string]string{
			"openrouter/a/X": want,
			"openrouter/a/x": "openrouter/q/two",
		}, nil, nil)
		if fr == nil {
			t.Fatal("expected non-nil resolver")
		}
		got := fr.Resolve("openrouter/a/x")
		if got == nil {
			t.Fatal("expected a fallback for the folded key")
		}
		if m := got.Developer + "/" + got.ModelID; m != want {
			t.Fatalf("run %d: resolved %q, want %q on every run (deterministic winner)", i, m, want)
		}
	}
}
