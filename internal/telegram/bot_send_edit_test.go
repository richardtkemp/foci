package telegram

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/command"
	"foci/internal/dispatch"
	"foci/internal/platform"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

func TestEditMessageText(t *testing.T) {
	// Proves EditMessageText converts the text to Telegram HTML and edits the
	// identified message in the bot's current chat.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.SetChatID(12345)

	if err := b.EditMessageText("77", "**bold**"); err != nil {
		t.Fatalf("EditMessageText: %v", err)
	}
	if mock.editCount() != 1 || mock.lastEditOpts.MessageId != 77 {
		t.Errorf("edits=%d msgID=%d, want 1/77", mock.editCount(), mock.lastEditOpts.MessageId)
	}
	if !strings.Contains(mock.lastEditText, "<b>bold</b>") {
		t.Errorf("edit text = %q, want HTML bold", mock.lastEditText)
	}
	if kb := mock.lastEditOpts.ReplyMarkup.InlineKeyboard; kb != nil {
		t.Error("EditMessageText must remove buttons")
	}
}

func TestEditMessageWithButtons(t *testing.T) {
	// Proves EditMessageWithButtons edits the message and replaces its inline
	// keyboard with the prefixed callback buttons.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.SetChatID(12345)

	err := b.EditMessageWithButtons("78", "pick", []platform.ButtonChoice{{Label: "Yes", Data: "y"}}, "cmd:")
	if err != nil {
		t.Fatalf("EditMessageWithButtons: %v", err)
	}
	if mock.editCount() != 1 || mock.lastEditOpts.MessageId != 78 {
		t.Errorf("edits=%d msgID=%d, want 1/78", mock.editCount(), mock.lastEditOpts.MessageId)
	}
	btn := mock.lastEditOpts.ReplyMarkup.InlineKeyboard[0][0]
	if btn.Text != "Yes" || btn.CallbackData != "cmd:y" {
		t.Errorf("button = %q/%q, want Yes/cmd:y", btn.Text, btn.CallbackData)
	}
}

func TestSendTextWithButtons_NoChat(t *testing.T) {
	// Proves SendTextWithButtons fails with a clear error when no chat is
	// known yet (no default chat, no prior messages).
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	_, err := b.SendTextWithButtons("hi", []platform.ButtonChoice{{Label: "A", Data: "a"}}, "cmd:")
	if err == nil || !strings.Contains(err.Error(), "no chat ID") {
		t.Errorf("err = %v, want no-chat error", err)
	}
	if mock.sentCount() != 0 {
		t.Errorf("sends = %d, want 0", mock.sentCount())
	}
}

func TestSendStartupNotification(t *testing.T) {
	// Proves the startup notification is sent to the last known chat with a
	// restart message (plus diagnosis text when provided) and skipped
	// silently when no chat is known.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	b.SendStartupNotification("scout") // no chat yet: skip
	if mock.sentCount() != 0 {
		t.Fatalf("sends = %d, want 0 without chat", mock.sentCount())
	}

	b.SetChatID(12345)
	b.SendStartupNotification("scout")
	if mock.sentCount() != 1 || !strings.Contains(mock.lastSendInjected, "restarted at") {
		t.Errorf("sends=%d text=%q, want restart message", mock.sentCount(), mock.lastSendInjected)
	}

	b.SendStartupNotificationWithDiagnosis("scout", stubDiagnosis("crash loop detected"))
	if !strings.Contains(mock.lastSendInjected, "crash loop detected") {
		t.Errorf("text = %q, want diagnosis appended", mock.lastSendInjected)
	}
}

// stubDiagnosis implements StartupDiagnosis with a fixed notification string.
type stubDiagnosis string

func (s stubDiagnosis) FormatNotification() string { return string(s) }

func TestRenderCommandOutcome(t *testing.T) {
	// Proves renderCommandOutcome maps each outcome shape to the right
	// Telegram surface: keyboards and chains send buttoned messages, parts
	// send one message each, text sends one, DocPath sends a document and
	// removes the temp file, and NotHandled is silent.
	msg := makeMsg(111, "owner", "/x")

	t.Run("not handled is silent", func(t *testing.T) {
		b, mock := testBot([]string{"111"}, command.NewRegistry())
		b.renderCommandOutcome(msg, &dispatch.CommandOutcome{NotHandled: true})
		if mock.sentCount() != 0 {
			t.Errorf("sends = %d, want 0", mock.sentCount())
		}
	})

	t.Run("keyboard outcome sends buttons", func(t *testing.T) {
		b, mock := testBot([]string{"111"}, command.NewRegistry())
		b.SetChatID(12345)
		b.renderCommandOutcome(msg, &dispatch.CommandOutcome{
			Keyboard: &dispatch.KeyboardOutcome{
				CommandName: "pick", Header: "Pick one",
				Options: []command.KeyboardOption{{Label: "A", Data: "a"}},
			},
		})
		if mock.sentCount() != 1 {
			t.Fatalf("sends = %d, want 1", mock.sentCount())
		}
		if mock.lastSendOpts.ReplyMarkup == nil {
			t.Error("expected keyboard buttons")
		}
	})

	t.Run("chain outcome sends buttons", func(t *testing.T) {
		b, mock := testBot([]string{"111"}, command.NewRegistry())
		b.SetChatID(12345)
		b.renderCommandOutcome(msg, &dispatch.CommandOutcome{
			Chain: &dispatch.ChainOutcome{
				CommandName: "config", Label: "/config set:",
				Options: []command.KeyboardOption{{Label: "loop", Data: "set loop"}},
			},
		})
		if mock.sentCount() != 1 || mock.lastSendOpts.ReplyMarkup == nil {
			t.Errorf("sends=%d, want 1 buttoned message", mock.sentCount())
		}
	})

	t.Run("response parts send one message each", func(t *testing.T) {
		b, mock := testBot([]string{"111"}, command.NewRegistry())
		b.renderCommandOutcome(msg, &dispatch.CommandOutcome{
			Response: &dispatch.ResponseOutcome{
				Result: dispatch.Result{Handled: true, Response: command.Response{Parts: []string{"one", "two"}}},
			},
		})
		if mock.sentCount() != 2 {
			t.Errorf("sends = %d, want 2", mock.sentCount())
		}
	})

	t.Run("response text sends one message", func(t *testing.T) {
		b, mock := testBot([]string{"111"}, command.NewRegistry())
		b.renderCommandOutcome(msg, &dispatch.CommandOutcome{
			Response: &dispatch.ResponseOutcome{
				Result: dispatch.Result{Handled: true, Response: command.Response{Text: "done"}},
			},
		})
		if mock.sentCount() != 1 || !strings.Contains(mock.lastSendInjected, "done") {
			t.Errorf("sends=%d text=%q, want done", mock.sentCount(), mock.lastSendInjected)
		}
	})

	t.Run("response keyboard sends buttoned text", func(t *testing.T) {
		b, mock := testBot([]string{"111"}, command.NewRegistry())
		b.SetChatID(12345)
		b.renderCommandOutcome(msg, &dispatch.CommandOutcome{
			Response: &dispatch.ResponseOutcome{
				LookupText: "/pick",
				Result: dispatch.Result{Handled: true, Response: command.Response{
					Text:     "choose",
					Keyboard: []command.KeyboardOption{{Label: "A", Data: "a"}},
				}},
			},
		})
		if mock.sentCount() != 1 || mock.lastSendOpts.ReplyMarkup == nil {
			t.Errorf("sends=%d, want 1 buttoned message", mock.sentCount())
		}
	})

	t.Run("doc path sends document and removes file", func(t *testing.T) {
		b, mock := testBot([]string{"111"}, command.NewRegistry())
		docPath := filepath.Join(t.TempDir(), "out.txt")
		if err := os.WriteFile(docPath, []byte("content"), 0o600); err != nil {
			t.Fatal(err)
		}
		b.renderCommandOutcome(msg, &dispatch.CommandOutcome{
			Response: &dispatch.ResponseOutcome{
				Result: dispatch.Result{Handled: true, Response: command.Response{Text: "report", DocPath: docPath}},
			},
		})
		if mock.docCount() != 1 {
			t.Errorf("documents = %d, want 1", mock.docCount())
		}
		if _, err := os.Stat(docPath); !os.IsNotExist(err) {
			t.Error("doc file should be removed after sending")
		}
	})
}

func TestFormatUserInfo(t *testing.T) {
	// Proves user info formatting prefers username, falls back to first name,
	// then bare ID.
	tests := []struct {
		name string
		user *gotgbot.User
		want string
	}{
		{"username", &gotgbot.User{Id: 42, Username: "rich", FirstName: "Richard"}, "42 (rich)"},
		{"first name fallback", &gotgbot.User{Id: 42, FirstName: "Richard"}, "42 (Richard)"},
		{"bare id", &gotgbot.User{Id: 42}, "42"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatUserInfo(tt.user); got != tt.want {
				t.Errorf("formatUserInfo = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMessageContainsMention(t *testing.T) {
	// Proves @mention detection: matches the bot's username via a "mention"
	// entity, matches text_mention by user ID, and ignores other users or
	// bots without API identity.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.api = &gotgbot.Bot{User: gotgbot.User{Id: 99, Username: "focibot"}}

	mention := func(text string, entities ...gotgbot.MessageEntity) *gotgbot.Message {
		m := makeMsg(111, "owner", text)
		m.Entities = entities
		return m
	}

	if !b.messageContainsMention(mention("hi @focibot", gotgbot.MessageEntity{Type: "mention", Offset: 3, Length: 8})) {
		t.Error("should detect @focibot mention")
	}
	if b.messageContainsMention(mention("hi @otherbot", gotgbot.MessageEntity{Type: "mention", Offset: 3, Length: 9})) {
		t.Error("should not match a different bot's mention")
	}
	if !b.messageContainsMention(mention("hi", gotgbot.MessageEntity{Type: "text_mention", User: &gotgbot.User{Id: 99}})) {
		t.Error("should detect text_mention by user ID")
	}
	if b.messageContainsMention(mention("hi", gotgbot.MessageEntity{Type: "text_mention", User: &gotgbot.User{Id: 7}})) {
		t.Error("should not match text_mention for another user")
	}

	b.api = nil
	if b.messageContainsMention(mention("hi @focibot", gotgbot.MessageEntity{Type: "mention", Offset: 3, Length: 8})) {
		t.Error("bot without API identity cannot be mentioned")
	}
}
