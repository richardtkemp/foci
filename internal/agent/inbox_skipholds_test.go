package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/usagehold"
)

// skipHoldsInject is the envelope shape cmd/foci-gw builds for a message
// within one agent or an async tool result (skipsHolds, #3114): Text carries
// the message so the inbox can steer it, Steered replaces Run on a steer.
func skipHoldsInject(sk, text string, run, steered func()) Envelope {
	return Envelope{
		SessionKey: sk,
		Text:       text,
		Inject:     &InjectMeta{Trigger: "async_notify", Run: run, SkipHolds: true, Steered: steered},
	}
}

// TestSkipHolds_SteersRunningUserTurn: a spawn's message to a parent whose
// user turn is running folds into that turn (Dick, Q40 "Steer in"), with the
// agent's steer mode OFF: the ruling is not the agent's typed-text preference.
// Run never fires; Steered does. Fail-arm: the same message without SkipHolds
// queues for its own turn (today's system-injection behaviour).
func TestSkipHolds_SteersRunningUserTurn(t *testing.T) {
	t.Parallel()
	for _, skip := range []bool{true, false} {
		a := newTestAgent(t) // inbox not started: a queued envelope stays observable
		a.SetInboxSteerMode(false)
		be := &recordingBackend{}
		a.SetInboxBackend(func(context.Context, string) (delegator.Delegator, error) { return be, nil })
		inb := a.getOrCreateInbox("test/s")
		inb.turnActive.Store(true)

		var ran, steered atomic.Int32
		env := skipHoldsInject("test/s", "[SPAWN RESULT] done", func() { ran.Add(1) }, func() { steered.Add(1) })
		env.Inject.SkipHolds = skip
		if !a.Enqueue(env) {
			t.Fatalf("skip=%v: Enqueue refused the injection", skip)
		}
		injects := be.Injects()
		if !skip {
			if len(injects) != 0 || steered.Load() != 0 {
				t.Fatalf("plain system injection steered (%d injects) — only SkipHolds may", len(injects))
			}
			if q := inb.drainAvailable(); len(q) != 1 {
				t.Fatalf("plain system injection not queued: channel=%d", len(q))
			}
			continue
		}
		if len(injects) != 1 || injects[0].Source != delegator.SourceSteer || injects[0].Text != "[SPAWN RESULT] done" {
			t.Fatalf("injects = %+v, want one SourceSteer of the message", injects)
		}
		if injects[0].PromptSuggestion {
			t.Error("a steered spawn message asked for a prompt suggestion: it is not user input")
		}
		if ran.Load() != 0 || steered.Load() != 1 {
			t.Errorf("ran=%d steered=%d, want Run never and Steered once", ran.Load(), steered.Load())
		}
	}
}

// TestSkipHolds_SteersAdoptedRun: an adopted autonomous run (backend turn in
// flight, worker idle) is steered into, as Q40 rules for a running
// autonomous run.
func TestSkipHolds_SteersAdoptedRun(t *testing.T) {
	t.Parallel()
	a := newTestAgent(t)
	be := &mockBackendDT{}
	be.setTurnInFlight(true)
	a.DelegatedManager = newMockDelegatedManager(t, be)

	var ran, steered atomic.Int32
	a.Enqueue(skipHoldsInject("test/s", "msg", func() { ran.Add(1) }, func() { steered.Add(1) }))

	injects := be.InjectsRecorded()
	if len(injects) != 1 || injects[0].Source != delegator.SourceSteer {
		t.Fatalf("injects = %+v, want one SourceSteer", injects)
	}
	if ran.Load() != 0 || steered.Load() != 1 {
		t.Errorf("ran=%d steered=%d, want 0 and 1", ran.Load(), steered.Load())
	}
}

// TestSkipHolds_NeverSteersSystemTurn: a running system turn (a keepalive,
// a cron) is not steered (#2591); the message queues and runs after it (Q40
// "after a system turn").
func TestSkipHolds_NeverSteersSystemTurn(t *testing.T) {
	t.Parallel()
	a := newTestAgent(t)
	be := &mockBackendDT{}
	be.setTurnInFlight(true)
	a.DelegatedManager = newMockDelegatedManager(t, be)
	a.DelegatedManager.SetProtectedTurn("test/s", true)

	a.Enqueue(skipHoldsInject("test/s", "msg", func() {}, func() { t.Error("steered into a system turn") }))

	if n := len(be.InjectsRecorded()); n != 0 {
		t.Fatalf("%d injects into a running system turn, want 0", n)
	}
	if q := a.getOrCreateInbox("test/s").drainAvailable(); len(q) != 1 {
		t.Fatalf("message not queued behind the system turn: channel=%d", len(q))
	}
}

// TestSkipHolds_UsageHoldStillHolds: the usage gates still apply (Dick: "They
// should still hold for usage gates"): no steer during a usage hold.
func TestSkipHolds_UsageHoldStillHolds(t *testing.T) {
	t.Parallel()
	a := newTestAgent(t)
	be := &mockBackendDT{}
	be.setTurnInFlight(true)
	a.DelegatedManager = newMockDelegatedManager(t, be)
	a.UsageAccountKey = "acct-3114"
	a.UsageHoldRegistry = usagehold.New()
	a.ObserveUsage(weekly(0.98))
	if a.UsageHold().Phase != usagehold.Hold {
		t.Fatalf("setup: phase %v, want hold", a.UsageHold().Phase)
	}

	a.Enqueue(skipHoldsInject("test/s", "msg", func() {}, func() { t.Error("steered during a usage hold") }))
	if n := len(be.InjectsRecorded()); n != 0 {
		t.Fatalf("%d injects during a usage hold, want 0", n)
	}
}

// TestSkipHolds_NotDeferredByOpenAsk: an open foci_ask does not defer the
// message (the #2828 hold no longer applies to it). Fail-arm: the same
// trigger without SkipHolds is deferred.
func TestSkipHolds_NotDeferredByOpenAsk(t *testing.T) {
	t.Parallel()
	for _, skip := range []bool{true, false} {
		a, cancel := startedAgent(t)
		var cur atomic.Value
		cur.Store("ask-open")
		a.AskRouter = askRouterPending(&cur)

		var ran atomic.Int32
		env := skipHoldsInject("test/s", "msg", func() { ran.Add(1) }, nil)
		env.Inject.SkipHolds = skip
		a.Enqueue(env)

		got := waitFor(500*time.Millisecond, func() bool { return ran.Load() == 1 })
		cancel()
		if skip && !got {
			t.Fatal("SkipHolds message held by an open ask (#3114: it skips the ask hold)")
		}
		if !skip && got {
			t.Fatal("fail-arm: a plain async_notify ran past an open ask; the ask hold is gone for everyone")
		}
	}
}

// TestSkipHolds_BackgroundWorkThenSteersIntoRun is the #1068 constraint: with
// background work pending and no run in flight, the message does not start a
// turn (it parks); when the autonomous run starts, it steers into that run.
func TestSkipHolds_BackgroundWorkThenSteersIntoRun(t *testing.T) {
	t.Parallel()
	a, cancel := startedAgent(t)
	defer cancel()
	a.injectGatePoll = 10 * time.Millisecond

	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setAwaiting(true)
	a.DelegatedManager = newMockDelegatedManager(t, be)

	var ran, steered atomic.Int32
	a.Enqueue(skipHoldsInject("test/s", "result", func() { ran.Add(1) }, func() { steered.Add(1) }))

	time.Sleep(200 * time.Millisecond)
	if ran.Load() != 0 || len(be.InjectsRecorded()) != 0 {
		t.Fatalf("message started a turn in the background-work window (ran=%d injects=%d) — #1068", ran.Load(), len(be.InjectsRecorded()))
	}

	be.setTurnInFlight(true) // the autonomous run starts
	if !waitFor(2*time.Second, func() bool { return steered.Load() == 1 }) {
		t.Fatalf("message not steered into the autonomous run once it started (ran=%d)", ran.Load())
	}
	injects := be.InjectsRecorded()
	if len(injects) != 1 || injects[0].Source != delegator.SourceSteer || injects[0].Text != "result" {
		t.Fatalf("injects = %+v, want one SourceSteer of the message", injects)
	}
	if ran.Load() != 0 {
		t.Error("Run fired as well as the steer")
	}
}

// withMidTurnSteer sets the manager's CapMidTurnSteer declaration: Yes for a
// backend that steers natively (ccstream, codex), No for one that does not
// (opencode).
func withMidTurnSteer(m *DelegatedManager, native bool) {
	caps := make(map[delegator.Capability]delegator.Support, len(m.Spec.Caps)+1)
	for c, s := range m.Spec.Caps {
		caps[c] = s
	}
	if native {
		caps[delegator.CapMidTurnSteer] = delegator.Yes()
	} else {
		caps[delegator.CapMidTurnSteer] = delegator.No("test: no native mid-turn steer")
	}
	m.Spec.Caps = caps
}

// TestSkipHolds_NoNativeSteer_NeverSteers: on a backend with no native
// mid-turn steer (Dick, Q45: opencode), a message within the agent never
// folds into the running turn, so no turn is aborted for it. It queues,
// marked to run ahead of the queue. Fail-arm: the same message on a backend
// that steers natively is steered (Q40 kept for ccstream, codex, API).
func TestSkipHolds_NoNativeSteer_NeverSteers(t *testing.T) {
	t.Parallel()
	for _, native := range []bool{false, true} {
		a := newTestAgent(t)
		be := &mockBackendDT{}
		be.setTurnInFlight(true)
		a.DelegatedManager = newMockDelegatedManager(t, be)
		withMidTurnSteer(a.DelegatedManager, native)

		var ran, steered atomic.Int32
		a.Enqueue(skipHoldsInject("test/s", "msg", func() { ran.Add(1) }, func() { steered.Add(1) }))

		injects := be.InjectsRecorded()
		q := a.getOrCreateInbox("test/s").drainAvailable()
		if native {
			if len(injects) != 1 || steered.Load() != 1 {
				t.Fatalf("fail-arm: native steer backend: injects=%d steered=%d, want the message steered", len(injects), steered.Load())
			}
			continue
		}
		if len(injects) != 0 || steered.Load() != 0 {
			t.Fatalf("no-native-steer backend got %d injects (steered=%d) into the running turn, want 0 (#3114 Q45)", len(injects), steered.Load())
		}
		if len(q) != 1 || !q[0].jumpQueue {
			t.Fatalf("message not queued to jump the queue: channel=%+v", q)
		}
		if ran.Load() != 0 {
			t.Error("Run fired while the turn is still running")
		}
	}
}

// TestSkipHolds_NoNativeSteer_RunsAheadOfQueue: on a backend with no native
// steer, a message within the agent that arrives while a turn runs is the
// next thing the worker runs when that turn ends, ahead of an injection that
// queued before it (Q45 "ahead of the queue"). Fail-arm: on a native-steer
// backend with no turn to steer into, it keeps plain FIFO order.
func TestSkipHolds_NoNativeSteer_RunsAheadOfQueue(t *testing.T) {
	t.Parallel()
	for _, native := range []bool{false, true} {
		a, cancel := startedAgent(t)
		be := &mockBackendDT{}
		a.DelegatedManager = newMockDelegatedManager(t, be)
		withMidTurnSteer(a.DelegatedManager, native)

		var mu sync.Mutex
		var order []string
		record := func(s string) func() {
			return func() { mu.Lock(); order = append(order, s); mu.Unlock() }
		}
		release := make(chan struct{})
		started := make(chan struct{})
		// The turn in flight: a system injection the worker runs and that
		// blocks until released.
		a.Enqueue(Envelope{SessionKey: "test/s", Inject: &InjectMeta{Trigger: "cron", Run: func() {
			close(started)
			<-release
			record("turn")()
		}}})
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			cancel()
			t.Fatal("setup: the blocking turn never started")
		}
		a.Enqueue(Envelope{SessionKey: "test/s", Inject: &InjectMeta{Trigger: "cron", Run: record("queued")}})
		a.Enqueue(skipHoldsInject("test/s", "msg", record("within-agent"), record("steered")))
		close(release)

		ok := waitFor(2*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 3 })
		cancel()
		mu.Lock()
		got := strings.Join(order, ",")
		mu.Unlock()
		if !ok {
			t.Fatalf("native=%v: order=%s, want 3 runs", native, got)
		}
		want := "turn,within-agent,queued"
		if native {
			want = "turn,queued,within-agent"
		}
		if got != want {
			t.Fatalf("native=%v: run order %s, want %s", native, got, want)
		}
	}
}

// TestPushPending_JumpQueueOrder: jumpQueue entries go ahead of every other
// queued entry, first-in first-out among themselves.
func TestPushPending_JumpQueueOrder(t *testing.T) {
	t.Parallel()
	inb := &sessionInbox{}
	mk := func(name string, jump bool) Envelope {
		return Envelope{Text: name, jumpQueue: jump}
	}
	inb.pushPending(mk("u1", false), mk("j1", true), mk("u2", false))
	inb.pushPending(mk("j2", true), mk("u3", false))
	var got []string
	for {
		env, ok := inb.popPending()
		if !ok {
			break
		}
		got = append(got, env.Text)
	}
	if s := strings.Join(got, ","); s != "j1,j2,u1,u2,u3" {
		t.Fatalf("order %s, want j1,j2,u1,u2,u3", s)
	}
}
