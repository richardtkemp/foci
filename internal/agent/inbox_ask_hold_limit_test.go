package agent

import (
	"sync/atomic"
	"testing"
	"time"
)

// testAskHoldLimit stands in for maxAskHold (5 minutes in production) so a
// test sees the limit pass in milliseconds.
const testAskHoldLimit = 300 * time.Millisecond

// TestInbox_AskHoldEndsAfterLimit is the #2828 repro: a [SPAWN RESULT]
// (trigger async_notify) arriving while a foci_ask sits unanswered was held
// until the ask resolved — 1.5 h on 3 Nov, with no limit. Dick 2026-11-03:
// "Hold up to 5 min, like the #2792 cap". The ask stays open for the whole
// test; the inject must still run once it has been held for the limit, and
// not before. RED before #2828: it stays deferred for ever.
func TestInbox_AskHoldEndsAfterLimit(t *testing.T) {
	t.Parallel()
	a, cancel := startedAgent(t)
	defer cancel()
	a.askHoldMax = testAskHoldLimit

	var cur atomic.Value
	cur.Store("ask-open") // never answered
	a.AskRouter = askRouterPending(&cur)

	sk := "test/c2828"
	ranAt := make(chan time.Time, 1)
	enqueued := time.Now()
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "async_notify",
		Run:     func() { ranAt <- time.Now() },
	}})

	inb := a.getOrCreateInbox(sk)
	if !waitFor(time.Second, func() bool {
		inb.injMu.Lock()
		defer inb.injMu.Unlock()
		return len(inb.deferredInjects) == 1
	}) {
		t.Fatal("injection was not deferred by the open ask within 1s")
	}

	select {
	case at := <-ranAt:
		if held := at.Sub(enqueued); held < testAskHoldLimit {
			t.Errorf("injection ran after %s, before the %s ask-hold limit", held, testAskHoldLimit)
		}
	case <-time.After(testAskHoldLimit + 3*time.Second):
		t.Fatalf("injection still held by the open ask %s after the %s limit; want it delivered", time.Since(enqueued).Round(time.Millisecond), testAskHoldLimit)
	}
	inb.injMu.Lock()
	n := len(inb.deferredInjects)
	inb.injMu.Unlock()
	if n != 0 {
		t.Errorf("deferredInjects = %d after release, want 0", n)
	}
}

// TestInbox_AskHoldLimitKeepsSameAskResult is the control: an injection that
// is the RESULT of the ask still on screen (AskReqID == the pending ask) is the
// same-ask race the deferral was written for (#1712). The limit does not
// release it: it waits for that ask to resolve.
func TestInbox_AskHoldLimitKeepsSameAskResult(t *testing.T) {
	t.Parallel()
	a, cancel := startedAgent(t)
	defer cancel()
	a.askHoldMax = testAskHoldLimit

	var cur atomic.Value
	cur.Store("ask-open")
	a.AskRouter = askRouterPending(&cur)

	sk := "test/c2828same"
	var ran atomic.Int32
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "ask_grader", AskReqID: "ask-open",
		Run: func() { ran.Add(1) },
	}})

	time.Sleep(testAskHoldLimit + 500*time.Millisecond)
	if ran.Load() != 0 {
		t.Fatal("result of the still-pending ask ran after the limit; want it held until that ask resolves")
	}

	cur.Store("")
	a.DrainDeferredInjects(sk, "ask-open")
	if !waitFor(time.Second, func() bool { return ran.Load() == 1 }) {
		t.Fatalf("held result did not run after its ask resolved; ran=%d", ran.Load())
	}
}

// TestInbox_AskHoldLimitCountsFromFirstDeferral: an inject released when one
// ask resolves and deferred again by the next ask keeps its first stamp, so
// asks together hold it for at most the limit, not the limit per ask.
func TestInbox_AskHoldLimitCountsFromFirstDeferral(t *testing.T) {
	t.Parallel()
	a, cancel := startedAgent(t)
	defer cancel()
	a.askHoldMax = testAskHoldLimit

	var cur atomic.Value
	cur.Store("ask-1")
	a.AskRouter = askRouterPending(&cur)

	sk := "test/c2828chain"
	ranAt := make(chan time.Time, 1)
	enqueued := time.Now()
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "async_notify",
		Run:     func() { ranAt <- time.Now() },
	}})
	inb := a.getOrCreateInbox(sk)
	deferred := func() bool {
		inb.injMu.Lock()
		defer inb.injMu.Unlock()
		return len(inb.deferredInjects) == 1
	}
	if !waitFor(time.Second, deferred) {
		t.Fatal("injection was not deferred by ask-1")
	}
	time.Sleep(testAskHoldLimit / 2)
	// ask-1 resolves and ask-2 opens at once: the drain re-enqueues the
	// inject and ask-2 defers it again.
	cur.Store("ask-2")
	a.DrainDeferredInjects(sk, "ask-1")

	select {
	case at := <-ranAt:
		if held := at.Sub(enqueued); held > testAskHoldLimit+testAskHoldLimit/4 {
			t.Errorf("injection ran after %s; the limit restarted at the second ask", held)
		}
	case <-time.After(testAskHoldLimit + 3*time.Second):
		t.Fatal("injection still held after the limit by the second ask")
	}
}
