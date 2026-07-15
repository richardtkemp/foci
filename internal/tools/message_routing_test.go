package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/platform"
	"foci/internal/session"
)

func TestSendMessageToUserChatRouting(t *testing.T) {
	// Verifies that when the session key contains a chat ID, text is dispatched via SendTextToChat to that specific chat rather than the default sender.
	t.Parallel()
	mock := &mockSender{}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil, nil)

	ctx := WithSessionKey(context.Background(), "fotini/c99887766")
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
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil, nil)

	ctx := WithSessionKey(context.Background(), "fotini/c12345")
	params, _ := json.Marshal(map[string]interface{}{
		"file": "/tmp/report.pdf",
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
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil, nil)

	ctx := WithSessionKey(context.Background(), "fotini/c12345")
	params, _ := json.Marshal(map[string]interface{}{
		"file":    "/tmp/note.ogg",
		"send_as": "voice",
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
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil, nil)

	// Independent session — no chat ID
	ctx := WithSessionKey(context.Background(), "fotini/ispawn-12345")
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
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil, nil)

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
		{"fotini/c99887766", 99887766},
		{"clutch/c12345", 12345},
		{"fotini/c5970082313", 5970082313},
		{"fotini/c8792716180", 8792716180},
		{"test/c-1001234567890", -1001234567890}, // group chat
		{"test/ispawn-123456", 0},                // independent session
		{"test/i0", 0},                           // independent session
		{"test/if-123", 0},                       // facet (independent)
		{"", 0},
		{"fotini/c99887766/b2000", 99887766},           // branch preserves chat ID
		{"test/c-1001234567890/b2000", -1001234567890}, // branch — group chat
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

	tool := NewSendToChatTool(func(sessionKey string) platform.Sender {
		if strings.Contains(sessionKey, "/if-") {
			return facetMock
		}
		return primaryMock
	}, nil, nil)

	ctx := WithSessionKey(context.Background(), "clutch/c99887766")
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
	mock := &mockSender{sessionKey: "fotini/c99887766"}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil, nil)

	ctx := WithSessionKey(context.Background(), "fotini/ispawn-12345")
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
	want := "[[ message from fotini/ispawn-12345 ]]\nbackground task done"
	if mock.textCalls[0] != want {
		t.Errorf("text = %q, want %q", mock.textCalls[0], want)
	}
}

func TestSendMessageToUserSameSessionNoHeader(t *testing.T) {
	// Verifies that messages from the bot's own session are sent without a header, since no attribution is needed.
	t.Parallel()
	mock := &mockSender{sessionKey: "fotini/c99887766"}
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil, nil)

	ctx := WithSessionKey(context.Background(), "fotini/c99887766")
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
	tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil, nil)

	ctx := WithSessionKey(context.Background(), "fotini/ispawn-12345")
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

	tool := NewSendToChatTool(func(sessionKey string) platform.Sender {
		if strings.Contains(sessionKey, "/if-") {
			return facetMock
		}
		return primaryMock
	}, nil, nil)

	// Facet session — should use facet sender
	ctx := WithSessionKey(context.Background(), "clutch/if-123")
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

func TestSendMessageToUserFacetBranchRoutesToSelf(t *testing.T) {
	// A facet branch from a chat session (e.g. clutch/c123/b456) carries
	// the parent's chat ID in its key. sessionTypeFn=facet must override
	// the extracted chatID to 0 so the message lands in the facet's own
	// conversation, not the parent/root chat.
	t.Parallel()
	mock := &mockSender{sessionKey: "clutch/c123/b456"}
	tool := NewSendToChatTool(
		func(string) platform.Sender { return mock },
		nil,
		func(string) session.SessionType { return session.SessionTypeFacet },
	)

	ctx := WithSessionKey(context.Background(), "clutch/c123/b456")
	params, _ := json.Marshal(map[string]interface{}{
		"text": "facet branch text",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// chatID=0 → default SendText, not SendTextToChat(parentChatID).
	if len(mock.textCalls) != 1 || mock.textCalls[0] != "facet branch text" {
		t.Errorf("expected default textCall, got textCalls=%v chatTextCalls=%v", mock.textCalls, mock.chatTextCalls)
	}
	if len(mock.chatTextCalls) != 0 {
		t.Errorf("should not use SendTextToChat for facet branch, got %d calls", len(mock.chatTextCalls))
	}
}

func TestSendMessageToUserNonFacetBranchRoutesToRoot(t *testing.T) {
	// A non-facet branch (spawn, cron, etc.) from a chat session must keep
	// the extracted parent chat ID — messages go to the root chat, not the
	// branch's own session.
	t.Parallel()
	mock := &mockSender{sessionKey: "clutch/c123"}
	tool := NewSendToChatTool(
		func(string) platform.Sender { return mock },
		nil,
		func(string) session.SessionType { return session.SessionTypeSpawn },
	)

	ctx := WithSessionKey(context.Background(), "clutch/c123/b456")
	params, _ := json.Marshal(map[string]interface{}{
		"text": "spawn result",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Non-facet branch → uses SendTextToChat with the parent's chat ID.
	if len(mock.chatTextCalls) != 1 || mock.chatTextCalls[0].chatID != 123 {
		t.Errorf("expected chatTextCall to chat 123, got %v", mock.chatTextCalls)
	}
	if len(mock.textCalls) != 0 {
		t.Errorf("should not use default SendText for non-facet branch, got %d calls", len(mock.textCalls))
	}
}
