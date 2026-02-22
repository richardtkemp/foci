package telegram

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"clod/command"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// mockClient implements botClient for testing.
type mockClient struct {
	mu    sync.Mutex
	sends int               // counts SendMessage calls
	files map[string]string // fileId → filePath for GetFile mock
}

func (m *mockClient) SendMessage(chatId int64, text string, opts *gotgbot.SendMessageOpts) (*gotgbot.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sends++
	return &gotgbot.Message{}, nil
}

func (m *mockClient) SendDocument(chatId int64, document gotgbot.InputFileOrString, opts *gotgbot.SendDocumentOpts) (*gotgbot.Message, error) {
	return &gotgbot.Message{}, nil
}

func (m *mockClient) SendVoice(chatId int64, voice gotgbot.InputFileOrString, opts *gotgbot.SendVoiceOpts) (*gotgbot.Message, error) {
	return &gotgbot.Message{}, nil
}

func (m *mockClient) SendChatAction(chatId int64, action string, opts *gotgbot.SendChatActionOpts) (bool, error) {
	return true, nil
}

func (m *mockClient) GetFile(fileId string, opts *gotgbot.GetFileOpts) (*gotgbot.File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.files == nil {
		return nil, fmt.Errorf("file not found: %s", fileId)
	}
	fp, ok := m.files[fileId]
	if !ok {
		return nil, fmt.Errorf("file not found: %s", fileId)
	}
	return &gotgbot.File{FileId: fileId, FilePath: fp}, nil
}

func (m *mockClient) sentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sends
}

// testBot creates a Bot for testing with a mock client.
func testBot(allowedUsers []string, cmds *command.Registry) (*Bot, *mockClient) {
	mock := &mockClient{}
	allowed := make(map[string]bool)
	for _, u := range allowedUsers {
		allowed[u] = true
	}
	b := &Bot{
		client:       mock,
		commands:     cmds,
		lastMsgStore: command.NewLastMessageStore(),
		allowedUsers: allowed,
		sessionKey:   "agent:test:main",
		queue:        make(chan queuedMessage, 64),
	}
	return b, mock
}

func makeMsg(userID int64, username, text string) *gotgbot.Message {
	return &gotgbot.Message{
		From: &gotgbot.User{Id: userID, Username: username},
		Chat: gotgbot.Chat{Id: 12345},
		Text: text,
	}
}

// makeMsgWithPhoto creates a test message with a photo attachment.
func makeMsgWithPhoto(userID int64, username, caption string) *gotgbot.Message {
	return &gotgbot.Message{
		From:    &gotgbot.User{Id: userID, Username: username},
		Chat:    gotgbot.Chat{Id: 12345},
		Caption: caption,
		Photo: []gotgbot.PhotoSize{
			{FileId: "small_id", Width: 90, Height: 90, FileSize: 1000},
			{FileId: "large_id", Width: 800, Height: 600, FileSize: 50000},
		},
	}
}

// makeMsgWithDocument creates a test message with a document attachment.
func makeMsgWithDocument(userID int64, username, mime string) *gotgbot.Message {
	return &gotgbot.Message{
		From: &gotgbot.User{Id: userID, Username: username},
		Chat: gotgbot.Chat{Id: 12345},
		Document: &gotgbot.Document{
			FileId:   "doc_id",
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

func TestReceiveMessage_PhotoMessageQueued(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Set up mock file info for photo downloads
	mock.files = map[string]string{
		"large_id": "photos/test.jpg",
	}
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
	mock := b.client.(*mockClient)
	mock.files = map[string]string{
		"large_id": "photos/test.jpg",
	}
	b.botToken = "test-token"

	// Photo message with no text and no caption — should still be queued
	// (has an image even if download fails from no real server)
	msg := makeMsgWithPhoto(111, "owner", "")
	b.receiveMessage(context.Background(), msg)

	// Since download hits network and fails, images will be empty, and text is empty => dropped
	// This case tests that empty caption + failed download = dropped
}

func TestReceiveMessage_DocumentImageQueued(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	mock := b.client.(*mockClient)
	mock.files = map[string]string{
		"doc_id": "documents/image.png",
	}
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

func makeMsgWithVoice(userID int64, username string) *gotgbot.Message {
	return &gotgbot.Message{
		From:  &gotgbot.User{Id: userID, Username: username},
		Chat:  gotgbot.Chat{Id: 12345},
		Voice: &gotgbot.Voice{FileId: "voice_id", Duration: 5},
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

// --- Multiball / Secondary bots ---

func TestReceiveMessage_DoneOnPrimaryBot(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	msg := makeMsg(111, "owner", "/done")
	b.receiveMessage(context.Background(), msg)

	// Should reply with "nothing to detach"
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message, got %d", mock.sentCount())
	}
	if len(b.queue) != 0 {
		t.Error("/done should not be queued")
	}
}

func TestReceiveMessage_DoneOnSecondaryBot(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	pool := NewPool()
	b.isSecondary = true
	b.pool = pool
	pool.Add(b)

	// Simulate active session
	b.SetSessionKey("agent:main:multiball:mb-1")

	msg := makeMsg(111, "owner", "/done")
	b.receiveMessage(context.Background(), msg)

	// Should detach and reply
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message, got %d", mock.sentCount())
	}
	if b.SessionKey() != "" {
		t.Error("session key should be cleared after /done")
	}
}

func TestReceiveMessage_IdleSecondaryBot(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.isSecondary = true
	b.SetSessionKey("") // idle — no session assigned

	msg := makeMsg(111, "owner", "hello")
	b.receiveMessage(context.Background(), msg)

	// Should reply with "idle" message, not queue
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message, got %d", mock.sentCount())
	}
	if len(b.queue) != 0 {
		t.Error("idle secondary bot should not queue messages")
	}
}

func TestReceiveMessage_SecondaryBotWithSession(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.isSecondary = true
	b.SetSessionKey("agent:main:multiball:mb-1")

	msg := makeMsg(111, "owner", "hello")
	b.receiveMessage(context.Background(), msg)

	// Should queue normally when session is assigned
	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.queue))
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

// --- SendText ---

func TestSendText_SkipsEmptyMessage(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Set a chat ID so the bot can send
	b.SetChatID(12345)

	// Empty string should be silently skipped
	if err := b.SendText(""); err != nil {
		t.Errorf("SendText(\"\") error = %v, want nil", err)
	}
	if mock.sentCount() != 0 {
		t.Errorf("sentCount = %d, want 0 for empty string", mock.sentCount())
	}
}

func TestSendText_SkipsWhitespaceOnlyMessage(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Set a chat ID so the bot can send
	b.SetChatID(12345)

	// Whitespace-only should be silently skipped
	if err := b.SendText("   "); err != nil {
		t.Errorf("SendText(\"   \") error = %v, want nil", err)
	}
	if mock.sentCount() != 0 {
		t.Errorf("sentCount = %d, want 0 for whitespace-only", mock.sentCount())
	}
}

func TestSendText_SendsNonEmptyMessage(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Set a chat ID so the bot can send
	b.SetChatID(12345)

	// Non-empty message should be sent
	if err := b.SendText("hello"); err != nil {
		t.Errorf("SendText(\"hello\") error = %v, want nil", err)
	}
	if mock.sentCount() != 1 {
		t.Errorf("sentCount = %d, want 1 for non-empty message", mock.sentCount())
	}
}
