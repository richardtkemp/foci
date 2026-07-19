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
