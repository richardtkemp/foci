package modelinfo

import (
	"testing"
	"time"
)

// #1966: an exact registry lookup misses when OpenRouter's models.jsonl
// spells a version with a dot (claude-opus-4.8) and the caller (Claude Code)
// reports it with a hyphen (claude-opus-4-8) — one character apart, same
// model, price already known. These tests lock in the fix: the punctuation
// retry must find it, must NOT fire FamilyPricedModelHook (it's an exact hit
// by another spelling, not a family fallback), and must still refuse rather
// than guess when punctuation-folding would collapse two genuinely different
// registry rows together.

// TestPunctuationWildcardMatchesRealCatalogue is the red/green case from the
// todo: before the fix, claude-opus-4-8 missed the exact lookup and silently
// inherited familyPricing's claude-opus-4-6 canonical (stale since
// 2026-02-04) instead of the real, already-known claude-opus-4.8 rates
// (fetched 2026-07-20: input 5, output 25, cache_read 0.5, cache_write 6.25,
// cache_write_1h 10).
func TestPunctuationWildcardMatchesRealCatalogue(t *testing.T) {
	want, ok := Lookup("", "claude-opus-4.8")
	if !ok {
		t.Fatal("setup: claude-opus-4.8 must exist in the real registry for this test to mean anything")
	}

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

	got, ok := Lookup("", "claude-opus-4-8")
	if !ok {
		t.Fatal("Lookup(\"claude-opus-4-8\") missed — punctuation wildcard did not fire")
	}
	if got != want {
		t.Errorf("Lookup(\"claude-opus-4-8\") = %+v, want %+v (claude-opus-4.8's real row)", got, want)
	}

	// A wildcard hit is an EXACT hit by another spelling — it must not warn as
	// a family fallback (that hook exists precisely to flag when the rate is
	// NOT known and was inherited from a canonical).
	if len(familyWarned) != 0 {
		t.Errorf("FamilyPricedModelHook fired for a punctuation-wildcard hit: %v", familyWarned)
	}
	if len(unpriced) != 0 {
		t.Errorf("UnpricedModelHook fired for a punctuation-wildcard hit: %v", unpriced)
	}

	// Cost must use the real rate too, not the family canonical.
	wantCost := want.InputPer1M // 1M input tokens => InputPer1M dollars
	if cost := Cost("claude-opus-4-8", 1_000_000, 0, 0, 0); cost != wantCost {
		t.Errorf("Cost(\"claude-opus-4-8\") = %v, want %v", cost, wantCost)
	}
}

// TestPunctuationWildcardNoMatchStillFallsThrough is the positive control:
// an id with NO match in either punctuation form must still fall through to
// familyPricing and still fire FamilyPricedModelHook — proving the wildcard
// retry doesn't accidentally swallow or suppress the existing fallback path.
func TestPunctuationWildcardNoMatchStillFallsThrough(t *testing.T) {
	var familyWarned []string
	FamilyPricedModelHook = func(m string) { familyWarned = append(familyWarned, m) }
	t.Cleanup(func() {
		FamilyPricedModelHook = nil
		familyPricedMu.Lock()
		familyPricedSeen = map[string]bool{}
		familyPricedMu.Unlock()
	})

	// No such version exists as either "claude-sonnet-9-9" or
	// "claude-sonnet-9.9" — the wildcard must miss on both and let the
	// existing family-keyword fallback ("sonnet" → claude-sonnet-4-5) fire.
	const model = "claude-sonnet-9-9"
	if _, ok := Lookup("", "claude-sonnet-9.9"); ok {
		t.Fatal("setup: claude-sonnet-9.9 unexpectedly exists in the real registry")
	}
	_ = Cost(model, 100, 0, 0, 0)

	if len(familyWarned) != 1 || familyWarned[0] != "claude-sonnet-9-9" {
		t.Errorf("FamilyPricedModelHook = %v, want a single fire for %q", familyWarned, model)
	}
}

// TestPunctuationWildcardDuplicateSpellingsResolve: when punctuation-folding
// collapses two DIFFERENT registry keys that hold the IDENTICAL model (the
// expected shape of a dot/hyphen duplicate — see #1966's constraints), the
// wildcard must still resolve, not refuse, even when the query's own spelling
// matches neither literal key exactly.
func TestPunctuationWildcardDuplicateSpellingsResolve(t *testing.T) {
	data := []byte(`{"id":"dup-4-9-2","provider":"openrouter","input_per_1m":7.0}
{"id":"dup-4.9.2","provider":"openrouter","input_per_1m":7.0}`)
	reg, hist, err := parseModelsJSONL(data)
	if err != nil {
		t.Fatalf("parseModelsJSONL: %v", err)
	}
	swapRegistry(t, reg, hist)

	// "dup-4-9.2" matches neither literal key exactly (mixed punctuation) but
	// folds to the same form as both.
	got, ok := Lookup("", "dup-4-9.2")
	if !ok {
		t.Fatal("Lookup missed a punctuation-folded duplicate — want it to resolve")
	}
	if got.InputPer1M != 7.0 {
		t.Errorf("InputPer1M = %v, want 7.0", got.InputPer1M)
	}
}

// TestPunctuationWildcardCollisionRefuses: when punctuation-folding would
// collapse two DIFFERENT registry rows (different prices — a genuine
// conflict, not a duplicate spelling of one model), the wildcard must refuse
// rather than guess. A refusal here means Lookup misses; Cost/familyPricing
// callers fall through exactly as they would for any other miss.
func TestPunctuationWildcardCollisionRefuses(t *testing.T) {
	data := []byte(`{"id":"coll-4-9-2","provider":"openrouter","input_per_1m":3.0}
{"id":"coll-4.9.2","provider":"openrouter","input_per_1m":9.0}`)
	reg, hist, err := parseModelsJSONL(data)
	if err != nil {
		t.Fatalf("parseModelsJSONL: %v", err)
	}
	swapRegistry(t, reg, hist)

	if _, ok := Lookup("", "coll-4-9.2"); ok {
		t.Error("Lookup resolved a genuine punctuation collision instead of refusing")
	}
	if _, ok := LookupAsOf("", "coll-4-9.2", time.Now()); ok {
		t.Error("LookupAsOf resolved a genuine punctuation collision instead of refusing")
	}
}

// TestPunctuationWildcardVariantSuffixInteraction covers the interaction the
// todo calls out explicitly: claude-opus-4-6[1m] is a real, separately-priced
// registry row (not a colon routing-variant — the "[1m]" is literal in the
// id). Punctuation-folding must still find it under a dotted spelling of the
// SAME suffixed id, but must NOT let an unrelated suffixed id (claude-opus-
// 4-8[1m], which has no registry row in either spelling) borrow claude-
// opus-4.8's base rate — the "[1m]" context-window pricing may differ and
// silently reusing the base rate would be exactly the wrong-price risk the
// ambiguity rule exists to prevent.
func TestPunctuationWildcardVariantSuffixInteraction(t *testing.T) {
	want, ok := Lookup("", "claude-opus-4-6[1m]")
	if !ok {
		t.Fatal("setup: claude-opus-4-6[1m] must exist in the real registry for this test to mean anything")
	}

	got, ok := Lookup("", "claude-opus-4.6[1m]")
	if !ok {
		t.Fatal("Lookup(\"claude-opus-4.6[1m]\") missed — dotted spelling of a suffixed id should still fold")
	}
	if got != want {
		t.Errorf("Lookup(\"claude-opus-4.6[1m]\") = %+v, want %+v", got, want)
	}

	var familyWarned []string
	FamilyPricedModelHook = func(m string) { familyWarned = append(familyWarned, m) }
	t.Cleanup(func() {
		FamilyPricedModelHook = nil
		familyPricedMu.Lock()
		familyPricedSeen = map[string]bool{}
		familyPricedMu.Unlock()
	})
	// No "claude-opus-4-8[1m]" or "claude-opus-4.8[1m]" row exists — must NOT
	// silently borrow claude-opus-4.8's base rate; must fall through to family
	// pricing instead.
	_ = Cost("claude-opus-4-8[1m]", 100, 0, 0, 0)
	if len(familyWarned) != 1 || familyWarned[0] != "claude-opus-4-8[1m]" {
		t.Errorf("FamilyPricedModelHook = %v, want a single fire for claude-opus-4-8[1m]", familyWarned)
	}
}

// swapRegistry installs a synthetic registry/history for the duration of a
// test, restoring the real one on cleanup. Mirrors withCannedDevRegistry
// (dev_lookup_test.go) but takes already-parsed maps so callers can build
// small, purpose-built fixtures inline.
func swapRegistry(t *testing.T, reg map[string]map[string]Model, hist map[string]map[string][]historyRow) {
	t.Helper()
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
