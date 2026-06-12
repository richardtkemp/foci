package telegram

import (
	"context"
	"testing"

	"foci/internal/command"
	"foci/internal/platform"
)

func TestReceiveMessage_QueueFull(t *testing.T) {
	// Verifies that when the message queue is full,
	// new messages are dropped (the mq logs a warning internally).
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	// Replace the default mq with a tiny one (size=2)
	b.mq = platform.NewMessageQueue(platform.MessageQueueConfig{
		Size: 2,
	})
	b.mq.PushFlushed(platform.QueuedMessage{UserID: "111", Text: "msg1", ChatID: 12345})
	b.mq.PushFlushed(platform.QueuedMessage{UserID: "111", Text: "msg2", ChatID: 12345})

	// Next message should be dropped (queue full, Enqueue drops silently)
	msg := makeMsg(111, "owner", "msg3 overflow")
	b.receiveMessage(context.Background(), msg)

	// Queue should still have exactly 2
	if len(b.mq.Chan()) != 2 {
		t.Errorf("queue should still have 2 messages, got %d", len(b.mq.Chan()))
	}
}

func TestReceiveMessage_MultipleUsersAllowed(t *testing.T) {
	// Verifies that multiple authorized
	// users can send messages and they all get queued.
	b, _ := testBot([]string{"111", "222"}, command.NewRegistry())

	b.receiveMessage(context.Background(), makeMsg(111, "user1", "hello"))
	b.receiveMessage(context.Background(), makeMsg(222, "user2", "world"))
	b.receiveMessage(context.Background(), makeMsg(333, "user3", "rejected"))

	if len(b.mq.Chan()) != 2 {
		t.Errorf("expected 2 queued messages, got %d", len(b.mq.Chan()))
	}
}
