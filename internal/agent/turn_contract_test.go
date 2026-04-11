package agent

import (
	"context"
	"sync/atomic"
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


// TestAPITransportSatisfiesInterface verifies that *APITransport satisfies the
// TurnContract interface (compile-time check).
func TestAPITransportSatisfiesInterface(t *testing.T) {
	var _ TurnContract = &APITransport{sharedTurnOps{agent: &Agent{}}}
}

// TestDelegatedTransportSatisfiesInterface verifies that *DelegatedTransport
// satisfies the TurnContract interface (compile-time check).
func TestDelegatedTransportSatisfiesInterface(t *testing.T) {
	var _ TurnContract = &DelegatedTransport{sharedTurnOps{agent: &Agent{}}}
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

// TestRunPostTurn_BlocksUntilCompletion verifies that runPostTurn blocks
// when CompletionChan is not yet closed, then runs post-turn methods
// after it closes. This is the delegated path where CC signals completion
// asynchronously.
func TestRunPostTurn_BlocksUntilCompletion(t *testing.T) {
	ch := make(chan struct{})
	ts := &TurnState{
		SessionKey:     "test/blocking",
		CompletionChan: ch,
	}

	var saved atomic.Bool
	tc := &stubContract{
		saveFn: func() { saved.Store(true) },
	}

	a := &Agent{}
	done := make(chan struct{})
	go func() {
		a.runPostTurn(tc, ts)
		close(done)
	}()

	// runPostTurn should be blocking — not yet returned.
	select {
	case <-done:
		t.Fatal("runPostTurn should block until CompletionChan closes")
	case <-time.After(50 * time.Millisecond):
		// expected — still blocking
	}

	// Signal completion.
	close(ch)

	// runPostTurn should unblock and run post-turn.
	select {
	case <-done:
		if !saved.Load() {
			t.Error("SaveSession was not called after completion")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runPostTurn did not unblock after CompletionChan closed")
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
func (s *stubContract) RunInference(ts *TurnState) error        { close(ts.CompletionChan); return nil }
func (s *stubContract) SaveSession(ts *TurnState) error {
	if s.saveFn != nil {
		s.saveFn()
	}
	ts.NewMessages = nil
	return nil
}
func (s *stubContract) UpdateSessionMeta(*TurnState)    {}
func (s *stubContract) LogUsage(*TurnState)              {}
func (s *stubContract) RunCompaction(*TurnState)         {}
func (s *stubContract) LogConversationSent(*TurnState)   {}
func (s *stubContract) TouchActivityPost(*TurnState)     {}
