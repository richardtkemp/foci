package shell

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foci/internal/clock"
	"foci/internal/tools"
)

// newTestExecTool creates an ExecTool with all-zero/nil defaults.
func newTestExecTool() *tools.Tool {
	return NewExecTool(nil, nil, func() int { return 0 }, nil, "", nil, func() int64 { return 0 }, "", nil, 0)
}

// runExec marshals a command string into exec params and executes it.
func runExec(t *testing.T, tool *tools.Tool, command string) (tools.ToolResult, error) {
	t.Helper()
	params, _ := json.Marshal(map[string]interface{}{"command": command})
	return tool.Execute(context.Background(), params)
}

// requireError asserts err is non-nil and contains substr.
func requireError(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Errorf("error = %q, want substring %q", err.Error(), substr)
	}
}

// requireNoError fails the test if err is non-nil.
func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// requireContains asserts that s contains substr.
func requireContains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected %q to contain %q", s, substr)
	}
}

// executePastThreshold runs exec (a tool.Execute call whose notifier runs on
// fc) on its own goroutine, waits for RunInBackground to arm its threshold
// timer, advances fc past threshold, and returns what exec returned. The
// threshold passes in virtual time, so the test costs no wall-clock wait
// (#2539); the command must still be held (gatedCommand) so it cannot finish
// first. Mirrors the helper of the same name in internal/tools.
func executePastThreshold(t *testing.T, fc *clock.Fake, threshold time.Duration, exec func() (tools.ToolResult, error)) (tools.ToolResult, error) {
	t.Helper()
	type outcome struct {
		res tools.ToolResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := exec()
		done <- outcome{res, err}
	}()
	if !fc.BlockUntilArmed(1, 10*time.Second) {
		t.Fatal("the auto-background threshold timer was never armed")
	}
	fc.Advance(threshold)
	select {
	case o := <-done:
		return o.res, o.err
	case <-time.After(10 * time.Second):
		t.Fatal("Execute did not return after the threshold passed")
		return tools.ToolResult{}, nil
	}
}
