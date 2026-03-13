package agent

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRateLimitGate_NotLimitedByDefault(t *testing.T) {
	// Proves that a zero-value RateLimitGate is not limited.
	var g RateLimitGate
	limited, _ := g.IsLimited()
	if limited {
		t.Error("new gate should not be limited")
	}
}

func TestRateLimitGate_CloseAndIsLimited(t *testing.T) {
	// Proves that Close() marks the gate as limited until the given time, which IsLimited reports correctly.
	var g RateLimitGate
	until := time.Now().Add(1 * time.Hour)
	g.Close(until)

	limited, got := g.IsLimited()
	if !limited {
		t.Error("gate should be limited after Close")
	}
	if !got.Equal(until) {
		t.Errorf("until = %v, want %v", got, until)
	}
}

func TestRateLimitGate_ExpiredNotLimited(t *testing.T) {
	// Proves that a gate whose limit time has passed is no longer considered limited.
	var g RateLimitGate
	g.Close(time.Now().Add(-1 * time.Second))

	limited, _ := g.IsLimited()
	if limited {
		t.Error("gate should not be limited after expiry")
	}
}

func TestRateLimitGate_EnqueueAndDrain(t *testing.T) {
	// Proves that queued items are held while the gate is limited and released in order after expiry.
	var g RateLimitGate
	g.Close(time.Now().Add(1 * time.Hour))

	g.Enqueue("session1", "hello", "user")
	g.Enqueue("session2", "keepalive", "keepalive")

	// Drain should return nil while still limited
	items := g.DrainQueue()
	if items != nil {
		t.Errorf("drain while limited should return nil, got %d items", len(items))
	}

	// Move gate to expired
	g.Close(time.Now().Add(-1 * time.Second))

	items = g.DrainQueue()
	if len(items) != 2 {
		t.Fatalf("drain after expiry should return 2 items, got %d", len(items))
	}
	if items[0].SessionKey != "session1" || items[0].Message != "hello" || items[0].Trigger != "user" {
		t.Errorf("item[0] = %+v", items[0])
	}
	if items[1].SessionKey != "session2" || items[1].Message != "keepalive" || items[1].Trigger != "keepalive" {
		t.Errorf("item[1] = %+v", items[1])
	}

	// Second drain should be empty
	items = g.DrainQueue()
	if items != nil {
		t.Errorf("second drain should be nil, got %d items", len(items))
	}
}

func TestRateLimitGate_DrainClearsGate(t *testing.T) {
	// Proves that DrainQueue clears the gate's limited state so subsequent calls show it unlocked.
	var g RateLimitGate
	g.Close(time.Now().Add(-1 * time.Millisecond))
	g.Enqueue("s1", "msg", "user")

	g.DrainQueue()

	// Gate should be clear now
	limited, _ := g.IsLimited()
	if limited {
		t.Error("gate should be cleared after drain")
	}
}

func TestRateLimitGate_ConcurrentAccess(t *testing.T) {
	// Proves that concurrent Enqueue and IsLimited calls are race-condition-free and all items are preserved.
	var g RateLimitGate
	g.Close(time.Now().Add(100 * time.Millisecond))

	var wg sync.WaitGroup
	// Concurrent enqueues
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.Enqueue("session", "msg", "user")
		}()
	}
	// Concurrent IsLimited checks
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.IsLimited()
		}()
	}
	wg.Wait()

	// Wait for gate to expire, then drain
	time.Sleep(150 * time.Millisecond)
	items := g.DrainQueue()
	if len(items) != 50 {
		t.Errorf("expected 50 queued items, got %d", len(items))
	}
}

func TestRateLimitedError_Message(t *testing.T) {
	// Proves that RateLimitedError.Error() produces a human-readable message with "rate limited" and reset info.
	until := time.Now().Add(2 * time.Hour)
	err := &RateLimitedError{Until: until}
	msg := err.Error()
	if !strings.Contains(msg, "rate limited") {
		t.Errorf("error should contain 'rate limited', got: %s", msg)
	}
	if !strings.Contains(msg, "resets") {
		t.Errorf("error should contain 'resets', got: %s", msg)
	}
}

func TestRateLimitGate_DrainEmptyQueueNoItems(t *testing.T) {
	// Proves that DrainQueue on an empty, non-limited gate returns nil without panicking.
	var g RateLimitGate
	// Not limited, empty queue
	items := g.DrainQueue()
	if items != nil {
		t.Errorf("drain on empty gate should return nil, got %d items", len(items))
	}
}
