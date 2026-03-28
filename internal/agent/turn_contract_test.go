package agent

import (
	"context"
	"testing"
	"time"
)

// TestNewTurnState verifies that the constructor initialises all fields
// and that CompletionChan is non-nil and open.
func TestNewTurnState(t *testing.T) {
	ctx := context.Background()
	ts := NewTurnState(ctx, "test/session", []string{"hello"}, nil)

	if ts.SessionKey != "test/session" {
		t.Errorf("SessionKey = %q, want %q", ts.SessionKey, "test/session")
	}
	if ts.Ctx != ctx {
		t.Error("Ctx not set")
	}
	if len(ts.Texts) != 1 || ts.Texts[0] != "hello" {
		t.Errorf("Texts = %v, want [hello]", ts.Texts)
	}
	if ts.CompletionChan == nil {
		t.Fatal("CompletionChan should not be nil")
	}

	// Channel should not be closed yet.
	select {
	case <-ts.CompletionChan:
		t.Fatal("CompletionChan should not be closed yet")
	default:
		// expected
	}
}

// TestCompletionChanSemantics_Close verifies that closing CompletionChan
// unblocks a select waiting on it.
func TestCompletionChanSemantics_Close(t *testing.T) {
	ch := make(chan struct{})

	done := make(chan bool, 1)
	go func() {
		select {
		case <-ch:
			done <- true
		case <-time.After(5 * time.Second):
			done <- false
		}
	}()

	// Close the channel — the goroutine should unblock.
	close(ch)

	if result := <-done; !result {
		t.Fatal("closing CompletionChan did not unblock select")
	}
}

// TestCompletionChanSemantics_Timeout verifies that an unclosed CompletionChan
// causes the timeout path to fire.
func TestCompletionChanSemantics_Timeout(t *testing.T) {
	ch := make(chan struct{})

	done := make(chan string, 1)
	go func() {
		select {
		case <-ch:
			done <- "completed"
		case <-time.After(50 * time.Millisecond):
			done <- "timeout"
		}
	}()

	result := <-done
	if result != "timeout" {
		t.Fatalf("expected timeout path, got %q", result)
	}
}

// TestAPITransportSatisfiesInterface verifies at runtime that *APITransport
// satisfies the TurnContract interface.
func TestAPITransportSatisfiesInterface(t *testing.T) {
	var tc TurnContract = &APITransport{sharedTurnOps{agent: &Agent{}}}
	if tc == nil {
		t.Fatal("APITransport should satisfy TurnContract")
	}
}

// TestBackendTransportSatisfiesInterface verifies at runtime that
// *BackendTransport satisfies the TurnContract interface.
func TestBackendTransportSatisfiesInterface(t *testing.T) {
	var tc TurnContract = &BackendTransport{sharedTurnOps{agent: &Agent{}}}
	if tc == nil {
		t.Fatal("BackendTransport should satisfy TurnContract")
	}
}

// TestRunPostTurn_SyncPath verifies that runPostTurn calls post-turn methods
// inline when CompletionChan is already closed (API path).
func TestRunPostTurn_SyncPath(t *testing.T) {
	ch := make(chan struct{})
	close(ch) // already completed

	ts := &TurnState{
		SessionKey:     "test/sync",
		CompletionChan: ch,
	}

	called := false
	tc := &stubContract{
		saveFn: func() { called = true },
	}

	a := &Agent{}
	a.runPostTurn(tc, ts)

	// Sync path: post() should have been called inline before runPostTurn returns.
	if !called {
		t.Fatal("post-turn SaveSession was not called in sync path")
	}
}

// TestRunPostTurn_AsyncPath verifies that runPostTurn launches a goroutine
// when CompletionChan is not yet closed (backend path), and that closing it
// triggers the post-turn methods.
func TestRunPostTurn_AsyncPath(t *testing.T) {
	ch := make(chan struct{})
	ts := &TurnState{
		SessionKey:     "test/async",
		CompletionChan: ch,
	}

	doneCh := make(chan struct{})
	tc := &stubContract{
		saveFn: func() { close(doneCh) },
	}

	a := &Agent{}
	a.runPostTurn(tc, ts)

	// Should not have been called yet (async path).
	select {
	case <-doneCh:
		t.Fatal("post-turn should not have been called yet in async path")
	case <-time.After(20 * time.Millisecond):
		// expected
	}

	// Signal completion.
	close(ch)

	// Wait for post-turn to fire.
	select {
	case <-doneCh:
		// success
	case <-time.After(5 * time.Second):
		t.Fatal("post-turn was not called after closing CompletionChan in async path")
	}
}

// stubContract is a minimal TurnContract implementation for testing the
// orchestrator's post-turn logic. Only SaveSession is wired; all other
// methods are harmless no-ops.
type stubContract struct {
	saveFn func()
}

func (s *stubContract) RateLimitGate(*TurnState) error        { return nil }
func (s *stubContract) AcquireTurnLock(*TurnState) func()     { return func() {} }
func (s *stubContract) IncrementProcessing(*TurnState) func() { return func() {} }
func (s *stubContract) RegisterTurn(*TurnState) func()        { return func() {} }
func (s *stubContract) CheckStaleContext(*TurnState) error     { return nil }
func (s *stubContract) RegisterSessionIndex(*TurnState)        {}
func (s *stubContract) LogConversationRecv(*TurnState)         {}
func (s *stubContract) TouchActivity(*TurnState)               {}
func (s *stubContract) LoadSessionMeta(*TurnState)             {}
func (s *stubContract) ComposePrompt(*TurnState) error         { return nil }
func (s *stubContract) LoadAndRepairSession(*TurnState) error  { return nil }
func (s *stubContract) ResolveModelEffort(*TurnState)          {}
func (s *stubContract) BuildSystemAndTools(*TurnState)         {}
func (s *stubContract) InjectNudges(*TurnState)                {}
func (s *stubContract) ExecuteTurn(*TurnState) error           { return nil }
func (s *stubContract) SaveSession(ts *TurnState) error {
	if s.saveFn != nil {
		s.saveFn()
	}
	return nil
}
func (s *stubContract) UpdateSessionMeta(*TurnState)    {}
func (s *stubContract) RunCompaction(*TurnState)         {}
func (s *stubContract) LogConversationSent(*TurnState)   {}
func (s *stubContract) TouchActivityPost(*TurnState)     {}
