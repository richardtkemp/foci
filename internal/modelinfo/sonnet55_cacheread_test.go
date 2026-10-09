package modelinfo

import (
	"math"
	"testing"
	"time"
)

// Sonnet 5.5 cache reads dropped from $0.20 to $0.10 per million on the
// pricing page (seen 2026-10-09; CC 2.1.296 made the same change). The rate
// history keeps both: calls before the new row price at 0.20, calls after at
// 0.10, so the ledger's past rows are not repriced.
func TestSonnet55CacheReadRateHistory(t *testing.T) {
	for _, tc := range []struct {
		at   string
		want float64
	}{
		{"2026-10-05T12:00:00Z", 0.20},
		{"2026-10-10T12:00:00Z", 0.10},
	} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		usd, priced := CostAsOf("claude-sonnet-5-5", at, Tokens{ClassCacheRead: 1_000_000})
		if !priced || math.Abs(usd-tc.want) > 1e-9 {
			t.Errorf("at %s: 1M cache-read tokens = $%v (priced=%v), want $%v", tc.at, usd, priced, tc.want)
		}
	}
}
