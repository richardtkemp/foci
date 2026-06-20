package command

import (
	"context"
	"strings"
	"testing"

	"foci/internal/agent"
)

func TestLoginCommand_NoTrigger(t *testing.T) {
	// Agent with no ReloginTrigger (e.g. cctmux or API) → reports unavailable.
	cmd := LoginCommand()
	cc := CommandContext{Agent: &agent.Agent{}}
	resp, err := cmd.Execute(context.Background(), Request{}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Text, "only available on the ccstream backend") {
		t.Errorf("response = %q, want unavailable message", resp.Text)
	}
}

func TestLoginCommand_Starts(t *testing.T) {
	called := false
	var gotKey string
	cmd := LoginCommand()
	cc := CommandContext{Agent: &agent.Agent{
		ReloginTrigger: func(reason, sessionKey string) bool {
			called = true
			gotKey = sessionKey
			return true
		},
	}}
	resp, err := cmd.Execute(context.Background(), Request{SessionKey: "clutch/c123/456"}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("ReloginTrigger should have been called")
	}
	// The triggering session key must be threaded through so the login URL is
	// delivered to the chat that ran /login (#843).
	if gotKey != "clutch/c123/456" {
		t.Errorf("session key = %q, want %q", gotKey, "clutch/c123/456")
	}
	if !strings.Contains(resp.Text, "Re-login started") {
		t.Errorf("response = %q, want started message", resp.Text)
	}
}

func TestLoginCommand_AlreadyRunning(t *testing.T) {
	// Trigger returns false → a re-login is already in flight.
	cmd := LoginCommand()
	cc := CommandContext{Agent: &agent.Agent{
		ReloginTrigger: func(reason, sessionKey string) bool { return false },
	}}
	resp, err := cmd.Execute(context.Background(), Request{}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Text, "already in progress") {
		t.Errorf("response = %q, want already-in-progress message", resp.Text)
	}
}
