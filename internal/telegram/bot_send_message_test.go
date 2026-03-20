package telegram

import (
	"path/filepath"
	"testing"

	"foci/internal/command"
	"foci/internal/session"
)

func TestSendInjected_SkipsEmptyMessage(t *testing.T) {
	// Verifies that SendInjected silently
	// skips empty strings.
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
	// Verifies that SendInjected
	// silently skips whitespace-only messages.
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
	// Verifies that SendInjected sends
	// non-empty messages.
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

func TestSendToSession_ChatSession(t *testing.T) {
	// Verifies that SendToSession extracts the
	// chat ID from a chat-based session key and sends to that chat.
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

func TestSendToSession_IndependentSessionFallsBackToDefault(t *testing.T) {
	// Verifies that
	// SendToSession falls back to defaultChatID for independent sessions.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	// Independent session has no chat ID — needs a default chat fallback.
	// Set up a session index with a default chat.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	b.agentID = "main"
	b.chatmeta.AgentID = "main"
	b.SetSessionIndex(idx)
	_ = idx.SetDefaultChat("main", platformName, 11111)

	if err := b.SendToSession("main/i1709596800/1709596800", "hello independent"); err != nil {
		t.Fatalf("SendToSession error: %v", err)
	}
	if mock.sentCount() != 1 {
		t.Errorf("sentCount = %d, want 1", mock.sentCount())
	}
}

func TestSendToSession_NoChatIDNoDefaultErrors(t *testing.T) {
	// Verifies that SendToSession
	// returns an error when no chat ID and no default chat are configured.
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	err := b.SendToSession("main/i1709596800/1709596800", "hello")
	if err == nil {
		t.Fatal("expected error when no chat ID and no default")
	}
}

func TestSendToSession_SkipsEmptyMessage(t *testing.T) {
	// Verifies that SendToSession silently
	// skips empty messages.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	if err := b.SendToSession("main/c123/1709590000", ""); err != nil {
		t.Errorf("SendToSession with empty text should not error, got: %v", err)
	}
	if mock.sentCount() != 0 {
		t.Errorf("sentCount = %d, want 0", mock.sentCount())
	}
}

func TestSendToSession_BranchKeyUsesParentChat(t *testing.T) {
	// Verifies that branch session keys
	// resolve to the parent chat ID.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	err := b.SendToSession("main/c67890/1709590000/b1709596800", "branch message")
	if err != nil {
		t.Fatalf("SendToSession error: %v", err)
	}
	if mock.sentCount() != 1 {
		t.Errorf("sentCount = %d, want 1", mock.sentCount())
	}
}
