package telegram

import (
	"context"
	"testing"

	"foci/internal/command"
)

func TestReceiveMessage_RejectsUnauthorizedUser(t *testing.T) {
	// TestReceiveMessage_RejectsUnauthorizedUser verifies that unauthorized users
	// cannot send messages to the bot.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	msg := makeMsg(999, "hacker", "hello")
	b.receiveMessage(context.Background(), msg)

	// Should not send any reply or queue anything
	if mock.sentCount() != 0 {
		t.Error("should not send reply to unauthorized user")
	}
	if len(b.queue) != 0 {
		t.Error("should not queue message from unauthorized user")
	}
}

func TestReceiveMessage_AcceptsAuthorizedUser(t *testing.T) {
	// TestReceiveMessage_AcceptsAuthorizedUser verifies that authorized users can
	// send messages to the bot and they are properly queued.
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	msg := makeMsg(111, "owner", "hello world")
	b.receiveMessage(context.Background(), msg)

	// Should be queued for the agent
	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.queue))
	}
	qm := <-b.queue
	if qm.text != "hello world" {
		t.Errorf("queued text = %q, want %q", qm.text, "hello world")
	}
	if qm.userID != "111" {
		t.Errorf("queued userID = %q, want %q", qm.userID, "111")
	}
}

func TestReceiveMessage_IgnoresEmptyText(t *testing.T) {
	// TestReceiveMessage_IgnoresEmptyText verifies that empty or whitespace-only
	// messages are not queued.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	msg := makeMsg(111, "owner", "")
	b.receiveMessage(context.Background(), msg)

	if mock.sentCount() != 0 {
		t.Error("should not send reply to empty message")
	}
	if len(b.queue) != 0 {
		t.Error("should not queue empty message")
	}
}
