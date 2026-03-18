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

func TestFallbackResolver_AliasResolution(t *testing.T) {
	// Proves that aliases in both keys and values are resolved to
	// canonical form, so "opus" → "sonnet" works when aliases are set.
	aliases := map[string]string{
		"opus":   "anthropic/claude-opus-4-6",
		"sonnet": "anthropic/claude-sonnet-4-6",
	}
	fr := NewFallbackResolver(
		map[string]string{"opus": "sonnet"},
		nil, aliases,
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
	// Proves that fallback chains work: opus → sonnet → haiku,
	// where each Resolve returns the next hop (not the full chain).
	aliases := map[string]string{
		"opus":   "anthropic/claude-opus-4-6",
		"sonnet": "anthropic/claude-sonnet-4-6",
		"haiku":  "anthropic/claude-haiku-4-5",
	}
	fr := NewFallbackResolver(
		map[string]string{
			"opus":   "sonnet",
			"sonnet": "haiku",
		},
		nil, aliases,
	)
	if fr == nil {
		t.Fatal("expected non-nil resolver")
	}

	// First hop: opus → sonnet
	got := fr.Resolve("anthropic/claude-opus-4-6")
	if got == nil || got.ModelID != "claude-sonnet-4-6" {
		t.Fatalf("first hop: got %v, want sonnet", got)
	}

	// Second hop: sonnet → haiku
	got = fr.Resolve("anthropic/claude-sonnet-4-6")
	if got == nil || got.ModelID != "claude-haiku-4-5" {
		t.Fatalf("second hop: got %v, want haiku", got)
	}

	// Third hop: haiku → nil (end of chain)
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

	// Walk the chain — must terminate within MaxFallbackDepth
	model := "anthropic/claude-opus-4-6"
	for i := 0; i < MaxFallbackDepth+1; i++ {
		got := fr.Resolve(model)
		if got == nil {
			return // chain terminated — no cycle
		}
		model = got.Developer + "/" + got.ModelID
	}
	t.Fatal("chain did not terminate — cycle was not broken")
}

func TestFallbackResolver_PerAgentOverride(t *testing.T) {
	// Proves that per-agent fallback entries override global entries
	// for the same key, and global entries for other keys are preserved.
	aliases := map[string]string{
		"opus":   "anthropic/claude-opus-4-6",
		"sonnet": "anthropic/claude-sonnet-4-6",
		"haiku":  "anthropic/claude-haiku-4-5",
	}
	global := map[string]string{
		"opus":   "sonnet",
		"sonnet": "haiku",
	}
	perAgent := map[string]string{
		"opus": "haiku", // override: skip sonnet
	}
	fr := NewFallbackResolver(global, perAgent, aliases)
	if fr == nil {
		t.Fatal("expected non-nil resolver")
	}

	// Per-agent override: opus → haiku (not sonnet)
	got := fr.Resolve("anthropic/claude-opus-4-6")
	if got == nil || got.ModelID != "claude-haiku-4-5" {
		t.Fatalf("opus fallback: got %v, want haiku", got)
	}

	// Global preserved: sonnet → haiku
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
			"badkey":                      "anthropic/claude-sonnet-4-6",
			"anthropic/claude-opus-4-6": "badvalue",
			"":                           "anthropic/claude-sonnet-4-6",
		},
		nil, nil,
	)
	// All entries had invalid keys or values — resolver should be nil
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
