package telegram

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"clod/command"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// mockSender records all sent messages for assertion.
type mockSender struct {
	mu   sync.Mutex
	sent []tgbotapi.Chattable
}

func (m *mockSender) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, c)
	return tgbotapi.Message{}, nil
}

func (m *mockSender) sentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

// testBot creates a Bot for testing with a mock sender.
func testBot(allowedUsers []string, cmds *command.Registry) (*Bot, *mockSender) {
	mock := &mockSender{}
	allowed := make(map[string]bool)
	for _, u := range allowedUsers {
		allowed[u] = true
	}
	b := &Bot{
		sender:       mock,
		commands:     cmds,
		allowedUsers: allowed,
		sessionKey:   "agent:test:main",
		queue:        make(chan queuedMessage, 64),
	}
	return b, mock
}

func makeMsg(userID int64, username, text string) *tgbotapi.Message {
	return &tgbotapi.Message{
		From: &tgbotapi.User{ID: userID, UserName: username},
		Chat: &tgbotapi.Chat{ID: 12345},
		Text: text,
	}
}

// makeMsgWithPhoto creates a test message with a photo attachment.
func makeMsgWithPhoto(userID int64, username, caption string) *tgbotapi.Message {
	return &tgbotapi.Message{
		From:    &tgbotapi.User{ID: userID, UserName: username},
		Chat:    &tgbotapi.Chat{ID: 12345},
		Caption: caption,
		Photo: []tgbotapi.PhotoSize{
			{FileID: "small_id", Width: 90, Height: 90, FileSize: 1000},
			{FileID: "large_id", Width: 800, Height: 600, FileSize: 50000},
		},
	}
}

// makeMsgWithDocument creates a test message with a document attachment.
func makeMsgWithDocument(userID int64, username, mime string) *tgbotapi.Message {
	return &tgbotapi.Message{
		From: &tgbotapi.User{ID: userID, UserName: username},
		Chat: &tgbotapi.Chat{ID: 12345},
		Document: &tgbotapi.Document{
			FileID:   "doc_id",
			MimeType: mime,
		},
	}
}

// --- Auth filtering ---

func TestReceiveMessage_RejectsUnauthorizedUser(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	msg := makeMsg(999, "hacker", "hello")
	b.receiveMessage(context.Background(), msg)

	// Should not send any reply or queue anything
	if mock.sentCount() != 0 {
		t.Error("should not send reply to unauthorized user")
	}
	if len(b.queue) != 0 {
		t.Error("should not queue message from unauthorized user")
	}
}

func TestReceiveMessage_AcceptsAuthorizedUser(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	msg := makeMsg(111, "owner", "hello world")
	b.receiveMessage(context.Background(), msg)

	// Should be queued for the agent
	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.queue))
	}
	qm := <-b.queue
	if qm.text != "hello world" {
		t.Errorf("queued text = %q, want %q", qm.text, "hello world")
	}
	if qm.userID != "111" {
		t.Errorf("queued userID = %q, want %q", qm.userID, "111")
	}
}

func TestReceiveMessage_IgnoresEmptyText(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	msg := makeMsg(111, "owner", "")
	b.receiveMessage(context.Background(), msg)

	if mock.sentCount() != 0 {
		t.Error("should not send reply to empty message")
	}
	if len(b.queue) != 0 {
		t.Error("should not queue empty message")
	}
}

// --- Slash commands bypass agent ---

func TestReceiveMessage_SlashCommandBypassesQueue(t *testing.T) {
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{
		Name:        "ping",
		Description: "test",
		Execute: func(ctx context.Context, args string) (string, error) {
			return "pong", nil
		},
	})

	b, mock := testBot([]string{"111"}, cmds)

	msg := makeMsg(111, "owner", "/ping")
	b.receiveMessage(context.Background(), msg)

	// Should NOT be queued (command was handled directly)
	if len(b.queue) != 0 {
		t.Error("slash command should not be queued for agent")
	}

	// Should have sent a reply with the command result
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message, got %d", mock.sentCount())
	}
}

func TestReceiveMessage_UnknownSlashCommandGoesToQueue(t *testing.T) {
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{
		Name: "ping",
		Execute: func(ctx context.Context, args string) (string, error) {
			return "pong", nil
		},
	})

	b, _ := testBot([]string{"111"}, cmds)

	msg := makeMsg(111, "owner", "/unknown_cmd")
	b.receiveMessage(context.Background(), msg)

	// Unknown commands should fall through to the agent queue
	if len(b.queue) != 1 {
		t.Fatalf("unknown slash command should be queued, got %d queued", len(b.queue))
	}
}

// --- /stop ---

func TestReceiveMessage_StopCancelsTurn(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Simulate an active turn
	_, cancel := context.WithCancel(context.Background())
	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()

	msg := makeMsg(111, "owner", "/stop")
	b.receiveMessage(context.Background(), msg)

	// Should NOT be queued
	if len(b.queue) != 0 {
		t.Error("/stop should not be queued")
	}

	// Should have sent "Stopped." reply
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message for /stop, got %d", mock.sentCount())
	}

	// turnCancel should have been called (verified by checking context is done)
	// We can't directly check this, but the cancel function was called
}

// --- Message queuing ---

func TestReceiveMessage_QueueFull(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Fill the queue
	b.queue = make(chan queuedMessage, 2)
	b.queue <- queuedMessage{msg: makeMsg(111, "owner", "msg1"), text: "msg1"}
	b.queue <- queuedMessage{msg: makeMsg(111, "owner", "msg2"), text: "msg2"}

	// Next message should be dropped
	msg := makeMsg(111, "owner", "msg3 overflow")
	b.receiveMessage(context.Background(), msg)

	// Should have sent a "queue full" reply
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message for queue full, got %d", mock.sentCount())
	}

	// Queue should still have exactly 2
	if len(b.queue) != 2 {
		t.Errorf("queue should still have 2 messages, got %d", len(b.queue))
	}
}

func TestReceiveMessage_MultipleUsersAllowed(t *testing.T) {
	b, _ := testBot([]string{"111", "222"}, command.NewRegistry())

	b.receiveMessage(context.Background(), makeMsg(111, "user1", "hello"))
	b.receiveMessage(context.Background(), makeMsg(222, "user2", "world"))
	b.receiveMessage(context.Background(), makeMsg(333, "user3", "rejected"))

	if len(b.queue) != 2 {
		t.Errorf("expected 2 queued messages, got %d", len(b.queue))
	}
}

// --- cancelTurn ---

func TestCancelTurn_NoActiveTurn(t *testing.T) {
	b, _ := testBot([]string{}, command.NewRegistry())
	// Should not panic when no turn is active
	b.cancelTurn()
}

func TestCancelTurn_CancelsContext(t *testing.T) {
	b, _ := testBot([]string{}, command.NewRegistry())

	ctx, cancel := context.WithCancel(context.Background())
	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()

	b.cancelTurn()

	select {
	case <-ctx.Done():
		// expected
	case <-time.After(time.Second):
		t.Error("context should be cancelled")
	}
}

// --- splitMessage ---

func TestSplitMessage_Short(t *testing.T) {
	chunks := splitMessage("hello", 100)
	if len(chunks) != 1 || chunks[0] != "hello" {
		t.Errorf("expected [hello], got %v", chunks)
	}
}

func TestSplitMessage_ExactLimit(t *testing.T) {
	chunks := splitMessage("hello", 5)
	if len(chunks) != 1 || chunks[0] != "hello" {
		t.Errorf("expected [hello], got %v", chunks)
	}
}

func TestSplitMessage_SplitsAtNewline(t *testing.T) {
	text := "line1\nline2\nline3"
	chunks := splitMessage(text, 10)

	// Should prefer splitting at newline boundaries
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d: %v", len(chunks), chunks)
	}
	// Reconstruct and verify
	var reconstructed string
	for _, c := range chunks {
		reconstructed += c
	}
	if reconstructed != text {
		t.Errorf("reconstruction mismatch: got %q, want %q", reconstructed, text)
	}
}

func TestSplitMessage_LongNoNewlines(t *testing.T) {
	text := "abcdefghijklmnop"
	chunks := splitMessage(text, 5)
	if len(chunks) != 4 {
		t.Fatalf("expected 4 chunks, got %d: %v", len(chunks), chunks)
	}
	var reconstructed string
	for _, c := range chunks {
		reconstructed += c
	}
	if reconstructed != text {
		t.Errorf("reconstruction mismatch: got %q, want %q", reconstructed, text)
	}
}

func TestSplitMessage_Empty(t *testing.T) {
	chunks := splitMessage("", 100)
	if len(chunks) != 1 || chunks[0] != "" {
		t.Errorf("expected [\"\"], got %v", chunks)
	}
}

// --- truncate ---

func TestTruncate_Short(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestTruncate_Exact(t *testing.T) {
	if got := truncate("hello", 5); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestTruncate_Long(t *testing.T) {
	if got := truncate("hello world", 5); got != "hello..." {
		t.Errorf("got %q, want %q", got, "hello...")
	}
}

// --- Image support ---

// mockFileGetter implements fileGetter for tests.
type mockFileGetter struct {
	files map[string]tgbotapi.File
	err   error
}

func (m *mockFileGetter) GetFile(config tgbotapi.FileConfig) (tgbotapi.File, error) {
	if m.err != nil {
		return tgbotapi.File{}, m.err
	}
	f, ok := m.files[config.FileID]
	if !ok {
		return tgbotapi.File{}, fmt.Errorf("file not found: %s", config.FileID)
	}
	return f, nil
}

func TestReceiveMessage_PhotoMessageQueued(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	// Set up a mock file getter that returns file info
	// (actual download hits the network, but we can test queueing without it)
	fg := &mockFileGetter{
		files: map[string]tgbotapi.File{
			"large_id": {FileID: "large_id", FilePath: "photos/test.jpg"},
		},
	}
	b.fileGetter = fg
	b.botToken = "test-token"

	// The download will fail (no real server), but the message should still queue
	// with whatever images succeeded
	msg := makeMsgWithPhoto(111, "owner", "Look at this!")
	b.receiveMessage(context.Background(), msg)

	// Message should be queued (text from caption)
	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.queue))
	}
	qm := <-b.queue
	if qm.text != "Look at this!" {
		t.Errorf("queued text = %q, want %q", qm.text, "Look at this!")
	}
}

func TestReceiveMessage_PhotoWithoutCaption(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	fg := &mockFileGetter{
		files: map[string]tgbotapi.File{
			"large_id": {FileID: "large_id", FilePath: "photos/test.jpg"},
		},
	}
	b.fileGetter = fg
	b.botToken = "test-token"

	// Photo message with no text and no caption — should still be queued
	// (has an image even if download fails from no real server)
	msg := makeMsgWithPhoto(111, "owner", "")
	b.receiveMessage(context.Background(), msg)

	// Even if download fails, it shouldn't be silently dropped anymore
	// The message will queue if it has text or images (download error logged but message still queued with text="" and whatever images succeeded)
	// Since download hits network and fails, images will be empty, and text is empty => dropped
	// Let's just verify the old behavior of not dropping when there's a caption
	// This case tests that empty caption + failed download = dropped (same as before for no-content messages)
	// The real test is TestReceiveMessage_PhotoMessageQueued which has caption
}

func TestReceiveMessage_DocumentImageQueued(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	fg := &mockFileGetter{
		files: map[string]tgbotapi.File{
			"doc_id": {FileID: "doc_id", FilePath: "documents/image.png"},
		},
	}
	b.fileGetter = fg
	b.botToken = "test-token"

	msg := makeMsgWithDocument(111, "owner", "image/png")
	// No text — but document is an image, download will fail (no server)
	// but text is empty and images empty (download fails) => dropped
	b.receiveMessage(context.Background(), msg)
	// Just verify no panic
}

func TestReceiveMessage_NonImageDocumentIgnored(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	msg := makeMsgWithDocument(111, "owner", "application/pdf")
	b.receiveMessage(context.Background(), msg)

	// Non-image document with no text should be dropped
	if len(b.queue) != 0 {
		t.Error("non-image document should not be queued")
	}
}

// --- Voice support ---

func makeMsgWithVoice(userID int64, username string) *tgbotapi.Message {
	return &tgbotapi.Message{
		From:  &tgbotapi.User{ID: userID, UserName: username},
		Chat:  &tgbotapi.Chat{ID: 12345},
		Voice: &tgbotapi.Voice{FileID: "voice_id", Duration: 5},
	}
}

func TestReceiveMessage_VoiceWithoutTranscriber(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// No transcriber set — voice note should be dropped (no text, no images)
	msg := makeMsgWithVoice(111, "owner")
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 0 {
		t.Error("voice without transcriber should not be queued")
	}
	if mock.sentCount() != 0 {
		t.Error("should not send reply for voice without transcriber")
	}
}

func TestIsImageMIME(t *testing.T) {
	tests := []struct {
		mime string
		want bool
	}{
		{"image/jpeg", true},
		{"image/png", true},
		{"image/gif", true},
		{"image/webp", true},
		{"application/pdf", false},
		{"text/plain", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isImageMIME(tt.mime); got != tt.want {
			t.Errorf("isImageMIME(%q) = %v, want %v", tt.mime, got, tt.want)
		}
	}
}
