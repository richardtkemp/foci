package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/platform"
	"foci/internal/usagehold"
)

// arrivalLog records, in run order, the user turns and injections a worker
// runs: a user turn as its batch's texts joined with "+", an injection as
// its name.
type arrivalLog struct {
	mu  sync.Mutex
	got []string
}

func (l *arrivalLog) add(s string) {
	l.mu.Lock()
	l.got = append(l.got, s)
	l.mu.Unlock()
}

func (l *arrivalLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.got...)
}

// TestInbox_HeldInputKeepsArrivalOrderAtLift (#3121, Dick's Q58 ruling
// 2026-11-14: "keep arrival order"). A usage hold parks user messages
// (usageHeld) and system injections (held) on two separate lists. At lift
// the worker must run them in the order they arrived. Before the fix the
// loop top ran every released injection first, so an injection that arrived
// AFTER a held user message ran ahead of it.
//
// A user message heading the release takes every held user message into its
// one turn (#2491: held messages fold into one turn at lift; #2362: a user
// batch's head takes the queued user messages after it), so the "U1, I, U2"
// case runs U1+U2 and then I.
func TestInbox_HeldInputKeepsArrivalOrderAtLift(t *testing.T) {
	cases := []struct {
		name  string
		order []string // "U…" = user message, "I…" = cron injection
		want  []string
	}{
		{"user then injection", []string{"U1", "I1"}, []string{"U1", "I1"}},
		{"injection then user", []string{"I1", "U1"}, []string{"I1", "U1"}},
		{"injection, user, injection", []string{"I1", "U1", "I2"}, []string{"I1", "U1", "I2"}},
		{"user, injection, user", []string{"U1", "I1", "U2"}, []string{"U1+U2", "I1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, cancel := startedAgent(t)
			defer cancel()
			a.DelegatedManager = &DelegatedManager{BackendType: "claude-code"}
			putOnAccount(a, "", usagehold.New(), nil)
			a.StartUsageHold()
			defer a.StopUsageHold()

			log := &arrivalLog{}
			a.SetTurnObserver(func(_ string, batch []Envelope) {
				texts := make([]string, 0, len(batch))
				for _, env := range batch {
					texts = append(texts, env.Text)
				}
				log.add(strings.Join(texts, "+"))
			})
			d := &recordingDriver{}

			testAcct(a).ObserveUsage(weekly(0.98))
			for _, item := range tc.order {
				item := item
				if strings.HasPrefix(item, "U") {
					a.Enqueue(Envelope{SessionKey: "test/s", Text: item, Driver: d})
					continue
				}
				a.Enqueue(Envelope{SessionKey: "test/s", Inject: &InjectMeta{Trigger: "cron", Run: func() { log.add(item) }}})
			}
			// A keepalive runs during a hold. It is queued last, so once it
			// has run the worker has dispatched (parked) every item above.
			parked := make(chan struct{})
			a.Enqueue(Envelope{SessionKey: "test/s", Inject: &InjectMeta{Trigger: "keepalive", Run: func() { close(parked) }}})
			select {
			case <-parked:
			case <-time.After(2 * time.Second):
				t.Fatal("the worker never reached the keepalive queued behind the held items")
			}
			if got := log.snapshot(); len(got) != 0 {
				t.Fatalf("ran during the hold: %v", got)
			}

			testAcct(a).ObserveUsage(weekly(0.50))
			if !waitFor(3*time.Second, func() bool { return len(log.snapshot()) >= len(tc.want) }) {
				t.Fatalf("at lift ran %v, want %v", log.snapshot(), tc.want)
			}
			// Anything out of order would already be visible; a short settle
			// catches an extra run.
			time.Sleep(50 * time.Millisecond)
			if got := log.snapshot(); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("run order at lift = %v, want %v (arrival order, Q58)", got, tc.want)
			}
		})
	}
}

// TestInbox_PendingWorkHeldInjectKeepsArrivalOrder (#3121) pins the
// pending-work hold's side of Q58. An injection parks only when it reaches
// the head of the worker's queue, so every message still queued arrived
// after it: releasing it at the loop top, before the queue, keeps arrival
// order. A user message that arrives while it is parked and the worker is
// idle runs past it (#2233), as U0 does here.
func TestInbox_PendingWorkHeldInjectKeepsArrivalOrder(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setAwaiting(true)
	a.DelegatedManager = newMockDelegatedManager(t, be)
	sk := "test/s"

	log := &arrivalLog{}
	a.SetTurnObserver(func(_ string, batch []Envelope) {
		texts := make([]string, 0, len(batch))
		for _, env := range batch {
			texts = append(texts, env.Text)
		}
		log.add(strings.Join(texts, "+"))
	})
	inject := func(name string) {
		a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{Trigger: "cron", Run: func() { log.add(name) }}})
	}

	inject("I1")
	if !waitFor(2*time.Second, func() bool { return gateWatcherParked(a, sk) }) {
		t.Fatal("I1 never parked in the gate")
	}
	hook := make(chan struct{}, 1)
	finish := make(chan struct{})
	d := &gatedDriver{recordingDriver: recordingDriver{hookCh: hook}, finish: finish}
	a.Enqueue(Envelope{SessionKey: sk, Text: "U0", Driver: d})
	select {
	case <-hook:
	case <-time.After(2 * time.Second):
		t.Fatal("U0 never started")
	}
	// While U0's turn runs: U1 queues for the next turn, then I2 arrives,
	// then I1's hold opens. Only then does U0's turn end.
	a.Enqueue(Envelope{SessionKey: sk, Text: "U1", Driver: &recordingDriver{}, Steer: platform.SteerNever})
	inject("I2")
	be.setAwaiting(false)
	close(finish)

	want := []string{"U0", "I1", "U1", "I2"}
	if !waitFor(3*time.Second, func() bool { return len(log.snapshot()) >= len(want) }) {
		t.Fatalf("ran %v, want %v", log.snapshot(), want)
	}
	time.Sleep(50 * time.Millisecond)
	if got := log.snapshot(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("run order = %v, want %v (arrival order, Q58)", got, want)
	}
}

// gatedDriver is a recordingDriver whose turn does not end until finish is
// closed.
type gatedDriver struct {
	recordingDriver
	finish chan struct{}
}

func (d *gatedDriver) WrapTurn(ctx context.Context, fn func() error) error {
	return d.recordingDriver.WrapTurn(ctx, func() error {
		<-d.finish
		return fn()
	})
}

// TestInbox_RemindPassesUsageHeldMessage (#3121): the arrival-order limit at
// the loop top does not hold back what the usage hold lets run. A foci_remind
// fire that background work parked after a usage-held user message runs once
// the background work ends, while the message still waits for the lift
// (#2491 requirement 6).
func TestInbox_RemindPassesUsageHeldMessage(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	a.DelegatedManager = newMockDelegatedManager(t, be)
	a.DelegatedManager.BackendType = "claude-code"
	putOnAccount(a, "", usagehold.New(), nil)
	a.StartUsageHold()
	defer a.StopUsageHold()
	sk := "test/s"

	log := &arrivalLog{}
	a.SetTurnObserver(func(_ string, batch []Envelope) { log.add(batch[0].Text) })
	testAcct(a).ObserveUsage(weekly(0.98))
	a.Enqueue(Envelope{SessionKey: sk, Text: "U1", Driver: &recordingDriver{}})
	be.setAwaiting(true)
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{Trigger: "scheduled_wake", Run: func() { log.add("W1") }}})
	if !waitFor(2*time.Second, func() bool { return gateWatcherParked(a, sk) }) {
		t.Fatal("the wake never parked behind background work")
	}
	be.setAwaiting(false)
	if !waitFor(2*time.Second, func() bool { return len(log.snapshot()) == 1 }) {
		t.Fatalf("the wake did not run during the usage hold; ran %v", log.snapshot())
	}
	if got := log.snapshot(); got[0] != "W1" {
		t.Fatalf("ran %v during the hold, want [W1]", got)
	}
	testAcct(a).ObserveUsage(weekly(0.50))
	if !waitFor(2*time.Second, func() bool { return len(log.snapshot()) == 2 }) {
		t.Fatalf("U1 did not run at lift; ran %v", log.snapshot())
	}
}
