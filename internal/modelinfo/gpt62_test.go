package modelinfo

import (
	"math"
	"testing"
	"time"
)

// The gpt-6.2 models appeared on OpenRouter on 2026-11-17 and codex used
// gpt-6.2-sol on 2026-11-25 before models.jsonl had a row for it: every call
// was booked with no rate_model and a NULL cost (foci_todo #3272). Rates are
// OpenRouter's of 2026-11-25; a prompt over 272,000 tokens is priced in its
// own tier group. Only dated prices are pinned: a later sync appends rows with
// a later date, so it cannot change them.
func TestGPT62Rates(t *testing.T) {
	// Before the rows' fetched date, after it: the first row of a group is in
	// effect from the beginning, so the calls already booked price too.
	for _, day := range []string{"2026-11-18T12:00:00Z", "2026-11-25T12:16:38Z"} {
		at, err := time.Parse(time.RFC3339, day)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			id                      string // as booked
			group                   string
			in, out, read, write    float64
			tin, tout, tread, twrit float64
		}{
			{"codex/gpt-6.2-sol", "gpt-6.2-sol|openrouter|openai", 2, 10, 0.1, 2.5, 4, 15, 0.2, 5},
			{"openrouter/openai/gpt-6.2-sol", "gpt-6.2-sol|openrouter|openai", 2, 10, 0.1, 2.5, 4, 15, 0.2, 5},
			{"gpt-6.2-sol:batch", "gpt-6.2-sol:batch|openrouter|openai", 1, 5, 0.05, 1.25, 2, 7.5, 0.1, 2.5},
			{"gpt-6.2-sol-pro", "gpt-6.2-sol-pro|openrouter|openai", 2, 10, 0.1, 2.5, 4, 15, 0.2, 5},
			{"gpt-6.2-sol-pro:batch", "gpt-6.2-sol-pro:batch|openrouter|openai", 1, 5, 0.05, 1.25, 2, 7.5, 0.1, 2.5},
			{"codex/gpt-6.2-astra", "gpt-6.2-astra|openrouter|openai", 8, 40, 0.4, 10, 16, 60, 0.8, 20},
			{"gpt-6.2-astra:batch", "gpt-6.2-astra:batch|openrouter|openai", 4, 20, 0.2, 5, 8, 30, 0.4, 10},
			{"gpt-6.2-astra-pro", "gpt-6.2-astra-pro|openrouter|openai", 8, 40, 0.4, 10, 16, 60, 0.8, 20},
			{"gpt-6.2-astra-pro:batch", "gpt-6.2-astra-pro:batch|openrouter|openai", 4, 20, 0.2, 5, 8, 30, 0.4, 10},
		} {
			check := func(prompt int, wantGroup string, rates map[Class]float64) {
				t.Helper()
				if rm, ok := ResolveRateModel(tc.id, at, prompt); !ok || rm != wantGroup {
					t.Errorf("%s prompt %d at %s: rate group %q (ok=%v), want %q", tc.id, prompt, day, rm, ok, wantGroup)
					return
				}
				for class, want := range rates {
					usd, priced := CostAsOfPrompt(tc.id, at, Tokens{class: 1_000_000}, prompt)
					if !priced || math.Abs(usd-want) > 1e-9 {
						t.Errorf("%s prompt %d at %s: 1M %s = $%v (priced=%v), want $%v", tc.id, prompt, day, class, usd, priced, want)
					}
				}
			}
			check(272_000, tc.group, map[Class]float64{
				ClassInput: tc.in, ClassOutput: tc.out, ClassCacheRead: tc.read, ClassCacheWrite: tc.write})
			check(272_001, tc.group+"|>272000", map[Class]float64{
				ClassInput: tc.tin, ClassOutput: tc.tout, ClassCacheRead: tc.tread, ClassCacheWrite: tc.twrit})
		}
	}
}
