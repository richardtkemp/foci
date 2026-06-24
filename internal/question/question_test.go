package question

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParse_SingleQuestion(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"questions":[{"question":"Which approach?","header":"Approach","options":[{"label":"Option A","description":"First"},{"label":"Option B","description":"Second"}],"multiSelect":false}]}`)
	qs, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(qs) != 1 {
		t.Fatalf("got %d questions, want 1", len(qs))
	}
	if qs[0].Question != "Which approach?" || qs[0].Header != "Approach" {
		t.Errorf("unexpected question: %+v", qs[0])
	}
	if len(qs[0].Options) != 2 || qs[0].Options[0].Label != "Option A" {
		t.Errorf("unexpected options: %+v", qs[0].Options)
	}
}

func TestParse_InvalidJSON(t *testing.T) {
	t.Parallel()
	if _, err := Parse(json.RawMessage(`{invalid`)); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParse_EmptyQuestions(t *testing.T) {
	t.Parallel()
	qs, err := Parse(json.RawMessage(`{"questions":[]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(qs) != 0 {
		t.Errorf("got %d questions, want 0", len(qs))
	}
}

func TestParse_NoCap(t *testing.T) {
	// The whole point of the foci-native tool: more than 4 questions parse fine.
	t.Parallel()
	var sb strings.Builder
	sb.WriteString(`{"questions":[`)
	for i := 0; i < 9; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"question":"Q","options":[{"label":"A"}]}`)
	}
	sb.WriteString(`]}`)
	qs, err := Parse(json.RawMessage(sb.String()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(qs) != 9 {
		t.Fatalf("got %d questions, want 9 (no cap)", len(qs))
	}
}

func TestFormatText_Single(t *testing.T) {
	t.Parallel()
	q := &Question{Question: "Which library?", Header: "Library", Options: []Option{
		{Label: "React", Description: "UI framework"},
		{Label: "Vue", Description: "Progressive framework"},
	}}
	text := FormatText(q, 0, 1)
	for _, want := range []string{"**Library**", "Which library?", "1. **React** — UI framework", "2. **Vue** — Progressive framework"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "1/1") {
		t.Error("single question should not show position numbering")
	}
}

func TestFormatText_MultiQuestion(t *testing.T) {
	t.Parallel()
	q := &Question{Question: "Pick one", Header: "Step", Options: []Option{{Label: "X"}}}
	if text := FormatText(q, 1, 3); !strings.Contains(text, "**Step** (2/3)") {
		t.Errorf("should show header with position, got:\n%s", text)
	}
}

func TestFormatText_NoHeaderNoDescription(t *testing.T) {
	t.Parallel()
	q := &Question{Question: "Pick one", Options: []Option{{Label: "Opt"}}}
	text := FormatText(q, 0, 1)
	if !strings.HasPrefix(text, "Pick one") {
		t.Errorf("should start with question text, got:\n%s", text)
	}
	if !strings.Contains(text, "1. **Opt**") || strings.Contains(text, "—") {
		t.Errorf("option without description should have no dash, got:\n%s", text)
	}
	if text := FormatText(q, 0, 2); !strings.Contains(text, "**Question 1/2**") {
		t.Errorf("multi-question no-header should fall back to numbering, got:\n%s", text)
	}
}

func TestFormatText_NoOptionsHintsTyping(t *testing.T) {
	t.Parallel()
	q := &Question{Question: "What should I name it?"}
	text := FormatText(q, 0, 1)
	if !strings.HasPrefix(text, "What should I name it?") {
		t.Errorf("should start with question text, got:\n%s", text)
	}
	if !strings.Contains(text, "_Reply with your answer._") {
		t.Errorf("option-less question should hint at typing, got:\n%s", text)
	}
}

// TestChoices_NoOptionsCancelOnly proves an option-less question offers only the
// Cancel button (it is answered by typing).
func TestChoices_NoOptionsCancelOnly(t *testing.T) {
	t.Parallel()
	choices := Choices(&Question{Question: "Open?"})
	if len(choices) != 1 || choices[0].Data != CancelData {
		t.Errorf("option-less question should yield only a Cancel choice, got %+v", choices)
	}
}

func TestChoices(t *testing.T) {
	t.Parallel()
	q := &Question{Options: []Option{{Label: "Alpha"}, {Label: "Beta"}, {Label: "Gamma"}}}
	choices := Choices(q)
	if len(choices) != 4 {
		t.Fatalf("got %d choices, want 4 (3 options + Cancel)", len(choices))
	}
	for i, opt := range q.Options {
		if choices[i].Label != opt.Label {
			t.Errorf("choice[%d].Label = %q, want %q", i, choices[i].Label, opt.Label)
		}
		if want := "qa:" + string(rune('0'+i)); choices[i].Data != want {
			t.Errorf("choice[%d].Data = %q, want %q", i, choices[i].Data, want)
		}
	}
	if last := choices[len(choices)-1]; last.Label != "Cancel" || last.Data != CancelData {
		t.Errorf("last choice = {%q,%q}, want {Cancel,%s}", last.Label, last.Data, CancelData)
	}
}

func TestResolveAnswer(t *testing.T) {
	t.Parallel()
	q := &Question{Options: []Option{{Label: "Red"}, {Label: "Blue"}}}

	ans, cancelled, err := ResolveAnswer(q, "qa:1")
	if err != nil || cancelled || ans != "Blue" {
		t.Errorf("qa:1 → (%q,%v,%v), want (Blue,false,nil)", ans, cancelled, err)
	}

	_, cancelled, err = ResolveAnswer(q, CancelData)
	if err != nil || !cancelled {
		t.Errorf("cancel → (cancelled=%v,err=%v), want (true,nil)", cancelled, err)
	}

	ans, cancelled, err = ResolveAnswer(q, "freeform text")
	if err != nil || cancelled || ans != "freeform text" {
		t.Errorf("typed → (%q,%v,%v), want (freeform text,false,nil)", ans, cancelled, err)
	}

	if _, _, err = ResolveAnswer(q, "qa:9"); err == nil {
		t.Error("out-of-range index should error")
	}
	if _, _, err = ResolveAnswer(q, "qa:notnum"); err == nil {
		t.Error("non-numeric index should error")
	}
}

func TestMergeAnswers(t *testing.T) {
	t.Parallel()
	original := json.RawMessage(`{"questions":[{"question":"Q1?"}],"extra":"kept"}`)
	result, err := MergeAnswers(original, map[string]string{"Q1?": "Answer 1"})
	if err != nil {
		t.Fatalf("MergeAnswers: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["questions"]; !ok {
		t.Error("missing 'questions'")
	}
	if _, ok := m["extra"]; !ok {
		t.Error("missing preserved 'extra'")
	}
	var got map[string]string
	if err := json.Unmarshal(m["answers"], &got); err != nil {
		t.Fatalf("unmarshal answers: %v", err)
	}
	if got["Q1?"] != "Answer 1" {
		t.Errorf("answers[Q1?] = %q, want %q", got["Q1?"], "Answer 1")
	}
}

func TestAccumulator(t *testing.T) {
	t.Parallel()
	qs := []Question{
		{Question: "Q1?", Options: []Option{{Label: "A1"}}},
		{Question: "Q2?", Options: []Option{{Label: "A2"}}},
	}
	acc := NewAccumulator(qs)
	if acc.Total() != 2 || acc.Done() {
		t.Fatalf("fresh accumulator: total=%d done=%v", acc.Total(), acc.Done())
	}
	if acc.Current().Question != "Q1?" || acc.Index() != 0 {
		t.Errorf("current=%q idx=%d, want Q1?/0", acc.Current().Question, acc.Index())
	}
	acc.Record("A1")
	if acc.Done() || acc.Current().Question != "Q2?" {
		t.Errorf("after one record: done=%v current=%q", acc.Done(), acc.Current().Question)
	}
	acc.Record("A2")
	if !acc.Done() || acc.Current() != nil {
		t.Errorf("after two records: done=%v current=%v", acc.Done(), acc.Current())
	}
	acc.Record("ignored") // no-op past end
	answers := acc.Answers()
	if answers["Q1?"] != "A1" || answers["Q2?"] != "A2" || len(answers) != 2 {
		t.Errorf("answers = %+v, want {Q1?:A1, Q2?:A2}", answers)
	}
}
