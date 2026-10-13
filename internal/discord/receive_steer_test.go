package discord

import (
	"context"
	"testing"
	"time"

	"foci/internal/platform"

	"github.com/bwmarrin/discordgo"
)

// TestReceive_SteerPrefix_SetsPreferenceAndStripsText proves the discord
// receive path's half of #1318: a /steer or /queue first word
// (case-insensitive) is stripped from what the agent sees and the choice
// rides the queued message; the words never reach the command channel. A
// bot mention before the word parses too — discord strips the mention from
// the text before the prefix is parsed.
func TestReceive_SteerPrefix_SetsPreferenceAndStripsText(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		wantText  string
		wantSteer platform.SteerPreference
	}{
		{"steer", "/steer stop that", "stop that", platform.SteerAlways},
		{"queue", "/queue also do X", "also do X", platform.SteerNever},
		{"case-insensitive", "/QUEUE upper", "upper", platform.SteerNever},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, _, _ := newTestBot(t, "a")
			b.receiveMessage(context.Background(), testDiscordMessage("42", "u1", tc.text))

			select {
			case qm := <-b.mq.Chan():
				if qm.Text != tc.wantText {
					t.Errorf("queued text = %q, want %q", qm.Text, tc.wantText)
				}
				if qm.Steer != tc.wantSteer {
					t.Errorf("queued steer = %v, want %v", qm.Steer, tc.wantSteer)
				}
			default:
				t.Fatal("expected the stripped message on the main queue")
			}
			select {
			case <-b.mq.CmdChan():
				t.Fatal("prefix message reached the command channel")
			default:
			}
		})
	}

	t.Run("mention before the prefix parses", func(t *testing.T) {
		b, _, _ := newTestBot(t, "a")
		b.botUserID = "bot1"
		b.receiveMessage(context.Background(), testDiscordMessage("42", "u1", "<@bot1> /queue do it next"))

		select {
		case qm := <-b.mq.Chan():
			if qm.Text != "do it next" {
				t.Errorf("queued text = %q, want %q", qm.Text, "do it next")
			}
			if qm.Steer != platform.SteerNever {
				t.Errorf("queued steer = %v, want SteerNever", qm.Steer)
			}
		default:
			t.Fatal("expected the mention+prefix message on the main queue")
		}
	})

	// Control arm: other commands keep today's routing — /status is not one
	// of the two prefix words and goes to the command channel untouched.
	t.Run("other commands keep today's routing", func(t *testing.T) {
		b, _, _ := newTestBot(t, "a")
		msg := testDiscordMessage("42", "u1", "/status now")
		msg.Timestamp = time.Now()
		b.receiveMessage(context.Background(), msg)

		select {
		case cmd := <-b.mq.CmdChan():
			if cmd.Text != "/status now" {
				t.Errorf("command text = %q, want it untouched", cmd.Text)
			}
		default:
			t.Fatal("expected /status on the command channel as today")
		}
		select {
		case <-b.mq.Chan():
			t.Fatal("/status reached the main queue")
		default:
		}
	})
}

// TestReceive_BareQueuePrefix_RepliesUsageAndDeliversNothing: a bare /queue
// (no message text, spaces only count as none) answers with the one-line
// usage and enqueues nothing — neither on the main queue nor the command
// channel (#1318).
func TestReceive_BareQueuePrefix_RepliesUsageAndDeliversNothing(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"bare queue", "/queue"},
		{"bare queue trailing spaces", "/queue   "},
		{"bare steer", "/steer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, fs, _ := newTestBot(t, "a")
			b.receiveMessage(context.Background(), testDiscordMessage("42", "u1", tc.text))

			sends := fs.sendCount()
			if sends != 1 {
				t.Fatalf("sends = %d, want exactly the one usage reply", sends)
			}
			if got := fs.lastSend(t).content; got != platform.SteerPrefixUsage {
				t.Errorf("reply = %q, want the pinned usage line %q", got, platform.SteerPrefixUsage)
			}
			select {
			case <-b.mq.Chan():
				t.Fatal("bare prefix enqueued a message")
			default:
			}
			select {
			case <-b.mq.CmdChan():
				t.Fatal("bare prefix reached the command channel")
			default:
			}
		})
	}
}

// TestReceive_SteerPrefix_GroupUnaddressed_DroppedSilently proves addressing
// applies BEFORE prefix parsing (#1318): in a require-mention channel, a
// /steer message that does not mention the bot is dropped exactly like plain
// text — it reaches neither the agent, nor the command channel, nor a reply.
func TestReceive_SteerPrefix_GroupUnaddressed_DroppedSilently(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"prefixed message dropped", "/steer redirect this"},
		{"plain-text control dropped the same way", "just talking"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, fs, _ := newTestBot(t, "a")
			b.requireMention = true
			b.botUserID = "bot1"
			b.mq = platform.NewMessageQueue(platform.MessageQueueConfig{Size: 8, RequireMention: true})
			b.receiveMessage(context.Background(), guildMessage("42", "u1", tc.text))

			select {
			case <-b.mq.Chan():
				t.Fatal("unaddressed channel message reached the main queue")
			default:
			}
			select {
			case <-b.mq.CmdChan():
				t.Fatal("unaddressed channel message reached the command channel")
			default:
			}
			if fs.sendCount() != 0 {
				t.Errorf("sends = %d, want silence for an unaddressed message", fs.sendCount())
			}
		})
	}
}

// TestReceive_SteerPrefix_MentionDelivered: the same channel message WITH a
// mention of this bot is delivered with the prefix stripped (discord strips
// mentions from text before parsing) and the choice set (#1318).
func TestReceive_SteerPrefix_MentionDelivered(t *testing.T) {
	b, _, _ := newTestBot(t, "a")
	b.requireMention = true
	b.botUserID = "bot1"
	b.mq = platform.NewMessageQueue(platform.MessageQueueConfig{Size: 8, RequireMention: true})

	msg := guildMessage("42", "u1", "<@bot1> /queue do it next")
	msg.Mentions = []*discordgo.User{{ID: "bot1"}}
	b.receiveMessage(context.Background(), msg)

	select {
	case qm := <-b.mq.Chan():
		if qm.Text != "do it next" {
			t.Errorf("queued text = %q, want %q", qm.Text, "do it next")
		}
		if qm.Steer != platform.SteerNever {
			t.Errorf("queued steer = %v, want SteerNever", qm.Steer)
		}
	default:
		t.Fatal("expected the mentioned channel message on the main queue")
	}
}

// TestReceive_BarePrefix_GroupUnaddressed_Silent: an un-mentioned bare
// /queue in a require-mention channel is ignored with no usage reply
// (#1318) — the usage line belongs only to messages addressed to the bot.
func TestReceive_BarePrefix_GroupUnaddressed_Silent(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.requireMention = true
	b.botUserID = "bot1"
	b.mq = platform.NewMessageQueue(platform.MessageQueueConfig{Size: 8, RequireMention: true})
	b.receiveMessage(context.Background(), guildMessage("42", "u1", "/queue"))

	if fs.sendCount() != 0 {
		t.Errorf("sends = %d, want 0 (unaddressed bare prefix is silent)", fs.sendCount())
	}
	select {
	case <-b.mq.Chan():
		t.Fatal("unaddressed bare prefix enqueued a message")
	default:
	}
}

// guildMessage builds a guild (channel) message for the addressing tests.
func guildMessage(channelID, authorID, content string) *discordgo.Message {
	msg := testDiscordMessage(channelID, authorID, content)
	msg.GuildID = "g1"
	return msg
}
