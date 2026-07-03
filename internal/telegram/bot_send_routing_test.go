package telegram

import (
	"path/filepath"
	"testing"

	"foci/internal/command"
	"foci/internal/session"
)

func TestSendToSession_ChatSession(t *testing.T) {
	// Session key with embedded chat ID — should extract and route there.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	err := b.SendToSession("main/c67890", "hello from session")
	if err != nil {
		t.Fatalf("SendToSession error: %v", err)
	}
	if mock.sentCount() != 1 {
		t.Errorf("sentCount = %d, want 1", mock.sentCount())
	}
}

func TestSendToSession_IndependentSessionFallsBackToDefault(t *testing.T) {
	// Independent session has no embedded chat ID — should fall back to default.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	b.agentID = "main"
	b.chatmeta.AgentID = "main"
	b.SetSessionIndex(idx)
	_ = idx.SetDefaultChat("main", platformName, 11111)

	if err := b.SendToSession("main/i1709596800", "hello independent"); err != nil {
		t.Fatalf("SendToSession error: %v", err)
	}
	if mock.sentCount() != 1 {
		t.Errorf("sentCount = %d, want 1", mock.sentCount())
	}
}

func TestSendToSession_NoChatIDNoDefaultErrors(t *testing.T) {
	// No embedded chat ID and no default chat — should error.
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	err := b.SendToSession("main/i1709596800", "hello")
	if err == nil {
		t.Fatal("expected error when no chat ID and no default")
	}
}

func TestSendToSession_SkipsEmptyMessage(t *testing.T) {
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	if err := b.SendToSession("main/c123", ""); err != nil {
		t.Errorf("SendToSession with empty text should not error, got: %v", err)
	}
	if mock.sentCount() != 0 {
		t.Errorf("sentCount = %d, want 0", mock.sentCount())
	}
}

func TestSendToSession_BranchKeyUsesParentChat(t *testing.T) {
	// Branch session keys contain parent's chat ID — should resolve correctly.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	err := b.SendToSession("main/c67890/b1709596800", "branch message")
	if err != nil {
		t.Fatalf("SendToSession error: %v", err)
	}
	if mock.sentCount() != 1 {
		t.Errorf("sentCount = %d, want 1", mock.sentCount())
	}
}
