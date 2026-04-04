package agent

import (
	"context"
	"fmt"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// TestOrchestrateFullTurn_PostTurnAsyncPath verifies that when CompletionChan is not
// immediately closed (delegated path), post-turn runs asynchronously and
// completes when the channel closes.
func TestOrchestrateFullTurn_PostTurnAsyncPath(t *testing.T) {
	a := &Agent{}
	doneCh := make(chan struct{})

	tc := &asyncStubContract{
		completionDelay: 50 * time.Millisecond,
		onSave:          func() { close(doneCh) },
	}
	ts := NewTurnState(context.Background(), "test/async", []string{"hi"}, nil)

	_, err := a.OrchestrateFullTurn(context.Background(), tc, ts)
	if err != nil {
		t.Fatalf("OrchestrateFullTurn: %v", err)
	}

	select {
	case <-doneCh:
		// success — post-turn fired after async completion
	case <-time.After(5 * time.Second):
		t.Fatal("post-turn SaveSession not called within timeout")
	}
}

// TestOrchestrateFullTurn_SafetyNetOnError verifies that the orchestrator's safety-net
// defer calls SaveSession when RunInference returns an error.
func TestOrchestrateFullTurn_SafetyNetOnError(t *testing.T) {
	a := &Agent{}
	saved := false

	tc := &errorStubContract{
		onSave: func() { saved = true },
	}
	ts := NewTurnState(context.Background(), "test/err", []string{"hi"}, nil)
	ts.NewMessages = []provider.Message{{Role: "user"}} // simulate accumulated messages

	_, err := a.OrchestrateFullTurn(context.Background(), tc, ts)
	if err == nil {
		t.Fatal("expected error from RunInference")
	}
	if !saved {
		t.Fatal("safety-net should have called SaveSession on error")
	}
}

// TestOrchestrateFullTurn_SafetyNetSkipsOnSuccess verifies the safety-net defer does NOT
// double-save when the turn completes successfully (NewMessages nil'd by post-turn).
func TestOrchestrateFullTurn_SafetyNetSkipsOnSuccess(t *testing.T) {
	a := &Agent{}
	saveCount := 0

	tc := &stubContract{
		saveFn: func() { saveCount++ },
	}
	ts := NewTurnState(context.Background(), "test/ok", []string{"hi"}, nil)
	ts.NewMessages = []provider.Message{{Role: "user"}}

	_, err := a.OrchestrateFullTurn(context.Background(), tc, ts)
	if err != nil {
		t.Fatalf("OrchestrateFullTurn: %v", err)
	}
	if saveCount != 1 {
		t.Fatalf("SaveSession called %d times, want 1 (post-turn only, not safety-net)", saveCount)
	}
}

// TestTransportSelection_API verifies that HandleMessageWithAttachments
// routes through the API transport when DelegatedManager is nil, producing
// a response from the mock client.
func TestTransportSelection_API(t *testing.T) {
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID:         "msg_api",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("API response"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	ag := &Agent{
		Client:    client,
		Sessions:  session.NewStore(t.TempDir()),
		Tools:     tools.NewRegistry(),
		Bootstrap: workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:     "test-model",
	}

	resp, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "hello")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if resp != "API response" {
		t.Errorf("response = %q, want %q", resp, "API response")
	}
}

// TestTransportSelection_Delegated verifies that HandleMessageWithAttachments
// routes through the delegated transport when DelegatedManager is set.
func TestTransportSelection_Delegated(t *testing.T) {
	ag := &Agent{
		Sessions:  session.NewStore(t.TempDir()),
		Tools:     tools.NewRegistry(),
		Bootstrap: workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:     "test-model",
		DelegatedManager: &DelegatedManager{
			NewBackend: func() (delegator.Delegator, error) {
				return nil, fmt.Errorf("no real backend in test")
			},
		},
	}

	// Delegated path will fail (no real backend), but it should NOT fall
	// through to the API path. The error proves the delegated transport was selected.
	_, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "hello")
	if err == nil {
		t.Fatal("expected error from delegated transport (no real backend)")
	}
}

// --- Test stubs ---

// asyncStubContract simulates a delegated transport where CompletionChan
// closes after a delay (async turn completion).
type asyncStubContract struct {
	completionDelay time.Duration
	onSave          func()
}

func (s *asyncStubContract) RateLimitGate(*TurnState) error        { return nil }
func (s *asyncStubContract) AcquireTurnLock(*TurnState) func()     { return func() {} }
func (s *asyncStubContract) IncrementProcessing(*TurnState) func() { return func() {} }
func (s *asyncStubContract) RegisterTurn(*TurnState) func()        { return func() {} }
func (s *asyncStubContract) CheckStaleContext(*TurnState) error     { return nil }
func (s *asyncStubContract) RegisterSessionIndex(*TurnState)        {}
func (s *asyncStubContract) LogConversationRecv(*TurnState)         {}
func (s *asyncStubContract) TouchActivity(*TurnState)               {}
func (s *asyncStubContract) LoadSessionMeta(*TurnState)             {}
func (s *asyncStubContract) ComposePrompt(*TurnState) error         { return nil }
func (s *asyncStubContract) LoadAndRepairSession(*TurnState) error  { return nil }
func (s *asyncStubContract) ResolveModelEffort(*TurnState)          {}
func (s *asyncStubContract) BuildSystemAndTools(*TurnState)         {}
func (s *asyncStubContract) InjectNudges(*TurnState)                {}
func (s *asyncStubContract) RunInference(ts *TurnState) error {
	go func() {
		time.Sleep(s.completionDelay)
		close(ts.CompletionChan)
	}()
	return nil
}
func (s *asyncStubContract) SaveSession(ts *TurnState) error {
	if s.onSave != nil {
		s.onSave()
	}
	ts.NewMessages = nil
	return nil
}
func (s *asyncStubContract) UpdateSessionMeta(*TurnState)  {}
func (s *asyncStubContract) LogUsage(*TurnState)            {}
func (s *asyncStubContract) RunCompaction(*TurnState)       {}
func (s *asyncStubContract) LogConversationSent(*TurnState) {}
func (s *asyncStubContract) TouchActivityPost(*TurnState)   {}

// errorStubContract simulates RunInference returning an error.
type errorStubContract struct {
	onSave func()
}

func (s *errorStubContract) RateLimitGate(*TurnState) error        { return nil }
func (s *errorStubContract) AcquireTurnLock(*TurnState) func()     { return func() {} }
func (s *errorStubContract) IncrementProcessing(*TurnState) func() { return func() {} }
func (s *errorStubContract) RegisterTurn(*TurnState) func()        { return func() {} }
func (s *errorStubContract) CheckStaleContext(*TurnState) error     { return nil }
func (s *errorStubContract) RegisterSessionIndex(*TurnState)        {}
func (s *errorStubContract) LogConversationRecv(*TurnState)         {}
func (s *errorStubContract) TouchActivity(*TurnState)               {}
func (s *errorStubContract) LoadSessionMeta(*TurnState)             {}
func (s *errorStubContract) ComposePrompt(*TurnState) error         { return nil }
func (s *errorStubContract) LoadAndRepairSession(*TurnState) error  { return nil }
func (s *errorStubContract) ResolveModelEffort(*TurnState)          {}
func (s *errorStubContract) BuildSystemAndTools(*TurnState)         {}
func (s *errorStubContract) InjectNudges(*TurnState)                {}
func (s *errorStubContract) RunInference(*TurnState) error {
	return fmt.Errorf("simulated execution error")
}
func (s *errorStubContract) SaveSession(ts *TurnState) error {
	if s.onSave != nil {
		s.onSave()
	}
	ts.NewMessages = nil
	return nil
}
func (s *errorStubContract) UpdateSessionMeta(*TurnState)  {}
func (s *errorStubContract) LogUsage(*TurnState)            {}
func (s *errorStubContract) RunCompaction(*TurnState)       {}
func (s *errorStubContract) LogConversationSent(*TurnState) {}
func (s *errorStubContract) TouchActivityPost(*TurnState)   {}
