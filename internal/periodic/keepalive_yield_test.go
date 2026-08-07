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

// Keepalive must not branch a session while reflection or consolidation is
// branching the SAME session. The branch key carries a one-second timestamp
// (session.withChild), so two branches off one parent inside the same second
// collide on the key and one retries a second later — observed in production
// on 2026-08-06 20:53:21, where a keepalive and a reflection fired on the same
// scheduler tick.
//
// Two halves, both needed:
//   - the gate itself (maybeKeepalive yields to the flags)
//   - the ORDER in the run loop that makes the gate reachable on the same tick.
//
// The order half is the one that actually pins the fix: every maybeX sets its
// running flag synchronously and then dispatches in a goroutine, so if
// maybeKeepalive ran first (as it did before this change) it would read flags
// that are still false and fire anyway. A flags-only test passes under the
// broken ordering.

// dueRunner builds a Runner with one session that is due for BOTH keepalive
// (cache touched 2s ago against a 1s interval) and reflection (activity since a
// reflection stamped 2h ago against a 1h interval).
func dueRunner(t *testing.T, agent BackgroundAgent) *Runner {
	t.Helper()
	now := time.Now()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  "test/c1",
		FilePath:    "/tmp/test.jsonl",
		CreatedAt:   now.Add(-24 * time.Hour),
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})
	idx.TouchCacheTouch("test/c1", now.Add(-2*time.Second))
	idx.UpdateActivity("test/c1", now.Add(-30*time.Minute))
	idx.StampReflection("test/c1", now.Add(-2*time.Hour))

	return &Runner{
		log:             log.NewComponentLogger("keepalive:test"),
		agentID:         "test",
		kaCfg:           config.ResolvedKeepalive{Enabled: true, Interval: "1s"},
		reflectCfg:      config.ResolvedReflection{IntervalEnabled: true, Interval: "1h"},
		sessionIndex:    idx,
		agent:           agent,
		lastReflection:  now.Add(-2 * time.Hour),
		lastInteraction: now.Add(-30 * time.Minute),
		done:            make(chan struct{}),
	}
}

// TestTick_KeepaliveYieldsToReflection drives the REAL run loop for one tick
// with a session due for both, and proves keepalive stays silent for as long as
// the reflection branch is in flight. Fails on the pre-fix ordering, where
// maybeKeepalive ran before maybeReflection and both branched on tick one.
func TestTick_KeepaliveYieldsToReflection(t *testing.T) {
	reflectionStarted := make(chan struct{})
	keepaliveFired := make(chan struct{}, 1)
	release := make(chan struct{})

	r := dueRunner(t, &fakeBackgroundAgent{
		sessionKeyFn: func() string { return "test/c1" },
		branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
			switch branchType {
			case "reflection":
				close(reflectionStarted)
				<-release // hold reflectionRunning true for the rest of the test
			case "keepalive":
				select {
				case keepaliveFired <- struct{}{}:
				default:
				}
			}
			return true
		},
	})
	r.tickInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	// Release the parked branch and let its goroutine finish BEFORE t.Cleanup
	// closes the session index under it (defers run before cleanups).
	defer func() { close(release); r.Stop(); waitIdle(t, r) }()

	select {
	case <-reflectionStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("reflection never fired — the fixture is not due, so this test proves nothing")
	}

	// Reflection is now parked inside Branch, so reflectionRunning stays true.
	// Keepalive is due on every subsequent tick and must not fire on any of
	// them. Load-tolerant: heavier load means FEWER ticks in the window, and
	// the regression fires on the very first one.
	select {
	case <-keepaliveFired:
		t.Fatal("keepalive branched while a reflection branch was in flight on the same session " +
			"— they collide on the one-second branch key (see runner.go tick order)")
	case <-time.After(250 * time.Millisecond):
	}
}

// TestTick_KeepaliveYieldsToReset is the same proof for the reset pass, whose
// position in the tick is load-bearing for the same reason. Reset earns the
// gate twice over: it branches nothing itself (it calls ResetSession) but it
// ROTATES the session key, so a keepalive racing it warms a cache that is
// about to be discarded.
func TestTick_KeepaliveYieldsToReset(t *testing.T) {
	resetStarted := make(chan struct{})
	keepaliveFired := make(chan struct{}, 1)
	release := make(chan struct{})

	r := dueRunner(t, &fakeBackgroundAgent{
		sessionKeyFn: func() string { return "test/c1" },
		branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
			if branchType == "keepalive" {
				select {
				case keepaliveFired <- struct{}{}:
				default:
				}
			}
			return true
		},
		resetFn: func(ctx context.Context, sessionKey string) error {
			close(resetStarted)
			<-release // hold resetRunning true for the rest of the test
			return nil
		},
	})
	// Reflection off, reset due: last reset 2h ago against a 1h schedule.
	r.reflectCfg = config.ResolvedReflection{}
	r.maintCfg = config.ResolvedMaintenance{ResetTime: "1h"}
	r.lastReset = time.Now().Add(-2 * time.Hour)
	r.tickInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	defer func() { close(release); r.Stop(); waitReset(t, r) }()

	select {
	case <-resetStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("reset never fired — the fixture is not due, so this test proves nothing")
	}

	select {
	case <-keepaliveFired:
		t.Fatal("keepalive branched while a session reset was in flight — the reset rotates the " +
			"session key, discarding the cache keepalive just warmed (see runner.go tick order)")
	case <-time.After(250 * time.Millisecond):
	}
}

// waitReset waits for the reset goroutine to clear (waitIdle does not watch
// resetRunning), so t.Cleanup doesn't close the index under it.
func waitReset(t *testing.T, r *Runner) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		busy := r.resetRunning
		r.mu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("reset goroutine did not finish within 2s")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestMaybeKeepalive_YieldsToMemoryPasses covers the gate directly, for all
// three flags, without the loop.
func TestMaybeKeepalive_YieldsToMemoryPasses(t *testing.T) {
	cases := []struct {
		name        string
		reflection  bool
		consolidate bool
		reset       bool
		wantCalls   int
	}{
		{"none running fires normally", false, false, false, 1},
		{"reflection running yields", true, false, false, 0},
		{"consolidation running yields", false, true, false, 0},
		{"reset running yields", false, false, true, 0},
		{"all running yields", true, true, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r := dueRunner(t, &fakeBackgroundAgent{
				sessionKeyFn: func() string { return "test/c1" },
				branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
					calls++
					return true
				},
			})
			r.reflectionRunning = tc.reflection
			r.consolidationRunning = tc.consolidate
			r.resetRunning = tc.reset

			r.maybeKeepalive(context.Background())
			// Clear the flags so waitIdle only waits on the keepalive goroutine.
			r.mu.Lock()
			r.reflectionRunning, r.consolidationRunning, r.resetRunning = false, false, false
			r.mu.Unlock()
			waitIdle(t, r)

			if calls != tc.wantCalls {
				t.Errorf("keepalive branches = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}
