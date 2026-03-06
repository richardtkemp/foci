package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestSendTelegramChatRouting verifies that messages are routed to specific chats when chat ID is in session key.
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

// TestSendTelegramChatRoutingDocument verifies that documents are routed to specific chats.
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

// TestSendTelegramChatRoutingVoice verifies that voice notes are routed to specific chats.
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

// TestSendTelegramFallbackNoChat verifies that default routing is used when no chat ID is in session key.
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

// TestSendTelegramFallbackNoContext verifies that default routing is used when no session key is present.
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

// TestChatIDFromSessionKey verifies ChatIDFromSessionKey parsing for various session key formats.
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

// TestSendTelegramChatSessionUsesPrimary verifies that chat sessions use primary bot even with multiball callbacks.
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

// TestSendTelegramCrossSessionHeader verifies that messages from different sessions are prepended with a header.
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

// TestSendTelegramSameSessionNoHeader verifies that messages from the same session are not prepended with a header.
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

// TestSendTelegramCrossSessionNoHeaderWhenBotSessionEmpty verifies no header when bot session is empty.
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

// TestSendTelegramMultiballRouting verifies that multiball sessions are routed to the correct sender.
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
