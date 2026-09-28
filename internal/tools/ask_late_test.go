package tools

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/question"
)

// #1868: HandleBatchByPrompt used to WARN "(dropped)" and return for a prompt with
// no pending ask. It now reports the miss so the caller can deliver a late answer,
// and still routes a live one.
func TestAsk_HandleBatchByPromptReportsMatch(t *testing.T) {
	t.Parallel()
	tool, router, _, bp, d := newAskBatchFixture(true)
	execAsk(t, tool, twoQuestions)

	if router.HandleBatchByPrompt("ask-test-NOSUCHASK-q0", []string{"qa:0", "qa:0"}) {
		t.Fatal("unregistered prompt reported as matched")
	}
	if _, ok := d.last(); ok {
		t.Fatal("an unmatched prompt delivered something through the live path")
	}
	if !router.HandleBatchByPrompt(bp.lastPromptID, []string{"qa:0", "qa:1"}) {
		t.Fatal("live prompt reported as unmatched")
	}
	if msg, ok := d.last(); !ok || !strings.Contains(msg, "Red") {
		t.Fatalf("live prompt not delivered: %q", msg)
	}
}

// #2080: an answer to an ask that has already closed reaches the agent as a
// normal message, marked late, naming the question TEXT, when it was asked, and
// the chosen option labels.
func TestAsk_LateAnswerDeliveredAsMarkedMessage(t *testing.T) {
	t.Parallel()
	tool, router, _, bp, d := newAskBatchFixture(true)
	execAsk(t, tool, twoQuestions)
	promptID := bp.lastPromptID
	bp.answer([]string{"qa:cancel", "qa:cancel"}) // the ask closes (expiry resolves the same way)
	reqID := d.lastReqID()

	asked := time.Date(2026, 9, 23, 14, 2, 0, 0, time.UTC)
	ok := router.DeliverLateAnswer(LateAnswer{
		PromptID:   promptID,
		SessionKey: askSession,
		AskedAt:    asked,
		Questions: []question.Question{
			{Question: "Color?", Header: "Color", Options: []question.Option{{Label: "Red"}, {Label: "Blue"}}},
			{Question: "Size?", Options: []question.Option{{Label: "Small"}, {Label: "Large"}}},
		},
		Answers: []string{"qa:1", "qa:1"},
	})
	if !ok {
		t.Fatal("late answer not delivered")
	}
	msg, _ := d.last()
	for _, want := range []string{"LATE ANSWER", reqID, "2026-09-23 14:02", "Color?", "Size?", "Blue", "Large", `"late":true`} {
		if !strings.Contains(msg, want) {
			t.Errorf("late answer message missing %q:\n%s", want, msg)
		}
	}
	if got := d.lastReqID(); got != reqID {
		t.Errorf("late answer tagged with req %q, want %q", got, reqID)
	}
	d.mu.Lock()
	sk := d.sessions[len(d.sessions)-1]
	d.mu.Unlock()
	if sk != askSession {
		t.Errorf("late answer delivered to %q, want %q", sk, askSession)
	}
}

// Nothing the user chose is lost when the question text has aged out: the raw
// answer is passed through, flagged as having no question on record.
func TestAsk_LateAnswerWithoutQuestionStillDelivered(t *testing.T) {
	t.Parallel()
	_, router, _, _, d := newAskBatchFixture(true)
	if !router.DeliverLateAnswer(LateAnswer{PromptID: "ask-test-01OLD-q0", SessionKey: askSession, Answers: []string{"qa:2"}}) {
		t.Fatal("late answer with no question on record was not delivered")
	}
	msg, _ := d.last()
	if !strings.Contains(msg, "no longer on record") || !strings.Contains(msg, "qa:2") || !strings.Contains(msg, "unknown time") {
		t.Errorf("message = %q, want the raw answer flagged as having no question text", msg)
	}
}

// A late CANCEL answers nothing, and a prompt that is not one of this agent's asks
// (a permission prompt, another agent's ask) is never delivered as an answer.
func TestAsk_LateAnswerSkipsCancelAndForeignPrompts(t *testing.T) {
	t.Parallel()
	_, router, _, _, d := newAskBatchFixture(true)
	for _, la := range []LateAnswer{
		{PromptID: "ask-test-01X-q0", SessionKey: askSession, Answers: []string{"qa:0", question.CancelData}},
		{PromptID: "perm-01X", SessionKey: askSession, Answers: []string{"qa:0"}},
		{PromptID: "ask-other-01X-q0", SessionKey: askSession, Answers: []string{"qa:0"}},
	} {
		if router.DeliverLateAnswer(la) {
			t.Errorf("DeliverLateAnswer(%s %v) = true, want false", la.PromptID, la.Answers)
		}
	}
	if msg, ok := d.last(); ok {
		t.Fatalf("delivered %q, want nothing", msg)
	}
}

// fakeRemove records AskRemoveFn calls.
type fakeRemove struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeRemove) remove(sessionKey, msgID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sessionKey+"|"+msgID)
}

// #2080 ruling 2: an ask dropped as stale on restart disappears from the app. The
// removal is held with the agent notice (the transport is not up during restore)
// and released by DeliverRestoreNotices. A queued ask was never on screen, so it
// has nothing to remove.
func TestAskRestore_StaleAskRemovedFromApp(t *testing.T) {
	t.Parallel()
	idx := newStateDB(t)
	live := seedAsk("ask-test-stale-live", time.Now().Add(-2*pendingAskTTL))
	queued := seedAsk("ask-test-stale-queued", time.Now().Add(-2*pendingAskTTL))
	queued.Queued = true
	data, err := json.Marshal([]persistedAsk{live, queued})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.SetAgentMetadata("test", askMetaKey, string(data)); err != nil {
		t.Fatal(err)
	}

	fr := &fakeRemove{}
	_, router := NewAskTool((&fakePresenter{}).present, (&fakeRestore{}).restore, (&fakeDeliver{}).deliver, nil, idx, "test",
		WithRemove(fr.remove))
	if len(fr.calls) != 0 {
		t.Fatalf("removed during restore (%v) — the app transport is not up yet", fr.calls)
	}
	router.DeliverRestoreNotices()
	want := askSession + "|" + questionMsgID(live.RequestID, live.Idx)
	if len(fr.calls) != 1 || fr.calls[0] != want {
		t.Fatalf("removals = %v, want exactly [%s]", fr.calls, want)
	}
}
