package keepalive

import (
	"context"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/state"
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

func TestNew(t *testing.T) {
	cfg := RunnerConfig{
		AgentID: "test-agent",
		Keepalive: config.KeepaliveConfig{
			Enabled: true,
		},
		Background: config.BackgroundConfig{
			Enabled: true,
		},
		MemoryFormation: config.MemoryFormationConfig{
			Interval: "1h",
		},
		BranchFunc: func(branchType, promptText string, noCompact bool) {},
	}

	r := New(cfg)
	if r.agentID != "test-agent" {
		t.Errorf("agentID = %q, want test-agent", r.agentID)
	}
	if !r.kaCfg.Enabled {
		t.Errorf("keepalive not enabled")
	}
	if !r.bgCfg.Enabled {
		t.Errorf("background not enabled")
	}
	if r.done == nil {
		t.Errorf("done channel not initialized")
	}
}

func TestNew_WithStateStore(t *testing.T) {
	// Create a temporary state store file
	tmpfile := t.TempDir() + "/state.json"
	ss := state.New(tmpfile)
	consolidationTime := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	ss.Set("consolidation_last:test-agent", consolidationTime)

	cfg := RunnerConfig{
		AgentID:    "test-agent",
		StateStore: ss,
		BranchFunc: func(branchType, promptText string, noCompact bool) {},
	}

	r := New(cfg)

	r.mu.Lock()
	if !r.lastConsolidation.Equal(consolidationTime) {
		t.Errorf("lastConsolidation = %v, want %v", r.lastConsolidation, consolidationTime)
	}
	r.mu.Unlock()
}

func TestNotifyCacheWarmed(t *testing.T) {
	r := &Runner{
		log:             log.NewComponentLogger("keepalive:test"),
		lastCacheWarmed: time.Now().Add(-10 * time.Second),
		done:            make(chan struct{}),
	}

	before := r.lastCacheWarmed
	time.Sleep(10 * time.Millisecond)
	r.NotifyCacheWarmed()
	after := r.lastCacheWarmed

	if !after.After(before) {
		t.Errorf("lastCacheWarmed not updated")
	}
}

func TestNotifyInteraction(t *testing.T) {
	r := &Runner{
		log:            log.NewComponentLogger("keepalive:test"),
		lastInteraction: time.Now().Add(-10 * time.Second),
		done:           make(chan struct{}),
	}

	before := r.lastInteraction
	time.Sleep(10 * time.Millisecond)
	r.NotifyInteraction()
	after := r.lastInteraction

	if !after.After(before) {
		t.Errorf("lastInteraction not updated")
	}
}

func TestMaybeKeepalive_Disabled(t *testing.T) {
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		kaCfg: config.KeepaliveConfig{
			Enabled: false,
		},
		lastCacheWarmed: time.Now().Add(-1 * time.Hour),
		branchFn: func(branchType, promptText string, noCompact bool) {
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeKeepalive(context.Background())
	if calls != 0 {
		t.Errorf("keepalive called when disabled")
	}
}

func TestMaybeKeepalive_BadInterval(t *testing.T) {
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		kaCfg: config.KeepaliveConfig{
			Enabled:  true,
			Interval: "invalid",
		},
		lastCacheWarmed: time.Now().Add(-1 * time.Hour),
		branchFn: func(branchType, promptText string, noCompact bool) {
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeKeepalive(context.Background())
	if calls != 0 {
		t.Errorf("keepalive called with bad interval")
	}
}

func TestMaybeKeepalive_RecentCache(t *testing.T) {
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		kaCfg: config.KeepaliveConfig{
			Enabled:  true,
			Interval: "1h",
		},
		lastCacheWarmed: time.Now().Add(-10 * time.Minute),
		branchFn: func(branchType, promptText string, noCompact bool) {
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeKeepalive(context.Background())
	if calls != 0 {
		t.Errorf("keepalive called with recent cache")
	}
}

func TestMaybeKeepalive_Fires(t *testing.T) {
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		kaCfg: config.KeepaliveConfig{
			Enabled:  true,
			Interval: "1m",
			Prompt:   "keepalive.md",
		},
		lastCacheWarmed: time.Now().Add(-10 * time.Minute),
		branchFn: func(branchType, promptText string, noCompact bool) {
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeKeepalive(context.Background())
	time.Sleep(50 * time.Millisecond)

	if calls != 1 {
		t.Errorf("keepalive not called, expected 1 call")
	}
}

func TestMaybeKeepalive_AlreadyRunning(t *testing.T) {
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		kaCfg: config.KeepaliveConfig{
			Enabled:  true,
			Interval: "1m",
		},
		lastCacheWarmed: time.Now().Add(-10 * time.Minute),
		keepaliveRunning: true,
		branchFn: func(branchType, promptText string, noCompact bool) {
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeKeepalive(context.Background())
	if calls != 0 {
		t.Errorf("keepalive called while already running")
	}
}

func TestMaybeBackgroundWork_Disabled(t *testing.T) {
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		bgCfg: config.BackgroundConfig{
			Enabled: false,
		},
		lastInteraction: time.Now().Add(-10 * time.Minute),
		branchFn: func(branchType, promptText string, noCompact bool) {
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeBackgroundWork(context.Background())
	if calls != 0 {
		t.Errorf("background work called when disabled")
	}
}

func TestMaybeBackgroundWork_BadInterval(t *testing.T) {
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		bgCfg: config.BackgroundConfig{
			Enabled:  true,
			Interval: "invalid",
		},
		lastInteraction: time.Now().Add(-10 * time.Minute),
		branchFn: func(branchType, promptText string, noCompact bool) {
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeBackgroundWork(context.Background())
	if calls != 0 {
		t.Errorf("background work called with bad interval")
	}
}

func TestMaybeBackgroundWork_RecentInteraction(t *testing.T) {
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		bgCfg: config.BackgroundConfig{
			Enabled:  true,
			Interval: "1h",
		},
		lastInteraction: time.Now().Add(-10 * time.Minute),
		branchFn: func(branchType, promptText string, noCompact bool) {
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeBackgroundWork(context.Background())
	if calls != 0 {
		t.Errorf("background work called with recent interaction")
	}
}

func TestMaybeMemoryFormation_Disabled(t *testing.T) {
	disabled := false
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		mfCfg: config.MemoryFormationConfig{
			IntervalEnabled: &disabled,
		},
		lastInteraction:  time.Now().Add(-1 * time.Hour),
		lastMemoryFormation: time.Now().Add(-2 * time.Hour),
		branchFn: func(branchType, promptText string, noCompact bool) {
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeMemoryFormation()
	if calls != 0 {
		t.Errorf("memory formation called when disabled")
	}
}

func TestMaybeMemoryFormation_BadInterval(t *testing.T) {
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		mfCfg: config.MemoryFormationConfig{
			Interval: "invalid",
		},
		lastInteraction:  time.Now().Add(-1 * time.Hour),
		lastMemoryFormation: time.Now().Add(-2 * time.Hour),
		branchFn: func(branchType, promptText string, noCompact bool) {
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeMemoryFormation()
	if calls != 0 {
		t.Errorf("memory formation called with bad interval")
	}
}

func TestMaybeConsolidation_Disabled(t *testing.T) {
	disabled := false
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		mfCfg: config.MemoryFormationConfig{
			ConsolidationEnabled: &disabled,
		},
		lastInteraction:  time.Now(),
		lastConsolidation: time.Now().Add(-2 * time.Hour),
		branchFn: func(branchType, promptText string, noCompact bool) {
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeConsolidation()
	if calls != 0 {
		t.Errorf("consolidation called when disabled")
	}
}

func TestMaybeConsolidation_BadInterval(t *testing.T) {
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		mfCfg: config.MemoryFormationConfig{
			ConsolidationInterval: "invalid",
		},
		lastInteraction:  time.Now(),
		lastConsolidation: time.Now().Add(-2 * time.Hour),
		branchFn: func(branchType, promptText string, noCompact bool) {
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeConsolidation()
	if calls != 0 {
		t.Errorf("consolidation called with bad interval")
	}
}

func TestStartStop(t *testing.T) {
	r := New(RunnerConfig{
		AgentID: "test",
		Keepalive: config.KeepaliveConfig{
			Enabled: false,
		},
		Background: config.BackgroundConfig{
			Enabled: false,
		},
		MemoryFormation: config.MemoryFormationConfig{
			Interval: "1h",
		},
		BranchFunc: func(branchType, promptText string, noCompact bool) {},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	r.Start(ctx)
	r.Stop()

	// If we got here without deadlock, test passed
}

func TestStop_WithoutStart(t *testing.T) {
	r := New(RunnerConfig{
		AgentID:    "test",
		BranchFunc: func(branchType, promptText string, noCompact bool) {},
	})

	r.Stop() // Should not panic
}

func TestMaybeMemoryFormation_NoActivity(t *testing.T) {
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		mfCfg: config.MemoryFormationConfig{
			Interval: "1h",
		},
		lastInteraction:     time.Now().Add(-2 * time.Hour),
		lastMemoryFormation: time.Now().Add(-2 * time.Hour),
		branchFn: func(branchType, promptText string, noCompact bool) {
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeMemoryFormation()
	if calls != 0 {
		t.Errorf("memory formation called with no activity")
	}
}


func TestMaybeMemoryFormation_AlreadyRunning(t *testing.T) {
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		mfCfg: config.MemoryFormationConfig{
			Interval: "1h",
		},
		lastInteraction:        time.Now().Add(-30 * time.Minute),
		lastMemoryFormation:    time.Now().Add(-2 * time.Hour),
		memoryFormationRunning: true,
		branchFn: func(branchType, promptText string, noCompact bool) {
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeMemoryFormation()
	if calls != 0 {
		t.Errorf("memory formation called while already running")
	}
}

func TestMaybeMemoryFormation_Fires(t *testing.T) {
	var calls int
	now := time.Now()
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		mfCfg: config.MemoryFormationConfig{
			Interval:         "1h",
			IntervalPrompt:   "memory-formation.md",
		},
		lastInteraction:     now.Add(-30 * time.Minute),
		lastMemoryFormation: now.Add(-2 * time.Hour),
		branchFn: func(branchType, promptText string, noCompact bool) {
			if branchType != "memory-formation" {
				t.Errorf("expected branch type 'memory-formation', got %q", branchType)
			}
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeMemoryFormation()
	time.Sleep(50 * time.Millisecond)

	if calls != 1 {
		t.Errorf("memory formation not called, expected 1 call, got %d", calls)
	}
}

func TestMaybeConsolidation_TooMuchInactivity(t *testing.T) {
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		mfCfg: config.MemoryFormationConfig{
			ConsolidationInterval: "1h",
		},
		lastInteraction:   time.Now().Add(-3 * time.Hour),
		lastConsolidation: time.Now().Add(-2 * time.Hour),
		branchFn: func(branchType, promptText string, noCompact bool) {
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeConsolidation()
	if calls != 0 {
		t.Errorf("consolidation called with too much inactivity")
	}
}

func TestMaybeConsolidation_AlreadyRunning(t *testing.T) {
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		mfCfg: config.MemoryFormationConfig{
			ConsolidationInterval: "1h",
		},
		lastInteraction:    time.Now().Add(-30 * time.Minute),
		lastConsolidation:  time.Now().Add(-2 * time.Hour),
		consolidationRunning: true,
		branchFn: func(branchType, promptText string, noCompact bool) {
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeConsolidation()
	if calls != 0 {
		t.Errorf("consolidation called while already running")
	}
}

func TestMaybeConsolidation_Fires(t *testing.T) {
	var calls int
	now := time.Now()
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		mfCfg: config.MemoryFormationConfig{
			ConsolidationInterval: "1h",
			ConsolidationPrompt:   "memory-consolidation.md",
		},
		lastInteraction:   now.Add(-30 * time.Minute),
		lastConsolidation: now.Add(-2 * time.Hour),
		branchFn: func(branchType, promptText string, noCompact bool) {
			if branchType != "consolidation" {
				t.Errorf("expected branch type 'consolidation', got %q", branchType)
			}
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeConsolidation()
	time.Sleep(50 * time.Millisecond)

	if calls != 1 {
		t.Errorf("consolidation not called, expected 1 call, got %d", calls)
	}
}

func TestRun_ContextCancellation(t *testing.T) {
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		kaCfg: config.KeepaliveConfig{
			Enabled: false,
		},
		bgCfg: config.BackgroundConfig{
			Enabled: false,
		},
		mfCfg: config.MemoryFormationConfig{
			Interval: "1h",
		},
		branchFn: func(branchType, promptText string, noCompact bool) {},
		done:     make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	go r.run(ctx)

	// Give goroutine time to start
	time.Sleep(10 * time.Millisecond)

	// Cancel context to stop run loop
	cancel()

	// Wait for done to be closed
	select {
	case <-r.done:
		// Expected
	case <-time.After(1 * time.Second):
		t.Errorf("run did not exit within timeout")
	}
}

func TestBackgroundBlockedByActiveWork(t *testing.T) {
	// Verify that HasActiveWorkFn returning true prevents background dispatch.
	var mu sync.Mutex
	calls := 0

	activeWork := true

	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		bgCfg: config.BackgroundConfig{
			Enabled:  true,
			Interval: "1s",
		},
		lastInteraction: time.Now().Add(-2 * time.Second),
		hasActiveWorkFn: func() bool { return activeWork },
		branchFn: func(branchType, promptText string, noCompact bool) {
			mu.Lock()
			calls++
			mu.Unlock()
		},
		done: make(chan struct{}),
	}

	// Should be blocked by active work
	r.maybeBackgroundWork(context.Background())
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 0 {
		t.Errorf("expected 0 background calls while active work, got %d", got)
	}

	// Clear active work — should now fire
	activeWork = false
	r.maybeBackgroundWork(context.Background())
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got = calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("expected 1 background call after active work cleared, got %d", got)
	}
}

func TestMaybeBackgroundWork_WithBadInvestInterval(t *testing.T) {
	// Test the code path where InvestInterval parsing fails and falls back to 30m
	var calls int
	r := &Runner{
		log:                log.NewComponentLogger("keepalive:test"),
		agentID:            "test",
		manaInvestInterval: "invalid",
		bgCfg: config.BackgroundConfig{
			Enabled:  true,
			Interval: "1s",
		},
		lastInteraction: time.Now().Add(-2 * time.Second),
		branchFn: func(branchType, promptText string, noCompact bool) {
			calls++
		},
		done: make(chan struct{}),
	}

	r.maybeBackgroundWork(context.Background())
	time.Sleep(50 * time.Millisecond)

	// Even with bad InvestInterval, it should attempt background work
	// (it just falls back to 30m for mana check)
}
