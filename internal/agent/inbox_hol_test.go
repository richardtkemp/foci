package agent

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
)

// gateWatcherParked reports whether sk's gate watcher has entered its wait
// loop — inFlightChanged gains an entry exactly when InFlightWaitCh is called
// (nothing else creates it in these tests), which happens only inside
// waitInjectGate's gated loop. Deterministic proof that an inject is HELD, not
// merely dequeued.
func gateWatcherParked(a *Agent, sk string) bool {
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	_, gated := a.inFlightChanged[sk]
	return gated
}

// TestInbox_UserMessageRunsWhileInjectHeld is the #2233 head-of-line repro
// (dequeue path): a system inject parked by the pending-work gate must not
// stop the session from serving a later user message — the user turn drives
// while the inject is still held, and the inject runs once the gate opens.
// The same body pins the watcher's goroutine lifecycle: after the inject has
// run, the watcher goroutine is gone (no leak).
func TestInbox_UserMessageRunsWhileInjectHeld(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setAwaiting(true)
	mgr := newMockDelegatedManager(t, be)
	a.DelegatedManager = mgr

	sk := "test/s"
	// The inbox (and its worker) must exist before the baseline: the worker
	// goroutine is permanent, and counting it would read as a leak.
	a.getOrCreateInbox(sk)
	base := runtime.NumGoroutine()

	var ran atomic.Int32
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "reflection",
		Run:     func() { ran.Add(1) },
	}})
	if !waitFor(2*time.Second, func() bool { return gateWatcherParked(a, sk) }) {
		t.Fatal("the injection never parked in the gate")
	}

	// A user message on the same session must drive as a normal user turn —
	// the held inject no longer blocks the worker (#2233).
	hook := make(chan struct{}, 1)
	done := make(chan struct{}, 1)
	d := &recordingDriver{hookCh: hook, doneCh: done}
	a.SetTurnObserver(d.recordBatch)
	a.Enqueue(Envelope{SessionKey: sk, Text: "user speaks", Driver: d})
	select {
	case <-hook:
	case <-time.After(time.Second):
		t.Fatal("user message waited behind a held system inject (head-of-line block, #2233)")
	}
	if ran.Load() != 0 {
		t.Fatal("inject ran although the gate was still closed")
	}
	<-done

	// The gate opens; the parked inject runs.
	be.setAwaiting(false)
	if !waitFor(2*time.Second, func() bool { return ran.Load() == 1 }) {
		t.Fatalf("inject did not run after the gate opened; ran=%d", ran.Load())
	}

	// Watcher lifecycle: the watcher goroutine stopped once the held list
	// emptied — the count returns to the baseline taken before the enqueue.
	if !waitFor(2*time.Second, func() bool { return runtime.NumGoroutine() <= base }) {
		t.Fatalf("gate watcher goroutine leaked: %d goroutines, baseline %d", runtime.NumGoroutine(), base)
	}
}

// TestInbox_UserMessageRunsWhileDrainedInjectHeld is the same head-of-line
// repro for the SECOND inject site: an inject drained alongside a platform
// batch parks on the held list after the batch, and a later user message
// still drives while it waits.
func TestInbox_UserMessageRunsWhileDrainedInjectHeld(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setAwaiting(true) // the gate is closed for the whole test until released
	mgr := newMockDelegatedManager(t, be)
	a.DelegatedManager = mgr

	sk := "test/s"
	hook := make(chan struct{}, 4)
	done := make(chan struct{}, 4)
	d := &recordingDriver{hookCh: hook, doneCh: done}
	a.SetTurnObserver(d.recordBatch)

	// First user turn drives (a platform turn is never held on pending work).
	a.Enqueue(Envelope{SessionKey: sk, Text: "first", Driver: d})
	<-hook
	// While the batch is in flight, the inject lands on the channel and is
	// drained alongside it — the post-batch dispatch then parks it.
	var ran atomic.Int32
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "reflection",
		Run:     func() { ran.Add(1) },
	}})
	<-done
	if !waitFor(2*time.Second, func() bool { return gateWatcherParked(a, sk) }) {
		t.Fatal("the drained injection never parked in the gate")
	}

	// A later user message drives while the drained inject is parked.
	a.Enqueue(Envelope{SessionKey: sk, Text: "second", Driver: d})
	select {
	case <-hook:
	case <-time.After(time.Second):
		t.Fatal("user message waited behind a held drained inject (head-of-line block, #2233)")
	}
	<-done
	if ran.Load() != 0 {
		t.Fatal("drained inject ran although the gate was still closed")
	}

	be.setAwaiting(false)
	if !waitFor(2*time.Second, func() bool { return ran.Load() == 1 }) {
		t.Fatalf("drained inject did not run after the gate opened; ran=%d", ran.Load())
	}
}

// TestInbox_UserMessageRunsWhileInjectHeldDuringSlotWait is the THIRD inject
// site: an inject that parks while the worker sits in a turn-limit slot wait
// (waitTurnSlot). The waiting user batch still drives once the slot frees —
// the parked inject waits for it — and then runs when the gate opens.
func TestInbox_UserMessageRunsWhileInjectHeldDuringSlotWait(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.TurnLimit = NewTurnLimiter(1)

	releaseA := make(chan struct{})
	dA := &driverGated{ready: make(chan struct{}, 1), release: releaseA}
	dB := &driverGated{ready: make(chan struct{}, 1), done: make(chan struct{}, 1)}
	notices := newNoticeRecorder()
	a.TurnQueuedNotifyFunc = notices.hook()

	const skB = "sess/B"
	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "a", Driver: dA})
	mustRecv(t, dA.ready, "A to hold the only slot")
	a.Enqueue(Envelope{SessionKey: skB, Text: "b", Driver: dB})
	mustRecv(t, notices.signal, "B's user batch to be waiting for its slot")

	// Close the gate and let the slot-waiting worker park an inject.
	releaseInFlight := a.markInFlight(skB, true)
	var ran atomic.Int32
	a.Enqueue(Envelope{SessionKey: skB, Inject: &InjectMeta{
		Trigger: "reflection",
		Run:     func() { ran.Add(1) },
	}})
	if !waitFor(2*time.Second, func() bool { return gateWatcherParked(a, skB) }) {
		t.Fatal("the injection never parked in the gate during the slot wait")
	}

	// The slot frees: B's user batch drives — the parked inject did not
	// swallow the worker.
	close(releaseA)
	select {
	case <-dB.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting user batch never drove after the slot freed — the parked inject holds the worker (head-of-line block, #2233)")
	}
	<-dB.done
	if ran.Load() != 0 {
		t.Fatal("inject ran although the gate was still closed")
	}

	// The gate opens; the inject runs.
	releaseInFlight()
	if !waitFor(2*time.Second, func() bool { return ran.Load() == 1 }) {
		t.Fatalf("inject did not run after the gate opened; ran=%d", ran.Load())
	}
}

// TestInbox_HeldInjectsKeepFIFO pins that held injects keep arrival order
// among themselves once the gate opens (characterisation: the single worker
// was already serial; the held-list restructure must not reorder).
func TestInbox_HeldInjectsKeepFIFO(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setAwaiting(true)
	a.DelegatedManager = newMockDelegatedManager(t, be)

	sk := "test/s"
	var mu sync.Mutex
	var order []string
	mk := func(tag string) *InjectMeta {
		return &InjectMeta{Trigger: "reflection", Run: func() {
			mu.Lock()
			order = append(order, tag)
			mu.Unlock()
		}}
	}
	a.Enqueue(Envelope{SessionKey: sk, Inject: mk("first")})
	a.Enqueue(Envelope{SessionKey: sk, Inject: mk("second")})
	a.Enqueue(Envelope{SessionKey: sk, Inject: mk("third")})
	if !waitFor(2*time.Second, func() bool { return gateWatcherParked(a, sk) }) {
		t.Fatal("no injection ever parked in the gate")
	}

	be.setAwaiting(false)
	if !waitFor(2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 3
	}) {
		t.Fatalf("injects did not all run; order=%v", order)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, want := range []string{"first", "second", "third"} {
		if order[i] != want {
			t.Fatalf("held injects ran out of FIFO order: %v", order)
		}
	}
}

// workerlessInbox returns sk's inbox on an agent whose inbox was never started
// (no StartInbox), so no session worker exists to touch the worker-confined
// held list: the test goroutine is its only user and may park, release and
// inspect entries by hand. On a started agent the worker's loop-top
// releaseHeld races any such hand edit (#2447: the race detector flags it
// every run, and a late-scheduled worker panicked releaseHeld with a slice
// out of range) — hence the guard rather than a comment.
func workerlessInbox(t *testing.T, a *Agent, sk string) *sessionInbox {
	t.Helper()
	a.inboxesMu.Lock()
	started := a.inboxStarted
	a.inboxesMu.Unlock()
	if started {
		t.Fatal("workerlessInbox on a started agent: a live session worker would race the test's hand edits of inb.held (#2447)")
	}
	return a.getOrCreateInbox(sk)
}

// TestInbox_HeldInjectRecheckedBeforeRun pins that the hold is re-evaluated
// right before a held inject runs (characterisation of releaseHeld's
// decision): a gate that closed again between the watcher's signal and the
// release keeps the inject parked.
func TestInbox_HeldInjectRecheckedBeforeRun(t *testing.T) {
	a := newTestAgent(t)

	sk := "test/s"
	var ran atomic.Int32
	env := Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "reflection",
		Run:     func() { ran.Add(1) },
	}}
	inb := workerlessInbox(t, a, sk)

	// Park by hand (worker-less, so the held list is the test's alone) and
	// close the gate.
	inb.held = []Envelope{env}
	release := a.markInFlight(sk, true)
	defer release()

	// Gate closed: the release attempt leaves the inject parked.
	if !a.releaseHeld(context.Background(), inb) {
		t.Fatal("releaseHeld returned false with the ctx alive")
	}
	if len(inb.held) != 1 || ran.Load() != 0 {
		t.Fatalf("inject left the held list while the gate was closed: held=%d ran=%d", len(inb.held), ran.Load())
	}

	// Gate open: the same call runs it.
	release()
	if !a.releaseHeld(context.Background(), inb) {
		t.Fatal("releaseHeld returned false with the ctx alive")
	}
	if len(inb.held) != 0 || ran.Load() != 1 {
		t.Fatalf("inject did not run on the open gate: held=%d ran=%d", len(inb.held), ran.Load())
	}
}

// TestInbox_HeldInjectRefusedOnCtxEnd pins #2059 through the held list: an
// inject parked by the gate when the inbox ctx ends is refused (its waiter
// released with ErrShuttingDown), never silently dropped.
func TestInbox_HeldInjectRefusedOnCtxEnd(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setAwaiting(true)
	a.DelegatedManager = newMockDelegatedManager(t, be)

	sk := "test/s"
	errc := make(chan error, 1)
	go func() {
		ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		errc <- a.EnqueueInjectWait(ctx, sk, "scheduled_wake", func() {})
	}()
	if !waitFor(2*time.Second, func() bool { return gateWatcherParked(a, sk) }) {
		t.Fatal("the injection never parked in the gate")
	}

	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("EnqueueInjectWait err = %v, want ErrShuttingDown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("held inject dropped without Refused when the inbox ctx ended")
	}
}

// TestInbox_HeldInjectRefusedOnShutdown pins the shutdown arm of the same
// contract: BeginShutdown opens the gate so the held inject reaches
// runInject, whose drain gate refuses it (#2059) — run, never a silent drop.
func TestInbox_HeldInjectRefusedOnShutdown(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setAwaiting(true)
	a.DelegatedManager = newMockDelegatedManager(t, be)

	sk := "test/s"
	var ran atomic.Int32
	refused := make(chan struct{}, 1)
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "scheduled_wake",
		Run:     func() { ran.Add(1) },
		Refused: func() { refused <- struct{}{} },
	}})
	if !waitFor(2*time.Second, func() bool { return gateWatcherParked(a, sk) }) {
		t.Fatal("the injection never parked in the gate")
	}

	a.BeginShutdown()
	select {
	case <-refused:
	case <-time.After(2 * time.Second):
		t.Fatal("held inject was not refused at shutdown")
	}
	if ran.Load() != 0 {
		t.Fatal("held inject ran although the agent is draining")
	}
}

// TestInbox_WatchHintDuringUserTurn_InjectStaysHeld pins the watcher-hint
// protocol (SPEC-REVIEW note 1): the watcher's waitInjectGate returns true
// while the worker runs a turn (holdActive's busy refinement), so its signal
// is only a hint — after a user turn drives, the inject must STAY held while
// background work is still in flight, and run only when it actually clears.
func TestInbox_WatchHintDuringUserTurn_InjectStaysHeld(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	sk := "test/s"
	// The gate is closed by an adopted delivering run.
	releaseInFlight := a.markInFlight(sk, true)
	defer releaseInFlight()

	var ran atomic.Int32
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "reflection",
		Run:     func() { ran.Add(1) },
	}})
	if !waitFor(2*time.Second, func() bool { return gateWatcherParked(a, sk) }) {
		t.Fatal("the injection never parked in the gate")
	}

	// A user turn drives on the same session (the platform path is never held
	// on the adopted run). While it runs the watcher may signal "open" (the
	// busy refinement hides the worker's own turn) — the re-check must keep
	// the inject parked because the adopted run is still in flight.
	hook := make(chan struct{}, 1)
	done := make(chan struct{}, 1)
	d := &recordingDriver{hookCh: hook, doneCh: done, delay: 50 * time.Millisecond}
	a.SetTurnObserver(d.recordBatch)
	a.Enqueue(Envelope{SessionKey: sk, Text: "during", Driver: d})
	<-hook
	<-done
	// Give any misfired watcher signal time to be (wrongly) acted on.
	time.Sleep(300 * time.Millisecond)
	if ran.Load() != 0 {
		t.Fatal("inject ran off a watcher hint while the adopted run was still in flight")
	}

	// The real open: the adopted run ends and the inject runs.
	releaseInFlight()
	if !waitFor(2*time.Second, func() bool { return ran.Load() == 1 }) {
		t.Fatalf("inject did not run after the adopted run ended; ran=%d", ran.Load())
	}
}

// TestInbox_HeldWatchStopsOnWorkerExit pins the exit arm of the watcher
// lifecycle: the worker's defer stops the watcher, so a held inject left at
// worker exit does not leak its goroutine.
func TestInbox_HeldWatchStopsOnWorkerExit(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setAwaiting(true)
	a.DelegatedManager = newMockDelegatedManager(t, be)

	sk := "test/s"
	// The inbox (and its worker) must exist before the baseline: the worker
	// goroutine is permanent, and counting it would read as a leak.
	a.getOrCreateInbox(sk)
	base := runtime.NumGoroutine()
	refused := make(chan struct{}, 1)
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "reflection",
		Run:     func() {},
		Refused: func() { refused <- struct{}{} },
	}})
	if !waitFor(2*time.Second, func() bool { return gateWatcherParked(a, sk) }) {
		t.Fatal("the injection never parked in the gate")
	}

	cancel()
	select {
	case <-refused:
	case <-time.After(2 * time.Second):
		t.Fatal("held inject was not refused at worker exit")
	}
	if !waitFor(2*time.Second, func() bool { return runtime.NumGoroutine() <= base }) {
		t.Fatalf("watcher goroutine outlived the worker: %d goroutines, baseline %d", runtime.NumGoroutine(), base)
	}
}

// TestInbox_HeldInjectRefusedOnceWhenCtxEndsMidRelease pins the #2233 review
// F1 exit path: the inbox ctx ends while releaseHeld is mid-release — the
// head inject's hold has cleared but the re-login gate keeps the worker
// inside waitReloginGate. The envelope's Refused hook must fire EXACTLY ONCE
// (refuseHeld on the worker's exit path owns it; releaseHeld must not fire
// it too — a second firing closes an already-closed channel in an
// EnqueueInjectWait caller and re-runs the wake refusal hook), and the inject
// must never run.
func TestInbox_HeldInjectRefusedOnceWhenCtxEndsMidRelease(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setAwaiting(true)
	a.DelegatedManager = newMockDelegatedManager(t, be)

	sk := "test/s"
	var refused atomic.Int32
	var ran atomic.Int32
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "reflection",
		Run:     func() { ran.Add(1) },
		Refused: func() { refused.Add(1) },
	}})
	if !waitFor(2*time.Second, func() bool { return gateWatcherParked(a, sk) }) {
		t.Fatal("the injection never parked in the gate")
	}

	// The re-login gate is claimed BEFORE the hold clears, so the release
	// parks the worker inside waitReloginGate on the head instead of running
	// it. The watcher notices the cleared hold within its 1s poll backstop.
	claimRelogin(t)
	be.setAwaiting(false)
	time.Sleep(1500 * time.Millisecond)

	// The inbox ctx ends mid-release: the worker returns and its exit path
	// refuses every still-held inject — exactly once each.
	cancel()
	if !waitFor(2*time.Second, func() bool { return refused.Load() >= 1 }) {
		t.Fatal("held inject was not refused when the inbox ctx ended mid-release")
	}
	time.Sleep(300 * time.Millisecond)
	if got := refused.Load(); got != 1 {
		t.Fatalf("held inject refused %d times; want exactly 1 (double refusal, #2233 review F1)", got)
	}
	if ran.Load() != 0 {
		t.Fatal("the inject ran although the ctx ended mid-release")
	}
}

// keepaliveExemptGateBackend is a mockBackendDT whose ImmediateInject records
// every inject, so tests can assert a keepalive began while work was pending.
type sourceRecordingBackend struct {
	mockBackendDT
	mu      sync.Mutex
	injects []delegator.Inject
}

func (b *sourceRecordingBackend) ImmediateInject(_ context.Context, inj delegator.Inject) error {
	b.mu.Lock()
	b.injects = append(b.injects, inj)
	b.mu.Unlock()
	return b.mockBackendDT.ImmediateInject(nil, inj)
}

// TestInbox_UserMessageRunsWhileKeepaliveParksBehindAdoptedRun is the review-F1
// head-of-line repro: a keepalive arriving while an ADOPTED autonomous run
// delivers (and pending background work is outstanding) must PARK on the held
// list — not run on the worker and sit in RunInference's SourceSystem retry
// loop until the run ends, which is exactly the block requirement 1 removes.
// A non-steering user message on the same session drives while the keepalive
// waits, and the keepalive runs the moment the adopted run ends — while
// pending work is STILL outstanding, because background work never holds a
// keepalive. The user turn is long enough to cross the watcher's poll tick, so
// a (spurious) hint is consumed mid-test and the spent watch must be dropped
// and re-armed for the real release to be signalled.
func TestInbox_UserMessageRunsWhileKeepaliveParksBehindAdoptedRun(t *testing.T) {
	initTestConvo(t, "helen")
	const sk = "helen/c42"

	// The adopted run's in-flight turn; runEnded releases the SourceSystem
	// retry loop a wrongly-run keepalive would sit in.
	runEnded := make(chan struct{})
	be := &mockBackendDT{sessionFile: "/tmp/ka-hol.jsonl"}
	be.setAwaiting(true)     // pending background work is outstanding for the whole test
	be.setTurnInFlight(true) // the adopted run is the backend's in-flight turn
	be.waitForTurnFn = func(ctx context.Context) error {
		select {
		case <-runEnded:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	be.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
		if h != nil && h.OnTurnComplete != nil {
			h.OnTurnComplete(&delegator.TurnResult{Text: "ka", Model: "claude-sonnet-4-5"})
		}
		return nil, nil
	}
	a := newBatchTestAgent(t, be)
	// Register the backend under sk so the manager's pending-work probe
	// (BackendAwaitingAutonomousRun) actually consults THIS mock — without
	// this, be.setAwaiting is invisible for a session no Get ever touched.
	if _, err := a.DelegatedManager.Get(context.Background(), sk); err != nil {
		t.Fatalf("pre-register backend: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartInbox(ctx)

	// The adopted delivering run: in flight, delivering, worker idle.
	releaseRun := a.markInFlight(sk, true)

	kaDone := make(chan struct{}, 1)
	var kaErr error
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: TriggerKeepalive,
		Run: func() {
			kaErr = a.HandleMessage(WithTrigger(ctx, TriggerKeepalive), sk, []string{"keepalive"}, nil)
			kaDone <- struct{}{}
		},
	}})
	// Parked (green run); on the defective code the worker is already stuck
	// inside the keepalive instead — either way, proceed after a grace.
	waitFor(time.Second, func() bool { return gateWatcherParked(a, sk) })

	// A non-steering user message must drive while the keepalive waits.
	hook := make(chan struct{}, 1)
	done := make(chan struct{}, 1)
	d := &recordingDriver{hookCh: hook, doneCh: done, delay: 1500 * time.Millisecond}
	a.SetTurnObserver(d.recordBatch)
	a.Enqueue(Envelope{SessionKey: sk, Text: "user speaks", Driver: d})
	select {
	case <-hook:
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("user message waited behind a keepalive the gate did not park (head-of-line block, #2233 review F1)")
	}
	<-done
	select {
	case <-kaDone:
		t.Fatal("keepalive ran while the adopted delivering run was still in flight")
	default:
	}

	// The adopted run ends; pending work stays outstanding. The keepalive
	// must run NOW — background work never holds one.
	close(runEnded)
	be.setTurnInFlight(false)
	releaseRun()
	select {
	case <-kaDone:
	case <-time.After(2 * time.Second):
		t.Fatal("keepalive stayed parked after its adopted run ended — pending work must never hold it (#2233)")
	}
	if kaErr != nil {
		t.Fatalf("keepalive turn: %v", kaErr)
	}

	// Its turn began while work was pending: the backend-layer exemption.
	exempt := false
	for _, inj := range be.InjectsRecorded() {
		if inj.PendingWorkExempt {
			exempt = true
		}
	}
	if !exempt {
		t.Fatal("the keepalive turn reached the backend without PendingWorkExempt — the backend would have refused it while work is pending")
	}
}

// TestDispatchInject_KeepaliveNotParkedBehindPendingHeldSibling pins the
// dispatch half of the review-F1 fix at its decision point: a keepalive whose
// own hold is clear (no adopted run) RUNS when it leaves the queue, never
// parks behind an earlier sibling that pending work holds — queueing there is
// what stalled the cache-warming pass behind the sibling's whole
// background-work window. The same-class control pins the other arm of the
// same rule: a plain system inject DOES park behind a parked plain sibling
// even while the hold reads open (the flicker-FIFO arm). White-box on a
// WORKER-LESS agent (no StartInbox, so nothing drains the held list besides
// the test itself — the worker-confined held list is quiescent by
// construction; see workerlessInbox).
func TestDispatchInject_KeepaliveNotParkedBehindPendingHeldSibling(t *testing.T) {
	a := newTestAgent(t)
	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setAwaiting(true)
	a.DelegatedManager = newMockDelegatedManager(t, be)

	sk := "test/s"
	inb := workerlessInbox(t, a, sk)
	parkWake := func() {
		inb.held = []Envelope{{SessionKey: sk, Inject: &InjectMeta{Trigger: "scheduled_wake"}}}
	}

	t.Run("keepalive runs past a pending-held sibling", func(t *testing.T) {
		parkWake()
		var ran atomic.Int32
		if !a.dispatchInject(context.Background(), inb, Envelope{SessionKey: sk, Inject: &InjectMeta{
			Trigger: TriggerKeepalive,
			Run:     func() { ran.Add(1) },
		}}) {
			t.Fatal("dispatchInject returned false with the ctx alive")
		}
		if ran.Load() != 1 || len(inb.held) != 1 {
			t.Fatalf("keepalive parked behind a pending-held sibling instead of running: ran=%d held=%d (#2233 review F1)", ran.Load(), len(inb.held))
		}
	})

	t.Run("same-class sibling still pins through a flicker", func(t *testing.T) {
		parkWake()
		// The hold flickers open right as the sibling arrives: it must still
		// park behind the parked wake (FIFO among injects held for the same
		// reason), not jump it.
		be.setAwaiting(false)
		var ran atomic.Int32
		if !a.dispatchInject(context.Background(), inb, Envelope{SessionKey: sk, Inject: &InjectMeta{
			Trigger: "reflection",
			Run:     func() { ran.Add(1) },
		}}) {
			t.Fatal("dispatchInject returned false with the ctx alive")
		}
		if ran.Load() != 0 || len(inb.held) != 2 {
			t.Fatalf("plain inject jumped a parked same-class sibling through the open flicker: ran=%d held=%d", ran.Load(), len(inb.held))
		}
	})
}

// TestInbox_KeepaliveDoesNotQueueBehindHeldSibling pins requirement 4's
// keepalive ruling at the dispatch site the review's F1 stalled on: a
// keepalive arriving while an earlier system inject is parked by pending
// work does NOT queue behind it — pending work must never hold a keepalive —
// it runs at once while the sibling stays parked, and the sibling still runs
// (behind it, in arrival order) once the gate opens.
func TestInbox_KeepaliveDoesNotQueueBehindHeldSibling(t *testing.T) {
	a, be, release := awaitingHeldAgent(t)
	defer release()

	sk := "test/s"
	var mu sync.Mutex
	var order []string
	mk := func(trigger, tag string) *InjectMeta {
		return &InjectMeta{Trigger: trigger, Run: func() {
			mu.Lock()
			order = append(order, tag)
			mu.Unlock()
		}}
	}
	a.Enqueue(Envelope{SessionKey: sk, Inject: mk("scheduled_wake", "wake")})
	if !waitFor(2*time.Second, func() bool { return gateWatcherParked(a, sk) }) {
		t.Fatal("the wake never parked in the gate")
	}
	a.Enqueue(Envelope{SessionKey: sk, Inject: mk(TriggerKeepalive, "keepalive")})
	// The keepalive runs while the wake stays parked (review F1: on the
	// defective code it queued behind the wake's whole pending-work window).
	if !waitFor(500*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 1 && order[0] == "keepalive"
	}) {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("keepalive queued behind a wake parked by pending work: order=%v — background work must never hold it (#2233)", order)
	}

	// The gate opens; the wake runs after it.
	be.setAwaiting(false)
	if !waitFor(2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 2
	}) {
		t.Fatalf("the wake did not run after the gate opened; order=%v", order)
	}
	mu.Lock()
	defer mu.Unlock()
	if order[1] != "wake" {
		t.Fatalf("held injects ran out of order: %v", order)
	}
}

// TestInbox_KeepaliveReleasedWhileSiblingStaysHeld pins the other half of the
// review-F1 fix: a keepalive that legitimately PARKED (behind an adopted
// delivering run, with a wake also parked by the pending work behind it) is
// released the moment its OWN hold clears — the watcher wakes on any parked
// entry's hold and the release walk runs past still-held siblings — while
// the wake stays parked until the background work ends.
func TestInbox_KeepaliveReleasedWhileSiblingStaysHeld(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setAwaiting(true) // pending background work, outstanding for the whole test
	a.DelegatedManager = newMockDelegatedManager(t, be)

	sk := "test/s"
	// The adopted delivering run: in flight, delivering, worker idle — the
	// ONLY half of the hold that applies to a keepalive.
	releaseRun := a.markInFlight(sk, true)
	defer releaseRun()

	var mu sync.Mutex
	var order []string
	mk := func(trigger, tag string) *InjectMeta {
		return &InjectMeta{Trigger: trigger, Run: func() {
			mu.Lock()
			order = append(order, tag)
			mu.Unlock()
		}}
	}
	a.Enqueue(Envelope{SessionKey: sk, Inject: mk("scheduled_wake", "wake")})
	a.Enqueue(Envelope{SessionKey: sk, Inject: mk(TriggerKeepalive, "keepalive")})
	if !waitFor(2*time.Second, func() bool { return gateWatcherParked(a, sk) }) {
		t.Fatal("neither inject ever parked in the gate")
	}
	// Both sit on the held list while the adopted run and the pending work
	// hold their respective halves.
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	if len(order) != 0 {
		mu.Unlock()
		t.Fatalf("an inject ran during its hold: %v", order)
	}
	mu.Unlock()

	// The adopted run ends; pending work stays outstanding. The keepalive
	// must run NOW — a wake parked by pending work must not hold it.
	releaseRun()
	if !waitFor(3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 1 && order[0] == "keepalive"
	}) {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("keepalive stayed parked behind a pending-held sibling after its own hold cleared; order=%v (#2233 review F1)", order)
	}

	// The wake stays parked until the background work ends, then runs.
	be.setAwaiting(false)
	if !waitFor(3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 2 && order[1] == "wake"
	}) {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("the wake did not run after the background work ended; order=%v", order)
	}
}

// gateFlipBackend reports pending background work on the FIRST
// AwaitingAutonomousRun call only, so the gate opens between releaseHeld's
// checks of two parked entries.
type gateFlipBackend struct {
	mockBackendDT
	n atomic.Int32
}

func (b *gateFlipBackend) AwaitingAutonomousRun() bool { return b.n.Add(1) == 1 }

// TestReleaseHeld_FIFOWhenGateOpensMidWalk is the #2233 review F1 repro: two
// plain system injects A and B are parked; the gate is still closed when the
// release walk checks A (A stays) and open when it checks B. B must stay
// behind A — the walk applies the same same-class FIFO pin dispatch parks by
// (heldSiblingPins) — rather than run past it.
func TestReleaseHeld_FIFOWhenGateOpensMidWalk(t *testing.T) {
	a := newTestAgent(t)
	be := &gateFlipBackend{mockBackendDT: mockBackendDT{sessionFile: "/tmp/s.jsonl"}}
	a.DelegatedManager = newMockDelegatedManager(t, be)
	sk := "test/s"
	inb := workerlessInbox(t, a, sk)
	var mu sync.Mutex
	var order []string
	mk := func(tag string) Envelope {
		return Envelope{SessionKey: sk, Inject: &InjectMeta{Trigger: "scheduled_wake", Run: func() {
			mu.Lock()
			order = append(order, tag)
			mu.Unlock()
		}}}
	}
	inb.held = []Envelope{mk("A"), mk("B")}

	if !a.releaseHeld(context.Background(), inb) {
		t.Fatal("releaseHeld reported ctx end")
	}
	mu.Lock()
	ran := append([]string(nil), order...)
	mu.Unlock()
	if len(ran) != 0 || len(inb.held) != 2 {
		t.Fatalf("ran=%v held=%d, want nothing run and both still parked in order (FIFO: B must not pass a held A)", ran, len(inb.held))
	}

	// The gate is open now: the next walk releases both, A first.
	a.releaseHeld(context.Background(), inb)
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "A" || order[1] != "B" {
		t.Fatalf("order=%v, want [A B]", order)
	}
}

// TestInbox_KeepaliveRefusedBeginDoesNotBlockUser is the #2233 review N1
// repro: a keepalive the inbox lets through (no adopted run visible) meets a
// backend that refuses its SourceSystem begin — the post-run grace, or a run
// CC chained after the inbox's check. It must be skipped, not retried on the
// worker: a non-steering user message enqueued behind it drives at once.
func TestInbox_KeepaliveRefusedBeginDoesNotBlockUser(t *testing.T) {
	initTestConvo(t, "helen")
	const sk = "helen/c42"
	runEnded := make(chan struct{})
	be := &mockBackendDT{sessionFile: "/tmp/ka-hol.jsonl"}
	be.setAwaiting(true)     // pending work: a keepalive is exempt from it
	be.setTurnInFlight(true) // the backend refuses a SourceSystem begin
	be.waitForTurnFn = func(ctx context.Context) error {
		select {
		case <-runEnded:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	be.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
		if h != nil && h.OnTurnComplete != nil {
			h.OnTurnComplete(&delegator.TurnResult{Text: "ok", Model: "claude-sonnet-4-5"})
		}
		return nil, nil
	}
	a := newBatchTestAgent(t, be)
	if _, err := a.DelegatedManager.Get(context.Background(), sk); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartInbox(ctx)

	kaErr := make(chan error, 1)
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: TriggerKeepalive,
		Run: func() {
			kaErr <- a.HandleMessage(WithTrigger(ctx, TriggerKeepalive), sk, []string{"keepalive"}, nil)
		},
	}})
	select {
	case err := <-kaErr:
		if !errors.Is(err, ErrKeepaliveSkipped) {
			t.Errorf("keepalive result = %v, want ErrKeepaliveSkipped", err)
		}
	case <-time.After(3 * time.Second):
		close(runEnded)
		t.Fatal("keepalive still waiting on the worker in the SourceSystem retry loop (#2233 review N1)")
	}

	hook := make(chan struct{}, 1)
	done := make(chan struct{}, 1)
	d := &recordingDriver{hookCh: hook, doneCh: done}
	a.SetTurnObserver(d.recordBatch)
	a.Enqueue(Envelope{SessionKey: sk, Text: "user speaks", Driver: d})
	select {
	case <-hook:
	case <-time.After(3 * time.Second):
		t.Error("user message waited behind the keepalive (head-of-line block)")
	}
	close(runEnded)
	be.setTurnInFlight(false)
}
