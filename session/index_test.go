package session

import (
	"path/filepath"
	"testing"
	"time"

	"foci/anthropic"
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

func TestSessionIndex_UpsertAndQuery(t *testing.T) {
	idx := tempIndex(t)

	now := time.Now().UTC().Truncate(time.Second)
	idx.Upsert(SessionIndexEntry{
		SessionKey:  "agent:bot:chat:123",
		FilePath:    "/data/sessions/agent/bot/chat/123.jsonl",
		CreatedAt:   now,
		SessionType: SessionTypeChat,
		Status:      SessionStatusActive,
	})
	idx.Upsert(SessionIndexEntry{
		SessionKey:       "agent:bot:spawn:spawn-456",
		FilePath:         "/data/sessions/agent/bot/spawn/spawn-456.jsonl",
		CreatedAt:        now.Add(-time.Hour),
		ParentSessionKey: "agent:bot:chat:123",
		SessionType:      SessionTypeSpawn,
		Status:           SessionStatusActive,
	})

	if idx.Count() != 2 {
		t.Fatalf("expected 2 entries, got %d", idx.Count())
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
	if entries[0].SessionKey != "agent:bot:chat:123" {
		t.Errorf("expected chat first, got %s", entries[0].SessionKey)
	}
	if entries[1].ParentSessionKey != "agent:bot:chat:123" {
		t.Errorf("expected parent key on spawn, got %q", entries[1].ParentSessionKey)
	}
}

func TestSessionIndex_QueryByType(t *testing.T) {
	idx := tempIndex(t)

	now := time.Now().UTC()
	idx.Upsert(SessionIndexEntry{
		SessionKey: "agent:bot:chat:1", FilePath: "a", CreatedAt: now,
		SessionType: SessionTypeChat, Status: SessionStatusActive,
	})
	idx.Upsert(SessionIndexEntry{
		SessionKey: "agent:bot:spawn:2", FilePath: "b", CreatedAt: now,
		SessionType: SessionTypeSpawn, Status: SessionStatusActive,
	})

	entries, err := idx.Query(QueryOptions{SessionType: SessionTypeChat})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 chat entry, got %d", len(entries))
	}
	if entries[0].SessionKey != "agent:bot:chat:1" {
		t.Errorf("wrong entry: %s", entries[0].SessionKey)
	}
}

func TestSessionIndex_QueryByStatus(t *testing.T) {
	idx := tempIndex(t)

	now := time.Now().UTC()
	idx.Upsert(SessionIndexEntry{
		SessionKey: "agent:bot:chat:1", FilePath: "a", CreatedAt: now,
		SessionType: SessionTypeChat, Status: SessionStatusActive,
	})
	idx.Upsert(SessionIndexEntry{
		SessionKey: "agent:bot:chat:2", FilePath: "b", CreatedAt: now,
		SessionType: SessionTypeChat, Status: SessionStatusCompacted,
	})

	entries, err := idx.Query(QueryOptions{Status: SessionStatusActive})
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
		SessionKey: "agent:alpha:chat:1", FilePath: "a", CreatedAt: now,
		SessionType: SessionTypeChat, Status: SessionStatusActive,
	})
	idx.Upsert(SessionIndexEntry{
		SessionKey: "agent:beta:chat:2", FilePath: "b", CreatedAt: now,
		SessionType: SessionTypeChat, Status: SessionStatusActive,
	})

	entries, err := idx.Query(QueryOptions{AgentID: "alpha"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry for alpha, got %d", len(entries))
	}
	if entries[0].SessionKey != "agent:alpha:chat:1" {
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
		SessionKey: "agent:bot:chat:1", FilePath: "a",
		CreatedAt: time.Now().UTC(), SessionType: SessionTypeChat,
		Status: SessionStatusActive,
	})

	idx.SetStatus("agent:bot:chat:1", SessionStatusCompacted)

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
		SessionKey: "agent:bot:chat:1", FilePath: "a",
		CreatedAt: time.Now().UTC(), SessionType: SessionTypeChat,
		Status: SessionStatusActive,
	})

	idx.Delete("agent:bot:chat:1")

	if idx.Count() != 0 {
		t.Fatalf("expected 0 after delete, got %d", idx.Count())
	}
}

func TestSessionIndex_Upsert_Replaces(t *testing.T) {
	idx := tempIndex(t)

	now := time.Now().UTC()
	idx.Upsert(SessionIndexEntry{
		SessionKey: "agent:bot:chat:1", FilePath: "a",
		CreatedAt: now, SessionType: SessionTypeChat,
		Status: SessionStatusActive,
	})
	idx.Upsert(SessionIndexEntry{
		SessionKey: "agent:bot:chat:1", FilePath: "a",
		CreatedAt: now, SessionType: SessionTypeChat,
		Status: SessionStatusCompacted,
	})

	if idx.Count() != 1 {
		t.Fatalf("upsert should replace, got %d entries", idx.Count())
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
		{"agent:bot:chat:123", SessionTypeChat},
		{"agent:bot:multiball:mb-123", SessionTypeMultiball},
		{"agent:bot:spawn:spawn-123", SessionTypeSpawn},
		{"agent:bot:cron:keepalive-123", SessionTypeCron},
		{"agent:bot:multiball:mb-123:branch:session-end-456", SessionTypeBranch},
		{"agent:bot:chat:123:branch:session-end-456", SessionTypeBranch},
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
	store.Append("agent:bot:chat:100", msg("user", "hello"))
	store.Append("agent:bot:chat:200", msg("user", "world"))
	store.CreateBranchWithOptions("agent:bot:chat:100", "agent:bot:multiball:mb-1", BranchOptions{})

	// Create index and rebuild
	idx := tempIndex(t)
	n, err := idx.Rebuild(store)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 sessions from rebuild, got %d", n)
	}
	if idx.Count() != 3 {
		t.Fatalf("expected 3 in index, got %d", idx.Count())
	}

	// Verify types
	entries, _ := idx.Query(QueryOptions{SessionType: SessionTypeChat})
	if len(entries) != 2 {
		t.Errorf("expected 2 chat sessions, got %d", len(entries))
	}
	entries, _ = idx.Query(QueryOptions{SessionType: SessionTypeMultiball})
	if len(entries) != 1 {
		t.Errorf("expected 1 multiball session, got %d", len(entries))
	}

	// Verify parent key on multiball
	if entries[0].ParentSessionKey != "agent:bot:chat:100" {
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
	store.Append("agent:bot:chat:100", msg("user", "hello"))
	if idx.Count() != 1 {
		t.Fatalf("expected 1 after create, got %d", idx.Count())
	}

	entries, _ := idx.Query(QueryOptions{})
	if entries[0].SessionType != SessionTypeChat {
		t.Errorf("expected chat type, got %s", entries[0].SessionType)
	}
	if entries[0].Status != SessionStatusActive {
		t.Errorf("expected active status, got %s", entries[0].Status)
	}

	// Append again should NOT fire another create event
	store.Append("agent:bot:chat:100", msg("assistant", "hi"))
	if idx.Count() != 1 {
		t.Fatalf("expected still 1 after second append, got %d", idx.Count())
	}

	// Replace should fire compacted event
	store.Replace("agent:bot:chat:100", []anthropic.Message{msg("user", "compacted")})
	entries, _ = idx.Query(QueryOptions{})
	if entries[0].Status != SessionStatusCompacted {
		t.Errorf("expected compacted after Replace, got %s", entries[0].Status)
	}

	// Clear should fire cleared event
	store.Clear("agent:bot:chat:100")
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
	store.Append("agent:bot:chat:100", msg("user", "hello"))

	// Create branch
	store.CreateBranchWithOptions("agent:bot:chat:100", "agent:bot:multiball:mb-1", BranchOptions{})
	if idx.Count() != 2 {
		t.Fatalf("expected 2 after branch create, got %d", idx.Count())
	}

	entries, _ := idx.Query(QueryOptions{SessionType: SessionTypeMultiball})
	if len(entries) != 1 {
		t.Fatalf("expected 1 multiball, got %d", len(entries))
	}
	if entries[0].ParentSessionKey != "agent:bot:chat:100" {
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
		SessionKey: "agent:bot:chat:1", FilePath: "a",
		CreatedAt: time.Now().UTC(), SessionType: SessionTypeChat,
		Status: SessionStatusActive,
	})
	idx1.Close()

	idx2, err := NewSessionIndex(dbPath)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer idx2.Close()

	if idx2.Count() != 1 {
		t.Fatalf("expected 1 after reopen, got %d", idx2.Count())
	}
}
