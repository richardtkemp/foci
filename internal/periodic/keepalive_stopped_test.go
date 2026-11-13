package periodic

import (
	"context"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
)

// TestMaybeKeepalive_StoppedSessionSkipped (#3089): a session whose last fork
// missed its cache, and which has made no call of its own since, is not
// warmed, though keepalive is enabled and the session is due; with the stop
// lifted (or the gate unwired) the same session is warmed.
func TestMaybeKeepalive_StoppedSessionSkipped(t *testing.T) {
	for _, c := range []struct {
		name      string
		stopped   func(string) bool
		wantCalls int
	}{
		{"stopped after a fork miss", func(string) bool { return true }, 0},
		{"not stopped", func(string) bool { return false }, 1},
		{"gate not wired", nil, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls int
			r := &Runner{
				log:     log.NewComponentLogger("keepalive:test"),
				agentID: "test",
				kaCfg: config.ResolvedKeepalive{
					Enabled:  true,
					Interval: "1m",
					Prompt:   "keepalive.md",
				},
				cacheTTL:            time.Hour,
				keepaliveStoppedFor: c.stopped,
				sessionIndex:        indexWithTouch(t, "test/c1", 10*time.Minute),
				agent: &fakeBackgroundAgent{
					sessionKeyFn: func() string { return "test/c1" },
					branchFn: func(_, _, _ string, _ bool) bool {
						calls++
						return true
					},
				},
				done: make(chan struct{}),
			}
			r.maybeKeepalive(context.Background())
			waitIdle(t, r)
			if calls != c.wantCalls {
				t.Errorf("keepalive calls = %d, want %d", calls, c.wantCalls)
			}
		})
	}
}

// TestKeepaliveWillRewarm_Stopped (#3089): pre-cold compaction asks whether
// keepalive will warm a session again; for a stopped session the answer is
// no, so the two cannot disagree.
func TestKeepaliveWillRewarm_Stopped(t *testing.T) {
	newRunner := func(stopped bool) *Runner {
		return &Runner{
			log:     log.NewComponentLogger("keepalive:test"),
			agentID: "test",
			kaCfg: config.ResolvedKeepalive{
				Enabled:  true,
				Interval: "1m",
			},
			cacheTTL:            time.Hour,
			keepaliveStoppedFor: func(string) bool { return stopped },
			sessionIndex:        indexWithTouch(t, "test/c1", 10*time.Minute),
			agent: &fakeBackgroundAgent{
				sessionKeyFn: func() string { return "test/c1" },
			},
			done: make(chan struct{}),
		}
	}
	now := time.Now()
	if !newRunner(false).keepaliveWillRewarm("test/c1", now) {
		t.Fatal("control: keepaliveWillRewarm = false with no stop, want true")
	}
	if newRunner(true).keepaliveWillRewarm("test/c1", now) {
		t.Error("keepaliveWillRewarm = true for a stopped session, want false")
	}
}
