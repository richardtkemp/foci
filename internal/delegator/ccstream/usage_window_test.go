package ccstream

import (
	"testing"
	"time"

	"foci/internal/usagehold"
)

// TestOnRateLimit_ReportsUsageWindow is #2491: every five-hour and weekly
// rate_limit_event reaches the usage hold, allowed ones too (they clear).
func TestOnRateLimit_ReportsUsageWindow(t *testing.T) {
	b := &Backend{rlThrottle: NewRateLimitThrottle()}
	var evs []usagehold.Event
	b.onUsageWindow = func(ev usagehold.Event) { evs = append(evs, ev) }
	reset := float64(time.Now().Add(12 * time.Hour).Unix())
	u98, u20 := 0.98, 0.20
	b.OnRateLimit(&RateLimitEvent{RateLimitInfo: RateLimitInfo{Status: "allowed_warning", RateLimitType: "seven_day", Utilization: &u98, ResetsAt: &reset}})
	b.OnRateLimit(&RateLimitEvent{RateLimitInfo: RateLimitInfo{Status: "allowed", RateLimitType: "five_hour", Utilization: &u20, ResetsAt: &reset}})
	b.OnRateLimit(&RateLimitEvent{RateLimitInfo: RateLimitInfo{Status: "rejected", RateLimitType: "seven_day", ResetsAt: &reset}})
	if len(evs) != 3 {
		t.Fatalf("events = %+v, want 3", evs)
	}
	if evs[0].Window != "seven_day" || evs[0].Util != 0.98 || !evs[0].HasUtil || evs[0].Reached {
		t.Errorf("first = %+v, want weekly at 0.98", evs[0])
	}
	if evs[1].Window != "five_hour" || evs[1].Util != 0.20 {
		t.Errorf("second = %+v, want five-hour at 0.20", evs[1])
	}
	if !evs[2].Reached || evs[2].ResetAt.Unix() != int64(reset) {
		t.Errorf("third = %+v, want weekly reached", evs[2])
	}
}
