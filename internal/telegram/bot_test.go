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

	"foci/internal/command"
	"foci/internal/state"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// mockClient implements botClient for testing.
type mockClient struct {
	mu             sync.Mutex
	sends          int                  // counts SendMessage calls
	edits          int                  // counts EditMessageText calls
	files          map[string]string    // fileId → filePath for GetFile mock
	setCmds        []gotgbot.BotCommand // last SetMyCommands call
	setCmdsErr     error                // error to return from SetMyCommands
	lastSendOpts   *gotgbot.SendMessageOpts  // last SendMessage opts
	lastSendInjected   string                    // last SendMessage text
	lastEditOpts   *gotgbot.EditMessageTextOpts // last EditMessageText opts
	lastEditText   string                    // last EditMessageText text
	answerCBCalls  int                       // counts AnswerCallbackQuery calls
	editErr        error                     // error to return from EditMessageText
	editErrOnce    bool                      // if true, only return editErr on first call
}

func (m *mockClient) SendMessage(chatId int64, text string, opts *gotgbot.SendMessageOpts) (*gotgbot.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sends++
	m.lastSendInjected = text
	m.lastSendOpts = opts
	return &gotgbot.Message{MessageId: int64(m.sends)}, nil
}

func (m *mockClient) EditMessageText(text string, opts *gotgbot.EditMessageTextOpts) (*gotgbot.Message, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.edits++
	m.lastEditText = text
	m.lastEditOpts = opts
	if m.editErr != nil {
		err := m.editErr
		if m.editErrOnce {
			m.editErr = nil
		}
		return nil, false, err
	}
	return &gotgbot.Message{}, true, nil
}

func (m *mockClient) SendDocument(chatId int64, document gotgbot.InputFileOrString, opts *gotgbot.SendDocumentOpts) (*gotgbot.Message, error) {
	return &gotgbot.Message{}, nil
}

func (m *mockClient) SendVoice(chatId int64, voice gotgbot.InputFileOrString, opts *gotgbot.SendVoiceOpts) (*gotgbot.Message, error) {
	return &gotgbot.Message{}, nil
}

func (m *mockClient) SendVideo(chatId int64, video gotgbot.InputFileOrString, opts *gotgbot.SendVideoOpts) (*gotgbot.Message, error) {
	return &gotgbot.Message{}, nil
}

func (m *mockClient) SendPhoto(chatId int64, photo gotgbot.InputFileOrString, opts *gotgbot.SendPhotoOpts) (*gotgbot.Message, error) {
	return &gotgbot.Message{}, nil
}

func (m *mockClient) SendAudio(chatId int64, audio gotgbot.InputFileOrString, opts *gotgbot.SendAudioOpts) (*gotgbot.Message, error) {
	return &gotgbot.Message{}, nil
}

func (m *mockClient) SendAnimation(chatId int64, animation gotgbot.InputFileOrString, opts *gotgbot.SendAnimationOpts) (*gotgbot.Message, error) {
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

func (m *mockClient) AnswerCallbackQuery(callbackQueryId string, opts *gotgbot.AnswerCallbackQueryOpts) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answerCBCalls++
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
		client:          mock,
		commands:        cmds,
		lastMsgStore:    command.NewLastMessageStore(),
		allowedUsers:    allowed,
		sessionKey:      "agent:test:main",
		queue:           make(chan queuedMessage, 64),
		chatSessionKeys: make(map[int64]string),
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

func TestReceiveMessage_UnknownSlashCommandGetsSuggestion(t *testing.T) {
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{
		Name: "ping",
		Execute: func(ctx context.Context, args string) (string, error) {
			return "pong", nil
		},
	})

	b, mock := testBot([]string{"111"}, cmds)

	msg := makeMsg(111, "owner", "/unknown_cmd")
	b.receiveMessage(context.Background(), msg)

	// Unknown commands should get a suggestion reply, not be queued
	if len(b.queue) != 0 {
		t.Fatalf("unknown slash command should not be queued, got %d queued", len(b.queue))
	}
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 suggestion reply, got %d", mock.sentCount())
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
	b, mock := testBot([]string{"111"}, command.NewRegistry())

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

	// Should get a suggestion reply (unknown command), not queued or treated as stop
	if len(b.queue) != 0 {
		t.Fatalf("expected 0 queued messages for unknown /wait, got %d", len(b.queue))
	}
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 suggestion reply for unknown /wait, got %d", mock.sentCount())
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

func TestSplitMessage_PreservesCodeBlock(t *testing.T) {
	// A <pre><code> block that exceeds maxLen — tags must be closed/reopened.
	inner := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n"
	text := "<pre><code>" + inner + "</code></pre>"
	chunks := splitMessage(text, 40)

	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		if !strings.HasPrefix(chunk, "<pre><code>") {
			t.Errorf("chunk %d missing opening tags: %q", i, chunk)
		}
		if !strings.HasSuffix(chunk, "</code></pre>") {
			t.Errorf("chunk %d missing closing tags: %q", i, chunk)
		}
		if len(chunk) > 40 {
			t.Errorf("chunk %d exceeds maxLen: len=%d", i, len(chunk))
		}
	}
}

func TestSplitMessage_PreservesPreBlock(t *testing.T) {
	// A <pre> block (table) that exceeds maxLen.
	inner := "row1\nrow2\nrow3\nrow4\nrow5\nrow6\n"
	text := "<pre>" + inner + "</pre>"
	chunks := splitMessage(text, 25)

	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		if !strings.HasPrefix(chunk, "<pre>") {
			t.Errorf("chunk %d missing <pre>: %q", i, chunk)
		}
		if !strings.HasSuffix(chunk, "</pre>") {
			t.Errorf("chunk %d missing </pre>: %q", i, chunk)
		}
	}
}

func TestSplitMessage_NoTagsUnchanged(t *testing.T) {
	// Plain text without HTML tags — same behavior as before.
	text := "line1\nline2\nline3"
	chunks := splitMessage(text, 10)
	var reconstructed string
	for _, c := range chunks {
		reconstructed += c
	}
	if reconstructed != text {
		t.Errorf("reconstruction mismatch: got %q, want %q", reconstructed, text)
	}
}

func TestSplitMessage_ClosedTagsBeforeSplit(t *testing.T) {
	// Tags are fully closed before the split point — no reopening needed.
	text := "<b>bold</b>\nplain text that is long enough to need splitting"
	chunks := splitMessage(text, 30)

	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	// First chunk has balanced tags; second chunk should be plain.
	if strings.Contains(chunks[1], "<b>") {
		t.Errorf("second chunk should not reopen <b>: %q", chunks[1])
	}
}

func TestSplitMessage_NestedTags(t *testing.T) {
	// Nested <b> inside <pre> — both should be closed/reopened.
	text := "<pre><b>" + strings.Repeat("x\n", 20) + "</b></pre>"
	chunks := splitMessage(text, 30)

	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	// First chunk should close in reverse order: </b></pre>
	if !strings.HasSuffix(chunks[0], "</b></pre>") {
		t.Errorf("first chunk should close nested tags: %q", chunks[0])
	}
	// Second chunk should reopen in original order: <pre><b>
	if !strings.HasPrefix(chunks[1], "<pre><b>") {
		t.Errorf("second chunk should reopen nested tags: %q", chunks[1])
	}
}

func TestOpenHTMLTags(t *testing.T) {
	cases := []struct {
		html string
		want []string
	}{
		{"hello", nil},
		{"<pre>text", []string{"<pre>"}},
		{"<pre><code>text", []string{"<pre>", "<code>"}},
		{"<pre><code>text</code></pre>", nil},
		{"<b>bold</b> <i>open", []string{"<i>"}},
		{`<a href="url">link`, []string{`<a href="url">`}},
		{"<pre><code>line1\nline2\n", []string{"<pre>", "<code>"}},
	}
	for _, tc := range cases {
		got := openHTMLTags(tc.html)
		if len(got) != len(tc.want) {
			t.Errorf("openHTMLTags(%q) = %v, want %v", tc.html, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("openHTMLTags(%q)[%d] = %q, want %q", tc.html, i, got[i], tc.want[i])
			}
		}
	}
}

func TestClosingHTMLTag(t *testing.T) {
	cases := []struct {
		open, want string
	}{
		{"<pre>", "</pre>"},
		{"<code>", "</code>"},
		{"<b>", "</b>"},
		{`<a href="url">`, "</a>"},
	}
	for _, tc := range cases {
		if got := closingHTMLTag(tc.open); got != tc.want {
			t.Errorf("closingHTMLTag(%q) = %q, want %q", tc.open, got, tc.want)
		}
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

	msg := makeMsgWithDocument(111, "owner", "application/zip")
	b.receiveMessage(context.Background(), msg)

	// Non-image, non-PDF document with no text should be dropped
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

	// No transcriber set — voice note should not be queued but should get an error reply
	msg := makeMsgWithVoice(111, "owner")
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 0 {
		t.Error("voice without transcriber should not be queued")
	}
	if mock.sentCount() != 1 {
		t.Fatalf("expected 1 error reply for voice without transcriber, got %d", mock.sentCount())
	}
	if !strings.Contains(mock.lastSendInjected, "Voice notes require") {
		t.Errorf("reply text = %q, want it to mention 'Voice notes require'", mock.lastSendInjected)
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

	// Should silently drop — no reply, no queue
	if mock.sentCount() != 0 {
		t.Fatalf("expected 0 sent messages (silent drop), got %d", mock.sentCount())
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
		{"application/pdf", ".pdf"},
		{"image/tiff", ".bin"},
		{"", ".bin"},
	}
	for _, tt := range tests {
		if got := extForMediaType(tt.mt); got != tt.want {
			t.Errorf("extForMediaType(%q) = %q, want %q", tt.mt, got, tt.want)
		}
	}
}

func TestIsPDFMIME(t *testing.T) {
	if !isPDFMIME("application/pdf") {
		t.Error("application/pdf should be PDF")
	}
	if isPDFMIME("image/jpeg") {
		t.Error("image/jpeg should not be PDF")
	}
	if isPDFMIME("application/json") {
		t.Error("application/json should not be PDF")
	}
	if isPDFMIME("") {
		t.Error("empty string should not be PDF")
	}
}

func TestSaveImage(t *testing.T) {
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.receivedFilesDir = dir

	data := []byte("fake-jpeg-data")
	path, err := b.saveAttachment(data, "image/jpeg", 12345)
	if err != nil {
		t.Fatalf("saveAttachment: %v", err)
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
	// receivedFilesDir not set — verify saveAttachment is only called when dir is set
	if b.receivedFilesDir != "" {
		t.Error("expected empty receivedFilesDir by default")
	}

	// Directly construct an attachment to verify no savedPath
	att := attachment{data: []byte("test"), mediaType: "image/jpeg"}
	if att.savedPath != "" {
		t.Error("expected empty savedPath when receivedFilesDir is not set")
	}
}

func TestSaveImagePNG(t *testing.T) {
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.receivedFilesDir = dir

	data := []byte("fake-png-data")
	path, err := b.saveAttachment(data, "image/png", 99999)
	if err != nil {
		t.Fatalf("saveAttachment: %v", err)
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
	b.receivedFilesDir = dir

	path, err := b.saveAttachment([]byte("data"), "image/jpeg", 1)
	if err != nil {
		t.Fatalf("saveAttachment: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("saved file not found: %v", err)
	}
}

func TestSavedPathPropagatedToQueue(t *testing.T) {
	// Test that attachment.savedPath flows through queuedMessage
	att := attachment{
		data:      []byte("test"),
		mediaType: "image/jpeg",
		savedPath: "/tmp/test.jpg",
	}
	qm := queuedMessage{
		text:   "look at this",
		images: []attachment{att},
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

// --- SendInjected ---

func TestSendInjected_SkipsEmptyMessage(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Set a chat ID so the bot can send
	b.SetChatID(12345)

	// Empty string should be silently skipped
	if err := b.SendInjected(""); err != nil {
		t.Errorf("SendInjected(\"\") error = %v, want nil", err)
	}
	if mock.sentCount() != 0 {
		t.Errorf("sentCount = %d, want 0 for empty string", mock.sentCount())
	}
}

func TestSendInjected_SkipsWhitespaceOnlyMessage(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Set a chat ID so the bot can send
	b.SetChatID(12345)

	// Whitespace-only should be silently skipped
	if err := b.SendInjected("   "); err != nil {
		t.Errorf("SendInjected(\"   \") error = %v, want nil", err)
	}
	if mock.sentCount() != 0 {
		t.Errorf("sentCount = %d, want 0 for whitespace-only", mock.sentCount())
	}
}

func TestSendInjected_SendsNonEmptyMessage(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Set a chat ID so the bot can send
	b.SetChatID(12345)

	// Non-empty message should be sent
	if err := b.SendInjected("hello"); err != nil {
		t.Errorf("SendInjected(\"hello\") error = %v, want nil", err)
	}
	if mock.sentCount() != 1 {
		t.Errorf("sentCount = %d, want 1 for non-empty message", mock.sentCount())
	}
}

// --- SendToSession ---

// TestSendToSession_ChatSession verifies that SendToSession extracts the chat ID
// from a chat-based session key and sends to that specific chat.
func TestSendToSession_ChatSession(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Session key with chat ID 67890
	err := b.SendToSession("main/c67890/1709590000", "hello from session")
	if err != nil {
		t.Fatalf("SendToSession error: %v", err)
	}
	if mock.sentCount() != 1 {
		t.Errorf("sentCount = %d, want 1", mock.sentCount())
	}
}

// TestSendToSession_IndependentSessionFallsBackToDefault verifies that SendToSession
// falls back to defaultChatID when the session key has no chat ID (independent session).
func TestSendToSession_IndependentSessionFallsBackToDefault(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Independent session has no chat ID — needs a default chat fallback.
	// Set up a state store with a default chat.
	dir := t.TempDir()
	store := state.New(filepath.Join(dir, "state.db"))
	b.agentID = "main"
	b.SetStateStore(store, "bot:main")
	b.setDefaultChat(11111)

	if err := b.SendToSession("main/i1709596800/1709596800", "hello independent"); err != nil {
		t.Fatalf("SendToSession error: %v", err)
	}
	if mock.sentCount() != 1 {
		t.Errorf("sentCount = %d, want 1", mock.sentCount())
	}
}

// TestSendToSession_NoChatIDNoDefaultErrors verifies that SendToSession returns
// an error when the session key has no chat ID and no default chat is configured.
func TestSendToSession_NoChatIDNoDefaultErrors(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	err := b.SendToSession("main/i1709596800/1709596800", "hello")
	if err == nil {
		t.Fatal("expected error when no chat ID and no default")
	}
}

// TestSendToSession_SkipsEmptyMessage verifies that empty messages are silently skipped.
func TestSendToSession_SkipsEmptyMessage(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	if err := b.SendToSession("main/c123/1709590000", ""); err != nil {
		t.Errorf("SendToSession with empty text should not error, got: %v", err)
	}
	if mock.sentCount() != 0 {
		t.Errorf("sentCount = %d, want 0", mock.sentCount())
	}
}

// TestSendToSession_BranchKeyUsesParentChat verifies that branch session keys
// still resolve to the parent chat ID (root type 'c' is preserved in branches).
func TestSendToSession_BranchKeyUsesParentChat(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	err := b.SendToSession("main/c67890/1709590000/b1709596800", "branch message")
	if err != nil {
		t.Fatalf("SendToSession error: %v", err)
	}
	if mock.sentCount() != 1 {
		t.Errorf("sentCount = %d, want 1", mock.sentCount())
	}
}

// --- Async notifier delivery ---
// These tests verify the contract that async-notifier turns (tmux watch,
// exec auto-background) deliver responses via SendToSession, matching the
// wiring in main.go's notifier closure.

func TestAsyncNotifierDeliveryViaSendInjected(t *testing.T) {
	// Simulates the async notifier delivery path in main.go:
	// notifier calls HandleMessage → gets response → calls bot.SendInjected()
	mgr := NewBotManager()
	bot, mock := testBot([]string{"111"}, command.NewRegistry())
	bot.SetChatID(12345)
	mgr.AddPrimary("test-agent", bot)

	// Simulate: notifier got a response from HandleMessage
	resp := "Four undeployed commits now. Both queues empty."

	// Deliver via primary bot's SendInjected (same as main.go closure)
	primary := mgr.PrimaryBot("test-agent")
	if primary == nil {
		t.Fatal("PrimaryBot returned nil")
	}
	if err := primary.SendInjected(resp); err != nil {
		t.Fatalf("SendInjected error: %v", err)
	}
	if mock.sentCount() != 1 {
		t.Errorf("sentCount = %d, want 1", mock.sentCount())
	}
}

func TestAsyncNotifierSkipsEmptyResponse(t *testing.T) {
	// When HandleMessage returns empty string, notifier should not call SendInjected.
	// This is checked in the main.go closure before calling SendInjected.
	bot, mock := testBot([]string{"111"}, command.NewRegistry())
	bot.SetChatID(12345)

	// Empty response should be silently skipped by SendInjected
	if err := bot.SendInjected(""); err != nil {
		t.Fatalf("SendInjected(\"\") error: %v", err)
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
	toolCallObserver("shell", json.RawMessage(`{"command":"ls"}`))
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

func TestShowToolCalls_Preview(t *testing.T) {
	// When showToolCalls is "preview", tool call observer should send messages.
	mock := &mockClient{}
	b := &Bot{client: mock, showToolCalls: "preview"}

	var toolMsgID int64
	var toolMsgMu sync.Mutex

	observer := func(toolName string, params json.RawMessage) {
		if b.showToolCalls == "off" || b.showToolCalls == "" {
			return
		}
		toolMsgMu.Lock()
		defer toolMsgMu.Unlock()
		text := b.formatToolCall(toolName, params)
		if toolMsgID == 0 {
			sent, _ := b.client.SendMessage(12345, text, &gotgbot.SendMessageOpts{ParseMode: "HTML"})
			toolMsgID = sent.MessageId
		} else {
			b.client.EditMessageText(text, &gotgbot.EditMessageTextOpts{
				ChatId: 12345, MessageId: toolMsgID, ParseMode: "HTML",
			})
		}
	}

	observer("shell", json.RawMessage(`{"command":"ls"}`))
	if mock.sentCount() != 1 {
		t.Errorf("sends=%d, want 1", mock.sentCount())
	}

	observer("read", json.RawMessage(`{"path":"foo.txt"}`))
	if mock.editCount() != 1 {
		t.Errorf("edits=%d, want 1", mock.editCount())
	}
}

func TestShowToolCalls_Off(t *testing.T) {
	// When showToolCalls is "off", tool call observer should be a no-op.
	mock := &mockClient{}
	b := &Bot{client: mock, showToolCalls: "off"}

	var toolMsgID int64
	var toolMsgMu sync.Mutex

	observer := func(toolName string, params json.RawMessage) {
		if b.showToolCalls == "off" || b.showToolCalls == "" {
			return
		}
		toolMsgMu.Lock()
		defer toolMsgMu.Unlock()
		text := b.formatToolCall(toolName, params)
		if toolMsgID == 0 {
			sent, _ := b.client.SendMessage(12345, text, &gotgbot.SendMessageOpts{ParseMode: "HTML"})
			toolMsgID = sent.MessageId
		} else {
			b.client.EditMessageText(text, &gotgbot.EditMessageTextOpts{
				ChatId: 12345, MessageId: toolMsgID, ParseMode: "HTML",
			})
		}
	}

	observer("shell", json.RawMessage(`{"command":"ls"}`))
	observer("read", json.RawMessage(`{"path":"foo.txt"}`))

	if mock.sentCount() != 0 {
		t.Errorf("sends=%d, want 0 (tool calls should be suppressed)", mock.sentCount())
	}
	if mock.editCount() != 0 {
		t.Errorf("edits=%d, want 0 (tool calls should be suppressed)", mock.editCount())
	}
}

func TestShowToolCalls_Full(t *testing.T) {
	// When showToolCalls is "full", every tool call gets its own persistent
	// message with compact summary. The final response goes via sendReply.
	mock := &mockClient{}
	b := &Bot{client: mock, showToolCalls: "full"}

	var toolMsgID int64
	var toolMsgMu sync.Mutex

	// This mirrors the ToolCallObserver closure in processMessage.
	observer := func(toolName string, params json.RawMessage) {
		if b.showToolCalls == "off" || b.showToolCalls == "" {
			return
		}
		toolMsgMu.Lock()
		defer toolMsgMu.Unlock()

		if b.showToolCalls == "full" {
			compact := formatToolCallCompact(toolName, params)
			sent, _ := b.client.SendMessage(12345, compact, &gotgbot.SendMessageOpts{ParseMode: "HTML"})
			toolMsgID = sent.MessageId
			return
		}

		text := b.formatToolCall(toolName, params)
		if toolMsgID == 0 {
			sent, _ := b.client.SendMessage(12345, text, &gotgbot.SendMessageOpts{ParseMode: "HTML"})
			toolMsgID = sent.MessageId
		} else {
			b.client.EditMessageText(text, &gotgbot.EditMessageTextOpts{
				ChatId: 12345, MessageId: toolMsgID, ParseMode: "HTML",
			})
		}
	}

	// First tool call: new message with compact summary.
	observer("shell", json.RawMessage(`{"command":"ls"}`))
	if mock.sentCount() != 1 {
		t.Errorf("sends=%d, want 1", mock.sentCount())
	}
	if !strings.Contains(mock.lastSendInjected, "shell") {
		t.Errorf("sent text should contain tool name, got: %s", mock.lastSendInjected)
	}

	// Second tool call: also a new message (not an edit).
	observer("read", json.RawMessage(`{"path":"foo.txt"}`))
	if mock.sentCount() != 2 {
		t.Errorf("sends=%d, want 2 (full mode sends each tool call as new message)", mock.sentCount())
	}
	if mock.editCount() != 0 {
		t.Errorf("edits=%d, want 0 (full mode should never edit previous tool call)", mock.editCount())
	}

	// Simulate response delivery: in "full" mode, response should NOT edit the tool message.
	toolMsgMu.Lock()
	editID := toolMsgID
	toolMsgMu.Unlock()
	if editID != 0 && b.showToolCalls == "preview" {
		t.Error("should not enter preview branch for full mode")
	}
}

// --- Tool call visibility ---

func TestFormatToolCall(t *testing.T) {
	b := &Bot{}
	text := b.formatToolCall("shell", json.RawMessage(`{"command":"ls -la"}`))
	if !strings.Contains(text, "▶️") {
		t.Error("missing tool emoji")
	}
	if !strings.Contains(text, "<b>shell</b>") {
		t.Errorf("missing tool name in %q", text)
	}
	if !strings.Contains(text, "ls -la") {
		t.Errorf("missing params in %q", text)
	}
}

func TestFormatToolCall_HTMLEscape(t *testing.T) {
	b := &Bot{}
	text := b.formatToolCall("shell", json.RawMessage(`{"command":"echo <script>"}`))
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
	text := b.formatToolCall("shell", json.RawMessage(fmt.Sprintf(`{"command":"%s"}`, longVal)))
	// Long params should be truncated and contain "..."
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

func TestFormatToolCall_UnescapesUnicodeSequences(t *testing.T) {
	b := &Bot{}
	// Simulate a tool call where the JSON contains unicode escape sequences
	// This happens when the API returns escaped unicode sequences like \u003e for >
	params := json.RawMessage(`{"command":"echo \u003e test \u0026\u0026 more"}`)
	text := b.formatToolCall("shell", params)

	// After unescaping unicode sequences, the characters should be displayed properly
	// (not as literal \u003e escape sequences). They will be HTML-escaped for Telegram safety.
	if strings.Contains(text, `\u003e`) {
		t.Errorf("unicode escape \\u003e should be unescaped (not literal), got: %q", text)
	}
	if strings.Contains(text, `\u0026`) {
		t.Errorf("unicode escape \\u0026 should be unescaped (not literal), got: %q", text)
	}
	// The unescaped characters will be HTML-escaped for Telegram safety
	if !strings.Contains(text, "&gt;") {
		t.Errorf("expected > to be unescaped then HTML-escaped to &gt;, got: %q", text)
	}
	if !strings.Contains(text, "&amp;&amp;") {
		t.Errorf("expected && to be unescaped then HTML-escaped to &amp;&amp;, got: %q", text)
	}
}

// --- Compact tool call formatting ---

func TestFormatToolCallCompact(t *testing.T) {
	tests := []struct {
		name     string
		tool     string
		params   string
		contains string // expected substring in output
		emoji    string // expected per-tool emoji
	}{
		{"shell", "shell", `{"command":"ls -la /tmp"}`, "ls -la /tmp", "▶️"},
		{"web_search", "web_search", `{"query":"golang generics"}`, "golang generics", "🔍"},
		{"web_fetch", "web_fetch", `{"url":"https://example.com/page"}`, "https://example.com/page", "🔗"},
		{"http_request GET", "http_request", `{"url":"https://api.example.com/v1"}`, "GET https://api.example.com/v1", "🌍"},
		{"http_request POST", "http_request", `{"method":"POST","url":"https://api.example.com/v1"}`, "POST https://api.example.com/v1", "🌍"},
		{"read", "read", `{"path":"/home/user/file.txt"}`, "/home/user/file.txt", "📖"},
		{"tmux watch", "tmux", `{"operation":"watch","name":"cc-bash","threshold_seconds":30}`, "watch cc-bash", "🪟"},
		{"todo add", "todo", `{"action":"add","text":"buy milk"}`, "add", "☑️"},
		{"send_telegram", "send_telegram", `{"text":"hello world, how are you doing today?"}`, "hello world", "📨"},
		{"spawn", "spawn", `{"prompt":"summarize this document please"}`, "summarize this document", "🐣"},
		{"memory_search", "memory_search", `{"query":"project setup"}`, "project setup", "🧠"},
		{"unknown tool", "custom_tool", `{"foo":"bar value"}`, "bar value", "🔧"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatToolCallCompact(tt.tool, json.RawMessage(tt.params))
			if !strings.Contains(result, tt.emoji) {
				t.Errorf("expected emoji %s in %q", tt.emoji, result)
			}
			if !strings.Contains(result, tt.tool) {
				t.Errorf("missing tool name in %q", result)
			}
			if !strings.Contains(result, tt.contains) {
				t.Errorf("expected %q in %q", tt.contains, result)
			}
			// Should NOT contain <pre> block (that's the full format)
			if strings.Contains(result, "<pre>") {
				t.Errorf("compact format should not contain <pre>, got: %s", result)
			}
		})
	}
}

func TestFormatToolCallCompact_HTMLEscape(t *testing.T) {
	result := formatToolCallCompact("shell", json.RawMessage(`{"command":"echo <script>"}`))
	if strings.Contains(result, "<script>") {
		t.Errorf("HTML not escaped in %q", result)
	}
	if !strings.Contains(result, "&lt;script&gt;") {
		t.Errorf("expected escaped HTML in %q", result)
	}
}

func TestFormatToolCallCompact_Truncation(t *testing.T) {
	longCmd := strings.Repeat("x", 200)
	result := formatToolCallCompact("shell", json.RawMessage(fmt.Sprintf(`{"command":"%s"}`, longCmd)))
	// Should be truncated to ~60 chars + "..."
	if !strings.Contains(result, "...") {
		t.Errorf("long command should be truncated: %s", result)
	}
}

func TestFormatToolCallCompact_EmptyParams(t *testing.T) {
	result := formatToolCallCompact("unknown", json.RawMessage(`{}`))
	// Should just be the tool name with no summary; unknown tool gets 🔧
	if !strings.Contains(result, "🔧") {
		t.Error("missing fallback tool emoji")
	}
	if strings.Contains(result, ":") {
		t.Errorf("empty params should not have colon separator, got: %s", result)
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

func TestSendReply_SkipsEmptyText(t *testing.T) {
	// sendReply trims parts and skips empty text — callers don't need to guard.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	msg := makeMsg(111, "owner", "hello")

	b.sendReply(msg, "111", "")
	if mock.sentCount() != 0 {
		t.Errorf("sends = %d, want 0 (sendReply skips empty text)", mock.sentCount())
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

func TestNewSessionKeyForChat(t *testing.T) {
	key := NewSessionKeyForChat("fotini", 123456789)
	if !strings.HasPrefix(key, "fotini/c123456789/") {
		t.Errorf("got %q, want prefix %q", key, "fotini/c123456789/")
	}
}

func TestNewSessionKeyForChat_DifferentChats(t *testing.T) {
	k1 := NewSessionKeyForChat("fotini", 111)
	k2 := NewSessionKeyForChat("fotini", 222)
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
	if sk := b.DefaultSessionKey(); !strings.HasPrefix(sk, "test-agent/c12345/") {
		t.Errorf("expected prefix test-agent/c12345/, got %q", sk)
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
	if sk := b.SessionKey(); !strings.HasPrefix(sk, "test-agent/c12345/") {
		t.Errorf("expected prefix test-agent/c12345/, got %q", sk)
	}
}

func TestSessionKey_PrimaryBotIsStable(t *testing.T) {
	ss := state.New(t.TempDir() + "/state.json")
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.agentID = "test-agent"
	b.sessionKey = ""
	b.SetStateStore(ss, "bot:test")
	b.setDefaultChat(12345)

	k1 := b.SessionKey()
	k2 := b.SessionKey()
	if k1 != k2 {
		t.Errorf("SessionKey() not stable: %q vs %q", k1, k2)
	}
}

func TestDefaultSessionKey_IsStable(t *testing.T) {
	ss := state.New(t.TempDir() + "/state.json")
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.agentID = "test-agent"
	b.SetStateStore(ss, "bot:test")
	b.setDefaultChat(12345)

	k1 := b.DefaultSessionKey()
	k2 := b.DefaultSessionKey()
	if k1 != k2 {
		t.Errorf("DefaultSessionKey() not stable: %q vs %q", k1, k2)
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

func TestHtmlTagName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"pre>", "pre"},
		{"a href=\"url\">", "a"},
		{"div/", "div"},
		{"b", "b"},
		{"code>text", "code"},
	}
	for _, tt := range tests {
		got := htmlTagName(tt.in)
		if got != tt.want {
			t.Errorf("htmlTagName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestUnescapeJSONStringLiterals(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{`hello\nworld`, "hello\nworld"},
		{`col1\tcol2`, "col1\tcol2"},
		{`a\nb\tc`, "a\nb\tc"},
		{"no escapes", "no escapes"},
		{"", ""},
	}
	for _, tt := range tests {
		got := unescapeJSONStringLiterals(tt.in)
		if got != tt.want {
			t.Errorf("unescapeJSONStringLiterals(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestHtmlEscapeBot(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"a & b", "a &amp; b"},
		{"<tag>", "&lt;tag&gt;"},
		{"safe text", "safe text"},
		{"a & <b> end", "a &amp; &lt;b&gt; end"},
		{"", ""},
	}
	for _, tt := range tests {
		got := htmlEscapeBot(tt.in)
		if got != tt.want {
			t.Errorf("htmlEscapeBot(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 5, "hello..."},
		{"", 5, ""},
	}
	for _, tt := range tests {
		got := truncate(tt.in, tt.max)
		if got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
		}
	}
}

func TestSanitizeError(t *testing.T) {
	b := &Bot{botToken: "secret123"}

	// nil error
	if got := b.sanitizeError(nil); got != "" {
		t.Errorf("sanitizeError(nil) = %q, want empty", got)
	}

	// error without token
	if got := b.sanitizeError(fmt.Errorf("timeout")); got != "timeout" {
		t.Errorf("sanitizeError = %q, want 'timeout'", got)
	}

	// error with token
	if got := b.sanitizeError(fmt.Errorf("request to secret123/method failed")); !strings.Contains(got, "[REDACTED]") {
		t.Errorf("sanitizeError should redact token, got %q", got)
	}
	if strings.Contains(b.sanitizeError(fmt.Errorf("request to secret123/method failed")), "secret123") {
		t.Error("token should be redacted")
	}

	// empty token
	b2 := &Bot{botToken: ""}
	if got := b2.sanitizeError(fmt.Errorf("some error")); got != "some error" {
		t.Errorf("sanitizeError with empty token = %q", got)
	}
}

func TestFindSplitPoint(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		maxLen int
		want   int
	}{
		{"shorter than max", "hello", 100, 5},
		{"exact length", "hello", 5, 5},
		{"newline boundary", "hello\nworld\nfoo", 12, 12}, // split at second \n + 1
		{"no newline", "abcdefghij", 5, 5},
		{"inside HTML tag", "abc<b>def", 5, 3}, // split before '<'
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findSplitPoint(tt.text, tt.maxLen)
			if got != tt.want {
				t.Errorf("findSplitPoint(%q, %d) = %d, want %d", tt.text, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestSplitChunk(t *testing.T) {
	// Simple split — no open tags
	chunk, rest := splitChunk("hello world this is long", 11)
	if chunk != "hello world" || rest != " this is long" {
		t.Errorf("simple split: chunk=%q, rest=%q", chunk, rest)
	}

	// Split with open HTML tag — should close and reopen
	chunk, rest = splitChunk("<b>hello world</b>", 10)
	if !strings.HasSuffix(chunk, "</b>") {
		t.Errorf("chunk should end with </b>, got %q", chunk)
	}
	if !strings.HasPrefix(rest, "<b>") {
		t.Errorf("rest should start with <b>, got %q", rest)
	}
}

// --- Video support ---

func makeMsgWithVideo(userID int64, username, caption string, fileSize int64) *gotgbot.Message {
	return &gotgbot.Message{
		From:    &gotgbot.User{Id: userID, Username: username},
		Chat:    gotgbot.Chat{Id: 12345},
		Caption: caption,
		Date:    int64(time.Now().Unix()),
		Video: &gotgbot.Video{
			FileId:   "video_id",
			Width:    1920,
			Height:   1080,
			Duration: 30,
			MimeType: "video/mp4",
			FileSize: fileSize,
		},
	}
}

func makeMsgWithVideoNote(userID int64, username string, fileSize int64) *gotgbot.Message {
	return &gotgbot.Message{
		From: &gotgbot.User{Id: userID, Username: username},
		Chat: gotgbot.Chat{Id: 12345},
		Date: int64(time.Now().Unix()),
		VideoNote: &gotgbot.VideoNote{
			FileId:   "videonote_id",
			Length:   240,
			Duration: 15,
			FileSize: fileSize,
		},
	}
}

func makeMsgWithNonImageDocument(userID int64, username, filename, mime string, fileSize int64) *gotgbot.Message {
	return &gotgbot.Message{
		From: &gotgbot.User{Id: userID, Username: username},
		Chat: gotgbot.Chat{Id: 12345},
		Date: int64(time.Now().Unix()),
		Document: &gotgbot.Document{
			FileId:   "doc_id",
			FileName: filename,
			MimeType: mime,
			FileSize: fileSize,
		},
	}
}

func TestReceiveMessage_VideoQueuedWithSavedPath(t *testing.T) {
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.receivedFilesDir = dir
	b.botToken = "test-token"

	// Small video (under 20MB)
	msg := makeMsgWithVideo(111, "owner", "Check this out!", 5*1024*1024)
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.queue))
	}
	qm := <-b.queue
	// Note: Download will fail without a real server, so we can't test the saved path
	// Just verify the caption is preserved
	if !strings.Contains(qm.text, "Check this out!") {
		t.Errorf("text should contain original caption, got: %q", qm.text)
	}
}

func TestReceiveMessage_VideoTooLarge(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.receivedFilesDir = t.TempDir()
	b.botToken = "test-token"

	// Large video (over 20MB)
	msg := makeMsgWithVideo(111, "owner", "Check this out!", 25*1024*1024)
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.queue))
	}
	qm := <-b.queue
	if !strings.Contains(qm.text, "[Video too large to download (25 MB)]") {
		t.Errorf("text should indicate video too large, got: %q", qm.text)
	}
}

func TestReceiveMessage_VideoWithoutCaption(t *testing.T) {
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.receivedFilesDir = dir
	b.botToken = "test-token"

	// Video with no caption - when download fails, message is dropped (no text, no images)
	msg := makeMsgWithVideo(111, "owner", "", 5*1024*1024)
	b.receiveMessage(context.Background(), msg)

	// Without a real server, download fails, so message with no caption is dropped
	if len(b.queue) != 0 {
		t.Errorf("expected 0 queued messages when video download fails with no caption, got %d", len(b.queue))
	}
}

func TestReceiveMessage_VideoNoteQueuedWithSavedPath(t *testing.T) {
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.receivedFilesDir = dir
	b.botToken = "test-token"

	// Video note with text caption
	msg := makeMsgWithVideoNote(111, "owner", 5*1024*1024)
	msg.Caption = "Quick video note"
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.queue))
	}
	qm := <-b.queue
	// Download will fail without a real server, just check caption preserved
	if !strings.Contains(qm.text, "Quick video note") {
		t.Errorf("text should contain caption, got: %q", qm.text)
	}
}

func TestReceiveMessage_VideoNoteTooLarge(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.receivedFilesDir = t.TempDir()
	b.botToken = "test-token"

	msg := makeMsgWithVideoNote(111, "owner", 25*1024*1024)
	b.receiveMessage(context.Background(), msg)

	// Video note too large - message is still queued with size warning
	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.queue))
	}
	qm := <-b.queue
	if !strings.Contains(qm.text, "[Video too large to download (25 MB)]") {
		t.Errorf("text should indicate video too large, got: %q", qm.text)
	}
}

func TestReceiveMessage_NonImageDocumentSaved(t *testing.T) {
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.receivedFilesDir = dir
	b.botToken = "test-token"

	// Document with caption
	msg := makeMsgWithNonImageDocument(111, "owner", "report.pdf", "application/pdf", 100*1024)
	msg.Caption = "Here's the report"
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.queue))
	}
	qm := <-b.queue
	// Download will fail without a real server, just check caption preserved
	if !strings.Contains(qm.text, "Here's the report") {
		t.Errorf("text should contain caption, got: %q", qm.text)
	}
}

func TestReceiveMessage_NonImageDocumentTooLarge(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.receivedFilesDir = t.TempDir()
	b.botToken = "test-token"

	// Document with caption
	msg := makeMsgWithNonImageDocument(111, "owner", "large.zip", "application/zip", 25*1024*1024)
	msg.Caption = "Big file"
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.queue))
	}
	qm := <-b.queue
	if !strings.Contains(qm.text, "[Document too large to download (25 MB)]") {
		t.Errorf("text should indicate document too large, got: %q", qm.text)
	}
}

func TestReceiveMessage_VideoNoSaveDir(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	// receivedFilesDir not set
	b.botToken = "test-token"

	msg := makeMsgWithVideo(111, "owner", "Check this out!", 5*1024*1024)
	b.receiveMessage(context.Background(), msg)

	// Should still queue the message, but without the saved path
	if len(b.queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.queue))
	}
	qm := <-b.queue
	// Text should be the caption since we couldn't save
	if qm.text != "Check this out!" {
		t.Errorf("text should be original caption, got: %q", qm.text)
	}
}

// --- Helper function tests ---

func TestExtForVideo(t *testing.T) {
	tests := []struct {
		mime string
		want string
	}{
		{"video/mp4", ".mp4"},
		{"video/quicktime", ".mov"},
		{"video/webm", ".webm"},
		{"video/x-matroska", ".mkv"},
		{"video/avi", ".avi"},
		{"video/x-msvideo", ".avi"},
		{"video/unknown", ".mp4"},
		{"", ".mp4"},
	}
	for _, tt := range tests {
		if got := extForVideo(tt.mime); got != tt.want {
			t.Errorf("extForVideo(%q) = %q, want %q", tt.mime, got, tt.want)
		}
	}
}

func TestExtForMIME(t *testing.T) {
	tests := []struct {
		mime string
		want string
	}{
		{"video/mp4", ".mp4"},
		{"application/pdf", ".pdf"},
		{"application/json", ".json"},
		{"text/plain", ".txt"},
		{"text/csv", ".csv"},
		{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", ".xlsx"},
		{"application/vnd.ms-excel", ".xls"},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", ".docx"},
		{"application/msword", ".doc"},
		{"audio/mpeg", ".mp3"},
		{"audio/ogg", ".mp3"},
		{"application/octet-stream", ".bin"},
		{"", ".bin"},
	}
	for _, tt := range tests {
		if got := extForMIME(tt.mime); got != tt.want {
			t.Errorf("extForMIME(%q) = %q, want %q", tt.mime, got, tt.want)
		}
	}
}

func TestIsFileTooLarge(t *testing.T) {
	err := &fileTooLargeError{size: 25 * 1024 * 1024}
	if !isFileTooLarge(err) {
		t.Error("isFileTooLarge should return true for fileTooLargeError")
	}
	if isFileTooLarge(fmt.Errorf("other error")) {
		t.Error("isFileTooLarge should return false for other errors")
	}
}

func TestFileTooLargeError(t *testing.T) {
	size := int64(25 * 1024 * 1024)
	err := &fileTooLargeError{size: size}
	if !strings.Contains(err.Error(), "26214400") {
		t.Errorf("error message should contain size in bytes, got: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "20MB") {
		t.Errorf("error message should mention 20MB limit, got: %s", err.Error())
	}
}

func TestSaveMedia(t *testing.T) {
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.receivedFilesDir = dir

	data := []byte("fake-mp4-data")
	path, err := b.saveMedia(data, "video", 12345, ".mp4")
	if err != nil {
		t.Fatalf("saveMedia: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("saved content mismatch")
	}

	base := filepath.Base(path)
	if !strings.Contains(base, "_video_") {
		t.Errorf("filename should contain _video_, got: %s", base)
	}
	if !strings.HasSuffix(base, ".mp4") {
		t.Errorf("filename should end with .mp4, got: %s", base)
	}
}

func TestSaveMediaDocument(t *testing.T) {
	dir := t.TempDir()
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.receivedFilesDir = dir

	data := []byte("fake-pdf-data")
	path, err := b.saveMedia(data, "document", 99999, ".pdf")
	if err != nil {
		t.Fatalf("saveMedia: %v", err)
	}

	if !strings.HasSuffix(path, ".pdf") {
		t.Errorf("path should end with .pdf, got: %s", path)
	}

	base := filepath.Base(path)
	if !strings.Contains(base, "_document_") {
		t.Errorf("filename should contain _document_, got: %s", base)
	}
}

func TestDownloadAndSaveMedia_NoSaveDir(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	// receivedFilesDir not set

	_, err := b.downloadAndSaveMedia("file_id", 1024, "video", 12345, ".mp4")
	if err == nil {
		t.Error("expected error when receivedFilesDir not set")
	}
}

func TestDownloadAndSaveMedia_TooLarge(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.receivedFilesDir = t.TempDir()

	_, err := b.downloadAndSaveMedia("file_id", 25*1024*1024, "video", 12345, ".mp4")
	if err == nil {
		t.Error("expected error for file too large")
	}
	if !isFileTooLarge(err) {
		t.Errorf("expected fileTooLargeError, got: %v", err)
	}
}

// --- Inline keyboard for tool results (full mode) ---

func TestToolCallFull_InlineKeyboard(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.showToolCalls = "full"

	// Simulate the ToolCallObserver closure from processAgentMessage.
	var toolMsgID int64
	var toolMsgMu sync.Mutex
	chatID := int64(12345)

	observer := func(toolName string, params json.RawMessage) {
		toolMsgMu.Lock()
		defer toolMsgMu.Unlock()

		compact := formatToolCallCompact(toolName, params)
		sendOpts := &gotgbot.SendMessageOpts{
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
					{Text: "Show full", CallbackData: "tc:show:0"},
				}},
			},
		}
		sent, err := b.client.SendMessage(chatID, compact, sendOpts)
		if err != nil {
			return
		}
		toolMsgID = sent.MessageId
		// Update callback data with real message ID.
		b.client.EditMessageText(compact, &gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: toolMsgID,
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
					{Text: "Show full", CallbackData: fmt.Sprintf("tc:show:%d", toolMsgID)},
				}},
			},
		})
	}

	observer("shell", json.RawMessage(`{"command":"ls"}`))

	mock.mu.Lock()
	defer mock.mu.Unlock()

	if mock.sends != 1 {
		t.Fatalf("expected 1 send, got %d", mock.sends)
	}
	if mock.lastSendOpts == nil {
		t.Fatal("expected SendMessageOpts to be set")
	}
	// After send, an edit should update the callback data with the real message ID
	if mock.edits != 1 {
		t.Fatalf("expected 1 edit (to update callback data), got %d", mock.edits)
	}
	if mock.lastEditOpts == nil {
		t.Fatal("expected EditMessageTextOpts to be set")
	}
	kb := mock.lastEditOpts.ReplyMarkup
	if len(kb.InlineKeyboard) != 1 || len(kb.InlineKeyboard[0]) != 1 {
		t.Fatal("expected 1x1 inline keyboard")
	}
	btn := kb.InlineKeyboard[0][0]
	if btn.Text != "Show full" {
		t.Errorf("button text = %q, want %q", btn.Text, "Show full")
	}
	if btn.CallbackData != "tc:show:1" {
		t.Errorf("callback data = %q, want %q", btn.CallbackData, "tc:show:1")
	}
	// Sent text should be compact (no <pre> block)
	if strings.Contains(mock.lastSendInjected, "<pre>") {
		t.Errorf("compact summary should not contain <pre> block, got: %s", mock.lastSendInjected)
	}
}

func TestHandleCallbackQuery_Show(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Pre-store a tool result with compact text, full input, and result.
	var msgID int64 = 42
	compactText := `▶️ <b>shell</b>: ls`
	fullInput := "▶️ <b>shell</b>\n<pre>ls</pre>"
	b.toolResults.Store(msgID, toolResultEntry{
		compactText: compactText,
		fullInput:   fullInput,
		result:      "file1.txt\nfile2.txt",
	})

	cq := &gotgbot.CallbackQuery{
		Id:   "cq1",
		From: gotgbot.User{Id: 111},
		Message: gotgbot.Message{
			MessageId: msgID,
			Chat:      gotgbot.Chat{Id: 12345},
		},
		Data: fmt.Sprintf("tc:show:%d", msgID),
	}
	b.handleCallbackQuery(context.Background(), cq)

	mock.mu.Lock()
	defer mock.mu.Unlock()

	if mock.edits != 1 {
		t.Fatalf("expected 1 edit, got %d", mock.edits)
	}
	if mock.answerCBCalls != 1 {
		t.Fatalf("expected 1 AnswerCallbackQuery call, got %d", mock.answerCBCalls)
	}
	// The edited text should contain the full input and result
	if !strings.Contains(mock.lastEditText, "Result:") {
		t.Errorf("expected edit text to contain result, got: %s", mock.lastEditText)
	}
	if !strings.Contains(mock.lastEditText, "shell") {
		t.Errorf("expected edit text to contain full tool input, got: %s", mock.lastEditText)
	}
	// Button should now be "Hide"
	if mock.lastEditOpts == nil {
		t.Fatal("expected edit opts to be set")
	}
	kb := mock.lastEditOpts.ReplyMarkup
	if len(kb.InlineKeyboard) != 1 || len(kb.InlineKeyboard[0]) != 1 {
		t.Fatal("expected 1x1 inline keyboard")
	}
	btn := kb.InlineKeyboard[0][0]
	if btn.Text != "Hide" {
		t.Errorf("button text = %q, want %q", btn.Text, "Hide")
	}
	if !strings.HasPrefix(btn.CallbackData, "tc:hide:") {
		t.Errorf("callback data = %q, want tc:hide: prefix", btn.CallbackData)
	}
}

func TestHandleCallbackQuery_Hide(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	var msgID int64 = 42
	compactText := `▶️ <b>exec</b>: ls`
	fullInput := "▶️ <b>exec</b>\n<pre>ls</pre>"
	b.toolResults.Store(msgID, toolResultEntry{
		compactText: compactText,
		fullInput:   fullInput,
		result:      "file1.txt\nfile2.txt",
	})

	cq := &gotgbot.CallbackQuery{
		Id:   "cq2",
		From: gotgbot.User{Id: 111},
		Message: gotgbot.Message{
			MessageId: msgID,
			Chat:      gotgbot.Chat{Id: 12345},
		},
		Data: fmt.Sprintf("tc:hide:%d", msgID),
	}
	b.handleCallbackQuery(context.Background(), cq)

	mock.mu.Lock()
	defer mock.mu.Unlock()

	if mock.edits != 1 {
		t.Fatalf("expected 1 edit, got %d", mock.edits)
	}
	// The edited text should be the compact summary (collapsed)
	if mock.lastEditText != compactText {
		t.Errorf("expected compact text %q, got: %s", compactText, mock.lastEditText)
	}
	// Button should be "Show full"
	kb := mock.lastEditOpts.ReplyMarkup
	btn := kb.InlineKeyboard[0][0]
	if btn.Text != "Show full" {
		t.Errorf("button text = %q, want %q", btn.Text, "Show full")
	}
}

func TestHandleCommandCallback_HTMLFallback(t *testing.T) {
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "test",
		Execute: func(ctx context.Context, args string) (string, error) {
			return "result with <bad> html & stuff", nil
		},
	})

	b, mock := testBot([]string{"111"}, reg)

	// First EditMessageText call (HTML) fails, second (plain text) succeeds
	mock.editErr = fmt.Errorf("Bad Request: can't parse entities")
	mock.editErrOnce = true

	b.handleCommandCallback(context.Background(), 12345, 1, "/test")

	mock.mu.Lock()
	defer mock.mu.Unlock()

	// Should have tried twice: once HTML, once plain text
	if mock.edits != 2 {
		t.Fatalf("expected 2 edits (HTML + fallback), got %d", mock.edits)
	}
	// The last edit should be plain text (no ParseMode)
	if mock.lastEditOpts != nil && mock.lastEditOpts.ParseMode != "" {
		t.Errorf("fallback edit should have no ParseMode, got %q", mock.lastEditOpts.ParseMode)
	}
}

func TestHandleCommandCallback_Chain(t *testing.T) {
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "tmux",
		Execute: func(ctx context.Context, args string) (string, error) {
			return "executed: " + args, nil
		},
		ChainKeyboard: func(ctx context.Context, subcommand string) []command.KeyboardOption {
			if subcommand == "kill" {
				return []command.KeyboardOption{
					{Label: "sess-a", Data: "kill sess-a"},
					{Label: "sess-b", Data: "kill sess-b"},
				}
			}
			return nil
		},
	})

	b, mock := testBot([]string{"111"}, reg)

	// Callback for "/tmux kill" (bare subcommand) should chain to a second keyboard
	b.handleCommandCallback(context.Background(), 12345, 1, "/tmux kill")

	mock.mu.Lock()
	defer mock.mu.Unlock()

	if mock.edits != 1 {
		t.Fatalf("expected 1 edit, got %d", mock.edits)
	}
	if mock.lastEditOpts == nil {
		t.Fatal("expected edit opts")
	}

	// Should have inline keyboard with 2 buttons
	kb := mock.lastEditOpts.ReplyMarkup
	if len(kb.InlineKeyboard) == 0 {
		t.Fatal("expected inline keyboard rows")
	}
	totalButtons := 0
	for _, row := range kb.InlineKeyboard {
		totalButtons += len(row)
	}
	if totalButtons != 2 {
		t.Fatalf("expected 2 buttons, got %d", totalButtons)
	}

	// Check button data format
	btn := kb.InlineKeyboard[0][0]
	if btn.Text != "sess-a" {
		t.Errorf("button text = %q, want sess-a", btn.Text)
	}
	if btn.CallbackData != "cmd:/tmux kill sess-a" {
		t.Errorf("callback data = %q, want cmd:/tmux kill sess-a", btn.CallbackData)
	}

	// Message text should be the prompt
	if !strings.Contains(mock.lastEditText, "/tmux kill") {
		t.Errorf("edit text = %q, want to contain /tmux kill", mock.lastEditText)
	}
}

func TestToolResultObserver_StoresResult(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.showToolCalls = "full"

	// Simulate what the ToolResultObserver closure does.
	var toolMsgID int64 = 99
	var toolMsgText = `▶️ <b>exec</b>: ls`
	var toolMsgFullText = "▶️ <b>exec</b>\n<pre>ls</pre>"
	var toolMsgMu sync.Mutex

	observer := func(toolName string, result string, isError bool) {
		if b.showToolCalls != "full" {
			return
		}
		toolMsgMu.Lock()
		msgID := toolMsgID
		compact := toolMsgText
		full := toolMsgFullText
		toolMsgMu.Unlock()
		if msgID == 0 {
			return
		}
		b.toolResults.Store(msgID, toolResultEntry{
			compactText: compact,
			fullInput:   full,
			result:      result,
		})
	}

	observer("shell", "file1.txt\nfile2.txt", false)

	val, ok := b.toolResults.Load(int64(99))
	if !ok {
		t.Fatal("expected tool result to be stored")
	}
	entry := val.(toolResultEntry)
	if entry.result != "file1.txt\nfile2.txt" {
		t.Errorf("stored result = %q, want %q", entry.result, "file1.txt\nfile2.txt")
	}
	if entry.compactText != toolMsgText {
		t.Errorf("stored compactText = %q, want %q", entry.compactText, toolMsgText)
	}
	if entry.fullInput != toolMsgFullText {
		t.Errorf("stored fullInput = %q, want %q", entry.fullInput, toolMsgFullText)
	}
}

func TestFormatToolCallWithResult_Truncation(t *testing.T) {
	toolText := `▶️ <b>exec</b>\n<pre>ls</pre>`

	// Short result — should not be truncated
	result := "hello"
	out := formatToolCallWithResult(toolText, result)
	if !strings.Contains(out, "hello") {
		t.Error("expected result in output")
	}
	if !strings.Contains(out, "Result:") {
		t.Error("expected Result: header in output")
	}

	// Long result — should be truncated to fit 4096 chars
	longResult := strings.Repeat("x", 5000)
	out = formatToolCallWithResult(toolText, longResult)
	if len(out) > 4096 {
		t.Errorf("output length = %d, want <= 4096", len(out))
	}
	if !strings.HasSuffix(out, "</pre>") {
		t.Errorf("expected output to end with </pre>, got: ...%s", out[len(out)-20:])
	}
	if !strings.Contains(out, "...") {
		t.Error("expected truncation indicator ...")
	}
}

// --- Steer buffer ---

// TestSteerBuffer_AppendAndDrain verifies basic append/drain semantics:
// multiple appends accumulate and drain joins them with newlines.
func TestSteerBuffer_AppendAndDrain(t *testing.T) {
	b, _ := testBot([]string{}, command.NewRegistry())

	b.appendSteer("first")
	b.appendSteer("second")
	b.appendSteer("third")

	got := b.drainSteer()
	want := "first\nsecond\nthird"
	if got != want {
		t.Errorf("drainSteer() = %q, want %q", got, want)
	}

	// Second drain should return empty
	if again := b.drainSteer(); again != "" {
		t.Errorf("second drain = %q, want empty", again)
	}
}

// TestSteerBuffer_DrainEmpty verifies that draining an empty buffer returns "".
func TestSteerBuffer_DrainEmpty(t *testing.T) {
	b, _ := testBot([]string{}, command.NewRegistry())
	if got := b.drainSteer(); got != "" {
		t.Errorf("drainSteer() on empty buffer = %q, want empty", got)
	}
}

// TestSteerBuffer_Concurrent verifies that concurrent appends and drains
// don't race or lose data.
func TestSteerBuffer_Concurrent(t *testing.T) {
	b, _ := testBot([]string{}, command.NewRegistry())

	const n = 100
	done := make(chan struct{})

	// Writer goroutine
	go func() {
		for i := 0; i < n; i++ {
			b.appendSteer(fmt.Sprintf("msg-%d", i))
		}
		close(done)
	}()

	// Drain periodically until writer is done and buffer is empty
	var collected []string
	for {
		if text := b.drainSteer(); text != "" {
			collected = append(collected, text)
		}
		select {
		case <-done:
			// Writer finished — drain remaining
			if text := b.drainSteer(); text != "" {
				collected = append(collected, text)
			}
			// Verify we got all messages
			joined := strings.Join(collected, "\n")
			for i := 0; i < n; i++ {
				want := fmt.Sprintf("msg-%d", i)
				if !strings.Contains(joined, want) {
					t.Errorf("missing %q in collected steer text", want)
				}
			}
			return
		default:
		}
	}
}

// TestReceiveMessage_SteerRoutesToBuffer verifies that when steer mode is
// enabled and a turn is active, text messages go to the steer buffer instead
// of the queue.
func TestReceiveMessage_SteerRoutesToBuffer(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.SetSteerMode(true)

	// Simulate active turn
	_, cancel := context.WithCancel(context.Background())
	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()
	defer cancel()

	msg := makeMsg(111, "owner", "change direction")
	b.receiveMessage(context.Background(), msg)

	// Should NOT be in the queue
	if len(b.queue) != 0 {
		t.Error("message should not be in queue when steer mode is active")
	}

	// Should be in steer buffer
	got := b.drainSteer()
	if got != "change direction" {
		t.Errorf("steer buffer = %q, want %q", got, "change direction")
	}
}

// TestReceiveMessage_SteerDisabledQueuesNormally verifies that when steer mode
// is disabled, messages go to the queue even during an active turn.
func TestReceiveMessage_SteerDisabledQueuesNormally(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	// steerMode defaults to false

	// Simulate active turn
	_, cancel := context.WithCancel(context.Background())
	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()
	defer cancel()

	msg := makeMsg(111, "owner", "hello")
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 1 {
		t.Errorf("queue length = %d, want 1", len(b.queue))
	}
	if got := b.drainSteer(); got != "" {
		t.Errorf("steer buffer should be empty, got %q", got)
	}
}

// TestReceiveMessage_SteerNoActiveTurnQueuesNormally verifies that when steer
// mode is enabled but no turn is active, messages go to the normal queue.
func TestReceiveMessage_SteerNoActiveTurnQueuesNormally(t *testing.T) {
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	b.SetSteerMode(true)

	// No active turn (turnCancel is nil)
	msg := makeMsg(111, "owner", "hello")
	b.receiveMessage(context.Background(), msg)

	if len(b.queue) != 1 {
		t.Errorf("queue length = %d, want 1", len(b.queue))
	}
	if got := b.drainSteer(); got != "" {
		t.Errorf("steer buffer should be empty, got %q", got)
	}
}

// TestSetSteerMode verifies that SetSteerMode toggles the flag.
func TestSetSteerMode(t *testing.T) {
	b, _ := testBot([]string{}, command.NewRegistry())
	if b.steerMode {
		t.Error("steerMode should default to false")
	}
	b.SetSteerMode(true)
	if !b.steerMode {
		t.Error("steerMode should be true after SetSteerMode(true)")
	}
	b.SetSteerMode(false)
	if b.steerMode {
		t.Error("steerMode should be false after SetSteerMode(false)")
	}
}
