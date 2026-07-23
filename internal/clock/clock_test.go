package clock

import (
	"testing"
	"time"
)

func TestFakeAdvanceFiresDueTimer(t *testing.T) {
	c := NewFake()
	fired := false
	c.AfterFunc(10*time.Millisecond, func() { fired = true })

	c.Advance(9 * time.Millisecond)
	if fired {
		t.Fatal("timer fired before its deadline")
	}
	c.Advance(1 * time.Millisecond)
	if !fired {
		t.Fatal("timer did not fire at its deadline")
	}
}

func TestFakeAdvanceOrdersMultipleTimers(t *testing.T) {
	c := NewFake()
	var order []int
	c.AfterFunc(30*time.Millisecond, func() { order = append(order, 3) })
	c.AfterFunc(10*time.Millisecond, func() { order = append(order, 1) })
	c.AfterFunc(20*time.Millisecond, func() { order = append(order, 2) })

	c.Advance(30 * time.Millisecond)
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("fire order = %v, want [1 2 3]", order)
	}
}

func TestFakeResetExtendsDeadline(t *testing.T) {
	c := NewFake()
	fired := false
	timer := c.AfterFunc(10*time.Millisecond, func() { fired = true })

	c.Advance(9 * time.Millisecond)
	if !timer.Reset(10 * time.Millisecond) {
		t.Fatal("Reset() = false, want true (timer was still armed)")
	}
	// The original deadline (10ms) has now passed, but Reset pushed it to
	// 9ms+10ms=19ms, so advancing only to the original deadline must not fire.
	c.Advance(1 * time.Millisecond) // now at 10ms
	if fired {
		t.Fatal("timer fired at the pre-Reset deadline")
	}
	c.Advance(9 * time.Millisecond) // now at 19ms
	if !fired {
		t.Fatal("timer did not fire at the post-Reset deadline")
	}
}

func TestFakeStopPreventsFire(t *testing.T) {
	c := NewFake()
	fired := false
	timer := c.AfterFunc(10*time.Millisecond, func() { fired = true })

	if !timer.Stop() {
		t.Fatal("Stop() = false, want true (timer was armed)")
	}
	c.Advance(time.Hour)
	if fired {
		t.Fatal("stopped timer fired")
	}
	if timer.Stop() {
		t.Fatal("second Stop() = true, want false (already stopped)")
	}
}

func TestFakeNowAdvancesByExactDelta(t *testing.T) {
	c := NewFake()
	start := c.Now()
	c.Advance(5 * time.Second)
	if got := c.Now().Sub(start); got != 5*time.Second {
		t.Fatalf("Now() advanced by %v, want 5s", got)
	}
}

func TestRealClockUsesWallTime(t *testing.T) {
	c := Real()
	before := time.Now()
	got := c.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("Real().Now() = %v, want between %v and %v", got, before, after)
	}
}
