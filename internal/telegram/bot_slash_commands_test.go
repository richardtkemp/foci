package telegram

import (
	"context"
	"testing"

	"foci/internal/command"
)

func TestReceiveMessage_SlashCommandBypassesQueue(t *testing.T) {
	// Verifies that slash commands go to the command channel (not the agent
	// queue) and are dispatched by the worker when it processes them.
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

	// Should be in the command channel, not the agent queue.
	if len(b.mq.Chan()) != 0 {
		t.Error("slash command should not be in the agent queue")
	}
	if len(b.mq.CmdChan()) != 1 {
		t.Fatalf("expected 1 command in cmdCh, got %d", len(b.mq.CmdChan()))
	}

	// Simulate worker dispatch.
	cmd := <-b.mq.CmdChan()
	b.processQueuedCommand(context.Background(), cmd)

	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message, got %d", mock.sentCount())
	}
}

func TestReceiveMessage_UnknownSlashCommandGetsSuggestion(t *testing.T) {
	// Verifies that unknown slash commands go to the command channel and
	// produce a suggestion reply when the worker processes them.
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

	// Should be in the command channel, not the agent queue.
	if len(b.mq.Chan()) != 0 {
		t.Fatalf("unknown slash command should not be in agent queue, got %d", len(b.mq.Chan()))
	}

	// Simulate worker dispatch.
	cmd := <-b.mq.CmdChan()
	b.processQueuedCommand(context.Background(), cmd)

	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 suggestion reply, got %d", mock.sentCount())
	}
}
