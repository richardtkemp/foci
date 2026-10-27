package main

import (
	"slices"
	"testing"
)

func TestScopeFilter(t *testing.T) {
	all := scopeFilter("")
	if !all("anything") {
		t.Fatal("unset --only must keep every model in scope")
	}

	only := scopeFilter(" glm-5.3 ,qwen3.5-397b-a17b,")
	for id, want := range map[string]bool{
		"glm-5.3":                 true,
		"qwen3.5-397b-a17b":       true,
		"qwen3.5-397b-a17b:nitro": true, // a :nitro variant follows its base
		"glm-5.3-flash":           false,
		"glm-5":                   false,
	} {
		if got := only(id); got != want {
			t.Errorf("inScope(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestOnlyNotInAPI(t *testing.T) {
	api := map[string]orModel{"glm-5.3": {ID: "z-ai/glm-5.3"}}
	existing := map[string]bool{"hunter-alpha-old": true}
	got := onlyNotInAPI("glm-5.3,hunter-alpha,hunter-alpha-old", api, existing)
	if want := []string{"hunter-alpha"}; !slices.Equal(got, want) {
		t.Fatalf("onlyNotInAPI = %v, want %v", got, want)
	}
}

// TestWriteJSONLKeepsSameDateOrder guards the tie-break modelinfo relies on:
// among rows with the same id and `fetched`, the LATER line is the live one,
// so writeJSONL must never swap them.
func TestWriteJSONLKeepsSameDateOrder(t *testing.T) {
	var entries []jsonlEntry
	// Enough same-key rows that an unstable sort reliably reorders them.
	for i := range 40 {
		entries = append(entries, jsonlEntry{ID: "m", Fetched: "2026-07-19", InputPer1M: float64(i + 1)})
	}
	entries = append([]jsonlEntry{{ID: "z", Fetched: "2026-07-19"}}, entries...)
	path := t.TempDir() + "/models.jsonl"
	if err := writeJSONL(path, entries); err != nil {
		t.Fatal(err)
	}
	got, err := readJSONL(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		if want := float64(i + 1); got[i].InputPer1M != want {
			t.Fatalf("row %d of id m has input %v, want %v: same-date rows were reordered", i, got[i].InputPer1M, want)
		}
	}
}

// OpenRouter's time-of-day overrides carry no min_prompt_tokens. They must not
// become price tiers: a tier from 0 tokens makes modelinfo's init panic
// (models.jsonl sync 2026-10-27 broke every foci build).
func TestTimeOfDayOverridesAreNotTiers(t *testing.T) {
	var m orModel
	m.Pricing = orPricing{Prompt: "0.00000066", Completion: "0.00000198", Overrides: []orPricing{
		{Prompt: "0.00000132", Completion: "0.00000396"},                          // time-of-day: no min_prompt_tokens
		{MinPromptTokens: 200000, Prompt: "0.00000132", Completion: "0.00000396"}, // prompt-size tier
	}}
	f, _ := apiFields(m)
	if len(f.tiers) != 1 || f.tiers[0].MinPromptTokens != 200000 {
		t.Fatalf("tiers = %+v; want only the 200000-token tier", f.tiers)
	}
}
