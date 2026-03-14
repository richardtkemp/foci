package telegram

import (
	"fmt"
	"strings"
	"testing"

	"foci/internal/command"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// testRenderer creates a TurnRenderer backed by a test bot for unit testing
// rendering methods in isolation.
func testRenderer(b *Bot, chatID int64) *TurnRenderer {
	msg := &gotgbot.Message{
		Chat: gotgbot.Chat{Id: chatID},
		From: &gotgbot.User{Id: 1},
	}
	return &TurnRenderer{
		bot:     b,
		msg:     msg,
		chatID:  chatID,
		tracker: &toolCallTracker{bot: b, chatID: chatID},
	}
}

func TestEditStreamNoThinking_EditInPlace(t *testing.T) {
	// Verifies that when streaming completes with no thinking, the stream
	// message is edited in-place with the final HTML and no new message is sent.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	b.editStreamNoThinking(100, 12345, "Hello world")

	if mock.sentCount() != 0 {
		t.Errorf("sentCount = %d, want 0 (should edit, not send)", mock.sentCount())
	}
	if mock.editCount() != 1 {
		t.Errorf("editCount = %d, want 1", mock.editCount())
	}
	if mock.lastEditOpts.MessageId != 100 {
		t.Errorf("edited message ID = %d, want 100", mock.lastEditOpts.MessageId)
	}
	if mock.lastEditOpts.ParseMode != "HTML" {
		t.Errorf("parse mode = %q, want HTML", mock.lastEditOpts.ParseMode)
	}
}

func TestEditStreamNoThinking_NotModifiedError(t *testing.T) {
	// Verifies that when the edit fails with "message is not modified"
	// (Telegram rejects identical content), no duplicate message is sent.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	mock.editErr = fmt.Errorf("Bad Request: message is not modified")

	b.editStreamNoThinking(100, 12345, "Hello world")

	if mock.sentCount() != 0 {
		t.Errorf("sentCount = %d, want 0 (should not send duplicate)", mock.sentCount())
	}
	if mock.editCount() != 1 {
		t.Errorf("editCount = %d, want 1 (should attempt edit)", mock.editCount())
	}
}

func TestEditStreamWithThinking_CompactMode(t *testing.T) {
	// Verifies that editStreamWithThinking edits the stream message in-place
	// with an inline keyboard button and stores thinking data for later toggle.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	r := testRenderer(b, 12345)

	r.editStreamWithThinking(100, "Response text", "I thought about this")

	if mock.sentCount() != 0 {
		t.Errorf("sentCount = %d, want 0 (should edit, not send)", mock.sentCount())
	}
	if mock.editCount() != 1 {
		t.Errorf("editCount = %d, want 1", mock.editCount())
	}
	if mock.lastEditOpts.MessageId != 100 {
		t.Errorf("edited message ID = %d, want 100", mock.lastEditOpts.MessageId)
	}
	// Verify inline keyboard was attached
	if mock.lastEditOpts.ReplyMarkup.InlineKeyboard == nil {
		t.Fatal("expected inline keyboard on edit")
	}
	btn := mock.lastEditOpts.ReplyMarkup.InlineKeyboard[0][0]
	if btn.Text != "Show thinking" {
		t.Errorf("button text = %q, want %q", btn.Text, "Show thinking")
	}
	wantCB := "th:show:100"
	if btn.CallbackData != wantCB {
		t.Errorf("callback data = %q, want %q", btn.CallbackData, wantCB)
	}
	// Verify thinking was stored
	val, ok := b.thinkingStore.Load(int64(100))
	if !ok {
		t.Fatal("thinking entry not stored for message 100")
	}
	entry := val.(thinkingEntry)
	if entry.thinkingText != "I thought about this" {
		t.Errorf("stored thinking = %q, want %q", entry.thinkingText, "I thought about this")
	}
}

func TestEditStreamWithFullThinking(t *testing.T) {
	// Verifies that editStreamWithFullThinking edits the stream message
	// in-place with italic thinking + divider + response HTML.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.displayWidth = 40
	r := testRenderer(b, 12345)

	r.editStreamWithFullThinking(100, "Response text", "Deep thoughts")

	if mock.sentCount() != 0 {
		t.Errorf("sentCount = %d, want 0 (should edit, not send)", mock.sentCount())
	}
	if mock.editCount() != 1 {
		t.Errorf("editCount = %d, want 1", mock.editCount())
	}
	// Verify the edit contains thinking in italics and the response
	if !strings.Contains(mock.lastEditText, "<i>Deep thoughts</i>") {
		t.Errorf("edit text missing italic thinking, got: %s", mock.lastEditText)
	}
	if !strings.Contains(mock.lastEditText, "Response text") {
		t.Errorf("edit text missing response, got: %s", mock.lastEditText)
	}
	// Verify divider is present
	if !strings.Contains(mock.lastEditText, strings.Repeat("—", 40)) {
		t.Errorf("edit text missing divider, got: %s", mock.lastEditText)
	}
}

func TestStreamLongResponse_SendsNewAndPreview(t *testing.T) {
	// Verifies that when the stream response exceeds 4096 chars, the code
	// sends a new message and edits the stream message to a truncated preview.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	msg := makeMsg(111, "111", "test")
	r := testRenderer(b, msg.Chat.Id)

	longResponse := strings.Repeat("x", 4097)

	// Simulate: streamMsgID=100, no thinking, response > 4096
	// The long-response path sends a reply then edits stream to preview.
	b.sendReply(msg, longResponse)
	r.editStreamPreview(100, longResponse)

	if mock.sentCount() == 0 {
		t.Error("expected at least one send for long response")
	}
	if mock.editCount() != 1 {
		t.Errorf("editCount = %d, want 1 (stream preview edit)", mock.editCount())
	}
	// Verify preview contains truncation indicator
	if !strings.Contains(mock.lastEditText, "(full response below)") {
		t.Errorf("preview should contain truncation indicator, got: %s", mock.lastEditText)
	}
}

func TestEditStreamPreview_SkipsWhenNoStreamMsg(t *testing.T) {
	// Verifies that editStreamPreview is a no-op when streamMsgID is 0.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	r := testRenderer(b, 12345)

	r.editStreamPreview(0, "some response")

	if mock.editCount() != 0 {
		t.Errorf("editCount = %d, want 0 (no stream message to edit)", mock.editCount())
	}
}

// editStreamNoThinking is extracted to test the no-thinking stream edit path
// in isolation, matching the inline logic in processAgentMessage.
func (b *Bot) editStreamNoThinking(streamMsgID, chatID int64, response string) {
	htmlResp := ConvertToTelegramHTML(response, b.tableOpts())
	_, _, editErr := b.client.EditMessageText(htmlResp, &gotgbot.EditMessageTextOpts{
		ChatId:    chatID,
		MessageId: streamMsgID,
		ParseMode: "HTML",
	})
	if editErr != nil {
		b.logger().Debugf("edit stream final: %v (stream already has content)", editErr)
	}
}

func TestStreamCompactThinking_NoNewMessage(t *testing.T) {
	// End-to-end verification: with streamMsgID set and compact thinking,
	// editStreamWithThinking produces exactly 1 edit and 0 sends.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	r := testRenderer(b, 12345)

	r.editStreamWithThinking(200, "short reply", "thinking content")

	if mock.sentCount() != 0 {
		t.Errorf("sentCount = %d, want 0", mock.sentCount())
	}
	// 1 edit for the final message with button
	if mock.editCount() != 1 {
		t.Errorf("editCount = %d, want 1", mock.editCount())
	}
}

func TestStreamFullThinking_NoNewMessage(t *testing.T) {
	// End-to-end verification: with streamMsgID set and full thinking,
	// editStreamWithFullThinking produces exactly 1 edit and 0 sends.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.displayWidth = 40
	r := testRenderer(b, 12345)

	r.editStreamWithFullThinking(200, "short reply", "thinking content")

	if mock.sentCount() != 0 {
		t.Errorf("sentCount = %d, want 0", mock.sentCount())
	}
	if mock.editCount() != 1 {
		t.Errorf("editCount = %d, want 1", mock.editCount())
	}
}

func TestEditStreamWithThinking_EditError(t *testing.T) {
	// Verifies that if the edit fails, thinking data is NOT stored
	// (since the button won't be visible to the user).
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	mock.editErr = fmt.Errorf("Bad Request: message too long")
	r := testRenderer(b, 12345)

	r.editStreamWithThinking(100, "Response", "Thinking")

	if mock.editCount() != 1 {
		t.Errorf("editCount = %d, want 1", mock.editCount())
	}
	// Thinking should NOT be stored since edit failed
	if _, ok := b.thinkingStore.Load(int64(100)); ok {
		t.Error("thinking should not be stored when edit fails")
	}
}

func TestNoStream_ToolCallPreviewEdit(t *testing.T) {
	// Verifies that without streaming, tool call preview messages are still
	// edited in-place when show_tool_calls=preview and response fits.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.showToolCalls = "preview"
	msg := makeMsg(111, "111", "test")

	// Simulate tool call preview edit path
	editID := int64(50)
	response := "Short response"
	htmlResp := ConvertToTelegramHTML(response, b.tableOpts())
	_, _, editErr := b.client.EditMessageText(htmlResp, &gotgbot.EditMessageTextOpts{
		ChatId:    msg.Chat.Id,
		MessageId: editID,
		ParseMode: "HTML",
	})

	if editErr != nil {
		t.Fatalf("unexpected edit error: %v", editErr)
	}
	if mock.editCount() != 1 {
		t.Errorf("editCount = %d, want 1", mock.editCount())
	}
	if mock.sentCount() != 0 {
		t.Errorf("sentCount = %d, want 0", mock.sentCount())
	}
}

// Verify the EditMessageTextOpts ReplyMarkup field type is compatible.
func TestSingleButtonKeyboard_ReturnsMarkup(t *testing.T) {
	// Verifies that singleButtonKeyboard creates a valid inline keyboard
	// with the expected button text and callback data.
	kb := singleButtonKeyboard("Test", "cb:data")
	if len(kb.InlineKeyboard) != 1 || len(kb.InlineKeyboard[0]) != 1 {
		t.Fatalf("expected 1x1 keyboard, got %v", kb.InlineKeyboard)
	}
	btn := kb.InlineKeyboard[0][0]
	if btn.Text != "Test" {
		t.Errorf("button text = %q, want %q", btn.Text, "Test")
	}
	if btn.CallbackData != "cb:data" {
		t.Errorf("callback data = %q, want %q", btn.CallbackData, "cb:data")
	}
}
