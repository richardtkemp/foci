package cctmux

import (
	"context"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
)

// TestFireTurnComplete_PerTurnCallback verifies the per-turn callback fires
// with the correct TurnResult.
func TestFireTurnComplete_PerTurnCallback(t *testing.T) {
	b := &Backend{}

	var got *delegator.TurnResult
	b.turnCompleteMu.Lock()
	b.turnCompleteFn = func(r *delegator.TurnResult) { got = r }
	b.turnCompleteMu.Unlock()

	result := &delegator.TurnResult{
		Text:      "hello",
		ToolCalls: 3,
		Usage: &delegator.TurnUsage{
			InputTokens:  1000,
			OutputTokens: 200,
		},
	}
	b.fireTurnComplete(result)

	if got == nil {
		t.Fatal("per-turn callback was not called")
	}
	if got.Text != "hello" {
		t.Errorf("Text = %q, want %q", got.Text, "hello")
	}
	if got.ToolCalls != 3 {
		t.Errorf("ToolCalls = %d, want 3", got.ToolCalls)
	}
	if got.Usage == nil || got.Usage.InputTokens != 1000 {
		t.Errorf("Usage.InputTokens = %v, want 1000", got.Usage)
	}
}

// TestFireTurnComplete_OneShot verifies the callback is nil'd after first fire
// and a second fire doesn't panic or re-invoke.
func TestFireTurnComplete_OneShot(t *testing.T) {
	b := &Backend{}

	callCount := 0
	b.turnCompleteMu.Lock()
	b.turnCompleteFn = func(r *delegator.TurnResult) { callCount++ }
	b.turnCompleteMu.Unlock()

	b.fireTurnComplete(&delegator.TurnResult{Text: "first"})
	b.fireTurnComplete(&delegator.TurnResult{Text: "second"})

	if callCount != 1 {
		t.Fatalf("callback called %d times, want 1", callCount)
	}

	// turnCompleteFn should be nil.
	b.turnCompleteMu.Lock()
	fn := b.turnCompleteFn
	b.turnCompleteMu.Unlock()
	if fn != nil {
		t.Fatal("turnCompleteFn should be nil after firing")
	}
}

// TestFireTurnComplete_NilCallback verifies no panic when no callback is set.
func TestFireTurnComplete_NilCallback(t *testing.T) {
	b := &Backend{}
	// Should not panic.
	b.fireTurnComplete(&delegator.TurnResult{Text: "no callback"})
}

// TestFireTurnComplete_AlsoSignalsWaitForTurn verifies that fireTurnComplete
// also signals the legacy WaitForTurn channel.
func TestFireTurnComplete_AlsoSignalsWaitForTurn(t *testing.T) {
	b := &Backend{}

	ch := make(chan struct{}, 1)
	b.waitMu.Lock()
	b.waitCh = ch
	b.waitMu.Unlock()

	b.fireTurnComplete(&delegator.TurnResult{})

	select {
	case <-ch:
		// success — WaitForTurn was signalled
	default:
		t.Fatal("fireTurnComplete should also signal waitCh")
	}
}

// TestFireTurnComplete_RaceSafety exercises the mutex under concurrent access.
func TestFireTurnComplete_RaceSafety(t *testing.T) {
	b := &Backend{}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		// Concurrent set.
		go func() {
			defer wg.Done()
			b.turnCompleteMu.Lock()
			b.turnCompleteFn = func(r *delegator.TurnResult) {}
			b.turnCompleteMu.Unlock()
		}()
		// Concurrent fire.
		go func() {
			defer wg.Done()
			b.fireTurnComplete(&delegator.TurnResult{})
		}()
	}
	wg.Wait()
}

// TestFireTurnComplete_StopsTypingIndicator verifies that fireTurnComplete
// calls typingFunc(false) to stop the typing indicator.
func TestFireTurnComplete_StopsTypingIndicator(t *testing.T) {
	b := &Backend{}

	var typingCalls []bool
	b.replyMu.Lock()
	b.typingFunc = func(v bool) { typingCalls = append(typingCalls, v) }
	b.replyMu.Unlock()

	b.fireTurnComplete(&delegator.TurnResult{Text: "done"})

	if len(typingCalls) != 1 {
		t.Fatalf("typingFunc called %d times, want 1", len(typingCalls))
	}
	if typingCalls[0] != false {
		t.Error("typingFunc should be called with false (stop typing)")
	}
}

// --- WaitForTurn tests ---

// TestWaitForTurn_AlreadySignalled verifies that WaitForTurn returns
// immediately when the signal channel has already been written to
// (buffered case — turn completes between SendToPane and WaitForTurn).
func TestWaitForTurn_AlreadySignalled(t *testing.T) {
	b := &Backend{}

	// Pre-signal: simulate a turn completing before WaitForTurn is called.
	// We need to call WaitForTurn and have it pick up a signal that was
	// sent mid-flight. We do this by signalling from a goroutine after
	// a tiny delay.
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		done <- b.WaitForTurn(ctx)
	}()

	// Give WaitForTurn time to set up its channel.
	time.Sleep(10 * time.Millisecond)
	b.notifyTurnComplete()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForTurn returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForTurn did not return after signal")
	}
}

// TestWaitForTurn_ContextCancellation verifies that WaitForTurn returns
// the context error when the context is cancelled before a signal arrives.
func TestWaitForTurn_ContextCancellation(t *testing.T) {
	b := &Backend{}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- b.WaitForTurn(ctx)
	}()

	// Cancel without ever signalling.
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("WaitForTurn returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForTurn did not return after context cancel")
	}
}

// TestWaitForTurn_BlocksUntilSignal verifies that WaitForTurn blocks
// and only returns when notifyTurnComplete is called.
func TestWaitForTurn_BlocksUntilSignal(t *testing.T) {
	b := &Backend{}

	ctx := context.Background()
	returned := make(chan struct{})

	go func() {
		_ = b.WaitForTurn(ctx)
		close(returned)
	}()

	// Verify it's still blocking after a short delay.
	select {
	case <-returned:
		t.Fatal("WaitForTurn returned before signal")
	case <-time.After(50 * time.Millisecond):
		// expected — still blocking
	}

	// Now signal and expect it to return.
	b.notifyTurnComplete()

	select {
	case <-returned:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForTurn did not return after signal")
	}
}

// TestWaitForTurn_CleansUpChannel verifies that waitCh is nil'd after
// WaitForTurn returns, preventing stale channel leaks.
func TestWaitForTurn_CleansUpChannel(t *testing.T) {
	b := &Backend{}

	ctx := context.Background()
	done := make(chan struct{})

	go func() {
		_ = b.WaitForTurn(ctx)
		close(done)
	}()

	time.Sleep(10 * time.Millisecond)
	b.notifyTurnComplete()
	<-done

	b.waitMu.Lock()
	ch := b.waitCh
	b.waitMu.Unlock()
	if ch != nil {
		t.Fatal("waitCh should be nil after WaitForTurn returns")
	}
}

// --- WaitForTurn / IsTurnInFlight / SendCommand tested below ---
// Note: IsTurnInFlight basic cases (with/without callback, cleared after fire)
// are in lifecycle_test.go. Tests here focus on WaitForTurn and Interrupt
// which are specific to turn.go.

// --- notifyTurnComplete edge cases ---

// TestNotifyTurnComplete_NoWaiter verifies notifyTurnComplete is a safe
// no-op when no WaitForTurn caller is active.
func TestNotifyTurnComplete_NoWaiter(t *testing.T) {
	b := &Backend{}
	// Should not panic.
	b.notifyTurnComplete()
}

// TestNotifyTurnComplete_BufferedNonBlocking verifies notifyTurnComplete
// doesn't block even if the channel already has a pending signal (the
// default case in the select prevents deadlock).
func TestNotifyTurnComplete_BufferedNonBlocking(t *testing.T) {
	b := &Backend{}

	ch := make(chan struct{}, 1)
	b.waitMu.Lock()
	b.waitCh = ch
	b.waitMu.Unlock()

	// First signal fills the buffer.
	b.notifyTurnComplete()
	// Second signal should not block (default case).
	b.notifyTurnComplete()

	// Channel should have exactly one signal.
	select {
	case <-ch:
	default:
		t.Fatal("channel should have a signal")
	}
	select {
	case <-ch:
		t.Fatal("channel should only have one signal")
	default:
	}
}

// Note: SetPermissionPromptFunc, SetOnPermissionCleared, SetReplyFunc,
// SetTypingFunc, SetOnSessionReady, SessionID, SessionFilePath, and
// IsTurnInFlight basic cases are tested in lifecycle_test.go.

// TestSendCommand_NilPane verifies SendCommand returns an error when
// the backend hasn't been started (no tmux pane).
func TestSendCommand_NilPane(t *testing.T) {
	b := &Backend{}
	err := b.SendCommand(context.Background(), "test", "")
	if err == nil {
		t.Fatal("SendCommand should return error when pane is nil")
	}
}

// TestSendKeystroke_NilPane verifies SendKeystroke returns an error
// when the backend hasn't been started.
func TestSendKeystroke_NilPane(t *testing.T) {
	b := &Backend{}
	err := b.SendKeystroke(context.Background(), "a")
	if err == nil {
		t.Fatal("SendKeystroke should return error when pane is nil")
	}
}

// TestSendSpecialKey_NilPane verifies SendSpecialKey returns an error
// when the backend hasn't been started.
func TestSendSpecialKey_NilPane(t *testing.T) {
	b := &Backend{}
	err := b.SendSpecialKey(context.Background(), "Escape")
	if err == nil {
		t.Fatal("SendSpecialKey should return error when pane is nil")
	}
}

// TestInterrupt_NilPane verifies Interrupt returns an error when
// the backend hasn't been started (no pane to send keystrokes to).
func TestInterrupt_NilPane(t *testing.T) {
	b := &Backend{}
	err := b.Interrupt(context.Background())
	if err == nil {
		t.Fatal("Interrupt should return error when pane is nil")
	}
}
