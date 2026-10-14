package telegram

import (
	"context"
	"strings"
	"testing"

	"foci/internal/command"
	"foci/internal/platform"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// TestReceive_SteerPrefix_SetsPreferenceAndStripsText proves the receive
// path's half of #1318: a /steer or /queue first word (case-insensitive,
// caption or text) is stripped from what the agent sees and the choice rides
// the queued message; the words never reach the command channel. A reply
// whose own text starts with a prefix parses too — the [Replying to…] header
// is prepended to the already-stripped text, not the raw one.
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
			b, _ := testBot([]string{"111"}, command.NewRegistry())
			b.receiveMessage(context.Background(), makeMsg(111, "owner", tc.text))

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

	// Control arm: other commands keep today's routing. /status is not one of
	// the two prefix words, so it goes to the command channel untouched.
	t.Run("other commands keep today's routing", func(t *testing.T) {
		b, _ := testBot([]string{"111"}, command.NewRegistry())
		b.receiveMessage(context.Background(), makeMsg(111, "owner", "/status now"))

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

// TestReceive_QueuePrefix_PhotoCaption: a photo whose caption starts with a
// prefix works the same on the caption (#1318) — the payload is delivered as
// the caption text with the queue choice.
func TestReceive_QueuePrefix_PhotoCaption(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	mock.files = map[string]string{"large_id": "photos/test.jpg"}
	b.botToken = "test-token"

	b.receiveMessage(context.Background(), makeMsgWithPhoto(111, "owner", "/queue look at this"))

	select {
	case qm := <-b.mq.Chan():
		if qm.Text != "look at this" {
			t.Errorf("queued text = %q, want %q", qm.Text, "look at this")
		}
		if qm.Steer != platform.SteerNever {
			t.Errorf("queued steer = %v, want SteerNever", qm.Steer)
		}
	default:
		t.Fatal("expected the caption payload on the main queue")
	}
}

// TestReceive_SteerPrefix_ReplyTextParses: a reply whose own text starts
// with a prefix parses — the quote header is prepended to the stripped
// payload, so the agent sees the header followed by the payload.
func TestReceive_SteerPrefix_ReplyTextParses(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	msg := makeMsg(111, "owner", "/queue my answer")
	msg.ReplyToMessage = &gotgbot.Message{Text: "original question", Date: msg.Date}

	b.receiveMessage(context.Background(), msg)

	select {
	case qm := <-b.mq.Chan():
		if !strings.HasPrefix(qm.Text, "[Replying to (") || !strings.Contains(qm.Text, "original question") {
			t.Errorf("queued text = %q, want it to open with the reply header", qm.Text)
		}
		if !strings.HasSuffix(qm.Text, "\n\nmy answer") {
			t.Errorf("queued text = %q, want it to end with the stripped payload %q", qm.Text, "my answer")
		}
		if qm.Steer != platform.SteerNever {
			t.Errorf("queued steer = %v, want SteerNever", qm.Steer)
		}
	default:
		t.Fatal("expected the reply's stripped payload on the main queue")
	}
}

// sentUsageText reverses the HTML escaping sendReply applies (SendTextToChat
// renders HTML), so a test can compare what the user actually reads against
// the pinned constant.
func sentUsageText(mock *mockClient) string {
	s := strings.ReplaceAll(mock.lastSendInjected, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	return strings.ReplaceAll(s, "&amp;", "&")
}

// TestReceive_BareQueuePrefix_RepliesUsageAndDeliversNothing: a bare /queue
// (no message text, spaces only count as none) answers with the one-line
// usage and enqueues nothing — neither on the main queue nor the command
// channel, and nothing reaches the agent (#1318).
func TestReceive_BareQueuePrefix_RepliesUsageAndDeliversNothing(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"bare queue", "/queue"},
		{"bare queue trailing spaces", "/queue   "},
		{"bare steer", "/steer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, mock := testBot([]string{"111"}, command.NewRegistry())
			b.receiveMessage(context.Background(), makeMsg(111, "owner", tc.text))

			if mock.sends != 1 {
				t.Fatalf("sends = %d, want exactly the one usage reply", mock.sends)
			}
			if got := sentUsageText(mock); got != platform.SteerPrefixUsage {
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

// makeGroupMsg builds a group-chat message for the addressing tests.
func makeGroupMsg(userID int64, username, text string) *gotgbot.Message {
	msg := makeMsg(userID, username, text)
	msg.Chat.Type = "supergroup"
	return msg
}

// requireMentionBot rebuilds the bot's queue with require_mention on.
func requireMentionBot(t *testing.T, b *Bot) {
	t.Helper()
	b.mq = platform.NewMessageQueue(platform.MessageQueueConfig{Size: 8, RequireMention: true})
}

// TestReceive_SteerPrefix_GroupUnaddressed_DroppedSilently proves addressing
// applies BEFORE prefix parsing (#1318): in a require-mention group, a
// /steer message that does not mention the bot is dropped exactly like plain
// text — it reaches neither the agent, nor the command channel, nor a reply.
func TestReceive_SteerPrefix_GroupUnaddressed_DroppedSilently(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"prefixed message dropped", "/steer redirect this"},
		{"plain-text control dropped the same way", "just talking"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, mock := testBot([]string{"111"}, command.NewRegistry())
			requireMentionBot(t, b)
			b.receiveMessage(context.Background(), makeGroupMsg(111, "owner", tc.text))

			select {
			case <-b.mq.Chan():
				t.Fatal("unaddressed group message reached the main queue")
			default:
			}
			select {
			case <-b.mq.CmdChan():
				t.Fatal("unaddressed group message reached the command channel")
			default:
			}
			if mock.sends != 0 {
				t.Errorf("sends = %d, want silence for an unaddressed message", mock.sends)
			}
		})
	}
}

// TestReceive_SteerPrefix_GroupMentionDelivered: every way a group message
// addresses this bot still lets the prefix work — the leading @mention is an
// addressing token, not payload (stripped, like discord's mention strip), a
// command-menu tap arrives as "/queue@mybot" (a bot_command entity aimed at
// this bot, so it counts as addressed), and a TRAILING mention stays part of
// the payload (#1318).
func TestReceive_SteerPrefix_GroupMentionDelivered(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		entities []gotgbot.MessageEntity
		wantText string
	}{
		{
			name:     "leading mention",
			text:     "@mybot /queue do it next",
			entities: []gotgbot.MessageEntity{{Type: "mention", Offset: 0, Length: int64(len("@mybot"))}},
			wantText: "do it next",
		},
		{
			name:     "bot_command with this bot's suffix",
			text:     "/queue@mybot do it next",
			entities: []gotgbot.MessageEntity{{Type: "bot_command", Offset: 0, Length: int64(len("/queue@mybot"))}},
			wantText: "do it next",
		},
		{
			name: "trailing mention is payload",
			text: "/queue do it next @mybot",
			entities: []gotgbot.MessageEntity{{Type: "mention",
				Offset: int64(len("/queue do it next ")), Length: int64(len("@mybot"))}},
			wantText: "do it next @mybot",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := testBot([]string{"111"}, command.NewRegistry())
			requireMentionBot(t, b)
			b.api = &gotgbot.Bot{User: gotgbot.User{Id: 42, Username: "mybot"}}

			msg := makeGroupMsg(111, "owner", tc.text)
			msg.Entities = tc.entities
			b.receiveMessage(context.Background(), msg)

			select {
			case qm := <-b.mq.Chan():
				if qm.Text != tc.wantText {
					t.Errorf("queued text = %q, want %q", qm.Text, tc.wantText)
				}
				if qm.Steer != platform.SteerNever {
					t.Errorf("queued steer = %v, want SteerNever", qm.Steer)
				}
			default:
				t.Fatal("expected the addressed group message on the main queue")
			}
		})
	}
}

// TestReceive_SteerPrefix_GroupBareMenuTap_RepliesUsage: a bare command-menu
// tap in a require-mention group ("/queue@mybot" — a bot_command entity
// addressed to this bot) gets the one usage reply and delivers nothing
// (#1318); the menu entry the bot itself registered must not be a dead tap.
func TestReceive_SteerPrefix_GroupBareMenuTap_RepliesUsage(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	requireMentionBot(t, b)
	b.api = &gotgbot.Bot{User: gotgbot.User{Id: 42, Username: "mybot"}}

	msg := makeGroupMsg(111, "owner", "/queue@mybot")
	msg.Entities = []gotgbot.MessageEntity{{Type: "bot_command", Offset: 0, Length: int64(len("/queue@mybot"))}}
	b.receiveMessage(context.Background(), msg)

	if mock.sends != 1 || sentUsageText(mock) != platform.SteerPrefixUsage {
		t.Errorf("sends = %d last = %q, want the one usage reply", mock.sends, sentUsageText(mock))
	}
	select {
	case <-b.mq.Chan():
		t.Fatal("bare menu tap enqueued a message")
	default:
	}
	select {
	case <-b.mq.CmdChan():
		t.Fatal("bare menu tap reached the command channel")
	default:
	}
}

// TestReceive_QueuePrefix_PhotoCaptionMention: a photo whose caption leads
// with a mention of this bot parses the prefix on the caption in a
// require-mention group too — the caption's entity list is what addresses
// the message (#1318).
func TestReceive_QueuePrefix_PhotoCaptionMention(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	requireMentionBot(t, b)
	b.api = &gotgbot.Bot{User: gotgbot.User{Id: 42, Username: "mybot"}}
	mock.files = map[string]string{"large_id": "photos/test.jpg"}
	b.botToken = "test-token"

	msg := makeMsgWithPhoto(111, "owner", "@mybot /queue look at this")
	msg.Chat.Type = "supergroup"
	msg.CaptionEntities = []gotgbot.MessageEntity{{Type: "mention", Offset: 0, Length: int64(len("@mybot"))}}
	b.receiveMessage(context.Background(), msg)

	select {
	case qm := <-b.mq.Chan():
		if qm.Text != "look at this" {
			t.Errorf("queued text = %q, want %q", qm.Text, "look at this")
		}
		if qm.Steer != platform.SteerNever {
			t.Errorf("queued steer = %v, want SteerNever", qm.Steer)
		}
	default:
		t.Fatal("expected the captioned photo on the main queue")
	}
}

// TestReceive_BarePrefix_GroupUnaddressed_Silent: an un-mentioned bare
// /queue in a require-mention group is ignored with no usage reply (#1318) —
// the usage line belongs only to messages that were addressed to the bot.
func TestReceive_BarePrefix_GroupUnaddressed_Silent(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	requireMentionBot(t, b)
	b.receiveMessage(context.Background(), makeGroupMsg(111, "owner", "/queue"))

	if mock.sends != 0 {
		t.Errorf("sends = %d, want 0 (unaddressed bare prefix is silent)", mock.sends)
	}
	select {
	case <-b.mq.Chan():
		t.Fatal("unaddressed bare prefix enqueued a message")
	default:
	}
}

// TestReceive_SteerMenuTapBotSuffix: a Telegram command-menu tap in a group
// arrives as "/queue@mybot"; this bot's own suffix is stripped so the prefix
// parses. Another bot's suffix ("/queue@otherbot") is not ours to answer —
// it falls through to the command channel as today.
func TestReceive_SteerMenuTapBotSuffix(t *testing.T) {
	t.Run("own suffix stripped and delivered", func(t *testing.T) {
		b, _ := testBot([]string{"111"}, command.NewRegistry())
		b.api = &gotgbot.Bot{User: gotgbot.User{Id: 42, Username: "mybot"}}
		b.receiveMessage(context.Background(), makeMsg(111, "owner", "/queue@mybot hold this"))

		select {
		case qm := <-b.mq.Chan():
			if qm.Text != "hold this" {
				t.Errorf("queued text = %q, want %q", qm.Text, "hold this")
			}
			if qm.Steer != platform.SteerNever {
				t.Errorf("queued steer = %v, want SteerNever", qm.Steer)
			}
		default:
			t.Fatal("expected the menu-tap payload on the main queue")
		}
	})
	t.Run("own suffix bare taps get the usage reply", func(t *testing.T) {
		b, mock := testBot([]string{"111"}, command.NewRegistry())
		b.api = &gotgbot.Bot{User: gotgbot.User{Id: 42, Username: "mybot"}}
		b.receiveMessage(context.Background(), makeMsg(111, "owner", "/queue@mybot"))

		if mock.sends != 1 || sentUsageText(mock) != platform.SteerPrefixUsage {
			t.Errorf("sends = %d last = %q, want the one usage reply", mock.sends, sentUsageText(mock))
		}
		select {
		case <-b.mq.Chan():
			t.Fatal("bare menu tap enqueued a message")
		default:
		}
	})
	t.Run("other bot's suffix is not ours", func(t *testing.T) {
		b, _ := testBot([]string{"111"}, command.NewRegistry())
		b.api = &gotgbot.Bot{User: gotgbot.User{Id: 42, Username: "mybot"}}
		b.receiveMessage(context.Background(), makeMsg(111, "owner", "/queue@otherbot hi"))

		select {
		case qm := <-b.mq.Chan():
			t.Fatalf("other bot's tap delivered to our agent: %+v", qm)
		default:
		}
		select {
		case cmd := <-b.mq.CmdChan():
			if cmd.Text != "/queue@otherbot hi" {
				t.Errorf("command text = %q, want the untouched message", cmd.Text)
			}
		default:
			t.Fatal("expected the other bot's tap on the command channel (today's routing)")
		}
	})
}

// mentionUnits builds a mention entity of the bot from a UTF-16 offset —
// the coordinate system Telegram actually sends (multi-byte runes before
// the mention shift it away from the byte offset).
func mentionUnits(offset, length int64) gotgbot.MessageEntity {
	return gotgbot.MessageEntity{Type: "mention", Offset: offset, Length: length}
}

// TestReceive_SteerPrefix_MentionBehindNonAsciiText: a mention behind
// multi-byte runes still addresses a require-mention group message, and
// the prefix still parses — Telegram counts entity offsets in UTF-16 code
// units, so byte slicing (the round-5 bug) dropped "/queue привет @mybot"
// entirely: the mention was missed, the message ignored.
func TestReceive_SteerPrefix_MentionBehindNonAsciiText(t *testing.T) {
	for _, tc := range []struct {
		name      string
		text      string
		entities  []gotgbot.MessageEntity
		wantText  string
		wantSteer platform.SteerPreference
	}{
		{
			name:      "cyrillic before the mention",
			text:      "/queue привет @mybot",
			entities:  []gotgbot.MessageEntity{mentionUnits(14, 6)},
			wantText:  "привет @mybot",
			wantSteer: platform.SteerNever,
		},
		{
			name:      "emoji before the mention",
			text:      "/steer 😀 stop that @mybot",
			entities:  []gotgbot.MessageEntity{mentionUnits(20, 6)},
			wantText:  "😀 stop that @mybot",
			wantSteer: platform.SteerAlways,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := testBot([]string{"111"}, command.NewRegistry())
			requireMentionBot(t, b)
			b.api = &gotgbot.Bot{User: gotgbot.User{Id: 42, Username: "mybot"}}

			msg := makeGroupMsg(111, "owner", tc.text)
			msg.Entities = tc.entities
			b.receiveMessage(context.Background(), msg)

			select {
			case qm := <-b.mq.Chan():
				if qm.Text != tc.wantText {
					t.Errorf("queued text = %q, want %q", qm.Text, tc.wantText)
				}
				if qm.Steer != tc.wantSteer {
					t.Errorf("queued steer = %v, want %v", qm.Steer, tc.wantSteer)
				}
			default:
				t.Fatal("expected the addressed message on the main queue")
			}
		})
	}
}

// TestReceive_SteerPrefix_LeadingTextMentionNonAsciiName: a text_mention
// of this bot whose displayed name is multi-byte ("Фоци") strips by its
// UTF-16 length, not its byte length — the prefix behind it parses and no
// half of the name reaches the agent (the round-5 byte cut left "ци
// /queue x").
func TestReceive_SteerPrefix_LeadingTextMentionNonAsciiName(t *testing.T) {
	newBot := func(t *testing.T) (*Bot, *mockClient) {
		t.Helper()
		b, mock := testBot([]string{"111"}, command.NewRegistry())
		requireMentionBot(t, b)
		b.api = &gotgbot.Bot{User: gotgbot.User{Id: 42}} // no username: text_mention is the only mention form
		return b, mock
	}
	entity := gotgbot.MessageEntity{Type: "text_mention", Offset: 0, Length: 4, User: &gotgbot.User{Id: 42}}

	t.Run("prefix behind the name parses", func(t *testing.T) {
		b, _ := newBot(t)
		msg := makeGroupMsg(111, "owner", "Фоци /queue x")
		msg.Entities = []gotgbot.MessageEntity{entity}
		b.receiveMessage(context.Background(), msg)

		select {
		case qm := <-b.mq.Chan():
			if qm.Text != "x" {
				t.Errorf("queued text = %q, want %q", qm.Text, "x")
			}
			if qm.Steer != platform.SteerNever {
				t.Errorf("queued steer = %v, want SteerNever", qm.Steer)
			}
		default:
			t.Fatal("expected the payload behind the text_mention on the main queue")
		}
	})

	t.Run("bare prefix behind the name gets the usage reply", func(t *testing.T) {
		b, mock := newBot(t)
		msg := makeGroupMsg(111, "owner", "Фоци /queue")
		msg.Entities = []gotgbot.MessageEntity{entity}
		b.receiveMessage(context.Background(), msg)

		if mock.sends != 1 || sentUsageText(mock) != platform.SteerPrefixUsage {
			t.Errorf("sends = %d last = %q, want the one usage reply", mock.sends, sentUsageText(mock))
		}
		select {
		case <-b.mq.Chan():
			t.Fatal("bare prefix behind a text_mention enqueued a message")
		default:
		}
	})
}

// TestReceive_NonPrefixMentionMessagesUnchanged: the leading-mention strip
// exists only to uncover a /steer|/queue prefix. Without one, a message
// keeps its addressing verbatim, exactly as before the feature (round-5
// regression, review F2): the agent sees "@mybot hello there", a
// mention-only "@mybot" is still a delivered message — not a silent drop —
// and "@mybot /status" is payload for the agent, not a command.
func TestReceive_NonPrefixMentionMessagesUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		text     string
		wantText string
	}{
		{"mention and text kept", "@mybot hello there", "@mybot hello there"},
		{"mention-only message still delivered", "@mybot", "@mybot"},
		{"mentioned command stays payload", "@mybot /status now", "@mybot /status now"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, mock := testBot([]string{"111"}, command.NewRegistry())
			requireMentionBot(t, b)
			b.api = &gotgbot.Bot{User: gotgbot.User{Id: 42, Username: "mybot"}}

			msg := makeGroupMsg(111, "owner", tc.text)
			msg.Entities = []gotgbot.MessageEntity{mentionUnits(0, 6)}
			b.receiveMessage(context.Background(), msg)

			select {
			case qm := <-b.mq.Chan():
				if qm.Text != tc.wantText {
					t.Errorf("queued text = %q, want %q verbatim", qm.Text, tc.wantText)
				}
				if qm.Steer != platform.SteerDefault {
					t.Errorf("queued steer = %v, want SteerDefault (no prefix, no choice)", qm.Steer)
				}
			default:
				t.Fatal("expected the message on the main queue, delivered as before the feature")
			}
			select {
			case <-b.mq.CmdChan():
				t.Fatal("non-prefix message reached the command channel")
			default:
			}
			if mock.sends != 0 {
				t.Errorf("sends = %d, want 0 (nothing to answer)", mock.sends)
			}
		})
	}
}

// TestReceive_BarePrefixBehindMention_RepliesUsage: the bare form behind a
// leading mention ("@mybot /queue") replies the usage line like any other
// bare prefix — the mention is an addressing token, not message text.
func TestReceive_BarePrefixBehindMention_RepliesUsage(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	requireMentionBot(t, b)
	b.api = &gotgbot.Bot{User: gotgbot.User{Id: 42, Username: "mybot"}}

	msg := makeGroupMsg(111, "owner", "@mybot /queue")
	msg.Entities = []gotgbot.MessageEntity{mentionUnits(0, 6)}
	b.receiveMessage(context.Background(), msg)

	if mock.sends != 1 || sentUsageText(mock) != platform.SteerPrefixUsage {
		t.Errorf("sends = %d last = %q, want the one usage reply", mock.sends, sentUsageText(mock))
	}
	select {
	case <-b.mq.Chan():
		t.Fatal("bare prefix behind a mention enqueued a message")
	default:
	}
	select {
	case <-b.mq.CmdChan():
		t.Fatal("bare prefix behind a mention reached the command channel")
	default:
	}
}
