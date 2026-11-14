package tools

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
)

// recorded is one call of a UsageRecorder.
type recorded struct {
	info   *delegator.UsageInfo
	source string
}

// TestUsageCache_RecordsEachReadWithSource (#3103 requirement 6, H7): a
// scheduled read and a read a spawn's check starts are each recorded with
// their source; a failed read records nothing.
func TestUsageCache_RecordsEachReadWithSource(t *testing.T) {
	t.Parallel()
	c := NewUsageCache()
	c.wait = 10 * time.Second
	ticks := make(chan chan time.Time, 8)
	c.after = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ticks <- ch
		return ch
	}
	c.first = func() time.Duration { return time.Second }
	c.interval = func() time.Duration { return time.Minute }

	var mu sync.Mutex
	fail := true
	var got []recorded
	read := c.Register("acct", func(context.Context) (*delegator.UsageInfo, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return nil, errors.New("boom")
		}
		return usageOf(7, 3), nil
	}, nil, func(info *delegator.UsageInfo, source string) {
		mu.Lock()
		got = append(got, recorded{info, source})
		mu.Unlock()
	})
	snapshot := func() []recorded { mu.Lock(); defer mu.Unlock(); return append([]recorded(nil), got...) }

	// A spawn's read that fails stores nothing.
	if r := read(context.Background()); r.Err == nil {
		t.Fatalf("read = %+v, want the failure", r)
	}
	if n := len(snapshot()); n != 0 {
		t.Fatalf("a failed read recorded %d report(s)", n)
	}

	// A spawn's read that succeeds is recorded as "spawn" (after the retry gap).
	mu.Lock()
	fail = false
	mu.Unlock()
	c.mu.Lock()
	c.entries["acct"].errAt = time.Time{}
	c.mu.Unlock()
	if r := read(context.Background()); r.Info == nil {
		t.Fatalf("read = %+v, want a reading", r)
	}
	if g := snapshot(); len(g) != 1 || g[0].source != delegator.UsageSourceSpawn || g[0].info.FiveHour.Percent != 7 {
		t.Fatalf("after a spawn read: %+v, want one spawn record", g)
	}

	// A scheduled read is recorded as "scheduled".
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	select {
	case ch := <-ticks:
		ch <- time.Now()
	case <-time.After(10 * time.Second):
		t.Fatal("the schedule never waited")
	}
	select {
	case <-ticks: // the next wait is armed after the read ended
	case <-time.After(10 * time.Second):
		t.Fatal("the scheduled read never ended")
	}
	if g := snapshot(); len(g) != 2 || g[1].source != delegator.UsageSourceScheduled {
		t.Fatalf("after a scheduled read: %+v, want a scheduled record", g)
	}
}
