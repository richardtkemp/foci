package command

import (
	"context"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/workspace"
)

// TestResetCommand_UsesReqSessionKey verifies that /reset uses req.SessionKey
// when provided, not cc.DefaultSessionKey(). This ensures the chat that issued
// /reset gets its own session reset, not the bot's default chat.
func TestResetCommand_UsesReqSessionKey(t *testing.T) {
	sessDir := t.TempDir()
	store := session.NewStore(sessDir)

	// Write a session file so RotateKey has something to work with.
	reqKey := "testagent/c12345/1000000000"
	if err := store.TestAppend(reqKey, provider.Message{Role: "user", Content: []provider.ContentBlock{{Type: "text", Text: "hello"}}}); err != nil {
		t.Fatal(err)
	}

	defaultKey := "testagent/c99999/1000000000"

	ag := &agent.Agent{}
	cc := CommandContext{
		Agent:       ag,
		Sessions:    store,
		Config:      &config.Config{},
		AgentConfig: config.AgentConfig{},
		Resolved: &config.ResolvedAgentConfig{
			MemoryFormation: config.ResolvedMemoryFormation{
				SessionEndEnabled: false,
			},
		},
		Bootstrap:         workspace.NewBootstrap(t.TempDir(), nil),
		DefaultSessionKey: func() string { return defaultKey },
	}

	cmd := ResetCommand()
	resp, err := cmd.Execute(context.Background(), Request{SessionKey: reqKey}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != "Session cleared." {
		t.Errorf("response = %q, want %q", resp.Text, "Session cleared.")
	}
}

// TestResetCommand_FallsBackToDefaultSessionKey verifies that /reset uses
// cc.DefaultSessionKey() when req.SessionKey is empty.
func TestResetCommand_FallsBackToDefaultSessionKey(t *testing.T) {
	sessDir := t.TempDir()
	store := session.NewStore(sessDir)

	defaultKey := "testagent/c55555/1000000000"
	if err := store.TestAppend(defaultKey, provider.Message{Role: "user", Content: []provider.ContentBlock{{Type: "text", Text: "hello"}}}); err != nil {
		t.Fatal(err)
	}

	ag := &agent.Agent{}
	cc := CommandContext{
		Agent:       ag,
		Sessions:    store,
		Config:      &config.Config{},
		AgentConfig: config.AgentConfig{},
		Resolved: &config.ResolvedAgentConfig{
			MemoryFormation: config.ResolvedMemoryFormation{
				SessionEndEnabled: false,
			},
		},
		Bootstrap:         workspace.NewBootstrap(t.TempDir(), nil),
		DefaultSessionKey: func() string { return defaultKey },
	}

	cmd := ResetCommand()
	// Empty SessionKey in request → should use DefaultSessionKey
	resp, err := cmd.Execute(context.Background(), Request{}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != "Session cleared." {
		t.Errorf("response = %q, want %q", resp.Text, "Session cleared.")
	}
}

// TestResetCommand_NoSessionKey verifies that /reset returns an error when
// both req.SessionKey and DefaultSessionKey are empty.
func TestResetCommand_NoSessionKey(t *testing.T) {
	ag := &agent.Agent{}
	cc := CommandContext{
		Agent:             ag,
		DefaultSessionKey: func() string { return "" },
	}

	cmd := ResetCommand()
	_, err := cmd.Execute(context.Background(), Request{}, cc)
	if err == nil {
		t.Fatal("expected error when no session key")
	}
	if !strings.Contains(err.Error(), "no active session") {
		t.Errorf("error = %q, want 'no active session'", err.Error())
	}
}

// TestCompactCommand_UsesReqSessionKey verifies that /compact uses
// req.SessionKey when provided, not cc.DefaultSessionKey().
func TestCompactCommand_UsesReqSessionKey(t *testing.T) {
	defaultKey := "testagent/c99999/1000000000"
	ag := &agent.Agent{}
	cc := CommandContext{
		Agent:             ag,
		DefaultSessionKey: func() string { return defaultKey },
	}

	cmd := CompactCommand()
	// Without a compactor, the command errors with "not configured".
	// But with req.SessionKey set, the error message should be "not configured",
	// NOT "no active session" — proving the key resolution reaches past
	// the empty-check.
	_, err := cmd.Execute(context.Background(), Request{SessionKey: "testagent/c12345/1000000000"}, cc)
	if err == nil {
		t.Fatal("expected error without compactor")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error = %q, want 'not configured'", err.Error())
	}
}
