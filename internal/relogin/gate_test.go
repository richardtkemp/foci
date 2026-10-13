package relogin

import (
	"testing"
	"time"
)

func TestGateSingleFlight(t *testing.T) {
	g := &Gate{}
	if !g.Start() {
		t.Fatal("first Start should claim the gate")
	}
	if g.Start() {
		t.Fatal("second Start should fail while active")
	}
	if !g.Active() {
		t.Fatal("gate should be active")
	}
	g.Release()
	if g.Active() {
		t.Fatal("gate should be inactive after Release")
	}
	if !g.Start() {
		t.Fatal("Start should succeed again after Release")
	}
}

func TestGateCaptureWindow(t *testing.T) {
	g := &Gate{}
	g.Start()
	defer g.Release()

	// No capture window open yet.
	if g.ShouldCapture("clutch") {
		t.Fatal("should not capture before OpenCapture")
	}
	g.OpenCapture("clutch")
	if !g.ShouldCapture("clutch") {
		t.Fatal("should capture for the opened agent")
	}
	if g.ShouldCapture("scout") {
		t.Fatal("should not capture for a different agent")
	}

	// Submitting a code delivers it and closes the window.
	g.SubmitCode("abc123")
	if g.ShouldCapture("clutch") {
		t.Fatal("capture window should close after SubmitCode")
	}
	code, ok := g.AwaitCode(time.Second)
	if !ok || code != "abc123" {
		t.Fatalf("AwaitCode = %q,%v; want abc123,true", code, ok)
	}
}

func TestGateAwaitCodeTimeout(t *testing.T) {
	g := &Gate{}
	g.Start()
	defer g.Release()
	_, ok := g.AwaitCode(10 * time.Millisecond)
	if ok {
		t.Fatal("AwaitCode should time out when no code submitted")
	}
}

func TestGateInactiveNoCapture(t *testing.T) {
	g := &Gate{}
	// Never started.
	if g.Active() {
		t.Fatal("fresh gate should be inactive")
	}
	if g.ShouldCapture("clutch") {
		t.Fatal("inactive gate should never capture")
	}
	// SubmitCode on inactive gate is a no-op (no panic on nil channel).
	g.SubmitCode("x")
}

// TestGateReleased: Released blocks while a re-login is active, closes on
// Release, and is already closed when no re-login is in progress — so a waiter
// that raced a Release never blocks forever.
func TestGateReleased(t *testing.T) {
	g := &Gate{}
	select {
	case <-g.Released():
	default:
		t.Fatal("Released on an inactive gate should be closed")
	}

	g.Start()
	ch := g.Released()
	select {
	case <-ch:
		t.Fatal("Released should block while active")
	default:
	}
	g.Release()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("Released channel not closed by Release")
	}
	g.Release() // idempotent: must not double-close
}

// TestGatePending (#2384): the pending state outlives a failed attempt and
// ends only on Complete, or ClearPendingIf for the attempt that is still current.
func TestGatePending(t *testing.T) {
	g := &Gate{}
	var completed int
	g.OnComplete(func() { completed++ })
	if g.Pending() {
		t.Fatal("fresh gate should not be pending")
	}
	g.Start()
	g.Release() // aborted attempt
	if !g.Pending() {
		t.Fatal("a failed attempt must leave the login pending")
	}
	attempt, pending := g.PendingAttempt()
	if !pending {
		t.Fatal("PendingAttempt should report pending")
	}
	g.Start() // a new attempt began after the read
	if g.ClearPendingIf(attempt) {
		t.Fatal("ClearPendingIf must not clear while an attempt is running")
	}
	g.Release()
	if g.ClearPendingIf(attempt) {
		t.Fatal("ClearPendingIf must not clear for a stale attempt number")
	}
	attempt, _ = g.PendingAttempt()
	if !g.ClearPendingIf(attempt) || g.Pending() {
		t.Fatal("ClearPendingIf should clear for the current attempt with none running")
	}
	if completed != 1 {
		t.Fatalf("OnComplete ran %d times, want 1", completed)
	}
	g.Start()
	g.Complete()
	g.Complete() // second call is a no-op
	g.Release()
	if g.Pending() || completed != 2 {
		t.Fatalf("after Complete: pending=%v completed=%d, want false,2", g.Pending(), completed)
	}
}
