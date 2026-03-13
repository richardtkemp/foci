package telegram

import (
	"context"
	"testing"

	"foci/internal/command"
)

func TestReceiveMessage_QueueFull(t *testing.T) {
	// TestReceiveMessage_QueueFull verifies that when the message queue is full,
	// new messages are dropped and a reply is sent to the user.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Fill the queue
	b.queue = make(chan queuedMessage, 2)
	b.queue <- queuedMessage{msg: makeMsg(111, "owner", "msg1"), text: "msg1"}
	b.queue <- queuedMessage{msg: makeMsg(111, "owner", "msg2"), text: "msg2"}

	// Next message should be dropped
	msg := makeMsg(111, "owner", "msg3 overflow")
	b.receiveMessage(context.Background(), msg)

	// Should have sent a "queue full" reply
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message for queue full, got %d", mock.sentCount())
	}

	// Queue should still have exactly 2
	if len(b.queue) != 2 {
		t.Errorf("queue should still have 2 messages, got %d", len(b.queue))
	}
}

func TestReceiveMessage_MultipleUsersAllowed(t *testing.T) {
	// TestReceiveMessage_MultipleUsersAllowed verifies that multiple authorized
	// users can send messages and they all get queued.
	b, _ := testBot([]string{"111", "222"}, command.NewRegistry())

	b.receiveMessage(context.Background(), makeMsg(111, "user1", "hello"))
	b.receiveMessage(context.Background(), makeMsg(222, "user2", "world"))
	b.receiveMessage(context.Background(), makeMsg(333, "user3", "rejected"))

	if len(b.queue) != 2 {
		t.Errorf("expected 2 queued messages, got %d", len(b.queue))
	}
}
