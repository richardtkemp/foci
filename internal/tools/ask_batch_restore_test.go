package tools

import (
	"strings"
	"sync"
	"testing"
)

// fakeBatchRestore captures AskRestoreBatchFn calls so a test can drive a batched
// answer whose server-side registration was rebuilt after a simulated restart.
type fakeBatchRestore struct {
	mu           sync.Mutex
	calls        int
	lastPromptID string
	lastCount    int
	onResponse   func([]string)
}

func (f *fakeBatchRestore) restoreBatch(sessionKey, promptID string, questionCount int, onResponse func(answers []string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastPromptID = promptID
	f.lastCount = questionCount
	f.onResponse = onResponse
}

func (f *fakeBatchRestore) answer(answers []string) {
	f.mu.Lock()
	cb := f.onResponse
	f.mu.Unlock()
	if cb != nil {
		cb(answers)
	}
}

// TestAskRestore_BatchedAsk_ReRegistersViaBatchRestore is the ask-layer half of the
// #1473 fix (C): a BATCHED ask that survives a restart must be re-registered via the
// BATCHED restore hook (the analogue of the sequential AskRestoreFn), NOT via the
// sequential one — the app still shows the same form and answers with Answers.
//
// RED (before C): reattach had no batched restore path, so a restored batched ask
// fell through to the sequential AskRestoreFn (fr.calls==1, fbr.calls==0) whose
// registration the app's Answers reply never matches.
//
// GREEN (with C): restoreBatch fires for the batched ask, re-registers the callback
// under the same promptID, and that callback delivers the assembled batch.
func TestAskRestore_BatchedAsk_ReRegistersViaBatchRestore(t *testing.T) {
	idx := newStateDB(t)

	// Instance 1: present a batched (native-app) ask — records batched=true + persists.
	bp := &fakeBatchPresenter{batched: true}
	tool1, _ := NewAskTool((&fakePresenter{}).present, nil, (&fakeDeliver{}).deliver, nil, idx, "test",
		WithBatchPresent(bp.present))
	execAsk(t, tool1, twoQuestionAsk)

	// Instance 2: a restart. The app is offline, so a re-present would fail — restore
	// must re-register WITHOUT sending a frame. Wire both restore hooks to prove the
	// batched one is chosen and the sequential one is not touched.
	fbr := &fakeBatchRestore{}
	fr := &fakeRestore{}
	d2 := &fakeDeliver{}
	_, router2 := NewAskTool((&fakePresenter{}).present, fr.restore, d2.deliver, nil, idx, "test",
		WithBatchPresent((&fakeBatchPresenter{batched: false}).present), // offline app: present would decline
		WithBatchRestore(fbr.restoreBatch))

	if fbr.calls != 1 {
		t.Fatalf("batched restore calls = %d, want 1 (batched ask must re-register via restoreBatch)", fbr.calls)
	}
	if fr.calls != 0 {
		t.Fatalf("sequential restore calls = %d, want 0 (a batched ask must NOT sequential-restore)", fr.calls)
	}
	reqID := router2.PendingForSession(askSession)
	if reqID == "" {
		t.Fatal("pending ask not restored for session after restart")
	}
	if want := reqID + "-q0"; fbr.lastPromptID != want {
		t.Errorf("restoreBatch promptID = %q, want %q (question-0 form id)", fbr.lastPromptID, want)
	}
	if fbr.lastCount != 2 {
		t.Errorf("restoreBatch questionCount = %d, want 2", fbr.lastCount)
	}

	// The re-registered callback resolves the ask and delivers the assembled batch.
	fbr.answer([]string{"qa:0", "qa:1"}) // A1a, A2b
	msg, ok := d2.last()
	if !ok {
		t.Fatal("restored batched ask did not deliver after its answer")
	}
	if !strings.Contains(msg, "A1a") || !strings.Contains(msg, "A2b") {
		t.Errorf("delivered batch missing answers: %q", msg)
	}
}
