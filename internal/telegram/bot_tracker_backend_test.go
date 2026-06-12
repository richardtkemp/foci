package telegram

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/command"
	"foci/internal/tooldetail"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// newTrackerBackend builds a telegramTrackerBackend and store over a test bot.
func newTrackerBackend(t *testing.T) (*telegramTrackerBackend, *telegramTrackerStore, *Bot, *mockClient) {
	t.Helper()
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	return &telegramTrackerBackend{bot: b, chatID: 12345},
		&telegramTrackerStore{bot: b, chatID: 12345},
		b, mock
}

func TestTrackerBackend_Formatting(t *testing.T) {
	// Proves the tracker backend's pure formatting helpers produce the
	// Telegram-specific HTML surfaces: compact line, hint suffix with HTML
	// escaping, retry banner, and retry-clear confirmation.
	be, _, _, _ := newTrackerBackend(t)

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
	be, _, _, mock := newTrackerBackend(t)

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
	be, _, _, mock := newTrackerBackend(t)

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
	be, _, _, mock := newTrackerBackend(t)
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
	be, _, _, mock := newTrackerBackend(t)

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

func TestTrackerStore_StoreEntryAndIsExpanded(t *testing.T) {
	// Proves StoreEntry records a toolResultEntry in the bot's in-memory map
	// keyed by message ID, and IsExpanded reflects the stored flag (false for
	// unknown IDs).
	_, st, b, _ := newTrackerBackend(t)

	st.StoreEntry("100", "compact", "full", "result", true)
	val, ok := b.toolResults.Load(int64(100))
	if !ok {
		t.Fatal("entry not stored")
	}
	entry := val.(toolResultEntry)
	if entry.compactText != "compact" || entry.fullInput != "full" || entry.result != "result" || !entry.expanded {
		t.Errorf("unexpected entry: %+v", entry)
	}
	if entry.chatID != 12345 {
		t.Errorf("chatID = %d, want 12345", entry.chatID)
	}

	if !st.IsExpanded("100") {
		t.Error("IsExpanded(100) = false, want true")
	}
	if st.IsExpanded("999") {
		t.Error("IsExpanded(999) = true for unknown ID, want false")
	}
}

func TestTrackerStore_Persist(t *testing.T) {
	// Proves Persist is a no-op without a detail store, and write-through to
	// the SQLite store (readable via LoadAll) when one is configured.
	_, st, b, _ := newTrackerBackend(t)

	st.Persist("100", "c", "f", "r") // nil store: must not panic

	store, err := tooldetail.NewStore(filepath.Join(t.TempDir(), "details.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer func() { _ = store.Close() }()
	b.toolDetailStore = store

	st.Persist("100", "c", "f", "r")
	entries, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	e, ok := entries[100]
	if !ok {
		t.Fatal("persisted entry not found")
	}
	if e.CompactText != "c" || e.FullInput != "f" || e.Result != "r" {
		t.Errorf("unexpected entry: %+v", e)
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
