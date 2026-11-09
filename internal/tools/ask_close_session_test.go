package tools

import (
	"strings"
	"testing"

	"foci/internal/question"
)

// #3000: a depth leaf that reports (or is completed) closes the asks it left
// open. AskRouter.CloseSession ends every ask of the session, removes each
// on-screen prompt from the user's screen, never promotes a queued ask into the
// freed slot, and tells the agent only when asked to.

func TestAskCloseSession_SilentClosesAllAndRemovesPrompt(t *testing.T) {
	t.Parallel()
	idx := newStateDB(t)
	p := &fakePresenter{}
	d := &fakeDeliver{}
	fr := &fakeRemove{}
	tool, router := NewAskTool(p.present, nil, d.deliver, nil, idx, "test", WithRemove(fr.remove))

	older := askReqID(t, tool, askOlderInput)
	askReqID(t, tool, askNewerInput) // queued behind older: never on screen

	if n := router.CloseSession(askSession, false); n != 2 {
		t.Fatalf("CloseSession closed %d asks, want 2 (the one on screen and the queued one)", n)
	}
	if got := router.PendingForSession(askSession); got != "" {
		t.Fatalf("PendingForSession = %q after CloseSession, want none", got)
	}
	if p.presents != 1 {
		t.Errorf("presenter calls = %d, want 1: a closing session must not present its queued ask", p.presents)
	}
	want := askSession + "|" + questionMsgID(older, 0)
	if len(fr.calls) != 1 || fr.calls[0] != want {
		t.Errorf("removals = %v, want exactly [%s] (the queued ask was never shown)", fr.calls, want)
	}
	if msg, ok := d.last(); ok {
		t.Errorf("delivered %q, want nothing: a reported leaf is not told", msg)
	}
	if saved := loadPersisted(t, idx); len(saved) != 0 {
		t.Errorf("persisted asks after CloseSession = %d, want 0", len(saved))
	}
	if n := router.CloseSession(askSession, false); n != 0 {
		t.Errorf("second CloseSession closed %d, want 0", n)
	}
}

func TestAskCloseSession_TellDeliversTheCancelNotice(t *testing.T) {
	t.Parallel()
	p := &fakePresenter{}
	d := &fakeDeliver{}
	fr := &fakeRemove{}
	tool, router := NewAskTool(p.present, nil, d.deliver, nil, nil, "test", WithRemove(fr.remove))
	req := askReqID(t, tool, askOlderInput)

	if n := router.CloseSession(askSession, true); n != 1 {
		t.Fatalf("CloseSession closed %d, want 1", n)
	}
	msg, ok := d.last()
	if !ok || !strings.Contains(msg, "CANCELLED") || d.lastReqID() != req {
		t.Errorf("delivered %q (req %q), want the cancel notice for %s", msg, d.lastReqID(), req)
	}
	if len(fr.calls) != 1 {
		t.Errorf("removals = %v, want the on-screen prompt removed", fr.calls)
	}
}

// A batched app form is addressed by question 0's id whatever its progress.
func TestAskCloseSession_BatchedFormRemovedByItsPromptID(t *testing.T) {
	t.Parallel()
	seq := &fakePresenter{}
	d := &fakeDeliver{}
	fr := &fakeRemove{}
	var promptID string
	batch := func(_, pid string, _ []question.Question, _ func([]string)) bool { promptID = pid; return true }
	tool, router := NewAskTool(seq.present, nil, d.deliver, nil, nil, "test", WithBatchPresent(batch), WithRemove(fr.remove))
	askReqID(t, tool, `{"questions":[{"question":"A?","header":"A"},{"question":"B?","header":"B"}]}`)
	if promptID == "" {
		t.Fatal("the ask was not presented as a batched form")
	}
	router.CloseSession(askSession, false)
	if len(fr.calls) != 1 || fr.calls[0] != askSession+"|"+promptID {
		t.Errorf("removals = %v, want [%s|%s]", fr.calls, askSession, promptID)
	}
}
