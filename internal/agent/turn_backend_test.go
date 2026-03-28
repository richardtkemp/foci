package agent

import (
	"context"
	"strings"
	"testing"
)

// TestBackendTransport_NoOps verifies that no-op methods don't panic and
// return the expected zero values.
func TestBackendTransport_NoOps(t *testing.T) {
	a := &Agent{}
	tr := &BackendTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.Meta = &TurnMetadata{}
	ts.SessionMeta = a.getSessionMeta(ts.SessionKey)

	// Phase 1 no-ops return zero values.
	if err := tr.RateLimitGate(ts); err != nil {
		t.Fatalf("RateLimitGate: %v", err)
	}
	unlock := tr.AcquireTurnLock(ts)
	unlock() // should not panic
	dec := tr.IncrementProcessing(ts)
	dec() // should not panic
	unreg := tr.RegisterTurn(ts)
	unreg() // should not panic

	// Phase 2 no-ops.
	if err := tr.LoadAndRepairSession(ts); err != nil {
		t.Fatalf("LoadAndRepairSession: %v", err)
	}
	tr.BuildSystemAndTools(ts) // no panic
	tr.InjectNudges(ts)        // no panic

	// Phase 4 no-ops.
	if err := tr.SaveSession(ts); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	tr.UpdateSessionMeta(ts) // no panic (stub)
	tr.RunCompaction(ts)      // no panic (stub)
}

// TestBackendTransport_ResolveModelEffort verifies it reads agent-level model.
func TestBackendTransport_ResolveModelEffort(t *testing.T) {
	a := &Agent{Model: "anthropic/claude-opus-4-6"}
	tr := &BackendTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)

	tr.ResolveModelEffort(ts)

	if ts.TurnModel != "anthropic/claude-opus-4-6" {
		t.Errorf("TurnModel = %q, want %q", ts.TurnModel, "anthropic/claude-opus-4-6")
	}
}

// TestBackendTransport_ComposePrompt verifies it produces a non-empty prompt
// and updates lastMessageTime.
func TestBackendTransport_ComposePrompt(t *testing.T) {
	a := &Agent{Model: "anthropic/claude-opus-4-6"}
	tr := &BackendTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hello world"}, nil)
	ts.Meta = &TurnMetadata{}
	ts.SessionMeta = a.getSessionMeta(ts.SessionKey)
	ts.TurnModel = a.Model

	if err := tr.ComposePrompt(ts); err != nil {
		t.Fatalf("ComposePrompt: %v", err)
	}

	if ts.Prompt == "" {
		t.Fatal("Prompt should not be empty")
	}
	// The prompt should contain the user text.
	if !strings.Contains(ts.Prompt, "hello world") {
		t.Errorf("Prompt should contain user text, got: %q", ts.Prompt)
	}
	// lastMessageTime should have been updated.
	if ts.SessionMeta.lastMessageTime.IsZero() {
		t.Error("lastMessageTime should be non-zero after ComposePrompt")
	}
}

