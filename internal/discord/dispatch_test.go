package discord

import (
	"context"
	"testing"

	"foci/internal/command"

	"github.com/bwmarrin/discordgo"
)

// TestDispatchDotCommand verifies that dot-prefix commands (.model) are routed correctly.
func TestDispatchDotCommand(t *testing.T) {
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "model",
		Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "current model: haiku"}, nil
		},
	})

	d := NewDispatcher(reg, command.CommandContext{}, "agent1")

	msg := &discordgo.Message{
		Content:   ".model",
		ChannelID: "12345",
		Author:    &discordgo.User{ID: "user1"},
	}

	result := d.Dispatch(context.Background(), msg)
	if !result.Handled {
		t.Fatal("expected dot command to be handled")
	}
	if result.Response.Text != "current model: haiku" {
		t.Errorf("unexpected response: %q", result.Response.Text)
	}
}

// TestDispatchDotCommandWithArgs verifies that dot-prefix commands with arguments work.
func TestDispatchDotCommandWithArgs(t *testing.T) {
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "model",
		Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "switching to " + req.Args}, nil
		},
	})

	d := NewDispatcher(reg, command.CommandContext{}, "agent1")

	msg := &discordgo.Message{
		Content:   ".model opus",
		ChannelID: "12345",
		Author:    &discordgo.User{ID: "user1"},
	}

	result := d.Dispatch(context.Background(), msg)
	if !result.Handled {
		t.Fatal("expected dot command to be handled")
	}
	if result.Response.Text != "switching to opus" {
		t.Errorf("unexpected response: %q", result.Response.Text)
	}
}

// TestDispatchSlashCommand verifies that slash-prefix commands (/status) are routed correctly.
func TestDispatchSlashCommand(t *testing.T) {
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "status",
		Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "all systems go"}, nil
		},
	})

	d := NewDispatcher(reg, command.CommandContext{}, "agent1")

	msg := &discordgo.Message{
		Content:   "/status",
		ChannelID: "99999",
		Author:    &discordgo.User{ID: "user1"},
	}

	result := d.Dispatch(context.Background(), msg)
	if !result.Handled {
		t.Fatal("expected slash command to be handled")
	}
	if result.Response.Text != "all systems go" {
		t.Errorf("unexpected response: %q", result.Response.Text)
	}
}

// TestDispatchSlashStopNotHandled verifies that /stop is NOT handled by the dispatcher
// (it's handled locally by the bot's command handler).
func TestDispatchSlashStopNotHandled(t *testing.T) {
	reg := command.NewRegistry()
	d := NewDispatcher(reg, command.CommandContext{}, "agent1")

	msg := &discordgo.Message{
		Content:   "/stop",
		ChannelID: "12345",
		Author:    &discordgo.User{ID: "user1"},
	}

	result := d.Dispatch(context.Background(), msg)
	if result.Handled {
		t.Error("expected /stop to not be handled by dispatcher")
	}
}

// TestDispatchSlashDoneNotHandled verifies that /done is NOT handled by the dispatcher.
func TestDispatchSlashDoneNotHandled(t *testing.T) {
	reg := command.NewRegistry()
	d := NewDispatcher(reg, command.CommandContext{}, "agent1")

	msg := &discordgo.Message{
		Content:   "/done",
		ChannelID: "12345",
		Author:    &discordgo.User{ID: "user1"},
	}

	result := d.Dispatch(context.Background(), msg)
	if result.Handled {
		t.Error("expected /done to not be handled by dispatcher")
	}
}

// TestDispatchUnknownCommand verifies that unknown commands are not handled.
func TestDispatchUnknownCommand(t *testing.T) {
	reg := command.NewRegistry()
	d := NewDispatcher(reg, command.CommandContext{}, "agent1")

	msg := &discordgo.Message{
		Content:   ".nosuchcommand",
		ChannelID: "12345",
		Author:    &discordgo.User{ID: "user1"},
	}

	result := d.Dispatch(context.Background(), msg)
	if result.Handled {
		t.Error("expected unknown command to not be handled")
	}
}

// TestDispatchPlainTextNotHandled verifies that plain text messages are not routed.
func TestDispatchPlainTextNotHandled(t *testing.T) {
	reg := command.NewRegistry()
	d := NewDispatcher(reg, command.CommandContext{}, "agent1")

	msg := &discordgo.Message{
		Content:   "just a normal message",
		ChannelID: "12345",
		Author:    &discordgo.User{ID: "user1"},
	}

	result := d.Dispatch(context.Background(), msg)
	if result.Handled {
		t.Error("expected plain text to not be handled")
	}
}

// TestDispatchCallbackCommand verifies that button callback dispatch works.
func TestDispatchCallbackCommand(t *testing.T) {
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
