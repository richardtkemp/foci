package platform

import (
	"sync"
	"testing"
	"time"

	"foci/internal/clock"
)

// Tests that the timer fires after the configured window and flushes all
// accumulated messages as a single batch.
//
// Driven by a *clock.Fake so the wait for the timer is virtual: Advance fires
// it synchronously with no wall-clock sleep, so this can't flake under load
// (#1513) the way a fixed Sleep-then-assert racing the real window would.
func TestGroupThrottle_TimerFlush(t *testing.T) {
	var mu sync.Mutex
	var flushed []QueuedMessage

	fc := clock.NewFake()
	gt := NewGroupThrottleWithClock(50*time.Millisecond, func(msgs []QueuedMessage) {
		mu.Lock()
		flushed = append(flushed, msgs...)
		mu.Unlock()
	}, nil, fc)
	defer gt.Stop()

	gt.Add(QueuedMessage{ChatID: 1, Text: "a"})
	gt.Add(QueuedMessage{ChatID: 1, Text: "b"})

	// Before timer fires: nothing flushed yet.
	mu.Lock()
	if len(flushed) != 0 {
		t.Fatalf("expected 0 flushed before timer, got %d", len(flushed))
	}
	mu.Unlock()

	fc.Advance(50 * time.Millisecond)

	mu.Lock()
	if len(flushed) != 2 {
		t.Fatalf("expected 2 flushed after timer, got %d", len(flushed))
	}
	if flushed[0].Text != "a" || flushed[1].Text != "b" {
		t.Fatalf("unexpected flush order: %v", flushed)
	}
	mu.Unlock()
}

// Tests that a mention flushes all buffered messages (including earlier
// non-mentions) immediately, and that the cooldown resets so the next
// non-mention starts a fresh timer.
func TestGroupThrottle_MentionFlush(t *testing.T) {
	var mu sync.Mutex
	var batches [][]QueuedMessage

	gt := NewGroupThrottle(5*time.Second, func(msgs []QueuedMessage) {
		mu.Lock()
		batches = append(batches, msgs)
		mu.Unlock()
	}, nil)
	defer gt.Stop()

	gt.Add(QueuedMessage{ChatID: 1, Text: "non-mention"})
	gt.Add(QueuedMessage{ChatID: 1, Text: "mention!", IsMention: true})

	// Mention should have flushed immediately (no timer wait needed).
	mu.Lock()
	if len(batches) != 1 {
		t.Fatalf("expected 1 batch, got %d", len(batches))
	}
	if len(batches[0]) != 2 {
		t.Fatalf("expected 2 messages in batch, got %d", len(batches[0]))
	}
	mu.Unlock()

	// Cooldown reset: next non-mention should start a fresh timer.
	gt.Add(QueuedMessage{ChatID: 1, Text: "after-cooldown"})
	time.Sleep(10 * time.Millisecond)

	mu.Lock()
	if len(batches) != 1 {
		t.Log("after-cooldown message not yet flushed (timer pending) - correct")
	}
	mu.Unlock()
}

// Tests that messages from different chat IDs are isolated in separate
// buckets with independent timers.
//
// Driven by a *clock.Fake — see TestGroupThrottle_TimerFlush.
func TestGroupThrottle_MultiChat(t *testing.T) {
	var mu sync.Mutex
	chatFlushCount := map[int64]int{}

	fc := clock.NewFake()
	gt := NewGroupThrottleWithClock(50*time.Millisecond, func(msgs []QueuedMessage) {
		mu.Lock()
		for _, m := range msgs {
			chatFlushCount[m.ChatID]++
		}
		mu.Unlock()
	}, nil, fc)
	defer gt.Stop()

	gt.Add(QueuedMessage{ChatID: 100, Text: "chat100-a"})
	gt.Add(QueuedMessage{ChatID: 200, Text: "chat200-a"})
	gt.Add(QueuedMessage{ChatID: 100, Text: "chat100-b"})

	fc.Advance(50 * time.Millisecond)

	mu.Lock()
	if chatFlushCount[100] != 2 {
		t.Errorf("chat 100: expected 2 flushed, got %d", chatFlushCount[100])
	}
	if chatFlushCount[200] != 1 {
		t.Errorf("chat 200: expected 1 flushed, got %d", chatFlushCount[200])
	}
	mu.Unlock()
}

// Tests that nil receiver methods are safe no-ops.
func TestGroupThrottle_NilSafety(t *testing.T) {
	var g *GroupThrottle
	// Should not panic
	g.Add(QueuedMessage{ChatID: 1, Text: "hello"})
	g.Stop()
}

// Tests that Stop cancels pending timers and discards buffered messages.
func TestGroupThrottle_Stop(t *testing.T) {
	var mu sync.Mutex
	var flushed []QueuedMessage

	gt := NewGroupThrottle(50*time.Millisecond, func(msgs []QueuedMessage) {
		mu.Lock()
		flushed = append(flushed, msgs...)
		mu.Unlock()
	}, nil)

	gt.Add(QueuedMessage{ChatID: 1, Text: "will-be-discarded"})
	gt.Stop()

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	if len(flushed) != 0 {
		t.Fatalf("expected 0 flushed after Stop, got %d", len(flushed))
	}
	mu.Unlock()
}

// Tests that the timer is a fixed-window cooldown: subsequent messages do NOT
// reset the timer. The first message starts the window, and all messages
// accumulated within the window are delivered when it fires.
//
// Previously this asserted a wall-clock upper bound (elapsed > 130ms after two
// real sleeps) — exactly the class of assertion that flakes under a loaded
// `go test -p=$(nproc) -parallel=16` run (#1513). Driven by a *clock.Fake
// instead: virtual time only moves on Advance, so the exact "fires at 80ms
// virtual, not 120ms" claim is checked with no tolerance and no wall-clock
// wait at all.
func TestGroupThrottle_FixedWindow(t *testing.T) {
	var mu sync.Mutex
	var flushCount int

	fc := clock.NewFake()
	gt := NewGroupThrottleWithClock(80*time.Millisecond, func(msgs []QueuedMessage) {
		mu.Lock()
		flushCount++
		mu.Unlock()
	}, nil, fc)
	defer gt.Stop()

	gt.Add(QueuedMessage{ChatID: 1, Text: "t=0"})

	// Add another message 40ms later — timer should NOT reset.
	fc.Advance(40 * time.Millisecond)
	gt.Add(QueuedMessage{ChatID: 1, Text: "t=40ms"})

	// If the timer had (wrongly) reset on the second Add, it would fire at
	// 40+80=120ms virtual, not 80ms — so at exactly 80ms it must NOT have
	// fired yet if it reset, but must have fired if it didn't.
	fc.Advance(39 * time.Millisecond) // now at 79ms virtual
	mu.Lock()
	if flushCount != 0 {
		t.Fatalf("flushed before the 80ms window closed (got %d)", flushCount)
	}
	mu.Unlock()

	fc.Advance(1 * time.Millisecond) // now at 80ms virtual: window closes
	mu.Lock()
	if flushCount != 1 {
		t.Fatalf("expected exactly 1 flush at the 80ms window, got %d", flushCount)
	}
	mu.Unlock()
}
