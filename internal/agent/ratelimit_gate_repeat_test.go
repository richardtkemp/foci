package agent

import (
	"testing"
	"time"

	"foci/internal/ratelimit"
)

// TestEngageRateLimit_RepeatedRejectionNotifiesOnce: CC repeats a rejected
// rate_limit_event while it retries (#2488). Re-closing the gate to the same
// deadline notifies once; a new deadline notifies again.
func TestEngageRateLimit_RepeatedRejectionNotifiesOnce(t *testing.T) {
	t.Parallel()
	var notices []time.Time
	ag := &Agent{
		Endpoint:      "test-endpoint",
		RateLimitFunc: HookList[func(time.Time)]{func(reset time.Time) { notices = append(notices, reset) }},
	}
	until := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	for i := 0; i < 3; i++ {
		ag.EngageRateLimit(ratelimit.Signal{Kind: ratelimit.KindUsage, ResetAt: until})
	}
	if len(notices) != 1 {
		t.Fatalf("notices for one repeated rejection = %d, want 1", len(notices))
	}
	later := until.Add(time.Hour)
	ag.EngageRateLimit(ratelimit.Signal{Kind: ratelimit.KindUsage, ResetAt: later})
	if len(notices) != 2 || !notices[1].Equal(later) {
		t.Errorf("notices = %v, want a second one for the new deadline %v", notices, later)
	}
}
