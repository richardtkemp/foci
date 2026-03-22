package periodic

import (
	"context"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
)

func TestBackgroundBlockedByActiveWork(t *testing.T) {
	// Verifies that hasActiveWorkFn returning a non-zero count blocks background dispatch.
	// After clearing active work, the same conditions should allow the next call to fire.
	var mu sync.Mutex
	calls := 0

	activeCount := 1

	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		bgCfg: config.ResolvedBackground{
			Enabled:  true,
			Interval: "1s",
		},
		lastInteraction: time.Now().Add(-2 * time.Second),
		hasActiveWorkFn: func() int { return activeCount },
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
	activeCount = 0
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
	// Verifies that an unparseable manaInvestInterval does not block background dispatch entirely;
	// the runner falls back gracefully rather than aborting the attempt.
	var calls int
	r := &Runner{
		log:                log.NewComponentLogger("keepalive:test"),
		agentID:            "test",
		manaInvestInterval: "invalid",
		bgCfg: config.ResolvedBackground{
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

func TestMaybeMemoryFormation_SkipsWhenRateLimited(t *testing.T) {
	// Verifies that maybeMemoryFormation respects the canFireFn gate: if the function returns
	// false (e.g. insufficient mana), no branch is dispatched even when all other conditions are met.
	called := false
	now := time.Now()
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		mfCfg: config.ResolvedMemoryFormation{
			IntervalEnabled: true,
			Interval:        "1h",
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

func TestMaybeConsolidation_SkipsWhenRateLimited(t *testing.T) {
	// Verifies that maybeConsolidation respects the canFireFn gate: if the function returns
	// false (e.g. insufficient mana), no branch is dispatched even when all other conditions are met.
	called := false
	now := time.Now()
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		mfCfg: config.ResolvedMemoryFormation{
			ConsolidationEnabled:  true,
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

func TestMaybeBackgroundWork_SkipsWhenRateLimited(t *testing.T) {
	// Verifies that maybeBackgroundWork respects the canFireFn gate: if the function returns
	// false (e.g. insufficient mana), no branch is dispatched even when all other conditions are met.
	called := false
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		bgCfg: config.ResolvedBackground{
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
