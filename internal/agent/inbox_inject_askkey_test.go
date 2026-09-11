package agent

import (
	"sync/atomic"
	"testing"

	"foci/internal/tools"
)

// askRouterPending builds an AskRouter whose PendingForSession reports whatever
// *cur holds, so a test can open and close asks mid-flight.
func askRouterPending(cur *atomic.Value) *tools.AskRouter {
	return &tools.AskRouter{
		PendingForSession: func(string) string {
			v, _ := cur.Load().(string)
			return v
		},
		IsPaused: func(string) bool { return false },
	}
}

// TestRunInject_AskResultNotHeldByALaterAsk is #1712's root case. A quiz agent
// answers-then-immediately-asks, so the grader verdict for ask N is routinely
// ready a few seconds AFTER the agent has already opened ask N+1. Blanket
// deferral held that verdict behind the new ask — leaving the agent permanently
// one verdict behind (3h30m in the reported incident, and unbounded if the user
// stops answering, since the held verdict is the very thing that would tell the
// agent the previous group was graded).
//
// The verdict carries the requestID of the ask it came FROM, so the gate can tell
// it apart from a proactive interruption of the ask that is actually on screen.
func TestRunInject_AskResultNotHeldByALaterAsk(t *testing.T) {
	var cur atomic.Value
	cur.Store("ask-N1") // ask N is on screen
	ag := &Agent{AgentID: "test", AskRouter: askRouterPending(&cur)}
	sk := "test/imain"
	inb := ag.getOrCreateInbox(sk)

	var ran atomic.Int32
	// The user answers ask N; the agent immediately opens ask N+1.
	cur.Store("ask-N2")
	// Six seconds later the grader finishes and the verdict for ask N arrives.
	verdict := Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "ask_grader", AskReqID: "ask-N1", Run: func() { ran.Add(1) },
	}}
	ag.runInject(inb, verdict)

	if ran.Load() != 1 {
		t.Errorf("verdict for the RESOLVED ask ask-N1 did not run while ask-N2 is pending; want it delivered, not deferred")
	}
	inb.injMu.Lock()
	n := len(inb.deferredInjects)
	inb.injMu.Unlock()
	if n != 0 {
		t.Errorf("deferredInjects = %d, want 0 — a verdict for ask N must not be held by ask N+1", n)
	}
}

// TestRunInject_AskResultStillHeldBySameAsk is the control for the test above:
// direction (a) (exempt ask_grader wholesale) was rejected precisely because it
// would open the SAME-ask race the gate was written for. An injection carrying the
// requestID of the ask that is still on screen is still deferred.
func TestRunInject_AskResultStillHeldBySameAsk(t *testing.T) {
	var cur atomic.Value
	cur.Store("ask-N1")
	ag := &Agent{AgentID: "test", AskRouter: askRouterPending(&cur)}
	sk := "test/imain"
	inb := ag.getOrCreateInbox(sk)

	var ran atomic.Int32
	ag.runInject(inb, Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "ask_grader", AskReqID: "ask-N1", Run: func() { ran.Add(1) },
	}})
	if ran.Load() != 0 {
		t.Errorf("injection for the ask that is STILL pending ran; want deferred (the same-ask race stays closed)")
	}
	inb.injMu.Lock()
	n := len(inb.deferredInjects)
	inb.injMu.Unlock()
	if n != 1 {
		t.Fatalf("deferredInjects = %d, want 1", n)
	}
}

// TestDrainDeferredInjects_ReleasesOnlyThatAsksBacklog proves the drain is keyed
// per ask (#1711 ruling 5 → #1712). With two asks live on one session, resolving
// one must release only what IT was holding: draining everything would let a
// proactive injection race the ask still on the user's screen.
func TestDrainDeferredInjects_ReleasesOnlyThatAsksBacklog(t *testing.T) {
	var cur atomic.Value
	ag := &Agent{AgentID: "test", AskRouter: askRouterPending(&cur)}
	sk := "test/imain"
	inb := ag.getOrCreateInbox(sk)

	var ranA, ranB atomic.Int32
	cur.Store("ask-A")
	ag.runInject(inb, Envelope{SessionKey: sk, Inject: &InjectMeta{Trigger: "async_notify", Run: func() { ranA.Add(1) }}})
	cur.Store("ask-B")
	ag.runInject(inb, Envelope{SessionKey: sk, Inject: &InjectMeta{Trigger: "async_notify", Run: func() { ranB.Add(1) }}})

	inb.injMu.Lock()
	n := len(inb.deferredInjects)
	inb.injMu.Unlock()
	if n != 2 {
		t.Fatalf("deferredInjects = %d, want 2 (one held by each ask)", n)
	}

	// Ask A resolves. Only A's backlog is released; B is still on screen.
	ag.DrainDeferredInjects(sk, "ask-A")
	select {
	case env := <-inb.ch:
		if env.Inject == nil {
			t.Fatal("re-enqueued envelope is not an injection")
		}
	default:
		t.Fatal("draining ask-A did not re-enqueue the injection it was holding")
	}
	select {
	case <-inb.ch:
		t.Fatal("draining ask-A also released the injection held by ask-B, which is still pending")
	default:
	}
	inb.injMu.Lock()
	n = len(inb.deferredInjects)
	held := ""
	if n == 1 {
		held = inb.deferredInjects[0].heldBy
	}
	inb.injMu.Unlock()
	if n != 1 || held != "ask-B" {
		t.Fatalf("after draining ask-A: %d deferred (heldBy=%q), want 1 held by ask-B", n, held)
	}

	ag.DrainDeferredInjects(sk, "ask-B")
	select {
	case <-inb.ch:
	default:
		t.Fatal("draining ask-B did not re-enqueue the injection it was holding")
	}
}
