package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// deferredRefusalFixture leaves sk with one inject deferred behind ask-1
// (added by hold) and a full inbox channel behind a blocked worker, so the next
// release is refused. The returned func unblocks the worker.
func deferredRefusalFixture(t *testing.T, a *Agent, sk string, hold func()) (unblock func()) {
	t.Helper()
	var cur atomic.Value
	cur.Store("ask-1")
	a.AskRouter = askRouterPending(&cur)

	hold()
	inb := a.getOrCreateInbox(sk)
	if !waitFor(2*time.Second, func() bool {
		inb.injMu.Lock()
		defer inb.injMu.Unlock()
		return len(inb.deferredInjects) == 1
	}) {
		t.Fatal("the injection was never deferred behind the ask")
	}

	// An ask result of another ask is not held, so it occupies the worker.
	started, release := make(chan struct{}), make(chan struct{})
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger:  "async_notify",
		AskReqID: "other-ask",
		Run:      func() { close(started); <-release },
	}})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the blocking injection never started")
	}

	full := false
	for i := 0; i <= inboxChanSize && !full; i++ {
		full = !a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
			Trigger:  "async_notify",
			AskReqID: "other-ask",
			Run:      func() {},
		}})
	}
	if !full {
		t.Fatal("the inbox channel never filled")
	}
	return func() { close(release) }
}

// TestDeferredRelease_FullQueueFiresRefused is the #2847 repro: an inject the
// inbox accepted, deferred behind an ask, and then could not re-queue on release
// must fire Refused. Its producer holds state on the assumption that it runs: a
// wake's fired overlay, a spawn result awaiting its delivered verdict.
func TestDeferredRelease_FullQueueFiresRefused(t *testing.T) {
	t.Parallel()
	releases := map[string]func(*Agent, string){
		"ask resolved": func(a *Agent, sk string) { a.DrainDeferredInjects(sk, "ask-1") },
		"ask hold limit": func(a *Agent, sk string) {
			// Backdated instead of a short limit: the limit's own timer would
			// release the inject before the fixture has filled the queue.
			inb := a.getOrCreateInbox(sk)
			inb.injMu.Lock()
			inb.deferredInjects[0].env.askHeldSince = time.Now().Add(-2 * time.Hour)
			inb.injMu.Unlock()
			a.releaseExpiredAskHolds(sk)
		},
	}
	for name, release := range releases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a, cancel := startedAgent(t)
			defer cancel()
			a.askHoldMax = time.Hour
			sk := "test/c2847"

			var refused, ran atomic.Int32
			unblock := deferredRefusalFixture(t, a, sk, func() {
				a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
					Trigger: "async_notify",
					Run:     func() { ran.Add(1) },
					Refused: func() { refused.Add(1) },
				}})
			})
			defer unblock()

			release(a, sk)
			if got := refused.Load(); got != 1 {
				t.Errorf("Refused fired %d times, want 1", got)
			}
			if got := ran.Load(); got != 0 {
				t.Errorf("Run fired %d times on a refused inject, want 0", got)
			}
			inb := a.getOrCreateInbox(sk)
			inb.injMu.Lock()
			defer inb.injMu.Unlock()
			if len(inb.deferredInjects) != 0 {
				t.Errorf("%d injects still deferred, want 0: a second release would fire Refused again", len(inb.deferredInjects))
			}
		})
	}
}

// TestEnqueueInjectWait_ReleaseRefusedIsNotShutdown: the waiter is released when
// a full queue refuses its deferred inject, and is not told the agent is shutting
// down, because it is not. A caller reads ErrShuttingDown as "the turn is
// re-fired by the next process".
func TestEnqueueInjectWait_ReleaseRefusedIsNotShutdown(t *testing.T) {
	t.Parallel()
	a, cancel := startedAgent(t)
	defer cancel()
	a.askHoldMax = time.Hour
	sk := "test/c2847w"

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer waitCancel()
	errc := make(chan error, 1)
	unblock := deferredRefusalFixture(t, a, sk, func() {
		go func() {
			errc <- a.EnqueueInjectWait(waitCtx, sk, "async_notify", func() {})
		}()
	})
	defer unblock()

	a.DrainDeferredInjects(sk, "ask-1")
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("EnqueueInjectWait returned nil for an inject that never ran")
		}
		if errors.Is(err, ErrShuttingDown) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the inbox-rejected error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EnqueueInjectWait still waiting: the refused release did not reach it")
	}
}
