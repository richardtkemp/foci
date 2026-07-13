package ccstream

import "testing"

func TestLooksLikeRateLimit(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// The observed CC synthetic limit notices.
		{"You've hit your session limit · resets 10:30pm (Europe/London)", true},
		{"You've hit your usage limit · resets 9am (America/New_York)", true},
		{"Claude API rate limit reached — resets in a bit", true},
		// The OTHER synthetic CC emits — no "limit"+"reset" pair, must NOT match.
		{"There's an issue with the selected model (<synthetic>). It may not exist or you may not have access to it.", false},
		// A limit word without a reset, or a reset without a limit — neither matches.
		{"you have reached the session limit", false},
		{"the counter resets tomorrow", false},
		// Ordinary replies.
		{"", false},
		{"Sure, here's the session summary you asked for.", false},
		{"I reset the config and the limit is now 50.", false}, // "reset"+"limit" but not a limit-phrase
	}
	for _, tc := range cases {
		if got := looksLikeRateLimit(tc.in); got != tc.want {
			t.Errorf("looksLikeRateLimit(%q) = %v; want %v", tc.in, got, tc.want)
		}
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
