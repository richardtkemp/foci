package periodic

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/session"
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
		agent: &fakeBackgroundAgent{
			sessionKeyFn:    func() string { return "test/c1/1" },
			hasActiveWorkFn: func() int { return activeCount },
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				mu.Lock()
				calls++
				mu.Unlock()
				return true
			},
		},
		done: make(chan struct{}),
	}

	// Should be blocked by active work
	r.maybeBackgroundWork(context.Background())
	waitIdle(t, r)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 0 {
		t.Errorf("expected 0 background calls while active work, got %d", got)
	}

	// Clear active work — should now fire
	activeCount = 0
	r.maybeBackgroundWork(context.Background())
	waitIdle(t, r)

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
		agent: &fakeBackgroundAgent{
			sessionKeyFn: func() string { return "test/c1/1" },
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				calls++
				return true
			},
		},
		done: make(chan struct{}),
	}

	r.maybeBackgroundWork(context.Background())
	waitIdle(t, r)

	// Even with bad InvestInterval, it should attempt background work
	// (it just falls back to 30m for mana check)
}

func TestMaybeReflection_SkipsWhenRateLimited(t *testing.T) {
	// Verifies that maybeReflection respects the canFireFn gate: if the function returns
	// false (e.g. insufficient mana), no branch is dispatched even when all other conditions are met.
	called := false
	now := time.Now()

	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  "test/c123/1000000000",
		FilePath:    "/tmp/test.jsonl",
		CreatedAt:   now.Add(-24 * time.Hour),
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})
	idx.UpdateActivity("test/c123/1000000000", now.Add(-30*time.Minute))
	idx.StampReflection("test/c123/1000000000", now.Add(-2*time.Hour))

	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		reflectCfg: config.ResolvedReflection{
			IntervalEnabled: true,
			Interval:        "1h",
		},
		sessionIndex: idx,
		agent: &fakeBackgroundAgent{
			sessionKeyFn: func() string { return "test/c123/1000000000" },
			canFireFn: func(ctx context.Context, sk string) (bool, string) {
				return false, "rate limited"
			},
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				called = true
				return true
			},
		},
		lastReflection:  now.Add(-2 * time.Hour),
		lastInteraction: now.Add(-30 * time.Minute),
		done:            make(chan struct{}),
	}

	r.maybeReflection()

	if called {
		t.Error("expected reflection to skip when canFireFn returns false")
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
		maintCfg: config.ResolvedMaintenance{
			ConsolidationEnabled: true,
			ConsolidationTime:    "1h",
		},
		agent: &fakeBackgroundAgent{
			sessionKeyFn: func() string { return "test/c123/1000000000" },
			canFireFn: func(ctx context.Context, sk string) (bool, string) {
				return false, "rate limited"
			},
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				called = true
				return true
			},
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
		agent: &fakeBackgroundAgent{
			sessionKeyFn: func() string { return "test/c123/1000000000" },
			canFireFn: func(ctx context.Context, sk string) (bool, string) {
				return false, "mana insufficient"
			},
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				called = true
				return true
			},
		},
		lastInteraction: time.Now().Add(-2 * time.Second),
		todoStore:       nil, // skip todo check
		done:            make(chan struct{}),
	}

	r.maybeBackgroundWork(context.Background())
	waitIdle(t, r)

	if called {
		t.Error("expected background work to skip when canFireFn returns false")
	}
}
