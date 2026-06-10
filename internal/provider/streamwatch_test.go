package provider

import (
	"context"
	"testing"
	"time"
)

// TestStreamIdleWatchdogFiresOnStall proves a stream that goes silent past the
// idle window is cancelled and Fired() reports the idle timeout as the cause.
func TestStreamIdleWatchdogFiresOnStall(t *testing.T) {
	ctx, wd := NewStreamIdleWatchdog(context.Background(), 20*time.Millisecond)
	defer wd.Stop()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
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
func TestStreamIdleWatchdogResetKeepsAlive(t *testing.T) {
	ctx, wd := NewStreamIdleWatchdog(context.Background(), 40*time.Millisecond)
	defer wd.Stop()
	// Pump resets every 10ms for 200ms — 5x a single idle window.
	for i := 0; i < 20; i++ {
		time.Sleep(10 * time.Millisecond)
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
	ctx, wd := NewStreamIdleWatchdog(context.Background(), 0)
	time.Sleep(30 * time.Millisecond)
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
