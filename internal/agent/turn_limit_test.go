package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"foci/internal/platform"
	"foci/internal/turnevent"
)

// --- TurnLimiter unit tests ---

// TestTurnLimiter_NilMeansUnlimited proves a nil limiter (a limit of 0 or
// less) is unlimited and every method is nil-receiver safe — the property
// that makes "no limit configured" take no slot, send no notice, and log
// nothing anywhere in the inbox.
func TestTurnLimiter_NilMeansUnlimited(t *testing.T) {
	if l := NewTurnLimiter(0); l != nil {
		t.Errorf("NewTurnLimiter(0) = %v, want nil (unlimited)", l)
	}
	if l := NewTurnLimiter(-3); l != nil {
		t.Errorf("NewTurnLimiter(-3) = %v, want nil (unlimited)", l)
	}
	var l *TurnLimiter
	if l.Size() != 0 {
		t.Errorf("nil Size() = %d, want 0", l.Size())
	}
	if !l.TryAcquire() {
		t.Error("nil TryAcquire() = false, want true")
	}
	if !l.Acquire(context.Background()) {
		t.Error("nil Acquire() = false, want true")
	}
	l.Release() // must be a no-op, not a panic
}

// TestTurnLimiter_TryAcquireRespectsLimit proves TryAcquire hands out at
// most N slots and Release returns one to the pool.
func TestTurnLimiter_TryAcquireRespectsLimit(t *testing.T) {
	l := NewTurnLimiter(2)
	if !l.TryAcquire() || !l.TryAcquire() {
		t.Fatal("first two TryAcquire calls should succeed")
	}
	if l.TryAcquire() {
		t.Fatal("third TryAcquire should fail at limit 2")
	}
	l.Release()
	if !l.TryAcquire() {
		t.Error("TryAcquire should succeed after a Release")
	}
	if l.TryAcquire() {
		t.Error("TryAcquire should fail again at limit 2")
	}
}

// TestTurnLimiter_AcquireFIFO proves blocked waiters are granted in the
// order their Acquire calls reached the semaphore: Release hands the slot
// to the head waiter, so the grant order matches the enqueue order.
func TestTurnLimiter_AcquireFIFO(t *testing.T) {
	l := NewTurnLimiter(1)
	if !l.TryAcquire() {
		t.Fatal("holder TryAcquire failed")
	}
	granted := make(chan int, 3)
	for i := 0; i < 3; i++ {
		i := i
		go func() {
			if l.Acquire(context.Background()) {
				granted <- i
			}
		}()
		// Separate enqueue attempts enough that semaphore order matches
		// spawn order (FIFO holds at enqueue granularity).
		time.Sleep(50 * time.Millisecond)
	}
	for want := 0; want < 3; want++ {
		l.Release()
		select {
		case got := <-granted:
			if got != want {
				t.Fatalf("grant order broken: got waiter %d, want %d", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d not granted after Release", want)
		}
	}
}

// TestTurnLimiter_AcquireCtxCancel proves Acquire returns false when its ctx
// ends while queued, and that the abandoned wait leaks no slot: after the
// holder releases, a fresh acquire still succeeds.
func TestTurnLimiter_AcquireCtxCancel(t *testing.T) {
	l := NewTurnLimiter(1)
	if !l.TryAcquire() {
		t.Fatal("holder TryAcquire failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- l.Acquire(ctx) }()
	time.Sleep(50 * time.Millisecond) // let the waiter enqueue
	cancel()
	select {
	case got := <-done:
		if got {
			t.Fatal("Acquire reported true for a cancelled wait")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not return after ctx cancel")
	}
	l.Release()
	if !l.TryAcquire() {
		t.Fatal("slot leaked: TryAcquire failed after cancelled waiter + Release")
	}
}

// --- Inbox slot tests (#2281) ---

// noticeCall is one recorded queued-notice hook invocation.
type noticeCall struct {
	sk   string
	text string
}

// noticeRecorder captures queued-notice hook calls. hook() is handed to
// Agent.TurnQueuedNotifyFunc; signal receives one value per call so tests
// can wait deterministically for a notice to fire.
type noticeRecorder struct {
	mu     sync.Mutex
	calls  []noticeCall
	signal chan struct{}
}

func newNoticeRecorder() *noticeRecorder {
	return &noticeRecorder{signal: make(chan struct{}, 16)}
}

func (n *noticeRecorder) hook() func(sessionKey, text string) {
	return func(sk, text string) {
		n.mu.Lock()
		n.calls = append(n.calls, noticeCall{sk: sk, text: text})
		n.mu.Unlock()
		n.signal <- struct{}{}
	}
}

func (n *noticeRecorder) recorded() []noticeCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]noticeCall, len(n.calls))
	copy(out, n.calls)
	return out
}

const wantQueuedNotice = "⏳ Busy right now. Your message is queued and will run when a slot frees up."

// TestTurnLimit_AgentLimit1_SecondSessionWaitsAndNotified proves the core
// cap: with max_concurrent_turns = 1, session B's turn does not start while
// session A's turn still runs, B's sender gets exactly ONE queued notice
// with the exact text, and B runs once A's turn returns.
func TestTurnLimit_AgentLimit1_SecondSessionWaitsAndNotified(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.TurnLimit = NewTurnLimiter(1)

	releaseA := make(chan struct{})
	dA := &driverGated{ready: make(chan struct{}, 1), release: releaseA}
	dB := &driverGated{ready: make(chan struct{}, 1)}
	notices := newNoticeRecorder()
	a.TurnQueuedNotifyFunc = notices.hook()

	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "a", Driver: dA})
	<-dA.ready // A holds the only slot inside WrapTurn

	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	<-notices.signal // B's wait began

	// B must not start while A holds the slot.
	select {
	case <-dB.ready:
		t.Fatal("B started its turn while A held the only slot")
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseA)
	select {
	case <-dB.ready:
		// B ran once A's turn returned.
	case <-time.After(2 * time.Second):
		t.Fatal("B never ran after A released the slot")
	}

	// Exactly one notice, exact text, to B's session. A second notice would
	// have had to fire during B's wait — before B's ready above — so this is
	// deterministic without a sleep.
	calls := notices.recorded()
	if len(calls) != 1 {
		t.Fatalf("notice calls = %d, want exactly 1: %+v", len(calls), calls)
	}
	if calls[0].sk != "sess/B" {
		t.Errorf("notice sk = %q, want sess/B", calls[0].sk)
	}
	if calls[0].text != wantQueuedNotice {
		t.Errorf("notice text = %q, want %q", calls[0].text, wantQueuedNotice)
	}
}

// TestTurnLimit_AgentLimit1_FIFOThirdAfterSecond proves waiters are served
// first come, first served: with B and C queued behind A (in that order), A's
// release lets B run, and only B's return lets C run.
func TestTurnLimit_AgentLimit1_FIFOThirdAfterSecond(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.TurnLimit = NewTurnLimiter(1)

	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	dA := &driverGated{ready: make(chan struct{}, 1), release: releaseA}
	dB := &driverGated{ready: make(chan struct{}, 1), release: releaseB}
	dC := &driverGated{ready: make(chan struct{}, 1)}
	notices := newNoticeRecorder()
	a.TurnQueuedNotifyFunc = notices.hook()

	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "a", Driver: dA})
	<-dA.ready

	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	<-notices.signal // B queued first
	a.Enqueue(Envelope{SessionKey: "sess/C", Text: "c", Driver: dC})
	<-notices.signal // then C

	close(releaseA)
	select {
	case <-dB.ready:
		// B — the first waiter — runs.
	case <-time.After(2 * time.Second):
		t.Fatal("B (first waiter) did not run after A released")
	}
	select {
	case <-dC.ready:
		t.Fatal("C jumped the queue: ran while B still held the slot")
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseB)
	select {
	case <-dC.ready:
		// C runs after B.
	case <-time.After(2 * time.Second):
		t.Fatal("C (second waiter) did not run after B released")
	}
}

// TestTurnLimit_GlobalLimit1_SharedAcrossAgents proves the gateway-wide cap
// is one shared semaphore: a turn on agent 2 waits while agent 1 runs, even
// though neither agent has a per-agent limit.
func TestTurnLimit_GlobalLimit1_SharedAcrossAgents(t *testing.T) {
	a1, cancel1 := startedAgent(t)
	defer cancel1()
	a2, cancel2 := startedAgent(t)
	defer cancel2()

	global := NewTurnLimiter(1)
	a1.GlobalTurnLimit = global
	a2.GlobalTurnLimit = global

	releaseA := make(chan struct{})
	dA := &driverGated{ready: make(chan struct{}, 1), release: releaseA}
	dB := &driverGated{ready: make(chan struct{}, 1)}
	notices := newNoticeRecorder()
	a2.TurnQueuedNotifyFunc = notices.hook()

	a1.Enqueue(Envelope{SessionKey: "one/s", Text: "a", Driver: dA})
	<-dA.ready // agent 1's turn holds the global slot

	a2.Enqueue(Envelope{SessionKey: "two/s", Text: "b", Driver: dB})
	<-notices.signal // agent 2's turn began waiting
	select {
	case <-dB.ready:
		t.Fatal("agent 2's turn ran while agent 1 held the only global slot")
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseA)
	select {
	case <-dB.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("agent 2's turn never ran after agent 1 released the global slot")
	}
}

// TestTurnLimit_AgentLimit2_TwoConcurrentThirdWaits proves limit 2 lets two
// sessions run at once with no notice, and the third waits (with one).
func TestTurnLimit_AgentLimit2_TwoConcurrentThirdWaits(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.TurnLimit = NewTurnLimiter(2)

	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	dA := &driverGated{ready: make(chan struct{}, 1), release: releaseA}
	dB := &driverGated{ready: make(chan struct{}, 1), release: releaseB}
	dC := &driverGated{ready: make(chan struct{}, 1)}
	notices := newNoticeRecorder()
	a.TurnQueuedNotifyFunc = notices.hook()

	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "a", Driver: dA})
	<-dA.ready
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	<-dB.ready // two turns run concurrently — no notice for either
	if calls := notices.recorded(); len(calls) != 0 {
		t.Fatalf("notice fired for a turn that got a slot at once: %+v", calls)
	}

	a.Enqueue(Envelope{SessionKey: "sess/C", Text: "c", Driver: dC})
	<-notices.signal // the third must wait — and be told
	select {
	case <-dC.ready:
		t.Fatal("C started while both slots were held")
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseA)
	select {
	case <-dC.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("C never ran after a slot freed")
	}
	close(releaseB)
}

// TestTurnLimit_Unlimited_NoNoticeAndParallel pins today's behaviour
// (characterisation, red-gate-exempt): with no limiters, many sessions run
// their turns at once and the notice hook never fires.
func TestTurnLimit_Unlimited_NoNoticeAndParallel(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	sessions := []string{"sess/A", "sess/B", "sess/C"}
	drivers := make([]*driverGated, len(sessions))
	for i := range drivers {
		drivers[i] = &driverGated{ready: make(chan struct{}, 1), release: make(chan struct{})}
	}
	notices := newNoticeRecorder()
	a.TurnQueuedNotifyFunc = notices.hook()

	for i, d := range drivers {
		a.Enqueue(Envelope{SessionKey: sessions[i], Text: "x", Driver: d})
	}
	for i, d := range drivers {
		select {
		case <-d.ready:
		case <-time.After(2 * time.Second):
			t.Fatalf("session %s did not run — unlimited agents must stay parallel", sessions[i])
		}
	}
	if calls := notices.recorded(); len(calls) != 0 {
		t.Fatalf("notice fired with no limit configured: %+v", calls)
	}
	for _, d := range drivers {
		close(d.release)
	}
}

// TestTurnLimit_JoinerMergesIntoWaitingBatch proves a message arriving while
// its session's batch waits for a slot joins that batch: ONE WrapTurn call
// carries both envelopes, in arrival order, with no second notice.
func TestTurnLimit_JoinerMergesIntoWaitingBatch(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.TurnLimit = NewTurnLimiter(1)

	releaseA := make(chan struct{})
	dA := &driverGated{ready: make(chan struct{}, 1), release: releaseA}
	dB := &driverGated{ready: make(chan struct{}, 1)}
	notices := newNoticeRecorder()
	a.TurnQueuedNotifyFunc = notices.hook()

	var mu sync.Mutex
	var bBatches [][]Envelope
	a.SetTurnObserver(func(sk string, batch []Envelope) {
		if sk != "sess/B" {
			return
		}
		mu.Lock()
		bBatches = append(bBatches, append([]Envelope(nil), batch...))
		mu.Unlock()
	})

	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "a", Driver: dA})
	<-dA.ready

	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "first", Driver: dB})
	<-notices.signal // B is waiting
	// A second B message arrives while the batch waits — it must not get a
	// second notice, and must fold into the waiting batch.
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "joiner", Driver: dB})

	close(releaseA)
	<-dB.ready
	time.Sleep(100 * time.Millisecond) // let driveAndDrainOrphans settle: no follow-up may fire

	mu.Lock()
	defer mu.Unlock()
	if len(bBatches) != 1 {
		t.Fatalf("B drove %d batches, want 1 (joiner merged into the waiting batch): %+v", len(bBatches), bBatches)
	}
	if len(bBatches[0]) != 2 {
		t.Fatalf("B batch has %d envelopes, want 2: %+v", len(bBatches[0]), bBatches[0])
	}
	if got := []string{bBatches[0][0].Text, bBatches[0][1].Text}; got[0] != "first" || got[1] != "joiner" {
		t.Errorf("B batch texts = %v, want [first joiner]", got)
	}
	if calls := notices.recorded(); len(calls) != 1 {
		t.Errorf("notice calls = %d, want 1 (no second notice for the joiner): %+v", len(calls), calls)
	}
}

// TestTurnLimit_InjectionRunsWhileWaitingNoSlot proves the deadlock rule:
// an injection enqueued on a session whose user batch waits for a slot runs
// while the batch keeps waiting — before the slot-holding turn finishes —
// and takes no slot itself (it ran while the only slot was held).
func TestTurnLimit_InjectionRunsWhileWaitingNoSlot(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.TurnLimit = NewTurnLimiter(1)

	releaseA := make(chan struct{})
	dA := &driverGated{ready: make(chan struct{}, 1), release: releaseA}
	dB := &driverGated{ready: make(chan struct{}, 1)}
	notices := newNoticeRecorder()
	a.TurnQueuedNotifyFunc = notices.hook()

	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "a", Driver: dA})
	<-dA.ready
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	<-notices.signal // B's user batch is waiting

	injRan := make(chan struct{})
	a.Enqueue(Envelope{SessionKey: "sess/B", Inject: &InjectMeta{
		Trigger: "test",
		Run:     func() { close(injRan) },
	}})

	select {
	case <-injRan:
		// The injection ran while A still held the only slot — no slot,
		// no deadlock.
	case <-time.After(2 * time.Second):
		t.Fatal("injection did not run while the user batch waited — slot-exemption broken (deadlock at limit 1)")
	}

	// The waiting batch is intact: B's turn still runs after A releases.
	close(releaseA)
	select {
	case <-dB.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("B's user batch never ran after the slot freed")
	}
}

// sequentialGatedDriver blocks each of its turns on its own gate channel
// (closed by the test), signalling `ready` at every turn entry. Used to
// prove a batch's follow-up turns run under the same slot.
type sequentialGatedDriver struct {
	ready chan struct{}
	gates []chan struct{}
	next  int
}

func (d *sequentialGatedDriver) WrapTurn(ctx context.Context, fn func() error) error {
	select {
	case d.ready <- struct{}{}:
	default:
	}
	gate := d.gates[d.next]
	d.next++
	<-gate
	OnPrimaryWrittenFromContext(ctx)()
	return fn()
}

func (d *sequentialGatedDriver) NewTurnSink(_ Envelope) (turnevent.Sink, func()) { return nil, nil }
func (d *sequentialGatedDriver) Connection() platform.Connection                 { return nil }

// TestTurnLimit_FollowUpTurnSameSlotNotReleasedEarly proves one slot covers
// a batch AND the follow-up turns driveAndDrainOrphans builds: at limit 1 a
// second session starts only after the follow-up turn completes, and the
// follow-up takes no second slot.
func TestTurnLimit_FollowUpTurnSameSlotNotReleasedEarly(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.TurnLimit = NewTurnLimiter(1)

	gate1 := make(chan struct{})
	gate2 := make(chan struct{})
	dA := &sequentialGatedDriver{ready: make(chan struct{}, 4), gates: []chan struct{}{gate1, gate2}}
	dB := &driverGated{ready: make(chan struct{}, 1)}
	notices := newNoticeRecorder()
	a.TurnQueuedNotifyFunc = notices.hook()

	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "first", Driver: dA})
	<-dA.ready // turn 1 running under the slot
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	<-notices.signal // B waits

	// A late A message becomes a follow-up turn once turn 1 completes.
	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "late", Driver: dA})
	close(gate1)
	<-dA.ready // turn 2 (the follow-up) running

	select {
	case <-dB.ready:
		t.Fatal("slot released early: B ran before the follow-up turn finished")
	case <-time.After(150 * time.Millisecond):
	}
	close(gate2)
	select {
	case <-dB.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("B never ran after the batch (and its follow-up) released the slot")
	}
}

// holdForeverDriver enters WrapTurn, signals ready, and blocks on release
// regardless of ctx — unlike driverGated, a cancelled turn ctx does NOT end
// it. Used to prove the inbox ctx ends a slot WAIT without the slot itself
// being freed underneath the waiter.
type holdForeverDriver struct {
	ready   chan struct{}
	release chan struct{}
}

func (d *holdForeverDriver) WrapTurn(_ context.Context, _ func() error) error {
	select {
	case d.ready <- struct{}{}:
	default:
	}
	<-d.release
	return nil
}

func (d *holdForeverDriver) NewTurnSink(_ Envelope) (turnevent.Sink, func()) { return nil, nil }
func (d *holdForeverDriver) Connection() platform.Connection                 { return nil }

// TestTurnLimit_StopDropsWaitingBatch proves /stop while queued:
// CancelSession returns true, the waiting batch — including a message that
// joined it while waiting — never reaches the driver, no slot is taken, and
// A's release lets the NEXT waiter (C) run.
func TestTurnLimit_StopDropsWaitingBatch(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.TurnLimit = NewTurnLimiter(1)

	releaseA := make(chan struct{})
	dA := &driverGated{ready: make(chan struct{}, 1), release: releaseA}
	dB := &driverGated{ready: make(chan struct{}, 1)}
	dC := &driverGated{ready: make(chan struct{}, 1)}
	notices := newNoticeRecorder()
	a.TurnQueuedNotifyFunc = notices.hook()

	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "a", Driver: dA})
	<-dA.ready

	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "first", Driver: dB})
	<-notices.signal                                                      // B queued (before C, so B would win a freed slot)
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "joiner", Driver: dB}) // merged into B's waiting batch
	a.Enqueue(Envelope{SessionKey: "sess/C", Text: "c", Driver: dC})
	<-notices.signal // C queued behind B

	if !a.CancelSession("sess/B") {
		t.Fatal("CancelSession returned false for a slot-waiting batch")
	}

	close(releaseA)
	select {
	case <-dC.ready:
		// C — not B — got the freed slot: B's wait was dropped.
	case <-time.After(2 * time.Second):
		t.Fatal("next waiter C did not run after the slot freed")
	}
	if got := dB.count.Load(); got != 0 {
		t.Errorf("dropped batch reached the driver: B WrapTurn calls = %d, want 0", got)
	}
}

// TestTurnLimit_StopRunningTurnReleasesSlot proves the deferred release on
// the /stop exit path: cancelling A's RUNNING turn ends it, the slot comes
// back, and the next waiter runs.
func TestTurnLimit_StopRunningTurnReleasesSlot(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.TurnLimit = NewTurnLimiter(1)

	dA := &driverGated{ready: make(chan struct{}, 1), release: make(chan struct{}), done: make(chan struct{}, 1)}
	dB := &driverGated{ready: make(chan struct{}, 1)}
	notices := newNoticeRecorder()
	a.TurnQueuedNotifyFunc = notices.hook()

	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "a", Driver: dA})
	<-dA.ready // A's turn is running (gated) under the only slot

	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	<-notices.signal
	select {
	case <-dB.ready:
		t.Fatal("B started while A's turn held the only slot")
	case <-time.After(150 * time.Millisecond):
	}

	if !a.CancelSession("sess/A") {
		t.Fatal("CancelSession returned false for a running turn")
	}
	// A's turn ends via its cancelled ctx (the gated driver's ctx arm)...
	select {
	case <-dA.done:
	case <-time.After(2 * time.Second):
		t.Fatal("A's turn did not end after /stop")
	}
	// ...which releases the slot, so B — the waiter — runs next.
	select {
	case <-dB.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("B never ran after the stopped turn released the slot")
	}
}

// TestTurnLimit_StopWhileWaitingGlobalReleasesAgentSlot proves a batch
// dropped while waiting on the GLOBAL limiter gives back the per-agent slot
// it already held: the next waiter on the same agent proceeds (and then
// waits on the global slot like B did).
func TestTurnLimit_StopWhileWaitingGlobalReleasesAgentSlot(t *testing.T) {
	a1, cancel1 := startedAgent(t)
	defer cancel1()
	a2, cancel2 := startedAgent(t)
	defer cancel2()

	global := NewTurnLimiter(1)
	a1.GlobalTurnLimit = global
	a2.TurnLimit = NewTurnLimiter(1)
	a2.GlobalTurnLimit = global

	releaseA := make(chan struct{})
	dA := &driverGated{ready: make(chan struct{}, 1), release: releaseA}
	dB := &driverGated{ready: make(chan struct{}, 1)}
	dC := &driverGated{ready: make(chan struct{}, 1)}
	notices := newNoticeRecorder()
	a2.TurnQueuedNotifyFunc = notices.hook()

	a1.Enqueue(Envelope{SessionKey: "one/s", Text: "a", Driver: dA})
	<-dA.ready // agent 1 holds the global slot

	a2.Enqueue(Envelope{SessionKey: "two/B", Text: "b", Driver: dB})
	<-notices.signal // B: took agent 2's slot, waiting on the global one
	a2.Enqueue(Envelope{SessionKey: "two/C", Text: "c", Driver: dC})
	<-notices.signal // C: waiting on agent 2's slot (held by B)

	if !a2.CancelSession("two/B") {
		t.Fatal("CancelSession returned false for a batch waiting on the global limiter")
	}

	// B's drop must have released agent 2's slot to C. C now waits on the
	// global slot (still held by agent 1), so it must NOT run yet.
	select {
	case <-dC.ready:
		t.Fatal("C ran while agent 1 still held the only global slot")
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseA)
	select {
	case <-dC.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("C never ran — B's dropped batch leaked agent 2's slot")
	}
	if got := dB.count.Load(); got != 0 {
		t.Errorf("dropped batch reached the driver: B WrapTurn calls = %d, want 0", got)
	}
}

// TestTurnLimit_InboxCtxDoneDuringWait proves the worker stops waiting when
// the inbox ctx ends (agent shutdown), like the other gates: the waiting
// batch never runs. The slot-holder's turn ignores ctx here, so the freed
// slot cannot reach the waiter — only the wait's own ctx can end it.
func TestTurnLimit_InboxCtxDoneDuringWait(t *testing.T) {
	a, cancel := startedAgent(t)
	a.TurnLimit = NewTurnLimiter(1)

	releaseA := make(chan struct{})
	dA := &holdForeverDriver{ready: make(chan struct{}, 1), release: releaseA}
	dB := &driverGated{ready: make(chan struct{}, 1)}
	notices := newNoticeRecorder()
	a.TurnQueuedNotifyFunc = notices.hook()

	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "a", Driver: dA})
	<-dA.ready
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	<-notices.signal // B is waiting

	cancel() // agent shutdown — the wait ends, the batch never runs
	select {
	case <-dB.ready:
		t.Fatal("waiting batch ran after the inbox ctx was cancelled")
	case <-time.After(150 * time.Millisecond):
	}
	// Let A's blocked turn finish so no goroutine is left behind.
	close(releaseA)
}
