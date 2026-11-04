package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"foci/internal/log"
	"foci/internal/question"
)

const askOneQ = `{"questions":[{"question":"Which?","options":[{"label":"A"},{"label":"B"}]}]}`

// TestAsk_UndeliverableFirstQuestionFailsTheCall (#2764, audit A8): a question no
// chat could show ends the ask and fails the tool call with the reason. Before
// this the tool said "Posted to the user" and the ask stayed pending, on no
// screen and with no expiry sweep, until a restart.
func TestAsk_UndeliverableFirstQuestionFailsTheCall(t *testing.T) {
	t.Parallel()
	p := &fakePresenter{fail: errors.New("no chat can show it")}
	d := &fakeDeliver{}
	resolved := make(chan string, 1)
	idx := newStateDB(t)
	tool, router := NewAskTool(p.present, nil, d.deliver, nil, idx, "test",
		WithOnResolve(func(_, reqID string) { resolved <- reqID }))

	_, err := tool.Execute(askCtx(), json.RawMessage(askOneQ))
	if err == nil || !strings.Contains(err.Error(), "no chat can show it") {
		t.Fatalf("Execute err = %v, want the presenter's reason", err)
	}
	if id := router.PendingForSession(askSession); id != "" {
		t.Errorf("session still has pending ask %s", id)
	}
	if raw, _ := idx.GetAgentMetadata("test", askMetaKey); strings.Contains(raw, "Which?") {
		t.Errorf("the dropped ask is still persisted: %s", raw)
	}
	if id := <-resolved; id == "" {
		t.Error("onResolve fired with no request id")
	}
	if len(d.messages) != 0 {
		t.Errorf("delivered %q; the tool's error is the agent's answer", d.messages)
	}
}

// TestAsk_UndeliverableNextQuestionDeliversAnswersSoFar (#2764): when question 2
// of a sequence cannot be shown, the ask ends and the user's first answer still
// reaches the agent, with the reason and on the user-input route.
func TestAsk_UndeliverableNextQuestionDeliversAnswersSoFar(t *testing.T) {
	t.Parallel()
	p := &fakePresenter{fail: errors.New("the chat went away"), failAfter: 1}
	sys, user := &fakeDeliver{}, &fakeDeliver{}
	tool, router := NewAskTool(p.present, nil, sys.deliver, nil, nil, "test", WithUserInputDeliver(user.deliver))

	execAsk(t, tool, `{"questions":[
		{"question":"First?","header":"One","options":[{"label":"A"},{"label":"B"}]},
		{"question":"Second?","header":"Two","options":[{"label":"C"}]}]}`)
	p.answer("qa:1")

	if id := router.PendingForSession(askSession); id != "" {
		t.Errorf("session still has pending ask %s", id)
	}
	if len(sys.messages) != 0 || len(user.messages) != 1 {
		t.Fatalf("system deliveries %q, user deliveries %q; want one user delivery", sys.messages, user.messages)
	}
	got := user.messages[0]
	for _, want := range []string{"1 of 2", "never asked", "the chat went away", "One → B"} {
		if !strings.Contains(got, want) {
			t.Errorf("delivery lacks %q:\n%s", want, got)
		}
	}
}

// TestAsk_UndeliverablePromotedAskTellsAgentAndDrainsQueue (#2764): a queued ask
// has no tool call left to fail. When promotion cannot show it the agent gets a
// system notice, and the next queued ask still gets its turn.
func TestAsk_UndeliverablePromotedAskTellsAgentAndDrainsQueue(t *testing.T) {
	t.Parallel()
	p := &fakePresenter{fail: errors.New("the chat went away"), failAfter: 1}
	sys, user := &fakeDeliver{}, &fakeDeliver{}
	st := newAskState(p.present, nil, sys.deliver, nil, nil, "test")
	st.deliverUser = user.deliver
	qs := func(s string) []question.Question { return []question.Question{{Question: s}} }

	first, _, err := st.start(askSession, qs("FIRST"), nil, graderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	second, queued2, err2 := st.start(askSession, qs("SECOND"), nil, graderConfig{})
	third, queued3, err3 := st.start(askSession, qs("THIRD"), nil, graderConfig{})
	if !queued2 || !queued3 || err2 != nil || err3 != nil {
		t.Fatalf("second/third: queued=%v/%v err=%v/%v, want queued with no error", queued2, queued3, err2, err3)
	}

	st.handleResponse(first, "typed")

	if len(user.messages) != 1 {
		t.Errorf("user deliveries = %q, want the first ask's answer", user.messages)
	}
	if len(sys.messages) != 2 || sys.reqIDs[0] != second || sys.reqIDs[1] != third {
		t.Fatalf("system notices for %v, want [%s %s]", sys.reqIDs, second, third)
	}
	for _, want := range []string{"nothing was answered", "the chat went away"} {
		if !strings.Contains(sys.messages[0], want) {
			t.Errorf("notice lacks %q: %s", want, sys.messages[0])
		}
	}
	if p.presents != 3 {
		t.Errorf("presents = %d, want 3 (each queued ask was tried)", p.presents)
	}
	if len(st.byReqID) != 0 {
		t.Errorf("%d asks still registered", len(st.byReqID))
	}
}

// TestAsk_SequentialTextLeavesLabelsToButtons (#2764, audit C3): every sequential
// surface shows one button per option, so the text does not repeat the labels. A
// description has no button to go on, so it stays in the body.
func TestAsk_SequentialTextLeavesLabelsToButtons(t *testing.T) {
	t.Parallel()
	p := &fakePresenter{}
	tool, _ := NewAskTool(p.present, nil, (&fakeDeliver{}).deliver, nil, nil, "test")
	execAsk(t, tool, `{"questions":[{"question":"Which?","options":[{"label":"Plain"},{"label":"Rich","description":"has detail"}]}]}`)

	if strings.Contains(p.lastText, "1.") || strings.Contains(p.lastText, "Plain") {
		t.Errorf("text repeats the options:\n%s", p.lastText)
	}
	if !strings.Contains(p.lastText, "**Rich** — has detail") {
		t.Errorf("text lost the description:\n%s", p.lastText)
	}
}

// TestAsk_AnswerForUnknownAskWarns (#2764, audit C6): an answer for an ask foci no
// longer holds cannot be delivered, so it must at least leave a WARN naming it, on
// both answer paths. An expiry carries no answer and stays out of WARN.
// Not parallel: log.SetOutput is process-global.
func TestAsk_AnswerForUnknownAskWarns(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	st := newAskState((&fakePresenter{}).present, nil, (&fakeDeliver{}).deliver, nil, nil, "test")

	for name, fn := range map[string]func(){
		"sequential": func() { st.handleResponse("ask-gone", "my typed answer") },
		"batched":    func() { st.handleBatchResponse("ask-gone", []string{"my typed answer"}) },
	} {
		logs.Reset()
		fn()
		out := logs.String()
		if !strings.Contains(out, "WARN") || !strings.Contains(out, "ask-gone") || !strings.Contains(out, "my typed answer") {
			t.Errorf("%s: log = %q, want a WARN naming the ask and the answer", name, out)
		}
	}

	logs.Reset()
	st.handleResponse("ask-gone", question.ExpiredData)
	st.handleBatchResponse("ask-gone", []string{question.ExpiredData})
	if strings.Contains(logs.String(), "WARN") {
		t.Errorf("expiry of a resolved ask warned: %q", logs.String())
	}
}
