package periodic

import (
	"context"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
)

// TestMaybeKeepalive_EndpointOffSkipsSession (#3020): a session whose
// endpoint turns keepalive off is never warmed, though keepalive is enabled
// for the agent and the session is due; with the gate open (or unwired) the
// same session is warmed.
func TestMaybeKeepalive_EndpointOffSkipsSession(t *testing.T) {
	for _, c := range []struct {
		name      string
		offFor    func(string) bool
		wantCalls int
	}{
		{"endpoint keepalive off", func(string) bool { return true }, 0},
		{"endpoint keepalive not off", func(string) bool { return false }, 1},
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
				cacheTTL:        time.Hour,
				keepaliveOffFor: c.offFor,
				sessionIndex:    indexWithTouch(t, "test/c1", 10*time.Minute),
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

// TestKeepaliveWillRewarm_EndpointOff (#3020): pre-cold compaction asks
// whether keepalive will warm a session again; for a session whose endpoint
// turns keepalive off the answer is no.
func TestKeepaliveWillRewarm_EndpointOff(t *testing.T) {
	newRunner := func(off bool) *Runner {
		return &Runner{
			log:     log.NewComponentLogger("keepalive:test"),
			agentID: "test",
			kaCfg: config.ResolvedKeepalive{
				Enabled:  true,
				Interval: "1m",
			},
			cacheTTL:        time.Hour,
			keepaliveOffFor: func(string) bool { return off },
			sessionIndex:    indexWithTouch(t, "test/c1", 10*time.Minute),
			agent: &fakeBackgroundAgent{
				sessionKeyFn: func() string { return "test/c1" },
			},
			done: make(chan struct{}),
		}
	}
	now := time.Now()
	if !newRunner(false).keepaliveWillRewarm("test/c1", now) {
		t.Fatal("control: keepaliveWillRewarm = false with the endpoint gate open, want true")
	}
	if newRunner(true).keepaliveWillRewarm("test/c1", now) {
		t.Error("keepaliveWillRewarm = true for a session whose endpoint turns keepalive off, want false")
	}
}
