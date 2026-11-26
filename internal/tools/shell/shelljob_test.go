package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/usercmd"
)

// #3286: a shell job's command is the agent's, so the blocked-path check
// that "!" commands skip applies to it, and the {{secret:}} refusal stays. A
// leading sleep is allowed (a job blocks no turn).
func TestUserRunner_CheckJob(t *testing.T) {
	t.Parallel()
	r := userRunnerWithSecret(t, 10*time.Second, 15000)
	blocked := filepath.Join(r.WorkDir, "blocked.txt")
	if err := os.WriteFile(blocked, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.Store.AddBlockedPaths([]string{blocked})

	if err := r.CheckJob("cat " + blocked); err == nil || !strings.Contains(err.Error(), "blocked path") {
		t.Errorf("blocked path: err = %v", err)
	}
	if err := r.CheckJob("echo {{secret:brave.api_key}}"); err == nil {
		t.Error("{{secret:}} template: no error")
	}
	if err := r.CheckJob("sleep 60; gh pr merge 42"); err != nil {
		t.Errorf("leading sleep refused: %v", err)
	}
	// The "!" runner's own check is unchanged: no blocked-path check (#2812).
	if err := r.Check("cat " + blocked); err != nil {
		t.Errorf("Check (the ! path) now refuses a blocked path: %v", err)
	}
}

// RunFor applies the caller's timeout, not the runner's own.
func TestUserRunner_RunForTimeout(t *testing.T) {
	t.Parallel()
	r := userRunner(t, time.Hour, 15000)
	res := r.RunFor(context.Background(), "sleep 30", 200*time.Millisecond)
	if res.State != usercmd.StateTimedOut || res.Timeout != 200*time.Millisecond {
		t.Fatalf("state = %s timeout = %s, want timed out at 200ms", res.State, res.Timeout)
	}
	if res := r.Run(context.Background(), "echo hi"); res.Timeout != time.Hour || strings.TrimSpace(res.Output) != "hi" {
		t.Fatalf("Run: timeout %s output %q, want the runner's own hour", res.Timeout, res.Output)
	}
}
