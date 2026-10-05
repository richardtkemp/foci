package command

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/tools"
)

// TestPassCommandMetadata verifies PassCommand returns a command with the
// correct name, description, and category.
func TestPassCommandMetadata(t *testing.T) {
	cmd := PassCommand("claude-code")
	if cmd.Name != "pass" {
		t.Errorf("Name = %q, want %q", cmd.Name, "pass")
	}
	if cmd.Description == "" {
		t.Error("Description should not be empty")
	}
	if cmd.Category != "operations" {
		t.Errorf("Category = %q, want %q", cmd.Category, "operations")
	}
}

// TestPassCommandDescriptionVariesByBackend verifies the /pass description
// names the actual configured backend rather than being hardwired to
// "Claude Code" — it must track backendType per agent (codex, opencode, an
// unrecognised future backend, and the empty/API-mode case all render
// distinct, non-empty text).
func TestPassCommandDescriptionVariesByBackend(t *testing.T) {
	tests := []struct {
		backendType string
		wantSubstr  string
	}{
		{"claude-code", "Claude Code"},
		{"codex", "Codex CLI"},
		{"opencode", "OpenCode"},
		{"", "the delegated backend"},
	}
	for _, tt := range tests {
		cmd := PassCommand(tt.backendType)
		if !strings.Contains(cmd.Description, tt.wantSubstr) {
			t.Errorf("PassCommand(%q).Description = %q, want substring %q", tt.backendType, cmd.Description, tt.wantSubstr)
		}
	}
}

// TestPassExecuteNoDelegatedManager verifies that /pass returns an error when
// the agent has no delegated manager (i.e. it's an API-mode agent).
func TestPassExecuteNoDelegatedManager(t *testing.T) {
	cmd := PassCommand("claude-code")
	cc := CommandContext{
		Agent: &agent.Agent{},
	}
	_, err := cmd.Execute(context.Background(), Request{Args: "/help"}, cc)
	if err == nil {
		t.Fatal("expected error for nil DelegatedManager")
	}
	if !strings.Contains(err.Error(), "delegated") {
		t.Errorf("error = %q, want mention of 'delegated'", err)
	}
}

// TestPassExecuteNoArgs verifies that /pass with empty args returns a usage error.
func TestPassExecuteNoArgs(t *testing.T) {
	cmd := PassCommand("claude-code")
	cc := CommandContext{
		Agent: &agent.Agent{
			DelegatedManager: &agent.DelegatedManager{},
		},
	}
	_, err := cmd.Execute(context.Background(), Request{Args: ""}, cc)
	if err == nil {
		t.Fatal("expected error for empty args")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error = %q, want mention of 'usage'", err)
	}
}

// TestPassExecuteNoSession verifies that /pass returns an error when neither
// the context nor the request contains a session key.
func TestPassExecuteNoSession(t *testing.T) {
	cmd := PassCommand("claude-code")
	cc := CommandContext{
		Agent: &agent.Agent{
			DelegatedManager: &agent.DelegatedManager{},
		},
	}
	_, err := cmd.Execute(context.Background(), Request{Args: "/help"}, cc)
	if err == nil {
		t.Fatal("expected error for missing session key")
	}
	if !strings.Contains(err.Error(), "no active session") {
		t.Errorf("error = %q, want mention of 'no active session'", err)
	}
}

// TestPassExecuteGetBackendError verifies that /pass surfaces errors from
// DelegatedManager.Get when the backend cannot be resolved.
func TestPassExecuteGetBackendError(t *testing.T) {
	cmd := PassCommand("claude-code")

	dm := &agent.DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			return nil, fmt.Errorf("boom")
		},
	}

	cc := CommandContext{
		Agent: &agent.Agent{
			DelegatedManager: dm,
		},
	}

	ctx := tools.WithSessionKey(context.Background(), "agent:test:main")
	_, err := cmd.Execute(ctx, Request{Args: "/help"}, cc)
	if err == nil {
		t.Fatal("expected error from Get")
	}
	if !strings.Contains(err.Error(), "get backend") {
		t.Errorf("error = %q, want wrapped 'get backend'", err)
	}
}

// TestPassExecuteSuccess verifies the happy path: /pass forwards the command
// to the backend and returns the "sent" confirmation.
func TestPassExecuteSuccess(t *testing.T) {
	cmd := PassCommand("claude-code")

	mb := &mockPassBackend{}
	dm := &agent.DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) { return mb, nil },
	}

	cc := CommandContext{
		Agent: &agent.Agent{
			DelegatedManager: dm,
		},
	}

	ctx := tools.WithSessionKey(context.Background(), "agent:test:main")

	// Pre-seed the backend in the manager.
	_, err := dm.Get(ctx, "agent:test:main")
	if err != nil {
		t.Fatalf("seeding backend: %v", err)
	}

	resp, err := cmd.Execute(ctx, Request{Args: "/compact"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(resp.Text, "Sent to Claude Code") {
		t.Errorf("response = %q, want 'Sent to Claude Code' confirmation", resp.Text)
	}
	if mb.sentCommand != "/compact" {
		t.Errorf("sentCommand = %q, want %q", mb.sentCommand, "/compact")
	}
}

// TestPassExecuteSessionKeyFromRequest verifies that the session key is read
// from the Request when not present in the context.
func TestPassExecuteSessionKeyFromRequest(t *testing.T) {
	cmd := PassCommand("claude-code")

	mb := &mockPassBackend{}
	dm := &agent.DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) { return mb, nil },
	}

	cc := CommandContext{
		Agent: &agent.Agent{
			DelegatedManager: dm,
		},
	}

	ctx := context.Background() // no session key in context

	// Pre-seed the backend in the manager.
	seedCtx := tools.WithSessionKey(context.Background(), "agent:test:main")
	_, err := dm.Get(seedCtx, "agent:test:main")
	if err != nil {
		t.Fatalf("seeding backend: %v", err)
	}

	resp, err := cmd.Execute(ctx, Request{Args: "/compact", SessionKey: "agent:test:main"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(resp.Text, "Sent to Claude Code") {
		t.Errorf("response = %q, want 'Sent to Claude Code' confirmation", resp.Text)
	}
}

// --- Mock backends for pass tests ---

// mockPassBackend implements delegator.Delegator for testing /pass.
type mockPassBackend struct {
	sentCommand string
}

func (m *mockPassBackend) Start(context.Context, delegator.StartOptions) error { return nil }
func (m *mockPassBackend) SendToPane(context.Context, string) (*delegator.TurnResult, error) {
	return &delegator.TurnResult{}, nil
}
func (m *mockPassBackend) WaitForTurn(context.Context) error { return nil }
func (m *mockPassBackend) IsTurnInFlight() bool              { return false }
func (m *mockPassBackend) SendCommand(_ context.Context, cmd string) error {
	m.sentCommand = cmd
	return nil
}
func (m *mockPassBackend) ImmediateInject(ctx context.Context, inj delegator.Inject) error {
	switch inj.Source {
	case delegator.SourceUser, delegator.SourceSteer:
		if !m.IsTurnInFlight() {
			_, err := m.SendToPane(ctx, inj.Text)
			return err
		}
		return m.SendCommand(ctx, inj.Text)
	case delegator.SourceCompact, delegator.SourcePass:
		return m.SendCommand(ctx, inj.Text)
	}
	return nil
}
func (m *mockPassBackend) IsRunning() bool                                        { return true }
func (m *mockPassBackend) SetPermissionPromptFunc(delegator.PermissionPromptFunc) {}
func (m *mockPassBackend) SetOnPromptsCleared(func())                             {}
func (m *mockPassBackend) RegisterPromptCancelListener(string, func(string))      {}
func (m *mockPassBackend) SetOnSessionReady(func(string))                         {}
func (m *mockPassBackend) SetTypingFunc(func(bool))                               {}
func (m *mockPassBackend) AttachSessionEvents(*delegator.SessionEvents)           {}
func (m *mockPassBackend) Interrupt(context.Context) error                        { return nil }
func (m *mockPassBackend) SessionID() string                                      { return "" }
func (m *mockPassBackend) SessionFilePath() string                                { return "" }
func (m *mockPassBackend) WaitReady(context.Context) error                        { return nil }
func (m *mockPassBackend) CheckReady(context.Context) (bool, error)               { return true, nil }
func (m *mockPassBackend) StatusDetail() string                                   { return "" }
func (m *mockPassBackend) Close() error                                           { return nil }

// Compile-time verification.
var _ delegator.Delegator = (*mockPassBackend)(nil)
