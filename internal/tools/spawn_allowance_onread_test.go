package tools

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
)

// TestUsageCache_OnReadReachesEveryAgentOfTheAccount (#3144 requirement 3):
// every agent whose usage maps to an account hears each successful read of
// it, whichever agent registered first and whether it subscribed before or
// after the query was registered; a failed read reaches none, and a second
// subscription of one agent replaces its first.
func TestUsageCache_OnReadReachesEveryAgentOfTheAccount(t *testing.T) {
	t.Parallel()
	c := NewUsageCache()
	c.wait = 10 * time.Second

	var mu sync.Mutex
	heard := map[string][]string{} // agent -> sources
	listen := func(agent string) UsageRecorder {
		return func(info *delegator.UsageInfo, source string) {
			if info == nil || info.FiveHour.Percent != 7 {
				t.Errorf("%s heard %+v, want the read", agent, info)
			}
			mu.Lock()
			heard[agent] = append(heard[agent], source)
			mu.Unlock()
		}
	}
	snapshot := func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]int{}
		for k, v := range heard {
			out[k] = len(v)
		}
		return out
	}

	c.OnRead("acct", "early", func(*delegator.UsageInfo, string) { t.Error("replaced listener called") })
	c.OnRead("acct", "early", listen("early")) // replaces
	fail := true
	read := c.Register("acct", func(context.Context) (*delegator.UsageInfo, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return nil, errors.New("boom")
		}
		return usageOf(7, 3), nil
	}, nil)
	c.OnRead("acct", "late", listen("late"))
	c.OnRead("other", "stranger", listen("stranger"))

	if r := read(context.Background()); r.Err == nil {
		t.Fatalf("read = %+v, want the failure", r)
	}
	if got := snapshot(); len(got) != 0 {
		t.Fatalf("a failed read reached %v", got)
	}

	mu.Lock()
	fail = false
	mu.Unlock()
	c.mu.Lock()
	c.entries["acct"].errAt = time.Time{}
	c.mu.Unlock()
	if r := read(context.Background()); r.Info == nil {
		t.Fatalf("read = %+v, want a reading", r)
	}
	// Listeners run before the read counts as ended, so they have run by now.
	got := snapshot()
	if got["early"] != 1 || got["late"] != 1 || got["stranger"] != 0 {
		t.Fatalf("listeners heard %v, want early and late once each and no stranger", got)
	}
	mu.Lock()
	src := heard["late"][0]
	mu.Unlock()
	if src != delegator.UsageSourceSpawn {
		t.Fatalf("source = %q, want %q", src, delegator.UsageSourceSpawn)
	}
}
