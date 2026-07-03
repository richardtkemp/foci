package session

import (
	"testing"

	"foci/internal/provider"
)

func TestListChatSessions_Empty(t *testing.T) {
	// Proves that ListChatSessions returns an empty (not nil) slice when no
	// sessions exist for the requested agent.
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
	// Proves that ListChatSessions discovers all chat sessions for an agent,
	// returning the correct chat IDs and per-session message counts.
	dir := t.TempDir()
	store := NewStore(dir)

	// Create two chat sessions
	store.TestAppend("test/c111", provider.Message{Role: "user", Content: provider.TextContent("hi")})
	store.TestAppend("test/c111", provider.Message{Role: "assistant", Content: provider.TextContent("hello")})
	store.TestAppend("test/c222", provider.Message{Role: "user", Content: provider.TextContent("hey")})

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
	// Proves that ListChatSessions filters out non-chat sessions (e.g. independent
	// sessions with 'i' prefix) and only returns chat-keyed sessions.

	dir := t.TempDir()
	store := NewStore(dir)

	// Create a main session and a chat session
	store.TestAppend("test/imain", provider.Message{Role: "user", Content: provider.TextContent("hi")})
	store.TestAppend("test/c111", provider.Message{Role: "user", Content: provider.TextContent("hi")})

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
	// Proves that ListChatSessions is scoped to the requested agent and does not
	// return sessions belonging to other agents sharing the same store.
	dir := t.TempDir()
	store := NewStore(dir)

	store.TestAppend("alice/c111", provider.Message{Role: "user", Content: provider.TextContent("hi")})
	store.TestAppend("bob/c222", provider.Message{Role: "user", Content: provider.TextContent("hey")})

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

func newAliasTestIndex(t *testing.T) *SessionIndex {
	t.Helper()
	idx, err := NewSessionIndex(t.TempDir() + "/index.db")
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func TestResolveChatAlias(t *testing.T) {
	// An alias on a chat resolves to the chat's DERIVED session key; a
	// 'session_key' adoption-override row (app conversation pointed at a
	// named session) wins over derivation. Matching is case-insensitive and
	// trimmed; unknown aliases and other agents' aliases don't resolve.
	idx := newAliasTestIndex(t)
	if err := idx.SetChatAliasUnique("clutch", "app", 42, "Holiday Plans"); err != nil {
		t.Fatal(err)
	}

	// Case-insensitive, trimmed match — derives the deterministic chat key.
	if got, err := idx.ResolveChatAlias("clutch", "  holiday plans "); err != nil || got != "clutch/c42" {
		t.Fatalf("ResolveChatAlias = %q, %v; want clutch/c42, nil", got, err)
	}
	// An adoption override row wins over derivation.
	if err := idx.SetChatMetadata("clutch", "app", 42, "session_key", "clutch/iholiday"); err != nil {
		t.Fatal(err)
	}
	if got, err := idx.ResolveChatAlias("clutch", "holiday plans"); err != nil || got != "clutch/iholiday" {
		t.Fatalf("ResolveChatAlias (adopted) = %q, %v; want clutch/iholiday, nil", got, err)
	}
	// Unknown alias.
	if _, err := idx.ResolveChatAlias("clutch", "nope"); err != ErrAliasNotFound {
		t.Fatalf("unknown alias err = %v, want ErrAliasNotFound", err)
	}
	// Wrong agent doesn't match.
	if _, err := idx.ResolveChatAlias("scout", "holiday plans"); err != ErrAliasNotFound {
		t.Fatalf("cross-agent err = %v, want ErrAliasNotFound", err)
	}
}

func TestResolveChatAlias_Ambiguous(t *testing.T) {
	// Two chats sharing an alias (only possible via direct writes predating
	// uniqueness) derive distinct keys → ambiguous; but if their adoption
	// overrides point at the SAME session, DISTINCT collapses them to one.
	idx := newAliasTestIndex(t)
	for _, chat := range []int64{1, 2} {
		if err := idx.SetChatMetadata("clutch", "app", chat, "alias", "dupe"); err != nil {
			t.Fatal(err)
		}
	}
	// Distinct derived keys (clutch/c1 vs clutch/c2) → ambiguous.
	if _, err := idx.ResolveChatAlias("clutch", "dupe"); err != ErrAliasAmbiguous {
		t.Fatalf("err = %v, want ErrAliasAmbiguous", err)
	}
	// Both adopted onto the same named session → collapses to one, not ambiguous.
	for _, chat := range []int64{1, 2} {
		if err := idx.SetChatMetadata("clutch", "app", chat, "session_key", "clutch/ix"); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := idx.ResolveChatAlias("clutch", "dupe"); err != nil || got != "clutch/ix" {
		t.Fatalf("same-adoption dupe = %q, %v; want clutch/ix, nil", got, err)
	}
}

func TestSetChatAliasUnique(t *testing.T) {
	idx := newAliasTestIndex(t)
	if err := idx.SetChatAliasUnique("clutch", "app", 1, "work"); err != nil {
		t.Fatal(err)
	}
	// A different chat can't steal it.
	if err := idx.SetChatAliasUnique("clutch", "app", 2, "Work"); err != ErrAliasTaken {
		t.Fatalf("collision err = %v, want ErrAliasTaken", err)
	}
	// The same chat can re-set its own alias.
	if err := idx.SetChatAliasUnique("clutch", "app", 1, "work"); err != nil {
		t.Fatalf("self re-set: %v", err)
	}
	// Clearing releases the alias for another chat.
	if err := idx.SetChatAliasUnique("clutch", "app", 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetChatAliasUnique("clutch", "app", 2, "work"); err != nil {
		t.Fatalf("after clear: %v", err)
	}
	// Reserved characters rejected.
	if err := idx.SetChatAliasUnique("clutch", "app", 3, "a/b"); err == nil {
		t.Fatal("expected rejection of alias with '/'")
	}
}
