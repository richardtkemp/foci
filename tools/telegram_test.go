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
	textCalls     []string
	documentCalls []string
	voiceCalls    []string
	textErr       error
	documentErr   error
	voiceErr      error

	// Chat-targeted calls
	chatTextCalls     []mockChatCall
	chatDocumentCalls []mockChatCall
	chatVoiceCalls    []mockChatCall
}

type mockChatCall struct {
	chatID int64
	value  string // text or filePath
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

func TestSendTelegramTextOnly(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello user",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Sent: text" {
		t.Errorf("result = %q, want %q", result, "Sent: text")
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
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/report.pdf",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Sent: document" {
		t.Errorf("result = %q", result)
	}
	if len(mock.documentCalls) != 1 || mock.documentCalls[0] != "/tmp/report.pdf" {
		t.Errorf("documentCalls = %v", mock.documentCalls)
	}
}

func TestSendTelegramVoice(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/note.ogg",
		"as_voice":  true,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Sent: voice note" {
		t.Errorf("result = %q", result)
	}
	if len(mock.voiceCalls) != 1 || mock.voiceCalls[0] != "/tmp/note.ogg" {
		t.Errorf("voiceCalls = %v", mock.voiceCalls)
	}
}

func TestSendTelegramTextAndDocument(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

	params, _ := json.Marshal(map[string]interface{}{
		"text":      "here's the file",
		"file_path": "/tmp/data.csv",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Sent: text + document" {
		t.Errorf("result = %q", result)
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
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

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
	tool := NewSendTelegramTool(func() TelegramSender { return nil })

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
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

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
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

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
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/voice.ogg",
		"as_voice":  true,
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
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

	ctx := WithSessionKey(context.Background(), "agent:fotini:chat:99887766")
	params, _ := json.Marshal(map[string]interface{}{
		"text": "hello Dick",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Sent: text" {
		t.Errorf("result = %q", result)
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
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

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
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

	ctx := WithSessionKey(context.Background(), "agent:fotini:chat:12345")
	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/note.ogg",
		"as_voice":  true,
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
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

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
	tool := NewSendTelegramTool(func() TelegramSender { return mock })

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
		{"agent:test:spawn:spawn-123456", 0},
		{"agent:test:main", 0},
		{"agent:test:multiball:mb-123", 0},
		{"", 0},
		{"agent:test:chat:notanumber", 0},
	}
	for _, tt := range tests {
		got := chatIDFromSessionKey(tt.key)
		if got != tt.want {
			t.Errorf("chatIDFromSessionKey(%q) = %d, want %d", tt.key, got, tt.want)
		}
	}
}
