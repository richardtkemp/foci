package dispatch

import (
	"context"
	"testing"

	"foci/internal/command"
)

// TestDispatchTextDotCommand verifies that dot-prefix commands (.model) are
// routed through the registry and return a handled result.
func TestDispatchTextDotCommand(t *testing.T) {
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "model",
		Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "current model: haiku"}, nil
		},
	})

	d := NewDispatcher(reg, command.CommandContext{}, "agent1")

	result := d.DispatchText(context.Background(), ".model", 123, "user1")
	if !result.Handled {
		t.Fatal("expected dot command to be handled")
	}
	if result.Response.Text != "current model: haiku" {
		t.Errorf("unexpected response: %q", result.Response.Text)
	}
	if result.UserID != "user1" {
		t.Errorf("unexpected userID: %q", result.UserID)
	}
}

// TestDispatchTextDotCommandWithArgs verifies that dot-prefix commands with
// arguments pass the args through correctly.
func TestDispatchTextDotCommandWithArgs(t *testing.T) {
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "model",
		Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "switching to " + req.Args}, nil
		},
	})

	d := NewDispatcher(reg, command.CommandContext{}, "agent1")

	result := d.DispatchText(context.Background(), ".model opus", 123, "user1")
	if !result.Handled {
		t.Fatal("expected dot command with args to be handled")
	}
	if result.Response.Text != "switching to opus" {
		t.Errorf("unexpected response: %q", result.Response.Text)
	}
}

// TestDispatchTextSlashCommand verifies that slash-prefix commands (/status)
// are routed correctly.
func TestDispatchTextSlashCommand(t *testing.T) {
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "status",
		Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "all systems go"}, nil
		},
	})

	d := NewDispatcher(reg, command.CommandContext{}, "agent1")

	result := d.DispatchText(context.Background(), "/status", 99999, "user1")
	if !result.Handled {
		t.Fatal("expected slash command to be handled")
	}
	if result.Response.Text != "all systems go" {
		t.Errorf("unexpected response: %q", result.Response.Text)
	}
}

// TestDispatchTextUnknownDotCommand verifies that unknown dot-commands are not
// handled (registry.Get returns nil).
func TestDispatchTextUnknownDotCommand(t *testing.T) {
	reg := command.NewRegistry()
	d := NewDispatcher(reg, command.CommandContext{}, "agent1")

	result := d.DispatchText(context.Background(), ".nosuchcommand", 123, "user1")
	if result.Handled {
		t.Error("expected unknown dot command to not be handled")
	}
}

// TestDispatchTextPlainText verifies that plain text (no prefix) is not
// dispatched.
func TestDispatchTextPlainText(t *testing.T) {
	reg := command.NewRegistry()
	d := NewDispatcher(reg, command.CommandContext{}, "agent1")

	result := d.DispatchText(context.Background(), "just a normal message", 123, "user1")
	if result.Handled {
		t.Error("expected plain text to not be handled")
	}
}

// TestDispatchCallback verifies that DispatchCallback routes a /command text
// correctly (as used by button/callback interactions).
func TestDispatchCallback(t *testing.T) {
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "model",
		Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "set to " + req.Args}, nil
		},
	})

	d := NewDispatcher(reg, command.CommandContext{}, "agent1")

	result := d.DispatchCallback(context.Background(), 12345, "/model opus")
	if !result.Handled {
		t.Fatal("expected callback command to be handled")
	}
	if result.Response.Text != "set to opus" {
		t.Errorf("unexpected response: %q", result.Response.Text)
	}
}

// TestSetSessionKeyFunc verifies that a custom session key resolver overrides
// the default NewChatSessionKey derivation.
func TestSetSessionKeyFunc(t *testing.T) {
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "echo",
		Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: req.SessionKey}, nil
		},
	})

	d := NewDispatcher(reg, command.CommandContext{}, "agent1")
	d.SetSessionKeyFunc(func(chatID int64) string {
		return "custom-key"
	})

	result := d.DispatchText(context.Background(), "/echo", 123, "user1")
	if !result.Handled {
		t.Fatal("expected command to be handled")
	}
	if result.SessionKey != "custom-key" {
		t.Errorf("expected custom session key, got %q", result.SessionKey)
	}
}
