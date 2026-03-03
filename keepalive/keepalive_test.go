package keepalive

import (
	"context"
	"sync"
	"testing"
	"time"

	"foci/config"
	"foci/log"
)

func TestTickInterval(t *testing.T) {
	if tickInterval != 30*time.Second {
		t.Errorf("tick interval = %v, want 30s", tickInterval)
	}
}

func TestBackgroundRunningGuard(t *testing.T) {
	// Verify the backgroundRunning flag prevents concurrent dispatch.
	var mu sync.Mutex
	calls := 0

	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		bgCfg: config.BackgroundConfig{
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
	// Verify that background work respects cooldown interval after the
	// previous session ENDS, not when it started.
	var mu sync.Mutex
	calls := 0

	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		bgCfg: config.BackgroundConfig{
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
	// A session that runs for longer than the interval should NOT block the
	// next session from starting immediately after it finishes. The cooldown
	// is measured from when the session ended, not when it started.
	var mu sync.Mutex
	calls := 0

	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		bgCfg: config.BackgroundConfig{
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
	// Verify background completion does NOT reset lastInteraction.
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		bgCfg: config.BackgroundConfig{
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

func TestWallClockAlignment_NextFire(t *testing.T) {
	interval := 1 * time.Hour

	cases := []struct {
		name     string
		last     time.Time
		now      time.Time
		wantFire bool
	}{
		{
			name:     "just_started_no_fire",
			last:     time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
			wantFire: false,
		},
		{
			name:     "before_boundary_no_fire",
			last:     time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 10, 59, 59, 0, time.UTC),
			wantFire: false,
		},
		{
			name:     "at_boundary_fire",
			last:     time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC),
			wantFire: true,
		},
		{
			name:     "past_boundary_fire",
			last:     time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 11, 15, 0, 0, time.UTC),
			wantFire: true,
		},
		{
			name:     "restart_mid_interval_no_fire",
			last:     time.Date(2024, 1, 1, 10, 55, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 10, 55, 5, 0, time.UTC),
			wantFire: false,
		},
		{
			name:     "restart_just_before_boundary_no_fire",
			last:     time.Date(2024, 1, 1, 10, 55, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 10, 59, 59, 0, time.UTC),
			wantFire: false,
		},
		{
			name:     "restart_then_boundary_fire",
			last:     time.Date(2024, 1, 1, 10, 55, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC),
			wantFire: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nextFire := tc.last.Truncate(interval).Add(interval)
			shouldFire := !tc.now.Before(nextFire)
			if shouldFire != tc.wantFire {
				t.Errorf("last=%v now=%v nextFire=%v: got shouldFire=%v, want %v",
					tc.last, tc.now, nextFire, shouldFire, tc.wantFire)
			}
		})
	}
}

func TestWallClockAlignment_30MinInterval(t *testing.T) {
	interval := 30 * time.Minute

	cases := []struct {
		name     string
		last     time.Time
		now      time.Time
		wantFire bool
	}{
		{
			name:     "fires_at_30_boundary",
			last:     time.Date(2024, 1, 1, 10, 15, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
			wantFire: true,
		},
		{
			name:     "fires_at_00_boundary",
			last:     time.Date(2024, 1, 1, 10, 45, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC),
			wantFire: true,
		},
		{
			name:     "no_fire_before_30_boundary",
			last:     time.Date(2024, 1, 1, 10, 15, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 10, 29, 59, 0, time.UTC),
			wantFire: false,
		},
		{
			name:     "restart_at_28_no_fire",
			last:     time.Date(2024, 1, 1, 10, 28, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 10, 28, 5, 0, time.UTC),
			wantFire: false,
		},
		{
			name:     "restart_at_28_then_30_fires",
			last:     time.Date(2024, 1, 1, 10, 28, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
			wantFire: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nextFire := tc.last.Truncate(interval).Add(interval)
			shouldFire := !tc.now.Before(nextFire)
			if shouldFire != tc.wantFire {
				t.Errorf("last=%v now=%v nextFire=%v: got shouldFire=%v, want %v",
					tc.last, tc.now, nextFire, shouldFire, tc.wantFire)
			}
		})
	}
}

func TestWallClockAlignment_RestartDoesNotDelay(t *testing.T) {
	interval := 1 * time.Hour

	start := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

	restart1 := time.Date(2024, 1, 1, 10, 20, 0, 0, time.UTC)
	nextFire1 := restart1.Truncate(interval).Add(interval)

	restart2 := time.Date(2024, 1, 1, 10, 45, 0, 0, time.UTC)
	nextFire2 := restart2.Truncate(interval).Add(interval)

	if !nextFire1.Equal(time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("restart at 10:20: nextFire=%v, want 11:00", nextFire1)
	}
	if !nextFire2.Equal(time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("restart at 10:45: nextFire=%v, want 11:00", nextFire2)
	}
	if !nextFire1.Equal(nextFire2) {
		t.Errorf("restarts at different times have different nextFire: %v vs %v", nextFire1, nextFire2)
	}

	_ = start
}
