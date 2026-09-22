package modelinfo

import "testing"

// TestFamilyPricingUsesNewestInFamily proves familyPricing's canonical member
// tracks the newest registered version rather than a hand-picked literal
// (foci_todo #1967). claude-sonnet-4-5 (the old literal, $3/$15, 200k ctx) and
// claude-sonnet-5 (the newest sonnet in the registry, $2/$10, 1M ctx) price
// DIFFERENTLY, so this is a real discriminator — unlike opus's canonical,
// whose old and new rates happen to coincide and would pass either way.
//
// Before the fix, familyPricing hardcoded "claude-sonnet-4-5" and this test
// failed (got the 4-5 rate for an unregistered sonnet id). After routing
// through newestInFamilyLocked, an unregistered sonnet variant prices off
// claude-sonnet-5, matching this test.
func TestFamilyPricingUsesNewestInFamily(t *testing.T) {
	want := Cost("claude-sonnet-5", 1_000_000, 1_000_000, 0, 0)
	stale := Cost("claude-sonnet-4-5", 1_000_000, 1_000_000, 0, 0)
	if want == stale {
		t.Fatalf("test fixture is not discriminating: claude-sonnet-5 and "+
			"claude-sonnet-4-5 price the same (%v) — pick different models.jsonl rows", want)
	}

	got := Cost("claude-sonnet-9-9", 1_000_000, 1_000_000, 0, 0) // no exact row → family fallback
	if got != want {
		t.Errorf("Cost(unregistered sonnet variant) = %v, want %v (newest-in-family, "+
			"claude-sonnet-5's rate) — got the stale claude-sonnet-4-5 rate %v instead", got, want, stale)
	}
}

// TestContextWindowUsesNewestInFamily is ContextWindow's counterpart to the
// pricing test above: claude-sonnet-4-5 (old literal) carries a 200k context
// window while claude-sonnet-5 (newest) carries 1M, so an unregistered sonnet
// variant must inherit the newest figure, not the stale one (foci_todo #1967).
func TestContextWindowUsesNewestInFamily(t *testing.T) {
	want := ContextWindow("claude-sonnet-5")
	stale := ContextWindow("claude-sonnet-4-5")
	if want == stale {
		t.Fatalf("test fixture is not discriminating: claude-sonnet-5 and "+
			"claude-sonnet-4-5 report the same context window (%d)", want)
	}

	got := ContextWindow("claude-sonnet-9-9") // no exact row → family fallback
	if got != want {
		t.Errorf("ContextWindow(unregistered sonnet variant) = %d, want %d "+
			"(newest-in-family) — got the stale figure %d instead", got, want, stale)
	}
}
