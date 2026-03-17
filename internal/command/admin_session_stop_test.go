package command

import (
	"context"
	"testing"
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
		IsSecondaryBot:    true,
		DefaultSessionKey: func() string { return "" },
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
		IsSecondaryBot:    true,
		DefaultSessionKey: func() string { return "agent:main:facet:f-1" },
		StopFunc:          func() { stopCalled = true },
		ReleaseFunc:       func() { releaseCalled = true },
	}
	resp, err := cmd.Execute(context.Background(), Request{}, cc)
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
		IsSecondaryBot:    true,
		DefaultSessionKey: func() string { return "agent:main:facet:f-1" },
		// StopFunc and ReleaseFunc are nil
	}
	resp, err := cmd.Execute(context.Background(), Request{}, cc)
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
