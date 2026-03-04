package session

import (
	"path/filepath"
	"testing"
	"time"

	"foci/provider"
)

func tempIndex(t *testing.T) *SessionIndex {
	t.Helper()
	dir := t.TempDir()
	idx, err := NewSessionIndex(filepath.Join(dir, "test_index.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx
}

// Test helper: create new-format session keys
// chatKey("bot", 123) → "bot/c123/1000000000"
func chatKey(agentID string, chatID int64) string {
	return ChatSessionKey(agentID, chatID)
}

// branchKey("bot/c123/1000000000") → "bot/c123/1000000000/b1000000001"
func branchKey(parent string) string {
	k, _ := ParseSessionKey(parent)
	return k.Branch().String()
}

// independentKey("bot") → "bot/i1000000000/1000000000"
func independentKey(agentID string) string {
	return IndependentSessionKey(agentID)
}

func TestSessionIndex_UpsertAndQuery(t *testing.T) {
	idx := tempIndex(t)

	now := time.Now().UTC().Truncate(time.Second)
	idx.Upsert(SessionIndexEntry{
		SessionKey:  "bot/c123/1000000000",
		FilePath:    "/data/sessions/bot/bot/c123/1000000000.jsonl",
		CreatedAt:   now,
		SessionType: SessionTypeChat,
		Status:      SessionStatusActive,
	})
	idx.Upsert(SessionIndexEntry{
		SessionKey:       "bot/i456/456",
		FilePath:         "/data/sessions/bot/bot/i456/456.jsonl",
		CreatedAt:        now.Add(-time.Hour),
		ParentSessionKey: "bot/c123/1000000000",
		SessionType:      SessionTypeSpawn,
		Status:           SessionStatusActive,
	})

count, _ := idx.Count()
	if count != 2 {
count, _ := idx.Count()
		t.Fatalf("expected 2 entries, got %d", count)
	}

	// Query all
	entries, err := idx.Query(QueryOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	// Should be ordered by created_at desc — chat first (newer)
	if entries[0].SessionKey != "bot/c123/1000000000" {
		t.Errorf("expected chat first, got %s", entries[0].SessionKey)
	}
	if entries[1].ParentSessionKey != "bot/c123/1000000000" {
		t.Errorf("expected parent key on spawn, got %q", entries[1].ParentSessionKey)
	}
}

func TestSessionIndex_QueryByType(t *testing.T) {
	idx := tempIndex(t)

	now := time.Now().UTC()
	idx.Upsert(SessionIndexEntry{
		SessionKey: "bot/c1/1000000000", FilePath: "a", CreatedAt: now,
		SessionType: SessionTypeChat, Status: SessionStatusActive,
	})
	idx.Upsert(SessionIndexEntry{
		SessionKey: "bot/i2/2", FilePath: "b", CreatedAt: now,
		SessionType: SessionTypeSpawn, Status: SessionStatusActive,
	})

	entries, err := idx.Query(QueryOptions{SessionType: string(SessionTypeChat)})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 chat entry, got %d", len(entries))
	}
	if entries[0].SessionKey != "bot/c1/1000000000" {
		t.Errorf("wrong entry: %s", entries[0].SessionKey)
	}
}

func TestSessionIndex_QueryByStatus(t *testing.T) {
	idx := tempIndex(t)

	now := time.Now().UTC()
	idx.Upsert(SessionIndexEntry{
		SessionKey: "bot/c1/1000000000", FilePath: "a", CreatedAt: now,
		SessionType: SessionTypeChat, Status: SessionStatusActive,
	})
	idx.Upsert(SessionIndexEntry{
		SessionKey: "bot/c2/1000000000", FilePath: "b", CreatedAt: now,
		SessionType: SessionTypeChat, Status: SessionStatusCompacted,
	})

	entries, err := idx.Query(QueryOptions{Status: string(SessionStatusActive)})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 active entry, got %d", len(entries))
	}
}

func TestSessionIndex_QueryByAgent(t *testing.T) {
	idx := tempIndex(t)

	now := time.Now().UTC()
	idx.Upsert(SessionIndexEntry{
		SessionKey: "alpha/c1/1000000000", FilePath: "a", CreatedAt: now,
		SessionType: SessionTypeChat, Status: SessionStatusActive,
	})
	idx.Upsert(SessionIndexEntry{
		SessionKey: "beta/c2/1000000000", FilePath: "b", CreatedAt: now,
		SessionType: SessionTypeChat, Status: SessionStatusActive,
	})

	entries, err := idx.Query(QueryOptions{AgentID: "alpha"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry for alpha, got %d", len(entries))
	}
	if entries[0].SessionKey != "alpha/c1/1000000000" {
		t.Errorf("wrong entry: %s", entries[0].SessionKey)
	}
}

func TestSessionIndex_QueryLimit(t *testing.T) {
	idx := tempIndex(t)

	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		idx.Upsert(SessionIndexEntry{
			SessionKey: "agent:bot:chat:" + string(rune('a'+i)), FilePath: "f",
			CreatedAt: now.Add(time.Duration(i) * time.Minute),
			SessionType: SessionTypeChat, Status: SessionStatusActive,
		})
	}

	entries, err := idx.Query(QueryOptions{Limit: 3})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries with limit, got %d", len(entries))
	}
}

func TestSessionIndex_SetStatus(t *testing.T) {
	idx := tempIndex(t)

	idx.Upsert(SessionIndexEntry{
		SessionKey: "bot/c1/1000000000", FilePath: "a",
		CreatedAt: time.Now().UTC(), SessionType: SessionTypeChat,
		Status: SessionStatusActive,
	})

	idx.SetStatus("bot/c1/1000000000", SessionStatusCompacted)

	entries, err := idx.Query(QueryOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 || entries[0].Status != SessionStatusCompacted {
		t.Errorf("expected compacted status, got %v", entries)
	}
}

func TestSessionIndex_Delete(t *testing.T) {
	idx := tempIndex(t)

	idx.Upsert(SessionIndexEntry{
		SessionKey: "bot/c1/1000000000", FilePath: "a",
		CreatedAt: time.Now().UTC(), SessionType: SessionTypeChat,
		Status: SessionStatusActive,
	})

	idx.Delete("bot/c1/1000000000")

count, _ := idx.Count()
	if count != 0 {
count, _ := idx.Count()
		t.Fatalf("expected 0 after delete, got %d", count)
	}
}

func TestSessionIndex_Upsert_Replaces(t *testing.T) {
	idx := tempIndex(t)

	now := time.Now().UTC()
	idx.Upsert(SessionIndexEntry{
		SessionKey: "bot/c1/1000000000", FilePath: "a",
		CreatedAt: now, SessionType: SessionTypeChat,
		Status: SessionStatusActive,
	})
	idx.Upsert(SessionIndexEntry{
		SessionKey: "bot/c1/1000000000", FilePath: "a",
		CreatedAt: now, SessionType: SessionTypeChat,
		Status: SessionStatusCompacted,
	})

count, _ := idx.Count()
	if count != 1 {
count, _ := idx.Count()
		t.Fatalf("upsert should replace, got %d entries", count)
	}
	entries, _ := idx.Query(QueryOptions{})
	if entries[0].Status != SessionStatusCompacted {
		t.Errorf("expected compacted after upsert, got %s", entries[0].Status)
	}
}

func TestClassifySessionKey(t *testing.T) {
	tests := []struct {
		key  string
		want SessionType
	}{
		{"bot/c123/1000000000", SessionTypeChat},
		{"bot/i123/123", SessionTypeMultiball},
		{"bot/i123/123", SessionTypeSpawn},
		{"bot/i123/123", SessionTypeCron},
		{"bot/c123/1000000000/b456", SessionTypeBranch},
		{"bot/c123/1000000000/b456", SessionTypeBranch},
		{"agent:bot:unknown:thing", SessionTypeUnknown},
		{"bad", SessionTypeUnknown},
	}
	for _, tt := range tests {
		got := ClassifySessionKey(tt.key)
		if got != tt.want {
			t.Errorf("ClassifySessionKey(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

func TestSessionIndex_Rebuild(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	// Create a few sessions
	store.Append("bot/c100/1000000000", msg("user", "hello"))
	store.Append("bot/c200/1000000000", msg("user", "world"))
	store.CreateBranchWithOptions("bot/c100/1000000000", "bot/i1/1", BranchOptions{})

	// Create index and rebuild
	idx := tempIndex(t)
	n, err := idx.Rebuild(store)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 sessions from rebuild, got %d", n)
	}
count, _ := idx.Count()
	if count != 3 {
count, _ := idx.Count()
		t.Fatalf("expected 3 in index, got %d", count)
	}

	// Verify types
	entries, _ := idx.Query(QueryOptions{SessionType: string(SessionTypeChat)})
	if len(entries) != 2 {
		t.Errorf("expected 2 chat sessions, got %d", len(entries))
	}
	entries, _ = idx.Query(QueryOptions{SessionType: string(SessionTypeMultiball)})
	if len(entries) != 1 {
		t.Errorf("expected 1 multiball session, got %d", len(entries))
	}

	// Verify parent key on multiball
	if entries[0].ParentSessionKey != "bot/c100/1000000000" {
		t.Errorf("expected parent key agent:bot:chat:100, got %q", entries[0].ParentSessionKey)
	}
}

func TestSessionIndex_EventFiring(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	idx := tempIndex(t)

	// Wire up events
	store.OnSessionEvent(func(e SessionEvent) {
		switch e.Status {
		case SessionStatusActive:
			idx.Upsert(SessionIndexEntry{
				SessionKey:       e.Key,
				FilePath:         e.FilePath,
				CreatedAt:        e.CreatedAt,
				ParentSessionKey: e.ParentKey,
				SessionType:      e.Type,
				Status:           SessionStatusActive,
			})
		case SessionStatusCompacted:
			idx.SetStatus(e.Key, SessionStatusCompacted)
		case SessionStatusCleared:
			idx.SetStatus(e.Key, SessionStatusCleared)
		}
	})

	// Create a session via Append (new file triggers event)
	store.Append("bot/c100/1000000000", msg("user", "hello"))
count, _ := idx.Count()
	if count != 1 {
count, _ := idx.Count()
		t.Fatalf("expected 1 after create, got %d", count)
	}

	entries, _ := idx.Query(QueryOptions{})
	if entries[0].SessionType != SessionTypeChat {
		t.Errorf("expected chat type, got %s", entries[0].SessionType)
	}
	if entries[0].Status != SessionStatusActive {
		t.Errorf("expected active status, got %s", entries[0].Status)
	}

	// Append again should NOT fire another create event
	store.Append("bot/c100/1000000000", msg("assistant", "hi"))
	if count != 1 {
count, _ := idx.Count()
		t.Fatalf("expected still 1 after second append, got %d", count)
	}

	// Replace should fire compacted event
	store.Replace("bot/c100/1000000000", []provider.Message{msg("user", "compacted")})
	entries, _ = idx.Query(QueryOptions{})
	if entries[0].Status != SessionStatusCompacted {
		t.Errorf("expected compacted after Replace, got %s", entries[0].Status)
	}

	// Clear should fire cleared event
	store.Clear("bot/c100/1000000000")
	entries, _ = idx.Query(QueryOptions{})
	if entries[0].Status != SessionStatusCleared {
		t.Errorf("expected cleared after Clear, got %s", entries[0].Status)
	}
}

func TestSessionIndex_BranchEventFiring(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	idx := tempIndex(t)

	store.OnSessionEvent(func(e SessionEvent) {
		if e.Status == SessionStatusActive {
			idx.Upsert(SessionIndexEntry{
				SessionKey:       e.Key,
				FilePath:         e.FilePath,
				CreatedAt:        e.CreatedAt,
				ParentSessionKey: e.ParentKey,
				SessionType:      e.Type,
				Status:           SessionStatusActive,
			})
		}
	})

	// Create parent
	store.Append("bot/c100/1000000000", msg("user", "hello"))

	// Create branch
	store.CreateBranchWithOptions("bot/c100/1000000000", "bot/i1/1", BranchOptions{})
count, _ := idx.Count()
	if count != 2 {
count, _ := idx.Count()
		t.Fatalf("expected 2 after branch create, got %d", count)
	}

	entries, _ := idx.Query(QueryOptions{SessionType: string(SessionTypeMultiball)})
	if len(entries) != 1 {
		t.Fatalf("expected 1 multiball, got %d", len(entries))
	}
	if entries[0].ParentSessionKey != "bot/c100/1000000000" {
		t.Errorf("expected parent key, got %q", entries[0].ParentSessionKey)
	}
}

func TestSessionIndex_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persist.db")

	idx1, err := NewSessionIndex(dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	idx1.Upsert(SessionIndexEntry{
		SessionKey: "bot/c1/1000000000", FilePath: "a",
		CreatedAt: time.Now().UTC(), SessionType: SessionTypeChat,
		Status: SessionStatusActive,
	})
	idx1.Close()

	idx2, err := NewSessionIndex(dbPath)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer idx2.Close()

	count2, _ := idx2.Count()
	if count2 != 1 {
		t.Fatalf("expected 1 after reopen, got %d", count2)
	}
}
