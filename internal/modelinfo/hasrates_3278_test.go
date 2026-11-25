package modelinfo

import (
	"testing"
	"time"
)

// TestHasRates (#3278): HasRates agrees with ResolveRateModel and fires no
// hook, so asking about a listed model neither logs nor spends the
// once-per-model warning of its first call. The calls at the end are the
// control: the same models do fire the hooks when priced.
func TestHasRates(t *testing.T) {
	var fired []string
	prevU, prevF := UnpricedModelHook, FamilyPricedModelHook
	UnpricedModelHook = func(m string) { fired = append(fired, "unpriced "+m) }
	FamilyPricedModelHook = func(m string) { fired = append(fired, "family "+m) }
	t.Cleanup(func() { UnpricedModelHook, FamilyPricedModelHook = prevU, prevF })
	unpricedMu.Lock()
	delete(unpricedSeen, "hr3278-none")
	unpricedMu.Unlock()
	familyPricedMu.Lock()
	delete(familyPricedSeen, "claude-opus-9-8")
	familyPricedMu.Unlock()

	for _, tc := range []struct {
		model string
		want  bool
	}{
		{"codex/gpt-6.2-sol", true},
		{"gpt-6.2-sol", true},
		{"claude/claude-opus-9-8", true}, // its family's rates
		{"codex/<synthetic>", true},
		{"codex/HR3278-none", false},
	} {
		if got := HasRates(tc.model); got != tc.want {
			t.Errorf("HasRates(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
	if len(fired) != 0 {
		t.Errorf("HasRates fired %v", fired)
	}

	ResolveRateModel("codex/HR3278-none", time.Now(), 0)
	ResolveRateModel("claude/claude-opus-9-8", time.Now(), 0)
	if len(fired) != 2 || fired[0] != "unpriced hr3278-none" || fired[1] != "family claude-opus-9-8" {
		t.Errorf("hooks after pricing = %v", fired)
	}
}
