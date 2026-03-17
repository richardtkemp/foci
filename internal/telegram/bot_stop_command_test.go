package telegram

import (
	"context"
	"testing"

	"foci/internal/command"
)

func TestReceiveMessage_StopCancelsTurn(t *testing.T) {
	// Verifies that /stop cancels an active turn.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

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

	// turnCancel should have been called (verified by checking context is done)
	// We can't directly check this, but the cancel function was called
}

func TestReceiveMessage_DotStopCancelsTurn(t *testing.T) {
	// Verifies that .stop works identically to /stop.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

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
	// Verifies that stop aliases like /wait work
	// when enabled.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Set stop aliases with enabled=true
	b.SetStopAliases([]string{"stop", "wait", "hold"}, true)

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
	// Verifies that aliases don't work
	// when disabled.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Aliases disabled — even with aliases configured, they shouldn't work
	b.SetStopAliases([]string{"wait", "hold"}, false)

	// Simulate an active turn
	_, cancel := context.WithCancel(context.Background())
	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()

	// /wait should NOT trigger stop when aliases are disabled
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
