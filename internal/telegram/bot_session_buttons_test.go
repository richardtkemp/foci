package telegram

import (
	"testing"

	"foci/internal/command"
	"foci/internal/platform"
)

// promptButtons is the shared minimal button set for the session-addressed
// prompt tests.
func promptButtons() []platform.ButtonChoice {
	return []platform.ButtonChoice{{Label: "Yes", Data: "yes"}, {Label: "No", Data: "no"}}
}

// TestSendTextWithButtonsToSession_RoutesToSessionChat proves #2275: a
// session-addressed prompt send targets the chat embedded in the session key,
// NOT the default chat — and falls back to the default for a chatless key
// (agent/i<name>). The last arm pins the "no chat at all" error.
func TestSendTextWithButtonsToSession_RoutesToSessionChat(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	setDefaultChat(t, b, "main", 999)

	if _, err := b.SendTextWithButtonsToSession("main/c222", "Q?", promptButtons(), "im:"); err != nil {
		t.Fatalf("SendTextWithButtonsToSession: %v", err)
	}
	if mock.lastSendChatID != 222 {
		t.Errorf("routed to chat %d, want 222 (the session's chat, not the default 999)", mock.lastSendChatID)
	}

	if _, err := b.SendTextWithButtonsToSession("main/iwork", "Q?", promptButtons(), "im:"); err != nil {
		t.Fatalf("SendTextWithButtonsToSession (chatless key): %v", err)
	}
	if mock.lastSendChatID != 999 {
		t.Errorf("routed to chat %d, want 999 (default fallback for a chatless key)", mock.lastSendChatID)
	}

	bare, _ := testBot([]string{"111"}, command.NewRegistry()) // no default, no last chat
	if _, err := bare.SendTextWithButtonsToSession("main/iwork", "Q?", promptButtons(), "im:"); err == nil {
		t.Error("expected an error when no chat can be resolved (chatless key, no default and no last chat)")
	}
}

// TestEditMessageTextInSession_TargetsSessionChat proves the button-stripping
// edit addresses the session's chat (Telegram needs chatID+msgID; a msgID from
// another chat is rejected) (#2275).
func TestEditMessageTextInSession_TargetsSessionChat(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	setDefaultChat(t, b, "main", 999)

	if err := b.EditMessageTextInSession("main/c222", "7", "✅ done"); err != nil {
		t.Fatalf("EditMessageTextInSession: %v", err)
	}
	if mock.lastEditOpts == nil || mock.lastEditOpts.ChatId != 222 {
		t.Errorf("edit targeted chat %v, want 222 (the session's chat)", mock.lastEditOpts)
	}

	if err := b.EditMessageTextInSession("main/iwork", "7", "✅ done"); err != nil {
		t.Fatalf("EditMessageTextInSession (chatless key): %v", err)
	}
	if mock.lastEditOpts == nil || mock.lastEditOpts.ChatId != 999 {
		t.Errorf("edit targeted chat %v, want 999 (default fallback for a chatless key)", mock.lastEditOpts)
	}
}

// TestEditMessageWithButtonsInSession_TargetsSessionChat proves the
// keyboard-replacing edit addresses the session's chat and keeps the buttons
// (#2275).
func TestEditMessageWithButtonsInSession_TargetsSessionChat(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	setDefaultChat(t, b, "main", 999)

	if err := b.EditMessageWithButtonsInSession("main/c222", "7", "again?", promptButtons(), "im:"); err != nil {
		t.Fatalf("EditMessageWithButtonsInSession: %v", err)
	}
	if mock.lastEditOpts == nil || mock.lastEditOpts.ChatId != 222 {
		t.Errorf("edit targeted chat %v, want 222 (the session's chat)", mock.lastEditOpts)
	}
	if kb := mock.lastEditOpts.ReplyMarkup.InlineKeyboard; kb == nil {
		t.Error("expected the replacement keyboard to be present")
	}
}
