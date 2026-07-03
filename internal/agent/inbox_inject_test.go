package agent

import (
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/tools"
)

// TestRunInject_DefersWhileAskPending proves the #984 gate: a proactive injection
// arriving while a foci_ask is pending is deferred (not run), a control injection
// is exempt (runs anyway), and resolving the ask redelivers the deferred one.
func TestRunInject_DefersWhileAskPending(t *testing.T) {
	var pending atomic.Bool
	pending.Store(true)
	ag := &Agent{
		AgentID: "test",
		AskRouter: &tools.AskRouter{
			PendingForSession: func(string) string {
				if pending.Load() {
					return "req1"
				}
				return ""
			},
		},
	}
	sk := "test/imain"
	inb := ag.getOrCreateInbox(sk) // inboxStarted=false → no worker drains the channel

	var ran atomic.Int32
	proactive := Envelope{SessionKey: sk, Inject: &InjectMeta{Trigger: "async_notify", Run: func() { ran.Add(1) }}}

	// Ask pending → proactive injection is deferred, not run.
	ag.runInject(inb, proactive)
	if ran.Load() != 0 {
		t.Fatalf("proactive inject ran while ask pending; want deferred")
	}
	inb.injMu.Lock()
	n := len(inb.deferredInjects)
	inb.injMu.Unlock()
	if n != 1 {
		t.Fatalf("deferredInjects = %d, want 1", n)
	}

	// Control injection is exempt — runs even while an ask is pending.
	ctrl := Envelope{SessionKey: sk, Inject: &InjectMeta{Trigger: "compaction-resume", Run: func() { ran.Add(1) }}}
	ag.runInject(inb, ctrl)
	if ran.Load() != 1 {
		t.Fatalf("control inject did not run while ask pending; ran=%d", ran.Load())
	}

	// Resolve the ask → DrainDeferredInjects re-enqueues the deferred injection.
	pending.Store(false)
	ag.DrainDeferredInjects(sk)
	select {
	case env := <-inb.ch:
		if env.Inject == nil {
			t.Fatal("re-enqueued envelope is not an injection")
		}
	default:
		t.Fatal("DrainDeferredInjects did not re-enqueue the deferred injection")
	}
	inb.injMu.Lock()
	n = len(inb.deferredInjects)
	inb.injMu.Unlock()
	if n != 0 {
		t.Fatalf("deferredInjects after drain = %d, want 0", n)
	}
}

// TestInbox_Inject_DefersThroughWorker drives the real Enqueue → sessionWorker →
// runInject path: an injection enqueued while an ask is pending is deferred by the
// worker, and redelivered (and run) once the ask resolves and the buffer is drained.
func TestInbox_Inject_DefersThroughWorker(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	var pending atomic.Bool
	pending.Store(true)
	a.AskRouter = &tools.AskRouter{
		PendingForSession: func(string) string {
			if pending.Load() {
				return "ask-1"
			}
			return ""
		},
		IsPaused: func(string) bool { return false },
	}

	sk := "test/s"
	var ran atomic.Int32
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{Trigger: "async_notify", Run: func() { ran.Add(1) }}})

	inb := a.getOrCreateInbox(sk)
	if !waitFor(time.Second, func() bool {
		inb.injMu.Lock()
		defer inb.injMu.Unlock()
		return len(inb.deferredInjects) == 1
	}) {
		t.Fatal("injection was not deferred by the worker within 1s")
	}
	if ran.Load() != 0 {
		t.Fatalf("deferred injection ran while ask pending; ran=%d", ran.Load())
	}

	pending.Store(false)
	a.DrainDeferredInjects(sk)
	if !waitFor(time.Second, func() bool { return ran.Load() == 1 }) {
		t.Fatalf("redelivered injection did not run after ask resolved; ran=%d", ran.Load())
	}
}
