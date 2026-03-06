package command

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestTmuxCommandList verifies tmux list sessions operation.
func TestTmuxCommandList(t *testing.T) {
	execFn, calls := mockTmuxExec("SESSION  W  AGE  STATUS\nwork  2w  1h  idle", nil)
	cmd := NewTmuxCommand(execFn)

	// Explicit "list" arg
	result, err := cmd.Execute(context.Background(), "list")
	if err != nil {
		t.Fatalf("Execute list: %v", err)
	}
	if !strings.Contains(result, "work") {
		t.Errorf("result = %q, want 'work'", result)
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
	execFn, calls := mockTmuxExec("Session started: myses", nil)
	cmd := NewTmuxCommand(execFn)

	result, err := cmd.Execute(context.Background(), "start myses sleep 60")
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
	if !strings.Contains(result, "Session started") {
		t.Errorf("result = %q, want 'Session started'", result)
	}
}

// TestTmuxCommandStartNoWatch verifies tmux session creation without auto-watch.
func TestTmuxCommandStartNoWatch(t *testing.T) {
	execFn, calls := mockTmuxExec("Session started: myses", nil)
	cmd := NewTmuxCommand(execFn)

	_, err := cmd.Execute(context.Background(), "start myses --no-watch sleep 60")
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
	execFn, calls := mockTmuxExec("Session started: foci-1", nil)
	cmd := NewTmuxCommand(execFn)

	_, err := cmd.Execute(context.Background(), "start")
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
	execFn, calls := mockTmuxExec("Keys sent.", nil)
	cmd := NewTmuxCommand(execFn)

	_, err := cmd.Execute(context.Background(), "send myses hello world")
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
	execFn, _ := mockTmuxExec("", nil)
	cmd := NewTmuxCommand(execFn)

	_, err := cmd.Execute(context.Background(), "send myses")
	if err == nil {
		t.Fatal("expected error for send with no keys")
	}
}

// TestTmuxCommandRead verifies reading output from a tmux session.
func TestTmuxCommandRead(t *testing.T) {
	execFn, calls := mockTmuxExec("some output here", nil)
	cmd := NewTmuxCommand(execFn)

	result, err := cmd.Execute(context.Background(), "read myses 100")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Should wrap in code block
	if !strings.HasPrefix(result, "```\n") || !strings.HasSuffix(result, "\n```") {
		t.Errorf("result not wrapped in code block: %q", result)
	}
	if !strings.Contains(result, "some output here") {
		t.Errorf("result missing output: %q", result)
	}
	// Check lines param
	if (*calls)[0]["lines"] != float64(100) { // JSON numbers are float64
		t.Errorf("lines = %v, want 100", (*calls)[0]["lines"])
	}
}

// TestTmuxCommandReadMissingName verifies error when read missing session name.
func TestTmuxCommandReadMissingName(t *testing.T) {
	execFn, _ := mockTmuxExec("", nil)
	cmd := NewTmuxCommand(execFn)

	_, err := cmd.Execute(context.Background(), "read")
	if err == nil {
		t.Fatal("expected error for read with no name")
	}
}

// TestTmuxCommandKill verifies killing a tmux session.
func TestTmuxCommandKill(t *testing.T) {
	execFn, calls := mockTmuxExec("Session killed: myses", nil)
	cmd := NewTmuxCommand(execFn)

	_, err := cmd.Execute(context.Background(), "kill myses")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if (*calls)[0]["name"] != "myses" {
		t.Errorf("name = %v, want myses", (*calls)[0]["name"])
	}
}

// TestTmuxCommandWatch verifies setting up tmux session watch with threshold.
func TestTmuxCommandWatch(t *testing.T) {
	execFn, calls := mockTmuxExec("Watching session myses", nil)
	cmd := NewTmuxCommand(execFn)

	_, err := cmd.Execute(context.Background(), "watch myses 60")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if (*calls)[0]["threshold_seconds"] != float64(60) {
		t.Errorf("threshold_seconds = %v, want 60", (*calls)[0]["threshold_seconds"])
	}
}

// TestTmuxCommandUnwatch verifies stopping watch on a tmux session.
func TestTmuxCommandUnwatch(t *testing.T) {
	execFn, calls := mockTmuxExec("Stopped watching session myses", nil)
	cmd := NewTmuxCommand(execFn)

	_, err := cmd.Execute(context.Background(), "unwatch myses")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if (*calls)[0]["name"] != "myses" {
		t.Errorf("name = %v, want myses", (*calls)[0]["name"])
	}
}

// TestTmuxCommandUnknownOp verifies usage message for unknown operation.
func TestTmuxCommandUnknownOp(t *testing.T) {
	execFn, _ := mockTmuxExec("", nil)
	cmd := NewTmuxCommand(execFn)

	result, err := cmd.Execute(context.Background(), "restart")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "Usage:") {
		t.Errorf("result = %q, want usage help", result)
	}
}

// TestTmuxCommandNoArgsShowsUsage verifies usage message when no args provided.
func TestTmuxCommandNoArgsShowsUsage(t *testing.T) {
	execFn, calls := mockTmuxExec("session1\nsession2\n", nil)
	cmd := NewTmuxCommand(execFn)

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "Usage:") {
		t.Errorf("result = %q, want usage help", result)
	}
	if !strings.Contains(result, "Commands:") {
		t.Errorf("result = %q, want commands list", result)
	}
	if len(*calls) > 0 {
		t.Errorf("execFn should not be called with no args, got calls: %v", *calls)
	}
}

// TestTmuxCommandError verifies error handling when tmux operation fails.
func TestTmuxCommandError(t *testing.T) {
	execFn, _ := mockTmuxExec("", fmt.Errorf("tmux not running"))
	cmd := NewTmuxCommand(execFn)

	_, err := cmd.Execute(context.Background(), "list")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "tmux not running") {
		t.Errorf("error = %q", err.Error())
	}
}
