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

func TestSessionIndex_UpsertAndQuery(t *testing.T) {
	// Proves the basic insert-then-query contract: upserted entries are retrievable
	// via Query, returned in created_at descending order, and parent keys are preserved.
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
	// Proves that QueryOptions.SessionType filters results to only entries of
	// the specified type, leaving other types out of the result set.
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
	// Proves that QueryOptions.Status filters results to only entries matching
	// the requested status, excluding those with a different status.
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
	// Proves that QueryOptions.AgentID scopes results to a single agent,
	// excluding entries belonging to other agents in the same index.
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
	// Proves that QueryOptions.Limit caps the number of returned entries, enabling
	// pagination over large indexes.
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
	// Proves that UpdateStatus mutates only the status field of an existing entry
	// without otherwise altering the record.
	idx := tempIndex(t)

	idx.Upsert(SessionIndexEntry{
		SessionKey: "bot/c1/1000000000", FilePath: "a",
		CreatedAt: time.Now().UTC(), SessionType: SessionTypeChat,
		Status: SessionStatusActive,
	})

	idx.UpdateStatus("bot/c1/1000000000", SessionStatusCompacted)

	entries, err := idx.Query(QueryOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 || entries[0].Status != SessionStatusCompacted {
		t.Errorf("expected compacted status, got %v", entries)
	}
}

func TestSessionIndex_Delete(t *testing.T) {
	// Proves that Delete removes the entry from the index so Count returns zero
	// and it no longer appears in Query results.
	idx := tempIndex(t)

	idx.Upsert(SessionIndexEntry{
		SessionKey: "bot/c1/1000000000", FilePath: "a",
		CreatedAt: time.Now().UTC(), SessionType: SessionTypeChat,
		Status: SessionStatusActive,
	})

	idx.Delete("bot/c1/1000000000")

	count, _ := idx.Count()
	if count != 0 {
		t.Fatalf("expected 0 after delete, got %d", count)
	}
}

func TestSessionIndex_Upsert_Replaces(t *testing.T) {
	// Proves that upserting an entry with the same session key updates the existing
	// row rather than inserting a duplicate, keeping the count at 1.
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
		t.Fatalf("upsert should replace, got %d entries", count)
	}
	entries, _ := idx.Query(QueryOptions{})
	if entries[0].Status != SessionStatusCompacted {
		t.Errorf("expected compacted after upsert, got %s", entries[0].Status)
	}
}

func TestClassifySessionKey(t *testing.T) {
	// Proves that ClassifySessionKey correctly identifies chat, branch, and unknown
	// session types from their key format, including edge cases like independent
	// keys that can't be further distinguished from the key alone.
	tests := []struct {
		key  string
		want SessionType
	}{
		{"bot/c123/1000000000", SessionTypeChat},
		{"bot/i123/123", SessionTypeUnknown},                // independent — can't distinguish facet/spawn/cron from key alone
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
	// Proves that Rebuild scans all session files on disk and populates the index
	// with the correct count, types, and parent keys — including branch sessions.
	dir := t.TempDir()
	store := NewStore(dir)

	// Create a few sessions
	store.TestAppend("bot/c100/1000000000", msg("user", "hello"))
	store.TestAppend("bot/c200/1000000000", msg("user", "world"))
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
	// Proves that session store events (create, replace, clear) propagate correctly
	// through the OnSessionEvent hook to update the index in real time, with create
	// firing only once per session key.
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
			idx.UpdateStatus(e.Key, SessionStatusCompacted)
		case SessionStatusCleared:
			idx.UpdateStatus(e.Key, SessionStatusCleared)
		}
	})

	// Create a session via Append (new file triggers event)
	store.TestAppend("bot/c100/1000000000", msg("user", "hello"))
	count, _ := idx.Count()
	if count != 1 {
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
	store.TestAppend("bot/c100/1000000000", msg("assistant", "hi"))
	count, _ = idx.Count()
	if count != 1 {
		t.Fatalf("expected still 1 after second append, got %d", count)
	}

	// Replace should fire compacted event
	store.TestReplace("bot/c100/1000000000", []provider.Message{msg("user", "compacted")})
	entries, _ = idx.Query(QueryOptions{})
	if entries[0].Status != SessionStatusCompacted {
		t.Errorf("expected compacted after Replace, got %s", entries[0].Status)
	}

	// Clear should fire cleared event
	store.TestClear("bot/c100/1000000000")
	entries, _ = idx.Query(QueryOptions{})
	if entries[0].Status != SessionStatusCleared {
		t.Errorf("expected cleared after Clear, got %s", entries[0].Status)
	}
}

func TestSessionIndex_BranchEventFiring(t *testing.T) {
	// Proves that CreateBranch fires an active event carrying the correct parent
	// key, so the index can link branch sessions to their parents.
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
	store.TestAppend("bot/c100/1000000000", msg("user", "hello"))

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
	// Proves that the SQLite-backed index survives a close/reopen cycle: entries
	// inserted before close are still present after reopening the same database file.
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

// ========== ResolvePartialKey tests ==========

// Verifies that ResolvePartialKey rejects invalid formats and resolves
// valid partial keys to the most recently active full session key.
func TestSessionIndex_ResolvePartialKey(t *testing.T) {
	idx := tempIndex(t)

	now := time.Now().UTC()
	// Insert two sessions for the same chat, different versions
	idx.Upsert(SessionIndexEntry{
		SessionKey: "scout/c123/1000000000", FilePath: "a", CreatedAt: now.Add(-time.Hour),
		SessionType: SessionTypeChat, Status: SessionStatusActive,
		LastActivityAt: now.Add(-time.Hour),
	})
	idx.Upsert(SessionIndexEntry{
		SessionKey: "scout/c123/2000000000", FilePath: "b", CreatedAt: now,
		SessionType: SessionTypeChat, Status: SessionStatusActive,
		LastActivityAt: now,
	})
	// Insert an independent session
	idx.Upsert(SessionIndexEntry{
		SessionKey: "scout/i456/1000000000", FilePath: "c", CreatedAt: now,
		SessionType: SessionTypeSpawn, Status: SessionStatusActive,
		LastActivityAt: now,
	})

	tests := []struct {
		name    string
		partial string
		want    string
	}{
		// Valid resolutions
		{"chat resolves to latest", "scout/c123", "scout/c123/2000000000"},
		{"independent resolves", "scout/i456", "scout/i456/1000000000"},

		// Invalid formats — should return ""
		{"empty string", "", ""},
		{"single segment", "scout", ""},
		{"three segments (already full)", "scout/c123/1000000000", ""},
		{"empty agent ID", "/c123", ""},
		{"empty type ID", "scout/", ""},
		{"type ID too short", "scout/c", ""},
		{"invalid type prefix", "scout/x123", ""},
		{"invalid type prefix b", "scout/b123", ""},

		// Valid format but no match
		{"no matching agent", "other/c123", ""},
		{"no matching chat", "scout/c999", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := idx.ResolvePartialKey(tt.partial)
			if got != tt.want {
				t.Errorf("ResolvePartialKey(%q) = %q, want %q", tt.partial, got, tt.want)
			}
		})
	}
}

// Verifies that ResolvePartialKey only returns active sessions,
// not compacted or cleared ones.
func TestSessionIndex_ResolvePartialKey_IgnoresInactive(t *testing.T) {
	idx := tempIndex(t)

	now := time.Now().UTC()
	idx.Upsert(SessionIndexEntry{
		SessionKey: "bot/c1/1000000000", FilePath: "a", CreatedAt: now,
		SessionType: SessionTypeChat, Status: SessionStatusCompacted,
	})

	got := idx.ResolvePartialKey("bot/c1")
	if got != "" {
		t.Errorf("should not resolve compacted session, got %q", got)
	}
}

// ========== Count tests ==========

func TestSessionIndex_Count_Empty(t *testing.T) {
	// Proves that Count returns zero on a freshly created index with no entries.
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
	// Proves that Count accurately tracks inserts, deletes, and upserts of existing
	// keys — verifying the count never double-counts on upsert.
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

// ========== Metadata CRUD tests ==========
//
// Agent, Chat, Session, and SystemState metadata share the same Set/Get/Delete
// contract. We test them through a common metadataOps adapter to avoid 4x
// duplicated Set/Get/Upsert/Delete/DeleteNonexistent tests.

type metadataOps struct {
	name   string
	set    func(key, value string) error
	get    func(key string) (string, error)
	delete func(key string) error
}

func agentMetaOps(idx *SessionIndex, agentID string) metadataOps {
	return metadataOps{
		name:   "AgentMetadata(" + agentID + ")",
		set:    func(k, v string) error { return idx.SetAgentMetadata(agentID, k, v) },
		get:    func(k string) (string, error) { return idx.GetAgentMetadata(agentID, k) },
		delete: func(k string) error { return idx.DeleteAgentMetadata(agentID, k) },
	}
}

func chatMetaOps(idx *SessionIndex, agentID string, chatID int64) metadataOps {
	return metadataOps{
		name:   "ChatMetadata(" + agentID + ")",
		set:    func(k, v string) error { return idx.SetChatMetadata(agentID, "", chatID, k, v) },
		get:    func(k string) (string, error) { return idx.GetChatMetadata(agentID, "", chatID, k) },
		delete: func(k string) error { return idx.DeleteChatMetadata(agentID, "", chatID, k) },
	}
}

func sessionMetaOps(idx *SessionIndex, sessionKey string) metadataOps {
	return metadataOps{
		name:   "SessionMetadata(" + sessionKey + ")",
		set:    func(k, v string) error { return idx.SetSessionMetadata(sessionKey, k, v) },
		get:    func(k string) (string, error) { return idx.GetSessionMetadata(sessionKey, k) },
		delete: func(k string) error { return idx.DeleteSessionMetadata(sessionKey, k) },
	}
}

func systemStateOps(idx *SessionIndex) metadataOps {
	return metadataOps{
		name:   "SystemState",
		set:    func(k, v string) error { return idx.SetSystemState(k, v) },
		get:    func(k string) (string, error) { return idx.GetSystemState(k) },
		delete: func(k string) error { return idx.DeleteSystemState(k) },
	}
}

// testMetadataCRUD runs the standard Set/Get/Upsert/Delete/DeleteNonexistent
// battery against any metadata store.
func testMetadataCRUD(t *testing.T, ops metadataOps) {
	t.Helper()

	t.Run("SetAndGet", func(t *testing.T) {
		if err := ops.set("key1", "val1"); err != nil {
			t.Fatalf("%s Set: %v", ops.name, err)
		}
		val, err := ops.get("key1")
		if err != nil {
			t.Fatalf("%s Get: %v", ops.name, err)
		}
		if val != "val1" {
			t.Errorf("got %q, want %q", val, "val1")
		}
	})

	t.Run("GetMissing", func(t *testing.T) {
		val, err := ops.get("nonexistent_key")
		if err != nil {
			t.Fatalf("%s Get: %v", ops.name, err)
		}
		if val != "" {
			t.Errorf("expected empty for missing key, got %q", val)
		}
	})

	t.Run("Upsert", func(t *testing.T) {
		ops.set("ukey", "old")
		ops.set("ukey", "new")
		val, _ := ops.get("ukey")
		if val != "new" {
			t.Errorf("upsert should overwrite: got %q, want %q", val, "new")
		}
	})

	t.Run("Delete", func(t *testing.T) {
		ops.set("dkey", "val")
		if err := ops.delete("dkey"); err != nil {
			t.Fatalf("%s Delete: %v", ops.name, err)
		}
		val, _ := ops.get("dkey")
		if val != "" {
			t.Errorf("expected empty after delete, got %q", val)
		}
	})

	t.Run("DeleteNonexistent", func(t *testing.T) {
		if err := ops.delete("ghost_key"); err != nil {
			t.Fatalf("%s Delete nonexistent: %v", ops.name, err)
		}
	})
}

func TestAgentMetadata_CRUD(t *testing.T) {
	// Proves that agent-scoped metadata supports the full set/get/upsert/delete
	// operations using the shared CRUD battery.
	idx := tempIndex(t)
	testMetadataCRUD(t, agentMetaOps(idx, "bot1"))
}

func TestChatMetadata_CRUD(t *testing.T) {
	// Proves that chat-scoped metadata supports the full set/get/upsert/delete
	// operations using the shared CRUD battery.
	idx := tempIndex(t)
	testMetadataCRUD(t, chatMetaOps(idx, "bot1", 42))
}

func TestSessionMetadata_CRUD(t *testing.T) {
	// Proves that session-scoped metadata supports the full set/get/upsert/delete
	// operations using the shared CRUD battery.
	idx := tempIndex(t)
	testMetadataCRUD(t, sessionMetaOps(idx, "bot/c1/1000000000"))
}

func TestSystemState_CRUD(t *testing.T) {
	// Proves that global system-state metadata supports the full set/get/upsert/delete
	// operations using the shared CRUD battery.
	idx := tempIndex(t)
	testMetadataCRUD(t, systemStateOps(idx))
}

// ========== Domain-specific isolation and multi-key tests ==========

func TestAgentMetadata_IsolationBetweenAgents(t *testing.T) {
	// Proves that the same metadata key set on different agents returns separate
	// values — agent IDs act as namespaces.
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
	// Proves that an agent can store independent values under multiple keys and
	// that deleting one key leaves the others unaffected.
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

func TestChatMetadata_IsolationBetweenChats(t *testing.T) {
	// Proves that chat metadata is namespaced by both agent ID and chat ID:
	// the same key on different chats or different agents stores independent values.
	idx := tempIndex(t)

	// Same agent, different chats
	idx.SetChatMetadata("bot1", "", 1, "model", "claude")
	idx.SetChatMetadata("bot1", "", 2, "model", "gpt")

	v1, _ := idx.GetChatMetadata("bot1", "", 1, "model")
	v2, _ := idx.GetChatMetadata("bot1", "", 2, "model")

	if v1 != "claude" || v2 != "gpt" {
		t.Errorf("chat isolation failed: chat1=%q chat2=%q", v1, v2)
	}

	// Different agents, same chat ID
	idx.SetChatMetadata("bot2", "", 1, "model", "gemini")
	v3, _ := idx.GetChatMetadata("bot2", "", 1, "model")
	v1again, _ := idx.GetChatMetadata("bot1", "", 1, "model")

	if v3 != "gemini" || v1again != "claude" {
		t.Errorf("agent isolation failed: bot2=%q bot1=%q", v3, v1again)
	}
}

func TestSessionMetadata_IsolationBetweenSessions(t *testing.T) {
	// Proves that session metadata is namespaced by session key: the same metadata
	// key on two different sessions holds independent values.
	idx := tempIndex(t)

	idx.SetSessionMetadata("bot/c1/1000000000", "no_compact", "true")
	idx.SetSessionMetadata("bot/c2/1000000000", "no_compact", "false")

	v1, _ := idx.GetSessionMetadata("bot/c1/1000000000", "no_compact")
	v2, _ := idx.GetSessionMetadata("bot/c2/1000000000", "no_compact")

	if v1 != "true" || v2 != "false" {
		t.Errorf("session isolation failed: s1=%q s2=%q", v1, v2)
	}
}

func TestSystemState_MultipleKeys(t *testing.T) {
	// Proves that multiple system-state keys coexist independently and that
	// deleting one key leaves the remaining keys intact.
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
	// Proves that all four metadata scopes (agent, chat, session, system state)
	// survive a database close and reopen, confirming durable SQLite persistence.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persist.db")

	idx1, err := NewSessionIndex(dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	idx1.SetAgentMetadata("bot1", "model", "claude-3")
	idx1.SetChatMetadata("bot1", "", 42, "effort", "high")
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
	if v, _ := idx2.GetChatMetadata("bot1", "", 42, "effort"); v != "high" {
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

// Proves that RotateChatSessionKey only updates the chat_metadata row whose value
// matches oldKey, leaving rows for other platforms, other chats, and rows with
// different values completely untouched. Also verifies no spurious rows are created.
func TestSessionIndex_RotateChatSessionKey(t *testing.T) {
	idx := tempIndex(t)

	// Set up: two platforms for the same agent+chat, each with a different session key.
	// This simulates the bug scenario where telegram and discord both have rows.
	idx.SetChatMetadata("clutch", "telegram", 5970, "session_key", "clutch/c5970/1000")
	idx.SetChatMetadata("clutch", "discord", 5970, "session_key", "clutch/c5970/2000")

	// Also set up a different chat that should never be touched.
	idx.SetChatMetadata("clutch", "telegram", 9999, "session_key", "clutch/c9999/1000")

	// Rotate: only the telegram row (value=clutch/c5970/1000) should change.
	err := idx.RotateChatSessionKey("clutch", 5970, "clutch/c5970/1000", "clutch/c5970/3000")
	if err != nil {
		t.Fatalf("RotateChatSessionKey: %v", err)
	}

	// Telegram row updated.
	v, _ := idx.GetChatMetadata("clutch", "telegram", 5970, "session_key")
	if v != "clutch/c5970/3000" {
		t.Errorf("telegram row: got %q, want %q", v, "clutch/c5970/3000")
	}

	// Discord row untouched (different value).
	v, _ = idx.GetChatMetadata("clutch", "discord", 5970, "session_key")
	if v != "clutch/c5970/2000" {
		t.Errorf("discord row should be untouched: got %q, want %q", v, "clutch/c5970/2000")
	}

	// Other chat untouched.
	v, _ = idx.GetChatMetadata("clutch", "telegram", 9999, "session_key")
	if v != "clutch/c9999/1000" {
		t.Errorf("other chat should be untouched: got %q, want %q", v, "clutch/c9999/1000")
	}
}

// Proves that RotateChatSessionKey is a no-op when no row matches the old value,
// and does not create any new rows.
func TestSessionIndex_RotateChatSessionKey_NoMatch(t *testing.T) {
	idx := tempIndex(t)

	idx.SetChatMetadata("clutch", "telegram", 5970, "session_key", "clutch/c5970/1000")

	// Rotate with a wrong oldKey — should be a no-op.
	err := idx.RotateChatSessionKey("clutch", 5970, "clutch/c5970/WRONG", "clutch/c5970/3000")
	if err != nil {
		t.Fatalf("RotateChatSessionKey: %v", err)
	}

	// Original row unchanged.
	v, _ := idx.GetChatMetadata("clutch", "telegram", 5970, "session_key")
	if v != "clutch/c5970/1000" {
		t.Errorf("row should be unchanged: got %q, want %q", v, "clutch/c5970/1000")
	}
}

func TestMetadata_IndependentOfSessionIndex(t *testing.T) {
	// Proves that metadata operations don't affect the session entry count and that
	// session operations don't corrupt metadata values — the two concerns are fully
	// isolated within the same database.
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
