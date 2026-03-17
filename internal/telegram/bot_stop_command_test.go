package telegram

import (
	"context"
	"testing"

	"foci/internal/command"
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
	b.dispatcher = NewDispatcher(cmds, cc, b.agentID)

	// Simulate an active turn
	_, cancel := context.WithCancel(context.Background())
	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()

	msg := makeMsg(111, "owner", "/stop")
	b.receiveMessage(context.Background(), msg)

	// Should NOT be queued
	if len(b.queue) != 0 {
		t.Error("/stop should not be queued")
	}

	// Should have sent "Stopped." reply
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message for /stop, got %d", mock.sentCount())
	}
}

func TestReceiveMessage_DotStopCancelsTurn(t *testing.T) {
	// Verifies that .stop works identically to /stop via the registry.
	cmds := command.NewRegistry()
	cmds.Register(command.StopCommand())

	b, mock := testBot([]string{"111"}, cmds)

	cc := command.CommandContext{
		StopFunc: b.cancelTurn,
	}
	b.dispatcher = NewDispatcher(cmds, cc, b.agentID)

	_, cancel := context.WithCancel(context.Background())
	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()

	msg := makeMsg(111, "owner", ".stop")
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 0 {
		t.Error(".stop should not be queued")
	}
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message for .stop, got %d", mock.sentCount())
	}
}

func TestReceiveMessage_StopAlias(t *testing.T) {
	// Verifies that stop aliases like /wait work via the command registry.
	cmds := command.NewRegistry()
	stopCmd := command.StopCommand()
	cmds.Register(stopCmd)
	// Register "wait" as an alias for "stop"
	cmds.Register(&command.Command{Name: "wait", Hidden: true, Execute: stopCmd.Execute})

	b, mock := testBot([]string{"111"}, cmds)

	cc := command.CommandContext{
		StopFunc: b.cancelTurn,
	}
	b.dispatcher = NewDispatcher(cmds, cc, b.agentID)

	// Simulate an active turn
	_, cancel := context.WithCancel(context.Background())
	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()

	// Test /wait alias
	msg := makeMsg(111, "owner", "/wait")
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 0 {
		t.Error("/wait should not be queued")
	}
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message for /wait, got %d", mock.sentCount())
	}
}

func TestReceiveMessage_StopAliasNotConfigured(t *testing.T) {
	// Verifies that /wait without an alias registration
	// is treated as an unknown command.
	cmds := command.NewRegistry()
	cmds.Register(command.StopCommand())
	// No "wait" alias registered

	b, mock := testBot([]string{"111"}, cmds)

	cc := command.CommandContext{
		StopFunc: b.cancelTurn,
	}
	b.dispatcher = NewDispatcher(cmds, cc, b.agentID)

	// Simulate an active turn
	_, cancel := context.WithCancel(context.Background())
	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()

	// /wait should NOT trigger stop — it's not registered
	msg := makeMsg(111, "owner", "/wait")
	b.receiveMessage(context.Background(), msg)

	// Should get a suggestion reply (unknown command), not queued or treated as stop
	if len(b.queue) != 0 {
		t.Fatalf("expected 0 queued messages for unknown /wait, got %d", len(b.queue))
	}
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 suggestion reply for unknown /wait, got %d", mock.sentCount())
	}
}
