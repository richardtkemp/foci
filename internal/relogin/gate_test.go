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
