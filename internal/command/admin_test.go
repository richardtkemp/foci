package command

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

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
