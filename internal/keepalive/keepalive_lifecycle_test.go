package keepalive

import (
	"context"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/state"
)

// TestMaybeKeepalive_Disabled verifies keepalive doesn't fire when disabled.
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

// TestMaybeKeepalive_BadInterval verifies keepalive doesn't fire with invalid interval.
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

// TestMaybeKeepalive_RecentCache verifies keepalive doesn't fire with recent cache.
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

// TestMaybeKeepalive_Fires verifies keepalive fires when enabled and cache is stale.
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

// TestMaybeKeepalive_AlreadyRunning verifies keepalive doesn't fire while already running.
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

// TestMaybeBackgroundWork_Disabled verifies background work doesn't fire when disabled.
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

// TestMaybeBackgroundWork_BadInterval verifies background work doesn't fire with invalid interval.
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

// TestMaybeBackgroundWork_RecentInteraction verifies background work doesn't fire with recent interaction.
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

// TestMaybeMemoryFormation_Disabled verifies memory formation doesn't fire when disabled.
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

// TestMaybeMemoryFormation_BadInterval verifies memory formation doesn't fire with invalid interval.
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

// TestMaybeMemoryFormation_Fires verifies memory formation fires when enabled and interval elapsed.
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

// TestMaybeConsolidation_Disabled verifies consolidation doesn't fire when disabled.
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

// TestMaybeConsolidation_BadInterval verifies consolidation doesn't fire with invalid interval.
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

// TestMaybeConsolidation_Fires verifies consolidation fires when enabled and interval elapsed.
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

// TestMaybeMemoryFormation_NoActivity verifies memory formation doesn't fire without recent activity.
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

// TestMaybeMemoryFormation_AlreadyRunning verifies memory formation doesn't fire while already running.
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

// TestMaybeConsolidation_TooMuchInactivity verifies consolidation doesn't fire with too much inactivity.
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

// TestMaybeConsolidation_AlreadyRunning verifies consolidation doesn't fire while already running.
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

// TestNew verifies basic Runner initialization with config.
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

// TestNew_WithStateStore verifies Runner initializes lastConsolidation from state store.
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

// TestNotifyCacheWarmed verifies NotifyCacheWarmed updates lastCacheWarmed.
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

// TestNotifyInteraction verifies NotifyInteraction updates lastInteraction.
func TestNotifyInteraction(t *testing.T) {
	r := &Runner{
		log:             log.NewComponentLogger("keepalive:test"),
		lastInteraction: time.Now().Add(-10 * time.Second),
		done:            make(chan struct{}),
	}

	before := r.lastInteraction
	time.Sleep(10 * time.Millisecond)
	r.NotifyInteraction()
	after := r.lastInteraction

	if !after.After(before) {
		t.Errorf("lastInteraction not updated")
	}
}

// TestStartStop verifies Start and Stop complete without deadlock.
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

// TestStop_WithoutStart verifies Stop doesn't panic when Start was not called.
func TestStop_WithoutStart(t *testing.T) {
	r := New(RunnerConfig{
		AgentID:    "test",
		BranchFunc: func(branchType, promptText string, noCompact bool) {},
	})

	r.Stop() // Should not panic
}

// TestRun_ContextCancellation verifies run loop exits cleanly when context is cancelled.
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
