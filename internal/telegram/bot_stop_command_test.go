package telegram

import (
	"context"
	"testing"

	"foci/internal/command"
	"foci/internal/dispatch"
)

func TestReceiveMessage_StopCancelsTurn(t *testing.T) {
	// Verifies that /stop cancels an active turn via the command registry.
	cmds := command.NewRegistry()
	cmds.Register(command.StopCommand())

	b, mock := testBot([]string{"111"}, cmds)

	// Wire StopFunc via SetCommandContext
	cc := command.CommandContext{
		StopFunc: b.cancelTurn,
	}
	b.dispatcher = dispatch.NewDispatcher(cmds, cc, b.agentID)

	// Simulate an active turn

	msg := makeMsg(111, "owner", "/stop")
	b.receiveMessage(context.Background(), msg)

	// Should NOT be queued
	if len(b.mq.Chan()) != 0 {
		t.Error("/stop should not be queued")
	}

	// Should have sent "Stopped." reply
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message for /stop, got %d", mock.sentCount())
	}
}

func TestReceiveMessage_DotStopCancelsTurn(t *testing.T) {
	// Verifies that .stop is treated as immediate (Immediate=true on StopCommand)
	// and dispatched in the polling goroutine to cancel a live turn.
	cmds := command.NewRegistry()
	cmds.Register(command.StopCommand())

	b, mock := testBot([]string{"111"}, cmds)

	cc := command.CommandContext{
		StopFunc: b.cancelTurn,
	}
	b.dispatcher = dispatch.NewDispatcher(cmds, cc, b.agentID)

	msg := makeMsg(111, "owner", ".stop")
	b.receiveMessage(context.Background(), msg)

	if len(b.mq.Chan()) != 0 {
		t.Error(".stop should not be in agent queue")
	}
	if len(b.mq.CmdChan()) != 0 {
		t.Error(".stop should not be deferred (it is Immediate)")
	}
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message for .stop, got %d", mock.sentCount())
	}
}

func TestReceiveMessage_StopAlias(t *testing.T) {
	// Verifies that a stop alias registered with Immediate:true is dispatched
	// in the polling goroutine, cancelling a live turn immediately.
	cmds := command.NewRegistry()
	stopCmd := command.StopCommand()
	cmds.Register(stopCmd)
	// Register "wait" as an immediate alias for "stop".
	// Immediate:true is required for the alias to cancel live turns.
	cmds.Register(&command.Command{Name: "wait", Hidden: true, Immediate: true, Execute: stopCmd.Execute})

	b, mock := testBot([]string{"111"}, cmds)

	cc := command.CommandContext{
		StopFunc: b.cancelTurn,
	}
	b.dispatcher = dispatch.NewDispatcher(cmds, cc, b.agentID)

	// Simulate an active turn

	msg := makeMsg(111, "owner", "/wait")
	b.receiveMessage(context.Background(), msg)

	if len(b.mq.Chan()) != 0 {
		t.Error("/wait should not be in agent queue")
	}
	if len(b.mq.CmdChan()) != 0 {
		t.Error("/wait should not be deferred (it is Immediate)")
	}
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message for /wait, got %d", mock.sentCount())
	}
}

func TestReceiveMessage_StopAliasNotConfigured(t *testing.T) {
	// Verifies that /wait without Immediate:true is deferred to the command
	// channel and produces a suggestion reply when the worker dispatches it.
	cmds := command.NewRegistry()
	cmds.Register(command.StopCommand())
	// /wait is not registered at all

	b, mock := testBot([]string{"111"}, cmds)

	cc := command.CommandContext{
		StopFunc: b.cancelTurn,
	}
	b.dispatcher = dispatch.NewDispatcher(cmds, cc, b.agentID)

	msg := makeMsg(111, "owner", "/wait")
	b.receiveMessage(context.Background(), msg)

	// Should be in the command channel (unrecognised command, but still a command).
	if len(b.mq.Chan()) != 0 {
		t.Fatalf("expected 0 in agent queue, got %d", len(b.mq.Chan()))
	}

	// Simulate worker dispatch — should produce a suggestion reply.
	cmd := <-b.mq.CmdChan()
	b.processQueuedCommand(context.Background(), cmd)

	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 suggestion reply for unknown /wait, got %d", mock.sentCount())
	}
}
