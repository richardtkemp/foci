package command

import (
	"context"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// TestResetCommand_UsesSessionKeyFromContext verifies that /reset reads the
// session key from the context (injected by the dispatch layer).
func TestResetCommand_UsesSessionKeyFromContext(t *testing.T) {
	sessDir := t.TempDir()
	store := session.NewStore(sessDir)

	reqKey := "testagent/c12345/1000000000"
	if err := store.TestAppend(reqKey, provider.Message{Role: "user", Content: []provider.ContentBlock{{Type: "text", Text: "hello"}}}); err != nil {
		t.Fatal(err)
	}

	bs := workspace.NewBootstrap(t.TempDir(), nil)
	ag := &agent.Agent{
		Sessions:  store,
		Bootstrap: bs,
		MemoryFormationConfig: config.ResolvedMemoryFormation{
			SessionEndEnabled: false,
		},
	}
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
		Bootstrap: bs,
	}

	ctx := tools.WithSessionKey(context.Background(), reqKey)
	cmd := ResetCommand()
	resp, err := cmd.Execute(ctx, Request{}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != "Session cleared." {
		t.Errorf("response = %q, want %q", resp.Text, "Session cleared.")
	}
}

// TestResetCommand_NoSessionKey verifies that /reset returns an error when
// no session key is in the context.
func TestResetCommand_NoSessionKey(t *testing.T) {
	ag := &agent.Agent{}
	cc := CommandContext{
		Agent: ag,
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

// TestCompactCommand_UsesSessionKeyFromContext verifies that /compact reads
// the session key from the context.
func TestCompactCommand_UsesSessionKeyFromContext(t *testing.T) {
	ag := &agent.Agent{}
	cc := CommandContext{
		Agent: ag,
	}

	ctx := tools.WithSessionKey(context.Background(), "testagent/c12345/1000000000")
	cmd := CompactCommand()
	// Without a compactor, the command errors with "not configured".
	_, err := cmd.Execute(ctx, Request{}, cc)
	if err == nil {
		t.Fatal("expected error without compactor")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error = %q, want 'not configured'", err.Error())
	}
}
