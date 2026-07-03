package telegram

import (
	"context"
	"foci/internal/platform"
	"testing"

	"foci/internal/command"
	"foci/internal/dispatch"
)

func TestReceiveMessage_DoneOnPrimaryBot(t *testing.T) {
	// Verifies that /done on the primary bot goes to the command channel and,
	// when the worker dispatches it, returns a "nothing to detach" message.
	cmds := command.NewRegistry()
	cmds.Register(command.DoneCommand())
	b, mock := testBot([]string{"111"}, cmds)

	cc := command.CommandContext{
		StopFunc:       b.cancelTurn,
		IsSecondaryBot: false,
	}
	b.dispatcher = dispatch.NewDispatcher(cmds, cc, b.agentID)

	msg := makeMsg(111, "owner", "/done")
	b.receiveMessage(context.Background(), msg)

	// Should be in command channel, not the agent queue.
	if len(b.mq.Chan()) != 0 {
		t.Error("/done should not be in agent queue")
	}

	// Simulate worker dispatch.
	cmd := <-b.mq.CmdChan()
	b.processQueuedCommand(context.Background(), cmd)

	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message, got %d", mock.sentCount())
	}
}

func TestReceiveMessage_DoneOnSecondaryBot(t *testing.T) {
	// Verifies that /done on a secondary bot goes to the command channel and,
	// when the worker dispatches it, detaches the session.
	cmds := command.NewRegistry()
	cmds.Register(command.DoneCommand())
	b, mock := testBot([]string{"111"}, cmds)
	pool := platform.NewPool[*Bot]("telegram")
	b.isSecondary = true
	b.pool = pool
	pool.Add(b)

	// Simulate active session
	b.SetSessionKey("main/c1/b1")

	// Wire CC with secondary bot state — mirrors SetCommandContext logic
	cc := command.CommandContext{
		StopFunc:       b.cancelTurn,
		IsSecondaryBot: true,
		ReleaseFunc: func() {
			pool.Release(b)
		},
	}
	b.dispatcher = dispatch.NewDispatcher(cmds, cc, b.agentID)

	msg := makeMsg(111, "owner", "/done")
	b.receiveMessage(context.Background(), msg)

	// Should be in command channel, not the agent queue.
	if len(b.mq.Chan()) != 0 {
		t.Error("/done should not be in agent queue")
	}

	// Simulate worker dispatch.
	cmd := <-b.mq.CmdChan()
	b.processQueuedCommand(context.Background(), cmd)

	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message, got %d", mock.sentCount())
	}
	if b.SessionKey() != "" {
		t.Error("session key should be cleared after /done")
	}
}

func TestReceiveMessage_IdleSecondaryBot(t *testing.T) {
	// Verifies that idle secondary bots
	// (with no assigned session) silently drop messages.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.isSecondary = true
	b.SetSessionKey("") // idle — no session assigned

	msg := makeMsg(111, "owner", "hello")
	b.receiveMessage(context.Background(), msg)

	// Should silently drop — no reply, no queue
	if mock.sentCount() != 0 {
		t.Fatalf("expected 0 sent messages (silent drop), got %d", mock.sentCount())
	}
	if len(b.mq.Chan()) != 0 {
		t.Error("idle secondary bot should not queue messages")
	}
}

func TestReceiveMessage_SecondaryBotWithSession(t *testing.T) {
	// Verifies that secondary bots
	// with an active session queue messages normally.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.isSecondary = true
	b.SetSessionKey("main/c1/b1")

	msg := makeMsg(111, "owner", "hello")
	b.receiveMessage(context.Background(), msg)

	// Should queue normally when session is assigned
	if len(b.mq.Chan()) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.mq.Chan()))
	}
}
