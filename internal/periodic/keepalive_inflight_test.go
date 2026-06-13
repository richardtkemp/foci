// Tests for the in-flight gate on internal periodic schedulers (TODO #760).
//
// The gateway send/branch path checks Agent.IsTurnInFlight (TODO #753) before
// dispatching cron-fired prompts. The four internal periodic schedulers
// (maybeKeepalive, maybeBackgroundWork, maybeReflection, maybeConsolidation)
// dispatch via branchFn directly, bypassing that gate. Without their own
// in-flight check, a periodic prompt fired while a turn is in flight gets
// queued behind it as a SourceUser follow-up — wrong source, wrong timing.
//
// These tests verify each scheduler honours the IsTurnInFlightFunc callback:
// when the parent session reports a turn in flight, the scheduler skips and
// no branchFn dispatch happens. When IsTurnInFlightFunc is nil (test runners
// that don't wire it), the schedulers fire as before.
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

// waitIdle blocks until every periodic scheduler goroutine started by the
// maybe* methods has finished (all *Running flags clear). Each goroutine clears
// its flag under r.mu only after its branchFn callbacks return, so observing the
// cleared flags under r.mu establishes a happens-before edge: any state those
// callbacks wrote is then safe to read from the test goroutine. This replaces
// the previous fixed time.Sleep, which both raced (no happens-before) and was
// timing-fragile.
func waitIdle(t *testing.T, r *Runner) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		busy := r.keepaliveRunning || r.backgroundRunning || r.reflectionRunning || r.consolidationRunning
		r.mu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("periodic scheduler goroutine did not finish within 2s")
		}
		time.Sleep(time.Millisecond)
	}
}

// inFlightFn returns a stub IsTurnInFlightFunc that reports the given base
// keys as in-flight. Useful for asserting selective filtering in maybeReflection.
func inFlightFn(t *testing.T, busyBases ...string) func(string) bool {
	t.Helper()
	busy := make(map[string]bool, len(busyBases))
	for _, b := range busyBases {
		busy[b] = true
	}
	return func(base string) bool {
		return busy[base]
	}
}

func TestMaybeKeepalive_SkipsWhenTurnInFlight(t *testing.T) {
	// When IsTurnInFlightFunc reports the parent base as in-flight,
	// maybeKeepalive must not dispatch a branch.
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		kaCfg: config.ResolvedKeepalive{
			Enabled:  true,
			Interval: "1m",
			Prompt:   "keepalive.md",
		},
		lastCacheWarmed:  time.Now().Add(-10 * time.Minute),
		sessionKeyFn:     func() string { return "test/c1/1" },
		isTurnInFlightFn: inFlightFn(t, "test/c1"),
		branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
			calls++
			return true
		},
		done: make(chan struct{}),
	}

	r.maybeKeepalive(context.Background())
	waitIdle(t, r)

	if calls != 0 {
		t.Errorf("keepalive fired %d times while turn in flight, want 0", calls)
	}
}

func TestMaybeBackgroundWork_SkipsWhenTurnInFlight(t *testing.T) {
	// maybeBackgroundWork must skip branchFn dispatch when the parent base
	// has a turn in flight, even if all other gates would pass.
	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		bgCfg: config.ResolvedBackground{
			Enabled:  true,
			Interval: "1m",
			Prompt:   "background.md",
		},
		lastInteraction:  time.Now().Add(-10 * time.Minute),
		sessionKeyFn:     func() string { return "test/c1/1" },
		isTurnInFlightFn: inFlightFn(t, "test/c1"),
		// hasActiveWorkFn=nil and todoStore=nil means the open-todo gate is skipped,
		// so the only thing protecting us from a fire is the in-flight gate.
		branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
			calls++
			return true
		},
		done: make(chan struct{}),
	}

	r.maybeBackgroundWork(context.Background())
	waitIdle(t, r)

	if calls != 0 {
		t.Errorf("background fired %d times while turn in flight, want 0", calls)
	}
}

func TestMaybeReflection_FiltersInFlightSessions(t *testing.T) {
	// SessionsNeedingReflection returns multiple keys. maybeReflection must
	// filter out sessions whose base has a turn in flight, but still fire on
	// the remaining ones. Skips the whole pass when all candidates are busy.
	now := time.Now()

	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Three sessions, all needing reflection (activity newer than reflection stamp).
	for _, key := range []string{"test/c1/1", "test/c2/1", "test/c3/1"} {
		idx.Upsert(session.SessionIndexEntry{
			SessionKey:  key,
			FilePath:    "/tmp/test.jsonl",
			CreatedAt:   now.Add(-24 * time.Hour),
			SessionType: session.SessionTypeChat,
			Status:      session.SessionStatusActive,
		})
		idx.UpdateActivity(key, now.Add(-30*time.Minute))
		idx.StampReflection(key, now.Add(-2*time.Hour))
	}

	got := map[string]int{}
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
		// c1 and c3 are busy; c2 is free.
		isTurnInFlightFn: inFlightFn(t, "test/c1", "test/c3"),
		branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
			got[parentKey]++
			return true
		},
		done: make(chan struct{}),
	}

	r.maybeReflection()
	waitIdle(t, r)

	if got["test/c1/1"] != 0 {
		t.Errorf("c1 fired despite in-flight turn (got %d calls)", got["test/c1/1"])
	}
	if got["test/c3/1"] != 0 {
		t.Errorf("c3 fired despite in-flight turn (got %d calls)", got["test/c3/1"])
	}
	if got["test/c2/1"] != 1 {
		t.Errorf("c2 fired %d times, want 1 (it's free)", got["test/c2/1"])
	}
}

func TestMaybeReflection_SkipsWhenAllSessionsBusy(t *testing.T) {
	// If every candidate session has a turn in flight, the whole reflection
	// pass is skipped — reflectionRunning never goes true so the next tick
	// can re-evaluate.
	now := time.Now()

	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  "test/c1/1",
		FilePath:    "/tmp/test.jsonl",
		CreatedAt:   now.Add(-24 * time.Hour),
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})
	idx.UpdateActivity("test/c1/1", now.Add(-30*time.Minute))
	idx.StampReflection("test/c1/1", now.Add(-2*time.Hour))

	var calls int
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		reflectCfg: config.ResolvedReflection{
			IntervalEnabled: true,
			Interval:        "1h",
			IntervalPrompt:  "reflection.md",
		},
		sessionIndex:     idx,
		lastInteraction:  now.Add(-30 * time.Minute),
		lastReflection:   now.Add(-2 * time.Hour),
		isTurnInFlightFn: inFlightFn(t, "test/c1"),
		branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
			calls++
			return true
		},
		done: make(chan struct{}),
	}

	r.maybeReflection()
	waitIdle(t, r)

	if calls != 0 {
		t.Errorf("reflection fired %d times when all sessions busy, want 0", calls)
	}
	r.mu.Lock()
	running := r.reflectionRunning
	r.mu.Unlock()
	if running {
		t.Error("reflectionRunning should be false after all-busy skip")
	}
}

func TestMaybeConsolidation_SkipsWhenTurnInFlight(t *testing.T) {
	// maybeConsolidation must skip dispatch when the parent base has a turn
	// in flight, regardless of interval / activity gates.
	var calls int
	now := time.Now()
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		reflectCfg: config.ResolvedReflection{
			ConsolidationEnabled:  true,
			ConsolidationInterval: "1h",
			ConsolidationPrompt:   "memory-consolidation.md",
		},
		lastInteraction:   now.Add(-30 * time.Minute),
		lastConsolidation: now.Add(-2 * time.Hour),
		sessionKeyFn:      func() string { return "test/c1/1" },
		isTurnInFlightFn:  inFlightFn(t, "test/c1"),
		branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
			calls++
			return true
		},
		done: make(chan struct{}),
	}

	r.maybeConsolidation()
	waitIdle(t, r)

	if calls != 0 {
		t.Errorf("consolidation fired %d times while turn in flight, want 0", calls)
	}
}

func TestParentTurnInFlight_NilFunc(t *testing.T) {
	// When IsTurnInFlightFunc is nil (test runners, agents that don't wire
	// it), parentTurnInFlight returns false unconditionally — schedulers
	// behave as they did pre-TODO #760 (no in-flight gating).
	r := &Runner{}
	if r.parentTurnInFlight("test/c1/1") {
		t.Error("parentTurnInFlight should return false when isTurnInFlightFn is nil")
	}
}

func TestParentTurnInFlight_EmptyKey(t *testing.T) {
	// Empty parent key (no default session) → return false. Avoids passing
	// an empty base into the callback and getting a misleading positive.
	r := &Runner{
		isTurnInFlightFn: func(string) bool { return true },
	}
	if r.parentTurnInFlight("") {
		t.Error("parentTurnInFlight should return false on empty parentKey")
	}
}

func TestParentTurnInFlight_BasePassedToCallback(t *testing.T) {
	// parentTurnInFlight must pass SessionKeyBase(parentKey), not the full
	// key. The callback's job is to look up in-flight state by base.
	var got string
	r := &Runner{
		isTurnInFlightFn: func(base string) bool {
			got = base
			return false
		},
	}
	r.parentTurnInFlight("test/c1/1759000000")
	if got != "test/c1" {
		t.Errorf("callback received base %q, want %q (SessionKeyBase strips the version)", got, "test/c1")
	}
}
