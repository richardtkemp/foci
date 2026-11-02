package periodic

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/session"
)

// twoChatRunner is a Runner with two open app chats, c1 and c2, both due for
// a keepalive (cache touched past the interval, well inside the TTL).
func twoChatRunner(t *testing.T, agent BackgroundAgent, timeout string) (*Runner, *session.SessionIndex) {
	t.Helper()
	now := time.Now()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	for _, sk := range []string{"test/c1", "test/c2"} {
		idx.Upsert(session.SessionIndexEntry{
			SessionKey:  sk,
			FilePath:    "/tmp/" + filepath.Base(sk) + ".jsonl",
			CreatedAt:   now.Add(-24 * time.Hour),
			SessionType: session.SessionTypeChat,
			Status:      session.SessionStatusActive,
		})
		idx.TouchCacheTouch(sk, now.Add(-2*time.Second))
	}
	return &Runner{
		log:                log.NewComponentLogger("keepalive:test"),
		agentID:            "test",
		kaCfg:              config.ResolvedKeepalive{Enabled: true, Interval: "1s", WarmOpenAppChats: true, Timeout: timeout},
		sessionIndex:       idx,
		cacheTTL:           time.Hour,
		agent:              agent,
		openChatSessionsFn: func() []string { return []string{"test/c1", "test/c2"} },
		done:               make(chan struct{}),
	}, idx
}

// TestKeepalive_StuckSessionDoesNotBlockAnother is #2487: the keepalive of c1
// hangs; c2, due on the same tick, is still warmed, and a second tick does not
// start another keepalive on c1 while its first one runs.
func TestKeepalive_StuckSessionDoesNotBlockAnother(t *testing.T) {
	release := make(chan struct{})
	c2Warmed := make(chan struct{}, 4)
	var mu sync.Mutex
	calls := map[string]int{}
	r, _ := twoChatRunner(t, &fakeBackgroundAgent{
		branchCtxFn: func(ctx context.Context, branchType, parentKey, promptText string, noCompact bool) bool {
			mu.Lock()
			calls[parentKey]++
			mu.Unlock()
			if parentKey == "test/c1" {
				select {
				case <-release:
				case <-ctx.Done():
				}
				return false
			}
			c2Warmed <- struct{}{}
			return true
		},
	}, "1h")
	defer close(release)

	r.maybeKeepalive(context.Background())
	select {
	case <-c2Warmed:
	case <-time.After(3 * time.Second):
		t.Fatal("c2 was not warmed while c1's keepalive hung (#2487)")
	}

	r.maybeKeepalive(context.Background())
	select {
	case <-c2Warmed:
	case <-time.After(3 * time.Second):
		t.Fatal("second tick did not warm c2")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["test/c1"] != 1 {
		t.Errorf("keepalives started on c1 = %d, want 1 (one per session at a time)", calls["test/c1"])
	}
	if !r.keepaliveRunningFor("test/c1") {
		t.Error("c1's hung keepalive is not tracked as running")
	}
}

// TestKeepalive_TimeoutCancelsAndReleases is #2489: a keepalive that has not
// completed within keepalive.timeout is cancelled, its session's run state is
// released, the cache touch it advanced is put back, and the next tick can
// warm the session again.
func TestKeepalive_TimeoutCancelsAndReleases(t *testing.T) {
	cancelled := make(chan error, 1)
	var idx *session.SessionIndex
	var mu sync.Mutex
	c1Calls := 0
	r, idx0 := twoChatRunner(t, &fakeBackgroundAgent{
		branchCtxFn: func(ctx context.Context, branchType, parentKey, promptText string, noCompact bool) bool {
			if parentKey != "test/c1" {
				return true
			}
			mu.Lock()
			c1Calls++
			n := c1Calls
			mu.Unlock()
			if n > 1 {
				return true
			}
			// The turn reached the backend (which records a touch) and then
			// hangs, as on a rejected usage limit.
			idx.TouchCacheTouch("test/c1", time.Now())
			<-ctx.Done()
			cancelled <- ctx.Err()
			return false
		},
	}, "150ms")
	idx = idx0
	before, _ := idx.LastCacheTouch("test/c1")

	r.maybeKeepalive(context.Background())
	select {
	case err := <-cancelled:
		if err != context.DeadlineExceeded {
			t.Errorf("keepalive ctx err = %v, want deadline exceeded", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hung keepalive was not cancelled by its timeout")
	}
	deadline := time.Now().Add(2 * time.Second)
	for r.keepaliveRunningFor("test/c1") {
		if time.Now().After(deadline) {
			t.Fatal("c1's run state was not released after the timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got, _ := idx.LastCacheTouch("test/c1"); !got.Equal(before) {
		t.Errorf("last_cache_touch after a timed-out keepalive = %v, want it put back to %v", got, before)
	}

	r.maybeKeepalive(context.Background())
	deadline = time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := c1Calls
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the next tick did not warm c1 again after its keepalive timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestKeepaliveTimeout_Default is #2489's config key: 2 minutes when unset or
// bad, the configured value otherwise.
func TestKeepaliveTimeout_Default(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"", 2 * time.Minute},
		{"30s", 30 * time.Second},
		{"0", 2 * time.Minute},
		{"bogus", 2 * time.Minute},
	} {
		r := &Runner{log: log.NewComponentLogger("keepalive:test"), kaCfg: config.ResolvedKeepalive{Timeout: tc.in}}
		if got := r.keepaliveTimeout(); got != tc.want {
			t.Errorf("keepaliveTimeout(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// The shipped default of the config key is 2 minutes too.
	f, ok := config.LookupField("keepalive.timeout")
	if !ok {
		t.Fatal("keepalive.timeout is not a registered config field")
	}
	if f.Default != "2m" {
		t.Errorf("keepalive.timeout default = %q, want 2m", f.Default)
	}
}
