package periodic

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/session"
)

// indexWithTouch returns a SessionIndex holding one session `sk` whose
// last_cache_touch is `touchAgo` before now. Backs the keepalive window-gate
// tests (keepaliveTargets reads r.sessionIndex.LastCacheTouch). Closed via t.Cleanup.
func indexWithTouch(t *testing.T, sk string, touchAgo time.Duration) *session.SessionIndex {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  sk,
		FilePath:    "/tmp/test.jsonl",
		CreatedAt:   time.Now().Add(-24 * time.Hour),
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})
	idx.TouchCacheTouch(sk, time.Now().Add(-touchAgo))
	return idx
}

func TestMaybeKeepalive_Disabled(t *testing.T) {
	// Verifies that maybeKeepalive is a no-op when the keepalive feature is disabled in config.
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		kaCfg: config.ResolvedKeepalive{
			Enabled: false,
		},
		agent: &fakeBackgroundAgent{
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				calls++
				return true
			},
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
		kaCfg: config.ResolvedKeepalive{
			Enabled:  true,
			Interval: "invalid",
		},
		agent: &fakeBackgroundAgent{
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				calls++
				return true
			},
		},
		done: make(chan struct{}),
	}

	r.maybeKeepalive(context.Background())
	if calls != 0 {
		t.Errorf("keepalive called with bad interval")
	}
}

func TestMaybeKeepalive_RecentCache(t *testing.T) {
	// Skips dispatch when the session's cache was touched within the interval
	// (touched 10m ago, interval 1h → not due yet).
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		kaCfg: config.ResolvedKeepalive{
			Enabled:  true,
			Interval: "1h",
		},
		sessionIndex: indexWithTouch(t, "test/c1", 10*time.Minute),
		agent: &fakeBackgroundAgent{
			sessionKeyFn: func() string { return "test/c1" },
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				calls++
				return true
			},
		},
		done: make(chan struct{}),
	}

	r.maybeKeepalive(context.Background())
	waitIdle(t, r)
	if calls != 0 {
		t.Errorf("keepalive called with recent cache")
	}
}

func TestMaybeKeepalive_Fires(t *testing.T) {
	// Dispatches when the session is in the warm window: touched 10m ago, due
	// (interval 1m), and not yet expired (fake CacheExpiry = touch+1h).
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		kaCfg: config.ResolvedKeepalive{
			Enabled:  true,
			Interval: "1m",
			Prompt:   "keepalive.md",
		},
		cacheTTL:     time.Hour,
		sessionIndex: indexWithTouch(t, "test/c1", 10*time.Minute),
		agent: &fakeBackgroundAgent{
			sessionKeyFn: func() string { return "test/c1" },
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				calls++
				return true
			},
		},
		done: make(chan struct{}),
	}

	r.maybeKeepalive(context.Background())
	waitIdle(t, r)

	if calls != 1 {
		t.Errorf("keepalive not called, expected 1 call")
	}
}

func TestMaybeKeepalive_NeverWarmed(t *testing.T) {
	// A session with no recorded cache-touch (never warmed / just reset) is
	// skipped — there is no live cache to keep alive.
	var calls int
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	idx.Upsert(session.SessionIndexEntry{
		SessionKey: "test/c1", FilePath: "/tmp/t.jsonl", CreatedAt: time.Now(),
		SessionType: session.SessionTypeChat, Status: session.SessionStatusActive,
	}) // no TouchCacheTouch → last_cache_touch NULL

	r := &Runner{
		log:          log.NewComponentLogger("keepalive:test"),
		agentID:      "test",
		kaCfg:        config.ResolvedKeepalive{Enabled: true, Interval: "1m"},
		sessionIndex: idx,
		agent: &fakeBackgroundAgent{
			sessionKeyFn: func() string { return "test/c1" },
			branchFn:     func(_, _, _ string, _ bool) bool { calls++; return true },
		},
		done: make(chan struct{}),
	}

	r.maybeKeepalive(context.Background())
	waitIdle(t, r)
	if calls != 0 {
		t.Errorf("keepalive warmed a never-touched session, want skip")
	}
}

func TestMaybeKeepalive_ExpiredCache(t *testing.T) {
	// A session touched beyond its cache TTL is skipped — warming a dead cache
	// pays full write cost to establish a fresh one, which keepalive must not do.
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		kaCfg:   config.ResolvedKeepalive{Enabled: true, Interval: "1m"},
		// Touched 90m ago with a 1h TTL → expired 30m ago.
		cacheTTL:     time.Hour,
		sessionIndex: indexWithTouch(t, "test/c1", 90*time.Minute),
		agent: &fakeBackgroundAgent{
			sessionKeyFn: func() string { return "test/c1" },
			branchFn:     func(_, _, _ string, _ bool) bool { calls++; return true },
		},
		done: make(chan struct{}),
	}

	r.maybeKeepalive(context.Background())
	waitIdle(t, r)
	if calls != 0 {
		t.Errorf("keepalive warmed an expired-cache session, want skip")
	}
}

func TestMaybeKeepalive_AlreadyRunning(t *testing.T) {
	// Verifies that maybeKeepalive is a no-op when keepaliveRunning is already true, preventing
	// concurrent keepalive sessions.
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		kaCfg: config.ResolvedKeepalive{
			Enabled:  true,
			Interval: "1m",
		},
		keepaliveRunning: true,
		agent: &fakeBackgroundAgent{
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				calls++
				return true
			},
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
		bgCfg: config.ResolvedBackground{
			Enabled: false,
		},
		lastInteraction: time.Now().Add(-10 * time.Minute),
		agent: &fakeBackgroundAgent{
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				calls++
				return true
			},
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
		bgCfg: config.ResolvedBackground{
			Enabled:  true,
			Interval: "invalid",
		},
		lastInteraction: time.Now().Add(-10 * time.Minute),
		agent: &fakeBackgroundAgent{
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				calls++
				return true
			},
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
		bgCfg: config.ResolvedBackground{
			Enabled:  true,
			Interval: "1h",
		},
		lastInteraction: time.Now().Add(-10 * time.Minute),
		agent: &fakeBackgroundAgent{
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				calls++
				return true
			},
		},
		done: make(chan struct{}),
	}

	r.maybeBackgroundWork(context.Background())
	if calls != 0 {
		t.Errorf("background work called with recent interaction")
	}
}

func TestMaybeReflection_Disabled(t *testing.T) {
	// Verifies that maybeReflection is a no-op when IntervalEnabled is explicitly set to false.
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		reflectCfg: config.ResolvedReflection{
			IntervalEnabled: false,
		},
		lastInteraction: time.Now().Add(-1 * time.Hour),
		lastReflection:  time.Now().Add(-2 * time.Hour),
		agent: &fakeBackgroundAgent{
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				calls++
				return true
			},
		},
		done: make(chan struct{}),
	}

	r.maybeReflection()
	if calls != 0 {
		t.Errorf("memory formation called when disabled")
	}
}

func TestMaybeReflection_BadInterval(t *testing.T) {
	// Verifies that maybeReflection skips dispatch when the configured interval string is
	// unparseable.
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		reflectCfg: config.ResolvedReflection{
			IntervalEnabled: true,
			Interval:        "invalid",
		},
		lastInteraction: time.Now().Add(-1 * time.Hour),
		lastReflection:  time.Now().Add(-2 * time.Hour),
		agent: &fakeBackgroundAgent{
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				calls++
				return true
			},
		},
		done: make(chan struct{}),
	}

	r.maybeReflection()
	if calls != 0 {
		t.Errorf("memory formation called with bad interval")
	}
}

func TestMaybeReflection_Fires(t *testing.T) {
	// Verifies that maybeReflection dispatches a "reflection" branch when enabled,
	// there has been recent activity, and the session has activity newer than its last formation.
	var calls int
	var gotParentKey string
	now := time.Now()

	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	// Insert an active chat session with activity newer than its formation stamp.
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  "test/c1/1",
		FilePath:    "/tmp/test.jsonl",
		CreatedAt:   now.Add(-24 * time.Hour),
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})
	idx.UpdateActivity("test/c1/1", now.Add(-30*time.Minute))
	idx.StampReflection("test/c1/1", now.Add(-2*time.Hour))

	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		reflectCfg: config.ResolvedReflection{
			IntervalEnabled: true,
			Interval:        "1h",
			IntervalPrompt:  "reflection.md",
		},
		sessionIndex:    idx,
		lastInteraction: now.Add(-30 * time.Minute),
		lastReflection:  now.Add(-2 * time.Hour),
		agent: &fakeBackgroundAgent{
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				if branchType != "reflection" {
					t.Errorf("expected branch type 'reflection', got %q", branchType)
				}
				gotParentKey = parentKey
				calls++
				return true
			},
		},
		done: make(chan struct{}),
	}

	r.maybeReflection()
	waitIdle(t, r)

	if calls != 1 {
		t.Errorf("memory formation not called, expected 1 call, got %d", calls)
	}
	if gotParentKey != "test/c1/1" {
		t.Errorf("parent key = %q, want test/c1/1", gotParentKey)
	}
}

func TestMaybeConsolidation_Disabled(t *testing.T) {
	// Verifies that maybeConsolidation is a no-op when ConsolidationEnabled is explicitly false.
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		maintCfg: config.ResolvedMaintenance{
			ConsolidationEnabled: false,
		},
		lastInteraction:   time.Now(),
		lastConsolidation: time.Now().Add(-2 * time.Hour),
		agent: &fakeBackgroundAgent{
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				calls++
				return true
			},
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
		maintCfg: config.ResolvedMaintenance{
			ConsolidationEnabled: true,
			ConsolidationTime:    "invalid",
		},
		lastInteraction:   time.Now(),
		lastConsolidation: time.Now().Add(-2 * time.Hour),
		agent: &fakeBackgroundAgent{
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				calls++
				return true
			},
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
		maintCfg: config.ResolvedMaintenance{
			ConsolidationEnabled: true,
			ConsolidationTime:    "1h",
			ConsolidationPrompt:  "memory-consolidation.md",
		},
		lastInteraction:   now.Add(-30 * time.Minute),
		lastConsolidation: now.Add(-2 * time.Hour),
		agent: &fakeBackgroundAgent{
			sessionKeyFn: func() string { return "test/c1/1" },
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				if branchType != "consolidation" {
					t.Errorf("expected branch type 'consolidation', got %q", branchType)
				}
				calls++
				return true
			},
		},
		done: make(chan struct{}),
	}

	r.maybeConsolidation()
	waitIdle(t, r)

	if calls != 1 {
		t.Errorf("consolidation not called, expected 1 call, got %d", calls)
	}
}

func TestMaybeConsolidation_UsesRunOnce(t *testing.T) {
	// Verifies that when runOnceFn is set, consolidation uses it instead of branchFn.
	var branchCalls int
	var runOnceCalls int
	var gotPrompt string
	now := time.Now()
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		maintCfg: config.ResolvedMaintenance{
			ConsolidationEnabled: true,
			ConsolidationTime:    "1h",
			ConsolidationPrompt:  "memory-consolidation.md",
		},
		isDelegatedAgent:  true,
		lastInteraction:   now.Add(-30 * time.Minute),
		lastConsolidation: now.Add(-2 * time.Hour),
		agent: &fakeBackgroundAgent{
			sessionKeyFn: func() string { return "test/c1/1" },
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				branchCalls++
				return true
			},
			runOnceFn: func(_ context.Context, prompt, systemPrompt string) (string, error) {
				runOnceCalls++
				gotPrompt = prompt
				return "done", nil
			},
		},
		done: make(chan struct{}),
	}

	r.maybeConsolidation()
	waitIdle(t, r)

	if branchCalls != 0 {
		t.Errorf("branchFn should not be called when runOnceFn is set, got %d calls", branchCalls)
	}
	if runOnceCalls != 1 {
		t.Errorf("expected 1 runOnceFn call, got %d", runOnceCalls)
	}
	if gotPrompt == "" {
		t.Error("expected non-empty prompt")
	}
}

func TestMaybeReflection_NoActivity(t *testing.T) {
	// Verifies that maybeReflection requires recent interaction activity; if the last
	// interaction is also older than the interval, formation is skipped.
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		reflectCfg: config.ResolvedReflection{
			IntervalEnabled: true,
			Interval:        "1h",
		},
		lastInteraction: time.Now().Add(-2 * time.Hour),
		lastReflection:  time.Now().Add(-2 * time.Hour),
		agent: &fakeBackgroundAgent{
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				calls++
				return true
			},
		},
		done: make(chan struct{}),
	}

	r.maybeReflection()
	if calls != 0 {
		t.Errorf("memory formation called with no activity")
	}
}

func TestMaybeReflection_AlreadyRunning(t *testing.T) {
	// Verifies that maybeReflection is a no-op when reflectionRunning is true, preventing
	// concurrent formation sessions.
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		reflectCfg: config.ResolvedReflection{
			IntervalEnabled: true,
			Interval:        "1h",
		},
		lastInteraction:   time.Now().Add(-30 * time.Minute),
		lastReflection:    time.Now().Add(-2 * time.Hour),
		reflectionRunning: true,
		agent: &fakeBackgroundAgent{
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				calls++
				return true
			},
		},
		done: make(chan struct{}),
	}

	r.maybeReflection()
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
		maintCfg: config.ResolvedMaintenance{
			ConsolidationEnabled: true,
			ConsolidationTime:    "1h",
		},
		lastInteraction:   time.Now().Add(-3 * time.Hour),
		lastConsolidation: time.Now().Add(-2 * time.Hour),
		agent: &fakeBackgroundAgent{
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				calls++
				return true
			},
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
		maintCfg: config.ResolvedMaintenance{
			ConsolidationEnabled: true,
			ConsolidationTime:    "1h",
		},
		lastInteraction:      time.Now().Add(-30 * time.Minute),
		lastConsolidation:    time.Now().Add(-2 * time.Hour),
		consolidationRunning: true,
		agent: &fakeBackgroundAgent{
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				calls++
				return true
			},
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
		Keepalive: config.ResolvedKeepalive{
			Enabled: true,
		},
		Background: config.ResolvedBackground{
			Enabled: true,
		},
		Reflection: config.ResolvedReflection{
			IntervalEnabled: true,
			Interval:        "1h",
		},
		Agent: &fakeBackgroundAgent{branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool { return true }},
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

func TestNew_WithSessionIndex(t *testing.T) {
	// Verifies that New loads lastConsolidation from the session index on startup,
	// so that consolidation timing survives process restarts.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	consolidationTime := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	if err := idx.SetAgentMetadata("test-agent", "consolidation_last", consolidationTime.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	cfg := RunnerConfig{
		AgentID:      "test-agent",
		SessionIndex: idx,
		Agent:        &fakeBackgroundAgent{branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool { return true }},
	}

	r := New(cfg)

	r.mu.Lock()
	if !r.lastConsolidation.Equal(consolidationTime) {
		t.Errorf("lastConsolidation = %v, want %v", r.lastConsolidation, consolidationTime)
	}
	r.mu.Unlock()
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
		Keepalive: config.ResolvedKeepalive{
			Enabled: false,
		},
		Background: config.ResolvedBackground{
			Enabled: false,
		},
		Reflection: config.ResolvedReflection{
			IntervalEnabled: true,
			Interval:        "1h",
		},
		Agent: &fakeBackgroundAgent{branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool { return true }},
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
		AgentID: "test",
		Agent:   &fakeBackgroundAgent{branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool { return true }},
	})

	r.Stop() // Should not panic
}

func TestRun_ContextCancellation(t *testing.T) {
	// Verifies that the run loop exits and closes the done channel within a reasonable timeout
	// after the context is cancelled.
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		kaCfg: config.ResolvedKeepalive{
			Enabled: false,
		},
		bgCfg: config.ResolvedBackground{
			Enabled: false,
		},
		reflectCfg: config.ResolvedReflection{
			IntervalEnabled: true,
			Interval:        "1h",
		},
		agent: &fakeBackgroundAgent{branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool { return true }},
		done:  make(chan struct{}),
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
