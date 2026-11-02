package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// evictingAgent is a started agent whose idle eviction runs at millisecond
// scale: threshold idle, reaper every interval.
func evictingAgent(t *testing.T, idle, interval time.Duration) (*Agent, context.CancelFunc) {
	t.Helper()
	a := newTestAgent(t)
	a.inboxIdleAfter = idle
	a.inboxReapEvery = interval
	ctx, cancel := context.WithCancel(context.Background())
	a.StartInbox(ctx)
	t.Cleanup(cancel)
	return a, cancel
}

func inboxCount(a *Agent) int {
	a.inboxesMu.Lock()
	defer a.inboxesMu.Unlock()
	return len(a.inboxes)
}

func mustWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	if !waitFor(10*time.Second, cond) {
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestInboxEvict_IdleWorkerExitsAndKeyRecreates proves (a): an idle session's
// worker exits and a.inboxes drops the key; a later Enqueue on the same key
// creates a new inbox and the message runs.
func TestInboxEvict_IdleWorkerExitsAndKeyRecreates(t *testing.T) {
	a, _ := evictingAgent(t, 5*time.Millisecond, 2*time.Millisecond)
	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "one", Driver: d})
	// Take the inbox now, while the first message keeps it busy: with a 5 ms
	// idle limit it can be evicted before a lookup made after the turn ends,
	// which returned nil and panicked below on a fast host (CI, 6127687ca).
	first := a.lookupInbox("test/s")
	if first == nil {
		t.Fatal("Enqueue should have created the session's inbox")
	}
	mustWait(t, "first turn", func() bool { return d.NumCalls() == 1 })
	mustWait(t, "eviction", func() bool { return inboxCount(a) == 0 && a.inboxWorkers.Load() == 0 })
	first.evictMu.RLock()
	evicted := first.evicted
	first.evictMu.RUnlock()
	if !evicted {
		t.Fatal("the dropped inbox should be marked evicted")
	}

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "two", Driver: d})
	mustWait(t, "second turn", func() bool { return d.NumCalls() == 2 })
	if got := d.Calls()[1][0].Text; got != "two" {
		t.Fatalf("second turn ran %q", got)
	}
	if a.lookupInbox("test/s") == first {
		t.Fatal("a new inbox should have been created after eviction")
	}
}

// TestInboxEvict_NotWhileBusy proves (b): a session is not evicted while its
// channel or steer buffer holds an envelope, it holds a deferred inject, a turn
// runs or waits for a slot, a turn is in flight (agent or delegated backend),
// or it was used within the threshold; and a skipped eviction leaves it
// working. tryEvictInbox runs here on inboxes with no worker, the same check
// the worker runs at its idle point.
func TestInboxEvict_NotWhileBusy(t *testing.T) {
	idle := time.Millisecond
	cases := []struct {
		name  string
		setup func(a *Agent, inb *sessionInbox)
	}{
		{"channel", func(a *Agent, inb *sessionInbox) { inb.ch <- Envelope{SessionKey: inb.sk, Text: "x"} }},
		{"steer buffer", func(a *Agent, inb *sessionInbox) { inb.appendSteer(SteerEntry{Text: "x"}) }},
		{"deferred inject", func(a *Agent, inb *sessionInbox) {
			inb.deferredInjects = append(inb.deferredInjects, deferredInject{env: Envelope{SessionKey: inb.sk}})
		}},
		{"turn running", func(a *Agent, inb *sessionInbox) { inb.turnCancel = func() {} }},
		{"slot wait", func(a *Agent, inb *sessionInbox) { inb.setWaitCancel(func() {}) }},
		{"turn in flight", func(a *Agent, inb *sessionInbox) { a.SetTurnInFlightForTest(inb.sk, true) }},
		{"backend turn in flight", func(a *Agent, inb *sessionInbox) {
			r := newCapRig(t, nil, 0, nil)
			a.DelegatedManager = r.m
			mustGet(t, r.m.Get, inb.sk)
			r.mock(t, inb.sk).inFlight.Store(true)
		}},
		{"used again", func(a *Agent, inb *sessionInbox) { a.inboxIdleAfter = time.Hour; inb.touch() }},
		{"pending queue", func(a *Agent, inb *sessionInbox) { inb.pushPending(Envelope{SessionKey: inb.sk}) }},
		{"held list", func(a *Agent, inb *sessionInbox) { inb.held = append(inb.held, Envelope{SessionKey: inb.sk}) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newTestAgent(t)
			a.inboxIdleAfter = idle
			inb := a.getOrCreateInbox("test/" + strings.ReplaceAll(c.name, " ", "-"))
			inb.lastUsed.Store(time.Now().Add(-time.Minute).UnixNano())
			c.setup(a, inb)
			if a.tryEvictInbox(inb) {
				t.Fatalf("evicted while %s", c.name)
			}
			if a.lookupInbox(inb.sk) != inb || inb.evicted {
				t.Fatal("a skipped eviction must leave the inbox in place")
			}
		})
	}
}

// TestInboxEvict_EnqueueRacingEviction proves (c): an Enqueue paused after it
// found the existing inbox and before it wrote, while that inbox's eviction
// runs to completion, delivers its message exactly once, through a new
// worker.
func TestInboxEvict_EnqueueRacingEviction(t *testing.T) {
	a, _ := evictingAgent(t, time.Nanosecond, time.Hour) // idle at once; eviction only on request
	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "warm", Driver: d})
	mustWait(t, "warm-up turn", func() bool { return d.NumCalls() == 1 })
	old := a.lookupInbox("test/s")

	found := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	a.enqueueFoundHook = func(string) {
		once.Do(func() {
			close(found)
			<-resume
		})
	}
	done := make(chan bool, 1)
	go func() { done <- a.Enqueue(Envelope{SessionKey: "test/s", Text: "raced", Driver: d}) }()
	<-found

	// Eviction of the inbox Enqueue found runs to completion.
	a.requestIdleEvictions()
	mustWait(t, "eviction while Enqueue is paused", func() bool { return a.lookupInbox("test/s") == nil && a.inboxWorkers.Load() == 0 })

	close(resume)
	if !<-done {
		t.Fatal("Enqueue reported the message dropped")
	}
	mustWait(t, "raced message to run", func() bool { return d.NumCalls() == 2 })
	if got := d.Calls()[1]; len(got) != 1 || got[0].Text != "raced" {
		t.Fatalf("second turn = %+v, want the raced message once", got)
	}
	if len(old.ch) != 0 {
		t.Fatal("the message was left on the evicted inbox")
	}
	if inb := a.lookupInbox("test/s"); inb == nil || inb == old {
		t.Fatal("the message should have run on a new inbox")
	}
	// Exactly once: a later message runs after it, and "raced" never again.
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "fence", Driver: d})
	mustWait(t, "fence", func() bool { return d.NumCalls() == 3 })
	n := 0
	for _, b := range d.Calls() {
		for _, e := range b {
			if e.Text == "raced" {
				n++
			}
		}
	}
	if n != 1 {
		t.Fatalf("raced message ran %d times, want 1", n)
	}
}

// TestInboxEvict_LoadExactlyOnce proves (d): under concurrent senders on a few
// keys with eviction at a very short interval and threshold, every message is
// observed exactly once and no worker goroutine is left behind.
func TestInboxEvict_LoadExactlyOnce(t *testing.T) {
	a, cancel := evictingAgent(t, 100*time.Microsecond, 50*time.Microsecond)
	var mu sync.Mutex
	seen := map[string]int{}
	var total atomic.Int64
	a.SetTurnObserver(func(_ string, batch []Envelope) {
		mu.Lock()
		for _, e := range batch {
			seen[e.Text]++
		}
		mu.Unlock()
		total.Add(int64(len(batch)))
	})
	d := &recordingDriver{}
	const senders, perSender = 8, 64 // 512 messages
	var wg sync.WaitGroup
	for s := 0; s < senders; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			for i := 0; i < perSender; i++ {
				key := fmt.Sprintf("test/k%d", (s+i)%3)
				text := fmt.Sprintf("m-%d-%d", s, i)
				for !a.Enqueue(Envelope{SessionKey: key, Text: text, Driver: d}) {
					time.Sleep(time.Millisecond) // full channel: retry, the message is not lost
				}
				// Pace the senders so keys go idle between messages and
				// evictions interleave with enqueues (asserted below).
				time.Sleep(time.Duration(i%4) * 200 * time.Microsecond)
			}
		}(s)
	}
	wg.Wait()
	if n := a.inboxEvictions.Load(); n < 10 {
		t.Fatalf("only %d evictions during the load: the race was not exercised", n)
	}
	mustWait(t, "every message", func() bool { return total.Load() >= senders*perSender })
	mu.Lock()
	for s := 0; s < senders; s++ {
		for i := 0; i < perSender; i++ {
			if n := seen[fmt.Sprintf("m-%d-%d", s, i)]; n != 1 {
				t.Errorf("m-%d-%d observed %d times", s, i, n)
			}
		}
	}
	mu.Unlock()
	mustWait(t, "all idle inboxes evicted", func() bool { return inboxCount(a) == 0 && a.inboxWorkers.Load() == 0 })
	cancel()
}

// TestInboxEvict_TurnLockRemovalKeepsTurnsSerial proves (e): removing a turn
// mutex from the map while a caller holds it, or has fetched it and is about
// to lock it, never lets two turns on the key run at once — for eviction's
// removal and ClearSessionState's alike.
func TestInboxEvict_TurnLockRemovalKeepsTurnsSerial(t *testing.T) {
	a := newTestAgent(t)
	const sk = "test/s"

	// A held mutex is not removed, by either path.
	held := a.lockTurn(sk)
	a.dropTurnLock(sk)
	a.ClearSessionState(sk)
	if a.turnLock(sk) != held {
		t.Fatal("a held turn mutex was removed")
	}

	// C fetches the mutex and pauses before locking it.
	fetched := make(chan struct{}, 4)
	resume := make(chan struct{})
	var first atomic.Bool
	a.turnLockFetchedHook = func(string) {
		fetched <- struct{}{}
		if first.CompareAndSwap(false, true) { // only C's first fetch pauses
			<-resume
		}
	}
	cDone := make(chan struct{})
	go func() {
		mu := a.lockTurn(sk)
		close(cDone)
		mu.Unlock()
	}()
	<-fetched // C holds a pointer to the current mutex
	held.Unlock()
	a.dropTurnLock(sk)  // now unheld: removed
	b := a.lockTurn(sk) // B takes the fresh mutex
	<-fetched           // B's own fetch
	close(resume)       // C locks the removed one

	select {
	case <-cDone:
		t.Fatal("C acquired a turn lock while B holds the key's lock")
	case <-fetched: // C found its mutex stale and fetched again
	}
	select {
	case <-cDone:
		t.Fatal("C acquired a turn lock while B holds the key's lock")
	default:
	}
	b.Unlock()
	select {
	case <-cDone:
	case <-time.After(10 * time.Second):
		t.Fatal("C never got the lock after B released it")
	}
}

// TestInboxEvict_InFlightChangedWakesWaiter proves (f): dropping a session's
// in-flight change channel wakes a goroutine blocked on InFlightWaitCh, and is
// skipped while a turn is in flight.
func TestInboxEvict_InFlightChangedWakesWaiter(t *testing.T) {
	a := newTestAgent(t)
	const sk = "test/s"
	a.SetTurnInFlightForTest(sk, true)
	ch := a.InFlightWaitCh(sk)
	a.dropInFlightChanged(sk)
	select {
	case <-ch:
		t.Fatal("dropped while a turn is in flight")
	default:
	}
	a.SetTurnInFlightForTest(sk, false) // closes ch (state change)
	ch = a.InFlightWaitCh(sk)
	woke := make(chan struct{})
	go func() { <-ch; close(woke) }()
	a.dropInFlightChanged(sk)
	select {
	case <-woke:
	case <-time.After(10 * time.Second):
		t.Fatal("waiter stranded on a removed channel")
	}
	a.inFlightMu.Lock()
	_, still := a.inFlightChanged[sk]
	a.inFlightMu.Unlock()
	if still {
		t.Fatal("channel entry not removed")
	}
}

// TestInboxEvict_BgOriginsPruned proves (g): an entry holding only expired
// stamps is deleted, while a live stamp (and its entry) survives.
func TestInboxEvict_BgOriginsPruned(t *testing.T) {
	a := newTestAgent(t)
	old := time.Now().Add(-bgOriginMaxAge - time.Minute)
	a.bgOrigins = map[string]map[string]*bgOrigin{
		"test/expired": {"g1": {at: old}},
		"test/mixed":   {"g1": {at: old}, "g2": {at: time.Now()}},
	}
	a.pruneBgOrigins("test/expired")
	a.pruneBgOrigins("test/mixed")
	if _, ok := a.bgOrigins["test/expired"]; ok {
		t.Error("expired-only entry not deleted")
	}
	m := a.bgOrigins["test/mixed"]
	if _, ok := m["g2"]; !ok || len(m) != 1 {
		t.Errorf("mixed entry = %v, want only the live stamp", m)
	}
}

// TestInboxEvict_KeepsLiveStateMaps pins Dick's ruling: eviction leaves the
// non-refilling per-session state (meta, routers, lastTurnEndAt) alone.
func TestInboxEvict_KeepsLiveStateMaps(t *testing.T) {
	a := newTestAgent(t)
	a.inboxIdleAfter = time.Millisecond
	const sk = "test/s"
	a.metaMu.Lock()
	a.meta = map[string]*sessionMeta{sk: {}}
	a.metaMu.Unlock()
	router := a.sessionRouter(sk)
	a.inFlightMu.Lock()
	a.lastTurnEndAt = map[string]time.Time{sk: time.Now()}
	a.inFlightMu.Unlock()
	inb := a.getOrCreateInbox(sk)
	inb.lastUsed.Store(time.Now().Add(-time.Minute).UnixNano())
	if !a.tryEvictInbox(inb) {
		t.Fatal("idle inbox not evicted")
	}
	if a.sessionRouter(sk) != router {
		t.Error("router rebuilt")
	}
	if a.LastTurnEnd(sk).IsZero() {
		t.Error("lastTurnEndAt dropped")
	}
	a.metaMu.Lock()
	_, ok := a.meta[sk]
	a.metaMu.Unlock()
	if !ok {
		t.Error("meta dropped")
	}
}
