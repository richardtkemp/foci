package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// mockTelegramSender records calls to all send methods.
type mockTelegramSender struct {
	sessionKey    string
	textCalls     []string
	documentCalls []string
	voiceCalls    []string
	videoCalls     []string
	photoCalls     []string
	audioCalls     []string
	animationCalls   []string
	voiceDataCalls   [][]byte
	textErr          error
	documentErr      error
	voiceErr         error
	videoErr         error
	photoErr         error
	audioErr         error
	animationErr     error
	voiceDataErr     error

	// Chat-targeted calls
	chatTextCalls      []mockChatCall
	chatDocumentCalls  []mockChatCall
	chatVoiceCalls     []mockChatCall
	chatVideoCalls     []mockChatCall
	chatPhotoCalls     []mockChatCall
	chatAudioCalls     []mockChatCall
	chatAnimationCalls []mockChatCall
	chatVoiceDataCalls []mockChatDataCall
}

func (m *mockTelegramSender) SessionKey() string {
	return m.sessionKey
}

type mockChatCall struct {
	chatID int64
	value  string // text or filePath
}

type mockChatDataCall struct {
	chatID int64
	data   []byte
}

func (m *mockTelegramSender) SendText(text string) error {
	m.textCalls = append(m.textCalls, text)
	return m.textErr
}

func (m *mockTelegramSender) SendDocument(filePath string) error {
	m.documentCalls = append(m.documentCalls, filePath)
	return m.documentErr
}

func (m *mockTelegramSender) SendVoice(filePath string) error {
	m.voiceCalls = append(m.voiceCalls, filePath)
	return m.voiceErr
}

func (m *mockTelegramSender) SendVideo(filePath string) error {
	m.videoCalls = append(m.videoCalls, filePath)
	return m.videoErr
}

func (m *mockTelegramSender) SendPhoto(filePath string) error {
	m.photoCalls = append(m.photoCalls, filePath)
	return m.photoErr
}

func (m *mockTelegramSender) SendAudio(filePath string) error {
	m.audioCalls = append(m.audioCalls, filePath)
	return m.audioErr
}

func (m *mockTelegramSender) SendAnimation(filePath string) error {
	m.animationCalls = append(m.animationCalls, filePath)
	return m.animationErr
}

func (m *mockTelegramSender) SendTextToChat(chatID int64, text string) error {
	m.chatTextCalls = append(m.chatTextCalls, mockChatCall{chatID, text})
	return m.textErr
}

func (m *mockTelegramSender) SendDocumentToChat(chatID int64, filePath string) error {
	m.chatDocumentCalls = append(m.chatDocumentCalls, mockChatCall{chatID, filePath})
	return m.documentErr
}

func (m *mockTelegramSender) SendVoiceToChat(chatID int64, filePath string) error {
	m.chatVoiceCalls = append(m.chatVoiceCalls, mockChatCall{chatID, filePath})
	return m.voiceErr
}

func (m *mockTelegramSender) SendVideoToChat(chatID int64, filePath string) error {
	m.chatVideoCalls = append(m.chatVideoCalls, mockChatCall{chatID, filePath})
	return m.videoErr
}

func (m *mockTelegramSender) SendPhotoToChat(chatID int64, filePath string) error {
	m.chatPhotoCalls = append(m.chatPhotoCalls, mockChatCall{chatID, filePath})
	return m.photoErr
}

func (m *mockTelegramSender) SendAudioToChat(chatID int64, filePath string) error {
	m.chatAudioCalls = append(m.chatAudioCalls, mockChatCall{chatID, filePath})
	return m.audioErr
}

func (m *mockTelegramSender) SendAnimationToChat(chatID int64, filePath string) error {
	m.chatAnimationCalls = append(m.chatAnimationCalls, mockChatCall{chatID, filePath})
	return m.animationErr
}

func (m *mockTelegramSender) SendVoiceData(audioData []byte) error {
	m.voiceDataCalls = append(m.voiceDataCalls, audioData)
	return m.voiceDataErr
}

func (m *mockTelegramSender) SendVoiceDataToChat(chatID int64, audioData []byte) error {
	m.chatVoiceDataCalls = append(m.chatVoiceDataCalls, mockChatDataCall{chatID, audioData})
	return m.voiceDataErr
}

func TestSendTelegramTextOnly(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello user",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: text" {
		t.Errorf("result = %q, want %q", result.Text, "Sent: text")
	}
	if len(mock.textCalls) != 1 || mock.textCalls[0] != "hello user" {
		t.Errorf("textCalls = %v", mock.textCalls)
	}
	if len(mock.documentCalls) != 0 {
		t.Errorf("documentCalls = %v", mock.documentCalls)
	}
}

func TestSendTelegramDocumentOnly(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/report.pdf",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: document" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.documentCalls) != 1 || mock.documentCalls[0] != "/tmp/report.pdf" {
		t.Errorf("documentCalls = %v", mock.documentCalls)
	}
}

func TestSendTelegramVoice(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/note.ogg",
		"send_as":   "voice",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: voice note" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.voiceCalls) != 1 || mock.voiceCalls[0] != "/tmp/note.ogg" {
		t.Errorf("voiceCalls = %v", mock.voiceCalls)
	}
}

func TestSendTelegramTextAndDocument(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"text":      "here's the file",
		"file_path": "/tmp/data.csv",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: text + document" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.textCalls) != 1 {
		t.Errorf("textCalls = %v", mock.textCalls)
	}
	if len(mock.documentCalls) != 1 {
		t.Errorf("documentCalls = %v", mock.documentCalls)
	}
}

func TestSendTelegramNoInput(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for no input")
	}
	if !strings.Contains(err.Error(), "at least one") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendTelegramNilSender(t *testing.T) {
	tool := NewSendTelegramTool(func(string) TelegramSender { return nil }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for nil sender")
	}
	if !strings.Contains(err.Error(), "telegram not configured") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendTelegramTextError(t *testing.T) {
	mock := &mockTelegramSender{textErr: fmt.Errorf("network down")}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "network down") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendTelegramDocumentError(t *testing.T) {
	mock := &mockTelegramSender{documentErr: fmt.Errorf("file too large")}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/huge.bin",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "file too large") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendTelegramVoiceError(t *testing.T) {
	mock := &mockTelegramSender{voiceErr: fmt.Errorf("codec error")}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/voice.ogg",
		"send_as":   "voice",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "codec error") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestJoinWords(t *testing.T) {
	tests := []struct {
		words []string
		want  string
	}{
		{nil, "nothing"},
		{[]string{"text"}, "text"},
		{[]string{"text", "document"}, "text + document"},
	}
	for _, tt := range tests {
		got := joinWords(tt.words)
		if got != tt.want {
			t.Errorf("joinWords(%v) = %q, want %q", tt.words, got, tt.want)
		}
	}
}

// --- Chat routing tests ---

func TestSendTelegramChatRouting(t *testing.T) {
	// When session key contains a chat ID, send to that specific chat.
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "agent:fotini:chat:99887766")
	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello Dick",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: text" {
		t.Errorf("result = %q", result.Text)
	}

	// Should use chat-targeted method, not default
	if len(mock.chatTextCalls) != 1 {
		t.Fatalf("expected 1 chatTextCall, got %d", len(mock.chatTextCalls))
	}
	if mock.chatTextCalls[0].chatID != 99887766 {
		t.Errorf("chatID = %d, want 99887766", mock.chatTextCalls[0].chatID)
	}
	if mock.chatTextCalls[0].value != "hello Dick" {
		t.Errorf("text = %q", mock.chatTextCalls[0].value)
	}
	if len(mock.textCalls) != 0 {
		t.Errorf("default SendText should not be called, got %d calls", len(mock.textCalls))
	}
}

func TestSendTelegramChatRoutingDocument(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "agent:fotini:chat:12345")
	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/report.pdf",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.chatDocumentCalls) != 1 {
		t.Fatalf("expected 1 chatDocumentCall, got %d", len(mock.chatDocumentCalls))
	}
	if mock.chatDocumentCalls[0].chatID != 12345 {
		t.Errorf("chatID = %d, want 12345", mock.chatDocumentCalls[0].chatID)
	}
	if len(mock.documentCalls) != 0 {
		t.Errorf("default SendDocument should not be called")
	}
}

func TestSendTelegramChatRoutingVoice(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "agent:fotini:chat:12345")
	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/note.ogg",
		"send_as":   "voice",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.chatVoiceCalls) != 1 {
		t.Fatalf("expected 1 chatVoiceCall, got %d", len(mock.chatVoiceCalls))
	}
	if mock.chatVoiceCalls[0].chatID != 12345 {
		t.Errorf("chatID = %d, want 12345", mock.chatVoiceCalls[0].chatID)
	}
	if len(mock.voiceCalls) != 0 {
		t.Errorf("default SendVoice should not be called")
	}
}

func TestSendTelegramFallbackNoChat(t *testing.T) {
	// When session key doesn't contain a chat ID, fall back to default.
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	// Spawn branch session — no chat ID
	ctx := WithSessionKey(context.Background(), "agent:fotini:spawn:spawn-12345")
	params, _ := json.Marshal(map[string]interface{}{
		"text": "background result",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should use default SendText, not chat-targeted
	if len(mock.textCalls) != 1 {
		t.Fatalf("expected 1 default textCall, got %d", len(mock.textCalls))
	}
	if len(mock.chatTextCalls) != 0 {
		t.Errorf("chat-targeted should not be called")
	}
}

func TestSendTelegramFallbackNoContext(t *testing.T) {
	// No session key in context at all — fall back to default.
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello",
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.textCalls) != 1 {
		t.Fatalf("expected 1 default textCall, got %d", len(mock.textCalls))
	}
	if len(mock.chatTextCalls) != 0 {
		t.Errorf("chat-targeted should not be called")
	}
}

func TestChatIDFromSessionKey(t *testing.T) {
	tests := []struct {
		key  string
		want int64
	}{
		{"agent:fotini:chat:99887766", 99887766},
		{"agent:clutch:chat:12345", 12345},
		{"agent:fotini:chat:5970082313", 5970082313},
		{"agent:fotini:chat:8792716180", 8792716180},
		{"agent:test:chat:-1001234567890", -1001234567890}, // group chat
		{"agent:test:spawn:spawn-123456", 0},
		{"agent:test:main", 0},
		{"agent:test:multiball:mb-123", 0},
		{"agent:fotini:8792716180", 8792716180},          // legacy format without chat: segment
		{"agent:test:5970082313", 5970082313},              // legacy format — another agent
		{"agent:test:-1001234567890", -1001234567890},      // legacy format — group chat (negative ID)
		{"", 0},
		{"agent:test:chat:notanumber", 0},
		{"agent:test:notanumber", 0}, // non-numeric third segment is not a chat ID
		{"fotini/c99887766/1000000000", 99887766},                   // new format
		{"test/c12345/1000000000", 12345},                           // new format
		{"test/c-1001234567890/1000000000", -1001234567890},         // new format — group chat
		{"test/imain/1000000000", 0},                                // new format — independent session, not chat
		{"test/imain/1000000000/b1000000001", 0},                    // new format — branch, not chat
	}
	for _, tt := range tests {
		got := ChatIDFromSessionKey(tt.key)
		if got != tt.want {
			t.Errorf("ChatIDFromSessionKey(%q) = %d, want %d", tt.key, got, tt.want)
		}
	}
}

// --- send_as tests ---

func TestSendTelegramSendAsVideo(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/clip.mp4",
		"send_as":   "video",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: video" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.videoCalls) != 1 || mock.videoCalls[0] != "/tmp/clip.mp4" {
		t.Errorf("videoCalls = %v", mock.videoCalls)
	}
	if len(mock.documentCalls) != 0 {
		t.Errorf("documentCalls should be empty, got %v", mock.documentCalls)
	}
}

func TestSendTelegramSendAsVoice(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/note.ogg",
		"send_as":   "voice",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: voice note" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.voiceCalls) != 1 {
		t.Errorf("voiceCalls = %v", mock.voiceCalls)
	}
}

func TestSendTelegramSendAsDocument(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/report.pdf",
		"send_as":   "document",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: document" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.documentCalls) != 1 {
		t.Errorf("documentCalls = %v", mock.documentCalls)
	}
}

func TestSendTelegramSendAsDefaultIsDocument(t *testing.T) {
	// No send_as — should default to document
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/file.bin",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: document" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.documentCalls) != 1 {
		t.Errorf("documentCalls = %v", mock.documentCalls)
	}
}

func TestSendTelegramVideoError(t *testing.T) {
	mock := &mockTelegramSender{videoErr: fmt.Errorf("video too large")}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/big.mp4",
		"send_as":   "video",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "video too large") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendTelegramVideoChatRouting(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "agent:fotini:chat:12345")
	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/clip.mp4",
		"send_as":   "video",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.chatVideoCalls) != 1 {
		t.Fatalf("expected 1 chatVideoCall, got %d", len(mock.chatVideoCalls))
	}
	if mock.chatVideoCalls[0].chatID != 12345 {
		t.Errorf("chatID = %d, want 12345", mock.chatVideoCalls[0].chatID)
	}
	if len(mock.videoCalls) != 0 {
		t.Errorf("default SendVideo should not be called")
	}
}

func TestSendTelegramTextAndVideo(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"text":      "check this out",
		"file_path": "/tmp/clip.mp4",
		"send_as":   "video",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: text + video" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.textCalls) != 1 {
		t.Errorf("textCalls = %v", mock.textCalls)
	}
	if len(mock.videoCalls) != 1 {
		t.Errorf("videoCalls = %v", mock.videoCalls)
	}
}

// --- photo tests ---

func TestSendTelegramSendAsPhoto(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/image.jpg",
		"send_as":   "photo",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: photo" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.photoCalls) != 1 || mock.photoCalls[0] != "/tmp/image.jpg" {
		t.Errorf("photoCalls = %v", mock.photoCalls)
	}
}

func TestSendTelegramPhotoError(t *testing.T) {
	mock := &mockTelegramSender{photoErr: fmt.Errorf("image too large")}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/huge.jpg",
		"send_as":   "photo",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "image too large") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendTelegramPhotoChatRouting(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "agent:fotini:chat:12345")
	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/image.jpg",
		"send_as":   "photo",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.chatPhotoCalls) != 1 || mock.chatPhotoCalls[0].chatID != 12345 {
		t.Errorf("chatPhotoCalls = %v", mock.chatPhotoCalls)
	}
	if len(mock.photoCalls) != 0 {
		t.Errorf("default SendPhoto should not be called")
	}
}

// --- audio tests ---

func TestSendTelegramSendAsAudio(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/song.mp3",
		"send_as":   "audio",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: audio" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.audioCalls) != 1 || mock.audioCalls[0] != "/tmp/song.mp3" {
		t.Errorf("audioCalls = %v", mock.audioCalls)
	}
}

func TestSendTelegramAudioError(t *testing.T) {
	mock := &mockTelegramSender{audioErr: fmt.Errorf("bad format")}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/bad.mp3",
		"send_as":   "audio",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bad format") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendTelegramAudioChatRouting(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "agent:fotini:chat:12345")
	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/song.mp3",
		"send_as":   "audio",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.chatAudioCalls) != 1 || mock.chatAudioCalls[0].chatID != 12345 {
		t.Errorf("chatAudioCalls = %v", mock.chatAudioCalls)
	}
	if len(mock.audioCalls) != 0 {
		t.Errorf("default SendAudio should not be called")
	}
}

// --- animation tests ---

func TestSendTelegramSendAsAnimation(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/funny.gif",
		"send_as":   "animation",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: animation" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.animationCalls) != 1 || mock.animationCalls[0] != "/tmp/funny.gif" {
		t.Errorf("animationCalls = %v", mock.animationCalls)
	}
}

func TestSendTelegramAnimationError(t *testing.T) {
	mock := &mockTelegramSender{animationErr: fmt.Errorf("gif corrupted")}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/bad.gif",
		"send_as":   "animation",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "gif corrupted") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendTelegramAnimationChatRouting(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "agent:fotini:chat:12345")
	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/funny.gif",
		"send_as":   "animation",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.chatAnimationCalls) != 1 || mock.chatAnimationCalls[0].chatID != 12345 {
		t.Errorf("chatAnimationCalls = %v", mock.chatAnimationCalls)
	}
	if len(mock.animationCalls) != 0 {
		t.Errorf("default SendAnimation should not be called")
	}
}

// --- Multiball routing tests ---

func TestSendTelegramMultiballRouting(t *testing.T) {
	// When session key contains :multiball:, the getSender callback receives
	// the session key so it can resolve the correct bot.
	multiballMock := &mockTelegramSender{}
	primaryMock := &mockTelegramSender{}

	tool := NewSendTelegramTool(func(sessionKey string) TelegramSender {
		if strings.Contains(sessionKey, ":multiball:") {
			return multiballMock
		}
		return primaryMock
	}, nil)

	// Multiball session — should use multiball sender
	ctx := WithSessionKey(context.Background(), "agent:clutch:multiball:mb-123")
	params, _ := json.Marshal(map[string]interface{}{
		"text": "multiball message",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: text" {
		t.Errorf("result = %q", result.Text)
	}
	if len(multiballMock.textCalls) != 1 || multiballMock.textCalls[0] != "multiball message" {
		t.Errorf("multiball textCalls = %v", multiballMock.textCalls)
	}
	if len(primaryMock.textCalls) != 0 {
		t.Errorf("primary should not be called for multiball session")
	}
}

func TestSendTelegramChatSessionUsesPrimary(t *testing.T) {
	// Regular chat sessions should still use the primary bot.
	multiballMock := &mockTelegramSender{}
	primaryMock := &mockTelegramSender{}

	tool := NewSendTelegramTool(func(sessionKey string) TelegramSender {
		if strings.Contains(sessionKey, ":multiball:") {
			return multiballMock
		}
		return primaryMock
	}, nil)

	ctx := WithSessionKey(context.Background(), "agent:clutch:chat:99887766")
	params, _ := json.Marshal(map[string]interface{}{
		"text": "primary message",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(primaryMock.chatTextCalls) != 1 || primaryMock.chatTextCalls[0].chatID != 99887766 {
		t.Errorf("primary chatTextCalls = %v", primaryMock.chatTextCalls)
	}
	if len(multiballMock.textCalls) != 0 && len(multiballMock.chatTextCalls) != 0 {
		t.Errorf("multiball should not be called for chat session")
	}
}

// --- Cross-session header tests ---

func TestSendTelegramCrossSessionHeader(t *testing.T) {
	// Message from a different session than the bot's own session
	// should be prepended with a header.
	mock := &mockTelegramSender{sessionKey: "agent:fotini:chat:99887766"}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "agent:fotini:spawn:spawn-12345")
	params, _ := json.Marshal(map[string]interface{}{
		"text": "background task done",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.textCalls) != 1 {
		t.Fatalf("expected 1 textCall, got %d", len(mock.textCalls))
	}
	want := "[[ message from agent:fotini:spawn:spawn-12345 ]]\nbackground task done"
	if mock.textCalls[0] != want {
		t.Errorf("text = %q, want %q", mock.textCalls[0], want)
	}
}

func TestSendTelegramSameSessionNoHeader(t *testing.T) {
	// Message from the bot's own session should NOT get a header.
	mock := &mockTelegramSender{sessionKey: "agent:fotini:chat:99887766"}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "agent:fotini:chat:99887766")
	params, _ := json.Marshal(map[string]interface{}{
		"text": "normal message",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.chatTextCalls) != 1 {
		t.Fatalf("expected 1 chatTextCall, got %d", len(mock.chatTextCalls))
	}
	if mock.chatTextCalls[0].value != "normal message" {
		t.Errorf("text = %q, want %q", mock.chatTextCalls[0].value, "normal message")
	}
}

func TestSendTelegramCrossSessionNoHeaderWhenBotSessionEmpty(t *testing.T) {
	// When bot has no session key (not yet attached), don't add header.
	mock := &mockTelegramSender{sessionKey: ""}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "agent:fotini:spawn:spawn-12345")
	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.textCalls) != 1 {
		t.Fatalf("expected 1 textCall, got %d", len(mock.textCalls))
	}
	if mock.textCalls[0] != "hello" {
		t.Errorf("text = %q, want %q", mock.textCalls[0], "hello")
	}
}

// --- TTS synthesis tests ---

type mockTTS struct {
	data []byte
	err  error
}

func (m *mockTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.data, nil
}

func TestSendTelegramVoiceTTS(t *testing.T) {
	mock := &mockTelegramSender{}
	tts := &mockTTS{data: []byte("fake-audio")}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, tts)

	params, _ := json.Marshal(map[string]interface{}{
		"text":    "hello world",
		"send_as": "voice",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: voice note" {
		t.Errorf("result = %q, want %q", result.Text, "Sent: voice note")
	}
	if len(mock.voiceDataCalls) != 1 {
		t.Fatalf("expected 1 voiceDataCall, got %d", len(mock.voiceDataCalls))
	}
	if string(mock.voiceDataCalls[0]) != "fake-audio" {
		t.Errorf("voiceDataCalls[0] = %q", string(mock.voiceDataCalls[0]))
	}
	// Should NOT send text separately
	if len(mock.textCalls) != 0 {
		t.Errorf("textCalls = %v, should be empty for TTS synthesis", mock.textCalls)
	}
}

func TestSendTelegramVoiceTTSChatRouting(t *testing.T) {
	mock := &mockTelegramSender{}
	tts := &mockTTS{data: []byte("fake-audio")}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, tts)

	ctx := WithSessionKey(context.Background(), "agent:fotini:chat:12345")
	params, _ := json.Marshal(map[string]interface{}{
		"text":    "hello world",
		"send_as": "voice",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: voice note" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.chatVoiceDataCalls) != 1 {
		t.Fatalf("expected 1 chatVoiceDataCall, got %d", len(mock.chatVoiceDataCalls))
	}
	if mock.chatVoiceDataCalls[0].chatID != 12345 {
		t.Errorf("chatID = %d, want 12345", mock.chatVoiceDataCalls[0].chatID)
	}
	if len(mock.voiceDataCalls) != 0 {
		t.Errorf("default SendVoiceData should not be called")
	}
}

func TestSendTelegramVoiceTTSNoProvider(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"text":    "hello world",
		"send_as": "voice",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for nil TTS")
	}
	if !strings.Contains(err.Error(), "tts not configured") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendTelegramVoiceTTSSynthesizeError(t *testing.T) {
	mock := &mockTelegramSender{}
	tts := &mockTTS{err: fmt.Errorf("API rate limit")}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, tts)

	params, _ := json.Marshal(map[string]interface{}{
		"text":    "hello",
		"send_as": "voice",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error from synthesize")
	}
	if !strings.Contains(err.Error(), "API rate limit") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendTelegramVoiceFilePathStillWorks(t *testing.T) {
	// When file_path is provided with send_as=voice, it should use the file-based path
	mock := &mockTelegramSender{}
	tts := &mockTTS{data: []byte("should-not-be-used")}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, tts)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/note.ogg",
		"send_as":   "voice",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: voice note" {
		t.Errorf("result = %q", result.Text)
	}
	// Should use file-based voice, not TTS synthesis
	if len(mock.voiceCalls) != 1 {
		t.Errorf("voiceCalls = %v, want 1 file-based call", mock.voiceCalls)
	}
	if len(mock.voiceDataCalls) != 0 {
		t.Errorf("voiceDataCalls should be empty for file-based voice")
	}
}
