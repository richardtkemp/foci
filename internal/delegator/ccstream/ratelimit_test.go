package ccstream

import (
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/ratelimit"
)

func TestOnRateLimit(t *testing.T) {
	var fires []string
	b := &Backend{rlThrottle: NewRateLimitThrottle()}
	b.onRateLimited = func(_, detail string) { fires = append(fires, detail) }

	resets := 1752349800.0
	warn := func(util float64) *RateLimitEvent {
		u := util
		return &RateLimitEvent{RateLimitInfo: RateLimitInfo{
			Status: "allowed_warning", RateLimitType: "five_hour", ResetsAt: &resets, Utilization: &u,
		}}
	}

	// "allowed" (under threshold) and nil never warn.
	b.OnRateLimit(&RateLimitEvent{RateLimitInfo: RateLimitInfo{Status: "allowed", RateLimitType: "five_hour"}})
	b.OnRateLimit(nil)
	if len(fires) != 0 {
		t.Fatalf("allowed/nil fired %d warnings, want 0", len(fires))
	}

	// #3145: a warning fires at each default usage warning level reached
	// (50..90 in tens, then 95..99), once; values between levels say nothing
	// new. The first event announces the highest level it has reached.
	b.OnRateLimit(warn(0.81))
	if len(fires) != 1 || !strings.Contains(fires[0], "Usage: 81%") {
		t.Fatalf("first warning at 81%%: fires %q, want one at 81%%", fires)
	}
	b.OnRateLimit(warn(0.82))
	b.OnRateLimit(warn(0.80))
	b.OnRateLimit(warn(0.89))
	if len(fires) != 1 {
		t.Fatalf("80-89 after level 80 fired %d, want 1", len(fires))
	}
	b.OnRateLimit(warn(0.90))
	b.OnRateLimit(warn(0.94))
	if len(fires) != 2 {
		t.Fatalf("level 90 fired %d, want 2", len(fires))
	}

	// At/above 95% every 1% level fires once.
	b.OnRateLimit(warn(0.95))
	b.OnRateLimit(warn(0.96))
	b.OnRateLimit(warn(0.96)) // same → throttled
	b.OnRateLimit(warn(0.97))
	if len(fires) != 5 {
		t.Fatalf("near-limit steps fired %d, want 5", len(fires))
	}

	// A status transition is a distinct series → fires.
	b.OnRateLimit(&RateLimitEvent{RateLimitInfo: RateLimitInfo{Status: "rejected", RateLimitType: "five_hour", ResetsAt: &resets}})
	if len(fires) != 6 {
		t.Fatalf("transition fired %d, want 6", len(fires))
	}

	// A new window (changed resetsAt) re-arms warnings.
	newResets := resets + 18000
	b.OnRateLimit(&RateLimitEvent{RateLimitInfo: RateLimitInfo{
		Status: "allowed_warning", RateLimitType: "five_hour", ResetsAt: &newResets, Utilization: ptrTo(0.50),
	}})
	if len(fires) != 7 {
		t.Fatalf("new window fired %d, want 7", len(fires))
	}
}

func ptrTo(f float64) *float64 { return &f }

// TestOnRateLimit_ConfiguredLevels (#3145): the stream warns at the levels
// HostHooks.UsageWarningLevels returns, read at each event, not CC's own
// steps; with none configured only a rejection fires.
func TestOnRateLimit_ConfiguredLevels(t *testing.T) {
	var fires []string
	levels := []int{70, 85}
	b := &Backend{rlThrottle: NewRateLimitThrottle()}
	b.SetHostHooks(delegator.HostHooks{
		OnRateLimitNotice:  func(_, d string) { fires = append(fires, d) },
		UsageWarningLevels: func() []int { return levels },
	})
	resets := 1752349800.0
	warn := func(util float64) {
		u := util
		b.OnRateLimit(&RateLimitEvent{RateLimitInfo: RateLimitInfo{
			Status: "allowed_warning", RateLimitType: "seven_day", ResetsAt: &resets, Utilization: &u,
		}})
	}
	warn(0.65)
	if len(fires) != 0 {
		t.Fatalf("65%% under every level fired %d, want 0", len(fires))
	}
	warn(0.75)
	warn(0.80) // a default level, not a configured one
	if len(fires) != 1 {
		t.Fatalf("75%% then 80%% fired %d, want 1 (level 70)", len(fires))
	}
	warn(0.96)
	warn(0.99) // CC's own 1% steps are not levels here
	if len(fires) != 2 {
		t.Fatalf("96%% then 99%% fired %d, want 2 (level 85 once)", len(fires))
	}

	levels = []int{} // "none", applied live
	resets += 7 * 86400
	warn(0.90)
	if len(fires) != 2 {
		t.Fatalf("levels off: 90%% fired %d, want still 2", len(fires))
	}
	b.OnRateLimit(&RateLimitEvent{RateLimitInfo: RateLimitInfo{Status: "rejected", RateLimitType: "seven_day", ResetsAt: &resets}})
	if len(fires) != 3 {
		t.Fatalf("levels off: a rejection fired %d total, want 3", len(fires))
	}
}

// TestOnRateLimit_LogsEveryEventInFull pins #2120: every rate_limit_event —
// including "allowed" ones and ones the throttle skips — leaves one DEBUG line
// carrying every rate_limit_info field, so an early/late warning (the server
// decides the threshold, surpassedThreshold) can be explained from foci.log.
func TestOnRateLimit_LogsEveryEventInFull(t *testing.T) {
	buf := captureDebugLog(t)
	b := &Backend{rlThrottle: NewRateLimitThrottle()}
	resets := 1789791000.0
	overageResets := 1789800000.0
	using := false

	b.OnRateLimit(&RateLimitEvent{RateLimitInfo: RateLimitInfo{Status: "allowed", RateLimitType: "five_hour", ResetsAt: &resets}})
	b.OnRateLimit(&RateLimitEvent{RateLimitInfo: RateLimitInfo{
		Status: "allowed_warning", RateLimitType: "five_hour", ResetsAt: &resets,
		Utilization: ptrTo(0.87), SurpassedThreshold: ptrTo(0.75),
		OverageStatus: "rejected", OverageResetsAt: &overageResets, IsUsingOverage: &using,
	}})

	out := buf.String()
	for _, want := range []string{
		"rate_limit_event received: status=allowed type=five_hour util=nil surpassedThreshold=nil resetsAt=1789791000 overageStatus= overageResetsAt=nil isUsingOverage=nil",
		"rate_limit_event received: status=allowed_warning type=five_hour util=0.8700 surpassedThreshold=0.7500 resetsAt=1789791000 overageStatus=rejected overageResetsAt=1789800000 isUsingOverage=false",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q\n--- log ---\n%s", want, out)
		}
	}
}

func TestFireRateLimited(t *testing.T) {
	b := &Backend{}
	// No hook set → no panic.
	b.fireRateLimited("x")

	var got string
	b.onRateLimited = func(_, detail string) { got = detail }
	b.fireRateLimited("You've hit your session limit · resets 10:30pm")
	if got != "You've hit your session limit · resets 10:30pm" {
		t.Errorf("hook got %q", got)
	}
}

// TestFireRateLimitedCarriesSessionKey pins #1857: the hook must receive the
// Backend's OWN foci session key (StartOptions.SessionKey), which is what lets
// the handler deliver the notice to the triggering chat instead of only the
// agent's default one. Before the fix the hook was func(detail string) and
// carried no session identity at all.
func TestFireRateLimitedCarriesSessionKey(t *testing.T) {
	b := &Backend{startOpts: delegator.StartOptions{SessionKey: "clutch/facet-7"}}

	var gotKey, gotDetail string
	b.onRateLimited = func(sessionKey, detail string) { gotKey, gotDetail = sessionKey, detail }
	b.fireRateLimited("⚠️ Approaching Anthropic 5-hour rate limit.")

	if gotKey != "clutch/facet-7" {
		t.Errorf("hook got session key %q, want %q — the notice cannot be routed to the triggering chat without it", gotKey, "clutch/facet-7")
	}
	if gotDetail != "⚠️ Approaching Anthropic 5-hour rate limit." {
		t.Errorf("hook got detail %q", gotDetail)
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
	if !got.ResetAt.Equal(want) {
		t.Errorf("reset = %s, want %s", got.ResetAt, want)
	}

	// A clock time already past today rolls to tomorrow.
	got2, ok := parseSessionLimitReset("resets 6:30am (Europe/London)", now)
	if !ok {
		t.Fatalf("expected parse to succeed")
	}
	want2 := time.Date(2026, 7, 15, 6, 30, 0, 0, loc)
	if !got2.ResetAt.Equal(want2) {
		t.Errorf("rolled reset = %s, want %s", got2.ResetAt, want2)
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
	var fired ratelimit.Signal
	b.onSessionLimit = func(signal ratelimit.Signal) { fired = signal }

	b.OnAssistant(&AssistantMessage{
		Message: BetaMessage{
			Model:   syntheticModel,
			Content: []ContentBlock{{Type: "text", Text: "You've hit your session limit · resets 11:30pm (Europe/London)"}},
		},
	})

	if fired.ResetAt.IsZero() {
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

func TestOnAssistant_SessionLimitWithoutResetUsesNeutralSignal(t *testing.T) {
	// Proves a definite session limit without a parseable reset is still
	// intercepted so the shared policy can apply its usage fallback.
	b := &Backend{}
	var texts []string
	applyHandler(b, &testHandler{OnText: func(s string) { texts = append(texts, s) }})
	var fired ratelimit.Signal
	b.onSessionLimit = func(signal ratelimit.Signal) { fired = signal }

	b.OnAssistant(&AssistantMessage{
		Message: BetaMessage{
			Model:   syntheticModel,
			Content: []ContentBlock{{Type: "text", Text: "You've hit your session limit (no reset given)"}},
		},
	})

	if fired.Kind != ratelimit.KindUsage || !fired.ResetAt.IsZero() {
		t.Errorf("signal = %+v, want usage limit without reset hint", fired)
	}
	if len(texts) != 0 {
		t.Errorf("OnText called %d times, want 0 (limit signal dropped)", len(texts))
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
