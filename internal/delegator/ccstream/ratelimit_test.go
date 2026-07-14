package ccstream

import (
	"testing"
	"time"
)

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

func TestFormatRateLimitNotice(t *testing.T) {
	util := 0.52
	warn := FormatRateLimitNotice(RateLimitInfo{
		Status: "allowed_warning", RateLimitType: "seven_day", Utilization: &util,
	})
	for _, want := range []string{"Approaching", "7-day", "52%"} {
		if !contains(warn, want) {
			t.Errorf("warning notice %q missing %q", warn, want)
		}
	}

	rej := FormatRateLimitNotice(RateLimitInfo{Status: "rejected", RateLimitType: "five_hour"})
	for _, want := range []string{"rejected", "5-hour"} {
		if !contains(rej, want) {
			t.Errorf("rejected notice %q missing %q", rej, want)
		}
	}
}

func TestParseSessionLimitReset(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("tz unavailable: %v", err)
	}
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, loc)

	got, ok := parseSessionLimitReset("You've hit your session limit · resets 11:30pm (Europe/London)", now)
	if !ok {
		t.Fatalf("expected parse to succeed")
	}
	want := time.Date(2026, 7, 14, 23, 30, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("reset = %s, want %s", got, want)
	}

	// A clock time already past today rolls to tomorrow.
	got2, ok := parseSessionLimitReset("resets 6:30am (Europe/London)", now)
	if !ok {
		t.Fatalf("expected parse to succeed")
	}
	want2 := time.Date(2026, 7, 15, 6, 30, 0, 0, loc)
	if !got2.Equal(want2) {
		t.Errorf("rolled reset = %s, want %s", got2, want2)
	}

	if _, ok := parseSessionLimitReset("no reset clause here", now); ok {
		t.Errorf("expected parse to fail on missing clause")
	}
}

func TestOnAssistant_SessionLimitFiresHookAndDrops(t *testing.T) {
	b := &Backend{}
	b.lastModel = "claude-opus-4-20250514"
	var texts []string
	applyHandler(b, &testHandler{OnText: func(s string) { texts = append(texts, s) }})
	var fired time.Time
	b.onSessionLimit = func(until time.Time) { fired = until }

	b.OnAssistant(&AssistantMessage{
		Message: BetaMessage{
			Model:   syntheticModel,
			Content: []ContentBlock{{Type: "text", Text: "You've hit your session limit · resets 11:30pm (Europe/London)"}},
		},
	})

	if fired.IsZero() {
		t.Fatalf("onSessionLimit did not fire")
	}
	if len(texts) != 0 {
		t.Errorf("OnText called %d times, want 0 (session-limit message dropped)", len(texts))
	}
	b.turnMu.Lock()
	tlen := b.turnText.Len()
	b.turnMu.Unlock()
	if tlen != 0 {
		t.Errorf("turnText len = %d, want 0", tlen)
	}
}

func TestOnAssistant_SessionLimitUnparsedFallsThrough(t *testing.T) {
	b := &Backend{}
	var texts []string
	applyHandler(b, &testHandler{OnText: func(s string) { texts = append(texts, s) }})
	fired := false
	b.onSessionLimit = func(time.Time) { fired = true }

	b.OnAssistant(&AssistantMessage{
		Message: BetaMessage{
			Model:   syntheticModel,
			Content: []ContentBlock{{Type: "text", Text: "You've hit your session limit (no reset given)"}},
		},
	})

	if fired {
		t.Errorf("onSessionLimit fired despite unparseable reset")
	}
	if len(texts) != 1 {
		t.Errorf("OnText called %d times, want 1 (unparseable message not dropped)", len(texts))
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
