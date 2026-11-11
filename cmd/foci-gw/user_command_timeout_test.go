package main

import (
	"context"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/tools/shell"
	"foci/internal/usercmd"
)

// TestUserCommandTimeout_LiveEdit is #3045: the limit of a chat "!" command is
// [tools] user_command_timeout, 15 minutes when unset on every backend, and an
// edit applied through the registered live applier reaches the next command a
// runner runs, with no rebuild of the runner.
func TestUserCommandTimeout_LiveEdit(t *testing.T) {
	t.Cleanup(func() { applyUserCommandTimeout(&config.Config{}) })

	applyUserCommandTimeout(&config.Config{})
	if got := userCommandTimeout(); got != 15*time.Minute {
		t.Fatalf("unset = %s, want 15m", got)
	}

	la := newLiveApply("", nil)
	registerLiveAppliers(la, registryFromMap(nil))
	applier := la.appliers["tools.user_command_timeout"]
	if applier == nil {
		t.Fatal("no live applier for tools.user_command_timeout")
	}

	r := &shell.UserRunner{WorkDir: t.TempDir(), Timeout: userCommandTimeout}

	if err := applier(&config.Config{Tools: config.ToolsConfig{UserCommandTimeout: "2h"}}); err != nil {
		t.Fatalf("applier: %v", err)
	}
	if res := r.Run(context.Background(), "true"); res.Timeout != 2*time.Hour || res.State != usercmd.StateExited {
		t.Fatalf("after edit to 2h: timeout %s, state %v; want 2h, exited", res.Timeout, res.State)
	}

	if err := applier(&config.Config{Tools: config.ToolsConfig{UserCommandTimeout: "300ms"}}); err != nil {
		t.Fatalf("applier: %v", err)
	}
	start := time.Now()
	res := r.Run(context.Background(), "sleep 30")
	if res.State != usercmd.StateTimedOut || res.Timeout != 300*time.Millisecond {
		t.Fatalf("after edit to 300ms: state %v, timeout %s; want timed out at 300ms", res.State, res.Timeout)
	}
	if el := time.Since(start); el > 20*time.Second {
		t.Errorf("sleep 30 took %s: the live limit did not stop it", el)
	}
}
