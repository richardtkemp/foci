package command

import (
	"context"
	"testing"

	"foci/internal/tools"
)

func TestStopCommand_NilStopFunc(t *testing.T) {
	// Verifies that StopCommand handles a nil StopFunc gracefully.
	cmd := StopCommand()
	cc := CommandContext{} // StopFunc is nil
	resp, err := cmd.Execute(context.Background(), Request{}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != "Stopped." {
		t.Errorf("response = %q, want %q", resp.Text, "Stopped.")
	}
}

func TestStopCommand_CallsStopFunc(t *testing.T) {
	// Verifies that StopCommand calls StopFunc when non-nil.
	called := false
	cmd := StopCommand()
	cc := CommandContext{
		StopFunc: func() { called = true },
	}
	resp, err := cmd.Execute(context.Background(), Request{}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("StopFunc should have been called")
	}
	if resp.Text != "Stopped." {
		t.Errorf("response = %q, want %q", resp.Text, "Stopped.")
	}
}

func TestDoneCommand_PrimaryBot(t *testing.T) {
	// Verifies that DoneCommand on a primary bot (IsSecondaryBot=false)
	// returns "nothing to detach".
	cmd := DoneCommand()
	cc := CommandContext{
		IsSecondaryBot: false,
	}
	resp, err := cmd.Execute(context.Background(), Request{}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != "Nothing to detach — this is the main session." {
		t.Errorf("response = %q", resp.Text)
	}
}

func TestDoneCommand_SecondaryIdleBotSolomon(t *testing.T) {
	// Verifies that DoneCommand on a secondary bot with no session
	// returns "already idle".
	cmd := DoneCommand()
	cc := CommandContext{
		IsSecondaryBot: true,
	}
	resp, err := cmd.Execute(context.Background(), Request{}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != "Already idle." {
		t.Errorf("response = %q", resp.Text)
	}
}

func TestDoneCommand_SecondaryBotWithSession(t *testing.T) {
	// Verifies that DoneCommand on a secondary bot with an active session
	// calls StopFunc and ReleaseFunc and returns "session ended".
	stopCalled := false
	releaseCalled := false

	cmd := DoneCommand()
	cc := CommandContext{
		IsSecondaryBot: true,
		StopFunc:       func() { stopCalled = true },
		ReleaseFunc:    func() { releaseCalled = true },
	}
	ctx := tools.WithSessionKey(context.Background(), "agent:main:facet:f-1")
	resp, err := cmd.Execute(ctx, Request{}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != "Session ended." {
		t.Errorf("response = %q, want %q", resp.Text, "Session ended.")
	}
	if !stopCalled {
		t.Error("StopFunc should have been called")
	}
	if !releaseCalled {
		t.Error("ReleaseFunc should have been called")
	}
}

func TestDoneCommand_NilFuncs(t *testing.T) {
	// Verifies that DoneCommand handles nil StopFunc and ReleaseFunc gracefully
	// on a secondary bot with a session.
	cmd := DoneCommand()
	cc := CommandContext{
		IsSecondaryBot: true,
		// StopFunc and ReleaseFunc are nil
	}
	ctx := tools.WithSessionKey(context.Background(), "agent:main:facet:f-1")
	resp, err := cmd.Execute(ctx, Request{}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != "Session ended." {
		t.Errorf("response = %q, want %q", resp.Text, "Session ended.")
	}
}

func TestStopCommand_IsVisible(t *testing.T) {
	// Verifies that StopCommand is not hidden (shows in command listings).
	cmd := StopCommand()
	if cmd.Hidden {
		t.Error("stop command should not be hidden")
	}
}

func TestDoneCommand_IsHidden(t *testing.T) {
	// Verifies that DoneCommand is hidden (doesn't show in command listings).
	cmd := DoneCommand()
	if !cmd.Hidden {
		t.Error("done command should be hidden")
	}
}

// TestStopCommand_Args pins the #2138/#2140 arguments: "subagents" and
// "commands" with no delegated backend have nothing to stop and must not
// cancel the turn, "all" stops the turn, and an unknown argument is an error
// rather than a silent plain stop.
func TestStopCommand_Args(t *testing.T) {
	cmd := StopCommand()
	called := false
	cc := CommandContext{StopFunc: func() { called = true }}

	for arg, want := range map[string]string{"subagents": "No subagents to stop.", "commands": "No commands to stop."} {
		resp, err := cmd.Execute(context.Background(), Request{Args: arg}, cc)
		if err != nil || resp.Text != want || called {
			t.Fatalf("%s: resp=%q err=%v turnStopped=%v", arg, resp.Text, err, called)
		}
	}
	resp, err := cmd.Execute(context.Background(), Request{Args: "all"}, cc)
	if err != nil || !called || resp.Text != "Stopped." {
		t.Fatalf("all: resp=%q err=%v turnStopped=%v", resp.Text, err, called)
	}
	if _, err := cmd.Execute(context.Background(), Request{Args: "everything"}, cc); err == nil {
		t.Fatal("unknown argument: want an error")
	}
}
