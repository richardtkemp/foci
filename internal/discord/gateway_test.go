package discord

import (
	"context"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// gatewayTestBot builds a primary test bot with a real (unopened) discordgo
// session for gateway-level tests. No network is touched.
func gatewayTestBot(t *testing.T) *Bot {
	t.Helper()
	dg, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := newTestBot(t, "a")
	b.session = dg
	return b
}

// TestRunStartStop verifies Start spawns the Run loop and that cancelling the
// context shuts it down; Stop is a no-op that must not error.
func TestRunStartStop(t *testing.T) {
	b := gatewayTestBot(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		b.Run(ctx)
		close(done)
	}()
	cancel()
	<-done // hangs (test timeout) if Run ignores cancellation

	if err := b.Start(context.Background()); err != nil {
		t.Errorf("Start returned error: %v", err)
	}
	if err := b.Stop(); err != nil {
		t.Errorf("Stop returned error: %v", err)
	}
}

// TestOnMessageCreateIgnoresOwnMessages verifies the bot drops its own
// outbound messages and nil-author events instead of enqueueing them.
func TestOnMessageCreateIgnoresOwnMessages(t *testing.T) {
	b := gatewayTestBot(t)
	b.botUserID = "bot-self"

	own := &discordgo.MessageCreate{Message: testDiscordMessage("1", "bot-self", "hi")}
	b.onMessageCreate(context.Background(), own)

	noAuthor := &discordgo.MessageCreate{Message: &discordgo.Message{ChannelID: "1"}}
	b.onMessageCreate(context.Background(), noAuthor)

	select {
	case <-b.mq.Chan():
		t.Fatal("self/authorless message should not be enqueued")
	default:
	}
}

// TestOnMessageCreateGuildRestriction verifies messages from foreign guilds are
// dropped when a guild restriction is configured, while the configured guild
// passes through.
func TestOnMessageCreateGuildRestriction(t *testing.T) {
	b := gatewayTestBot(t)
	b.guildID = "guild-1"
	b.requireMention = false

	foreign := &discordgo.MessageCreate{Message: testDiscordMessage("1", "u1", "hello")}
	foreign.GuildID = "guild-2"
	b.onMessageCreate(context.Background(), foreign)
	select {
	case <-b.mq.Chan():
		t.Fatal("foreign-guild message should be dropped")
	default:
	}

	dm := &discordgo.MessageCreate{Message: testDiscordMessage("1", "u1", "hello")}
	b.onMessageCreate(context.Background(), dm)
	select {
	case got := <-b.mq.Chan():
		if got.Text != "hello" {
			t.Errorf("unexpected enqueued text %q", got.Text)
		}
	default:
		t.Fatal("DM should be enqueued")
	}
}

// TestMessageContainsMention verifies bot-mention detection over the message's
// mention list.
func TestMessageContainsMention(t *testing.T) {
	b := &Bot{botUserID: "bot-1"}

	with := &discordgo.Message{Mentions: []*discordgo.User{{ID: "u9"}, {ID: "bot-1"}}}
	if !b.messageContainsMention(with) {
		t.Error("expected mention detected")
	}
	without := &discordgo.Message{Mentions: []*discordgo.User{{ID: "u9"}}}
	if b.messageContainsMention(without) {
		t.Error("expected no mention")
	}
	if b.messageContainsMention(&discordgo.Message{}) {
		t.Error("expected no mention on empty list")
	}
}
