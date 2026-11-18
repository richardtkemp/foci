package agent

import (
	"sync"
	"testing"
	"time"
)

// #3163: a burst of more than inboxChanSize SkipHolds injections (children's
// reports to a chat) while the session's worker is held in a long turn is not
// dropped: every one is accepted, and each runs, in arrival order, once the
// worker is free.
func TestSkipHoldsBurstPastQueueCapDropsNothing(t *testing.T) {
	t.Parallel()
	a, cancel := startedAgent(t)
	defer cancel()
	sk := "test/c3163"

	// A long system turn holds the worker (the protected turn on the work
	// Mac).
	started, release := make(chan struct{}), make(chan struct{})
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "keepalive",
		Run:     func() { close(started); <-release },
	}})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the holding turn never started")
	}

	const n = 2*inboxChanSize + 8
	var mu sync.Mutex
	var ran []int
	for i := range n {
		if !a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
			Trigger:   "async_notify",
			SkipHolds: true,
			Run: func() {
				mu.Lock()
				ran = append(ran, i)
				mu.Unlock()
			},
		}}) {
			t.Fatalf("injection %d of %d refused (the queue holds %d): a child's report would be dropped", i, n, inboxChanSize)
		}
	}
	close(release)
	if !waitFor(10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(ran) == n
	}) {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("%d of %d injections ran", len(ran), n)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, v := range ran {
		if v != i {
			t.Fatalf("injection %d ran in place %d: order lost (%v)", v, i, ran)
		}
	}
}
