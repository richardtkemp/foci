package agent

import (
	"context"
	"errors"
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

// recvSoon receives from c within d, reporting false on timeout. Every
// handoff in these tests is bounded so a regression fails in seconds
// instead of hanging the package until go test's own timeout kills it; the
// boolean shape lets non-test goroutines (turn observers run on worker
// goroutines, where t.Fatalf is illegal) propagate the failure through a
// channel instead.
func recvSoon(c chan struct{}, d time.Duration) bool {
	select {
	case <-c:
		return true
	case <-time.After(d):
		return false
	}
}

// mustRecv receives from c or fails the test — for handoffs observed on the
// test goroutine.
func mustRecv(t *testing.T, c chan struct{}, what string) {
	t.Helper()
	if !recvSoon(c, 2*time.Second) {
		t.Fatalf("timed out waiting for %s", what)
	}
}

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
	mustRecv(t, dA.ready, "A to hold the only slot inside WrapTurn")

	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	mustRecv(t, notices.signal, "B's wait to begin")

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
	mustRecv(t, dA.ready, "A's turn to start")

	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	mustRecv(t, notices.signal, "B to queue first")
	a.Enqueue(Envelope{SessionKey: "sess/C", Text: "c", Driver: dC})
	mustRecv(t, notices.signal, "C to queue behind B")

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
	mustRecv(t, dA.ready, "agent 1's turn to hold the global slot")

	a2.Enqueue(Envelope{SessionKey: "two/s", Text: "b", Driver: dB})
	mustRecv(t, notices.signal, "agent 2's turn to begin waiting")
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
	mustRecv(t, dA.ready, "A's turn to start")
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	mustRecv(t, dB.ready, "B to start concurrently with A (limit 2)")
	if calls := notices.recorded(); len(calls) != 0 {
		t.Fatalf("notice fired for a turn that got a slot at once: %+v", calls)
	}

	a.Enqueue(Envelope{SessionKey: "sess/C", Text: "c", Driver: dC})
	mustRecv(t, notices.signal, "the third session's wait to begin")
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

	sessions := []string{"sess/A", "sess/B", "sess/C", "sess/D", "sess/E"}
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
	mustRecv(t, dA.ready, "A's turn to start")

	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "first", Driver: dB})
	mustRecv(t, notices.signal, "B's wait to begin")
	// A second B message arrives while the batch waits — it must not get a
	// second notice, and must fold into the waiting batch.
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "joiner", Driver: dB})

	close(releaseA)
	mustRecv(t, dB.ready, "B's turn to start")
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
	mustRecv(t, dA.ready, "A's turn to start")
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	mustRecv(t, notices.signal, "B's user batch to be waiting")

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
	mustRecv(t, dA.ready, "turn 1 to start under the slot")
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	mustRecv(t, notices.signal, "B's wait to begin")

	// A late A message becomes a follow-up turn once turn 1 completes.
	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "late", Driver: dA})
	close(gate1)
	mustRecv(t, dA.ready, "the follow-up turn to start")

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
	mustRecv(t, dA.ready, "A's turn to start")

	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "first", Driver: dB})
	mustRecv(t, notices.signal, "B to queue first (before C, so B would win a freed slot)")
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "joiner", Driver: dB}) // merged into B's waiting batch
	a.Enqueue(Envelope{SessionKey: "sess/C", Text: "c", Driver: dC})
	mustRecv(t, notices.signal, "C to queue behind B")

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
	mustRecv(t, dA.ready, "A's running turn to hold the only slot")

	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	mustRecv(t, notices.signal, "a queued notice")
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
	mustRecv(t, notices.signal, "B to take agent 2's slot and wait on the global one")
	a2.Enqueue(Envelope{SessionKey: "two/C", Text: "c", Driver: dC})
	mustRecv(t, notices.signal, "C to wait on agent 2's slot held by B")

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
	mustRecv(t, dA.ready, "A's turn to start")
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	mustRecv(t, notices.signal, "B's wait to begin")

	cancel() // agent shutdown — the wait ends, the batch never runs
	select {
	case <-dB.ready:
		t.Fatal("waiting batch ran after the inbox ctx was cancelled")
	case <-time.After(150 * time.Millisecond):
	}
	// Let A's blocked turn finish so no goroutine is left behind.
	close(releaseA)
}

// TestTurnLimit_EnqueueInjectWaitFromSlotHolderNoDeadlock proves the
// deadlock rule end to end (H3's exact shape): a turn that HOLDS the only
// slot calls EnqueueInjectWait on ANOTHER session whose user batch is queued
// behind that same slot. The injection must run while the holder still waits
// (B's worker serves it slot-free), EnqueueInjectWait must return nil, and
// B's queued batch must run afterwards once the holder releases.
func TestTurnLimit_EnqueueInjectWaitFromSlotHolderNoDeadlock(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.TurnLimit = NewTurnLimiter(1)

	releaseA := make(chan struct{})
	dA := &driverGated{ready: make(chan struct{}, 1), release: releaseA}
	dB := &driverGated{ready: make(chan struct{}, 1)}
	notices := newNoticeRecorder()
	a.TurnQueuedNotifyFunc = notices.hook()

	errc := make(chan error, 1)
	injRan := make(chan struct{})
	// The turn observer fires inside driveOnce — under A's held slot, before
	// WrapTurn — so this is "inside A's slot-holding turn". It queues B,
	// waits for B to be parked behind A's slot, then blocks on B's injection.
	a.SetTurnObserver(func(sk string, batch []Envelope) {
		if sk != "sess/A" {
			return
		}
		a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
		if !recvSoon(notices.signal, 2*time.Second) {
			errc <- errors.New("B never queued behind A's slot")
			return
		}
		errc <- a.EnqueueInjectWait(context.Background(), "sess/B", "test", func() { close(injRan) })
	})

	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "a", Driver: dA})
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("EnqueueInjectWait from inside a slot-holding turn: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EnqueueInjectWait deadlocked behind the slot its caller holds")
	}
	mustRecv(t, injRan, "the injection to run")
	// The injection ran while A still held the slot (releaseA is not closed
	// until after this point) — no slot, no deadlock.
	mustRecv(t, dA.ready, "A's turn to enter WrapTurn after the observer")
	close(releaseA)
	mustRecv(t, dB.ready, "B's queued batch to run after the release")
}

// TestTurnLimit_SteerAlwaysJoinerMergesIntoWaitingBatch proves a message
// sent with SteerAlways while its session's batch waits for a slot still
// goes to the channel (turnActive is false while waiting — there is nothing
// to steer into) and joins the waiting batch: ONE WrapTurn call carries
// both envelopes, in arrival order, with no second notice.
func TestTurnLimit_SteerAlwaysJoinerMergesIntoWaitingBatch(t *testing.T) {
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
	mustRecv(t, dA.ready, "A to hold the only slot")
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "first", Driver: dB})
	mustRecv(t, notices.signal, "B's wait to begin")
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "joiner", Driver: dB, Steer: SteerAlways})

	close(releaseA)
	mustRecv(t, dB.ready, "B's turn to start after the release")
	time.Sleep(100 * time.Millisecond) // let driveAndDrainOrphans settle: no follow-up may fire

	mu.Lock()
	defer mu.Unlock()
	if len(bBatches) != 1 {
		t.Fatalf("B drove %d batches, want 1 (SteerAlways joiner merged into the waiting batch): %+v", len(bBatches), bBatches)
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

// TestTurnLimit_CancelSessionIdleReturnsFalse proves /stop semantics with
// nothing waiting and nothing running: an idle session returns false (and
// touches no slot state), and an unknown session key returns false.
func TestTurnLimit_CancelSessionIdleReturnsFalse(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.TurnLimit = NewTurnLimiter(1)

	// Run one turn to completion (release == nil returns immediately).
	d := &driverGated{ready: make(chan struct{}, 1)}
	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "a", Driver: d})
	mustRecv(t, d.ready, "A's turn to start")
	// The slot returning to the pool proves the turn fully unwound — its
	// turnCancel was cleared before the release — so the session is idle.
	if !waitFor(2*time.Second, func() bool { return a.TurnLimit.TryAcquire() }) {
		t.Fatal("the finished turn never released the slot")
	}
	a.TurnLimit.Release()

	if a.CancelSession("sess/A") {
		t.Error("CancelSession on an idle session returned true")
	}
	if a.CancelSession("sess/unknown") {
		t.Error("CancelSession on an unknown session returned true")
	}
}

// TestTurnLimit_StopWhileQueuedKeepsGatedInjection proves the in-wait
// injection path keeps its gates on the inbox ctx: a /stop that drops the
// queued USER batch must not abandon an injection already pulled off the
// channel and held in a gate — the injection runs to completion once the
// gate opens, and only the batch is dropped.
//
// Determinism: inFlightChanged gains a "sess/B" entry exactly when the
// worker enters the inject gate's wait loop (nothing else calls
// InFlightWaitCh for B here), which proves the injection is parked IN the
// gate before the /stop. The marker message then separates the two worlds:
// on the fixed path the worker stays parked in the gate (marker immobile,
// no second notice); on the broken path the /stop aborts the batch, the
// worker comes back around, and the marker queues (second notice) — all
// before the gate is ever opened.
func TestTurnLimit_StopWhileQueuedKeepsGatedInjection(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.TurnLimit = NewTurnLimiter(1)

	releaseA := make(chan struct{})
	dA := &driverGated{ready: make(chan struct{}, 1), release: releaseA}
	dB := &driverGated{ready: make(chan struct{}, 1)}
	notices := newNoticeRecorder()
	a.TurnQueuedNotifyFunc = notices.hook()

	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "a", Driver: dA})
	mustRecv(t, dA.ready, "A to hold the only slot")
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	mustRecv(t, notices.signal, "B's user batch to be waiting")

	// Close the inject gate on B: an adopted delivering run is in flight.
	releaseInFlight := a.markInFlight("sess/B", true)

	// An injection for B is pulled off the channel by the waiting worker
	// and held in the (closed) inject gate. inFlightChanged["sess/B"]
	// appearing proves the worker is parked INSIDE the gate's wait loop.
	injRan := make(chan struct{})
	refused := make(chan struct{})
	a.Enqueue(Envelope{SessionKey: "sess/B", Inject: &InjectMeta{
		Trigger: "test",
		Run:     func() { close(injRan) },
		Refused: func() { close(refused) },
	}})
	if !waitFor(2*time.Second, func() bool {
		a.inFlightMu.Lock()
		defer a.inFlightMu.Unlock()
		_, gated := a.inFlightChanged["sess/B"]
		return gated
	}) {
		t.Fatal("the injection never entered the inject gate")
	}

	// /stop drops the queued user batch — and nothing else.
	if !a.CancelSession("sess/B") {
		t.Fatal("CancelSession returned false for a slot-waiting batch")
	}

	// A further B message separates "worker back around (batch already
	// dropped — the injection was lost with it)" from "worker still parked
	// in the injection's gate". The gate is still closed, so on the fixed
	// path the marker cannot move and no second notice can fire.
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "marker", Driver: dB})
	if recvSoon(notices.signal, 2*time.Second) {
		releaseInFlight()
		select {
		case <-injRan:
			t.Fatal("injection ran after its batch was already dropped — impossible ordering")
		case <-refused:
			t.Fatal("injection refused after /stop of the queued user batch — it was gated, not declined")
		case <-time.After(2 * time.Second):
			t.Fatal("injection lost after /stop of the queued user batch: never ran, never refused")
		}
		t.Fatal("unreachable")
	}

	// Open the gate: the injection runs despite the /stop.
	releaseInFlight()
	select {
	case <-injRan:
	case <-refused:
		t.Fatal("the gated injection was refused instead of run")
	case <-time.After(5 * time.Second):
		t.Fatal("the gated injection never ran after the gate opened")
	}

	// Only the user batch was dropped: B's turns never drive (A still holds
	// the slot; the marker is queued behind it, not dropped).
	select {
	case <-dB.ready:
		t.Fatal("B's dropped batch ran")
	case <-time.After(150 * time.Millisecond):
	}
	if got := dB.count.Load(); got != 0 {
		t.Errorf("dropped batch reached the driver: B WrapTurn calls = %d, want 0", got)
	}
	close(releaseA)
}

// TestTurnLimit_CompactionDuringWaitHoldsAfterGrant proves the gates are
// re-checked AFTER the slots are granted: a compaction that starts on the
// session while its batch queues delays the drive — the granted slot is
// given back, the #856 hold is waited out, and only then does the batch run
// (with no second queued notice for the same batch).
func TestTurnLimit_CompactionDuringWaitHoldsAfterGrant(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.TurnLimit = NewTurnLimiter(1)

	releaseA := make(chan struct{})
	dA := &driverGated{ready: make(chan struct{}, 1), release: releaseA}
	dB := &driverGated{ready: make(chan struct{}, 1)}
	notices := newNoticeRecorder()
	a.TurnQueuedNotifyFunc = notices.hook()

	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "a", Driver: dA})
	mustRecv(t, dA.ready, "A to hold the only slot")
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	mustRecv(t, notices.signal, "B's user batch to be waiting")

	// A /compact starts on B while its batch queues.
	a.markCompacting("sess/B")

	// A's turn finishes and frees the slot — B is granted it, but the
	// compaction hold must stop the drive (#856 would be bypassed otherwise).
	close(releaseA)
	select {
	case <-dB.ready:
		t.Fatal("B's turn drove while its session was compacting (#856 hold bypassed by the slot wait)")
	case <-time.After(300 * time.Millisecond):
	}

	a.clearCompacting("sess/B")
	mustRecv(t, dB.ready, "B's turn to start after the compaction cleared")
	if calls := notices.recorded(); len(calls) != 1 {
		t.Errorf("notice calls = %d, want 1 (no re-notice when a gate re-check requeues the same batch): %+v", len(calls), calls)
	}
}

// TestTurnLimit_NonDeliveringInFlightDuringWaitHoldsAfterGrant proves the
// same post-grant re-check for the #767 hold: a non-delivering turn that
// goes in flight on the session while its batch queues (e.g. a delegated
// branch pass run as an in-wait injection) holds the drive until it clears.
func TestTurnLimit_NonDeliveringInFlightDuringWaitHoldsAfterGrant(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.TurnLimit = NewTurnLimiter(1)

	releaseA := make(chan struct{})
	dA := &driverGated{ready: make(chan struct{}, 1), release: releaseA}
	dB := &driverGated{ready: make(chan struct{}, 1)}
	notices := newNoticeRecorder()
	a.TurnQueuedNotifyFunc = notices.hook()

	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "a", Driver: dA})
	mustRecv(t, dA.ready, "A to hold the only slot")
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	mustRecv(t, notices.signal, "B's user batch to be waiting")

	// A non-delivering turn goes in flight on B while its batch queues.
	releaseInFlight := a.markInFlight("sess/B", false)

	close(releaseA)
	select {
	case <-dB.ready:
		t.Fatal("B's turn drove while a non-delivering turn was in flight (#767 hold bypassed by the slot wait)")
	case <-time.After(300 * time.Millisecond):
	}

	releaseInFlight()
	mustRecv(t, dB.ready, "B's turn to start after the non-delivering turn cleared")
	if calls := notices.recorded(); len(calls) != 1 {
		t.Errorf("notice calls = %d, want 1: %+v", len(calls), calls)
	}
}

// TestTurnLimit_ReloginNeverHoldsAPIAgent pins the delegated-only scope of
// the #1932 re-login hold through the #2281 gate-then-slot order: the gate
// is process-wide, but an API agent (no DelegatedManager) shares no CC
// credential, so its user batch must pass every re-login hold and reach the
// slot wait — proven by the queued notice, which only fires once the gates
// are open (a held gate parks the worker before the slot step).
func TestTurnLimit_ReloginNeverHoldsAPIAgent(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.TurnLimit = NewTurnLimiter(1)
	// No DelegatedManager: an API agent.

	releaseA := make(chan struct{})
	dA := &driverGated{ready: make(chan struct{}, 1), release: releaseA}
	dB := &driverGated{ready: make(chan struct{}, 1)}
	notices := newNoticeRecorder()
	a.TurnQueuedNotifyFunc = notices.hook()

	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "a", Driver: dA})
	mustRecv(t, dA.ready, "A to hold the only slot")

	claimRelogin(t)
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	mustRecv(t, notices.signal, "B to pass the re-login hold and queue for the slot")
	select {
	case <-dB.ready:
		t.Fatal("B ran while A held the only slot")
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseA)
	mustRecv(t, dB.ready, "B to run once the slot frees")
}

// TestTurnLimit_StopAfterGrantDropsBatch proves a /stop that lands after
// the slots were granted but before the wait is settled still drops the
// batch: CancelSession returned true (and logged the drop), so the batch
// must never run, and the slot it briefly held comes back for the next
// session. The grant observer fires the /stop from inside the exact race
// window, so this is deterministic.
func TestTurnLimit_StopAfterGrantDropsBatch(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.TurnLimit = NewTurnLimiter(1)

	dB := &driverGated{ready: make(chan struct{}, 1)}
	dC := &driverGated{ready: make(chan struct{}, 1)}
	notices := newNoticeRecorder()
	a.TurnQueuedNotifyFunc = notices.hook()

	// The test holds the only slot, so B must queue behind it.
	if !a.TurnLimit.TryAcquire() {
		t.Fatal("test could not take the only slot")
	}
	stopped := make(chan bool, 1)
	a.SetTurnSlotGrantObserver(func() {
		// Runs on the worker between the grant and the settle — inside the
		// exact grant-vs-/stop race window.
		stopped <- a.CancelSession("sess/B")
	})

	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "b", Driver: dB})
	mustRecv(t, notices.signal, "B to queue behind the test's slot")

	a.TurnLimit.Release() // grant B; the observer fires /stop inside the window
	if got := <-stopped; !got {
		t.Fatal("CancelSession inside the grant window returned false")
	}

	select {
	case <-dB.ready:
		t.Fatal("the batch ran although /stop landed after the grant")
	case <-time.After(150 * time.Millisecond):
	}
	if got := dB.count.Load(); got != 0 {
		t.Errorf("stopped batch reached the driver: B WrapTurn calls = %d, want 0", got)
	}
	// The slot the batch briefly held came back: C runs at once.
	a.Enqueue(Envelope{SessionKey: "sess/C", Text: "c", Driver: dC})
	mustRecv(t, dC.ready, "C to run on the released slot")
}
