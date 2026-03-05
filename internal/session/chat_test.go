package session

import (
	"testing"

	"foci/internal/provider"
)

func TestListChatSessions_Empty(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	sessions, err := store.ListChatSessions("test-agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestListChatSessions_WithSessions(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	// Create two chat sessions
	store.Append("test/c111/1000000000", provider.Message{Role: "user", Content: provider.TextContent("hi")})
	store.Append("test/c111/1000000000", provider.Message{Role: "assistant", Content: provider.TextContent("hello")})
	store.Append("test/c222/1000000000", provider.Message{Role: "user", Content: provider.TextContent("hey")})

	sessions, err := store.ListChatSessions("test")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}

	// Check chat IDs (order may vary)
	ids := map[int64]bool{}
	for _, s := range sessions {
		ids[s.ChatID] = true
	}
	if !ids[111] || !ids[222] {
		t.Errorf("expected chat IDs 111 and 222, got %v", ids)
	}

	// Check message counts
	for _, s := range sessions {
		if s.ChatID == 111 && s.MessageCount != 2 {
			t.Errorf("chat 111: expected 2 messages, got %d", s.MessageCount)
		}
		if s.ChatID == 222 && s.MessageCount != 1 {
			t.Errorf("chat 222: expected 1 message, got %d", s.MessageCount)
		}
	}
}

func TestListChatSessions_IgnoresNonChat(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	// Create a main session and a chat session
	store.Append("test/imain/1000000000", provider.Message{Role: "user", Content: provider.TextContent("hi")})
	store.Append("test/c111/1000000000", provider.Message{Role: "user", Content: provider.TextContent("hi")})

	sessions, err := store.ListChatSessions("test")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session (chat only), got %d", len(sessions))
	}
	if sessions[0].ChatID != 111 {
		t.Errorf("expected chat ID 111, got %d", sessions[0].ChatID)
	}
}

func TestListChatSessions_DifferentAgents(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	store.Append("alice/c111/1000000000", provider.Message{Role: "user", Content: provider.TextContent("hi")})
	store.Append("bob/c222/1000000000", provider.Message{Role: "user", Content: provider.TextContent("hey")})

	// Should only list alice's sessions
	sessions, err := store.ListChatSessions("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].ChatID != 111 {
		t.Errorf("expected chat ID 111, got %d", sessions[0].ChatID)
	}
}
