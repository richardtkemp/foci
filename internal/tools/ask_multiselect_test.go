package tools

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"foci/internal/question"
)

// answerPayload decodes the compact JSON line every delivered answer batch ends
// with.
func answerPayload(t *testing.T, msg string) map[string]json.RawMessage {
	t.Helper()
	i := strings.LastIndex(msg, "\n{")
	if i < 0 {
		t.Fatalf("no JSON payload in %q", msg)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(msg[i+1:]), &m); err != nil {
		t.Fatalf("payload: %v in %q", err, msg)
	}
	return m
}

func payloadSelections(t *testing.T, msg string) map[string]question.MultiAnswer {
	t.Helper()
	raw, ok := answerPayload(t, msg)["selections"]
	if !ok {
		t.Fatalf("no \"selections\" in %q", msg)
	}
	var sel map[string]question.MultiAnswer
	if err := json.Unmarshal(raw, &sel); err != nil {
		t.Fatalf("selections: %v", err)
	}
	return sel
}

const multiAsk = `{"questions":[
	{"question":"Toppings?","id":"top","multiSelect":true,"options":["Cheese","Ham","Olives"]},
	{"question":"Size?","options":["Small","Large"]}]}`

// TestAsk_MultiSelectBatched is #2308's done-condition: the app answers a
// multiSelect question with several ticks plus typed text, and the agent's
// message carries every selected label — on the readable line and in the
// structured "selections" (by text and by id). The single-select question in
// the same ask is untouched and gets no selections entry.
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
	if !strings.Contains(msg, "Toppings? → Cheese, Olives (other: extra basil)") || !strings.Contains(msg, "Size? → Large") {
		t.Errorf("readable lines wrong: %q", msg)
	}
	want := question.MultiAnswer{Selected: []string{"Cheese", "Olives"}, Other: "extra basil"}
	sel := payloadSelections(t, msg)
	if len(sel) != 1 || !reflect.DeepEqual(sel["Toppings?"], want) {
		t.Errorf("selections = %+v, want only Toppings? → %+v", sel, want)
	}
	var byID map[string]question.MultiAnswer
	_ = json.Unmarshal(answerPayload(t, msg)["selections_by_id"], &byID)
	if !reflect.DeepEqual(byID["top"], want) {
		t.Errorf("selections_by_id = %+v", byID)
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
	got := payloadSelections(t, msg)["Toppings?"]
	if !reflect.DeepEqual(got, question.MultiAnswer{Selected: []string{"Ham"}, SingleSelectFallback: true}) {
		t.Errorf("fallback selection = %+v, want Ham flagged single_select_fallback", got)
	}
	if !strings.Contains(msg, `"single_select_fallback":true`) {
		t.Errorf("fallback flag missing from the agent's payload: %q", msg)
	}
}

// TestAsk_SingleSelectHasNoSelections: an ask with no multiSelect question
// delivers exactly the old payload keys.
func TestAsk_SingleSelectHasNoSelections(t *testing.T) {
	t.Parallel()
	tool, _, _, bp, d := newAskBatchFixture(true)
	execAsk(t, tool, twoQuestions)
	bp.answer([]string{"qa:0", "qa:1"})
	msg, _ := d.last()
	if _, ok := answerPayload(t, msg)["selections"]; ok {
		t.Errorf("single-select batch gained \"selections\": %q", msg)
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
		var res map[string]bool
		_ = json.Unmarshal(answerPayload(t, msg)["quiz_results"], &res)
		if got, ok := res["Primes?"]; !ok || got != want {
			t.Errorf("%s: quiz_results = %v, want Primes?=%v (msg %q)", token, res, want, msg)
		}
	}
}

// TestAsk_MultiSelectSurvivesRestart: a structured multi answer recorded before
// a restart is still in the delivered payload after it.
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
	got := payloadSelections(t, msg)["Toppings?"]
	if !reflect.DeepEqual(got, question.MultiAnswer{Selected: []string{"Olives"}, SingleSelectFallback: true}) {
		t.Errorf("selection after restart = %+v", got)
	}
}
