package periodic

import (
	"context"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
)

// TestMaybeKeepalive_CacheTTLForOverridesStatic: the per-session resolver, not
// the static CacheTTL, decides whether a cache is still worth warming.
func TestMaybeKeepalive_CacheTTLForOverridesStatic(t *testing.T) {
	for _, c := range []struct {
		name      string
		static    time.Duration
		perSess   time.Duration
		wantCalls int
	}{
		{"per-session 5m says expired despite static 1h", time.Hour, 5 * time.Minute, 0},
		{"per-session 1h says warm despite static 5m", 5 * time.Minute, time.Hour, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls int
			perSess := c.perSess
			r := &Runner{
				log:     log.NewComponentLogger("keepalive:test"),
				agentID: "test",
				kaCfg: config.ResolvedKeepalive{
					Enabled:  true,
					Interval: "1m",
					Prompt:   "keepalive.md",
				},
				cacheTTL:     c.static,
				cacheTTLFor:  func(string) time.Duration { return perSess },
				sessionIndex: indexWithTouch(t, "test/c1", 10*time.Minute),
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
