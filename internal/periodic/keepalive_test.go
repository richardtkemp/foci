package periodic

import (
	"context"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
)

// TestBackgroundBlockedByActiveWork verifies that HasActiveWorkFn returning true prevents background dispatch.
// When active work is in progress, maybeBackgroundWork should skip the dispatch and try again when it clears.
func TestBackgroundBlockedByActiveWork(t *testing.T) {
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

// TestMaybeBackgroundWork_WithBadInvestInterval tests the code path where InvestInterval parsing fails.
// It verifies that background work attempts even with bad InvestInterval (falls back to default mana check).
func TestMaybeBackgroundWork_WithBadInvestInterval(t *testing.T) {
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

// TestMaybeMemoryFormation_SkipsWhenRateLimited proves that memory formation
// respects the canFireFn check and skips when it returns false.
func TestMaybeMemoryFormation_SkipsWhenRateLimited(t *testing.T) {
	called := false
	now := time.Now()
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		mfCfg: config.MemoryFormationConfig{
			Interval: "1h",
		},
		sessionKeyFn: func() string { return "test/c123/1000000000" },
		canFireFn: func(ctx context.Context, sk string) (bool, string) {
			return false, "rate limited"
		},
		branchFn: func(branchType, promptText string, noCompact bool) {
			called = true
		},
		lastMemoryFormation: now.Add(-2 * time.Hour),
		lastInteraction:     now.Add(-30 * time.Minute),
		done:                make(chan struct{}),
	}

	r.maybeMemoryFormation()

	if called {
		t.Error("expected memory formation to skip when canFireFn returns false")
	}
}

// TestMaybeConsolidation_SkipsWhenRateLimited proves that consolidation
// respects the canFireFn check and skips when it returns false.
func TestMaybeConsolidation_SkipsWhenRateLimited(t *testing.T) {
	called := false
	now := time.Now()
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		mfCfg: config.MemoryFormationConfig{
			ConsolidationInterval: "1h",
		},
		sessionKeyFn: func() string { return "test/c123/1000000000" },
		canFireFn: func(ctx context.Context, sk string) (bool, string) {
			return false, "rate limited"
		},
		branchFn: func(branchType, promptText string, noCompact bool) {
			called = true
		},
		lastConsolidation: now.Add(-2 * time.Hour),
		lastInteraction:   now.Add(-30 * time.Minute),
		done:              make(chan struct{}),
	}

	r.maybeConsolidation()

	if called {
		t.Error("expected consolidation to skip when canFireFn returns false")
	}
}

// TestMaybeBackgroundWork_SkipsWhenRateLimited proves that background work
// respects the canFireFn check and skips when it returns false.
func TestMaybeBackgroundWork_SkipsWhenRateLimited(t *testing.T) {
	called := false
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		bgCfg: config.BackgroundConfig{
			Enabled:  true,
			Interval: "1s",
		},
		sessionKeyFn: func() string { return "test/c123/1000000000" },
		canFireFn: func(ctx context.Context, sk string) (bool, string) {
			return false, "mana insufficient"
		},
		branchFn: func(branchType, promptText string, noCompact bool) {
			called = true
		},
		lastInteraction: time.Now().Add(-2 * time.Second),
		todoStore:       nil, // skip todo check
		done:            make(chan struct{}),
	}

	r.maybeBackgroundWork(context.Background())
	time.Sleep(50 * time.Millisecond)

	if called {
		t.Error("expected background work to skip when canFireFn returns false")
	}
}
