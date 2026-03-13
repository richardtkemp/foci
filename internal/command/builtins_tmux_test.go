package command

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"foci/internal/tools"
)

// tmuxCC builds a CommandContext with a mock TmuxTool for testing.
func tmuxCC(result string, err error) (CommandContext, *[]map[string]interface{}) {
	execFn, calls := mockTmuxExec(result, err)
	return CommandContext{
		TmuxTool: &tools.Tool{
			Name:    "tmux",
			Execute: execFn,
		},
	}, calls
}

// TestTmuxCommandList verifies tmux list sessions operation.
func TestTmuxCommandList(t *testing.T) {
	cc, calls := tmuxCC("SESSION  W  AGE  STATUS\nwork  2w  1h  idle", nil)
	cmd := TmuxCommand()

	// Explicit "list" arg
	result, err := cmd.Execute(context.Background(), Request{Args: "list"}, cc)
	if err != nil {
		t.Fatalf("Execute list: %v", err)
	}
	if !strings.Contains(result.Text, "work") {
		t.Errorf("result = %q, want 'work'", result.Text)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(*calls))
	}
	if (*calls)[0]["operation"] != "list" {
		t.Errorf("operation = %v, want list", (*calls)[0]["operation"])
	}
}

// TestTmuxCommandStart verifies tmux session creation with auto-watch.
func TestTmuxCommandStart(t *testing.T) {
	cc, calls := tmuxCC("Session started: myses", nil)
	cmd := TmuxCommand()

	result, err := cmd.Execute(context.Background(), Request{Args: "start myses sleep 60"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Should have 2 calls: start + auto-watch
	if len(*calls) != 2 {
		t.Fatalf("calls = %d, want 2 (start + watch)", len(*calls))
	}
	if (*calls)[0]["operation"] != "start" {
		t.Errorf("call[0] operation = %v, want start", (*calls)[0]["operation"])
	}
	if (*calls)[0]["name"] != "myses" {
		t.Errorf("call[0] name = %v, want myses", (*calls)[0]["name"])
	}
	if (*calls)[0]["command"] != "sleep 60" {
		t.Errorf("call[0] command = %v, want 'sleep 60'", (*calls)[0]["command"])
	}
	if (*calls)[1]["operation"] != "watch" {
		t.Errorf("call[1] operation = %v, want watch", (*calls)[1]["operation"])
	}
	if (*calls)[1]["name"] != "myses" {
		t.Errorf("call[1] name = %v, want myses", (*calls)[1]["name"])
	}
	if !strings.Contains(result.Text, "Session started") {
		t.Errorf("result = %q, want 'Session started'", result.Text)
	}
}

// TestTmuxCommandStartNoWatch verifies tmux session creation without auto-watch.
func TestTmuxCommandStartNoWatch(t *testing.T) {
	cc, calls := tmuxCC("Session started: myses", nil)
	cmd := TmuxCommand()

	_, err := cmd.Execute(context.Background(), Request{Args: "start myses --no-watch sleep 60"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Should have only 1 call: start (no watch)
	if len(*calls) != 1 {
		t.Fatalf("calls = %d, want 1 (start only)", len(*calls))
	}
	if (*calls)[0]["operation"] != "start" {
		t.Errorf("operation = %v, want start", (*calls)[0]["operation"])
	}
}

// TestTmuxCommandStartAutoName verifies auto-generated session names are tracked for watch.
func TestTmuxCommandStartAutoName(t *testing.T) {
	cc, calls := tmuxCC("Session started: foci-1", nil)
	cmd := TmuxCommand()

	_, err := cmd.Execute(context.Background(), Request{Args: "start"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Auto-watch should parse name from result
	if len(*calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(*calls))
	}
	if (*calls)[1]["name"] != "foci-1" {
		t.Errorf("watch name = %v, want foci-1", (*calls)[1]["name"])
	}
}

// TestTmuxCommandSend verifies sending keys to a tmux session.
func TestTmuxCommandSend(t *testing.T) {
	cc, calls := tmuxCC("Keys sent.", nil)
	cmd := TmuxCommand()

	_, err := cmd.Execute(context.Background(), Request{Args: "send myses hello world"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(*calls))
	}
	if (*calls)[0]["keys"] != "hello world" {
		t.Errorf("keys = %v, want 'hello world'", (*calls)[0]["keys"])
	}
}

// TestTmuxCommandSendMissingArgs verifies error when send missing keys.
func TestTmuxCommandSendMissingArgs(t *testing.T) {
	cc, _ := tmuxCC("", nil)
	cmd := TmuxCommand()

	_, err := cmd.Execute(context.Background(), Request{Args: "send myses"}, cc)
	if err == nil {
		t.Fatal("expected error for send with no keys")
	}
}

// TestTmuxCommandRead verifies reading output from a tmux session.
func TestTmuxCommandRead(t *testing.T) {
	cc, calls := tmuxCC("some output here", nil)
	cmd := TmuxCommand()

	result, err := cmd.Execute(context.Background(), Request{Args: "read myses 100"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Should wrap in code block
	if !strings.HasPrefix(result.Text, "```\n") || !strings.HasSuffix(result.Text, "\n```") {
		t.Errorf("result not wrapped in code block: %q", result.Text)
	}
	if !strings.Contains(result.Text, "some output here") {
		t.Errorf("result missing output: %q", result.Text)
	}
	// Check lines param
	if (*calls)[0]["lines"] != float64(100) { // JSON numbers are float64
		t.Errorf("lines = %v, want 100", (*calls)[0]["lines"])
	}
}

// TestTmuxCommandReadMissingName verifies error when read missing session name.
func TestTmuxCommandReadMissingName(t *testing.T) {
	cc, _ := tmuxCC("", nil)
	cmd := TmuxCommand()

	_, err := cmd.Execute(context.Background(), Request{Args: "read"}, cc)
	if err == nil {
		t.Fatal("expected error for read with no name")
	}
}

// TestTmuxCommandKill verifies killing a tmux session.
func TestTmuxCommandKill(t *testing.T) {
	cc, calls := tmuxCC("Session killed: myses", nil)
	cmd := TmuxCommand()

	_, err := cmd.Execute(context.Background(), Request{Args: "kill myses"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if (*calls)[0]["name"] != "myses" {
		t.Errorf("name = %v, want myses", (*calls)[0]["name"])
	}
}

// TestTmuxCommandWatch verifies setting up tmux session watch with threshold.
func TestTmuxCommandWatch(t *testing.T) {
	cc, calls := tmuxCC("Watching session myses", nil)
	cmd := TmuxCommand()

	_, err := cmd.Execute(context.Background(), Request{Args: "watch myses 60"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if (*calls)[0]["threshold_seconds"] != float64(60) {
		t.Errorf("threshold_seconds = %v, want 60", (*calls)[0]["threshold_seconds"])
	}
}

// TestTmuxCommandUnwatch verifies stopping watch on a tmux session.
func TestTmuxCommandUnwatch(t *testing.T) {
	cc, calls := tmuxCC("Stopped watching session myses", nil)
	cmd := TmuxCommand()

	_, err := cmd.Execute(context.Background(), Request{Args: "unwatch myses"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if (*calls)[0]["name"] != "myses" {
		t.Errorf("name = %v, want myses", (*calls)[0]["name"])
	}
}

// TestTmuxCommandUnknownOp verifies usage message for unknown operation.
func TestTmuxCommandUnknownOp(t *testing.T) {
	cc, _ := tmuxCC("", nil)
	cmd := TmuxCommand()

	result, err := cmd.Execute(context.Background(), Request{Args: "restart"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Usage:") {
		t.Errorf("result = %q, want usage help", result.Text)
	}
}

// TestTmuxCommandNoArgsShowsUsage verifies usage message when no args provided.
func TestTmuxCommandNoArgsShowsUsage(t *testing.T) {
	cc, calls := tmuxCC("session1\nsession2\n", nil)
	cmd := TmuxCommand()

	result, err := cmd.Execute(context.Background(), Request{Args: ""}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Usage:") {
		t.Errorf("result = %q, want usage help", result.Text)
	}
	if !strings.Contains(result.Text, "Commands:") {
		t.Errorf("result = %q, want commands list", result.Text)
	}
	if len(*calls) > 0 {
		t.Errorf("execFn should not be called with no args, got calls: %v", *calls)
	}
}

// TestTmuxCommandError verifies error handling when tmux operation fails.
func TestTmuxCommandError(t *testing.T) {
	cc, _ := tmuxCC("", fmt.Errorf("tmux not running"))
	cmd := TmuxCommand()

	_, err := cmd.Execute(context.Background(), Request{Args: "list"}, cc)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "tmux not running") {
		t.Errorf("error = %q", err.Error())
	}
}
