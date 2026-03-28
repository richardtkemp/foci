package claudecode

import (
	"sync"
	"testing"

	"foci/internal/backend"
)

// TestFireTurnComplete_PerTurnCallback verifies the per-turn callback fires
// with the correct TurnResult.
func TestFireTurnComplete_PerTurnCallback(t *testing.T) {
	b := &Backend{}

	var got *backend.TurnResult
	b.turnCompleteMu.Lock()
	b.turnCompleteFn = func(r *backend.TurnResult) { got = r }
	b.turnCompleteMu.Unlock()

	result := &backend.TurnResult{
		Text:      "hello",
		ToolCalls: 3,
		Usage: &backend.TurnUsage{
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
	b.turnCompleteFn = func(r *backend.TurnResult) { callCount++ }
	b.turnCompleteMu.Unlock()

	b.fireTurnComplete(&backend.TurnResult{Text: "first"})
	b.fireTurnComplete(&backend.TurnResult{Text: "second"})

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
	b.fireTurnComplete(&backend.TurnResult{Text: "no callback"})
}

// TestFireTurnComplete_AlsoSignalsWaitForTurn verifies that fireTurnComplete
// also signals the legacy WaitForTurn channel.
func TestFireTurnComplete_AlsoSignalsWaitForTurn(t *testing.T) {
	b := &Backend{}

	ch := make(chan struct{}, 1)
	b.waitMu.Lock()
	b.waitCh = ch
	b.waitMu.Unlock()

	b.fireTurnComplete(&backend.TurnResult{})

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
			b.turnCompleteFn = func(r *backend.TurnResult) {}
			b.turnCompleteMu.Unlock()
		}()
		// Concurrent fire.
		go func() {
			defer wg.Done()
			b.fireTurnComplete(&backend.TurnResult{})
		}()
	}
	wg.Wait()
}
