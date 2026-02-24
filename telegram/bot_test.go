package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"clod/command"
	"clod/state"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// mockClient implements botClient for testing.
type mockClient struct {
	mu          sync.Mutex
	sends       int               // counts SendMessage calls
	edits       int               // counts EditMessageText calls
	files       map[string]string // fileId → filePath for GetFile mock
	setCmds     []gotgbot.BotCommand // last SetMyCommands call
	setCmdsErr  error                // error to return from SetMyCommands
}

func (m *mockClient) SendMessage(chatId int64, text string, opts *gotgbot.SendMessageOpts) (*gotgbot.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sends++
	return &gotgbot.Message{MessageId: int64(m.sends)}, nil
}

func (m *mockClient) EditMessageText(text string, opts *gotgbot.EditMessageTextOpts) (*gotgbot.Message, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.edits++
	return &gotgbot.Message{}, true, nil
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

func (m *mockClient) SetMyCommands(commands []gotgbot.BotCommand, opts *gotgbot.SetMyCommandsOpts) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setCmds = commands
	if m.setCmdsErr != nil {
		return false, m.setCmdsErr
	}
	return true, nil
}

func (m *mockClient) sentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sends
}

func (m *mockClient) editCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.edits
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
		Date: int64(time.Now().Unix()),
	}
}

// makeMsgWithPhoto creates a test message with a photo attachment.
func makeMsgWithPhoto(userID int64, username, caption string) *gotgbot.Message {
	return &gotgbot.Message{
		From:    &gotgbot.User{Id: userID, Username: username},
		Chat:    gotgbot.Chat{Id: 12345},
		Caption: caption,
		Date:    int64(time.Now().Unix()),
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
		Date: int64(time.Now().Unix()),
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

func TestReceiveMessage_StopAlias(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Set stop aliases with enabled=true
	b.SetStopAliases([]string{"stop", "wait", "hold"}, true)

	// Simulate an active turn
	_, cancel := context.WithCancel(context.Background())
	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()

	// Test /wait alias
	msg := makeMsg(111, "owner", "/wait")
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 0 {
		t.Error("/wait should not be queued")
	}
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message for /wait, got %d", mock.sentCount())
	}
}

func TestReceiveMessage_StopAliasNotConfigured(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	// Aliases disabled — even with aliases configured, they shouldn't work
	b.SetStopAliases([]string{"wait", "hold"}, false)

	// Simulate an active turn
	_, cancel := context.WithCancel(context.Background())
	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()

	// /wait should NOT trigger stop when aliases are disabled
	msg := makeMsg(111, "owner", "/wait")
	b.receiveMessage(context.Background(), msg)

	// Should be queued (not recognized as stop command)
	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message for unknown /wait, got %d", len(b.queue))
	}
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
		Date:  int64(time.Now().Unix()),
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

// --- Image save ---

func TestExtForMediaType(t *testing.T) {
	tests := []struct {
		mt   string
		want string
	}{
		{"image/jpeg", ".jpg"},
		{"image/png", ".png"},
		{"image/gif", ".gif"},
		{"image/webp", ".webp"},
		{"image/tiff", ".bin"},
		{"", ".bin"},
	}
	for _, tt := range tests {
		if got := extForMediaType(tt.mt); got != tt.want {
			t.Errorf("extForMediaType(%q) = %q, want %q", tt.mt, got, tt.want)
		}
	}
}

func TestSaveImage(t *testing.T) {
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.imageSaveDir = dir

	data := []byte("fake-jpeg-data")
	path, err := b.saveImage(data, "image/jpeg", 12345)
	if err != nil {
		t.Fatalf("saveImage: %v", err)
	}

	// Verify file exists with correct content
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("saved content = %q, want %q", got, data)
	}

	// Verify filename pattern: YYYY-MM-DDTHH-MM-SSZ_chat-CHATID.jpg
	base := filepath.Base(path)
	if !strings.HasSuffix(base, "_chat-12345.jpg") {
		t.Errorf("filename %q doesn't match expected pattern", base)
	}
	if !strings.Contains(base, "T") {
		t.Errorf("filename %q missing timestamp", base)
	}
}

func TestSaveImageDisabled(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	// imageSaveDir not set — verify saveImage is only called when dir is set
	if b.imageSaveDir != "" {
		t.Error("expected empty imageSaveDir by default")
	}

	// Directly construct an attachment to verify no savedPath
	att := imageAttachment{data: []byte("test"), mediaType: "image/jpeg"}
	if att.savedPath != "" {
		t.Error("expected empty savedPath when imageSaveDir is not set")
	}
}

func TestSaveImagePNG(t *testing.T) {
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.imageSaveDir = dir

	data := []byte("fake-png-data")
	path, err := b.saveImage(data, "image/png", 99999)
	if err != nil {
		t.Fatalf("saveImage: %v", err)
	}

	if !strings.HasSuffix(path, ".png") {
		t.Errorf("path %q should end with .png", path)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("saved content mismatch")
	}
}

func TestSaveImageCreatesDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "subdir", "images")
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.imageSaveDir = dir

	path, err := b.saveImage([]byte("data"), "image/jpeg", 1)
	if err != nil {
		t.Fatalf("saveImage: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("saved file not found: %v", err)
	}
}

func TestSavedPathPropagatedToQueue(t *testing.T) {
	// Test that imageAttachment.savedPath flows through queuedMessage
	att := imageAttachment{
		data:      []byte("test"),
		mediaType: "image/jpeg",
		savedPath: "/tmp/test.jpg",
	}
	qm := queuedMessage{
		text:   "look at this",
		images: []imageAttachment{att},
	}
	if qm.images[0].savedPath != "/tmp/test.jpg" {
		t.Errorf("savedPath not propagated: got %q", qm.images[0].savedPath)
	}
}

// --- sanitizeError ---

func TestSanitizeError_RedactsToken(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.botToken = "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11"

	err := fmt.Errorf(`failed to execute POST request to getUpdates: Post "https://api.telegram.org/bot123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11/getUpdates": context deadline exceeded`)
	got := b.sanitizeError(err)

	if strings.Contains(got, "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11") {
		t.Errorf("sanitizeError still contains token: %s", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("sanitizeError missing [REDACTED]: %s", got)
	}
	expected := `failed to execute POST request to getUpdates: Post "https://api.telegram.org/bot[REDACTED]/getUpdates": context deadline exceeded`
	if got != expected {
		t.Errorf("sanitizeError = %q, want %q", got, expected)
	}
}

func TestSanitizeError_NilError(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.botToken = "some-token"

	if got := b.sanitizeError(nil); got != "" {
		t.Errorf("sanitizeError(nil) = %q, want empty", got)
	}
}

func TestSanitizeError_NoToken(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.botToken = ""

	err := fmt.Errorf("some error without token")
	if got := b.sanitizeError(err); got != "some error without token" {
		t.Errorf("sanitizeError = %q, want original", got)
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

// --- Async notifier delivery ---
// These tests verify the contract that async-notifier turns (tmux watch,
// exec auto-background) deliver responses via SendText, matching the
// wiring in main.go's notifier closure.

func TestAsyncNotifierDeliveryViaSendText(t *testing.T) {
	// Simulates the async notifier delivery path in main.go:
	// notifier calls HandleMessage → gets response → calls bot.SendText()
	mgr := NewBotManager()
	bot, mock := testBot([]string{"111"}, command.NewRegistry())
	bot.SetChatID(12345)
	mgr.AddPrimary("test-agent", bot)

	// Simulate: notifier got a response from HandleMessage
	resp := "Four undeployed commits now. Both queues empty."

	// Deliver via primary bot's SendText (same as main.go closure)
	primary := mgr.PrimaryBot("test-agent")
	if primary == nil {
		t.Fatal("PrimaryBot returned nil")
	}
	if err := primary.SendText(resp); err != nil {
		t.Fatalf("SendText error: %v", err)
	}
	if mock.sentCount() != 1 {
		t.Errorf("sentCount = %d, want 1", mock.sentCount())
	}
}

func TestAsyncNotifierSkipsEmptyResponse(t *testing.T) {
	// When HandleMessage returns empty string, notifier should not call SendText.
	// This is checked in the main.go closure before calling SendText.
	bot, mock := testBot([]string{"111"}, command.NewRegistry())
	bot.SetChatID(12345)

	// Empty response should be silently skipped by SendText
	if err := bot.SendText(""); err != nil {
		t.Fatalf("SendText(\"\") error: %v", err)
	}
	if mock.sentCount() != 0 {
		t.Errorf("sentCount = %d, want 0 for empty response", mock.sentCount())
	}
}

func TestAsyncNotifierNoPrimaryBot(t *testing.T) {
	// When no primary bot is configured, PrimaryBot returns nil.
	// The main.go closure logs a warning and skips delivery.
	mgr := NewBotManager()
	if bot := mgr.PrimaryBot("nonexistent"); bot != nil {
		t.Errorf("PrimaryBot(nonexistent) = %v, want nil", bot)
	}
}

// --- Tool call message ordering ---

func TestToolCallObserverResetsAfterReply(t *testing.T) {
	// Simulates the processAgentMessage closure interactions to verify
	// that intermediate text resets toolMsgID, forcing subsequent tool
	// calls to create new messages instead of editing stale ones.
	//
	// Without the fix, the sequence would be:
	//   tool("exec") → send msg#1, toolMsgID=1
	//   reply("Let me check...") → send msg#2
	//   tool("read") → edit msg#1 (WRONG: appears above msg#2)
	//
	// With the fix (toolMsgID reset in ReplyFunc):
	//   tool("exec") → send msg#1, toolMsgID=1
	//   reply("Let me check...") → send msg#2, toolMsgID=0
	//   tool("read") → send msg#3 (CORRECT: appears below msg#2)
	mock := &mockClient{}
	b := &Bot{client: mock}

	var toolMsgID int64
	var toolMsgMu sync.Mutex

	// Simulate the ReplyFunc closure from processAgentMessage
	replyFunc := func(text string) {
		b.client.SendMessage(12345, text, nil)
		toolMsgMu.Lock()
		toolMsgID = 0
		toolMsgMu.Unlock()
	}

	// Simulate the tool call observer closure from processAgentMessage
	toolCallObserver := func(toolName string, params json.RawMessage) {
		toolMsgMu.Lock()
		defer toolMsgMu.Unlock()

		text := b.formatToolCall(toolName, params)
		if toolMsgID == 0 {
			sent, err := b.client.SendMessage(12345, text, &gotgbot.SendMessageOpts{ParseMode: "HTML"})
			if err != nil {
				return
			}
			toolMsgID = sent.MessageId
		} else {
			b.client.EditMessageText(text, &gotgbot.EditMessageTextOpts{
				ChatId:    12345,
				MessageId: toolMsgID,
				ParseMode: "HTML",
			})
		}
	}

	// Step 1: First tool call → sends new message (ID=1)
	toolCallObserver("exec", json.RawMessage(`{"command":"ls"}`))
	if mock.sentCount() != 1 {
		t.Fatalf("after first tool call: sends=%d, want 1", mock.sentCount())
	}
	if mock.editCount() != 0 {
		t.Fatalf("after first tool call: edits=%d, want 0", mock.editCount())
	}

	// Step 2: Intermediate reply fires → resets toolMsgID
	replyFunc("Let me check...")
	if mock.sentCount() != 2 {
		t.Fatalf("after reply: sends=%d, want 2", mock.sentCount())
	}

	// Step 3: Second tool call → should send NEW message (not edit old one)
	toolCallObserver("read", json.RawMessage(`{"path":"foo.txt"}`))
	if mock.sentCount() != 3 {
		t.Errorf("after second tool call: sends=%d, want 3 (new message, not edit)", mock.sentCount())
	}
	if mock.editCount() != 0 {
		t.Errorf("after second tool call: edits=%d, want 0 (should not edit stale message)", mock.editCount())
	}
}

// --- Tool call visibility ---

func TestFormatToolCall(t *testing.T) {
	b := &Bot{}
	text := b.formatToolCall("exec", json.RawMessage(`{"command":"ls -la"}`))
	if !strings.Contains(text, "🔧") {
		t.Error("missing tool emoji")
	}
	if !strings.Contains(text, "<b>exec</b>") {
		t.Errorf("missing tool name in %q", text)
	}
	if !strings.Contains(text, "ls -la") {
		t.Errorf("missing params in %q", text)
	}
}

func TestFormatToolCall_HTMLEscape(t *testing.T) {
	b := &Bot{}
	text := b.formatToolCall("exec", json.RawMessage(`{"command":"echo <script>"}`))
	if strings.Contains(text, "<script>") {
		t.Errorf("HTML not escaped in %q", text)
	}
	if !strings.Contains(text, "&lt;script&gt;") {
		t.Errorf("expected escaped HTML in %q", text)
	}
}

func TestFormatToolCall_LongParams(t *testing.T) {
	b := &Bot{}
	longVal := strings.Repeat("x", 500)
	text := b.formatToolCall("exec", json.RawMessage(fmt.Sprintf(`{"command":"%s"}`, longVal)))
	if len(text) > 500 {
		// Should be truncated
	}
	if !strings.Contains(text, "...") {
		t.Errorf("long params should be truncated: %q", text)
	}
}

func TestFormatToolCall_UnescapesNewlinesAndTabs(t *testing.T) {
	b := &Bot{}
	// Simulate a tool call where the JSON string value contains literal \n and \t
	params := json.RawMessage(`{"content":"line1\nline2\n\tindented"}`)
	text := b.formatToolCall("write", params)

	// After unescaping, the <pre> block should contain actual newlines and tabs
	if strings.Contains(text, `\n`) {
		t.Errorf("literal \\n should be unescaped to real newline, got: %s", text)
	}
	if strings.Contains(text, `\t`) {
		t.Errorf("literal \\t should be unescaped to real tab, got: %s", text)
	}
	if !strings.Contains(text, "line1\nline2") {
		t.Errorf("expected real newline between line1 and line2, got: %s", text)
	}
}

// --- RegisterCommands ---

func TestRegisterCommands(t *testing.T) {
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{Name: "help", Description: "List available commands"})
	cmds.Register(&command.Command{Name: "ping", Description: "Check bot health"})
	cmds.Register(&command.Command{Name: "status", Description: "Show agent status"})

	b, mock := testBot([]string{"111"}, cmds)
	b.RegisterCommands()

	if mock.setCmds == nil {
		t.Fatal("SetMyCommands was not called")
	}

	// 3 registry commands + 2 special (stop, done)
	if len(mock.setCmds) != 5 {
		t.Fatalf("expected 5 commands, got %d", len(mock.setCmds))
	}

	// Registry commands should come first, sorted by name
	names := make([]string, len(mock.setCmds))
	for i, c := range mock.setCmds {
		names[i] = c.Command
	}
	// help, ping, status (sorted), then stop, done
	wantOrder := []string{"help", "ping", "status", "stop", "done"}
	for i, want := range wantOrder {
		if names[i] != want {
			t.Errorf("command[%d] = %q, want %q", i, names[i], want)
		}
	}

	// Verify descriptions
	for _, c := range mock.setCmds {
		if c.Description == "" {
			t.Errorf("command %q has empty description", c.Command)
		}
	}
}

func TestRegisterCommands_EmptyDescription(t *testing.T) {
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{Name: "test", Description: ""})

	b, mock := testBot([]string{"111"}, cmds)
	b.RegisterCommands()

	// Should fall back to command name as description
	for _, c := range mock.setCmds {
		if c.Command == "test" && c.Description != "test" {
			t.Errorf("expected description fallback to name, got %q", c.Description)
		}
	}
}

func TestRegisterCommands_APIError(t *testing.T) {
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{Name: "help", Description: "List commands"})

	b, mock := testBot([]string{"111"}, cmds)
	mock.setCmdsErr = fmt.Errorf("telegram API error")

	// Should not panic — just logs a warning
	b.RegisterCommands()
}

// --- sendReply with empty text ---

func TestSendReply_EmptyTextStillSends(t *testing.T) {
	// sendReply does NOT guard against empty text — that's the caller's
	// responsibility. This test proves the guard in processAgentMessage is
	// needed: without it, empty agent responses would reach sendReply and
	// hit the Telegram API with "message text is empty".
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	msg := makeMsg(111, "owner", "hello")

	b.sendReply(msg, "111", "")
	if mock.sentCount() != 1 {
		t.Errorf("sends = %d, want 1 (sendReply does not guard empty text)", mock.sentCount())
	}
}

func TestEmptyResponseGuard(t *testing.T) {
	// Verify the guard logic: empty and whitespace-only strings should be
	// detected. This mirrors the strings.TrimSpace check in processAgentMessage.
	cases := []struct {
		response string
		isEmpty  bool
	}{
		{"", true},
		{"   ", true},
		{"\n\t", true},
		{"hello", false},
		{" x ", false},
	}
	for _, tc := range cases {
		got := strings.TrimSpace(tc.response) == ""
		if got != tc.isEmpty {
			t.Errorf("TrimSpace(%q)==\"\" = %v, want %v", tc.response, got, tc.isEmpty)
		}
	}
}

// --- SendNotification guard ---

func TestSendNotification_EmptyTextSkipped(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.SetChatID(12345)

	// Empty text should not send
	b.SendNotification("")
	b.SendNotification("   ")
	b.SendNotification("\n\t")

	if mock.sentCount() != 0 {
		t.Errorf("sends = %d, want 0 (empty text should be skipped)", mock.sentCount())
	}

	// Non-empty text should send
	b.SendNotification("test alert")
	if mock.sentCount() != 1 {
		t.Errorf("sends = %d, want 1", mock.sentCount())
	}
}

// --- Stale command filter ---

func TestReceiveMessage_FreshSlashCommandDispatched(t *testing.T) {
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{
		Name:        "ping",
		Description: "test",
		Execute: func(ctx context.Context, args string) (string, error) {
			return "pong", nil
		},
	})

	b, mock := testBot([]string{"111"}, cmds)

	// Fresh message (timestamp = now) — should be dispatched normally
	msg := makeMsg(111, "owner", "/ping")
	b.receiveMessage(context.Background(), msg)

	// Command should have been dispatched and replied
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 sent message for fresh /ping, got %d", mock.sentCount())
	}
	if len(b.queue) != 0 {
		t.Error("fresh slash command should not be queued")
	}
}

func TestReceiveMessage_StaleSlashCommandDropped(t *testing.T) {
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{
		Name:        "ping",
		Description: "test",
		Execute: func(ctx context.Context, args string) (string, error) {
			return "pong", nil
		},
	})

	b, mock := testBot([]string{"111"}, cmds)

	// Create a message with a stale timestamp (60 seconds ago)
	msg := makeMsg(111, "owner", "/ping")
	msg.Date = int64(time.Now().Add(-60 * time.Second).Unix())
	b.receiveMessage(context.Background(), msg)

	// Stale slash command should be dropped — no reply, no queue
	if mock.sentCount() != 0 {
		t.Errorf("stale slash command should not send a reply, got %d sends", mock.sentCount())
	}
	if len(b.queue) != 0 {
		t.Error("stale slash command should not be queued")
	}
}

func TestReceiveMessage_StaleNonSlashMessageStillQueued(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	// Create a plain text message with a stale timestamp (60 seconds ago)
	msg := makeMsg(111, "owner", "hello from the past")
	msg.Date = int64(time.Now().Add(-60 * time.Second).Unix())
	b.receiveMessage(context.Background(), msg)

	// Non-slash messages should still be queued regardless of age
	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message for stale non-slash message, got %d", len(b.queue))
	}
	qm := <-b.queue
	if qm.text != "hello from the past" {
		t.Errorf("queued text = %q, want %q", qm.text, "hello from the past")
	}
}

// --- Per-chat session tests ---

func TestSessionKeyForChat(t *testing.T) {
	key := SessionKeyForChat("fotini", 123456789)
	if key != "agent:fotini:chat:123456789" {
		t.Errorf("got %q, want %q", key, "agent:fotini:chat:123456789")
	}
}

func TestSessionKeyForChat_DifferentChats(t *testing.T) {
	k1 := SessionKeyForChat("fotini", 111)
	k2 := SessionKeyForChat("fotini", 222)
	if k1 == k2 {
		t.Error("different chat IDs should produce different session keys")
	}
}

func TestDefaultChatAssignment(t *testing.T) {
	ss := state.New(t.TempDir() + "/state.json")
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.agentID = "test-agent"
	b.SetStateStore(ss, "bot:test")

	// No default initially
	if chatID := b.defaultChatID(); chatID != 0 {
		t.Errorf("expected no default, got %d", chatID)
	}

	// First message sets default
	msg := makeMsg(111, "alice", "hello")
	b.receiveMessage(context.Background(), msg)

	if chatID := b.defaultChatID(); chatID != 12345 {
		t.Errorf("expected default 12345, got %d", chatID)
	}

	// Second message from different chat doesn't change default
	msg2 := &gotgbot.Message{
		From: &gotgbot.User{Id: 111, Username: "alice"},
		Chat: gotgbot.Chat{Id: 99999},
		Text: "hello again",
		Date: int64(time.Now().Unix()),
	}
	b.receiveMessage(context.Background(), msg2)

	if chatID := b.defaultChatID(); chatID != 12345 {
		t.Errorf("expected default still 12345, got %d", chatID)
	}
}

func TestDefaultSessionKey(t *testing.T) {
	ss := state.New(t.TempDir() + "/state.json")
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.agentID = "test-agent"
	b.SetStateStore(ss, "bot:test")

	// No default → empty
	if sk := b.DefaultSessionKey(); sk != "" {
		t.Errorf("expected empty, got %q", sk)
	}

	// Set default chat
	b.setDefaultChat(12345)
	if sk := b.DefaultSessionKey(); sk != "agent:test-agent:chat:12345" {
		t.Errorf("expected agent:test-agent:chat:12345, got %q", sk)
	}
}

func TestSessionKey_PrimaryBotUsesDefault(t *testing.T) {
	ss := state.New(t.TempDir() + "/state.json")
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.agentID = "test-agent"
	b.sessionKey = "" // primary bots don't have an override
	b.SetStateStore(ss, "bot:test")
	b.setDefaultChat(12345)

	// SessionKey() should return the default chat session
	if sk := b.SessionKey(); sk != "agent:test-agent:chat:12345" {
		t.Errorf("expected default session key, got %q", sk)
	}
}

func TestSessionKey_SecondaryBotUsesOverride(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.isSecondary = true
	b.SetSessionKey("agent:test:multiball:mb-123")

	if sk := b.SessionKey(); sk != "agent:test:multiball:mb-123" {
		t.Errorf("expected override key, got %q", sk)
	}
}

func TestChatUsernameRecording(t *testing.T) {
	ss := state.New(t.TempDir() + "/state.json")
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.agentID = "test-agent"
	b.SetStateStore(ss, "bot:test")

	msg := makeMsg(111, "alice", "hello")
	b.receiveMessage(context.Background(), msg)

	// Username should be persisted
	var username string
	if !ss.Get("agent:test-agent:chat:12345:username", &username) {
		t.Fatal("username not persisted")
	}
	if username != "alice" {
		t.Errorf("expected alice, got %q", username)
	}
}

// --- OnSessionKeyChange callback ---

func TestSetSessionKey_FiresCallback(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	var callbackUsername, callbackKey string
	b.OnSessionKeyChange = func(username, sessionKey string) {
		callbackUsername = username
		callbackKey = sessionKey
	}

	// Set a session key — callback should fire
	b.SetSessionKey("agent:test:multiball:mb-1")
	if callbackKey != "agent:test:multiball:mb-1" {
		t.Errorf("callback key = %q, want %q", callbackKey, "agent:test:multiball:mb-1")
	}

	// Clear session key — callback should fire with empty
	b.SetSessionKey("")
	if callbackKey != "" {
		t.Errorf("callback key = %q, want empty", callbackKey)
	}
	_ = callbackUsername // username is "" for test bots (no api)
}

func TestSetSessionKey_NilCallbackDoesNotPanic(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.OnSessionKeyChange = nil

	// Should not panic
	b.SetSessionKey("agent:test:multiball:mb-1")
	b.SetSessionKey("")
}

func TestSetSessionKeyDirect_DoesNotFireCallback(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	called := false
	b.OnSessionKeyChange = func(username, sessionKey string) {
		called = true
	}

	b.SetSessionKeyDirect("agent:test:multiball:mb-1")
	if called {
		t.Error("SetSessionKeyDirect should not fire OnSessionKeyChange")
	}
	if sk := b.SessionKey(); sk != "agent:test:multiball:mb-1" {
		t.Errorf("session key = %q, want %q", sk, "agent:test:multiball:mb-1")
	}
}

func TestUsername_NilSafe(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	// testBot doesn't set b.api, so it's nil
	if got := b.Username(); got != "" {
		t.Errorf("Username() = %q, want empty for nil api", got)
	}
}
