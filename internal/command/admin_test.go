package command

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/provider"
	"foci/internal/workspace"
)

// TestReloadCommand_ReplyFormat proves /reload's reply reports the skill count
// returned by Agent.ReloadSystem and notes that foci.toml changes need a
// restart. The reload mechanics are covered by agent.ReloadSystem's own tests;
// this covers only the command's reply formatting — which the L2 suite no
// longer exercises now that /reload is gated to API backends (TODO #799).
func TestReloadCommand_ReplyFormat(t *testing.T) {
	ag := &agent.Agent{
		Bootstrap:      workspace.NewBootstrap(t.TempDir(), nil),
		ReloadSystemFn: func() ([]provider.SystemBlock, int) { return nil, 3 },
	}
	resp, err := ReloadCommand().Execute(context.Background(), Request{}, CommandContext{Agent: ag})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"Reloaded", "3 skills", "foci.toml", "restart"} {
		if !strings.Contains(resp.Text, want) {
			t.Errorf("reply missing %q; got: %q", want, resp.Text)
		}
	}
}

// TestRestartCommand_Fallback verifies that the /restart command invokes the
// restart function and returns its message. Uses a stub to avoid actually
// restarting anything.
func TestRestartCommand_Fallback(t *testing.T) {
	original := restartFunc
	t.Cleanup(func() { restartFunc = original })

	restartFunc = func() (string, error) {
		return "stub restart triggered", nil
	}

	cmd := RestartCommand()
	resp, err := cmd.Execute(context.Background(), Request{}, CommandContext{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Text != "stub restart triggered" {
		t.Errorf("got %q, want %q", resp.Text, "stub restart triggered")
	}
}

// TestRestartCommand_Error verifies that errors from the restart function
// are propagated correctly.
func TestRestartCommand_Error(t *testing.T) {
	original := restartFunc
	t.Cleanup(func() { restartFunc = original })

	restartFunc = func() (string, error) {
		return "", fmt.Errorf("no restart mechanism available")
	}

	cmd := RestartCommand()
	_, err := cmd.Execute(context.Background(), Request{}, CommandContext{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no restart mechanism") {
		t.Errorf("error = %q, want it to contain %q", err, "no restart mechanism")
	}
}

// TestDoRestart_Assignable verifies doRestart has the expected signature and
// can be used as the restartFunc value.
func TestDoRestart_Assignable(t *testing.T) {
	// Verify doRestart satisfies the restartFunc type by assigning it.
	var fn func() (string, error) = doRestart
	_ = fn
}
