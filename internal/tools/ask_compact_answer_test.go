package tools

import (
	"strings"
	"testing"
	"time"

	"foci/internal/question"
)

// TestAsk_CompactAnswerDelivery is #3126's done-condition (Dick 2026-11-15):
// an answered ask is delivered as ONE line per question — the header, the
// question text cut to 50 characters, and the chosen option LABEL in full —
// with no second copy (no JSON block repeating the full question text). On the
// pre-#3126 tree the full question text reached the agent inside a trailing
// {"answers":{...}} block, so the "no full question" check fails there.
func TestAsk_CompactAnswerDelivery(t *testing.T) {
	t.Parallel()
	tool, _, p, d := newAskFixture()
	long := "Should we drop part one of the migration plan and keep only the second phase for now?"
	raw := `{"questions":[
		{"question":"` + long + `","header":"Q5 #3080","options":[{"label":"Drop part 1 (Recommended)"},{"label":"Keep both"}]},
		{"question":"Free text?","header":"Notes"}]}`
	execAsk(t, tool, raw)
	p.answer("qa:0")
	typed := "keep the second phase, but only after the March release — and tell Toby first"
	p.answer(typed)

	msg, ok := d.last()
	if !ok {
		t.Fatal("no delivery")
	}
	if strings.Contains(msg, long) {
		t.Errorf("full question text delivered (a second copy); want it cut to 50 chars:\n%s", msg)
	}
	if strings.Contains(msg, "{") {
		t.Errorf("delivered batch still carries a JSON block:\n%s", msg)
	}
	want := "\n• Q5 #3080: " + string([]rune(long)[:50]) + "… → Drop part 1 (Recommended)"
	if !strings.Contains(msg, want) {
		t.Errorf("missing compact line %q in:\n%s", want, msg)
	}
	if !strings.Contains(msg, "\n• Notes: Free text? → "+typed) {
		t.Errorf("typed answer not delivered in full:\n%s", msg)
	}
	// Exactly one line per question after the preamble.
	if n := strings.Count(msg, "\n• "); n != 2 {
		t.Errorf("got %d answer lines, want 2:\n%s", n, msg)
	}
}

// TestAnswerLine covers the line's edge cases: no header, header equal to the
// question, an id, a multi-line question, a multi-byte cut, and a
// single-pick multiSelect fallback.
func TestAnswerLine(t *testing.T) {
	t.Parallel()
	greek := strings.Repeat("α", 60)
	cases := []struct {
		name  string
		q     question.Question
		ans   string
		multi map[string]question.MultiAnswer
		want  string
	}{
		{"no header", question.Question{Question: "Color?"}, "Red", nil, "\n• Color? → Red"},
		{"header equals question", question.Question{Question: "Color?", Header: "Color?"}, "Red", nil, "\n• Color? → Red"},
		{"id leads", question.Question{ID: "q_color", Question: "Color?", Header: "Pick"}, "Blue", nil, "\n• [q_color] Pick: Color? → Blue"},
		{"newlines collapse", question.Question{Question: "Which?\n1. a\n2. b", Header: "H"}, "1", nil, "\n• H: Which? 1. a 2. b → 1"},
		{"rune-safe cut", question.Question{Question: greek}, "x", nil, "\n• " + strings.Repeat("α", 50) + "… → x"},
		{"single-pick fallback", question.Question{Question: "Toppings?", MultiSelect: true, Options: []question.Option{{Label: "Ham"}}}, "Ham",
			map[string]question.MultiAnswer{"Toppings?": {Selected: []string{"Ham"}, SingleSelectFallback: true}},
			"\n• Toppings? → Ham (single-pick client: unpicked options are not a considered no)"},
	}
	for _, c := range cases {
		if got := answerLine(&c.q, c.ans, c.multi); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// TestFormatLateAnswer_Compact: a late answer uses the same compact line and
// carries no JSON block; the request id and asked-at time stay in the preamble.
func TestFormatLateAnswer_Compact(t *testing.T) {
	t.Parallel()
	asked := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	la := LateAnswer{
		Questions: []question.Question{{Question: "Color?", Header: "Pick", Options: []question.Option{{Label: "Red"}, {Label: "Blue"}}}},
		Answers:   []string{"qa:1"},
		AskedAt:   asked,
	}
	msg := formatLateAnswer("req1", la, asked.Add(time.Hour))
	if !strings.Contains(msg, "\n• Pick: Color? → Blue") || strings.Contains(msg, "{") {
		t.Errorf("late answer not compact:\n%s", msg)
	}
	if !strings.Contains(msg, "req req1") || !strings.Contains(msg, "2026-11-15 10:00") {
		t.Errorf("late preamble lost the request id or asked-at time:\n%s", msg)
	}
}
