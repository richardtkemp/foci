package opencode

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
)

const (
	abortPath  = "/session/sess-inject/abort"
	promptPath = "/session/sess-inject/prompt_async"
)

func waitForPrompts(t *testing.T, rec *recordingHandler, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for rec.countPath(promptPath) < want {
		if time.Now().After(deadline) {
			t.Fatalf("/prompt_async POSTs = %d, want %d", rec.countPath(promptPath), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// steerUntilBackstopFlush runs a steer whose abort burst has not arrived when
// the backstop fires, so the fresh turn is in flight when the test returns.
func steerUntilBackstopFlush(t *testing.T) (*Backend, *recordingHandler, *atomic.Int32) {
	t.Helper()
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)
	b.abortDrainTimeout = time.Millisecond
	var completions atomic.Int32
	b.beginTurn(&delegator.TurnEvents{
		OnTurnComplete: func(*delegator.TurnResult) { completions.Add(1) },
	})
	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer, Text: "steer",
	}); err != nil {
		t.Fatalf("Inject steer: %v", err)
	}
	waitForPrompts(t, rec, 1)
	return b, rec, &completions
}

// #2845: real opencode publishes the abort's session.idle AFTER prompt_async
// has returned for the follow-up, and before the follow-up goes busy. That
// idle belongs to the aborted turn.
func TestSteerBackstop_LateBurstIdle_DoesNotEndFreshTurn(t *testing.T) {
	b, _, _ := steerUntilBackstopFlush(t)

	b.onSessionIdle(b.sessionID)
	if !b.IsTurnInFlight() {
		t.Fatal("a late burst idle ended the fresh turn: it is no longer in flight")
	}

	// The fresh turn's own busy and idle still end it.
	b.onSessionStatus(b.sessionID, SessionStatus{Type: StatusBusy})
	if !b.IsTurnInFlight() {
		t.Fatal("busy ended the fresh turn")
	}
	b.onSessionIdle(b.sessionID)
	if b.IsTurnInFlight() {
		t.Error("the fresh turn's own idle did not end it")
	}
}

func TestSteerBackstop_LateAbortError_DoesNotEndFreshTurn(t *testing.T) {
	b, _, _ := steerUntilBackstopFlush(t)

	b.onSessionError(b.sessionID, &MessageError{Name: ErrMessageAborted})
	if !b.IsTurnInFlight() {
		t.Fatal("a late abort error ended the fresh turn")
	}

	// Any other error before busy is about the fresh turn: it must end it.
	b.onSessionError(b.sessionID, &MessageError{Name: "UnknownError"})
	if b.IsTurnInFlight() {
		t.Error("a real session error did not end the fresh turn")
	}
}

// When the backstop wins, no event has completed the aborted turn. The flush
// must not overwrite its TurnEvents without completing them.
func TestSteerBackstop_CompletesAbortedTurnOnce(t *testing.T) {
	b, _, completions := steerUntilBackstopFlush(t)

	b.onSessionError(b.sessionID, &MessageError{Name: ErrMessageAborted})
	b.onSessionIdle(b.sessionID)
	if got := completions.Load(); got != 1 {
		t.Errorf("aborted turn OnTurnComplete fired %d times, want 1", got)
	}
}

// The first burst idle arrives, then the backstop wins, then the second idle:
// the order the 500ms timer and opencode's two-idle burst can produce.
func TestSteerBackstop_BetweenBurstIdles_SecondIdleIgnored(t *testing.T) {
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)
	b.abortDrainTimeout = time.Hour
	var completions atomic.Int32
	b.beginTurn(&delegator.TurnEvents{
		OnTurnComplete: func(*delegator.TurnResult) { completions.Add(1) },
	})
	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer, Text: "steer",
	}); err != nil {
		t.Fatalf("Inject steer: %v", err)
	}
	b.onSessionIdle(b.sessionID)
	b.abortDrainComplete() // the backstop's call
	waitForPrompts(t, rec, 1)

	b.onSessionIdle(b.sessionID)
	if !b.IsTurnInFlight() {
		t.Error("the burst's second idle ended the fresh turn")
	}
	if got := completions.Load(); got != 1 {
		t.Errorf("aborted turn OnTurnComplete fired %d times, want 1", got)
	}
}

// A full burst leaves no idle outstanding, so the next idle ends the fresh
// turn with no busy needed (control: the guard is only for the backstop).
func TestSteerDrain_FullBurst_NextIdleEndsFreshTurn(t *testing.T) {
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)
	b.abortDrainTimeout = time.Hour
	b.beginTurn(&delegator.TurnEvents{})
	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer, Text: "steer",
	}); err != nil {
		t.Fatalf("Inject steer: %v", err)
	}
	b.onSessionIdle(b.sessionID)
	b.onSessionIdle(b.sessionID)
	waitForPrompts(t, rec, 1)
	b.onSessionIdle(b.sessionID)
	if b.IsTurnInFlight() {
		t.Error("the fresh turn's idle did not end it")
	}
}

// A user stop in the window before busy is a real abort: its idle must end the
// fresh turn, or the session stays in flight with nothing running.
func TestSteerBackstop_InterruptBeforeBusy_NextIdleEndsFreshTurn(t *testing.T) {
	b, _, _ := steerUntilBackstopFlush(t)

	if err := b.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	b.onSessionIdle(b.sessionID)
	if b.IsTurnInFlight() {
		t.Error("the idle of a user abort did not end the fresh turn")
	}
}

// Real opencode discards a prompt_async that reaches it before /abort: the user
// message is stored and no model call follows (probed on 1.17.15). So the
// backstop must not start counting until /abort has returned.
func TestSteerBackstop_NotArmedWhileAbortInFlight(t *testing.T) {
	rec := &recordingHandler{}
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	rec.onRequest = func(path string) int {
		if path == abortPath {
			once.Do(func() { close(entered) })
			<-release
		}
		return 0
	}
	b := newReadyBackend(t, rec)
	b.abortDrainTimeout = time.Millisecond
	b.beginTurn(&delegator.TurnEvents{})

	injected := make(chan error, 1)
	go func() {
		injected <- b.ImmediateInject(context.Background(), delegator.Inject{
			Source: delegator.SourceSteer, Text: "steer",
		})
	}()
	<-entered
	time.Sleep(50 * time.Millisecond) // many backstop periods
	if got := rec.countPath(promptPath); got != 0 {
		t.Errorf("/prompt_async POSTs while /abort is in flight = %d, want 0", got)
	}
	close(release)
	if err := <-injected; err != nil {
		t.Fatalf("Inject steer: %v", err)
	}
	waitForPrompts(t, rec, 1)
}

// The burst can finish before POST /abort returns. The backstop must not be
// armed after that: it would have nothing to do, and aborting must stay clear.
func TestSteerDrain_BurstBeforeAbortReturns_NoBackstopArmed(t *testing.T) {
	rec := &recordingHandler{}
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	rec.onRequest = func(path string) int {
		if path == abortPath {
			once.Do(func() { close(entered) })
			<-release
		}
		return 0
	}
	b := newReadyBackend(t, rec)
	b.abortDrainTimeout = time.Millisecond
	b.beginTurn(&delegator.TurnEvents{})

	injected := make(chan error, 1)
	go func() {
		injected <- b.ImmediateInject(context.Background(), delegator.Inject{
			Source: delegator.SourceSteer, Text: "steer",
		})
	}()
	<-entered
	b.onSessionIdle(b.sessionID)
	b.onSessionIdle(b.sessionID)
	waitForPrompts(t, rec, 1)
	close(release)
	if err := <-injected; err != nil {
		t.Fatalf("Inject steer: %v", err)
	}
	b.turnMu.Lock()
	timer, aborting := b.abortTimer, b.aborting
	b.turnMu.Unlock()
	if timer != nil || aborting {
		t.Errorf("after the drain: abortTimer set=%v aborting=%v, want neither", timer != nil, aborting)
	}
}

// A follow-up that never reached opencode cannot go busy or idle. Leaving its
// turn registered would hold the session in flight with nothing running.
func TestSteerBackstop_FlushSendFails_NoTurnLeftInFlight(t *testing.T) {
	rec := &recordingHandler{}
	rec.onRequest = func(path string) int {
		if path == promptPath {
			return http.StatusInternalServerError
		}
		return 0
	}
	b := newReadyBackend(t, rec)
	b.abortDrainTimeout = time.Hour
	b.beginTurn(&delegator.TurnEvents{})
	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer, Text: "steer",
	}); err != nil {
		t.Fatalf("Inject steer: %v", err)
	}
	b.abortDrainComplete()
	if got := rec.countPath(promptPath); got != 1 {
		t.Fatalf("/prompt_async POSTs = %d, want 1", got)
	}
	if b.IsTurnInFlight() {
		t.Error("a follow-up whose send failed is still in flight")
	}
}

// #2845 (1): SendControl writes b.model under b.mu on a control goroutine while
// a prompt goes out on another. Only meaningful under the race detector:
//
//	make local-test-one CGO_ENABLED=1 GOFLAGS=-race PKG=./internal/delegator/opencode/ RUN=TestSendPrompt_ModelRead
//
// Use local-test-one: test-one runs on a build host and does not forward GOFLAGS,
// so there it passes without the detector.
func TestSendPrompt_ModelReadVsSetModel(t *testing.T) {
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)
	b.resolveModelFn = func(_ context.Context, _, _, model string) (string, error) {
		return model, nil
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := b.SendControl(context.Background(), &delegator.SetModelRequest{Model: "p/m"}); err != nil {
				t.Errorf("SendControl: %v", err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := b.sendPrompt(context.Background(), "x", nil, ""); err != nil {
				t.Errorf("sendPrompt: %v", err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := b.sendPromptWithAgent(context.Background(), "x", "plan"); err != nil {
				t.Errorf("sendPromptWithAgent: %v", err)
			}
		}
	}()
	wg.Wait()
}
