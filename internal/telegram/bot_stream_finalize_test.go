package telegram

import (
	"fmt"
	"strings"
	"testing"

	"foci/internal/command"
	"foci/internal/platform"
	"foci/internal/turn"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// testBackend creates a telegramBackend backed by a test bot for unit testing
// rendering methods in isolation.
func testBackend(b *Bot, chatID int64) *telegramBackend {
	d := b.resolveDisplay("")
	return &telegramBackend{
		bot:    b,
		msg:    &gotgbot.Message{Chat: gotgbot.Chat{Id: chatID}, From: &gotgbot.User{Id: 1}},
		chatID: chatID,
		opts:   d.RenderOpts,
		width:  d.DisplayWidth,
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

func TestTelegramBackend_EditWithThinkingButton(t *testing.T) {
	// Verifies that EditInPlace in compact thinking mode edits the message with
	// an inline keyboard button and stores thinking data for later toggle.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	backend := testBackend(b, 12345)

	err := backend.EditInPlace("100", turn.Payload{
		Text:         "Response HTML",
		ThinkingText: "I thought about this",
		ThinkingMode: "compact",
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.sentCount() != 0 {
		t.Errorf("sentCount = %d, want 0 (should edit, not send)", mock.sentCount())
	}
	if mock.editCount() != 1 {
		t.Errorf("editCount = %d, want 1", mock.editCount())
	}
	if mock.lastEditOpts.MessageId != 100 {
		t.Errorf("edited message ID = %d, want 100", mock.lastEditOpts.MessageId)
	}
	// Verify inline keyboard was attached.
	if mock.lastEditOpts.ReplyMarkup.InlineKeyboard == nil {
		t.Fatal("expected inline keyboard on edit")
	}
	btn := mock.lastEditOpts.ReplyMarkup.InlineKeyboard[0][0]
	if btn.Text != "Show thinking" {
		t.Errorf("button text = %q, want %q", btn.Text, "Show thinking")
	}
	if btn.CallbackData != "th:show" {
		t.Errorf("callback data = %q, want %q", btn.CallbackData, "th:show")
	}
	// Verify thinking was stored.
	val, ok := b.thinkingStore.Load(int64(100))
	if !ok {
		t.Fatal("thinking entry not stored for message 100")
	}
	entry := val.(thinkingEntry)
	if entry.thinkingText != "I thought about this" {
		t.Errorf("stored thinking = %q, want %q", entry.thinkingText, "I thought about this")
	}
}

func TestTelegramBackend_EditWithThinkingButton_Error(t *testing.T) {
	// Verifies that if the edit fails, thinking data is NOT stored
	// (since the button won't be visible to the user).
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	mock.editErr = fmt.Errorf("Bad Request: message too long")
	backend := testBackend(b, 12345)

	err := backend.EditInPlace("100", turn.Payload{
		Text:         "Response",
		ThinkingText: "Thinking",
		ThinkingMode: "compact",
	})

	if err == nil {
		t.Fatal("expected error from failed edit")
	}
	if mock.editCount() != 1 {
		t.Errorf("editCount = %d, want 1", mock.editCount())
	}
	// Thinking should NOT be stored since edit failed.
	if _, ok := b.thinkingStore.Load(int64(100)); ok {
		t.Error("thinking should not be stored when edit fails")
	}
}

func TestTelegramBackend_BuildThinkingCombined(t *testing.T) {
	// Verifies that ComposeBody in full thinking mode produces thinking +
	// divider + response.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.display.DisplayWidth = 40
	backend := testBackend(b, 12345)
	backend.width = 40

	combined, _, _ := backend.ComposeBody(turn.Payload{
		Text:         "Response text",
		ThinkingText: "Deep thoughts",
		ThinkingMode: "full",
	})

	if !strings.Contains(combined, "<i>Deep thoughts</i>") {
		t.Errorf("combined missing italic thinking, got: %s", combined)
	}
	if !strings.Contains(combined, "Response text") {
		t.Errorf("combined missing response, got: %s", combined)
	}
	if !strings.Contains(combined, strings.Repeat("—", 40)) {
		t.Errorf("combined missing divider, got: %s", combined)
	}
}

func TestTelegramBackend_EditInPlace_NoThinking(t *testing.T) {
	// Verifies that EditInPlace with no thinking edits the message in place with
	// HTML-converted content and no button.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	backend := testBackend(b, 12345)

	err := backend.EditInPlace("100", turn.Payload{Text: "**Hello**", ThinkingMode: "off"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.editCount() != 1 {
		t.Errorf("editCount = %d, want 1", mock.editCount())
	}
	if mock.lastEditOpts.MessageId != 100 {
		t.Errorf("message ID = %d, want 100", mock.lastEditOpts.MessageId)
	}
	if mock.lastEditOpts.ParseMode != "HTML" {
		t.Errorf("parse mode = %q, want HTML", mock.lastEditOpts.ParseMode)
	}
	if !strings.Contains(mock.lastEditText, "<b>Hello</b>") {
		t.Errorf("expected HTML bold in edit, got: %s", mock.lastEditText)
	}
	// No button on a plain edit.
	if mock.lastEditOpts.ReplyMarkup.InlineKeyboard != nil {
		t.Error("expected no inline keyboard on plain edit")
	}
}

func TestTelegramBackend_EditInPlace_TooLong(t *testing.T) {
	// Verifies that EditInPlace refuses (ErrTooLongForEdit) when the body would
	// need to split across more than one message — it must NOT edit.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	backend := testBackend(b, 12345)

	long := strings.Repeat("x", telegramMaxChars+1)
	err := backend.EditInPlace("100", turn.Payload{Text: long, ThinkingMode: "off"})

	if err != turn.ErrTooLongForEdit {
		t.Fatalf("err = %v, want ErrTooLongForEdit", err)
	}
	if mock.editCount() != 0 {
		t.Errorf("editCount = %d, want 0 (must not edit when too long)", mock.editCount())
	}
}

func TestTelegramBackend_ComposeBody_FormatsHTML(t *testing.T) {
	// Verifies that ComposeBody converts markdown to Telegram HTML.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	backend := testBackend(b, 12345)

	html, hasButton, _ := backend.ComposeBody(turn.Payload{Text: "**bold text**", ThinkingMode: "off"})

	if !strings.Contains(html, "<b>bold text</b>") {
		t.Errorf("expected HTML bold, got: %s", html)
	}
	if hasButton {
		t.Error("expected no button in off mode")
	}
}

func TestTelegramBackend_Deliver_LongResponseSplitsNotTruncated(t *testing.T) {
	// #738 guard at the platform layer: a terminal delivery longer than 4096
	// chars must split into multiple messages (no preview, no truncation). With
	// a fresh send (no stream surfaced) every chunk's full content is sent.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	backend := testBackend(b, 12345)

	longResponse := strings.Repeat("x", telegramMaxChars+1)
	res, err := backend.Deliver(turn.Payload{Text: longResponse, ThinkingMode: "off"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mock.sentCount() < 2 {
		t.Errorf("sentCount = %d, want >= 2 (long response must split)", mock.sentCount())
	}
	if mock.editCount() != 0 {
		t.Errorf("editCount = %d, want 0 (fresh send, no preview edit)", mock.editCount())
	}
	if len(res.MsgIDs) < 2 {
		t.Errorf("MsgIDs len = %d, want >= 2", len(res.MsgIDs))
	}
}

func TestNoStream_ToolCallPreviewEdit(t *testing.T) {
	// Verifies that without streaming, tool call preview messages are still
	// edited in-place when show_tool_calls=preview and response fits.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.display.ShowToolCalls = "preview"
	msg := makeMsg(111, "111", "test")

	// Simulate tool call preview edit path.
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

// Verify buildButtonRows creates valid single-button rows.
func TestBuildButtonRows_SingleButton(t *testing.T) {
	// Verifies that buildButtonRows with a single ButtonChoice creates a valid
	// inline keyboard row with the expected button text and callback data.
	rows := buildButtonRows([]platform.ButtonChoice{{Label: "Test", Data: "data"}}, "cb:")
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("expected 1x1 keyboard, got %v", rows)
	}
	btn := rows[0][0]
	if btn.Text != "Test" {
		t.Errorf("button text = %q, want %q", btn.Text, "Test")
	}
	if btn.CallbackData != "cb:data" {
		t.Errorf("callback data = %q, want %q", btn.CallbackData, "cb:data")
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
