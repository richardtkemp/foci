package periodic

import (
	"context"
	"testing"

	"foci/internal/log"
)

// TestMaybeEphemeralCleanup verifies the daily GC gate: disabled at 0 days,
// fires once when enabled (passing the retention through), and skips a second
// immediate call (24h gate).
func TestMaybeEphemeralCleanup(t *testing.T) {
	calls := 0
	lastDays := 0
	fake := &fakeBackgroundAgent{
		cleanupFn: func(_ context.Context, retentionDays int) int {
			calls++
			lastDays = retentionDays
			return 2
		},
	}

	// Disabled (0 days) → never calls.
	rOff := &Runner{log: log.NewComponentLogger("keepalive:test"), agentID: "test", agent: fake, ephemeralRetentionDays: 0}
	rOff.maybeEphemeralCleanup(context.Background())
	if calls != 0 {
		t.Fatalf("disabled cleanup fired %d times", calls)
	}

	// Enabled → fires once, passing the retention days through.
	r := &Runner{log: log.NewComponentLogger("keepalive:test"), agentID: "test", agent: fake, ephemeralRetentionDays: 30}
	r.maybeEphemeralCleanup(context.Background())
	if calls != 1 || lastDays != 30 {
		t.Fatalf("first fire: calls=%d days=%d, want 1/30", calls, lastDays)
	}

	// Second immediate call → skipped by the 24h gate.
	r.maybeEphemeralCleanup(context.Background())
	if calls != 1 {
		t.Fatalf("24h gate failed: calls=%d, want 1", calls)
	}
}
