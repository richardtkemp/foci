package command

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/tools"
)

// TestPassCommandMetadata verifies PassCommand returns a command with the
// correct name, description, and category.
func TestPassCommandMetadata(t *testing.T) {
	cmd := PassCommand()
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

// TestPassExecuteNoDelegatedManager verifies that /pass returns an error when
// the agent has no delegated manager (i.e. it's an API-mode agent).
func TestPassExecuteNoDelegatedManager(t *testing.T) {
	cmd := PassCommand()
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
	cmd := PassCommand()
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
	cmd := PassCommand()
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
	cmd := PassCommand()

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

// TestPassExecuteSuccessWithCapture verifies the happy path: /pass forwards a
// command via SendCommand and captures output from a CommandOutputCapturer
// backend, returning the extracted text.
func TestPassExecuteSuccessWithCapture(t *testing.T) {
	cmd := PassCommand()

	mb := &mockPassBackend{
		captureOutput: "❯ /model\n  ⎿  claude-opus-4-6\n─────────────────────────\n❯",
	}
	dm := &agent.DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) { return mb, nil },
	}

	cc := CommandContext{
		Agent: &agent.Agent{
			DelegatedManager: dm,
		},
	}

	ctx := tools.WithSessionKey(context.Background(), "agent:test:main")

	// Pre-seed the backend in the manager by calling Get first.
	_, err := dm.Get(ctx, "agent:test:main")
	if err != nil {
		t.Fatalf("seeding backend: %v", err)
	}

	resp, err := cmd.Execute(ctx, Request{Args: "/model"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(resp.Text, "claude-opus-4-6") {
		t.Errorf("response = %q, want mention of 'claude-opus-4-6'", resp.Text)
	}
	if mb.sentCommand != "/model" {
		t.Errorf("sentCommand = %q, want %q", mb.sentCommand, "/model")
	}
}

// TestPassExecuteSuccessNoCapturer verifies that when the backend does not
// implement CommandOutputCapturer, /pass returns the "sent" confirmation.
func TestPassExecuteSuccessNoCapturer(t *testing.T) {
	cmd := PassCommand()

	mb := &mockPassBackendNoCapturer{}
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
	if !strings.Contains(resp.Text, "Sent to CC") {
		t.Errorf("response = %q, want 'Sent to CC' confirmation", resp.Text)
	}
}

// TestPassExecuteSessionKeyFromRequest verifies that the session key is read
// from the Request when not present in the context.
func TestPassExecuteSessionKeyFromRequest(t *testing.T) {
	cmd := PassCommand()

	mb := &mockPassBackendNoCapturer{}
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
	if !strings.Contains(resp.Text, "Sent to CC") {
		t.Errorf("response = %q, want 'Sent to CC' confirmation", resp.Text)
	}
}

// TestExtractCommandOutput exercises the pane output parser with a
// table-driven set of inputs covering normal output, empty output,
// missing prompts, and various edge cases.
func TestExtractCommandOutput(t *testing.T) {
	tests := []struct {
		name    string
		pane    string
		command string
		want    string
	}{
		{
			name: "normal output with prompts and separator",
			pane: "❯ /model\n  ⎿  claude-opus-4-6\n─────────────────────────\n❯",
			command: "/model",
			want:    "⎿  claude-opus-4-6",
		},
		{
			name: "multi-line output",
			pane: "❯ /context\n  ⎿  System: 1200 tokens\n  ⎿  History: 800 tokens\n  ⎿  Total: 2000 tokens\n───────────────────\n❯",
			command: "/context",
			want:    "⎿  System: 1200 tokens\n  ⎿  History: 800 tokens\n  ⎿  Total: 2000 tokens",
		},
		{
			name:    "no matching command line",
			pane:    "❯ /something_else\n  output here\n❯",
			command: "/model",
			want:    "",
		},
		{
			name:    "empty pane content",
			pane:    "",
			command: "/model",
			want:    "",
		},
		{
			name: "no trailing prompt — output extends to end",
			pane: "❯ /help\n  ⎿  Available commands:\n  ⎿  /model /context /compact",
			command: "/help",
			want:    "⎿  Available commands:\n  ⎿  /model /context /compact",
		},
		{
			name:    "only separator and empty lines between prompts",
			pane:    "❯ /compact\n\n──────────────────────\n\n❯",
			command: "/compact",
			want:    "",
		},
		{
			name: "trailing prompt with space",
			pane: "❯ /model opus\n  ⎿  Switched to opus\n❯ ",
			command: "/model opus",
			want:    "⎿  Switched to opus",
		},
		{
			name: "multiple commands — picks last matching",
			pane: "❯ /model\n  ⎿  haiku\n❯ /model\n  ⎿  opus\n❯",
			command: "/model",
			want:    "⎿  opus",
		},
		{
			name:    "command without leading slash in pane",
			pane:    "❯ model\n  ⎿  opus\n❯",
			command: "/model",
			want:    "⎿  opus",
		},
		{
			name: "heavy separator characters",
			pane: "❯ /status\n  ⎿  all good\n━━━━━━━━━━━━━━━━━━━━\n❯",
			command: "/status",
			want:    "⎿  all good",
		},
		{
			name: "mixed separator with dashes",
			pane: "❯ /status\n  ⎿  ok\n-------------------\n❯",
			command: "/status",
			want:    "⎿  ok",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCommandOutput(tt.pane, tt.command)
			if got != tt.want {
				t.Errorf("extractCommandOutput(%q, %q)\n  got:  %q\n  want: %q", tt.pane, tt.command, got, tt.want)
			}
		})
	}
}

// TestIsSeparatorLine verifies that the separator detection accepts known
// separator patterns and rejects non-separator strings.
func TestIsSeparatorLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{name: "box-drawing long", line: "──────────────────────", want: true},
		{name: "heavy box-drawing", line: "━━━━━━━━━━━━━━━━━━━━━━", want: true},
		{name: "ascii dashes long", line: "----------------------", want: true},
		{name: "short dash", line: "-", want: false},
		{name: "exactly 10 dashes", line: "----------", want: false},
		{name: "11 dashes", line: "-----------", want: true},
		{name: "mixed box-drawing and dashes", line: "──────────-──────", want: true},
		{name: "empty string", line: "", want: false},
		{name: "text content", line: "hello world", want: false},
		{name: "separator with text", line: "───── output ─────", want: false},
		{name: "single box-drawing", line: "─", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSeparatorLine(tt.line)
			if got != tt.want {
				t.Errorf("isSeparatorLine(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

// --- Mock backends for pass tests ---

// mockPassBackend implements delegator.Delegator and delegator.CommandOutputCapturer
// for testing /pass with pane capture.
type mockPassBackend struct {
	sentCommand   string
	captureOutput string
}

func (m *mockPassBackend) Start(context.Context, delegator.StartOptions) error { return nil }
func (m *mockPassBackend) SendToPane(context.Context, string, *delegator.EventHandler) (*delegator.TurnResult, error) {
	return &delegator.TurnResult{}, nil
}
func (m *mockPassBackend) WaitForTurn(context.Context) error             { return nil }
func (m *mockPassBackend) IsTurnInFlight() bool                          { return false }
func (m *mockPassBackend) SendCommand(_ context.Context, cmd, _ string) error {
	m.sentCommand = cmd
	return nil
}
func (m *mockPassBackend) IsRunning() bool                                        { return true }
func (m *mockPassBackend) Restart(context.Context) error                          { return nil }
func (m *mockPassBackend) SetPermissionPromptFunc(delegator.PermissionPromptFunc)   {}
func (m *mockPassBackend) SetOnPermissionCleared(func())                          {}
func (m *mockPassBackend) SetOnSessionReady(func(string))                         {}
func (m *mockPassBackend) SetTypingFunc(func(bool))                               {}
func (m *mockPassBackend) SendKeystroke(context.Context, string) error             { return nil }
func (m *mockPassBackend) SendSpecialKey(context.Context, string) error            { return nil }
func (m *mockPassBackend) Interrupt(context.Context) error                         { return nil }
func (m *mockPassBackend) SessionID() string                                       { return "" }
func (m *mockPassBackend) SessionFilePath() string                                 { return "" }
func (m *mockPassBackend) WaitReady(context.Context) error                         { return nil }
func (m *mockPassBackend) Close() error                                            { return nil }

func (m *mockPassBackend) CaptureCommandOutput(_ context.Context, _, _ time.Duration) (string, error) {
	return m.captureOutput, nil
}

// Compile-time verification that mockPassBackend satisfies both interfaces.
var (
	_ delegator.Delegator               = (*mockPassBackend)(nil)
	_ delegator.CommandOutputCapturer = (*mockPassBackend)(nil)
)

// mockPassBackendNoCapturer implements only delegator.Delegator (no capture support).
type mockPassBackendNoCapturer struct {
	sentCommand string
}

func (m *mockPassBackendNoCapturer) Start(context.Context, delegator.StartOptions) error { return nil }
func (m *mockPassBackendNoCapturer) SendToPane(context.Context, string, *delegator.EventHandler) (*delegator.TurnResult, error) {
	return &delegator.TurnResult{}, nil
}
func (m *mockPassBackendNoCapturer) WaitForTurn(context.Context) error             { return nil }
func (m *mockPassBackendNoCapturer) IsTurnInFlight() bool                          { return false }
func (m *mockPassBackendNoCapturer) SendCommand(_ context.Context, cmd, _ string) error {
	m.sentCommand = cmd
	return nil
}
func (m *mockPassBackendNoCapturer) IsRunning() bool                                        { return true }
func (m *mockPassBackendNoCapturer) Restart(context.Context) error                          { return nil }
func (m *mockPassBackendNoCapturer) SetPermissionPromptFunc(delegator.PermissionPromptFunc)   {}
func (m *mockPassBackendNoCapturer) SetOnPermissionCleared(func())                          {}
func (m *mockPassBackendNoCapturer) SetOnSessionReady(func(string))                         {}
func (m *mockPassBackendNoCapturer) SetTypingFunc(func(bool))                               {}
func (m *mockPassBackendNoCapturer) SendKeystroke(context.Context, string) error             { return nil }
func (m *mockPassBackendNoCapturer) SendSpecialKey(context.Context, string) error            { return nil }
func (m *mockPassBackendNoCapturer) Interrupt(context.Context) error                         { return nil }
func (m *mockPassBackendNoCapturer) SessionID() string                                       { return "" }
func (m *mockPassBackendNoCapturer) SessionFilePath() string                                 { return "" }
func (m *mockPassBackendNoCapturer) WaitReady(context.Context) error                         { return nil }
func (m *mockPassBackendNoCapturer) Close() error                                            { return nil }

// Compile-time verification.
var _ delegator.Delegator = (*mockPassBackendNoCapturer)(nil)
