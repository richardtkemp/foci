package warnings

import (
	"sync"
	"testing"
	"time"
)

func TestDispatcher_NilQueue_Skips(t *testing.T) {
	// Proves that MaybeFire is a no-op when the dispatcher has no queue configured.
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
	// Proves that MaybeFire does not invoke the dispatch function when the queue has no pending warnings.
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
	// Proves that when the user is active, a second MaybeFire call within the active interval is suppressed even though warnings are queued.
	q := NewQueue(0, 0)
	var mu sync.Mutex
	calls := 0

	d := NewDispatcher(DispatcherConfig{
		Queue:                 q,
		ActiveInterval:        5 * time.Minute,
		InactiveInterval:      1 * time.Hour,
		ActivityThreshold:     10 * time.Minute,
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
	// Proves that when the user is inactive, the longer inactive interval is enforced and a second MaybeFire within that interval is suppressed.
	q := NewQueue(0, 0)
	var mu sync.Mutex
	calls := 0

	d := NewDispatcher(DispatcherConfig{
		Queue:                 q,
		ActiveInterval:        5 * time.Minute,
		InactiveInterval:      1 * time.Hour,
		ActivityThreshold:     10 * time.Minute,
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
	// Proves that MaybeFire drains the queue, passes the result through FormatFn, and delivers the formatted text to DispatchFn.
	q := NewQueue(0, 0)
	var mu sync.Mutex
	var dispatched string

	d := NewDispatcher(DispatcherConfig{
		Queue:                 q,
		ActiveInterval:        0, // no rate limit for test
		InactiveInterval:      0,
		ActivityThreshold:     10 * time.Minute,
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
	// Proves that a second MaybeFire call while a dispatch goroutine is still running does not launch a concurrent second dispatch.
	q := NewQueue(0, 0)
	var mu sync.Mutex
	calls := 0

	d := NewDispatcher(DispatcherConfig{
		Queue:                 q,
		ActiveInterval:        0,
		InactiveInterval:      0,
		ActivityThreshold:     10 * time.Minute,
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

func TestDispatcher_SkipsWhenProcessing(t *testing.T) {
	// Proves that MaybeFire defers dispatch when IsProcessingFn returns true,
	// leaving warnings in the queue for later FlushPending.
	q := NewQueue(0, 0)
	var mu sync.Mutex
	calls := 0

	d := NewDispatcher(DispatcherConfig{
		Queue:            q,
		ActiveInterval:   0,
		InactiveInterval: 0,
		DispatchFn: func(text string) {
			mu.Lock()
			calls++
			mu.Unlock()
		},
		IsProcessingFn: func() bool { return true },
	})

	q.Push("WARN", "test", "deferred warning")
	d.MaybeFire()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 0 {
		t.Errorf("expected 0 dispatches while processing, got %d", got)
	}
	if !q.Pending() {
		t.Errorf("expected warnings to remain in queue")
	}
}

func TestDispatcher_FlushPending(t *testing.T) {
	// Proves that FlushPending dispatches queued warnings without rate-limit checks.
	q := NewQueue(0, 0)
	var mu sync.Mutex
	calls := 0

	d := NewDispatcher(DispatcherConfig{
		Queue:            q,
		ActiveInterval:   1 * time.Hour, // would normally block
		InactiveInterval: 1 * time.Hour,
		DispatchFn: func(text string) {
			mu.Lock()
			calls++
			mu.Unlock()
		},
	})

	q.Push("WARN", "test", "flush me")
	d.FlushPending()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("expected 1 dispatch from FlushPending, got %d", got)
	}
}

func TestDispatcher_FlushPending_Floored(t *testing.T) {
	// Proves a second FlushPending within flushMinInterval does not re-dispatch,
	// capping the turn-end feedback path.
	q := NewQueue(0, 0)
	var mu sync.Mutex
	calls := 0

	d := NewDispatcher(DispatcherConfig{
		Queue:      q,
		DispatchFn: func(string) { mu.Lock(); calls++; mu.Unlock() },
	})

	q.Push("WARN", "test", "one")
	d.FlushPending()
	time.Sleep(50 * time.Millisecond)

	q.Push("WARN", "test", "two")
	d.FlushPending() // within the floor → suppressed
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("expected 1 dispatch (second within floor suppressed), got %d", got)
	}
}

func TestDispatcher_FiresWhenNotProcessing(t *testing.T) {
	// Proves that MaybeFire dispatches normally when IsProcessingFn returns false.
	q := NewQueue(0, 0)
	var mu sync.Mutex
	calls := 0

	d := NewDispatcher(DispatcherConfig{
		Queue:            q,
		ActiveInterval:   0,
		InactiveInterval: 0,
		DispatchFn: func(text string) {
			mu.Lock()
			calls++
			mu.Unlock()
		},
		IsProcessingFn: func() bool { return false },
	})

	q.Push("WARN", "test", "should fire")
	d.MaybeFire()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("expected 1 dispatch when not processing, got %d", got)
	}
}

func TestDispatcher_SuppressesFeedbackLoop(t *testing.T) {
	// Proves that warnings generated during dispatch don't re-enter the same
	// queue. This prevents a feedback loop: dispatch fails → logs warning →
	// warning collected → next dispatch grows indefinitely.
	q := NewQueue(0, 0)
	var mu sync.Mutex
	calls := 0

	d := NewDispatcher(DispatcherConfig{
		Queue:            q,
		ActiveInterval:   0,
		InactiveInterval: 0,
		DispatchFn: func(text string) {
			mu.Lock()
			calls++
			mu.Unlock()
			// Simulate what happens when Discord has no channel:
			// the send path logs a warning that would normally be
			// captured by the warn hook and pushed back into the queue.
			q.Push("WARN", "discord", "no channel ID for notification: "+text)
		},
	})

	q.Push("WARN", "startup", "skills dir not found")
	d.MaybeFire()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("dispatch calls = %d, want 1", got)
	}

	// The re-entrant push during dispatch should have been suppressed.
	if q.Pending() {
		t.Error("queue should be empty — re-entrant push during dispatch must be suppressed")
	}
}

func TestDispatcher_SuppressesCrossQueueFeedback(t *testing.T) {
	// Proves that warnings generated during dispatch don't enter peer queues
	// either. The warn hook pushes to both ChatWarnings and Warnings queues;
	// without PeerQueues suppression, a ChatWarnings dispatch failure enters
	// the Warnings queue and vice versa, creating a cross-queue feedback loop.
	chatQ := NewQueue(0, 0)
	agentQ := NewQueue(0, 0)
	var mu sync.Mutex
	calls := 0

	d := NewDispatcher(DispatcherConfig{
		Queue:            chatQ,
		PeerQueues:       []*Queue{agentQ},
		ActiveInterval:   0,
		InactiveInterval: 0,
		DispatchFn: func(text string) {
			mu.Lock()
			calls++
			mu.Unlock()
			// Simulate the warn hook pushing to both queues when
			// Discord's send fails with "no channel ID".
			chatQ.Push("WARN", "discord", "no channel ID for notification: "+text)
			agentQ.Push("WARN", "discord", "no channel ID for notification: "+text)
		},
	})

	chatQ.Push("WARN", "startup", "skills dir not found")
	d.MaybeFire()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("dispatch calls = %d, want 1", got)
	}

	if chatQ.Pending() {
		t.Error("chatQ should be empty — re-entrant push must be suppressed")
	}
	if agentQ.Pending() {
		t.Error("agentQ should be empty — peer queue push during dispatch must be suppressed")
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

// memTimeStore is an in-memory TimeStore standing in for the agent_metadata
// row (session.PersistedTime) that outlives the process.
type memTimeStore struct {
	mu sync.Mutex
	t  time.Time
}

func (m *memTimeStore) Load() (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t, !m.t.IsZero()
}

func (m *memTimeStore) Save(t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t = t
	return nil
}

// #2026: the dispatch cadence survives a restart. A dispatch just before the
// restart must still throttle the first pending warning after it; before, a
// fresh Dispatcher's zero lastDispatch let it through immediately.
func TestDispatcher_CadenceSurvivesRestart(t *testing.T) {
	store := &memTimeStore{}
	var mu sync.Mutex
	calls := 0
	boot := func() (*Dispatcher, *Queue) {
		q := NewQueue(0, 0)
		return NewDispatcher(DispatcherConfig{
			Queue:             q,
			ActiveInterval:    5 * time.Minute,
			InactiveInterval:  time.Hour,
			ActivityThreshold: 10 * time.Minute,
			LastDispatchStore: store,
			DispatchFn: func(string) {
				mu.Lock()
				calls++
				mu.Unlock()
			},
		}), q
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}

	d1, q1 := boot()
	q1.Push("WARN", "test", "before restart")
	d1.MaybeFire()
	waitDispatched(t, d1)
	if count() != 1 {
		t.Fatalf("premise: first dispatch should fire, calls=%d", count())
	}
	if _, ok := store.Load(); !ok {
		t.Fatal("dispatch did not persist its time")
	}

	d2, q2 := boot()
	q2.Push("WARN", "test", "after restart")
	d2.MaybeFire()
	waitDispatched(t, d2)
	if count() != 1 {
		t.Errorf("dispatch after restart fired (calls=%d), want throttled by the 1h inactive interval", count())
	}

	// Control: once the persisted dispatch is older than the interval, the
	// restarted dispatcher fires.
	_ = store.Save(time.Now().Add(-2 * time.Hour))
	d3, q3 := boot()
	q3.Push("WARN", "test", "much later")
	d3.MaybeFire()
	waitDispatched(t, d3)
	if count() != 2 {
		t.Errorf("calls=%d, want 2 (persisted dispatch 2h old, interval 1h)", count())
	}
}

// waitDispatched waits for an in-flight dispatch goroutine to finish.
func waitDispatched(t *testing.T, d *Dispatcher) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		busy := d.dispatching
		d.mu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("dispatch goroutine did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}
