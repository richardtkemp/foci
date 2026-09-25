package periodic

import (
	"path/filepath"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/session"
)

// restartRunner builds a Runner through New() — exactly what a fresh process
// does at boot — over a state.db whose agent "ag" last had a human interaction
// `idleFor` ago (idleFor < 0 = no recorded interaction). Consolidation is due
// (last run 2h ago, 1h cadence) and gated on consolidation_max_idle=1h, so the
// only thing deciding whether it fires is the user-activity lookup.
func restartRunner(t *testing.T, idleFor time.Duration, calls *int) (*Runner, *session.SessionIndex) {
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
	if idleFor >= 0 {
		idx.TouchUserActivity("ag/c1", now.Add(-idleFor))
	}
	if err := idx.SetAgentMetadata("ag", "consolidation_last", now.Add(-2*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	r := New(RunnerConfig{
		AgentID:      "ag",
		SessionIndex: idx,
		Maintenance: config.ResolvedMaintenance{
			ConsolidationEnabled: true,
			ConsolidationTime:    "1h",
			ConsolidationMaxIdle: "1h",
			ConsolidationPrompt:  "memory-consolidation.md",
		},
		Agent: &fakeBackgroundAgent{
			sessionKeyFn: func() string { return "ag/c1" },
			branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
				*calls++
				return true
			},
		},
	})
	return r, idx
}

// TestRestart_IdleAgentDoesNotLookActive is the #2023 regression: an agent
// whose human last spoke a day ago must not look active just because the
// process restarted. Before the fix New() seeded lastInteraction to boot time,
// so every idle agent consolidated in its first hour after a restart.
func TestRestart_IdleAgentDoesNotLookActive(t *testing.T) {
	var calls int
	r, _ := restartRunner(t, 24*time.Hour, &calls)

	if since := r.sinceUserActivity(); since < 23*time.Hour {
		t.Errorf("sinceUserActivity() = %s right after restart, want ~24h (the recorded interaction, not boot)", since)
	}
	r.maybeConsolidation()
	waitIdle(t, r)
	if calls != 0 {
		t.Errorf("consolidation fired %d time(s) for an agent idle 24h — the restart made it look active", calls)
	}
}

// Control: a restart must not make a genuinely recent interaction look stale.
func TestRestart_RecentlyActiveAgentStillActive(t *testing.T) {
	var calls int
	r, _ := restartRunner(t, 30*time.Minute, &calls)
	r.maybeConsolidation()
	waitIdle(t, r)
	if calls != 1 {
		t.Errorf("consolidation calls = %d, want 1 (human active 30m ago, within max_idle 1h)", calls)
	}
}

// Documented choice: an agent with NO recorded human interaction keeps the
// pre-#2023 behaviour — boot counts as its last activity. That keeps a brand-new
// agent's first hour identical to before (reflection/consolidation may run,
// background work and reset wait out their idle windows) instead of treating a
// never-used agent as idle since the epoch.
func TestRestart_NoRecordedActivityFallsBackToBoot(t *testing.T) {
	var calls int
	r, _ := restartRunner(t, -1, &calls)
	if _, ok := r.LastUserActivity(); ok {
		t.Fatal("premise: fixture must have no recorded user activity")
	}
	if since := r.sinceUserActivity(); since > time.Minute {
		t.Errorf("sinceUserActivity() = %s with no record, want ~0 (boot fallback)", since)
	}
	r.maybeConsolidation()
	waitIdle(t, r)
	if calls != 1 {
		t.Errorf("consolidation calls = %d, want 1 (no record → boot counts as activity)", calls)
	}
}

// The lookup is live, not a boot-time seed: a human turn recorded in the DB
// after the runner started (the turn path writes last_user_activity_at) is seen
// on the next read, and an in-process receipt (NotifyInteraction — slash
// commands, which reach no turn) newer than the DB wins.
func TestLastUserActivity_LiveMaxOfDBAndReceipt(t *testing.T) {
	var calls int
	r, idx := restartRunner(t, 24*time.Hour, &calls)

	idx.TouchUserActivity("ag/c1", time.Now().Add(-10*time.Minute))
	if since := r.sinceUserActivity(); since > 11*time.Minute || since < 9*time.Minute {
		t.Errorf("after a DB touch 10m ago, sinceUserActivity() = %s, want ~10m", since)
	}

	r.NotifyInteraction()
	if since := r.sinceUserActivity(); since > time.Minute {
		t.Errorf("after NotifyInteraction, sinceUserActivity() = %s, want ~0", since)
	}
}
