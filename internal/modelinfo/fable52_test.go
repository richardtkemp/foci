package modelinfo

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Claude Fable 5.2 (claude-fable-5-2) shipped 2026-11-09 and became CC
// 2.1.321's default Fable model (foci_todo #3001). Rates read from
// platform.claude.com/docs/en/about-claude/pricing.md that day: input 8,
// 5m write 10, 1h write 16, cache read 0.20 (0.025x input, footnoted), output
// 40. Without its own row, a 5.2 call fell back to the fable family's
// canonical member, claude-fable-5-1 ($10/$50, read $0.25), and booked ~25%
// high.
func TestFable52Rates(t *testing.T) {
	at, _ := time.Parse(time.RFC3339, "2026-11-10T12:00:00Z")
	want := map[Class]float64{
		ClassInput:        8,
		ClassOutput:       40,
		ClassCacheRead:    0.20,
		ClassCacheWrite5m: 10,
		ClassCacheWrite1h: 16,
	}
	// The dash id is what CC and the API report; the dot spelling is the one
	// OpenRouter uses (as for claude-fable-5.1).
	for _, id := range []string{"claude-fable-5-2", "claude/claude-fable-5-2", "claude-fable-5.2"} {
		rm, ok := ResolveRateModel(id, at, 0)
		if !ok {
			t.Errorf("%s: unpriced", id)
			continue
		}
		// Must resolve to its OWN row, not a family fallback onto claude-fable-5-1.
		if leaf := strings.SplitN(rm, "|", 2)[0]; leaf != "claude-fable-5-2" && leaf != "claude-fable-5.2" {
			t.Errorf("%s: priced from rate group %q, want its own claude-fable-5-2 row", id, rm)
		}
		for class, rate := range want {
			usd, priced := CostAsOf(id, at, Tokens{class: 1_000_000})
			if !priced || math.Abs(usd-rate) > 1e-9 {
				t.Errorf("%s: 1M %s tokens = $%v (priced=%v), want $%v", id, class, usd, priced, rate)
			}
		}
	}
	if cw := ContextWindow("claude-fable-5-2"); cw != 1_000_000 {
		t.Errorf("ContextWindow(claude-fable-5-2) = %d, want 1000000", cw)
	}
}

// claude-fable-latest is a moving pointer with its own dated rows. From the day
// 5.2 shipped it must price as 5.2; before that it kept 5.1's rates, so ledger
// rows booked earlier are not repriced.
func TestFableLatestTracksNewest(t *testing.T) {
	for _, tc := range []struct {
		at        string
		wantInput float64
		wantRead  float64
	}{
		{"2026-11-08T12:00:00Z", 10, 0.25},
		{"2026-11-10T12:00:00Z", 8, 0.20},
	} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		in, _ := CostAsOf("claude-fable-latest", at, Tokens{ClassInput: 1_000_000})
		rd, _ := CostAsOf("claude-fable-latest", at, Tokens{ClassCacheRead: 1_000_000})
		if math.Abs(in-tc.wantInput) > 1e-9 || math.Abs(rd-tc.wantRead) > 1e-9 {
			t.Errorf("claude-fable-latest at %s: input $%v read $%v, want $%v / $%v",
				tc.at, in, rd, tc.wantInput, tc.wantRead)
		}
	}
	if id, ok := NewestInFamily("anthropic", "fable"); !ok || id != "claude-fable-5-2" {
		t.Errorf("NewestInFamily(anthropic, fable) = %q, %v; want claude-fable-5-2", id, ok)
	}
}
