package log

import (
	"sync"
	"testing"
	"time"
)

// testClock is a hand-wound time source. The escalation schedule is the whole
// contract here, so it has to be asserted by advancing time deterministically
// rather than by sleeping through a 30-minute cap.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// A condition is never born throttled: the first occurrence must be visible
// immediately. A limiter that made the caller wait out one interval before the
// first line would delay every alarm by its own cooldown.
func TestWarnLimiter_FirstOccurrenceEmits(t *testing.T) {
	clk := newTestClock()
	l := NewWarnLimiterWithClock(time.Minute, 30*time.Minute, clk.Now)

	emit, suppressed := l.Allow("k")
	if !emit {
		t.Fatal("first occurrence was suppressed; a new condition must warn immediately")
	}
	if suppressed != 0 {
		t.Errorf("suppressed = %d on the first occurrence, want 0", suppressed)
	}
}

// The core behaviour: repeats inside the window are dropped, and the count of
// what was dropped is handed back so the next emitted line can say so.
func TestWarnLimiter_SuppressesWithinWindowAndReportsCount(t *testing.T) {
	clk := newTestClock()
	l := NewWarnLimiterWithClock(time.Minute, 30*time.Minute, clk.Now)

	l.Allow("k") // opens the window
	for i := 0; i < 9; i++ {
		clk.Advance(5 * time.Second)
		if emit, _ := l.Allow("k"); emit {
			t.Fatalf("occurrence %d emitted inside the suppression window", i+2)
		}
	}

	clk.Advance(time.Minute) // past the window
	emit, suppressed := l.Allow("k")
	if !emit {
		t.Fatal("did not emit once the suppression window had elapsed")
	}
	if suppressed != 9 {
		t.Errorf("suppressed = %d, want 9 — a reader who cannot see that lines were dropped cannot tell quiet from broken", suppressed)
	}

	// The count must clear, or every later line would over-report.
	clk.Advance(time.Hour)
	if _, again := l.Allow("k"); again != 0 {
		t.Errorf("suppressed = %d on the next emit, want 0 (count did not clear)", again)
	}
}

// The interval doubles and then holds at max. This is what makes a long outage
// cost a handful of lines instead of one per retry, without muting the early
// minutes when the information is still new.
func TestWarnLimiter_EscalatesAndCaps(t *testing.T) {
	clk := newTestClock()
	base, max := time.Minute, 8*time.Minute
	l := NewWarnLimiterWithClock(base, max, clk.Now)

	l.Allow("k") // emit 1; window now base

	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 8 * time.Minute} {
		// Just short of the expected window: must still be suppressed.
		clk.Advance(want - time.Second)
		if emit, _ := l.Allow("k"); emit {
			t.Fatalf("emitted after %s, want suppression until %s", want-time.Second, want)
		}
		clk.Advance(time.Second)
		if emit, _ := l.Allow("k"); !emit {
			t.Fatalf("did not emit after the %s window elapsed", want)
		}
	}
}

// base == max is the fixed-cooldown case (internal/log's stale-inode warning).
// It must not escalate, because that warning bounds a recursion hazard and its
// ceiling has to stay predictable.
func TestWarnLimiter_FixedIntervalWhenBaseEqualsMax(t *testing.T) {
	clk := newTestClock()
	l := NewWarnLimiterWithClock(time.Minute, time.Minute, clk.Now)

	for i := 0; i < 5; i++ {
		if emit, _ := l.Allow("k"); !emit {
			t.Fatalf("emit %d suppressed; a fixed interval must not escalate", i+1)
		}
		clk.Advance(time.Minute)
	}
}

// The property that keeps an INTERMITTENT fault loud: each new episode starts
// from scratch. Without the reset, a flapping condition would inherit the
// silence earned by the previous episode and be progressively muted exactly as
// it got worse — which is the failure mode throttling must not introduce.
func TestWarnLimiter_ResetRestartsEscalation(t *testing.T) {
	clk := newTestClock()
	l := NewWarnLimiterWithClock(time.Minute, 30*time.Minute, clk.Now)

	// Escalate well past base.
	l.Allow("k")
	for _, d := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		clk.Advance(d)
		l.Allow("k")
	}

	l.Reset("k") // condition cleared

	if emit, suppressed := l.Allow("k"); !emit || suppressed != 0 {
		t.Fatalf("after Reset: emit=%v suppressed=%d, want an immediate clean emit", emit, suppressed)
	}
	// And the window is back to base, not the escalated value.
	clk.Advance(time.Minute)
	if emit, _ := l.Allow("k"); !emit {
		t.Error("window did not return to base after Reset — a new episode inherited the old silence")
	}
}

// One limiter serves several independent conditions; throttling one must never
// silence another. internal/log relies on this to key by log path.
func TestWarnLimiter_KeysAreIndependent(t *testing.T) {
	clk := newTestClock()
	l := NewWarnLimiterWithClock(time.Minute, 30*time.Minute, clk.Now)

	if emit, _ := l.Allow("a"); !emit {
		t.Fatal("first occurrence of key a suppressed")
	}
	if emit, _ := l.Allow("b"); !emit {
		t.Fatal("key b was suppressed by key a's window — keys are not independent")
	}
	if emit, _ := l.Allow("a"); emit {
		t.Error("key a emitted twice inside one window")
	}
}

// A nil limiter means "unthrottled", never "silent". Getting this backwards
// would turn an unconfigured limiter into a silent alarm.
func TestWarnLimiter_NilAllowsEverything(t *testing.T) {
	var l *WarnLimiter
	for i := 0; i < 3; i++ {
		if emit, _ := l.Allow("k"); !emit {
			t.Fatal("nil limiter suppressed a warning; nil must mean unthrottled")
		}
	}
	l.Reset("k") // must not panic
}
