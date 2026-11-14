package ccstream

import (
	"testing"
	"time"

	"foci/internal/ratelimit"
)

// fableCreditsText is CC 2.1.325's -p text for a model the account may use
// only with usage credits (model_requires_usage_credits, #3106): the model
// name, "requires usage credits", then the headless suffix.
const fableCreditsText = "Fable 5 requires usage credits. Switch to another model, or manage usage credits at https://claude.ai/settings/usage, to continue."

// TestOnAssistant_ModelRequiresCreditsFiresModelBlock is #3106: the synthetic
// "<model> requires usage credits" frame reports a model block for this
// session, is not delivered as reply text, and does not close the rate-limit
// gate (it has no reset time).
func TestOnAssistant_ModelRequiresCreditsFiresModelBlock(t *testing.T) {
	b := &Backend{}
	b.startOpts.SessionKey = "clutch/c1"
	var texts []string
	applyHandler(b, &testHandler{OnText: func(s string) { texts = append(texts, s) }})
	gate := false
	b.onSessionLimit = func(ratelimit.Signal) { gate = true }
	var gotKey, gotDetail string
	b.onModelBlocked = func(sk, detail string) { gotKey, gotDetail = sk, detail }

	b.OnAssistant(&AssistantMessage{Message: BetaMessage{
		Model:   syntheticModel,
		Content: []ContentBlock{{Type: "text", Text: fableCreditsText}},
	}})

	if gotKey != "clutch/c1" || gotDetail != fableCreditsText {
		t.Fatalf("model block = (%q, %q), want (clutch/c1, the CC text)", gotKey, gotDetail)
	}
	if gate {
		t.Error("a model block closed the rate-limit gate")
	}
	if len(texts) != 0 {
		t.Errorf("OnText called %d times, want 0 (model block is a signal, not a reply)", len(texts))
	}
}

// TestOnAssistant_ModelBlockNeedsSyntheticModel: a real reply quoting the
// phrase is a reply, never a block (as #1238 for limits).
func TestOnAssistant_ModelBlockNeedsSyntheticModel(t *testing.T) {
	b := &Backend{}
	applyHandler(b, &testHandler{})
	fired := false
	b.onModelBlocked = func(string, string) { fired = true }
	b.OnAssistant(&AssistantMessage{Message: BetaMessage{
		Model:   "claude-opus-4-20250514",
		Content: []ContentBlock{{Type: "text", Text: fableCreditsText}},
	}})
	if fired {
		t.Error("a real reply quoting the credits text fired a model block")
	}
}

// TestOnAssistant_FableLimitWithCreditsClauseStaysALimit: the "reached your
// Fable limit" text also names "requires usage credits"; it is a limit and
// closes the gate, it does not block the model.
func TestOnAssistant_FableLimitWithCreditsClauseStaysALimit(t *testing.T) {
	b := &Backend{}
	applyHandler(b, &testHandler{})
	gate, blocked := false, false
	b.onSessionLimit = func(ratelimit.Signal) { gate = true }
	b.onModelBlocked = func(string, string) { blocked = true }
	b.OnAssistant(&AssistantMessage{Message: BetaMessage{
		Model:   syntheticModel,
		Content: []ContentBlock{{Type: "text", Text: "You've reached your Fable limit. Fable 5 requires usage credits. Switch to another model to continue."}},
	}})
	if !gate || blocked {
		t.Errorf("gate=%v blocked=%v, want gate=true blocked=false", gate, blocked)
	}
}

// TestSyntheticModelBlockText_Variants: the model families CC's own matcher
// names, with and without a version.
func TestSyntheticModelBlockText_Variants(t *testing.T) {
	for _, s := range []string{
		fableCreditsText,
		"Opus requires usage credits. Switch to another model to continue.",
		"Mythos 1 Preview requires usage credits.",
	} {
		m := BetaMessage{Model: syntheticModel, Content: []ContentBlock{{Type: "text", Text: s}}}
		if syntheticModelBlockText(m) == "" {
			t.Errorf("not matched: %q", s)
		}
	}
	for _, s := range []string{
		"Fast mode requires usage credits.",
		"You're out of usage credits. Switch to another model to continue.",
	} {
		m := BetaMessage{Model: syntheticModel, Content: []ContentBlock{{Type: "text", Text: s}}}
		if got := syntheticModelBlockText(m); got != "" {
			t.Errorf("matched %q, want no model block", s)
		}
	}
}

// TestParseSessionLimitReset_DatedWithYear is #3102 F2: CC adds the year to a
// dated reset when it is not this year ("Jan 2, 2027, 3pm"); the parser reads
// it instead of falling back to the default wait.
func TestParseSessionLimitReset_DatedWithYear(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("tz unavailable: %v", err)
	}
	now := time.Date(2026, time.December, 30, 10, 0, 0, 0, loc)
	cases := []struct {
		text string
		want time.Time
	}{
		{"You've hit your weekly limit · resets Jan 2, 2027, 3pm (Europe/London)", time.Date(2027, time.January, 2, 15, 0, 0, 0, loc)},
		{"You've hit your Fable limit · resets Jan 2, 2027, 3:30pm (Europe/London). Switch to another model to continue.", time.Date(2027, time.January, 2, 15, 30, 0, 0, loc)},
		// An explicit year is taken as written, not rolled to the next
		// future occurrence.
		{"You've hit your weekly limit · resets Feb 3, 2028, 9am (Europe/London)", time.Date(2028, time.February, 3, 9, 0, 0, 0, loc)},
	}
	for _, tc := range cases {
		sig, ok := parseSessionLimitReset(tc.text, now)
		if !ok {
			t.Errorf("not parsed: %q", tc.text)
			continue
		}
		if !sig.ResetAt.Equal(tc.want) {
			t.Errorf("%q: reset = %v, want %v", tc.text, sig.ResetAt, tc.want)
		}
	}
}
