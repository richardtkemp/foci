package session

import (
	"path/filepath"
	"testing"
	"time"

	"foci/internal/provider"
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
	return NewChatSessionKey(agentID, chatID)
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
		{"bot/i123/123", SessionTypeUnknown},                // independent — can't distinguish multiball/spawn/cron from key alone
		{"bot/c123/1000000000/b456", SessionTypeBranch},     // branch child type
		{"bot/i123/1000000000/b456", SessionTypeBranch},     // branch from independent parent
		{"bot/c123/1000000000/i456", SessionTypeUnknown},    // independent spawn child
		{"agent:bot:unknown:thing", SessionTypeUnknown},     // old format / unparseable
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
	branchKey := "bot/c100/1000000000/b1000000001"
	store.CreateBranchWithOptions("bot/c100/1000000000", branchKey, BranchOptions{})

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
		t.Fatalf("expected 3 in index, got %d", count)
	}

	// Verify types
	entries, _ := idx.Query(QueryOptions{SessionType: string(SessionTypeChat)})
	if len(entries) != 2 {
		t.Errorf("expected 2 chat sessions, got %d", len(entries))
	}
	entries, _ = idx.Query(QueryOptions{SessionType: string(SessionTypeBranch)})
	if len(entries) != 1 {
		t.Errorf("expected 1 branch session, got %d", len(entries))
	}

	// Verify parent key on branch
	if entries[0].ParentSessionKey != "bot/c100/1000000000" {
		t.Errorf("expected parent key bot/c100/1000000000, got %q", entries[0].ParentSessionKey)
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
	store.CreateBranchWithOptions("bot/c100/1000000000", "bot/c100/1000000000/b1000000001", BranchOptions{})
	count, _ := idx.Count()
	if count != 2 {
		t.Fatalf("expected 2 after branch create, got %d", count)
	}

	entries, _ := idx.Query(QueryOptions{SessionType: string(SessionTypeBranch)})
	if len(entries) != 1 {
		t.Fatalf("expected 1 branch, got %d", len(entries))
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

// ========== Count tests ==========

func TestSessionIndex_Count_Empty(t *testing.T) {
	idx := tempIndex(t)

	count, err := idx.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 on empty index, got %d", count)
	}
}

func TestSessionIndex_Count_ReflectsInsertionsAndDeletions(t *testing.T) {
	idx := tempIndex(t)
	now := time.Now().UTC()

	// Insert 3 sessions
	for i, key := range []string{"a/c1/1000000000", "b/c2/1000000000", "c/c3/1000000000"} {
		idx.Upsert(SessionIndexEntry{
			SessionKey:  key,
			FilePath:    "f",
			CreatedAt:   now.Add(time.Duration(i) * time.Minute),
			SessionType: SessionTypeChat,
			Status:      SessionStatusActive,
		})
	}

	count, _ := idx.Count()
	if count != 3 {
		t.Fatalf("expected 3 after inserts, got %d", count)
	}

	// Delete one
	idx.Delete("b/c2/1000000000")
	count, _ = idx.Count()
	if count != 2 {
		t.Fatalf("expected 2 after delete, got %d", count)
	}

	// Upsert existing (should not increase count)
	idx.Upsert(SessionIndexEntry{
		SessionKey:  "a/c1/1000000000",
		FilePath:    "f-updated",
		CreatedAt:   now,
		SessionType: SessionTypeChat,
		Status:      SessionStatusCompacted,
	})
	count, _ = idx.Count()
	if count != 2 {
		t.Fatalf("expected still 2 after upsert of existing key, got %d", count)
	}
}

// ========== Agent Metadata tests ==========

func TestAgentMetadata_SetAndGet(t *testing.T) {
	idx := tempIndex(t)

	if err := idx.SetAgentMetadata("bot1", "model", "claude-3"); err != nil {
		t.Fatalf("SetAgentMetadata: %v", err)
	}

	val, err := idx.GetAgentMetadata("bot1", "model")
	if err != nil {
		t.Fatalf("GetAgentMetadata: %v", err)
	}
	if val != "claude-3" {
		t.Errorf("got %q, want %q", val, "claude-3")
	}
}

func TestAgentMetadata_GetMissing(t *testing.T) {
	idx := tempIndex(t)

	val, err := idx.GetAgentMetadata("nonexistent", "key")
	if err != nil {
		t.Fatalf("GetAgentMetadata: %v", err)
	}
	if val != "" {
		t.Errorf("expected empty string for missing key, got %q", val)
	}
}

func TestAgentMetadata_Upsert(t *testing.T) {
	idx := tempIndex(t)

	idx.SetAgentMetadata("bot1", "model", "claude-2")
	idx.SetAgentMetadata("bot1", "model", "claude-3")

	val, _ := idx.GetAgentMetadata("bot1", "model")
	if val != "claude-3" {
		t.Errorf("upsert should overwrite: got %q, want %q", val, "claude-3")
	}
}

func TestAgentMetadata_Delete(t *testing.T) {
	idx := tempIndex(t)

	idx.SetAgentMetadata("bot1", "model", "claude-3")
	if err := idx.DeleteAgentMetadata("bot1", "model"); err != nil {
		t.Fatalf("DeleteAgentMetadata: %v", err)
	}

	val, _ := idx.GetAgentMetadata("bot1", "model")
	if val != "" {
		t.Errorf("expected empty after delete, got %q", val)
	}
}

func TestAgentMetadata_DeleteNonexistent(t *testing.T) {
	idx := tempIndex(t)

	// Should not error when deleting a key that doesn't exist
	if err := idx.DeleteAgentMetadata("ghost", "key"); err != nil {
		t.Fatalf("DeleteAgentMetadata on missing key: %v", err)
	}
}

func TestAgentMetadata_IsolationBetweenAgents(t *testing.T) {
	idx := tempIndex(t)

	idx.SetAgentMetadata("bot1", "model", "claude-3")
	idx.SetAgentMetadata("bot2", "model", "gpt-4")

	v1, _ := idx.GetAgentMetadata("bot1", "model")
	v2, _ := idx.GetAgentMetadata("bot2", "model")

	if v1 != "claude-3" {
		t.Errorf("bot1 model = %q, want %q", v1, "claude-3")
	}
	if v2 != "gpt-4" {
		t.Errorf("bot2 model = %q, want %q", v2, "gpt-4")
	}
}

func TestAgentMetadata_MultipleKeys(t *testing.T) {
	idx := tempIndex(t)

	idx.SetAgentMetadata("bot1", "model", "claude-3")
	idx.SetAgentMetadata("bot1", "effort", "high")
	idx.SetAgentMetadata("bot1", "voice", "enabled")

	v1, _ := idx.GetAgentMetadata("bot1", "model")
	v2, _ := idx.GetAgentMetadata("bot1", "effort")
	v3, _ := idx.GetAgentMetadata("bot1", "voice")

	if v1 != "claude-3" || v2 != "high" || v3 != "enabled" {
		t.Errorf("multiple keys: model=%q effort=%q voice=%q", v1, v2, v3)
	}

	// Delete one, others remain
	idx.DeleteAgentMetadata("bot1", "effort")
	v2, _ = idx.GetAgentMetadata("bot1", "effort")
	v1, _ = idx.GetAgentMetadata("bot1", "model")
	if v2 != "" {
		t.Errorf("deleted key should be empty, got %q", v2)
	}
	if v1 != "claude-3" {
		t.Errorf("other keys should be unaffected, got %q", v1)
	}
}

// ========== Chat Metadata tests ==========

func TestChatMetadata_SetAndGet(t *testing.T) {
	idx := tempIndex(t)

	if err := idx.SetChatMetadata("bot1", 42, "effort", "high"); err != nil {
		t.Fatalf("SetChatMetadata: %v", err)
	}

	val, err := idx.GetChatMetadata("bot1", 42, "effort")
	if err != nil {
		t.Fatalf("GetChatMetadata: %v", err)
	}
	if val != "high" {
		t.Errorf("got %q, want %q", val, "high")
	}
}

func TestChatMetadata_GetMissing(t *testing.T) {
	idx := tempIndex(t)

	val, err := idx.GetChatMetadata("bot", 999, "key")
	if err != nil {
		t.Fatalf("GetChatMetadata: %v", err)
	}
	if val != "" {
		t.Errorf("expected empty string for missing key, got %q", val)
	}
}

func TestChatMetadata_Upsert(t *testing.T) {
	idx := tempIndex(t)

	idx.SetChatMetadata("bot1", 42, "model", "old")
	idx.SetChatMetadata("bot1", 42, "model", "new")

	val, _ := idx.GetChatMetadata("bot1", 42, "model")
	if val != "new" {
		t.Errorf("upsert should overwrite: got %q, want %q", val, "new")
	}
}

func TestChatMetadata_Delete(t *testing.T) {
	idx := tempIndex(t)

	idx.SetChatMetadata("bot1", 42, "model", "claude")
	if err := idx.DeleteChatMetadata("bot1", 42, "model"); err != nil {
		t.Fatalf("DeleteChatMetadata: %v", err)
	}

	val, _ := idx.GetChatMetadata("bot1", 42, "model")
	if val != "" {
		t.Errorf("expected empty after delete, got %q", val)
	}
}

func TestChatMetadata_IsolationBetweenChats(t *testing.T) {
	idx := tempIndex(t)

	// Same agent, different chats
	idx.SetChatMetadata("bot1", 1, "model", "claude")
	idx.SetChatMetadata("bot1", 2, "model", "gpt")

	v1, _ := idx.GetChatMetadata("bot1", 1, "model")
	v2, _ := idx.GetChatMetadata("bot1", 2, "model")

	if v1 != "claude" || v2 != "gpt" {
		t.Errorf("chat isolation failed: chat1=%q chat2=%q", v1, v2)
	}

	// Different agents, same chat ID
	idx.SetChatMetadata("bot2", 1, "model", "gemini")
	v3, _ := idx.GetChatMetadata("bot2", 1, "model")
	v1again, _ := idx.GetChatMetadata("bot1", 1, "model")

	if v3 != "gemini" || v1again != "claude" {
		t.Errorf("agent isolation failed: bot2=%q bot1=%q", v3, v1again)
	}
}

func TestChatMetadata_DeleteNonexistent(t *testing.T) {
	idx := tempIndex(t)

	if err := idx.DeleteChatMetadata("ghost", 0, "key"); err != nil {
		t.Fatalf("DeleteChatMetadata on missing key: %v", err)
	}
}

// ========== Session Metadata tests ==========

func TestSessionMetadata_SetAndGet(t *testing.T) {
	idx := tempIndex(t)

	if err := idx.SetSessionMetadata("bot/c1/1000000000", "no_compact", "true"); err != nil {
		t.Fatalf("SetSessionMetadata: %v", err)
	}

	val, err := idx.GetSessionMetadata("bot/c1/1000000000", "no_compact")
	if err != nil {
		t.Fatalf("GetSessionMetadata: %v", err)
	}
	if val != "true" {
		t.Errorf("got %q, want %q", val, "true")
	}
}

func TestSessionMetadata_GetMissing(t *testing.T) {
	idx := tempIndex(t)

	val, err := idx.GetSessionMetadata("nonexistent/session", "key")
	if err != nil {
		t.Fatalf("GetSessionMetadata: %v", err)
	}
	if val != "" {
		t.Errorf("expected empty string for missing key, got %q", val)
	}
}

func TestSessionMetadata_Upsert(t *testing.T) {
	idx := tempIndex(t)

	idx.SetSessionMetadata("bot/c1/1000000000", "no_compact", "false")
	idx.SetSessionMetadata("bot/c1/1000000000", "no_compact", "true")

	val, _ := idx.GetSessionMetadata("bot/c1/1000000000", "no_compact")
	if val != "true" {
		t.Errorf("upsert should overwrite: got %q, want %q", val, "true")
	}
}

func TestSessionMetadata_Delete(t *testing.T) {
	idx := tempIndex(t)

	idx.SetSessionMetadata("bot/c1/1000000000", "no_compact", "true")
	if err := idx.DeleteSessionMetadata("bot/c1/1000000000", "no_compact"); err != nil {
		t.Fatalf("DeleteSessionMetadata: %v", err)
	}

	val, _ := idx.GetSessionMetadata("bot/c1/1000000000", "no_compact")
	if val != "" {
		t.Errorf("expected empty after delete, got %q", val)
	}
}

func TestSessionMetadata_IsolationBetweenSessions(t *testing.T) {
	idx := tempIndex(t)

	idx.SetSessionMetadata("bot/c1/1000000000", "no_compact", "true")
	idx.SetSessionMetadata("bot/c2/1000000000", "no_compact", "false")

	v1, _ := idx.GetSessionMetadata("bot/c1/1000000000", "no_compact")
	v2, _ := idx.GetSessionMetadata("bot/c2/1000000000", "no_compact")

	if v1 != "true" || v2 != "false" {
		t.Errorf("session isolation failed: s1=%q s2=%q", v1, v2)
	}
}

func TestSessionMetadata_DeleteNonexistent(t *testing.T) {
	idx := tempIndex(t)

	if err := idx.DeleteSessionMetadata("ghost/session", "key"); err != nil {
		t.Fatalf("DeleteSessionMetadata on missing key: %v", err)
	}
}

// ========== System State tests ==========

func TestSystemState_SetAndGet(t *testing.T) {
	idx := tempIndex(t)

	if err := idx.SetSystemState("migration_done", "true"); err != nil {
		t.Fatalf("SetSystemState: %v", err)
	}

	val, err := idx.GetSystemState("migration_done")
	if err != nil {
		t.Fatalf("GetSystemState: %v", err)
	}
	if val != "true" {
		t.Errorf("got %q, want %q", val, "true")
	}
}

func TestSystemState_GetMissing(t *testing.T) {
	idx := tempIndex(t)

	val, err := idx.GetSystemState("nonexistent")
	if err != nil {
		t.Fatalf("GetSystemState: %v", err)
	}
	if val != "" {
		t.Errorf("expected empty string for missing key, got %q", val)
	}
}

func TestSystemState_Upsert(t *testing.T) {
	idx := tempIndex(t)

	idx.SetSystemState("version", "1")
	idx.SetSystemState("version", "2")

	val, _ := idx.GetSystemState("version")
	if val != "2" {
		t.Errorf("upsert should overwrite: got %q, want %q", val, "2")
	}
}

func TestSystemState_Delete(t *testing.T) {
	idx := tempIndex(t)

	idx.SetSystemState("temp_key", "value")
	if err := idx.DeleteSystemState("temp_key"); err != nil {
		t.Fatalf("DeleteSystemState: %v", err)
	}

	val, _ := idx.GetSystemState("temp_key")
	if val != "" {
		t.Errorf("expected empty after delete, got %q", val)
	}
}

func TestSystemState_DeleteNonexistent(t *testing.T) {
	idx := tempIndex(t)

	if err := idx.DeleteSystemState("ghost"); err != nil {
		t.Fatalf("DeleteSystemState on missing key: %v", err)
	}
}

func TestSystemState_MultipleKeys(t *testing.T) {
	idx := tempIndex(t)

	idx.SetSystemState("key1", "val1")
	idx.SetSystemState("key2", "val2")
	idx.SetSystemState("key3", "val3")

	v1, _ := idx.GetSystemState("key1")
	v2, _ := idx.GetSystemState("key2")
	v3, _ := idx.GetSystemState("key3")

	if v1 != "val1" || v2 != "val2" || v3 != "val3" {
		t.Errorf("multiple keys: k1=%q k2=%q k3=%q", v1, v2, v3)
	}

	idx.DeleteSystemState("key2")
	v2, _ = idx.GetSystemState("key2")
	v1, _ = idx.GetSystemState("key1")
	if v2 != "" {
		t.Errorf("deleted key should be empty, got %q", v2)
	}
	if v1 != "val1" {
		t.Errorf("other key should be unaffected, got %q", v1)
	}
}

// ========== Metadata persistence across reopen ==========

func TestMetadata_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persist.db")

	idx1, err := NewSessionIndex(dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	idx1.SetAgentMetadata("bot1", "model", "claude-3")
	idx1.SetChatMetadata("bot1", 42, "effort", "high")
	idx1.SetSessionMetadata("bot/c1/1000000000", "no_compact", "true")
	idx1.SetSystemState("version", "1")
	idx1.Close()

	idx2, err := NewSessionIndex(dbPath)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer idx2.Close()

	if v, _ := idx2.GetAgentMetadata("bot1", "model"); v != "claude-3" {
		t.Errorf("agent metadata not persisted: got %q", v)
	}
	if v, _ := idx2.GetChatMetadata("bot1", 42, "effort"); v != "high" {
		t.Errorf("chat metadata not persisted: got %q", v)
	}
	if v, _ := idx2.GetSessionMetadata("bot/c1/1000000000", "no_compact"); v != "true" {
		t.Errorf("session metadata not persisted: got %q", v)
	}
	if v, _ := idx2.GetSystemState("version"); v != "1" {
		t.Errorf("system state not persisted: got %q", v)
	}
}

// ========== Metadata tables don't interfere with session index ==========

func TestMetadata_IndependentOfSessionIndex(t *testing.T) {
	idx := tempIndex(t)

	// Metadata operations should work with an empty session index
	idx.SetAgentMetadata("bot1", "key", "val")
	idx.SetSystemState("sys_key", "sys_val")

	count, _ := idx.Count()
	if count != 0 {
		t.Errorf("metadata shouldn't affect session count: got %d", count)
	}

	// Session operations shouldn't affect metadata
	idx.Upsert(SessionIndexEntry{
		SessionKey:  "bot/c1/1000000000",
		FilePath:    "f",
		CreatedAt:   time.Now().UTC(),
		SessionType: SessionTypeChat,
		Status:      SessionStatusActive,
	})

	v, _ := idx.GetAgentMetadata("bot1", "key")
	if v != "val" {
		t.Errorf("session upsert affected agent metadata: got %q", v)
	}
	v, _ = idx.GetSystemState("sys_key")
	if v != "sys_val" {
		t.Errorf("session upsert affected system state: got %q", v)
	}
}
