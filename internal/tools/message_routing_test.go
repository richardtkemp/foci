package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/platform"
)

func TestSendMessageToUserChatRouting(t *testing.T) {
	// Verifies that when the session key contains a chat ID, text is dispatched via SendTextToChat to that specific chat rather than the default sender.
	t.Parallel()
	mock := &mockSender{}
	tool := NewSendMessageToUserTool(func(string) platform.Sender { return mock }, nil)

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

func TestSendMessageToUserChatRoutingDocument(t *testing.T) {
	// Verifies that when the session key contains a chat ID, documents are dispatched via SendDocumentToChat rather than the default sender.
	t.Parallel()
	mock := &mockSender{}
	tool := NewSendMessageToUserTool(func(string) platform.Sender { return mock }, nil)

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

func TestSendMessageToUserChatRoutingVoice(t *testing.T) {
	// Verifies that when the session key contains a chat ID, voice notes are dispatched via SendVoiceToChat rather than the default sender.
	t.Parallel()
	mock := &mockSender{}
	tool := NewSendMessageToUserTool(func(string) platform.Sender { return mock }, nil)

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

func TestSendMessageToUserFallbackNoChat(t *testing.T) {
	// Verifies that when the session key has no chat ID (e.g. an independent spawn), the default SendText is used rather than the chat-targeted method.
	t.Parallel()
	mock := &mockSender{}
	tool := NewSendMessageToUserTool(func(string) platform.Sender { return mock }, nil)

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

func TestSendMessageToUserFallbackNoContext(t *testing.T) {
	// Verifies that when there is no session key in context at all, the default SendText is used.
	t.Parallel()
	mock := &mockSender{}
	tool := NewSendMessageToUserTool(func(string) platform.Sender { return mock }, nil)

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
	// Verifies that ChatIDFromSessionKey correctly extracts chat IDs from all session key formats, including group chats (negative IDs), branches, and returns 0 for non-chat sessions.
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
		{"test/if-123/1000", 0},                      // facet (independent)
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

func TestSendMessageToUserChatSessionUsesPrimary(t *testing.T) {
	// Verifies that regular chat sessions use the primary bot's sender even when a facet callback is registered, preventing misrouting.
	t.Parallel()
	facetMock := &mockSender{}
	primaryMock := &mockSender{}

	tool := NewSendMessageToUserTool(func(sessionKey string) platform.Sender {
		if strings.Contains(sessionKey, "/if-") {
			return facetMock
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
	if len(facetMock.textCalls) != 0 && len(facetMock.chatTextCalls) != 0 {
		t.Errorf("facet should not be called for chat session")
	}
}

func TestSendMessageToUserCrossSessionHeader(t *testing.T) {
	// Verifies that messages arriving from a session different from the bot's own session are prepended with a session header so the user knows the source.
	t.Parallel()
	mock := &mockSender{sessionKey: "fotini/c99887766/1000"}
	tool := NewSendMessageToUserTool(func(string) platform.Sender { return mock }, nil)

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

func TestSendMessageToUserSameSessionNoHeader(t *testing.T) {
	// Verifies that messages from the bot's own session are sent without a header, since no attribution is needed.
	t.Parallel()
	mock := &mockSender{sessionKey: "fotini/c99887766/1000"}
	tool := NewSendMessageToUserTool(func(string) platform.Sender { return mock }, nil)

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

func TestSendMessageToUserCrossSessionNoHeaderWhenBotSessionEmpty(t *testing.T) {
	// Verifies that when the bot has no session key (not yet attached), no header is prepended even for messages from other sessions.
	t.Parallel()
	mock := &mockSender{sessionKey: ""}
	tool := NewSendMessageToUserTool(func(string) platform.Sender { return mock }, nil)

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

func TestSendMessageToUserFacetRouting(t *testing.T) {
	// Verifies that facet session keys are passed to the getSender callback so the correct per-bot sender is resolved rather than the primary one.
	t.Parallel()
	facetMock := &mockSender{}
	primaryMock := &mockSender{}

	tool := NewSendMessageToUserTool(func(sessionKey string) platform.Sender {
		if strings.Contains(sessionKey, "/if-") {
			return facetMock
		}
		return primaryMock
	}, nil)

	// Facet session — should use facet sender
	ctx := WithSessionKey(context.Background(), "clutch/if-123/1000")
	params, _ := json.Marshal(map[string]interface{}{
		"text": "facet message",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: text" {
		t.Errorf("result = %q", result.Text)
	}
	if len(facetMock.textCalls) != 1 || facetMock.textCalls[0] != "facet message" {
		t.Errorf("facet textCalls = %v", facetMock.textCalls)
	}
	if len(primaryMock.textCalls) != 0 {
		t.Errorf("primary should not be called for facet session")
	}
}
