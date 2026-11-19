package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
)

// stoppableCompactBackend is a delegated backend whose compaction never
// completes on its own: WaitForCompaction returns only what the test sends
// on outcome, or ctx's error. That is codex before #3172 after a /stop
// aborted its compaction turn.
type stoppableCompactBackend struct {
	*mockBackendDM
	waiting chan struct{} // closed once WaitForCompaction is blocked
	outcome chan error
	once    sync.Once
}

func newStoppableCompactBackend() *stoppableCompactBackend {
	return &stoppableCompactBackend{
		mockBackendDM: &mockBackendDM{running: true},
		waiting:       make(chan struct{}),
		outcome:       make(chan error, 1),
	}
}

func (b *stoppableCompactBackend) ArmCompactionWait() {}

func (b *stoppableCompactBackend) WaitForCompaction(ctx context.Context) error {
	b.once.Do(func() { close(b.waiting) })
	select {
	case err := <-b.outcome:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runCompactAsync runs the auto-compaction primitive as the post-turn path
// does (context.Background, which nothing else can cancel) and returns its
// result channel and the notices the chat got.
func runCompactAsync(t *testing.T, ag *Agent, be delegator.Delegator, sk string) (<-chan error, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var notices []string
	ag.CompactionNotifyFunc.Add(func(_, msg, _ string) {
		mu.Lock()
		notices = append(notices, msg)
		mu.Unlock()
	})
	done := make(chan error, 1)
	go func() { done <- ag.runDelegatedCompact(context.Background(), be, sk, false) }()
	return done, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), notices...)
	}
}

func awaitCompact(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("runDelegatedCompact still waiting 5s after the stop (before #3172 it waited the full 20m delegatedCompactTimeout)")
		return nil
	}
}

func assertStoppedNotice(t *testing.T, notices []string, wantSub string) {
	t.Helper()
	if len(notices) != 1 {
		t.Fatalf("notices = %q, want exactly one", notices)
	}
	n := notices[0]
	if !strings.HasPrefix(n, "⏹️ Compaction stopped") || !strings.Contains(n, wantSub) {
		t.Errorf("notice = %q, want a ⏹️ stopped notice containing %q", n, wantSub)
	}
	if strings.Contains(n, "did not finish within") {
		t.Errorf("notice = %q: a stopped compaction is not a timeout", n)
	}
}

// TestStopEndsAutoCompactionWait is the #3172 regression: /stop during an
// auto-compaction (CancelSession) ends foci's wait at once, the session's
// compacting latch clears so held input flows, and the ⏳ is resolved as
// stopped rather than "did not finish within 20m".
func TestStopEndsAutoCompactionWait(t *testing.T) {
	t.Parallel()
	const sk = "test/stop-compact"
	ag := &Agent{}
	be := newStoppableCompactBackend()
	done, notices := runCompactAsync(t, ag, be, sk)
	<-be.waiting
	if !ag.IsCompacting(sk) {
		t.Fatal("premise: session should read as compacting while the wait runs")
	}

	if !ag.CancelSession(sk) {
		t.Error("CancelSession = false, want true: it ended a compaction wait")
	}
	err := awaitCompact(t, done)
	if !errors.Is(err, delegator.ErrCompactionStopped) {
		t.Fatalf("runDelegatedCompact = %v, want ErrCompactionStopped", err)
	}
	if !CompactionNoticeShown(err) {
		t.Error("error should mark the notice as shown")
	}
	if ag.IsCompacting(sk) {
		t.Error("session still compacting after the stop: held input is not released")
	}
	assertStoppedNotice(t, notices(), "foci stopped waiting")
}

// TestBackendReportedStopResolvesNotice: a backend that reports its
// compaction turn interrupted (codex, #3172) gets the stopped notice that
// says the context was not compacted.
func TestBackendReportedStopResolvesNotice(t *testing.T) {
	t.Parallel()
	const sk = "test/backend-stop"
	ag := &Agent{}
	be := newStoppableCompactBackend()
	done, notices := runCompactAsync(t, ag, be, sk)
	<-be.waiting
	be.outcome <- delegator.ErrCompactionStopped

	err := awaitCompact(t, done)
	if !errors.Is(err, delegator.ErrCompactionStopped) {
		t.Fatalf("runDelegatedCompact = %v, want ErrCompactionStopped", err)
	}
	assertStoppedNotice(t, notices(), "not compacted")
}

// TestStopMarkedBeforeBackendSettles: /stop marks the compaction before it
// interrupts the backend, and a backend can settle the wait in that gap
// (CC goes idle without a boundary). That outcome is a stop, not "nothing
// to compact".
func TestStopMarkedBeforeBackendSettles(t *testing.T) {
	t.Parallel()
	const sk = "test/stop-mark"
	ag := &Agent{}
	be := newStoppableCompactBackend()
	done, notices := runCompactAsync(t, ag, be, sk)
	<-be.waiting
	if !ag.MarkCompactionStopping(sk) {
		t.Fatal("MarkCompactionStopping = false, want true while a compaction waits")
	}
	be.outcome <- delegator.ErrCompactionNoBoundary

	err := awaitCompact(t, done)
	if !errors.Is(err, delegator.ErrCompactionStopped) {
		t.Fatalf("runDelegatedCompact = %v, want ErrCompactionStopped", err)
	}
	assertStoppedNotice(t, notices(), "Compaction stopped")
}

// TestCompactionRunUnregisteredAfterReturn: once the compaction returns, a
// later /stop finds no compaction to end.
func TestCompactionRunUnregisteredAfterReturn(t *testing.T) {
	t.Parallel()
	const sk = "test/stop-after"
	ag := &Agent{}
	be := newStoppableCompactBackend()
	done, _ := runCompactAsync(t, ag, be, sk)
	<-be.waiting
	be.outcome <- nil
	if err := awaitCompact(t, done); err != nil {
		t.Fatalf("runDelegatedCompact = %v, want nil", err)
	}
	if ag.MarkCompactionStopping(sk) || ag.CancelSession(sk) {
		t.Error("a finished compaction is still registered for /stop")
	}
}
