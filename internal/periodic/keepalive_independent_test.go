package periodic

import (
	"context"
	"path/filepath"
	"slices"
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

	// Age both warm attempts past the interval (#2509), so the second tick is
	// gated only by c1's keepalive still running, which is what this tests.
	r.noteWarmAttempt("test/c1", time.Now().Add(-time.Hour))
	r.noteWarmAttempt("test/c2", time.Now().Add(-time.Hour))
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
// released, and a later tick warms the session again. Since #2509 the hung
// turn produced no model response, so last_cache_touch never moved and there
// is nothing to put back; the retry waits one interval after the attempt.
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
			// The turn reached the backend and then hangs, as on a
			// rejected usage limit. No model response, so no touch.
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
		t.Errorf("last_cache_touch after a timed-out keepalive = %v, want unchanged %v", got, before)
	}

	// Ticks keep coming; c1 is retried once its 1s interval since the
	// attempt has passed.
	deadline = time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		n := c1Calls
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a later tick did not warm c1 again after its keepalive timed out")
		}
		r.maybeKeepalive(context.Background())
		time.Sleep(50 * time.Millisecond)
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

// TestKeepalive_FailedWarmRetriesOncePerInterval is #2509: a keepalive whose
// fork fails leaves last_cache_touch where it was, so the session stays in
// the warm window. The warm-attempt debounce keeps keepalive to one try per
// interval instead of one per tick, and keepaliveWillRewarm agrees.
func TestKeepalive_FailedWarmRetriesOncePerInterval(t *testing.T) {
	t.Parallel()
	r, _ := twoChatRunner(t, &fakeBackgroundAgent{}, "")
	interval := 10 * time.Minute
	r.kaCfg.Interval = interval.String()
	now := time.Now()
	r.sessionIndex.TouchCacheTouch("test/c1", now.Add(-20*time.Minute))
	r.sessionIndex.TouchCacheTouch("test/c2", now.Add(-20*time.Minute))

	r.noteWarmAttempt("test/c1", now)

	ready, _ := r.keepaliveTargetsAt(interval, now.Add(interval/2))
	if slices.Contains(ready, "test/c1") {
		t.Errorf("c1 retried %s after a failed attempt, want none within the %s interval", interval/2, interval)
	}
	if !slices.Contains(ready, "test/c2") {
		t.Errorf("c2 (no attempt) not ready: %v", ready)
	}
	ready, _ = r.keepaliveTargetsAt(interval, now.Add(interval))
	if !slices.Contains(ready, "test/c1") {
		t.Errorf("c1 not retried one interval after its failed attempt: %v", ready)
	}
}
