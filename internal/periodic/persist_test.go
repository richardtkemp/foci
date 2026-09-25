package periodic

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/session"
)

// #2026: every restart-sensitive runner timer is persisted through one helper
// and restored by New(), so a restart neither DELAYS a schedule (re-anchoring
// it to boot) nor SKIPS a gate (resetting it to zero). Each "restart" below is
// a fresh New() over the same state.db — exactly what a new process does.

// persistFixture is a state.db with one active chat session for agent "ag"
// whose human spoke 30m ago and which has never been reflected.
func persistFixture(t *testing.T) *session.SessionIndex {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	now := time.Now()
	idx.Upsert(session.SessionIndexEntry{
		SessionKey: "ag/c1", CreatedAt: now.Add(-48 * time.Hour),
		SessionType: session.SessionTypeChat, Status: session.SessionStatusActive,
	})
	idx.UpdateActivity("ag/c1", now.Add(-30*time.Minute))
	idx.TouchUserActivity("ag/c1", now.Add(-30*time.Minute))
	return idx
}

// persistCounts records how often each scheduler actually dispatched.
type persistCounts struct {
	branches map[string]*atomic.Int32 // by branch type
	resets   atomic.Int32
	cleanups atomic.Int32
}

func (c *persistCounts) branch(kind string) int { return int(c.branches[kind].Load()) }

// bootRunner is New() with every persisted scheduler enabled and a fake agent
// that counts dispatches. Intervals: reflection 1h, background 10m,
// consolidation/reset 24h, ephemeral cleanup daily.
func bootRunner(idx *session.SessionIndex, c *persistCounts) *Runner {
	c.branches = map[string]*atomic.Int32{}
	for _, k := range []string{"reflection", "background", "consolidation"} {
		c.branches[k] = &atomic.Int32{}
	}
	return New(RunnerConfig{
		AgentID:      "ag",
		SessionIndex: idx,
		Reflection:   config.ResolvedReflection{IntervalEnabled: true, Interval: "1h", IntervalPrompt: "reflection.md"},
		Background:   config.ResolvedBackground{Enabled: true, Interval: "10m", Prompt: "background.md"},
		Maintenance: config.ResolvedMaintenance{
			ConsolidationEnabled: true, ConsolidationTime: "24h", ConsolidationMaxIdle: "0s",
			ConsolidationPrompt: "memory-consolidation.md",
			ResetTime:           "24h", ResetIdleGuard: "0s",
		},
		EphemeralRetentionDays: 7,
		Agent: &fakeBackgroundAgent{
			sessionKeyFn: func() string { return "ag/c1" },
			branchFn: func(kind, _, _ string, _ bool) bool {
				if n, ok := c.branches[kind]; ok {
					n.Add(1)
				}
				return true
			},
			resetFn:   func(context.Context, string) error { c.resets.Add(1); return nil },
			cleanupFn: func(context.Context, int) int { c.cleanups.Add(1); return 0 },
		},
	})
}

func seedTimer(t *testing.T, idx *session.SessionIndex, key string, at time.Time) {
	t.Helper()
	if err := idx.PersistedTime("ag", key).Save(at); err != nil {
		t.Fatal(err)
	}
}

// Round trip: whatever a run leaves in memory is exactly what the next process
// boots with, for every persisted timer. This is the save half; the tests
// below pin the restore half against each gate.
func TestPersistedTimers_RoundTripAcrossRestart(t *testing.T) {
	idx := persistFixture(t)
	// Make every schedule due in the first process.
	long := time.Now().Add(-48 * time.Hour)
	for _, k := range []string{timerReflection, timerConsolidation, timerReset} {
		seedTimer(t, idx, k, long)
	}
	var c1 persistCounts
	r1 := bootRunner(idx, &c1)
	ctx := context.Background()
	r1.maybeReflection()
	waitIdle(t, r1)
	r1.maybeConsolidation()
	waitIdle(t, r1)
	r1.maybeReset(ctx)
	waitIdle(t, r1)
	r1.maybeBackgroundWork(ctx)
	waitIdle(t, r1)
	r1.maybeEphemeralCleanup(ctx)
	waitIdle(t, r1)
	if c1.branch("reflection") != 1 || c1.branch("consolidation") != 1 || c1.resets.Load() != 1 ||
		c1.branch("background") != 1 || c1.cleanups.Load() != 1 {
		t.Fatalf("premise: every scheduler must fire once in process 1 (refl=%d cons=%d reset=%d bg=%d cleanup=%d)",
			c1.branch("reflection"), c1.branch("consolidation"), c1.resets.Load(), c1.branch("background"), c1.cleanups.Load())
	}

	var c2 persistCounts
	r2 := bootRunner(idx, &c2)
	for _, tc := range []struct {
		name     string
		got, run time.Time
	}{
		{"lastReflection", r2.lastReflection, r1.lastReflection},
		{"lastConsolidation", r2.lastConsolidation, r1.lastConsolidation},
		{"lastReset", r2.lastReset, r1.lastReset},
		{"lastBackgroundEnded", r2.lastBackgroundEnded, r1.lastBackgroundEnded},
		{"lastEphemeralCleanup", r2.lastEphemeralCleanup, r1.lastEphemeralCleanup},
	} {
		if !tc.got.Equal(tc.run) {
			t.Errorf("%s after restart = %v, want the pre-restart value %v", tc.name, tc.got, tc.run)
		}
	}
}

// Reflection last ran 2h ago (1h interval) in the previous process: it is due
// NOW. Before #2026 New() re-anchored lastReflection to boot, delaying it a
// full interval after every restart.
func TestPersistedTimers_ReflectionNotDelayedByRestart(t *testing.T) {
	idx := persistFixture(t)
	seedTimer(t, idx, timerReflection, time.Now().Add(-2*time.Hour))
	var c persistCounts
	r := bootRunner(idx, &c)
	r.maybeReflection()
	waitIdle(t, r)
	if n := c.branch("reflection"); n != 1 {
		t.Errorf("reflection fired %d time(s) right after restart, want 1 (last ran 2h ago, interval 1h)", n)
	}
}

// Control: a reflection 10m before the restart is not re-run early.
func TestPersistedTimers_ReflectionNotRepeatedByRestart(t *testing.T) {
	idx := persistFixture(t)
	seedTimer(t, idx, timerReflection, time.Now().Add(-10*time.Minute))
	var c persistCounts
	r := bootRunner(idx, &c)
	r.maybeReflection()
	waitIdle(t, r)
	if n := c.branch("reflection"); n != 0 {
		t.Errorf("reflection fired %d time(s), want 0 (ran 10m ago, interval 1h)", n)
	}
}

// Background work ended 1m before the restart (10m cooldown): the new process
// must honour the cooldown. Before #2026 lastBackgroundEnded booted at zero,
// so the cooldown was skipped and a run could chain straight onto the last.
func TestPersistedTimers_BackgroundCooldownNotSkippedByRestart(t *testing.T) {
	idx := persistFixture(t)
	seedTimer(t, idx, timerBackgroundEnded, time.Now().Add(-time.Minute))
	var c persistCounts
	r := bootRunner(idx, &c)
	r.maybeBackgroundWork(context.Background())
	waitIdle(t, r)
	if n := c.branch("background"); n != 0 {
		t.Errorf("background fired %d time(s) 1m after the last one ended, want 0 (10m cooldown)", n)
	}
}

// Control: a cooldown that has elapsed does not block.
func TestPersistedTimers_BackgroundCooldownElapsedFires(t *testing.T) {
	idx := persistFixture(t)
	seedTimer(t, idx, timerBackgroundEnded, time.Now().Add(-time.Hour))
	var c persistCounts
	r := bootRunner(idx, &c)
	r.maybeBackgroundWork(context.Background())
	waitIdle(t, r)
	if n := c.branch("background"); n != 1 {
		t.Errorf("background fired %d time(s), want 1 (cooldown elapsed)", n)
	}
}

// The daily ephemeral cleanup ran 1h before the restart: it must not run
// again at boot. Before #2026 it ran on every boot.
func TestPersistedTimers_EphemeralCleanupNotRepeatedByRestart(t *testing.T) {
	idx := persistFixture(t)
	seedTimer(t, idx, timerEphemeralCleanup, time.Now().Add(-time.Hour))
	var c persistCounts
	r := bootRunner(idx, &c)
	r.maybeEphemeralCleanup(context.Background())
	waitIdle(t, r)
	if n := c.cleanups.Load(); n != 0 {
		t.Errorf("ephemeral cleanup ran %d time(s) 1h after the last run, want 0 (daily)", n)
	}
}

// Control: a day-old cleanup is due at boot.
func TestPersistedTimers_EphemeralCleanupDueFires(t *testing.T) {
	idx := persistFixture(t)
	seedTimer(t, idx, timerEphemeralCleanup, time.Now().Add(-25*time.Hour))
	var c persistCounts
	r := bootRunner(idx, &c)
	r.maybeEphemeralCleanup(context.Background())
	waitIdle(t, r)
	if n := c.cleanups.Load(); n != 1 {
		t.Errorf("ephemeral cleanup ran %d time(s), want 1 (last run 25h ago)", n)
	}
}

// Consolidation and reset: due before the restart → due after it; ran just
// before it → not re-run.
func TestPersistedTimers_ConsolidationAndResetAcrossRestart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ago    time.Duration
		wantN  int
		hasRun string
	}{
		{"due", 48 * time.Hour, 1, "overdue"},
		{"recent", time.Minute, 0, "ran 1m ago"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := persistFixture(t)
			seedTimer(t, idx, timerConsolidation, time.Now().Add(-tc.ago))
			seedTimer(t, idx, timerReset, time.Now().Add(-tc.ago))
			var c persistCounts
			r := bootRunner(idx, &c)
			r.maybeConsolidation()
			waitIdle(t, r)
			r.maybeReset(context.Background())
			waitIdle(t, r)
			if n := c.branch("consolidation"); n != tc.wantN {
				t.Errorf("consolidation fired %d, want %d (%s)", n, tc.wantN, tc.hasRun)
			}
			if n := int(c.resets.Load()); n != tc.wantN {
				t.Errorf("reset fired %d, want %d (%s)", n, tc.wantN, tc.hasRun)
			}
		})
	}
}

// Values persisted before the helper existed (RFC3339, whole seconds, by the
// old consolidation/reset code) must still restore.
func TestPersistedTimers_LegacyRFC3339Restores(t *testing.T) {
	idx := persistFixture(t)
	legacy := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	if err := idx.SetAgentMetadata("ag", timerConsolidation, legacy.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	var c persistCounts
	r := bootRunner(idx, &c)
	if !r.lastConsolidation.Equal(legacy) {
		t.Errorf("lastConsolidation = %v, want legacy value %v", r.lastConsolidation, legacy)
	}
}
