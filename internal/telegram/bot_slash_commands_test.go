package telegram

import (
	"context"
	"testing"

	"foci/internal/command"
)

func TestReceiveMessage_SlashCommandBypassesQueue(t *testing.T) {
	// Verifies that slash commands
	// are executed immediately and do not go through the agent queue.
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{
		Name:        "ping",
		Description: "test",
		Execute: func(ctx context.Context, req command.Request, cc command.CommandContext) (command.Response, error) {
			return command.Response{Text: "pong"}, nil
		},
	})

	b, mock := testBot([]string{"111"}, cmds)

	msg := makeMsg(111, "owner", "/ping")
	b.receiveMessage(context.Background(), msg)

	// Should NOT be queued (command was handled directly)
	if len(b.queue) != 0 {
		t.Error("slash command should not be queued for agent")
	}

	// Should have sent a reply with the command result
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message, got %d", mock.sentCount())
	}
}

func TestReceiveMessage_UnknownSlashCommandGetsSuggestion(t *testing.T) {
	// Verifies that unknown
	// slash commands receive a suggestion reply instead of being queued.
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{
		Name: "ping",
		Execute: func(ctx context.Context, req command.Request, cc command.CommandContext) (command.Response, error) {
			return command.Response{Text: "pong"}, nil
		},
	})

	b, mock := testBot([]string{"111"}, cmds)

	msg := makeMsg(111, "owner", "/unknown_cmd")
	b.receiveMessage(context.Background(), msg)

	// Unknown commands should get a suggestion reply, not be queued
	if len(b.queue) != 0 {
		t.Fatalf("unknown slash command should not be queued, got %d queued", len(b.queue))
	}
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 suggestion reply, got %d", mock.sentCount())
	}
}
