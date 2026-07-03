package agent

import (
	"context"
	"errors"
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

// TestEnqueueInjectWait_RunsAndReturns proves the synchronous inject path:
// EnqueueInjectWait blocks until the worker has executed the run closure,
// then returns nil — the mechanism behind sync HTTP /send and the delegated
// reflection/keepalive passes.
func TestEnqueueInjectWait_RunsAndReturns(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	var ran atomic.Int32
	err := a.EnqueueInjectWait(context.Background(), "test/s", "user", func() { ran.Add(1) })
	if err != nil {
		t.Fatalf("EnqueueInjectWait: %v", err)
	}
	if ran.Load() != 1 {
		t.Fatalf("run closure executed %d times, want 1 (must complete before return)", ran.Load())
	}
}

// TestEnqueueInjectWait_CtxCancelled proves the wait respects ctx: with no
// worker running (inbox not started), the injection never executes and a
// cancelled ctx unblocks the caller with ctx.Err().
func TestEnqueueInjectWait_CtxCancelled(t *testing.T) {
	a := newTestAgent(t) // StartInbox NOT called — nothing drains the channel

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := a.EnqueueInjectWait(ctx, "test/s", "user", func() {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("EnqueueInjectWait err = %v, want context.Canceled", err)
	}
}

// TestEnqueueInjectWait_RejectedQueue proves a full inbox channel surfaces as
// an error instead of a silent drop — a sync caller must never block forever
// on an envelope that was never queued.
func TestEnqueueInjectWait_RejectedQueue(t *testing.T) {
	a := newTestAgent(t) // no worker → channel fills and stays full
	sk := "test/s"
	inb := a.getOrCreateInbox(sk)
	for i := 0; i < inboxChanSize; i++ {
		inb.ch <- Envelope{SessionKey: sk}
	}

	err := a.EnqueueInjectWait(context.Background(), sk, "user", func() {})
	if err == nil {
		t.Fatal("EnqueueInjectWait returned nil for a rejected (full-queue) envelope")
	}
}

// TestDriveAndDrainOrphans_HoldsInjectsFromExtras proves an injection envelope
// arriving during a turn is NOT folded into the follow-up platform batch by
// the post-turn orphan drain (where its Run closure would never fire and the
// injection would be lost) — it is returned to the worker and executed after
// the batch completes.
func TestDriveAndDrainOrphans_HoldsInjectsFromExtras(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	sk := "test/s"
	var ran atomic.Int32
	var batches atomic.Int32
	a.SetTurnObserver(func(_ string, batch []Envelope) {
		// While the primary turn is "running", an injection and a late
		// platform message arrive. The drain loop must run the message as a
		// follow-up turn but hand the injection back to the worker.
		if batches.Add(1) == 1 {
			a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{Trigger: "async_notify", Run: func() { ran.Add(1) }}})
			a.Enqueue(Envelope{SessionKey: sk, Text: "late follow-up", Driver: &recordingDriver{}})
		}
	})

	a.Enqueue(Envelope{SessionKey: sk, Text: "primary", Driver: &recordingDriver{}})

	if !waitFor(time.Second, func() bool { return ran.Load() == 1 }) {
		t.Fatal("injection drained during orphan-drain was never executed")
	}
	if got := batches.Load(); got != 2 {
		t.Fatalf("turn batches = %d, want 2 (primary + follow-up; inject must not fold into a batch)", got)
	}
}
