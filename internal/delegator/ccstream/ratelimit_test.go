package ccstream

import "testing"

func TestOnRateLimit(t *testing.T) {
	var fires []string
	b := &Backend{}
	b.onRateLimited = func(detail string) { fires = append(fires, detail) }

	util := 0.99
	resets := 1752349800.0
	warn := func() *RateLimitEvent {
		return &RateLimitEvent{RateLimitInfo: RateLimitInfo{
			Status: "allowed_warning", RateLimitType: "five_hour", ResetsAt: &resets, Utilization: &util,
		}}
	}

	// "allowed" (under threshold) and nil never warn.
	b.OnRateLimit(&RateLimitEvent{RateLimitInfo: RateLimitInfo{Status: "allowed", RateLimitType: "five_hour"}})
	b.OnRateLimit(nil)
	if len(fires) != 0 {
		t.Fatalf("allowed/nil fired %d warnings, want 0", len(fires))
	}

	// First warning fires; a repeat of the same (status,type,resetsAt) is deduped.
	b.OnRateLimit(warn())
	b.OnRateLimit(warn())
	if len(fires) != 1 {
		t.Fatalf("warning+dedup fired %d, want 1", len(fires))
	}

	// A status transition is a new key → fires again.
	b.OnRateLimit(&RateLimitEvent{RateLimitInfo: RateLimitInfo{Status: "rejected", RateLimitType: "five_hour", ResetsAt: &resets}})
	if len(fires) != 2 {
		t.Fatalf("transition fired %d, want 2", len(fires))
	}
}

func TestFireRateLimited(t *testing.T) {
	b := &Backend{}
	// No hook set → no panic.
	b.fireRateLimited("x")

	var got string
	b.onRateLimited = func(detail string) { got = detail }
	b.fireRateLimited("You've hit your session limit · resets 10:30pm")
	if got != "You've hit your session limit · resets 10:30pm" {
		t.Errorf("hook got %q", got)
	}
}

func TestRateLimitEventSummary(t *testing.T) {
	if s := rateLimitEventSummary(nil); s != "rate_limit_event=<nil>" {
		t.Errorf("nil summary = %q", s)
	}
	util := 0.97
	resets := 1752349800.0
	ev := &RateLimitEvent{RateLimitInfo: RateLimitInfo{
		Status:        "rejected",
		RateLimitType: "five_hour",
		ResetsAt:      &resets,
		Utilization:   &util,
		OverageStatus: "exhausted",
	}}
	s := rateLimitEventSummary(ev)
	for _, want := range []string{"rejected", "five_hour", "0.97", "exhausted"} {
		if !contains(s, want) {
			t.Errorf("summary %q missing %q", s, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
