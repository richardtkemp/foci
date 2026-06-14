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
		Reflection: config.ResolvedReflection{
			SessionEndEnabled: false,
		},
	}
	cc := CommandContext{
		Agent:       ag,
		Sessions:    store,
		Config:      &config.Config{},
		AgentConfig: config.AgentConfig{},
		Resolved: &config.ResolvedAgentConfig{
			Reflection: config.ResolvedReflection{
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

// TestResetCommand_HardSubcommand verifies that /reset hard dispatches to
// ResetSessionHard rather than ResetSession — including when the agent is
// processing (the soft path would refuse).
func TestResetCommand_HardSubcommand(t *testing.T) {
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
	}
	// Simulate an in-flight turn on this session — the soft path would error here.
	ag.SetTurnInFlightForTest(reqKey, true)

	cc := CommandContext{
		Agent:     ag,
		Sessions:  store,
		Config:    &config.Config{},
		Bootstrap: bs,
	}

	ctx := tools.WithSessionKey(context.Background(), reqKey)
	cmd := ResetCommand()
	resp, err := cmd.Execute(ctx, Request{Args: "hard"}, cc)
	if err != nil {
		t.Fatalf("/reset hard: unexpected error: %v", err)
	}
	if !strings.Contains(resp.Text, "hard") {
		t.Errorf("response = %q, want to mention 'hard'", resp.Text)
	}
}

// TestResetCommand_HardImmediate verifies that the parent /reset command
// stays non-Immediate (so its default path doesn't tie up the polling
// goroutine on memory formation), but the `hard` subcommand IS Immediate
// (so it can cancel a live turn before the worker gets to it).
func TestResetCommand_HardImmediate(t *testing.T) {
	cmd := ResetCommand()
	if cmd.Immediate {
		t.Error("parent /reset must not be Immediate (soft path can block on memory formation)")
	}
	var hardSub *Subcommand
	for i := range cmd.Subcommands {
		if cmd.Subcommands[i].Name == "hard" {
			hardSub = &cmd.Subcommands[i]
			break
		}
	}
	if hardSub == nil {
		t.Fatal("`hard` subcommand not registered on /reset")
	}
	if !hardSub.Immediate {
		t.Error("/reset hard subcommand must be Immediate (cancels live turn)")
	}

	// Registry-level: dispatch should classify "/reset hard" as immediate
	// but "/reset" alone as deferred.
	r := NewRegistry()
	r.Register(cmd)
	if !r.IsImmediateText("/reset hard") {
		t.Error("IsImmediateText(\"/reset hard\") = false, want true")
	}
	if r.IsImmediateText("/reset") {
		t.Error("IsImmediateText(\"/reset\") = true, want false")
	}
	if r.IsImmediateText("/reset soft") {
		t.Error("IsImmediateText(\"/reset soft\") = true, want false (no `soft` subcommand)")
	}
}

// TestResetCommand_BareShowsNoKeyboard verifies that bare /reset executes the
// soft reset directly via DefaultExecute rather than showing a one-option
// keyboard built from the [hard] subcommand list.
//
// The subcommand dispatch auto-wires KeyboardOptions from Subcommands when
// the field is nil. /reset suppresses that by setting KeyboardOptions to a
// no-op that returns nil — preserving "bare /reset = soft reset" semantics
// while keeping `/reset hard` discoverable via help/usage.
func TestResetCommand_BareShowsNoKeyboard(t *testing.T) {
	cmd := ResetCommand()
	r := NewRegistry()
	r.Register(cmd)

	_, _, opts, ok := r.LookupKeyboard(context.Background(), "/reset", CommandContext{})
	if ok {
		t.Errorf("LookupKeyboard(\"/reset\") returned ok=true with opts=%+v; bare /reset must execute via DefaultExecute, not show a keyboard", opts)
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
