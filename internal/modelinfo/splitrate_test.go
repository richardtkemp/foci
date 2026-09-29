package modelinfo

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// Claude Code's MAIN THREAD caches at 1h while its SUBAGENTS cache at 5m, and
// foci priced every write at the 1h rate. On opus-5 that gap is $10.00 - $6.25
// = $3.75/MTok, which reconciled to six decimals against four production
// divergence warnings (#1866).

func closeTo(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.9f, want %.9f", what, got, want)
	}
}

// writeCost prices n cache-write tokens of one class on model now.
func writeCost(model string, c Class, n int) float64 {
	usd, _ := CostAsOf(model, time.Now(), Tokens{c: n})
	return usd
}

func TestCostAsOf_PricesEachTTLAtItsOwnRate(t *testing.T) {
	const mtok = 1_000_000
	closeTo(t, writeCost("claude-opus-5", ClassCacheWrite5m, mtok), 6.25, "1M 5m writes")
	closeTo(t, writeCost("claude-opus-5", ClassCacheWrite1h, mtok), 10.00, "1M 1h writes")
}

func TestCostAsOf_UnknownTTLPricesAtTheHigherRate(t *testing.T) {
	// An unobserved TTL is not "5m". Pricing it at the 1h rate errs toward
	// over-charging and preserves the pre-split behaviour exactly — assuming
	// the cheaper rate is the mistake that caused the bug in the first place.
	const mtok = 1_000_000
	unknown := writeCost("claude-opus-5", ClassCacheWrite, mtok)
	closeTo(t, unknown, writeCost("claude-opus-5", ClassCacheWrite1h, mtok), "unknown-TTL writes")
	if unknown == writeCost("claude-opus-5", ClassCacheWrite5m, mtok) {
		t.Error("unknown must not price at the 5m rate — that is the bug, not the fix")
	}
}

func TestCostAsOf_SubagentModelsHaveRealOneHourRates(t *testing.T) {
	// claude-haiku-4-5 and claude-sonnet-4-5 carried NO 1h rate, so a
	// MAIN-THREAD 1h write on either fell back to the 5m figure and was
	// under-priced by 1.6x — the mirror image of #1866, and invisible because
	// it errs cheap. These are exactly the models the delegate skill sends
	// subagents to (#1704). Rates read off Anthropic's published table
	// 2026-09-11 (platform.claude.com/docs/en/build-with-claude/prompt-caching.md),
	// not derived from the 2x rule.
	const mtok = 1_000_000
	for _, c := range []struct {
		model          string
		want5m, want1h float64
	}{
		{"claude-haiku-4-5", 1.25, 2.00},
		{"claude-sonnet-4-5", 3.75, 6.00},
	} {
		closeTo(t, writeCost(c.model, ClassCacheWrite5m, mtok), c.want5m, c.model+" 5m")
		closeTo(t, writeCost(c.model, ClassCacheWrite1h, mtok), c.want1h, c.model+" 1h")
	}
}

// TestOneHourRateIsTwiceBaseInput is the invariant behind every 1h figure, and
// it replaces checking a hardcoded list of model ids (#1704).
//
// Anthropic states it as policy in the same doc that carries the price table:
// "1-hour cache write tokens are 2 times the base input tokens price". Measured
// 2026-09-11 across every registry row carrying both figures: 27 models, ZERO
// violations. So a row that breaks it is a bad sync or a typo, not a new
// pricing tier — and the failure names the row rather than leaving a 1.6x
// mispricing to be found by a divergence warning months later.
//
// A row with NO 1h rate is not a violation here: absence means "not recorded",
// and the 1h class falls back to the 5m figure. That gap is #1704's subject.
func TestOneHourRateIsTwiceBaseInput(t *testing.T) {
	var checked int
	for _, line := range strings.Split(string(builtInData), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e jsonlEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("models.jsonl is not parseable: %v", err)
		}
		if e.CacheWrite1hPer1M <= 0 || e.InputPer1M <= 0 {
			continue
		}
		checked++
		if want := e.InputPer1M * 2; math.Abs(e.CacheWrite1hPer1M-want) > 1e-9 {
			t.Errorf("%s (%s): 1h cache write $%.2f, want $%.2f (2x base input $%.2f)",
				e.ID, e.Provider, e.CacheWrite1hPer1M, want, e.InputPer1M)
		}
	}
	if checked < 20 {
		t.Fatalf("only %d rows carried a 1h rate — the test is not reading models.jsonl "+
			"as expected and would pass vacuously", checked)
	}
}
