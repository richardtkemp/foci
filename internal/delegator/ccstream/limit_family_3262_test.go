package ccstream

import (
	"testing"
	"time"

	"foci/internal/ratelimit"
)

// Texts CC 2.1.325 builds when the plan limit rejected a call and usage
// credits could not cover it (#3262). Each is built from the binary's own
// strings: the fixed openings in ccLimitPrefixes, the multi-word limit names
// ("team's shared budget", "org's monthly spend limit", "individual spend
// limit") and the " · resets <time>" suffix.
var limitFamilyNoReset = []string{
	"Your org is out of usage · add funds to continue",
	"Your org is out of usage · contact your admin",
	"Your seat type doesn't include usage credits",
	"Your seat type doesn't include usage",
	"Your usage allocation has been disabled by your admin",
	"Your group's usage limit is set to $0",
	"You're out of extra usage",
	"Your seat type doesn't include extra usage",
	"You're out of usage credits",
	"You've hit your team's shared budget",
	"You've hit your org's monthly spend limit · visit https://claude.ai/settings to raise it",
	"You've hit your individual spend limit",
}

// limitFrame is a top-level synthetic assistant frame carrying text.
func limitFrame(text string) *AssistantMessage {
	return &AssistantMessage{Message: BetaMessage{
		Model:   syntheticModel,
		Content: []ContentBlock{{Type: "text", Text: text}},
	}}
}

// TestOnAssistant_LimitFamilyClosesGate is #3262: every message of CC's limit
// family closes the rate-limit gate as a usage limit, is not delivered as
// reply text, and is not a model block.
func TestOnAssistant_LimitFamilyClosesGate(t *testing.T) {
	t.Parallel()
	all := append([]string{"You're out of usage credits · resets 3pm (Europe/London)"}, limitFamilyNoReset...)
	for _, text := range all {
		t.Run(text, func(t *testing.T) {
			t.Parallel()
			b := &Backend{}
			var texts []string
			applyHandler(b, &testHandler{OnText: func(s string) { texts = append(texts, s) }})
			var signals []ratelimit.Signal
			b.onSessionLimit = func(s ratelimit.Signal) { signals = append(signals, s) }
			blocked := false
			b.onModelBlocked = func(string, string) { blocked = true }

			b.OnAssistant(limitFrame(text))

			if len(signals) != 1 {
				t.Fatalf("gate signals = %d, want 1", len(signals))
			}
			if signals[0].Kind != ratelimit.KindUsage || signals[0].Detail != text {
				t.Errorf("signal = %+v, want KindUsage with the CC text as Detail", signals[0])
			}
			if blocked {
				t.Error("a limit message fired a model block")
			}
			if len(texts) != 0 {
				t.Errorf("OnText called %d times, want 0 (a limit is a signal, not a reply)", len(texts))
			}
		})
	}
}

// TestOnAssistant_OutOfCreditsResetIsParsed: the " · resets <time>" CC adds
// to "You're out of usage credits" (the plan window's reset) sets the gate's
// deadline.
func TestOnAssistant_OutOfCreditsResetIsParsed(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("tz unavailable: %v", err)
	}
	now := time.Date(2026, time.November, 26, 10, 0, 0, 0, loc)
	sig, ok := parseSessionLimitReset("You're out of usage credits · resets 3pm (Europe/London) · progress saved", now)
	if !ok {
		t.Fatal("reset clause not parsed")
	}
	if want := time.Date(2026, time.November, 26, 15, 0, 0, 0, loc); !sig.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %v, want %v", sig.ResetAt, want)
	}
}

// TestLimitFamilyNoReset_GateLiftsAfterUsageFallback shows what lifts the
// gate for a message with no reset time (org out of usage, seat type, admin
// disabled, group $0): the signal carries no ResetAt, so the shared policy
// closes the gate for its usage fallback (one hour) and then it reopens by
// itself. The next turn then reaches CC, which refuses again while the cause
// stands, and the gate closes for another hour. /limit-reset (#3043) lifts it
// at once.
func TestLimitFamilyNoReset_GateLiftsAfterUsageFallback(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.November, 26, 16, 0, 0, 0, time.UTC)
	for _, text := range limitFamilyNoReset {
		b := &Backend{}
		applyHandler(b, &testHandler{})
		var got *ratelimit.Signal
		b.onSessionLimit = func(s ratelimit.Signal) { got = &s }
		b.OnAssistant(limitFrame(text))
		if got == nil {
			t.Fatalf("%q: no gate signal", text)
		}
		if !got.ResetAt.IsZero() || got.RetryAfter != 0 {
			t.Errorf("%q: signal has a reset hint %+v, want none", text, got)
		}
		res := ratelimit.Resolve(now, *got, 0)
		if want := now.Add(time.Hour); !res.Until.Equal(want) {
			t.Errorf("%q: gate until %v, want %v (the one-hour usage fallback)", text, res.Until, want)
		}
	}
}

// TestOnAssistant_LimitFamilyNeedsSyntheticModel: a real reply that quotes a
// family message is a reply (#1238).
func TestOnAssistant_LimitFamilyNeedsSyntheticModel(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	var texts []string
	applyHandler(b, &testHandler{OnText: func(s string) { texts = append(texts, s) }})
	fired := false
	b.onSessionLimit = func(ratelimit.Signal) { fired = true }
	b.OnAssistant(&AssistantMessage{Message: BetaMessage{
		Model:   "claude-opus-4-20250514",
		Content: []ContentBlock{{Type: "text", Text: "You're out of usage credits"}},
	}})
	if fired {
		t.Error("a real reply quoting the text closed the gate")
	}
	if len(texts) == 0 {
		t.Error("a real reply was not delivered")
	}
}

// TestIsCCLimitText: CC's rule is a prefix match; the text must start with the
// opening, and the model-credits pattern counts too.
func TestIsCCLimitText(t *testing.T) {
	t.Parallel()
	for _, s := range append([]string{fableCreditsText, "You've hit your session limit"}, limitFamilyNoReset...) {
		if !isCCLimitText(s) {
			t.Errorf("not matched: %q", s)
		}
	}
	for _, s := range []string{
		"Note: You're out of usage credits",
		"You've used 90% of your weekly limit",
		"Fast mode requires usage credits.",
		"",
	} {
		if isCCLimitText(s) {
			t.Errorf("matched %q, want not a limit message", s)
		}
	}
}
