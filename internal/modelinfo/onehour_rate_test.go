package modelinfo

import "testing"

// The cacheWriteRate() unit tests above prove the LOGIC prefers a 1h rate and
// falls back to the 5m one. These two guard the DATA, which is where it actually
// went wrong: on 2026-08-09 fabulo's claude-fable-5 turns diverged 33.9% from the
// backend's own figure while every other model was quiet. Nothing was wrong with
// the code — claude-fable-5 simply had no cache_write_1h_per_1m on either of its
// rows, so it fell back to the 5m rate and understated cache writes by exactly
// 1.6x. Solving the observed turn confirmed it to six decimals: 141,117 write
// tokens at $12.50/M gives foci's $2.059273, and at $20/M gives the backend's
// $3.117650.
//
// A silent 1.6x underprice on the models we actually run is worth a test; a
// missing rate on the ~50 rows we never call is not, so neither test demands
// blanket coverage.

// TestOneHourRate_PresentForModelsWeRun pins a 1h cache-write rate onto the
// models this household actually sends delegated traffic to. Delegated Claude
// Code traffic is 100% 1h-TTL cache writes (measured: zero 5m writes in a month),
// so for these ids the 5m fallback is never the right answer — it is the bug.
func TestOneHourRate_PresentForModelsWeRun(t *testing.T) {
	for _, id := range []string{"claude-opus-5", "claude-sonnet-5", "claude-fable-5", "claude-fable-5-1"} {
		m, ok := Lookup("claude", id)
		if !ok {
			t.Errorf("%s: not in the registry at all", id)
			continue
		}
		if m.CacheWritePer1M > 0 && m.CacheWrite1hPer1M == 0 {
			t.Errorf("%s: has a 5m cache-write rate (%v) but no 1h rate, so cacheWriteRate() "+
				"falls back to 5m and understates every cached turn — this is exactly the "+
				"claude-fable-5 defect (33.9%% divergence, 2026-08-09)",
				id, m.CacheWritePer1M)
		}
	}
}

// TestOneHourRate_RefreshDoesNotDropIt guards the regression CLASS rather than
// the instance. models.jsonl keeps a baseline row plus dated refresh rows per id,
// and the newest `fetched` date wins — so a refresh that omits a field silently
// REMOVES a rate an earlier row supplied, with no parse error and no warning.
// Whenever any row for an id carries a 1h rate, the row that actually wins must
// carry one too.
func TestOneHourRate_RefreshDoesNotDropIt(t *testing.T) {
	everHad := map[string]bool{}
	for id, byProvider := range history {
		for _, rows := range byProvider {
			for _, r := range rows {
				if r.model.CacheWrite1hPer1M > 0 {
					everHad[id] = true
				}
			}
		}
	}
	if len(everHad) == 0 {
		t.Fatal("no row anywhere carries a 1h cache-write rate — the fixture is not loaded, " +
			"so this test would pass vacuously")
	}
	for id := range everHad {
		for _, m := range registry[id] {
			if m.CacheWrite1hPer1M == 0 && m.CacheWritePer1M > 0 {
				t.Errorf("%s: some row supplies a 1h cache-write rate but the winning row does "+
					"not — a dated refresh dropped the field, silently reverting this model to "+
					"5m pricing", id)
			}
		}
	}
}

// TestFable51CacheReadIsQuarterRate guards the one price in the registry that
// does NOT follow Anthropic's standard multipliers. Every other Claude model
// prices a cache hit at 0.10x base input; Fable 5.1 and Mythos 5.1 price it at
// 0.025x ($0.25/M against $10/M input) — footnoted on the pricing page, and
// confirmed by Dick on release day (2026-09-02). sync-modelinfo appends dated
// refresh rows from OpenRouter and the newest row wins, so a refresh carrying
// the conventional 0.1x figure would silently quadruple foci's cache-read cost
// on this model, with no parse error and no warning — and cache reads are the
// bulk of every delegated turn. Anchor the exception in a test, not a comment.
func TestFable51CacheReadIsQuarterRate(t *testing.T) {
	m, ok := Lookup("claude", "claude-fable-5-1")
	if !ok {
		t.Fatal("claude-fable-5-1: not in the registry at all")
	}
	if m.InputPer1M != 10 {
		t.Errorf("input rate = %v, want 10 — the 0.025x check below assumes it", m.InputPer1M)
	}
	if m.CacheReadPer1M != 0.25 {
		t.Errorf("cache-read rate = %v, want 0.25 (0.025x input). A value of %v would mean a "+
			"refresh applied the standard 0.1x multiplier that Fable/Mythos 5.1 are exempt from",
			m.CacheReadPer1M, m.InputPer1M*0.1)
	}
}
