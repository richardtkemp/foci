package provider

import (
	"context"
	"testing"
	"time"

	"foci/internal/clock"
)

// TestStreamIdleWatchdogFiresOnStall proves a stream that goes silent past the
// idle window is cancelled and Fired() reports the idle timeout as the cause.
//
// Driven by a *clock.Fake so the "silence" is virtual: Advance fires the timer
// synchronously with no wall-clock wait, so this can never flake under load
// (#1513).
func TestStreamIdleWatchdogFiresOnStall(t *testing.T) {
	fc := clock.NewFake()
	ctx, wd := NewStreamIdleWatchdogWithClock(context.Background(), 20*time.Millisecond, fc)
	defer wd.Stop()

	if ctx.Err() != nil {
		t.Fatal("context cancelled before the idle window elapsed")
	}
	fc.Advance(20 * time.Millisecond)
	if ctx.Err() == nil {
		t.Fatal("watchdog did not fire on stall")
	}
	if !wd.Fired() {
		t.Error("Fired() = false, want true after idle cancel")
	}
}

// TestStreamIdleWatchdogResetKeepsAlive proves that a stream which keeps
// receiving chunks (Reset called repeatedly) is NOT cancelled even though the
// total duration far exceeds a single idle window — the fix for long
// progressing streams.
//
// Driven by a *clock.Fake: the loop advances virtual time by 10ms and resets,
// 20 times (5x a single 40ms idle window), asserting the context stays live
// at every step. No time.Sleep is involved, so there is no wall-clock margin
// to blow under a loaded `go test -p=$(nproc) -parallel=16` run (#1513).
func TestStreamIdleWatchdogResetKeepsAlive(t *testing.T) {
	fc := clock.NewFake()
	ctx, wd := NewStreamIdleWatchdogWithClock(context.Background(), 40*time.Millisecond, fc)
	defer wd.Stop()
	// Pump resets every 10ms (virtual) for 200ms — 5x a single idle window.
	for i := 0; i < 20; i++ {
		fc.Advance(10 * time.Millisecond)
		wd.Reset()
		if ctx.Err() != nil {
			t.Fatalf("context cancelled while still progressing (iter %d)", i)
		}
	}
	if wd.Fired() {
		t.Error("Fired() = true, want false while progressing")
	}
}

// TestStreamIdleWatchdogDisabled proves idle <= 0 disables the timeout: the
// context stays live indefinitely until Stop.
func TestStreamIdleWatchdogDisabled(t *testing.T) {
	fc := clock.NewFake()
	ctx, wd := NewStreamIdleWatchdogWithClock(context.Background(), 0, fc)
	fc.Advance(time.Hour)
	if ctx.Err() != nil {
		t.Fatal("disabled watchdog should not cancel on its own")
	}
	if wd.Fired() {
		t.Error("Fired() = true, want false when disabled")
	}
	wd.Stop()
	if ctx.Err() == nil {
		t.Error("Stop should cancel the context")
	}
}

// TestStreamIdleWatchdogParentCancel proves that parent cancellation propagates
// but is not misreported as an idle timeout.
func TestStreamIdleWatchdogParentCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx, wd := NewStreamIdleWatchdog(parent, time.Hour)
	defer wd.Stop()
	cancel()
	<-ctx.Done()
	if wd.Fired() {
		t.Error("Fired() = true on parent cancel, want false (not an idle timeout)")
	}
}
