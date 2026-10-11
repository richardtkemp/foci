package telegram

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"foci/internal/command"
	"foci/internal/turn"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// newTrackerBackend builds a telegramTrackerBackend over a test bot. The shared
// tool-result store (turn.ToolResultStore) is tested in the turn package.
func newTrackerBackend(t *testing.T) (*telegramTrackerBackend, *Bot, *mockClient) {
	t.Helper()
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	return &telegramTrackerBackend{bot: b, chatID: 12345}, b, mock
}

func TestTrackerBackend_Formatting(t *testing.T) {
	// Proves the tracker backend's pure formatting helpers produce the
	// Telegram-specific HTML surfaces: compact line, hint suffix with HTML
	// escaping, retry banner, and retry-clear confirmation.
	be, _, _ := newTrackerBackend(t)

	if got := be.FormatCompact("Read", []byte(`{"path":"/tmp/x"}`)); !strings.Contains(got, "Read") {
		t.Errorf("FormatCompact missing tool name: %q", got)
	}
	if got := be.FormatHintSuffix("a<b"); got != " → a&lt;b" {
		t.Errorf("FormatHintSuffix = %q, want escaped arrow suffix", got)
	}
	if got := be.FormatRetry("api.anthropic.com"); !strings.Contains(got, "api.anthropic.com is busy") {
		t.Errorf("FormatRetry = %q", got)
	}
	if got := be.FormatRetryClear(); !strings.Contains(got, "Request completed") {
		t.Errorf("FormatRetryClear = %q", got)
	}
	if got := be.FormatWithResult("<b>tool</b>", "out"); !strings.Contains(got, "Result:") || !strings.Contains(got, "out") {
		t.Errorf("FormatWithResult = %q", got)
	}
}

func TestTrackerBackend_SendAndSendWithButton(t *testing.T) {
	// Proves Send and SendWithButton deliver HTML messages to the tracker's
	// chat and return the sent Telegram message ID as a string; the button
	// variant attaches an inline keyboard with the given label/data.
	be, _, mock := newTrackerBackend(t)

	id, err := be.Send("<b>tool call</b>")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id != "1" {
		t.Errorf("Send id = %q, want \"1\"", id)
	}
	if mock.lastSendOpts.ParseMode != "HTML" {
		t.Errorf("parse mode = %q, want HTML", mock.lastSendOpts.ParseMode)
	}

	id, err = be.SendWithButton("text", "Show full", "show")
	if err != nil {
		t.Fatalf("SendWithButton: %v", err)
	}
	if id != "2" {
		t.Errorf("SendWithButton id = %q, want \"2\"", id)
	}
	kb, ok := mock.lastSendOpts.ReplyMarkup.(gotgbot.InlineKeyboardMarkup)
	if !ok || len(kb.InlineKeyboard) == 0 {
		t.Fatal("expected inline keyboard on SendWithButton")
	}
	btn := kb.InlineKeyboard[0][0]
	if btn.Text != "Show full" || btn.CallbackData != "show" {
		t.Errorf("button = %q/%q, want Show full/show", btn.Text, btn.CallbackData)
	}
}

func TestTrackerBackend_EditAndEditWithButton(t *testing.T) {
	// Proves Edit and EditWithButton edit the identified message in the
	// tracker's chat; the button variant attaches an inline keyboard.
	be, _, mock := newTrackerBackend(t)

	if err := be.Edit("42", "updated"); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if mock.editCount() != 1 || mock.lastEditOpts.MessageId != 42 {
		t.Errorf("edit count=%d msgID=%d, want 1/42", mock.editCount(), mock.lastEditOpts.MessageId)
	}

	if err := be.EditWithButton("43", "updated", "Hide", "hide"); err != nil {
		t.Fatalf("EditWithButton: %v", err)
	}
	if mock.editCount() != 2 || mock.lastEditOpts.MessageId != 43 {
		t.Errorf("edit count=%d msgID=%d, want 2/43", mock.editCount(), mock.lastEditOpts.MessageId)
	}
	if mock.lastEditOpts.ReplyMarkup.InlineKeyboard == nil {
		t.Fatal("expected inline keyboard on EditWithButton")
	}
	btn := mock.lastEditOpts.ReplyMarkup.InlineKeyboard[0][0]
	if btn.Text != "Hide" || btn.CallbackData != "hide" {
		t.Errorf("button = %q/%q, want Hide/hide", btn.Text, btn.CallbackData)
	}
}

func TestTrackerBackend_SendEditErrors(t *testing.T) {
	// Proves API failures surface as errors from Send/SendWithButton/Edit so
	// the shared tracker can fall back appropriately.
	be, _, mock := newTrackerBackend(t)
	mock.sendErr = fmt.Errorf("boom")
	mock.editErr = fmt.Errorf("boom")

	if _, err := be.Send("x"); err == nil {
		t.Error("Send: expected error")
	}
	if _, err := be.SendWithButton("x", "l", "d"); err == nil {
		t.Error("SendWithButton: expected error")
	}
	if err := be.Edit("1", "x"); err == nil {
		t.Error("Edit: expected error")
	}
	if err := be.EditWithButton("1", "x", "l", "d"); err == nil {
		t.Error("EditWithButton: expected error")
	}
}

func TestTrackerBackend_DeleteAndLogger(t *testing.T) {
	// Proves Delete removes the identified message (best-effort, nil error)
	// and Logger returns the bot's component logger.
	be, _, mock := newTrackerBackend(t)

	if err := be.Delete("7"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if mock.deleteCount() != 1 {
		t.Errorf("deletes = %d, want 1", mock.deleteCount())
	}
	if be.Logger() == nil {
		t.Error("Logger returned nil")
	}
}

func TestFormatToolCallWithResult_Truncation(t *testing.T) {
	// Proves results are HTML-escaped and truncated so the combined message
	// stays within Telegram's 4096-char limit, and that an over-long tool
	// text skips the result section entirely.
	short := formatToolCallWithResult("tool", "a<b")
	if !strings.Contains(short, "a&lt;b") {
		t.Errorf("result not escaped: %q", short)
	}

	long := formatToolCallWithResult("tool", strings.Repeat("x", 5000))
	if len(long) > 4096 {
		t.Errorf("len = %d, want <= 4096", len(long))
	}
	if !strings.HasSuffix(long, "...</pre>") {
		t.Errorf("expected ellipsis before closing tag, got tail %q", long[len(long)-20:])
	}

	hugeTool := strings.Repeat("y", 5000)
	if got := formatToolCallWithResult(hugeTool, "result"); got != hugeTool {
		t.Error("over-long tool text should be returned unchanged without result")
	}
}

func TestNewToolCallTracker(t *testing.T) {
	// Proves the constructor wires a non-nil shared tracker from the bot's
	// backend, store, and display settings.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	tr := newToolCallTracker(b, 12345, b.resolveDisplay(""))
	if tr == nil {
		t.Fatal("tracker is nil")
	}
}

func TestTrackerFullMode_StoresLoadedSkillLine(t *testing.T) {
	// Proves a SKILL.md read flowing through the full tracker chain — the
	// verbatim ccstream tool name/params — lands in chat and in the stored
	// entry as the chip: the sent message and CompactText are the chip line,
	// FullInput still carries the real path, and a second load of the same
	// skill gets its own entry (no de-duplication, #1299).
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	tr := newToolCallTracker(b, 12345, turn.TurnDisplay{ShowToolCalls: "full"})

	const skillPath = "/home/foci/shared/skills/writing-code/SKILL.md"
	params := json.RawMessage(`{"file_path":"` + skillPath + `"}`)
	tr.ObserveToolCall("t1", "Read", params)
	tr.ObserveToolCall("t2", "Read", params)

	const chip = "📖 Loaded skill: writing-code"
	if got := mock.sentCount(); got != 2 {
		t.Fatalf("sends = %d, want 2 (one per load, no dedup)", got)
	}
	if got := mock.lastSendInjected; got != chip {
		t.Errorf("sent text = %q, want %q", got, chip)
	}
	for _, id := range []string{"1", "2"} {
		e, ok := b.toolStore.Load(id)
		if !ok {
			t.Fatalf("no stored entry for message %s", id)
		}
		if e.CompactText != chip {
			t.Errorf("entry %s CompactText = %q, want %q", id, e.CompactText, chip)
		}
		if !strings.Contains(e.FullInput, skillPath) {
			t.Errorf("entry %s FullInput lost the real path: %q", id, e.FullInput)
		}
	}
}
