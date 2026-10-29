package agent

// Inbox run order (#2362, Dick 2026-10-28: "Obviously send in the order they
// arrive"). The worker runs queued items in enqueue order: an injection at the
// head of the queue runs before every user message enqueued after it, and a
// user message at the head takes every queued user message into one batch.
// A redelivery is not held by the #2233 pending-work hold.

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/platform"
	"foci/internal/turnevent"
)

// orderLog records what ran, in order: "turn:<texts>" for each batch the turn
// observer sees, "inject:<name>" for each injection's Run.
type orderLog struct {
	mu sync.Mutex
	ev []string
}

func (o *orderLog) add(s string) {
	o.mu.Lock()
	o.ev = append(o.ev, s)
	o.mu.Unlock()
}

func (o *orderLog) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.ev...)
}

func (o *orderLog) observer(_ string, batch []Envelope) {
	texts := make([]string, len(batch))
	for i, e := range batch {
		texts[i] = e.Text
	}
	o.add("turn:" + strings.Join(texts, ","))
}

// heldDriver holds its first len(holds) turns open, turn i until holds[i] is
// closed, so the test can enqueue items while that turn runs; later turns run
// at once. Follow-up turns run on the seed envelope's driver, so they count
// here too. started is signalled at the start of every turn.
type heldDriver struct {
	n       atomic.Int32
	holds   []chan struct{}
	started chan struct{}
}

func newHeldDriver(turnsHeld int) *heldDriver {
	d := &heldDriver{started: make(chan struct{}, 16)}
	for i := 0; i < turnsHeld; i++ {
		d.holds = append(d.holds, make(chan struct{}))
	}
	return d
}

func (d *heldDriver) WrapTurn(ctx context.Context, fn func() error) error {
	i := int(d.n.Add(1)) - 1
	d.started <- struct{}{}
	if i < len(d.holds) {
		select {
		case <-d.holds[i]:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	OnPrimaryWrittenFromContext(ctx)()
	return fn()
}

func (d *heldDriver) NewTurnSink(_ Envelope) (turnevent.Sink, func()) { return nil, nil }
func (d *heldDriver) Connection() platform.Connection                 { return nil }

// orderInject is an injection that logs its run.
func orderInject(sk, trigger, name string, log *orderLog, runs *atomic.Int32) Envelope {
	return Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: trigger,
		Run: func() {
			if runs != nil {
				runs.Add(1)
			}
			log.add("inject:" + name)
		},
	}}
}

// orderUser is a user message that never steers (steer mode is off in these
// tests; SteerNever makes that independent of the config).
func orderUser(sk, text string, d Driver) Envelope {
	return Envelope{SessionKey: sk, Text: text, Driver: d, Steer: platform.SteerNever}
}

func waitOrder(t *testing.T, log *orderLog, n int) []string {
	t.Helper()
	if !waitFor(3*time.Second, func() bool { return len(log.snapshot()) >= n }) {
		t.Fatalf("only %d of %d events ran: %v", len(log.snapshot()), n, log.snapshot())
	}
	// Give a wrong extra event a moment to show up.
	time.Sleep(50 * time.Millisecond)
	return log.snapshot()
}

func assertOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("run order:\n got  %v\n want %v", got, want)
	}
}

// (a) H1: the ticket case. During U1's turn a redelivery R and then U2 are
// enqueued. After the turn R runs before U2.
func TestInboxOrder_RedeliveryBeforeLaterUserMessage(t *testing.T) {
	t.Parallel()
	a, cancel := startedAgent(t)
	defer cancel()
	log := &orderLog{}
	a.SetTurnObserver(log.observer)
	d := newHeldDriver(1)
	sk := "test/order-a"

	a.Enqueue(orderUser(sk, "U1", d))
	mustRecv(t, d.started, "U1's turn to start")
	a.Enqueue(orderInject(sk, TriggerRedelivery, "R", log, nil))
	a.Enqueue(orderUser(sk, "U2", d))
	close(d.holds[0])

	assertOrder(t, waitOrder(t, log, 3), "turn:U1", "inject:R", "turn:U2")
}

// (b) H2: the same with background work outstanding for the whole test. The
// redelivery is not parked and runs before U2. A scheduled wake in the same
// position still parks, U2 runs, and the wake runs only after the hold clears
// (unchanged #2233 behaviour).
func TestInboxOrder_RedeliveryNotHeldByPendingWork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		trigger string
		parks   bool
	}{
		{TriggerRedelivery, false},
		{"scheduled_wake", true},
	} {
		t.Run(tc.trigger, func(t *testing.T) {
			t.Parallel()
			a, cancel := startedAgent(t)
			defer cancel()
			be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
			be.setAwaiting(true)
			a.DelegatedManager = newMockDelegatedManager(t, be)
			log := &orderLog{}
			a.SetTurnObserver(log.observer)
			d := newHeldDriver(1)
			sk := "test/s" // the session newMockDelegatedManager registers

			a.Enqueue(orderUser(sk, "U1", d))
			mustRecv(t, d.started, "U1's turn to start")
			a.Enqueue(orderInject(sk, tc.trigger, "I", log, nil))
			a.Enqueue(orderUser(sk, "U2", d))
			close(d.holds[0])

			if !tc.parks {
				assertOrder(t, waitOrder(t, log, 3), "turn:U1", "inject:I", "turn:U2")
				if gateWatcherParked(a, sk) {
					t.Fatal("the redelivery parked on the pending-work hold")
				}
				return
			}
			assertOrder(t, waitOrder(t, log, 2), "turn:U1", "turn:U2")
			if !gateWatcherParked(a, sk) {
				t.Fatal("the wake did not park while background work was outstanding")
			}
			be.setAwaiting(false)
			assertOrder(t, waitOrder(t, log, 3), "turn:U1", "turn:U2", "inject:I")
		})
	}
}

// (c) H3: a generic injection enqueued during a turn runs before a user
// message enqueued after it.
func TestInboxOrder_InjectBeforeLaterUserMessage(t *testing.T) {
	t.Parallel()
	a, cancel := startedAgent(t)
	defer cancel()
	log := &orderLog{}
	a.SetTurnObserver(log.observer)
	d := newHeldDriver(1)
	sk := "test/order-c"

	a.Enqueue(orderUser(sk, "U1", d))
	mustRecv(t, d.started, "U1's turn to start")
	a.Enqueue(orderInject(sk, "session_notify", "I", log, nil))
	a.Enqueue(orderUser(sk, "U3", d))
	close(d.holds[0])

	assertOrder(t, waitOrder(t, log, 3), "turn:U1", "inject:I", "turn:U3")
}

// (d) H4: the left-behind case. [A, I, B] are queued at batch start, so A and
// B run as one batch and I stays queued; C arrives during that batch. After
// it, I runs, then C.
func TestInboxOrder_LeftBehindInjectBeforeLaterArrival(t *testing.T) {
	t.Parallel()
	a, cancel := startedAgent(t)
	defer cancel()
	log := &orderLog{}
	a.SetTurnObserver(log.observer)
	// The {A, B} batch runs as the follow-up of U0's turn, on U0's driver, so
	// that one driver holds both turns.
	d := newHeldDriver(2)
	sk := "test/order-d"

	// U0 blocks the worker so that A, I and B are all queued when A's batch
	// starts.
	a.Enqueue(orderUser(sk, "U0", d))
	mustRecv(t, d.started, "U0's turn to start")
	a.Enqueue(orderUser(sk, "A", d))
	a.Enqueue(orderInject(sk, "session_notify", "I", log, nil))
	a.Enqueue(orderUser(sk, "B", d))
	close(d.holds[0])

	mustRecv(t, d.started, "the {A, B} batch to start")
	if got := log.snapshot(); strings.Join(got, " | ") != "turn:U0 | turn:A,B" {
		t.Fatalf("first batches: %v (want U0, then exactly {A, B} with I not run)", got)
	}
	a.Enqueue(orderUser(sk, "C", d))
	close(d.holds[1])

	assertOrder(t, waitOrder(t, log, 4), "turn:U0", "turn:A,B", "inject:I", "turn:C")
}

// (e) H5, characterisation: merging is kept. With [A, I, B] queued, A and B
// run as ONE batch and I runs after it, exactly once. Five sequences.
func TestInboxOrder_QueuedUserMessagesStillMerge(t *testing.T) {
	t.Parallel()
	a, cancel := startedAgent(t)
	defer cancel()
	log := &orderLog{}
	a.SetTurnObserver(log.observer)
	sk := "test/order-e"

	var runs [5]atomic.Int32
	var want []string
	for i := 0; i < 5; i++ {
		blocker := newHeldDriver(1)
		n := string(rune('0' + i))
		a.Enqueue(orderUser(sk, "U"+n, blocker))
		mustRecv(t, blocker.started, "the blocking turn to start")
		a.Enqueue(orderUser(sk, "A"+n, blocker))
		a.Enqueue(orderInject(sk, "session_notify", "I"+n, log, &runs[i]))
		a.Enqueue(orderUser(sk, "B"+n, blocker))
		close(blocker.holds[0])
		want = append(want, "turn:U"+n, "turn:A"+n+",B"+n, "inject:I"+n)
		waitOrder(t, log, len(want))
	}
	assertOrder(t, log.snapshot(), want...)
	for i := range runs {
		if got := runs[i].Load(); got != 1 {
			t.Fatalf("injection I%d ran %d times, want 1", i, got)
		}
	}
}
