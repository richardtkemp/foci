package discord

import (
	"context"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// TestBuildReceivedMessageNilAuthor proves a message with no author (webhook /
// system message) is dropped without a nil-deref on msg.Author — a pre-auth DoS
// on the receive path. (P3.)
func TestBuildReceivedMessageNilAuthor(t *testing.T) {
	b := &Bot{}
	msg := &discordgo.Message{Author: nil, ChannelID: "123"}
	_, ok := b.buildReceivedMessage(context.Background(), msg)
	if ok {
		t.Fatal("message with nil Author should be dropped (ok=false)")
	}
}
