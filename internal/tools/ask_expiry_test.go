package tools

import (
	"strings"
	"testing"

	"foci/internal/question"
)

// #2091: an ask that dies of old age must be reported to the agent as EXPIRED,
// not as the user cancelling it. The expiry sweeps feed question.ExpiredData
// where they used to feed question.CancelData.

func assertExpiredNotice(t *testing.T, msg string, wantProgress string) {
	t.Helper()
	if strings.Contains(strings.ToUpper(msg), "CANCEL") {
		t.Errorf("expiry told the agent the user cancelled:\n%s", msg)
	}
	if !strings.Contains(msg, "expired") {
		t.Errorf("expiry notice should say the ask expired:\n%s", msg)
	}
	if !strings.Contains(msg, wantProgress) {
		t.Errorf("expiry notice should report %q answered:\n%s", wantProgress, msg)
	}
}

func TestAsk_SequentialExpiryToldExpiredNotCancelled(t *testing.T) {
	t.Parallel()
	tool, _, p, d := newAskFixture()
	execAsk(t, tool, `{"questions":[
		{"question":"Q1?","options":[{"label":"A"}]},
		{"question":"Q2?","options":[{"label":"B"}]}]}`)
	p.answer("qa:0") // Q1 answered, Q2 on screen when it expires
	p.answer(question.ExpiredData)

	msg, ok := d.last()
	if !ok {
		t.Fatal("an expiry notice should be delivered so the agent knows")
	}
	assertExpiredNotice(t, msg, "1 of 2")

	// The ask is resolved: a stray click afterwards delivers nothing more.
	d.mu.Lock()
	before := len(d.messages)
	d.mu.Unlock()
	p.answer("qa:0")
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.messages) != before {
		t.Errorf("expired ask still accepted an answer: %q", d.messages[before:])
	}
}

func TestAsk_BatchExpiryToldExpiredNotCancelled(t *testing.T) {
	t.Parallel()
	tool, _, _, bp, d := newAskBatchFixture(true)
	execAsk(t, tool, twoQuestions)

	bp.answer([]string{question.ExpiredData})

	msg, ok := d.last()
	if !ok {
		t.Fatal("an expiry notice should be delivered so the agent knows")
	}
	assertExpiredNotice(t, msg, "0 of 2")
}

// The expiry notice keeps the #1302 cold-cache gate the cancel notice it replaces
// had: an expiry fires 24h after the ask, so the cache is nearly always cold, and
// behaviour on that axis is unchanged by #2091.
func TestAsk_ExpirySuppressedWhenCacheCold(t *testing.T) {
	t.Parallel()
	p := &fakePresenter{}
	d := &fakeDeliver{}
	tool, _ := NewAskTool(p.present, nil, d.deliver, nil, nil, "test",
		WithCacheWarm(func(string) bool { return false }))
	execAsk(t, tool, `{"questions":[{"question":"Q?","options":[{"label":"A"}]}]}`)
	p.answer(question.ExpiredData)
	if msg, ok := d.last(); ok {
		t.Fatalf("cold cache: the expiry notice must be suppressed, got %q", msg)
	}
}
