package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestSendMessageToUserChatRouting verifies that messages are routed to specific chats when chat ID is in session key.
func TestSendMessageToUserChatRouting(t *testing.T) {
	// When session key contains a chat ID, send to that specific chat.
	t.Parallel()
	mock := &mockMessageSender{}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "fotini/c99887766/1000")
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

// TestSendMessageToUserChatRoutingDocument verifies that documents are routed to specific chats.
func TestSendMessageToUserChatRoutingDocument(t *testing.T) {
	t.Parallel()
	mock := &mockMessageSender{}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "fotini/c12345/1000")
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

// TestSendMessageToUserChatRoutingVoice verifies that voice notes are routed to specific chats.
func TestSendMessageToUserChatRoutingVoice(t *testing.T) {
	t.Parallel()
	mock := &mockMessageSender{}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "fotini/c12345/1000")
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

// TestSendMessageToUserFallbackNoChat verifies that default routing is used when no chat ID is in session key.
func TestSendMessageToUserFallbackNoChat(t *testing.T) {
	// When session key doesn't contain a chat ID, fall back to default.
	t.Parallel()
	mock := &mockMessageSender{}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

	// Independent session — no chat ID
	ctx := WithSessionKey(context.Background(), "fotini/ispawn-12345/1000")
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

// TestSendMessageToUserFallbackNoContext verifies that default routing is used when no session key is present.
func TestSendMessageToUserFallbackNoContext(t *testing.T) {
	// No session key in context at all — fall back to default.
	t.Parallel()
	mock := &mockMessageSender{}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

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

// TestChatIDFromSessionKey verifies ChatIDFromSessionKey parsing for session key formats.
func TestChatIDFromSessionKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key  string
		want int64
	}{
		{"fotini/c99887766/1000", 99887766},
		{"clutch/c12345/1000", 12345},
		{"fotini/c5970082313/1000", 5970082313},
		{"fotini/c8792716180/1000", 8792716180},
		{"test/c-1001234567890/1000", -1001234567890}, // group chat
		{"test/ispawn-123456/1000", 0},                // independent session
		{"test/i0/0", 0},                              // independent session
		{"test/imb-123/1000", 0},                      // multiball (independent)
		{"", 0},
		{"fotini/c99887766/1000/b2000", 99887766},            // branch preserves chat ID
		{"test/c-1001234567890/1000/b2000", -1001234567890},  // branch — group chat
	}
	for _, tt := range tests {
		got := ChatIDFromSessionKey(tt.key)
		if got != tt.want {
			t.Errorf("ChatIDFromSessionKey(%q) = %d, want %d", tt.key, got, tt.want)
		}
	}
}

// TestSendMessageToUserChatSessionUsesPrimary verifies that chat sessions use primary bot even with multiball callbacks.
func TestSendMessageToUserChatSessionUsesPrimary(t *testing.T) {
	// Regular chat sessions should still use the primary bot.
	t.Parallel()
	multiballMock := &mockMessageSender{}
	primaryMock := &mockMessageSender{}

	tool := NewSendMessageToUserTool(func(sessionKey string) MessageSender {
		if strings.Contains(sessionKey, "/imb-") {
			return multiballMock
		}
		return primaryMock
	}, nil)

	ctx := WithSessionKey(context.Background(), "clutch/c99887766/1000")
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

// TestSendMessageToUserCrossSessionHeader verifies that messages from different sessions are prepended with a header.
func TestSendMessageToUserCrossSessionHeader(t *testing.T) {
	// Message from a different session than the bot's own session
	t.Parallel()
	// should be prepended with a header.
	mock := &mockMessageSender{sessionKey: "fotini/c99887766/1000"}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "fotini/ispawn-12345/1000")
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
	want := "[[ message from fotini/ispawn-12345/1000 ]]\nbackground task done"
	if mock.textCalls[0] != want {
		t.Errorf("text = %q, want %q", mock.textCalls[0], want)
	}
}

// TestSendMessageToUserSameSessionNoHeader verifies that messages from the same session are not prepended with a header.
func TestSendMessageToUserSameSessionNoHeader(t *testing.T) {
	// Message from the bot's own session should NOT get a header.
	t.Parallel()
	mock := &mockMessageSender{sessionKey: "fotini/c99887766/1000"}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "fotini/c99887766/1000")
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

// TestSendMessageToUserCrossSessionNoHeaderWhenBotSessionEmpty verifies no header when bot session is empty.
func TestSendMessageToUserCrossSessionNoHeaderWhenBotSessionEmpty(t *testing.T) {
	// When bot has no session key (not yet attached), don't add header.
	t.Parallel()
	mock := &mockMessageSender{sessionKey: ""}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "fotini/ispawn-12345/1000")
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

// TestSendMessageToUserMultiballRouting verifies that multiball sessions are routed to the correct sender.
func TestSendMessageToUserMultiballRouting(t *testing.T) {
	// When session key is a multiball session, the getSender callback receives
	t.Parallel()
	// the session key so it can resolve the correct bot.
	multiballMock := &mockMessageSender{}
	primaryMock := &mockMessageSender{}

	tool := NewSendMessageToUserTool(func(sessionKey string) MessageSender {
		if strings.Contains(sessionKey, "/imb-") {
			return multiballMock
		}
		return primaryMock
	}, nil)

	// Multiball session — should use multiball sender
	ctx := WithSessionKey(context.Background(), "clutch/imb-123/1000")
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
