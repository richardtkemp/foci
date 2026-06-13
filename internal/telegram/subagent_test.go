package telegram

import (
	"testing"

	"foci/internal/dispatch"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// newSubagentTestBackend wires a telegramBackend to a fresh Bot+mockClient for
// exercising the rolling "Hide this" button path.
func newSubagentTestBackend() (*telegramBackend, *Bot, *mockClient) {
	mock := &mockClient{}
	b := &Bot{client: mock}
	be := &telegramBackend{bot: b, chatID: 42}
	return be, b, mock
}

// TestSubagentRollingButton: each new subagent message carries the Hide button
// and strips it from the previous one — so exactly the newest message holds it.
func TestSubagentRollingButton(t *testing.T) {
	be, _, mock := newSubagentTestBackend()

	be.DeliverSubagentText("toolu_abc", "> first")
	if mock.sends != 1 {
		t.Fatalf("after msg 1: sends=%d, want 1", mock.sends)
	}
	if mock.markupEdits != 0 {
		t.Fatalf("after msg 1: markupEdits=%d, want 0 (nothing to strip yet)", mock.markupEdits)
	}
	// First message must carry an inline keyboard (the Hide button).
	if mock.lastSendOpts == nil {
		t.Fatalf("msg 1 sent without opts")
	}
	kb, ok := mock.lastSendOpts.ReplyMarkup.(gotgbot.InlineKeyboardMarkup)
	if !ok || len(kb.InlineKeyboard) == 0 {
		t.Fatalf("msg 1 sent without an inline keyboard (markup=%T)", mock.lastSendOpts.ReplyMarkup)
	}

	be.DeliverSubagentText("toolu_abc", "> second")
	if mock.sends != 2 {
		t.Fatalf("after msg 2: sends=%d, want 2", mock.sends)
	}
	// The previous message (id 1) must have had its button stripped.
	if mock.markupEdits != 1 {
		t.Fatalf("after msg 2: markupEdits=%d, want 1 (strip msg 1's button)", mock.markupEdits)
	}
	if mock.lastMarkupOpts == nil || mock.lastMarkupOpts.MessageId != 1 {
		t.Fatalf("strip targeted wrong message: %+v", mock.lastMarkupOpts)
	}
	if len(mock.lastMarkupOpts.ReplyMarkup.InlineKeyboard) != 0 {
		t.Fatalf("strip should clear the keyboard, got %d rows", len(mock.lastMarkupOpts.ReplyMarkup.InlineKeyboard))
	}
}

// TestSubagentHideDeletesAndSuppresses: clicking Hide deletes the whole group
// and drops any further messages from that subagent.
func TestSubagentHideDeletesAndSuppresses(t *testing.T) {
	be, b, mock := newSubagentTestBackend()

	be.DeliverSubagentText("toolu_xyz", "> one")
	be.DeliverSubagentText("toolu_xyz", "> two")
	if mock.sends != 2 {
		t.Fatalf("setup: sends=%d, want 2", mock.sends)
	}

	token := subagentToken(be.chatID, "toolu_xyz")
	b.handleSubagentHideCallback(be.chatID, token)

	if mock.deletes != 2 {
		t.Fatalf("hide: deletes=%d, want 2 (both messages)", mock.deletes)
	}
	if len(mock.deletedIDs) != 2 || mock.deletedIDs[0] != 1 || mock.deletedIDs[1] != 2 {
		t.Fatalf("hide deleted wrong ids: %v, want [1 2]", mock.deletedIDs)
	}

	// Further messages from the same subagent are suppressed.
	be.DeliverSubagentText("toolu_xyz", "> three")
	if mock.sends != 2 {
		t.Fatalf("after suppress: sends=%d, want 2 (msg 3 dropped)", mock.sends)
	}
}

// TestSubagentSeparateGroups: distinct subagents get independent buttons; no
// cross-strip between groups.
func TestSubagentSeparateGroups(t *testing.T) {
	be, _, mock := newSubagentTestBackend()

	be.DeliverSubagentText("toolu_A", "> a1")
	be.DeliverSubagentText("toolu_B", "> b1")
	// Two distinct groups, each its first message → no strips yet.
	if mock.markupEdits != 0 {
		t.Fatalf("markupEdits=%d, want 0 (different subagents, no cross-strip)", mock.markupEdits)
	}
	if mock.sends != 2 {
		t.Fatalf("sends=%d, want 2", mock.sends)
	}
}

// TestParseCallbackSubagentHide: the "sa:" prefix routes to CallbackSubagentHide.
func TestParseCallbackSubagentHide(t *testing.T) {
	action, data := dispatch.ParseCallback("sa:deadbeefdeadbeef")
	if action != dispatch.CallbackSubagentHide {
		t.Fatalf("action=%d, want CallbackSubagentHide", action)
	}
	if data != "deadbeefdeadbeef" {
		t.Fatalf("data=%q, want token", data)
	}
}
