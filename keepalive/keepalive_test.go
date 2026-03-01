package keepalive

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"foci/config"
)

func TestManaIsGood_InCredit(t *testing.T) {
	now := time.Now()
	// 2.5 hours into window, 50% mana remaining
	// expected_mana = 100 * (5h - 2.5h) / (5h - 30m) = 100 * 2.5h / 4.5h ≈ 55.6%
	// actual 50% < expected 55.6% → NOT good
	resetsAt := now.Add(2*time.Hour + 30*time.Minute) // 2.5h remaining → 2.5h since reset
	if ManaIsGood(50, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana NOT good at 50% with 2.5h elapsed (below expected ~55.6%)")
	}

	// Same point in time but 70% mana — above the line
	if !ManaIsGood(70, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana good at 70% with 2.5h elapsed (above expected ~55.6%)")
	}
}

func TestManaIsGood_InvestPeriod(t *testing.T) {
	now := time.Now()
	// 10 minutes into window (within 30m invest interval)
	resetsAt := now.Add(4*time.Hour + 50*time.Minute)
	if ManaIsGood(95, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana NOT good during invest period, even with 95% mana")
	}
}

func TestManaIsGood_NearReset(t *testing.T) {
	now := time.Now()
	// 2 minutes to reset, 5% mana
	// time_since_reset = 4h58m
	// expected_mana = 100 * (5h - 4h58m) / (5h - 30m) = 100 * 2m / 270m ≈ 0.74%
	// 5% > 0.74% → good
	resetsAt := now.Add(2 * time.Minute)
	if !ManaIsGood(5, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana good near reset (5% > expected ~0.74%)")
	}
}

func TestManaIsGood_JustAfterInvest(t *testing.T) {
	now := time.Now()
	// Exactly at invest interval boundary (30m into window)
	resetsAt := now.Add(4*time.Hour + 30*time.Minute)
	// expected_mana = 100 * (5h - 30m) / (5h - 30m) = 100%
	// Need > 100% which is impossible, so this should be false
	if ManaIsGood(99, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana NOT good right at invest boundary (99% < expected 100%)")
	}

	// Slightly past invest (31m in)
	resetsAt = now.Add(4*time.Hour + 29*time.Minute) // 31m since reset
	// expected_mana = 100 * (5h - 31m) / (5h - 30m) = 100 * 269m / 270m ≈ 99.6%
	// 99% < 99.6% → not good, but 100% would be good
	if ManaIsGood(99, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana NOT good at 99% just past invest (below expected ~99.6%)")
	}
	if !ManaIsGood(100, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana good at 100% just past invest (above expected ~99.6%)")
	}
}

func TestManaIsGood_ZeroReset(t *testing.T) {
	// No reset time = allow
	if !ManaIsGood(50, time.Time{}, 30*time.Minute, time.Now()) {
		t.Error("expected mana good when reset time is zero (no data)")
	}
}

func TestManaIsGood_MidWindow(t *testing.T) {
	now := time.Now()
	// Exactly halfway through window: 2.5h since reset, 2.5h to go
	// expected = 100 * (5h - 2.5h) / (5h - 30m) = 100 * 2.5h / 4.5h ≈ 55.6%
	resetsAt := now.Add(2*time.Hour + 30*time.Minute)

	// 60% > 55.6% → good
	if !ManaIsGood(60, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana good at 60% midway (above expected ~55.6%)")
	}

	// 40% < 55.6% → not good
	if ManaIsGood(40, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana NOT good at 40% midway (below expected ~55.6%)")
	}
}

func TestManaIsGood_PastReset(t *testing.T) {
	now := time.Now()
	// Reset was 5 minutes ago (past)
	resetsAt := now.Add(-5 * time.Minute)
	// time_since_reset = 5h + 5m, clamped: expected ≈ negative → any mana is good
	if !ManaIsGood(1, resetsAt, 30*time.Minute, now) {
		t.Error("expected mana good when past reset time")
	}
}

func TestTickInterval(t *testing.T) {
	if tickInterval != 30*time.Second {
		t.Errorf("tick interval = %v, want 30s", tickInterval)
	}
}

func TestManaWindow(t *testing.T) {
	if manaWindow != 5*time.Hour {
		t.Errorf("mana window = %v, want 5h", manaWindow)
	}
}

func TestOrientationBuilderIntegration(t *testing.T) {
	// OrientationBuilder is now injected from main. Verify the type is usable.
	var builder OrientationBuilder = func(branchKey, parentKey, branchType string) string {
		return fmt.Sprintf("branch=%s parent=%s type=%s", branchKey, parentKey, branchType)
	}
	text := builder("branch:1", "parent:1", "keepalive")
	if !containsAll(text, "branch:1", "parent:1", "keepalive") {
		t.Errorf("builder missing values: %s", text)
	}
}

func TestBackgroundRunningGuard(t *testing.T) {
	// Verify the backgroundRunning flag prevents concurrent dispatch.
	var mu sync.Mutex
	calls := 0

	r := &Runner{
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
	// Verify that background work respects cooldown interval after completion.
	var mu sync.Mutex
	calls := 0

	r := &Runner{
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

	// Wait for goroutine to complete
	time.Sleep(50 * time.Millisecond)

	// Immediately try again — should be blocked by cooldown
	// (lastBackgroundStarted was <1s ago)
	r.maybeBackgroundWork(context.Background())

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()

	if got != 1 {
		t.Errorf("expected 1 background call (cooldown should block second), got %d", got)
	}
}

func TestBackgroundNoSelfChaining(t *testing.T) {
	// Verify background completion does NOT reset lastInteraction.
	r := &Runner{
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

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
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
