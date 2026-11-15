package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/turnevent"
)

// #3120 (Dick 2026-11-14/15): Q57 "Work turns steer" — user messages steer into
// turns started by a spawn result, inter-session reply, remind wake or
// compaction wake, while keepalive, reflection and cron stay protected (#2591).
// Q60 "Batch" — consecutive queued system injections run as ONE turn, keeping
// arrival order relative to user messages (Q58).

// deliveringCtx is a turn ctx whose sink delivers to a platform chat, as
// deliverToSessionChatThen's injected turns have (turnSinkForConn).
func deliveringCtx() context.Context {
	return turnevent.WithSink(context.Background(), fnSink(func(context.Context, turnevent.Event) {}))
}

// TestWorkTurn_UserInputFoldsIntoWorkTurn: a user message arriving while a
// work turn runs folds into it at once — no wait — and the turn is not marked
// protected. Fail-arm: the same triggers before #3120 were protected and the
// message waited (TestNoFold_UserInputWaitsOutSystemTurn's shape).
func TestWorkTurn_UserInputFoldsIntoWorkTurn(t *testing.T) {
	t.Parallel()
	for _, trigger := range []string{"async_notify", "depth_results", "scheduled_wake", "compaction-resume"} {
		t.Run(trigger, func(t *testing.T) {
			t.Parallel()
			a, be, _ := newTypingTestAgent(t)
			runTypingTurn(t, a, deliveringCtx(), trigger)
			be.setTurnInFlight(true) // the work turn is still running

			if a.DelegatedManager.ProtectedTurnInFlight("test/s") {
				t.Fatalf("%s turn marked protected; Q57 rules it a work turn", trigger)
			}
			rec := &foldRecorder{}
			rec.install(be.mockBackendDT)
			be.waitForTurnFn = func(context.Context) error {
				t.Errorf("user input waited for the running %s turn; it must fold (#3120 Q57)", trigger)
				be.setTurnInFlight(false)
				return nil
			}
			runTypingTurn(t, a, context.Background(), "voice")
			if n := rec.folds.Load(); n != 1 {
				t.Fatalf("folds = %d, want the user message folded into the %s turn", n, trigger)
			}
		})
	}
}

// TestWorkTurn_ProtectedTurnsStayProtected: keepalive, reflection, cron and
// foci send (trigger "user"), webhooks and other agents' messages stay
// protected even with a delivering sink (Q57: "keepalive, reflection and cron
// stay protected"; the rest were not ruled work turns).
func TestWorkTurn_ProtectedTurnsStayProtected(t *testing.T) {
	t.Parallel()
	for _, trigger := range []string{TriggerKeepalive, "reflection", "user", "webhook", "session_notify", "tmux_watch"} {
		t.Run(trigger, func(t *testing.T) {
			t.Parallel()
			a, be, _ := newTypingTestAgent(t)
			runTypingTurn(t, a, deliveringCtx(), trigger)
			be.setTurnInFlight(true)
			if !a.DelegatedManager.ProtectedTurnInFlight("test/s") {
				t.Fatalf("%s turn not protected (#2591)", trigger)
			}
		})
	}
}

// TestWorkTurn_CaptureTurnStaysProtected: the target turn of a
// send_to_session reply_to=caller message runs under async_notify with a
// capture sink (BufferSink): its output goes to the caller, not to the chat,
// so a user message folded into it would be answered to the caller. It stays
// protected and the message waits.
func TestWorkTurn_CaptureTurnStaysProtected(t *testing.T) {
	t.Parallel()
	a, be, _ := newTypingTestAgent(t)
	capture := turnevent.WithSink(context.Background(), turnevent.NewBufferSink())
	runTypingTurn(t, a, capture, "async_notify")
	be.setTurnInFlight(true)
	if !a.DelegatedManager.ProtectedTurnInFlight("test/s") {
		t.Fatal("a capture (reply_to=caller target) turn is not protected; a user message would be relayed to the caller")
	}
}

// TestWorkTurn_InboxSteersTypedMessageIntoWorkTurn: with steer mode on, a
// typed message arriving during a spawn-result turn is steered into it
// (ImmediateInject(SourceSteer)), not queued. The example in the ticket: "wait,
// keep scraper paused" reached codex 4m55s late, after the deploy it meant to
// stop.
func TestWorkTurn_InboxSteersTypedMessageIntoWorkTurn(t *testing.T) {
	t.Parallel()
	a, be, _ := newTypingTestAgent(t)
	runTypingTurn(t, a, deliveringCtx(), "scheduled_wake")
	be.setTurnInFlight(true)
	a.SetInboxSteerMode(true)
	inb := a.getOrCreateInbox("test/s")

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "wait, keep scraper paused"})

	injects := be.InjectsRecorded()
	if last := injects[len(injects)-1]; last.Source != delegator.SourceSteer || last.Text != "wait, keep scraper paused" {
		t.Fatalf("last inject = %+v, want the message steered into the work turn", last)
	}
	if q := inb.drainAvailable(); len(q) != 0 {
		t.Fatalf("message queued instead of steered: %+v", q)
	}
}

// TestWorkTurn_SkipHoldsSteersIntoWorkTurn: a spawn result arriving while a
// work turn runs folds into it (it used to queue behind the "system" turn and
// then run in a turn of its own).
func TestWorkTurn_SkipHoldsSteersIntoWorkTurn(t *testing.T) {
	t.Parallel()
	a, be, _ := newTypingTestAgent(t)
	runTypingTurn(t, a, deliveringCtx(), "async_notify")
	be.setTurnInFlight(true)

	var ran, steered atomic.Int32
	a.Enqueue(skipHoldsInject("test/s", "[SPAWN RESULT] two", func() { ran.Add(1) }, func() { steered.Add(1) }))
	if ran.Load() != 0 || steered.Load() != 1 {
		t.Fatalf("ran=%d steered=%d, want the spawn result steered into the work turn", ran.Load(), steered.Load())
	}
}

// --- Q60: queued injections share one turn ---

// steerTexts returns the texts of the SourceSteer injects be recorded, in order.
func steerTexts(be *mockBackendDT) []string {
	var out []string
	for _, inj := range be.InjectsRecorded() {
		if inj.Source == delegator.SourceSteer {
			out = append(out, inj.Text)
		}
	}
	return out
}

// pendingTexts lists the worker queue's entries by text ("user:" prefix for a
// user message).
func pendingTexts(inb *sessionInbox) []string {
	inb.pendMu.Lock()
	defer inb.pendMu.Unlock()
	var out []string
	for _, e := range inb.pending {
		if e.Inject == nil {
			out = append(out, "user:"+e.Text)
			continue
		}
		out = append(out, e.Text)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestBatch_QueuedInjectsFoldIntoTheHeadTurn drives the real path: the worker
// runs the head injection (runInject), whose Run begins a work turn; the
// spawn results queued behind it fold into that turn in order, and never run
// a turn of their own. A user message queued after them stays queued, after
// them (Q58: arrival order), and so does what follows it.
func TestBatch_QueuedInjectsFoldIntoTheHeadTurn(t *testing.T) {
	t.Parallel()
	a, be, _ := newTypingTestAgent(t)
	be.sendToPaneFn = func(context.Context, string, *mockHandler) (*delegator.TurnResult, error) {
		be.setTurnInFlight(true) // the head's turn is running from its begin on
		return &delegator.TurnResult{Text: "ok"}, nil
	}
	inb := a.getOrCreateInbox("test/s")

	var mu sync.Mutex
	var order []string
	note := func(s string) func() { return func() { mu.Lock(); order = append(order, s); mu.Unlock() } }
	inb.pushPending(
		skipHoldsInject("test/s", "two", note("ran two"), note("steered two")),
		skipHoldsInject("test/s", "three", note("ran three"), note("steered three")),
		Envelope{SessionKey: "test/s", Text: "user msg"},
		skipHoldsInject("test/s", "four", note("ran four"), note("steered four")),
	)
	head := skipHoldsInject("test/s", "one", func() {
		runTypingTurn(t, a, WithSkipHolds(deliveringCtx()), "async_notify")
	}, nil)

	a.runInject(inb, head)

	if got := steerTexts(be.mockBackendDT); !equalStrings(got, []string{"two", "three"}) {
		t.Fatalf("folded = %v, want [two three] folded into the head's turn, in order", got)
	}
	mu.Lock()
	if !equalStrings(order, []string{"steered two", "steered three"}) {
		t.Errorf("hooks = %v, want Steered for two and three only", order)
	}
	mu.Unlock()
	if got := pendingTexts(inb); !equalStrings(got, []string{"user:user msg", "four"}) {
		t.Fatalf("queue = %v, want the user message, then four (nothing reordered past the user message)", got)
	}
	if inb.foldArmed.Load() {
		t.Error("foldArmed still set after the injection ran")
	}
}

// TestBatch_ProtectedHeadFoldsNothing: a keepalive or cron turn is protected
// (#2591), so the messages queued behind it wait and run after it.
func TestBatch_ProtectedHeadFoldsNothing(t *testing.T) {
	t.Parallel()
	a, be, _ := newTypingTestAgent(t)
	be.sendToPaneFn = func(context.Context, string, *mockHandler) (*delegator.TurnResult, error) {
		be.setTurnInFlight(true)
		return &delegator.TurnResult{Text: "ok"}, nil
	}
	inb := a.getOrCreateInbox("test/s")
	inb.pushPending(skipHoldsInject("test/s", "two", func() {}, func() { t.Error("folded into a protected turn") }))
	head := Envelope{SessionKey: "test/s", Inject: &InjectMeta{Trigger: "reflection", Run: func() {
		runTypingTurn(t, a, deliveringCtx(), "reflection")
	}}}

	a.runInject(inb, head)

	if got := steerTexts(be.mockBackendDT); len(got) != 0 {
		t.Fatalf("folded %v into a reflection turn, want nothing", got)
	}
	if got := pendingTexts(inb); !equalStrings(got, []string{"two"}) {
		t.Fatalf("queue = %v, want [two] still queued", got)
	}
}

// TestBatch_NonSkipHoldsInjectEndsTheWalk: an injection that keeps its holds
// (a wake, a cron, another agent's message) does not fold, and nothing queued
// behind it folds past it.
func TestBatch_NonSkipHoldsInjectEndsTheWalk(t *testing.T) {
	t.Parallel()
	a, be, _ := newTypingTestAgent(t)
	be.sendToPaneFn = func(context.Context, string, *mockHandler) (*delegator.TurnResult, error) {
		be.setTurnInFlight(true)
		return &delegator.TurnResult{Text: "ok"}, nil
	}
	inb := a.getOrCreateInbox("test/s")
	inb.pushPending(
		Envelope{SessionKey: "test/s", Text: "wake", Inject: &InjectMeta{Trigger: "scheduled_wake", Run: func() {}}},
		skipHoldsInject("test/s", "two", func() {}, func() { t.Error("folded past an earlier injection") }),
	)
	head := skipHoldsInject("test/s", "one", func() {
		runTypingTurn(t, a, deliveringCtx(), "async_notify")
	}, nil)

	a.runInject(inb, head)

	if got := steerTexts(be.mockBackendDT); len(got) != 0 {
		t.Fatalf("folded %v, want nothing (the wake heads the queue)", got)
	}
	if got := pendingTexts(inb); !equalStrings(got, []string{"wake", "two"}) {
		t.Fatalf("queue = %v, want [wake two]", got)
	}
}

// TestBatch_ChannelEntriesJoinInOrder: entries still on the channel (not yet
// moved onto the worker's queue) fold too, after those already queued.
func TestBatch_ChannelEntriesJoinInOrder(t *testing.T) {
	t.Parallel()
	a, be, _ := newTypingTestAgent(t)
	be.sendToPaneFn = func(context.Context, string, *mockHandler) (*delegator.TurnResult, error) {
		be.setTurnInFlight(true)
		return &delegator.TurnResult{Text: "ok"}, nil
	}
	inb := a.getOrCreateInbox("test/s")
	inb.pushPending(skipHoldsInject("test/s", "two", func() {}, func() {}))
	inb.ch <- skipHoldsInject("test/s", "three", func() {}, func() {})
	head := skipHoldsInject("test/s", "one", func() {
		runTypingTurn(t, a, deliveringCtx(), "async_notify")
	}, nil)

	a.runInject(inb, head)

	if got := steerTexts(be.mockBackendDT); !equalStrings(got, []string{"two", "three"}) {
		t.Fatalf("folded = %v, want [two three]", got)
	}
}

// --- Q59: /stop reaches an injection's turn ---

// TestStop_CancelSessionReachesInjectedTurn: while the worker runs an
// injection, CancelSession fires its cancel (it used to find no turn cancel and
// do nothing), and an API-transport turn's ctx (bindInjectCancel) ends.
func TestStop_CancelSessionReachesInjectedTurn(t *testing.T) {
	t.Parallel()
	a := newTestAgent(t)
	inb := a.getOrCreateInbox("test/s")
	var cancelled, fired bool
	head := Envelope{SessionKey: "test/s", Inject: &InjectMeta{Trigger: "reflection", Run: func() {
		ctx, release := a.bindInjectCancel(context.Background(), "test/s")
		defer release()
		fired = a.CancelSession("test/s")
		// context.AfterFunc cancels from its own goroutine; the deadline is
		// only a hang guard.
		select {
		case <-ctx.Done():
			cancelled = true
		case <-time.After(5 * time.Second):
		}
	}}}
	a.runInject(inb, head)
	if !fired || !cancelled {
		t.Fatalf("CancelSession during an injected turn: fired=%v ctx cancelled=%v, want both", fired, cancelled)
	}
	if a.CancelSession("test/s") {
		t.Fatal("CancelSession after the injection ended still found a turn to cancel")
	}
}

// TestStop_WaitTurnStopped: the turn /stop interrupted counts as ended when the
// backend has no turn in flight, or when a different turn is live (a queued
// one began right after it); a turn the backend keeps running does not.
func TestStop_WaitTurnStopped(t *testing.T) {
	t.Parallel()
	a, be, _ := newTypingTestAgent(t)
	be.setTurnInFlight(true)
	a.setLiveTurn("test/s", "turn-1")
	if a.WaitTurnStopped(context.Background(), "test/s", "turn-1", 0) {
		t.Fatal("a turn still running reported stopped")
	}
	a.setLiveTurn("test/s", "turn-2")
	if !a.WaitTurnStopped(context.Background(), "test/s", "turn-1", 0) {
		t.Fatal("the stopped turn was replaced by a queued one, but reported still running")
	}
	be.setTurnInFlight(false)
	if !a.WaitTurnStopped(context.Background(), "test/s", "", 0) {
		t.Fatal("no turn in flight, but reported still running")
	}
}
