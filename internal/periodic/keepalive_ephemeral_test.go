package periodic

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/log"
)

// TestMaybeEphemeralCleanup verifies the daily GC gate: disabled at 0 days,
// fires once when enabled (passing the retention through), and skips a second
// immediate call (24h gate). The GC runs off the tick goroutine, so the test
// waits for the async run to settle before asserting.
func TestMaybeEphemeralCleanup(t *testing.T) {
	var calls, lastDays atomic.Int64
	fake := &fakeBackgroundAgent{
		cleanupFn: func(_ context.Context, retentionDays int) int {
			calls.Add(1)
			lastDays.Store(int64(retentionDays))
			return 2
		},
	}

	// Disabled (0 days) → never calls.
	rOff := &Runner{log: log.NewComponentLogger("keepalive:test"), agentID: "test", agent: fake, ephemeralRetentionDays: 0}
	rOff.maybeEphemeralCleanup(context.Background())
	if got := calls.Load(); got != 0 {
		t.Fatalf("disabled cleanup fired %d times", got)
	}

	// Enabled → fires once (async), passing the retention days through.
	r := &Runner{log: log.NewComponentLogger("keepalive:test"), agentID: "test", agent: fake, ephemeralRetentionDays: 30}
	r.maybeEphemeralCleanup(context.Background())
	waitUntil(t, func() bool { return calls.Load() == 1 && !r.cleanupRunning() })
	if got := lastDays.Load(); got != 30 {
		t.Fatalf("first fire: days=%d, want 30", got)
	}

	// Second immediate call → skipped by the 24h gate (the GC has settled, so
	// this exercises the timestamp gate, not the running guard).
	r.maybeEphemeralCleanup(context.Background())
	if got := calls.Load(); got != 1 {
		t.Fatalf("24h gate failed: calls=%d, want 1", got)
	}
}

func (r *Runner) cleanupRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ephemeralCleanupRunning
}

// waitUntil polls cond up to 2s, failing the test if it never holds.
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}
