package ccstream

import (
	"testing"
	"time"

	"foci/internal/ratelimit"
)

// TestOnRateLimit_RejectedWindowClosesGate is #2488: a rejected five-hour or
// weekly rate_limit_event closes the agent's gate until the window resets.
func TestOnRateLimit_RejectedWindowClosesGate(t *testing.T) {
	for _, typ := range []string{"seven_day", "five_hour"} {
		t.Run(typ, func(t *testing.T) {
			b := &Backend{rlThrottle: NewRateLimitThrottle()}
			var fired []ratelimit.Signal
			b.onSessionLimit = func(s ratelimit.Signal) { fired = append(fired, s) }
			resets := float64(time.Now().Add(5 * time.Hour).Unix())
			b.OnRateLimit(&RateLimitEvent{RateLimitInfo: RateLimitInfo{Status: "rejected", RateLimitType: typ, ResetsAt: &resets}})
			if len(fired) != 1 {
				t.Fatalf("gate engaged %d times, want 1", len(fired))
			}
			if want := time.Unix(int64(resets), 0); !fired[0].ResetAt.Equal(want) || fired[0].Kind != ratelimit.KindUsage {
				t.Errorf("signal = %+v, want a usage limit resetting at %v", fired[0], want)
			}
		})
	}
}

// TestOnRateLimit_WarningDoesNotCloseGate: only the rejection closes the gate;
// a warning stays a notice (#1238).
func TestOnRateLimit_WarningDoesNotCloseGate(t *testing.T) {
	b := &Backend{rlThrottle: NewRateLimitThrottle()}
	fired := 0
	b.onSessionLimit = func(ratelimit.Signal) { fired++ }
	u := 0.99
	resets := float64(time.Now().Add(time.Hour).Unix())
	b.OnRateLimit(&RateLimitEvent{RateLimitInfo: RateLimitInfo{Status: "allowed_warning", RateLimitType: "seven_day", Utilization: &u, ResetsAt: &resets}})
	if fired != 0 {
		t.Errorf("a warning engaged the gate %d times, want 0", fired)
	}
}

// TestOnAssistant_WeeklyLimitTextClosesGate is #2488: CC's synthetic weekly
// text closes the gate, with its dated reset clause parsed.
func TestOnAssistant_WeeklyLimitTextClosesGate(t *testing.T) {
	b := &Backend{}
	applyHandler(b, &testHandler{})
	var fired ratelimit.Signal
	b.onSessionLimit = func(s ratelimit.Signal) { fired = s }
	b.OnAssistant(&AssistantMessage{Message: BetaMessage{
		Model:   syntheticModel,
		Content: []ContentBlock{{Type: "text", Text: "You've hit your weekly limit · resets Oct 12, 12am (Europe/London)"}},
	}})
	if fired.Kind != ratelimit.KindUsage {
		t.Fatalf("weekly limit text did not engage the gate: %+v", fired)
	}
	loc, _ := time.LoadLocation("Europe/London")
	got := fired.ResetAt.In(loc)
	if got.Month() != time.October || got.Day() != 12 || got.Hour() != 0 || got.Minute() != 0 || !fired.ResetAt.After(time.Now()) {
		t.Errorf("reset = %v, want the next Oct 12 00:00 Europe/London", got)
	}
}

// TestOnAssistant_ReplyDiscussingLimitsDoesNotCloseGate keeps #1238: real
// model text about limits never closes the gate.
func TestOnAssistant_ReplyDiscussingLimitsDoesNotCloseGate(t *testing.T) {
	b := &Backend{}
	applyHandler(b, &testHandler{})
	fired := false
	b.onSessionLimit = func(ratelimit.Signal) { fired = true }
	b.OnAssistant(&AssistantMessage{Message: BetaMessage{
		Model:   "claude-opus-4-20250514",
		Content: []ContentBlock{{Type: "text", Text: "You may hit your weekly limit soon."}},
	}})
	if fired {
		t.Error("a real reply discussing limits engaged the gate")
	}
}

// TestParseSessionLimitReset_Dated covers the dated clause and the year roll.
func TestParseSessionLimitReset_Dated(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/London")
	now := time.Date(2026, time.December, 30, 10, 0, 0, 0, loc)
	sig, ok := parseSessionLimitReset("You've hit your weekly limit · resets Jan 2, 3:30pm (Europe/London)", now)
	if !ok {
		t.Fatal("dated clause not parsed")
	}
	if want := time.Date(2027, time.January, 2, 15, 30, 0, 0, loc); !sig.ResetAt.Equal(want) {
		t.Errorf("reset = %v, want %v", sig.ResetAt, want)
	}
}

// TestOnAssistant_FableLimitTextClosesGate is #3102: the Fable limit texts
// close the gate. The first case is the -p text CC 2.1.325 builds for a
// rejected seven_day_overage_included window (the 2.1.323 change "says when
// the limit resets"): "You've hit your " + "Fable limit" + " · resets <t>",
// then the headless suffix. The second is the older "reached" wording
// (2.1.314's usage-credits path), which the regex must also accept.
func TestOnAssistant_FableLimitTextClosesGate(t *testing.T) {
	cases := []struct {
		name, text string
		wantReset  bool
	}{
		{"cc-2.1.325 -p", "You've hit your Fable limit · resets Nov 20, 5pm (Europe/London). Switch to another model, or manage usage credits at https://claude.ai/settings/usage, to continue.", true},
		{"reached wording", "You've reached your Fable limit. Fable 5 requires usage credits. Switch to another model to continue.", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &Backend{}
			applyHandler(b, &testHandler{})
			var fired ratelimit.Signal
			b.onSessionLimit = func(s ratelimit.Signal) { fired = s }
			b.OnAssistant(&AssistantMessage{Message: BetaMessage{
				Model:   syntheticModel,
				Content: []ContentBlock{{Type: "text", Text: tc.text}},
			}})
			if fired.Kind != ratelimit.KindUsage {
				t.Fatalf("Fable limit text did not engage the gate: %+v", fired)
			}
			if !tc.wantReset {
				return
			}
			loc, _ := time.LoadLocation("Europe/London")
			got := fired.ResetAt.In(loc)
			if got.Month() != time.November || got.Day() != 20 || got.Hour() != 17 || got.Minute() != 0 {
				t.Errorf("reset = %v, want the next Nov 20 17:00 Europe/London", got)
			}
		})
	}
}

// TestOnAssistant_ReplyReachedLimitDoesNotCloseGate: the widened "reached"
// arm still needs the synthetic model; a real reply never closes the gate.
func TestOnAssistant_ReplyReachedLimitDoesNotCloseGate(t *testing.T) {
	b := &Backend{}
	applyHandler(b, &testHandler{})
	fired := false
	b.onSessionLimit = func(ratelimit.Signal) { fired = true }
	b.OnAssistant(&AssistantMessage{Message: BetaMessage{
		Model:   "claude-opus-4-20250514",
		Content: []ContentBlock{{Type: "text", Text: "You've reached your Fable limit, it seems."}},
	}})
	if fired {
		t.Error("a real reply mentioning a reached limit engaged the gate")
	}
}
