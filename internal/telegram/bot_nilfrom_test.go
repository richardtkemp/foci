package telegram

import (
	"context"
	"testing"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// TestBuildReceivedMessageNilFrom proves a message with no sender (anonymous /
// post-as-channel) is dropped without a nil-deref on msg.From — a pre-auth DoS
// on the poll path. (P3.)
func TestBuildReceivedMessageNilFrom(t *testing.T) {
	b := &Bot{}
	msg := &gotgbot.Message{From: nil, Chat: gotgbot.Chat{Id: 42}}
	_, ok := b.buildReceivedMessage(context.Background(), msg)
	if ok {
		t.Fatal("message with nil From should be dropped (ok=false)")
	}
}
