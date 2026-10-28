package question

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// multiData builds a multiSelect answer token the way the app does (format on
// multiPrefix).
func multiData(indexes []int, other string) string {
	parts := make([]string, len(indexes))
	for i, idx := range indexes {
		parts[i] = strconv.Itoa(idx)
	}
	s := multiPrefix + strings.Join(parts, ",")
	if other != "" {
		s += ";" + other
	}
	return s
}

func multiQ() *Question {
	return &Question{
		Question:    "Which?",
		MultiSelect: true,
		Options:     []Option{{Label: "Red"}, {Label: "Green"}, {Label: "Blue"}},
	}
}

// TestResolve_MultiData pins the app's multiSelect token (#2308): every ticked
// label comes back in OPTION order (not tick order), duplicates collapse, and
// the typed text after the first ";" is kept verbatim.
func TestResolve_multiData(t *testing.T) {
	t.Parallel()
	q := multiQ()
	cases := []struct {
		name, token, answer string
		want                MultiAnswer
	}{
		{"ticks", multiData([]int{2, 0}, ""), "Red, Blue", MultiAnswer{Selected: []string{"Red", "Blue"}}},
		{"dup", "qa:m:1,1", "Green", MultiAnswer{Selected: []string{"Green"}}},
		{"ticks+text", multiData([]int{1}, "and teal; maybe, too"), "Green (other: and teal; maybe, too)",
			MultiAnswer{Selected: []string{"Green"}, Other: "and teal; maybe, too"}},
		{"text only", multiData(nil, "none"), "none", MultiAnswer{Selected: []string{}, Other: "none"}},
		{"empty", "qa:m:", "", MultiAnswer{Selected: []string{}}},
	}
	for _, c := range cases {
		r, err := Resolve(q, c.token)
		if err != nil || r.Cancelled || r.Answer != c.answer || r.Multi == nil || !reflect.DeepEqual(*r.Multi, c.want) {
			t.Errorf("%s: Resolve(%q) = %+v multi=%+v err=%v; want answer %q multi %+v", c.name, c.token, r, r.Multi, err, c.answer, c.want)
		}
	}
	for _, bad := range []string{"qa:m:3", "qa:m:-1", "qa:m:x", "qa:m:0,,1"} {
		if _, err := Resolve(q, bad); err == nil {
			t.Errorf("Resolve(%q) = nil error, want out-of-range/garbage rejected", bad)
		}
	}
}

// TestResolve_MultiFallback: a multiSelect question answered through a
// single-select surface (button or typed text) still resolves exactly as
// before, and its MultiAnswer is flagged so the agent can tell it apart from a
// considered multi-pick.
func TestResolve_MultiFallback(t *testing.T) {
	t.Parallel()
	q := multiQ()
	r, err := Resolve(q, "qa:1")
	if err != nil || r.Answer != "Green" || r.Multi == nil ||
		!reflect.DeepEqual(*r.Multi, MultiAnswer{Selected: []string{"Green"}, SingleSelectFallback: true}) {
		t.Errorf("button: %+v multi=%+v err=%v", r, r.Multi, err)
	}
	r, err = Resolve(q, "typed")
	if err != nil || r.Answer != "typed" || r.Multi == nil ||
		!reflect.DeepEqual(*r.Multi, MultiAnswer{Selected: []string{}, Other: "typed", SingleSelectFallback: true}) {
		t.Errorf("typed: %+v multi=%+v err=%v", r, r.Multi, err)
	}
	if r, _ := Resolve(q, CancelData); !r.Cancelled || r.Multi != nil {
		t.Errorf("cancel: %+v", r)
	}
	js, _ := json.Marshal(MultiAnswer{Selected: []string{}})
	if string(js) != `{"selected":[]}` {
		t.Errorf("empty MultiAnswer JSON = %s, want selected:[] and no fallback key", js)
	}
}

// TestResolve_MultiTokenOnSingleQuestion: a multi token is only meaningful on
// a multiSelect question; a single-select question must not silently accept it.
func TestResolve_MultiTokenOnSingleQuestion(t *testing.T) {
	t.Parallel()
	q := &Question{Question: "One?", Options: []Option{{Label: "A"}, {Label: "B"}}}
	if _, err := Resolve(q, "qa:m:0,1"); err == nil {
		t.Error("multi token on a single-select question accepted")
	}
	r, err := Resolve(q, "qa:1")
	if err != nil || r.Answer != "B" || r.Multi != nil {
		t.Errorf("single-select unchanged: %+v err=%v", r, err)
	}
}

// TestQuizVerdictFor_Multi: a multiSelect quiz question is right only when the
// ticked SET equals the correct set; single-select keeps the any-of rule.
func TestQuizVerdictFor_Multi(t *testing.T) {
	t.Parallel()
	q := multiQ()
	q.Quiz = &Quiz{Correct: []int{0, 2}}
	check := func(token string, wantCorrect, wantOK bool) {
		t.Helper()
		r, err := Resolve(q, token)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", token, err)
		}
		c, ok := QuizVerdictFor(q, r.Answer, r.Multi)
		if c != wantCorrect || ok != wantOK {
			t.Errorf("%q: verdict (%v,%v), want (%v,%v)", token, c, ok, wantCorrect, wantOK)
		}
	}
	check("qa:m:2,0", true, true)
	check("qa:m:0", false, true)
	check("qa:m:0,1,2", false, true)
	check("qa:m:;text", false, false)
	check("qa:0", false, true) // a fallback single pick cannot be the full set
	single := &Question{Question: "Q", Options: q.Options, Quiz: &Quiz{Correct: []int{0, 2}}}
	if c, ok := QuizVerdictFor(single, "Blue", nil); !c || !ok {
		t.Error("single-select any-of rule changed")
	}
	if !strings.Contains(FormatReveal(q, "Red, Blue", &MultiAnswer{Selected: []string{"Red", "Blue"}}), "✅") {
		t.Error("reveal of a full correct multi-pick is not marked correct")
	}
}

// TestFormatText_MultiSelectHint: the chat fallback tells the user they can only
// pick one here; a single-select question's text is unchanged.
func TestFormatText_MultiSelectHint(t *testing.T) {
	t.Parallel()
	q := multiQ()
	if !strings.Contains(FormatText(q, 0, 1), "pick the one that fits best") {
		t.Error("multiSelect fallback text lacks the single-pick hint")
	}
	q.MultiSelect = false
	if strings.Contains(FormatText(q, 0, 1), "pick the one") {
		t.Error("single-select text gained the hint")
	}
}

// TestAccumulator_RecordResolution keeps the structured multi answer alongside
// the display answer, keyed by question text.
func TestAccumulator_RecordResolution(t *testing.T) {
	t.Parallel()
	q := multiQ()
	acc := NewAccumulator([]Question{*q})
	r, _ := Resolve(q, "qa:m:0,1")
	acc.RecordResolution(r)
	if !acc.Done() || acc.Answers()["Which?"] != "Red, Green" ||
		!reflect.DeepEqual(acc.Multi()["Which?"].Selected, []string{"Red", "Green"}) {
		t.Errorf("answers=%v multi=%v", acc.Answers(), acc.Multi())
	}
}

// TestResolve_MultiBlankAndOptionless: a blank slot on a multiSelect question
// is an empty answer, not a fallback pick; and multiSelect on a question with
// no options is ignored (a plain typed question).
func TestResolve_MultiBlankAndOptionless(t *testing.T) {
	t.Parallel()
	r, err := Resolve(multiQ(), "")
	if err != nil || r.Multi == nil || r.Multi.SingleSelectFallback || len(r.Multi.Selected) != 0 || r.Multi.Other != "" {
		t.Errorf("blank: %+v multi=%+v err=%v", r, r.Multi, err)
	}
	free := &Question{Question: "Thoughts?", MultiSelect: true}
	if free.IsMulti() {
		t.Error("option-less question reports IsMulti")
	}
	r, err = Resolve(free, "typed")
	if err != nil || r.Answer != "typed" || r.Multi != nil {
		t.Errorf("option-less multiSelect: %+v err=%v", r, err)
	}
	if strings.Contains(FormatText(free, 0, 1), "pick the one") {
		t.Error("option-less question got the pick-one hint")
	}
}
