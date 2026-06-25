package tools

import (
	"strings"
	"sync"
	"testing"

	"foci/internal/question"
)

// fakeBatchPresenter is the AskPresentBatchFn counterpart to fakePresenter: it
// records the whole batched question set and the single onResponse callback, and
// returns a configurable `batched` verdict so a test can exercise both the
// batched path and the sequential fallback.
type fakeBatchPresenter struct {
	mu           sync.Mutex
	presents     int
	lastPromptID string
	lastQs       []question.Question
	onResponse   func([]string)
	batched      bool // what present() returns (app capable vs not)
}

func (f *fakeBatchPresenter) present(sessionKey, promptID string, qs []question.Question, onResponse func(answers []string)) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.presents++
	f.lastPromptID = promptID
	f.lastQs = qs
	f.onResponse = onResponse
	return f.batched
}

func (f *fakeBatchPresenter) answer(answers []string) {
	f.mu.Lock()
	cb := f.onResponse
	f.mu.Unlock()
	if cb != nil {
		cb(answers)
	}
}

func newAskBatchFixture(batched bool) (*Tool, *AskRouter, *fakePresenter, *fakeBatchPresenter, *fakeDeliver) {
	seq := &fakePresenter{}
	bp := &fakeBatchPresenter{batched: batched}
	d := &fakeDeliver{}
	tool, router := NewAskTool(seq.present, nil, d.deliver, nil, nil, "test", WithBatchPresent(bp.present))
	return tool, router, seq, bp, d
}

const twoQuestions = `{"questions":[
	{"question":"Color?","header":"Color","options":[{"label":"Red"},{"label":"Blue"}]},
	{"question":"Size?","header":"Size","options":[{"label":"Small"},{"label":"Large"}]}]}`

// A batch-capable client receives ALL questions in one form (the sequential
// presenter is never touched), and a single all-answers response delivers the
// assembled batch.
func TestAsk_BatchPresentsAllAtOnce(t *testing.T) {
	t.Parallel()
	tool, _, seq, bp, d := newAskBatchFixture(true)
	execAsk(t, tool, twoQuestions)

	if seq.presents != 0 {
		t.Fatalf("sequential presenter called %d times; want 0 (batched)", seq.presents)
	}
	if got := len(bp.lastQs); got != 2 {
		t.Fatalf("batch presenter got %d questions; want 2", got)
	}
	if _, ok := d.last(); ok {
		t.Fatal("answers delivered before the user responded")
	}

	// One submit carries both answers, positionally: Red (qa:0), Large (qa:1).
	bp.answer([]string{"qa:0", "qa:1"})

	msg, ok := d.last()
	if !ok {
		t.Fatal("no answers delivered after the batched response")
	}
	if !strings.Contains(msg, "Red") || !strings.Contains(msg, "Large") {
		t.Fatalf("delivered batch is missing answers: %q", msg)
	}
}

// When the client can't batch (present returns false), the ask falls back to the
// untouched sequential one-question-at-a-time path.
func TestAsk_BatchFallsBackToSequential(t *testing.T) {
	t.Parallel()
	tool, _, seq, bp, d := newAskBatchFixture(false)
	execAsk(t, tool, twoQuestions)

	if bp.presents != 1 {
		t.Fatalf("batch presenter called %d times; want 1 (attempted once, declined)", bp.presents)
	}
	if seq.presents != 1 {
		t.Fatalf("sequential presenter called %d times; want 1 (first question)", seq.presents)
	}

	seq.answer("qa:0") // Red
	seq.answer("qa:1") // Large

	msg, ok := d.last()
	if !ok {
		t.Fatal("no delivery after sequential answers")
	}
	if !strings.Contains(msg, "Red") || !strings.Contains(msg, "Large") {
		t.Fatalf("delivered batch is missing answers: %q", msg)
	}
}

// A Cancel in any slot of a batched response cancels the whole ask, matching the
// sequential Cancel button.
func TestAsk_BatchCancelCancelsWholeAsk(t *testing.T) {
	t.Parallel()
	tool, _, _, bp, d := newAskBatchFixture(true)
	execAsk(t, tool, twoQuestions)

	bp.answer([]string{"qa:0", "qa:cancel"})

	msg, ok := d.last()
	if !ok {
		t.Fatal("no delivery after cancel")
	}
	if !strings.Contains(msg, "CANCELLED") {
		t.Fatalf("expected a cancellation message, got %q", msg)
	}
}

// A short batched response (fewer answers than questions) is NOT delivered — the
// ask stays pending rather than handing the agent a partial result.
func TestAsk_BatchShortResponseStaysPending(t *testing.T) {
	t.Parallel()
	tool, _, _, bp, d := newAskBatchFixture(true)
	execAsk(t, tool, twoQuestions)

	bp.answer([]string{"qa:0"}) // only the first of two

	if _, ok := d.last(); ok {
		t.Fatal("a partial batch was delivered; want the ask to stay pending")
	}
}
