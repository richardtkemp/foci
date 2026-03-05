package warnings

import (
	"sync"
	"testing"
	"time"
)

func TestDispatcher_NilQueue_Skips(t *testing.T) {
	calls := 0
	d := NewDispatcher(DispatcherConfig{
		DispatchFn: func(text string) { calls++ },
	})
	d.MaybeFire()
	if calls != 0 {
		t.Errorf("expected 0 dispatch calls with nil queue, got %d", calls)
	}
}

func TestDispatcher_EmptyQueue_Skips(t *testing.T) {
	calls := 0
	q := NewQueue(0, 0)
	d := NewDispatcher(DispatcherConfig{
		Queue:      q,
		DispatchFn: func(text string) { calls++ },
	})
	d.MaybeFire()
	if calls != 0 {
		t.Errorf("expected 0 dispatch calls with empty queue, got %d", calls)
	}
}

func TestDispatcher_ActiveUser_RateLimit(t *testing.T) {
	q := NewQueue(0, 0)
	var mu sync.Mutex
	calls := 0

	d := NewDispatcher(DispatcherConfig{
		Queue:             q,
		ActiveInterval:    5 * time.Minute,
		InactiveInterval:  1 * time.Hour,
		ActivityThreshold: 10 * time.Minute,
		LastUserMessageTimeFn: func() time.Time { return time.Now() }, // active user
		DispatchFn: func(text string) {
			mu.Lock()
			calls++
			mu.Unlock()
		},
	})

	// First dispatch should fire
	q.Push("WARN", "test", "disk full")
	d.MaybeFire()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("first dispatch: expected 1 call, got %d", got)
	}

	// Second dispatch immediately should be rate-limited (5m not elapsed)
	q.Push("WARN", "test", "disk still full")
	d.MaybeFire()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got = calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("second dispatch: expected still 1 call (rate limited), got %d", got)
	}
}

func TestDispatcher_InactiveUser_RateLimit(t *testing.T) {
	q := NewQueue(0, 0)
	var mu sync.Mutex
	calls := 0

	d := NewDispatcher(DispatcherConfig{
		Queue:             q,
		ActiveInterval:    5 * time.Minute,
		InactiveInterval:  1 * time.Hour,
		ActivityThreshold: 10 * time.Minute,
		LastUserMessageTimeFn: func() time.Time { return time.Now().Add(-30 * time.Minute) }, // inactive user
		DispatchFn: func(text string) {
			mu.Lock()
			calls++
			mu.Unlock()
		},
	})

	// First dispatch should fire (no prior dispatch)
	q.Push("WARN", "test", "disk full")
	d.MaybeFire()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("first dispatch: expected 1 call, got %d", got)
	}

	// Second dispatch should be rate-limited (1h not elapsed)
	q.Push("WARN", "test", "still full")
	d.MaybeFire()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got = calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("second dispatch: expected still 1 (1h rate limit), got %d", got)
	}
}

func TestDispatcher_Dispatches(t *testing.T) {
	q := NewQueue(0, 0)
	var mu sync.Mutex
	var dispatched string

	d := NewDispatcher(DispatcherConfig{
		Queue:             q,
		ActiveInterval:    0, // no rate limit for test
		InactiveInterval:  0,
		ActivityThreshold: 10 * time.Minute,
		LastUserMessageTimeFn: func() time.Time { return time.Now() },
		FormatFn: func(body string) string {
			return "[PROACTIVE WARNINGS]\n" + body + "\n[SYSTEM INJECTION]"
		},
		DispatchFn: func(text string) {
			mu.Lock()
			dispatched = text
			mu.Unlock()
		},
	})

	q.Push("WARN", "disk", "filesystem 95% full")
	q.Push("ERROR", "tmux", "OOM killed")
	d.MaybeFire()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := dispatched
	mu.Unlock()

	if !containsAll(got, "PROACTIVE WARNINGS", "filesystem 95% full", "OOM killed", "SYSTEM INJECTION") {
		t.Errorf("dispatched text missing expected content: %q", got)
	}
}

func TestDispatcher_ConcurrentGuard(t *testing.T) {
	q := NewQueue(0, 0)
	var mu sync.Mutex
	calls := 0

	d := NewDispatcher(DispatcherConfig{
		Queue:             q,
		ActiveInterval:    0,
		InactiveInterval:  0,
		ActivityThreshold: 10 * time.Minute,
		LastUserMessageTimeFn: func() time.Time { return time.Now() },
		DispatchFn: func(text string) {
			mu.Lock()
			calls++
			mu.Unlock()
			time.Sleep(200 * time.Millisecond) // simulate slow dispatch
		},
	})

	q.Push("WARN", "test", "first")
	d.MaybeFire()
	time.Sleep(20 * time.Millisecond) // let goroutine start

	// Push more and try again while first is still dispatching
	q.Push("WARN", "test", "second")
	d.MaybeFire()

	time.Sleep(300 * time.Millisecond) // wait for first to finish

	mu.Lock()
	got := calls
	mu.Unlock()

	if got != 1 {
		t.Errorf("expected 1 dispatch (concurrent guard should block second), got %d", got)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
