package modelinfo

import (
	"math"
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

func TestCostAsOfSplit_ModelWithNoOneHourRateChargesTheSameEitherWay(t *testing.T) {
	// sonnet-4-5 and haiku-4-5 carry no 1h figure, so cacheWriteRate falls back
	// to the 5m rate and the split is a no-op for them. Asserted so that ADDING
	// a 1h rate later (#1704) shows up as a deliberate change here rather than
	// as a silent price movement.
	at := time.Now()
	const mtok = 1_000_000
	for _, model := range []string{"claude-sonnet-4-5", "claude-haiku-4-5"} {
		five := CostAsOfSplit(model, at, 0, 0, 0, CacheWrites{Ephemeral5m: mtok})
		hour := CostAsOfSplit(model, at, 0, 0, 0, CacheWrites{Ephemeral1h: mtok})
		closeTo(t, five, hour, model+" (no 1h rate in the registry)")
	}
}
