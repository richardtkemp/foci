package discord

import (
	"context"
	"testing"
	"time"

	"foci/internal/command"

	"github.com/bwmarrin/discordgo"
)

// TestFormatUserInfo verifies user display strings with and without a username.
func TestFormatUserInfo(t *testing.T) {
	if got := formatUserInfo(&discordgo.User{ID: "1", Username: "rich"}); got != "1 (rich)" {
		t.Errorf("got %q", got)
	}
	if got := formatUserInfo(&discordgo.User{ID: "1"}); got != "1" {
		t.Errorf("got %q", got)
	}
}

// TestBuildReceivedMessageUnauthorized verifies messages from users outside the
// allowed list are dropped while allowed users pass.
func TestBuildReceivedMessageUnauthorized(t *testing.T) {
	b, _, _ := newTestBot(t, "a")
	b.allowedUsers = map[string]bool{"111": true}

	if _, ok := b.buildReceivedMessage(context.Background(), testDiscordMessage("1", "999", "hi")); ok {
		t.Error("expected unauthorized drop")
	}
	if _, ok := b.buildReceivedMessage(context.Background(), testDiscordMessage("1", "111", "hi")); !ok {
		t.Error("expected allowed user to pass")
	}
}

// TestBuildReceivedMessageMentionStripping verifies both bot mention forms are
// stripped from the text.
func TestBuildReceivedMessageMentionStripping(t *testing.T) {
	b, _, _ := newTestBot(t, "a")
	b.botUserID = "bot1"

	qm, ok := b.buildReceivedMessage(context.Background(), testDiscordMessage("1", "u", "<@bot1> hello <@!bot1>"))
	if !ok {
		t.Fatal("expected message accepted")
	}
	if qm.text != "hello" {
		t.Errorf("expected mentions stripped, got %q", qm.text)
	}
}

// TestBuildReceivedMessageReplyContext verifies replied-to message content is
// prepended as context.
func TestBuildReceivedMessageReplyContext(t *testing.T) {
	b, _, _ := newTestBot(t, "a")
	msg := testDiscordMessage("1", "u", "my answer")
	msg.ReferencedMessage = &discordgo.Message{Content: "original question"}

	qm, ok := b.buildReceivedMessage(context.Background(), msg)
	if !ok {
		t.Fatal("expected message accepted")
	}
	want := "[Replying to: original question]\n\nmy answer"
	if qm.text != want {
		t.Errorf("got %q, want %q", qm.text, want)
	}
}

// TestBuildReceivedMessageEmptyDrop verifies messages with no text and no
// attachments are dropped.
func TestBuildReceivedMessageEmptyDrop(t *testing.T) {
	b, _, _ := newTestBot(t, "a")
	if _, ok := b.buildReceivedMessage(context.Background(), testDiscordMessage("1", "u", "")); ok {
		t.Error("expected empty message dropped")
	}
}

// TestBuildReceivedMessageSetsDefaultChat verifies the first inbound message
// sets the agent's default channel, records the username, and updates the
// last-known channel; a later channel does not steal the default.
func TestBuildReceivedMessageSetsDefaultChat(t *testing.T) {
	b, _, idx := newTestBot(t, "a")

	if _, ok := b.buildReceivedMessage(context.Background(), testDiscordMessage("42", "u1", "first")); !ok {
		t.Fatal("expected message accepted")
	}
	if got := idx.DefaultChatForAgent("a", platformName); got != 42 {
		t.Errorf("expected default chat 42, got %d", got)
	}
	if b.ChatID() != 42 {
		t.Errorf("expected last-known channel 42, got %d", b.ChatID())
	}

	if _, ok := b.buildReceivedMessage(context.Background(), testDiscordMessage("77", "u1", "second")); !ok {
		t.Fatal("expected message accepted")
	}
	if got := idx.DefaultChatForAgent("a", platformName); got != 42 {
		t.Errorf("default chat should remain 42, got %d", got)
	}
	if b.ChatID() != 77 {
		t.Errorf("expected last-known channel updated to 77, got %d", b.ChatID())
	}
}

// TestReceiveMessageEnqueuesText verifies a plain message flows through
// receiveMessage onto the main queue with sender metadata intact.
func TestReceiveMessageEnqueuesText(t *testing.T) {
	b, _, _ := newTestBot(t, "a")
	b.receiveMessage(context.Background(), testDiscordMessage("42", "u1", "hello agent"))

	select {
	case got := <-b.mq.Chan():
		if got.Text != "hello agent" || got.ChatID != 42 || got.UserID != "u1" {
			t.Errorf("unexpected queued message %+v", got)
		}
		if got.SenderName != "user-u1" {
			t.Errorf("expected sender name, got %q", got.SenderName)
		}
	default:
		t.Fatal("expected message enqueued")
	}
}

// TestReceiveMessageRoutesCommandToCmdChan verifies non-immediate slash
// commands are routed to the command channel rather than the main queue.
func TestReceiveMessageRoutesCommandToCmdChan(t *testing.T) {
	b, _, _ := newTestBot(t, "a")
	msg := testDiscordMessage("42", "u1", "/status")
	msg.Timestamp = time.Now()
	b.receiveMessage(context.Background(), msg)

	select {
	case got := <-b.mq.CmdChan():
		if got.Text != "/status" {
			t.Errorf("unexpected command %q", got.Text)
		}
	default:
		t.Fatal("expected command on command channel")
	}
	select {
	case <-b.mq.Chan():
		t.Fatal("command should not reach the main queue")
	default:
	}
}

// TestReceiveMessageDropsStaleCommand verifies slash commands older than the
// stale window are discarded instead of queued.
func TestReceiveMessageDropsStaleCommand(t *testing.T) {
	b, _, _ := newTestBot(t, "a")
	msg := testDiscordMessage("42", "u1", "/status")
	msg.Timestamp = time.Now().Add(-time.Minute)
	b.receiveMessage(context.Background(), msg)

	select {
	case <-b.mq.CmdChan():
		t.Fatal("stale command should be dropped")
	default:
	}
}

// TestReceiveMessageImmediateCommandIntercepted verifies an Immediate command
// is dispatched inline (reply sent) instead of being queued.
func TestReceiveMessageImmediateCommandIntercepted(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.commands.Register(&command.Command{
		Name:      "ping",
		Immediate: true,
		Execute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
			return command.Response{Text: "pong"}, nil
		},
	})
	b.SetCommandContext(command.CommandContext{})

	msg := testDiscordMessage("42", "u1", "/ping")
	msg.Timestamp = time.Now()
	b.receiveMessage(context.Background(), msg)

	if got := fs.lastSend(t); got.content != "pong" {
		t.Errorf("expected inline pong reply, got %q", got.content)
	}
	select {
	case <-b.mq.CmdChan():
		t.Fatal("immediate command should not be queued")
	case <-b.mq.Chan():
		t.Fatal("immediate command should not reach main queue")
	default:
	}
}

// TestReceiveMessageIdleSecondaryDrops verifies idle secondary bots silently
// consume normal messages.
func TestReceiveMessageIdleSecondaryDrops(t *testing.T) {
	b, _, _ := newTestBot(t, "")
	b.isSecondary = true
	b.receiveMessage(context.Background(), testDiscordMessage("42", "u1", "anyone there?"))

	select {
	case <-b.mq.Chan():
		t.Fatal("idle secondary should drop messages")
	default:
	}
}

// TestToPlatformMessage verifies the discord->platform message conversion maps
// attachments, group/mention flags, and IDs.
func TestToPlatformMessage(t *testing.T) {
	b, _, _ := newTestBot(t, "a")
	b.botUserID = "bot1"

	msg := testDiscordMessage("42", "u1", "raw")
	msg.GuildID = "g1"
	msg.Mentions = []*discordgo.User{{ID: "bot1"}}
	qm := queuedMessage{
		msg:    msg,
		userID: "u1",
		text:   "clean",
		attachments: []attachment{
			{data: []byte("img"), mediaType: "image/png", savedPath: "/tmp/x.png"},
		},
	}

	pm := b.toPlatformMessage(msg, qm)
	if pm.ChatID != 42 || pm.UserID != "u1" || pm.Text != "clean" {
		t.Errorf("unexpected mapping %+v", pm)
	}
	if !pm.IsGroupChat || !pm.IsMention {
		t.Error("expected group chat with mention")
	}
	if len(pm.Attachments) != 1 || pm.Attachments[0].MimeType != "image/png" || pm.Attachments[0].SavedPath != "/tmp/x.png" {
		t.Errorf("unexpected attachments %+v", pm.Attachments)
	}
	if pm.Original != msg {
		t.Error("expected original message carried through")
	}
}

// TestChatIDFromMsg verifies channel ID parsing including the malformed case.
func TestChatIDFromMsg(t *testing.T) {
	if got := chatIDFromMsg(&discordgo.Message{ChannelID: "123"}); got != 123 {
		t.Errorf("got %d", got)
	}
	if got := chatIDFromMsg(&discordgo.Message{ChannelID: "abc"}); got != 0 {
		t.Errorf("expected 0 for malformed ID, got %d", got)
	}
}

// TestTruncate verifies the package-local truncate alias shortens long strings.
func TestTruncate(t *testing.T) {
	if got := truncate("hello", 100); got != "hello" {
		t.Errorf("short string should be unchanged, got %q", got)
	}
	long := truncate("aaaaaaaaaaaaaaaaaaaa", 10)
	if len(long) > 13 { // 10 chars + ellipsis allowance
		t.Errorf("expected truncation, got %q (%d chars)", long, len(long))
	}
}
