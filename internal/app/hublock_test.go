package app

import (
	"sync"
	"testing"
	"time"

	"foci/internal/fap"
)

// #3170: no hub lock, and no conversation's waitMu, is held across a call into
// an agent, a backend or the reminder store. On 18 Nov one goroutine stuck in
// such a call (a roster build, inside the reminder store's held predicate)
// kept the hub's read lock; a writer queued behind it, and from then on every
// reader and every new socket blocked: the app was dead.

// gate blocks every caller of wait until open is called. entered is closed
// at the first caller's arrival.
type gate struct {
	once    sync.Once
	opened  sync.Once
	entered chan struct{}
	release chan struct{}
}

// newGate returns a gate that the test's cleanup opens, so a failed test
// does not leave its callers blocked.
func newGate(t *testing.T) *gate {
	g := &gate{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *gate) wait() {
	g.once.Do(func() { close(g.entered) })
	<-g.release
}

func (g *gate) open() { g.opened.Do(func() { close(g.release) }) }

// blockingWaitInputs blocks UsageHeldInput on g; the other inputs are a
// fakeWaitInputs.
type blockingWaitInputs struct {
	*fakeWaitInputs
	g *gate
}

func (b blockingWaitInputs) UsageHeldInput(sk string) bool {
	b.g.wait()
	return b.fakeWaitInputs.UsageHeldInput(sk)
}

// blockingAgent blocks CacheExpiryMs on g.
type blockingAgent struct {
	fakeAgent
	g *gate
}

func (b *blockingAgent) CacheExpiryMs(sk string, at time.Time) int64 {
	b.g.wait()
	return b.fakeAgent.CacheExpiryMs(sk, at)
}

// within fails the test unless fn returns within d.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v: a hub lock is held across the blocked agent call", what, d)
	}
}

// A roster build stuck in any agent-side call must not stop the hub from
// taking its write lock (a new socket) or serving a reader after it (a
// client snapshot). On main the build held h.mu.RLock across these calls, so
// the writer queued and the reader behind it blocked.
func TestRosterBuild_StuckAgentCallDoesNotHoldHubLock(t *testing.T) {
	cases := []struct {
		name  string
		block func(f *waitFixture, g *gate)
	}{
		{"reminder store", func(f *waitFixture, g *gate) {
			f.setWakes(func() []fap.PendingReminder { g.wait(); return []fap.PendingReminder{} })
		}},
		{"agent wait input", func(f *waitFixture, g *gate) {
			in := blockingWaitInputs{fakeWaitInputs: f.in, g: g}
			f.h.waitInputsFor = func(string) waitInputs { return in }
		}},
		{"agent cache expiry", func(f *waitFixture, g *gate) {
			f.h.agents["ag"].agentRef = &blockingAgent{g: g}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWaitFixture(t)
			f.bind("conv-a", false)
			g := newGate(t)
			tc.block(f, g)

			rosterDone := make(chan struct{})
			go func() {
				defer close(rosterDone)
				_ = f.h.agentRoster()
			}()
			select {
			case <-g.entered:
			case <-time.After(5 * time.Second):
				g.open()
				t.Fatal("the roster build never reached the blocking call")
			}

			// A new socket takes the write lock; a snapshot reads after it.
			c := fakeClientForHub(f.h)
			within(t, 2*time.Second, "addClient", func() { f.h.addClient(c) })
			within(t, 2*time.Second, "snapshotClients", func() {
				if n := len(f.h.snapshotClients()); n != 1 {
					t.Errorf("snapshotClients = %d clients, want 1", n)
				}
			})

			g.open()
			select {
			case <-rosterDone:
			case <-time.After(5 * time.Second):
				t.Fatal("the roster build did not finish after the call returned")
			}
		})
	}
}

// One refresh stuck in an agent call must not hold the conversation's waitMu:
// a later refresh of the same conversation completes and sends. The stuck
// one, when it returns, carries an older evaluation and must not overwrite the
// newer value (F4 of #3169).
func TestRefreshWaiting_StuckAgentCallDoesNotHoldWaitMu(t *testing.T) {
	f := newWaitFixture(t)
	b, c := f.bind("conv-a", false)
	drain(t, c)

	// The first evaluation blocks in UsageHeldInput and then reads "held"
	// (true); every later one reads the input unblocked: not held (false).
	g := newGate(t)
	var mu sync.Mutex
	first := true
	f.h.waitInputsFor = func(string) waitInputs {
		mu.Lock()
		defer mu.Unlock()
		if first {
			first = false
			stale := newFakeWaitInputs()
			stale.held[b.key()] = true
			return blockingWaitInputs{fakeWaitInputs: stale, g: g}
		}
		return f.in
	}
	// Start from a device that holds TRUE, so the newer FALSE is a change.
	b.mu.Lock()
	b.waitingOther = true
	b.mu.Unlock()

	stuckDone := make(chan struct{})
	go func() {
		defer close(stuckDone)
		f.h.refreshWaiting(b)
	}()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		g.open()
		t.Fatal("the first refresh never reached the blocking call")
	}

	within(t, 2*time.Second, "a second refreshWaiting of the same conversation", func() { f.h.refreshWaiting(b) })
	if got := waitingValues(t, c); len(got) != 1 || got[0] {
		t.Fatalf("frames after the second refresh = %v, want [false]", got)
	}

	g.open()
	select {
	case <-stuckDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the stuck refresh did not finish after the call returned")
	}
	if got := waitingValues(t, c); len(got) != 0 {
		t.Errorf("the stale refresh sent %v, want nothing (a newer evaluation was already sent)", got)
	}
	b.mu.Lock()
	v := b.waitingOther
	b.mu.Unlock()
	if v {
		t.Error("b.waitingOther = true after the stale refresh, want false (the newer value)")
	}
}
