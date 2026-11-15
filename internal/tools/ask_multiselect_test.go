package tools

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"foci/internal/question"
)

const multiAsk = `{"questions":[
	{"question":"Toppings?","id":"top","multiSelect":true,"options":["Cheese","Ham","Olives"]},
	{"question":"Size?","options":["Small","Large"]}]}`

// TestAsk_MultiSelectBatched is #2308's done-condition: the app answers a
// multiSelect question with several ticks plus typed text, and the agent's
// message carries every selected label on the question's one answer line
// (#3126: there is no second, structured copy).
func TestAsk_MultiSelectBatched(t *testing.T) {
	t.Parallel()
	tool, _, _, bp, d := newAskBatchFixture(true)
	execAsk(t, tool, multiAsk)
	if !bp.lastQs[0].MultiSelect || bp.lastQs[1].MultiSelect {
		t.Fatalf("multiSelect flag not carried to the batch presenter: %+v", bp.lastQs)
	}

	bp.answer([]string{"qa:m:2,0;extra basil", "qa:1"})

	msg, ok := d.last()
	if !ok {
		t.Fatal("no delivery")
	}
	if !strings.Contains(msg, "\n• [top] Toppings? → Cheese, Olives (other: extra basil)\n") || !strings.HasSuffix(msg, "\n• Size? → Large") {
		t.Errorf("readable lines wrong: %q", msg)
	}
	if strings.Contains(msg, "single-pick") {
		t.Errorf("a real multi pick flagged as single-pick: %q", msg)
	}
}

// TestAsk_MultiSelectGraderGetsSelections: the grader's stdin contract is
// unchanged by #3126 — it still receives the structured "selections".
func TestAsk_MultiSelectGraderGetsSelections(t *testing.T) {
	t.Parallel()
	tool, _, _, bp, d := newAskBatchFixture(true)
	grader := writeGrader(t, "#!/bin/sh\ncat\n")
	raw := strings.TrimSuffix(strings.TrimSpace(multiAsk), "}") + `,"grader":"` + grader + `"}`
	execAsk(t, tool, raw)
	bp.answer([]string{"qa:m:2,0;extra basil", "qa:1"})

	var in graderInput
	out := waitDeliver(t, d)
	// The delivery is a "[SYSTEM: graded result …]" header line, then the
	// grader's stdout — here its own stdin, echoed back.
	if _, body, ok := strings.Cut(out, "\n"); ok {
		out = body
	}
	if err := json.Unmarshal([]byte(out), &in); err != nil {
		t.Fatalf("grader stdin is not the graderInput JSON: %v\n%s", err, out)
	}
	want := question.MultiAnswer{Selected: []string{"Cheese", "Olives"}, Other: "extra basil"}
	if len(in.Selections) != 1 || !reflect.DeepEqual(in.Selections["Toppings?"], want) {
		t.Errorf("grader selections = %+v, want only Toppings? → %+v", in.Selections, want)
	}
	if in.AnswersByID["top"] != "Cheese, Olives (other: extra basil)" {
		t.Errorf("grader answers_by_id = %+v", in.AnswersByID)
	}
}

// TestAsk_MultiSelectChatFallback: on a transport without checkboxes the
// multiSelect question is the old single pick, and the agent is told so.
func TestAsk_MultiSelectChatFallback(t *testing.T) {
	t.Parallel()
	tool, _, seq, _, d := newAskBatchFixture(false)
	execAsk(t, tool, multiAsk)
	seq.answer("qa:1") // Ham
	seq.answer("qa:0") // Small

	msg, _ := d.last()
	if !strings.Contains(msg, "\n• [top] Toppings? → Ham (single-pick client: unpicked options are not a considered no)") {
		t.Errorf("fallback flag missing from the agent's answer line: %q", msg)
	}
}

// TestAsk_SingleSelectHasNoFallbackMarker: a plain single-select answer is
// never marked as a single-pick fallback.
func TestAsk_SingleSelectHasNoFallbackMarker(t *testing.T) {
	t.Parallel()
	tool, _, _, bp, d := newAskBatchFixture(true)
	execAsk(t, tool, twoQuestions)
	bp.answer([]string{"qa:0", "qa:1"})
	msg, _ := d.last()
	if strings.Contains(msg, "single-pick") {
		t.Errorf("single-select batch marked single-pick: %q", msg)
	}
}

// TestAsk_MultiTokenOnSingleQuestionRejected: a multi token for a question that
// is not multiSelect is invalid and leaves the ask pending (like any other
// malformed answer) rather than being delivered garbled.
func TestAsk_MultiTokenOnSingleQuestionRejected(t *testing.T) {
	t.Parallel()
	tool, _, _, bp, d := newAskBatchFixture(true)
	execAsk(t, tool, twoQuestions)
	bp.answer([]string{"qa:m:0,1", "qa:1"})
	if msg, ok := d.last(); ok {
		t.Errorf("invalid multi token delivered: %q", msg)
	}
}

// TestAsk_MultiSelectQuizVerdict: a multiSelect quiz question is right only
// for exactly the correct set.
func TestAsk_MultiSelectQuizVerdict(t *testing.T) {
	t.Parallel()
	const quiz = `{"questions":[{"question":"Primes?","multiSelect":true,"options":["2","4","5"],"quiz":{"correct":[0,2]}}]}`
	for token, want := range map[string]bool{"qa:m:0,2": true, "qa:m:0": false} {
		tool, _, _, bp, d := newAskBatchFixture(true)
		execAsk(t, tool, quiz)
		bp.answer([]string{token})
		msg, _ := d.last()
		mark := " ❌"
		if want {
			mark = " ✅"
		}
		if !strings.HasSuffix(msg, mark) {
			t.Errorf("%s: want verdict%s on the line, got %q", token, mark, msg)
		}
	}
}

// TestAsk_MultiSelectSurvivesRestart: a structured multi answer recorded before
// a restart is still on the delivered answer line after it.
func TestAsk_MultiSelectSurvivesRestart(t *testing.T) {
	idx := newStateDB(t)
	p1 := &fakePresenter{}
	tool1, _ := NewAskTool(p1.present, nil, (&fakeDeliver{}).deliver, nil, idx, "test")
	execAsk(t, tool1, multiAsk)
	p1.answer("qa:2") // Toppings? → Olives (single-pick fallback), now on Size?

	fr := &fakeRestore{}
	d2 := &fakeDeliver{}
	NewAskTool((&fakePresenter{}).present, fr.restore, d2.deliver, nil, idx, "test")
	fr.click("qa:1") // Size? → Large

	msg, ok := d2.last()
	if !ok {
		t.Fatal("no delivery after restart")
	}
	if !strings.Contains(msg, "\n• [top] Toppings? → Olives (single-pick client: unpicked options are not a considered no)") {
		t.Errorf("selection after restart lost: %q", msg)
	}
}
