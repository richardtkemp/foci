package periodic

import (
	"context"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/state"
)

func TestMaybeKeepalive_Disabled(t *testing.T) {
	// Verifies that maybeKeepalive is a no-op when the keepalive feature is disabled in config.
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
	// Verifies that maybeKeepalive skips dispatch when the configured interval string is unparseable.
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
	// Verifies that maybeKeepalive skips dispatch when the cache was warmed recently and the
	// interval has not yet elapsed.
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
	// Verifies that maybeKeepalive dispatches a branch when enabled, the cache is stale, and
	// no keepalive is already running.
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
	// Verifies that maybeKeepalive is a no-op when keepaliveRunning is already true, preventing
	// concurrent keepalive sessions.
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
	// Verifies that maybeBackgroundWork is a no-op when the background feature is disabled in config.
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
	// Verifies that maybeBackgroundWork skips dispatch when the configured interval string cannot
	// be parsed.
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
	// Verifies that maybeBackgroundWork skips dispatch when a user interaction occurred recently
	// and the idle interval has not elapsed.
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
	// Verifies that maybeMemoryFormation is a no-op when IntervalEnabled is explicitly set to false.
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
	// Verifies that maybeMemoryFormation skips dispatch when the configured interval string is
	// unparseable.
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

func TestMaybeMemoryFormation_Fires(t *testing.T) {
	// Verifies that maybeMemoryFormation dispatches a "memory-formation" branch when enabled,
	// there has been recent activity, and the interval since the last formation has elapsed.
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

func TestMaybeConsolidation_Disabled(t *testing.T) {
	// Verifies that maybeConsolidation is a no-op when ConsolidationEnabled is explicitly false.
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
	// Verifies that maybeConsolidation skips dispatch when the configured consolidation interval
	// string cannot be parsed.
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

func TestMaybeConsolidation_Fires(t *testing.T) {
	// Verifies that maybeConsolidation dispatches a "consolidation" branch when enabled, there
	// has been recent activity, and the consolidation interval has elapsed.
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

func TestMaybeMemoryFormation_NoActivity(t *testing.T) {
	// Verifies that maybeMemoryFormation requires recent interaction activity; if the last
	// interaction is also older than the interval, formation is skipped.
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
	// Verifies that maybeMemoryFormation is a no-op when memoryFormationRunning is true, preventing
	// concurrent formation sessions.
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

func TestMaybeConsolidation_TooMuchInactivity(t *testing.T) {
	// Verifies that maybeConsolidation skips dispatch when the last interaction was too long ago,
	// meaning there is no meaningful recent activity to consolidate.
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
	// Verifies that maybeConsolidation is a no-op when consolidationRunning is true, preventing
	// concurrent consolidation sessions.
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

func TestNew(t *testing.T) {
	// Verifies that New correctly initialises a Runner from RunnerConfig, wiring agentID, feature
	// configs, and the done channel.
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
	// Verifies that New loads lastConsolidation from a persistent state store on startup,
	// so that consolidation timing survives process restarts.
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
	// Verifies that NotifyCacheWarmed advances lastCacheWarmed to a time after the previous value.
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
	// Verifies that NotifyInteraction advances lastInteraction to a time after the previous value.
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

func TestStartStop(t *testing.T) {
	// Verifies that Start launches the run loop and Stop shuts it down cleanly without deadlock.
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
	// Verifies that calling Stop on a Runner that was never Started does not panic.
	r := New(RunnerConfig{
		AgentID:    "test",
		BranchFunc: func(branchType, promptText string, noCompact bool) {},
	})

	r.Stop() // Should not panic
}

func TestRun_ContextCancellation(t *testing.T) {
	// Verifies that the run loop exits and closes the done channel within a reasonable timeout
	// after the context is cancelled.
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
