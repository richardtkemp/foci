package modelinfo

import (
	"testing"
)

// resetUnpricedSeen / resetAmbiguousSeen clear the once-per-model dedup maps so
// a test can re-observe a hook fire. Test-only, so they live here (the package
// build has no non-test caller).
func resetUnpricedSeen() {
	unpricedMu.Lock()
	unpricedSeen = map[string]bool{}
	unpricedMu.Unlock()
}

func resetAmbiguousSeen() {
	ambiguousMu.Lock()
	ambiguousSeen = map[string]bool{}
	ambiguousMu.Unlock()
}

// cannedDevRegistry is the difficult, various canned data confirmed with Dick
// for the dev-aware lookup. input_per_1m doubles as an identity marker so a
// Cost() assertion reveals WHICH entry was picked. It stresses:
//   - a same-leaf / same-provider / different-dev collision (nova-1: acme vs
//     zenith) — impossible to represent in the old [leaf][provider] map, so it
//     proves the (provider,dev) keying;
//   - a same-leaf / same-dev / different-provider pair (glm-5.2 via openrouter
//     vs zai-coding-plan) — provider disambiguation;
//   - a dot-in-leaf + :nitro variant (kimi-k2.5:nitro);
//   - a direct-vendor 2-segment form (anthropic/…) plus a date suffix.
const cannedDevRegistry = `
{"id":"kimi-k3","provider":"openrouter","dev":"moonshotai","input_per_1m":3.0}
{"id":"nova-1","provider":"openrouter","dev":"acme","input_per_1m":1.0}
{"id":"nova-1","provider":"openrouter","dev":"zenith","input_per_1m":2.0}
{"id":"glm-5.2","provider":"openrouter","dev":"z-ai","input_per_1m":0.78}
{"id":"glm-5.2","provider":"zai-coding-plan","dev":"z-ai","input_per_1m":0.50}
{"id":"claude-opus-4-8","provider":"openrouter","dev":"anthropic","input_per_1m":15.0}
{"id":"kimi-k2.5:nitro","provider":"openrouter","dev":"moonshotai","input_per_1m":0.60}
{"id":"grok-4","provider":"openrouter","dev":"x-ai","input_per_1m":5.0}
`

// withCannedDevRegistry swaps the package registry/history for the canned set
// for the duration of the test.
func withCannedDevRegistry(t *testing.T) {
	t.Helper()
	reg, hist, err := parseModelsJSONL([]byte(cannedDevRegistry))
	if err != nil {
		t.Fatalf("parseModelsJSONL(canned): %v", err)
	}
	savedReg, savedHist := registry, history
	registryMu.Lock()
	registry = reg
	registryMu.Unlock()
	historyMu.Lock()
	history = hist
	historyMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		registry = savedReg
		registryMu.Unlock()
		historyMu.Lock()
		history = savedHist
		historyMu.Unlock()
	})
}

// costMarker returns Cost for 1M input tokens only, which equals the picked
// entry's input_per_1m — i.e. it reveals which candidate the lookup chose.
func costMarker(model string) float64 {
	return Cost(model, 1_000_000, 0, 0, 0)
}

func TestDevAwareLookup(t *testing.T) {
	withCannedDevRegistry(t)

	// Capture ambiguity warnings (deterministic-pick-and-log cases).
	var ambiguous []string
	savedHook := AmbiguousModelHook
	AmbiguousModelHook = func(bare string) { ambiguous = append(ambiguous, bare) }
	t.Cleanup(func() { AmbiguousModelHook = savedHook })
	resetAmbiguousSeen()

	cases := []struct {
		name      string
		model     string
		want      float64
		wantAmbig bool // expect an ambiguity log for this leaf
	}{
		{"3seg host/dev/leaf", "openrouter/moonshotai/kimi-k3", 3.0, false},
		{"bare leaf fast path", "kimi-k3", 3.0, false},
		{"single candidate wrong dev still returns", "openrouter/wrongdev/kimi-k3", 3.0, false},
		{"collision by dev acme", "openrouter/acme/nova-1", 1.0, false},
		{"collision by dev zenith", "openrouter/zenith/nova-1", 2.0, false},
		{"collision no disambiguator", "nova-1", 1.0, true},
		{"collision dev-miss provider-both", "openrouter/ghost/nova-1", 1.0, true},
		{"multi-provider dev-match provider-tiebreak", "openrouter/z-ai/glm-5.2", 0.78, false},
		{"multi-provider provider-segment picks host", "zai-coding-plan/glm-5.2", 0.50, false},
		{"date suffix single candidate", "anthropic/claude-opus-4-8-20260528", 15.0, false},
		{"dot-in-leaf plus nitro variant", "openrouter/moonshotai/kimi-k2.5:nitro", 0.60, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ambiguous = nil
			resetAmbiguousSeen()
			got := costMarker(tc.model)
			if got != tc.want {
				t.Errorf("Cost(%q) marker = %v, want %v", tc.model, got, tc.want)
			}
			sawAmbig := len(ambiguous) > 0
			if sawAmbig != tc.wantAmbig {
				t.Errorf("Cost(%q) ambiguity-logged = %v, want %v (logged=%v)", tc.model, sawAmbig, tc.wantAmbig, ambiguous)
			}
		})
	}
}

// TestRealRegistry_MultiSegmentResolves is the regression for the original bug:
// a 3-segment OpenRouter id (host/dev/model) must resolve against the real
// built-in registry (keyed by the bare leaf) instead of missing on the
// leftover "dev/" prefix and falling through to the unpriced fallback rate.
func TestRealRegistry_MultiSegmentResolves(t *testing.T) {
	var unpriced []string
	savedHook := UnpricedModelHook
	UnpricedModelHook = func(bare string) { unpriced = append(unpriced, bare) }
	t.Cleanup(func() { UnpricedModelHook = savedHook })
	resetUnpricedSeen()

	const model = "openrouter/moonshotai/kimi-k3"
	// kimi-k3's real window is 1,048,576 — a registry hit, not the 200k default.
	if got := ContextWindow(model); got != 1_048_576 {
		t.Errorf("ContextWindow(%q) = %d, want 1048576 (real registry hit)", model, got)
	}
	// A registry hit must not trip the unpriced warning.
	_ = Cost(model, 1_000_000, 0, 0, 0)
	if len(unpriced) != 0 {
		t.Errorf("Cost(%q) tripped unpriced warning %v — the multi-segment id didn't resolve", model, unpriced)
	}
}

// TestDevAwareLookup_UnknownLeafFallsBack confirms an unknown leaf misses the
// registry and falls through to the family/unpriced path (case #12).
func TestDevAwareLookup_UnknownLeafFallsBack(t *testing.T) {
	withCannedDevRegistry(t)

	var unpriced []string
	savedHook := UnpricedModelHook
	UnpricedModelHook = func(bare string) { unpriced = append(unpriced, bare) }
	t.Cleanup(func() { UnpricedModelHook = savedHook })
	resetUnpricedSeen()

	// ghost-9 is in no family and no registry → unpriced warning + haiku-family
	// fallback (or 0 if the fallback itself isn't in the canned set).
	_ = costMarker("openrouter/foocorp/ghost-9")
	if len(unpriced) == 0 {
		t.Errorf("expected an unpriced warning for an unknown leaf, got none")
	}
	if unpriced[0] != "ghost-9" {
		t.Errorf("unpriced bare = %q, want %q", unpriced[0], "ghost-9")
	}
}
