package periodic

import (
	"context"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
)

// newResetRunner builds a minimal Runner wired for maybeReset tests. resetCh
// receives the session key when the (async) reset fires.
func newResetRunner(t *testing.T, maint config.ResolvedMaintenance, lastReset, lastInteraction time.Time, resetCh chan string) *Runner {
	t.Helper()
	return &Runner{
		log:             log.NewComponentLogger("keepalive:test"),
		agentID:         "test",
		maintCfg:        maint,
		lastReset:       lastReset,
		lastInteraction: lastInteraction,
		sessionKeyFn:    func() string { return "test/c123/1000000000" },
		resetFn: func(ctx context.Context, sk string) error {
			resetCh <- sk
			return nil
		},
		done: make(chan struct{}),
	}
}

func TestMaybeReset_Fires(t *testing.T) {
	resetCh := make(chan string, 1)
	r := newResetRunner(t,
		config.ResolvedMaintenance{ResetTime: "1h", ResetIdleGuard: "55m"},
		time.Now().Add(-2*time.Hour), // last reset well past
		time.Now().Add(-2*time.Hour), // user idle longer than the guard
		resetCh)

	r.maybeReset(context.Background())

	select {
	case sk := <-resetCh:
		if sk != "test/c123/1000000000" {
			t.Errorf("reset fired on wrong session: %q", sk)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected reset to fire, but it did not")
	}
}

func TestMaybeReset_SkipsWhenRecentlyActive(t *testing.T) {
	resetCh := make(chan string, 1)
	r := newResetRunner(t,
		config.ResolvedMaintenance{ResetTime: "1h", ResetIdleGuard: "55m"},
		time.Now().Add(-2*time.Hour),
		time.Now().Add(-10*time.Minute), // active 10m ago, inside the 55m guard
		resetCh)

	r.maybeReset(context.Background())

	select {
	case <-resetCh:
		t.Fatal("reset fired despite recent activity inside the guard window")
	case <-time.After(200 * time.Millisecond):
		// expected: no fire
	}
}

func TestMaybeReset_DisabledWhenEmpty(t *testing.T) {
	resetCh := make(chan string, 1)
	r := newResetRunner(t,
		config.ResolvedMaintenance{ResetTime: "", ResetIdleGuard: "55m"},
		time.Now().Add(-2*time.Hour),
		time.Now().Add(-2*time.Hour),
		resetCh)

	r.maybeReset(context.Background())

	select {
	case <-resetCh:
		t.Fatal("reset fired when reset_time is empty (should be disabled)")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestMaybeReset_NoResetFn(t *testing.T) {
	r := &Runner{
		log:      log.NewComponentLogger("keepalive:test"),
		agentID:  "test",
		maintCfg: config.ResolvedMaintenance{ResetTime: "1h", ResetIdleGuard: "55m"},
		done:     make(chan struct{}),
	}
	// Must not panic with a nil resetFn.
	r.maybeReset(context.Background())
}

func TestMaybeReset_SkipsTooSoon(t *testing.T) {
	resetCh := make(chan string, 1)
	r := newResetRunner(t,
		config.ResolvedMaintenance{ResetTime: "1h", ResetIdleGuard: "55m"},
		time.Now(),                   // just reset → next slot is in the future
		time.Now().Add(-2*time.Hour), // guard satisfied
		resetCh)

	r.maybeReset(context.Background())

	select {
	case <-resetCh:
		t.Fatal("reset fired before the next scheduled slot")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestMaybeReset_FiresWithoutGuard proves an empty/invalid reset_idle_guard
// disables the inactivity check (always fire at the scheduled time).
func TestMaybeReset_FiresWithoutGuard(t *testing.T) {
	resetCh := make(chan string, 1)
	r := newResetRunner(t,
		config.ResolvedMaintenance{ResetTime: "1h", ResetIdleGuard: ""},
		time.Now().Add(-2*time.Hour),
		time.Now(), // active right now, but no guard configured
		resetCh)

	r.maybeReset(context.Background())

	select {
	case <-resetCh:
		// expected: fires despite recent activity
	case <-time.After(2 * time.Second):
		t.Fatal("expected reset to fire with no guard configured")
	}
}
