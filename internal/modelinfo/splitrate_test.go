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

func TestCostAsOfSplit_PricesEachTTLAtItsOwnRate(t *testing.T) {
	at := time.Now()
	const mtok = 1_000_000
	closeTo(t, CostAsOfSplit("claude-opus-5", at, 0, 0, 0, CacheWrites{Ephemeral5m: mtok}), 6.25, "1M 5m writes")
	closeTo(t, CostAsOfSplit("claude-opus-5", at, 0, 0, 0, CacheWrites{Ephemeral1h: mtok}), 10.00, "1M 1h writes")
}

func TestCostAsOfSplit_UnknownPricesAtTheHigherRate(t *testing.T) {
	// An unobserved TTL is not "5m". Pricing it at the 1h rate errs toward
	// over-charging and preserves the pre-split behaviour exactly — assuming
	// the cheaper rate is the mistake that caused the bug in the first place.
	at := time.Now()
	const mtok = 1_000_000
	unknown := CostAsOfSplit("claude-opus-5", at, 0, 0, 0, CacheWrites{Unknown: mtok})
	oneHour := CostAsOfSplit("claude-opus-5", at, 0, 0, 0, CacheWrites{Ephemeral1h: mtok})
	closeTo(t, unknown, oneHour, "unknown-TTL writes")
	if unknown == CostAsOfSplit("claude-opus-5", at, 0, 0, 0, CacheWrites{Ephemeral5m: mtok}) {
		t.Error("unknown must not price at the 5m rate — that is the bug, not the fix")
	}
}

func TestCostAsOf_IsCostAsOfSplitWithUnknown(t *testing.T) {
	// The flat entry point must remain byte-identical in behaviour, or every
	// caller that cannot observe a TTL silently changes price.
	at := time.Now()
	for _, model := range []string{"claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5", "claude-sonnet-4-5"} {
		flat := CostAsOf(model, at, 100, 200, 300, 400)
		split := CostAsOfSplit(model, at, 100, 200, 300, CacheWrites{Unknown: 400})
		closeTo(t, flat, split, model+" flat vs unknown-split")
	}
}

func TestCostAsOfSplit_SubagentModelsHaveRealOneHourRates(t *testing.T) {
	// claude-haiku-4-5 and claude-sonnet-4-5 carried NO 1h rate, so
	// cacheWriteRate fell back to the 5m figure and a MAIN-THREAD 1h write on
	// either was under-priced by 1.6x — the mirror image of #1866, and
	// invisible because it errs cheap. These are exactly the models the
	// delegate skill sends subagents to (#1704).
	//
	// This test previously asserted the OPPOSITE: that the split was a no-op
	// for them, with a note that adding a rate later should surface HERE as a
	// deliberate change rather than a silent price movement. It did exactly
	// that. Rates read off Anthropic's published table 2026-09-11
	// (platform.claude.com/docs/en/build-with-claude/prompt-caching.md), not
	// derived from the 2x rule.
	at := time.Now()
	const mtok = 1_000_000
	for _, c := range []struct {
		model          string
		want5m, want1h float64
	}{
		{"claude-haiku-4-5", 1.25, 2.00},
		{"claude-sonnet-4-5", 3.75, 6.00},
	} {
		closeTo(t, CostAsOfSplit(c.model, at, 0, 0, 0, CacheWrites{Ephemeral5m: mtok}), c.want5m, c.model+" 5m")
		closeTo(t, CostAsOfSplit(c.model, at, 0, 0, 0, CacheWrites{Ephemeral1h: mtok}), c.want1h, c.model+" 1h")
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
// and cacheWriteRate falls back to the 5m figure. That gap is #1704's subject.
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

// TestTTLSurcharge_NeverNegative is what lets a #1929 correction debit the
// parent CostUSD + TTLSurchargeUSD with no guard against a debit below the
// credit: the premium is non-negative by construction, even for a rate row
// whose 1h figure is (wrongly) below its 5m one. Before cacheWriteRate required
// 1h > 5m, such a row priced Unknown writes BELOW 5m and the premium went
// negative.
func TestTTLSurcharge_NeverNegative(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    Model
		want float64
	}{
		{"1h above 5m", Model{CacheWritePer1M: 6.25, CacheWrite1hPer1M: 10}, 3.75},
		{"no 1h figure", Model{CacheWritePer1M: 6.25}, 0},
		{"1h equal to 5m", Model{CacheWritePer1M: 6.25, CacheWrite1hPer1M: 6.25}, 0},
		{"1h below 5m (data error)", Model{CacheWritePer1M: 6.25, CacheWrite1hPer1M: 3}, 0},
	} {
		if got := tc.m.ttlPremium(); got != tc.want {
			t.Errorf("%s: ttlPremium = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestTTLSurchargeAsOf_OnlyFiveMinuteWritesCarryIt: 1h and Unknown writes price
// at the same rate the parent charged, so only the 5m ones were over-charged.
func TestTTLSurchargeAsOf_OnlyFiveMinuteWritesCarryIt(t *testing.T) {
	at := time.Now()
	const mtok = 1_000_000
	closeTo(t, TTLSurchargeAsOf("claude-opus-5", at, CacheWrites{Ephemeral5m: mtok}), 3.75, "1M 5m writes")
	if got := TTLSurchargeAsOf("claude-opus-5", at, CacheWrites{Ephemeral1h: mtok, Unknown: mtok}); got != 0 {
		t.Errorf("1h+Unknown writes surcharge = %v, want exactly 0", got)
	}
	// Agrees with pricing the same writes both ways, which is what the parent
	// and subagent rows actually hold.
	w := CacheWrites{Ephemeral5m: 300, Ephemeral1h: 700}
	both := CostAsOfSplit("claude-opus-5", at, 0, 0, 0, CacheWrites{Unknown: 1000}) -
		CostAsOfSplit("claude-opus-5", at, 0, 0, 0, w)
	closeTo(t, TTLSurchargeAsOf("claude-opus-5", at, w), both, "surcharge vs priced both ways")
}
