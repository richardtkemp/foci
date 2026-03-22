package periodic

import (
	"context"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
)


func TestBackgroundRunningGuard(t *testing.T) {
	// Verifies the backgroundRunning flag prevents concurrent dispatch. Calls maybeBackgroundWork
	// twice while the first is still running and confirms only one branchFn invocation occurs.
	var mu sync.Mutex
	calls := 0

	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		bgCfg: config.ResolvedBackground{
			Enabled:  true,
			Interval: "1s",
		},
		lastInteraction: time.Now().Add(-2 * time.Second), // idle for 2s (> 1s interval)
		branchFn: func(branchType, promptText string, noCompact bool) {
			mu.Lock()
			calls++
			mu.Unlock()
			time.Sleep(100 * time.Millisecond)
		},
		done: make(chan struct{}),
	}

	// First call should fire
	r.maybeBackgroundWork(context.Background())

	// Wait for goroutine to start
	time.Sleep(20 * time.Millisecond)

	// Second call while first is still running should be skipped
	r.maybeBackgroundWork(context.Background())

	// Wait for first to complete
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()

	if got != 1 {
		t.Errorf("expected 1 background call (guard should block second), got %d", got)
	}
}

func TestBackgroundCooldown(t *testing.T) {
	// Verifies that background work respects the cooldown interval measured from when the previous
	// session ended. Fires once, then immediately tries again and confirms the second is skipped.
	var mu sync.Mutex
	calls := 0

	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		bgCfg: config.ResolvedBackground{
			Enabled:  true,
			Interval: "1s",
		},
		lastInteraction: time.Now().Add(-2 * time.Second),
		branchFn: func(branchType, promptText string, noCompact bool) {
			mu.Lock()
			calls++
			mu.Unlock()
		},
		done: make(chan struct{}),
	}

	// First call should fire
	r.maybeBackgroundWork(context.Background())

	// Wait for goroutine to complete (sets lastBackgroundEnded)
	time.Sleep(50 * time.Millisecond)

	// Immediately try again — should be blocked by cooldown
	// (lastBackgroundEnded was <1s ago)
	r.maybeBackgroundWork(context.Background())

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()

	if got != 1 {
		t.Errorf("expected 1 background call (cooldown should block second), got %d", got)
	}
}

func TestBackgroundCooldownFromEndNotStart(t *testing.T) {
	// Verifies lastBackgroundEnded is stamped when the session finishes, not when it starts.
	// Runs a simulated long session and checks the timestamp is recent relative to completion.
	var mu sync.Mutex
	calls := 0

	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		bgCfg: config.ResolvedBackground{
			Enabled:  true,
			Interval: "1s",
		},
		lastInteraction: time.Now().Add(-5 * time.Second),
		branchFn: func(branchType, promptText string, noCompact bool) {
			mu.Lock()
			calls++
			mu.Unlock()
			// Simulate a session that runs longer than the interval
			time.Sleep(100 * time.Millisecond)
		},
		done: make(chan struct{}),
	}

	// First call fires — runs for 100ms (simulating a long session)
	r.maybeBackgroundWork(context.Background())

	// Wait for it to finish
	time.Sleep(200 * time.Millisecond)

	// Verify lastBackgroundEnded was set after the session finished
	r.mu.Lock()
	endedAge := time.Since(r.lastBackgroundEnded)
	r.mu.Unlock()

	// lastBackgroundEnded should be very recent (< 200ms ago, not 300ms+ ago)
	if endedAge > 200*time.Millisecond {
		t.Errorf("lastBackgroundEnded is %v old, expected < 200ms (should be set at end, not start)", endedAge)
	}
}

func TestBackgroundNoSelfChaining(t *testing.T) {
	// Verifies that completing a background session does not reset lastInteraction, which would
	// cause the runner to immediately schedule another background session (self-chaining loop).
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		bgCfg: config.ResolvedBackground{
			Enabled:  true,
			Interval: "1s",
		},
		lastInteraction: time.Now().Add(-2 * time.Second),
		branchFn: func(branchType, promptText string, noCompact bool) {
			// no-op
		},
		done: make(chan struct{}),
	}

	interactionBefore := r.lastInteraction

	r.maybeBackgroundWork(context.Background())

	// Wait for goroutine
	time.Sleep(50 * time.Millisecond)

	r.mu.Lock()
	interactionAfter := r.lastInteraction
	r.mu.Unlock()

	if !interactionAfter.Equal(interactionBefore) {
		t.Errorf("background completion should not reset lastInteraction: before=%v after=%v",
			interactionBefore, interactionAfter)
	}
}
