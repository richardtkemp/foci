package session

import (
	"fmt"
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
		SessionKey:  "bot/c123",
		FilePath:    "/data/sessions/bot/c123/root.jsonl",
		CreatedAt:   now,
		SessionType: SessionTypeChat,
		Status:      SessionStatusActive,
	})
	idx.Upsert(SessionIndexEntry{
		SessionKey:       "bot/i456",
		FilePath:         "/data/sessions/bot/i456/root.jsonl",
		CreatedAt:        now.Add(-time.Hour),
		ParentSessionKey: "bot/c123",
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
	if entries[0].SessionKey != "bot/c123" {
		t.Errorf("expected chat first, got %s", entries[0].SessionKey)
	}
	if entries[1].ParentSessionKey != "bot/c123" {
		t.Errorf("expected parent key on spawn, got %q", entries[1].ParentSessionKey)
	}
}

func TestSessionIndex_QueryByType(t *testing.T) {
	// Proves that QueryOptions.SessionType filters results to only entries of
	// the specified type, leaving other types out of the result set.
	idx := tempIndex(t)

	now := time.Now().UTC()
	idx.Upsert(SessionIndexEntry{
		SessionKey: "bot/c1", FilePath: "a", CreatedAt: now,
		SessionType: SessionTypeChat, Status: SessionStatusActive,
	})
	idx.Upsert(SessionIndexEntry{
		SessionKey: "bot/i2", FilePath: "b", CreatedAt: now,
		SessionType: SessionTypeSpawn, Status: SessionStatusActive,
	})

	entries, err := idx.Query(QueryOptions{SessionType: string(SessionTypeChat)})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 chat entry, got %d", len(entries))
	}
	if entries[0].SessionKey != "bot/c1" {
		t.Errorf("wrong entry: %s", entries[0].SessionKey)
	}
}

func TestSessionIndex_QueryByStatus(t *testing.T) {
	// Proves that QueryOptions.Status filters results to only entries matching
	// the requested status, excluding those with a different status.
	idx := tempIndex(t)

	now := time.Now().UTC()
	idx.Upsert(SessionIndexEntry{
		SessionKey: "bot/c1", FilePath: "a", CreatedAt: now,
		SessionType: SessionTypeChat, Status: SessionStatusActive,
	})
	idx.Upsert(SessionIndexEntry{
		SessionKey: "bot/c2", FilePath: "b", CreatedAt: now,
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
	// Proves that QueryOptions.AgentID scopes results to a single agent via the
	// agent_id column derived from the key at upsert, excluding entries belonging
	// to other agents in the same index.
	idx := tempIndex(t)

	now := time.Now().UTC()
	idx.Upsert(SessionIndexEntry{
		SessionKey: "alpha/c1", FilePath: "a", CreatedAt: now,
		SessionType: SessionTypeChat, Status: SessionStatusActive,
	})
	idx.Upsert(SessionIndexEntry{
		SessionKey: "beta/c2", FilePath: "b", CreatedAt: now,
		SessionType: SessionTypeChat, Status: SessionStatusActive,
	})

	entries, err := idx.Query(QueryOptions{AgentID: "alpha"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry for alpha, got %d", len(entries))
	}
	if entries[0].SessionKey != "alpha/c1" {
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
			SessionKey: fmt.Sprintf("bot/c%d", i), FilePath: "f",
			CreatedAt:   now.Add(time.Duration(i) * time.Minute),
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
		SessionKey: "bot/c1", FilePath: "a",
		CreatedAt: time.Now().UTC(), SessionType: SessionTypeChat,
		Status: SessionStatusActive,
	})

	idx.UpdateStatus("bot/c1", SessionStatusCompacted)

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
		SessionKey: "bot/c1", FilePath: "a",
		CreatedAt: time.Now().UTC(), SessionType: SessionTypeChat,
		Status: SessionStatusActive,
	})

	idx.Delete("bot/c1")

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
		SessionKey: "bot/c1", FilePath: "a",
		CreatedAt: now, SessionType: SessionTypeChat,
		Status: SessionStatusActive,
	})
	idx.Upsert(SessionIndexEntry{
		SessionKey: "bot/c1", FilePath: "a",
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

func TestSessionIndex_SessionExists(t *testing.T) {
	// Proves that SessionExists reports true for an upserted key and false for a
	// key with no index row (or after deletion).
	idx := tempIndex(t)

	if idx.SessionExists("bot/c1") {
		t.Error("SessionExists on empty index should be false")
	}

	idx.Upsert(SessionIndexEntry{
		SessionKey: "bot/c1", FilePath: "a",
		CreatedAt: time.Now().UTC(), SessionType: SessionTypeChat,
		Status: SessionStatusActive,
	})
	if !idx.SessionExists("bot/c1") {
		t.Error("SessionExists should be true after upsert")
	}

	idx.Delete("bot/c1")
	if idx.SessionExists("bot/c1") {
		t.Error("SessionExists should be false after delete")
	}
}

func TestKeyColumns(t *testing.T) {
	// Proves that keyColumns derives the structured index columns from a session
	// key: agent_id and chat_id from the parse, is_root=1 only for roots, and
	// falls back to agent-only attribution for archive bookkeeping keys that
	// don't parse (dotted suffixes).
	tests := []struct {
		key       string
		wantAgent string
		wantChat  int64
		wantRoot  int
	}{
		{"main/c123", "main", 123, 1},
		{"main/iwork", "main", 0, 1},
		{"main/c123/b1700000000", "main", 123, 0},
		{"main/i1700000000/i1700000001", "main", 0, 0},
		// Archive key: doesn't parse → agent-only fallback.
		{"main/c123/root.2026-03-04T02-30-00Z", "main", 0, 0},
		// Garbage: no separator → no agent.
		{"garbage", "", 0, 0},
	}
	for _, tt := range tests {
		agent, chat, root := keyColumns(tt.key)
		if agent != tt.wantAgent || chat != tt.wantChat || root != tt.wantRoot {
			t.Errorf("keyColumns(%q) = (%q, %d, %d), want (%q, %d, %d)",
				tt.key, agent, chat, root, tt.wantAgent, tt.wantChat, tt.wantRoot)
		}
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
		{"bot/c123", SessionTypeChat},
		{"bot/i123", SessionTypeUnknown},            // independent — can't distinguish facet/spawn/cron from key alone
		{"bot/c123/b456", SessionTypeBranch},        // branch child type
		{"bot/i123/b456", SessionTypeBranch},        // branch from independent parent
		{"bot/c123/i456", SessionTypeUnknown},       // independent spawn child
		{"bot/c123/1709590000", SessionTypeUnknown}, // old versioned format — no longer parses
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
	// Proves that Rebuild scans all session files on disk and populates the index
	// with the correct count, types, and parent keys — including branch sessions.
	dir := t.TempDir()
	store := NewStore(dir)

	// Create a few sessions
	store.TestAppend("bot/c100", msg("user", "hello"))
	store.TestAppend("bot/c200", msg("user", "world"))
	branchKey := "bot/c100/b1000000001"
	store.createBranchFile("bot/c100", branchKey, false, "")

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
	if entries[0].ParentSessionKey != "bot/c100" {
		t.Errorf("expected parent key bot/c100, got %q", entries[0].ParentSessionKey)
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
	store.TestAppend("bot/c100", msg("user", "hello"))
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
	store.TestAppend("bot/c100", msg("assistant", "hi"))
	count, _ = idx.Count()
	if count != 1 {
		t.Fatalf("expected still 1 after second append, got %d", count)
	}

	// Replace should fire compacted event
	store.TestReplace("bot/c100", []provider.Message{msg("user", "compacted")})
	entries, _ = idx.Query(QueryOptions{})
	if entries[0].Status != SessionStatusCompacted {
		t.Errorf("expected compacted after Replace, got %s", entries[0].Status)
	}

	// Clear should fire cleared event
	store.TestClear("bot/c100")
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
	store.TestAppend("bot/c100", msg("user", "hello"))

	// Create branch
	store.createBranchFile("bot/c100", "bot/c100/b1000000001", false, "")
	count, _ := idx.Count()
	if count != 2 {
		t.Fatalf("expected 2 after branch create, got %d", count)
	}

	entries, _ := idx.Query(QueryOptions{SessionType: string(SessionTypeBranch)})
	if len(entries) != 1 {
		t.Fatalf("expected 1 branch, got %d", len(entries))
	}
	if entries[0].ParentSessionKey != "bot/c100" {
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
		SessionKey: "bot/c1", FilePath: "a",
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
	for i, key := range []string{"a/c1", "b/c2", "c/c3"} {
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
	idx.Delete("b/c2")
	count, _ = idx.Count()
	if count != 2 {
		t.Fatalf("expected 2 after delete, got %d", count)
	}

	// Upsert existing (should not increase count)
	idx.Upsert(SessionIndexEntry{
		SessionKey:  "a/c1",
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
	testMetadataCRUD(t, sessionMetaOps(idx, "bot/c1"))
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

// ========== DefaultSessionKeyForAgent tests ==========

func TestDefaultSessionKeyForAgent_DefaultChat(t *testing.T) {
	// Proves DefaultSessionKeyForAgent resolves an is_default chat by DERIVING
	// the deterministic agent/c<chatID> key — no session_key row or index entry
	// is needed, because chat keys are stable identities.
	idx := tempIndex(t)

	if err := idx.SetDefaultChat("scout", "telegram", 123); err != nil {
		t.Fatalf("set default chat: %v", err)
	}

	key := idx.DefaultSessionKeyForAgent("scout")
	if key != "scout/c123" {
		t.Errorf("expected derived default-chat key scout/c123, got %q", key)
	}
}

func TestDefaultSessionKeyForAgent_MostActiveDefaultWins(t *testing.T) {
	// Proves that when an agent has is_default chats on several platforms, the
	// resolver returns the one whose derived session has the most recent
	// activity — the live platform wins over a dormant one.
	idx := tempIndex(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := idx.SetDefaultChat("helen", "telegram", 100); err != nil {
		t.Fatalf("set telegram default: %v", err)
	}
	if err := idx.SetDefaultChat("helen", "discord", 200); err != nil {
		t.Fatalf("set discord default: %v", err)
	}

	// telegram chat is dormant, discord chat is live. Routing orders by
	// last_user_activity_at (a human touched it recently), not raw activity.
	idx.Upsert(SessionIndexEntry{
		SessionKey: "helen/c100", FilePath: "a",
		CreatedAt: now.Add(-48 * time.Hour), LastActivityAt: now.Add(-48 * time.Hour),
		SessionType: SessionTypeChat, Status: SessionStatusActive,
	})
	idx.Upsert(SessionIndexEntry{
		SessionKey: "helen/c200", FilePath: "b",
		CreatedAt: now.Add(-48 * time.Hour), LastActivityAt: now,
		SessionType: SessionTypeChat, Status: SessionStatusActive,
	})
	idx.TouchUserActivity("helen/c100", now.Add(-48*time.Hour))
	idx.TouchUserActivity("helen/c200", now)

	if key := idx.DefaultSessionKeyForAgent("helen"); key != "helen/c200" {
		t.Errorf("expected most recently active default helen/c200, got %q", key)
	}
	if key := idx.ResolveLooseKey("helen"); key != "helen/c200" {
		t.Errorf("ResolveLooseKey: expected helen/c200, got %q", key)
	}
}

func TestDefaultSessionKeyForAgent_Fallback(t *testing.T) {
	// Proves DefaultSessionKeyForAgent falls back to the most recently
	// user-active is_root=1 active session when no default chat is set — and
	// that an instance root ('i', classified 'unknown') is eligible: is_root=1
	// already excludes branches/spawns, so a non-chat root here is the agent's
	// legitimate primary session, not something to drop.
	idx := tempIndex(t)
	now := time.Now().UTC().Truncate(time.Second)

	// A chat root, user-active 2h ago; an instance root, user-active now.
	idx.Upsert(SessionIndexEntry{
		SessionKey:     "scout/c999",
		FilePath:       "/tmp/chat.jsonl",
		CreatedAt:      now.Add(-3 * time.Hour),
		LastActivityAt: now.Add(-2 * time.Hour),
		SessionType:    SessionTypeChat,
		Status:         SessionStatusActive,
	})
	idx.Upsert(SessionIndexEntry{
		SessionKey:     "scout/iwork",
		FilePath:       "/tmp/instance.jsonl",
		CreatedAt:      now.Add(-time.Hour),
		LastActivityAt: now,
		SessionType:    SessionTypeUnknown,
		Status:         SessionStatusActive,
	})
	idx.TouchUserActivity("scout/c999", now.Add(-2*time.Hour))
	idx.TouchUserActivity("scout/iwork", now)

	key := idx.DefaultSessionKeyForAgent("scout")
	if key != "scout/iwork" {
		t.Errorf("expected most recently user-active root scout/iwork (instance root eligible), got %q", key)
	}
}

func TestDefaultSessionKeyForAgent_FallbackReturnsInstanceRoot(t *testing.T) {
	// Proves that when an agent's ONLY active root is an instance root ('i',
	// classified 'unknown') — e.g. an agent that received a /send before any
	// chat binding — the fallback still returns it rather than "". Regression
	// guard: an earlier session_type='chat' filter wrongly dropped it.
	idx := tempIndex(t)
	now := time.Now().UTC().Truncate(time.Second)

	idx.Upsert(SessionIndexEntry{
		SessionKey:     "scout/imain",
		FilePath:       "/tmp/instance.jsonl",
		CreatedAt:      now,
		LastActivityAt: now,
		SessionType:    SessionTypeUnknown,
		Status:         SessionStatusActive,
	})

	if key := idx.DefaultSessionKeyForAgent("scout"); key != "scout/imain" {
		t.Errorf("expected instance root scout/imain, got %q", key)
	}
}

func TestDefaultSessionKeyForAgent_ExcludesChildren(t *testing.T) {
	// Proves the fallback ignores branch/child sessions (is_root=0) and returns
	// "" when only child sessions exist.
	idx := tempIndex(t)

	// Insert only a branch session (child key)
	idx.Upsert(SessionIndexEntry{
		SessionKey:     "scout/c999/b1709590001",
		FilePath:       "/tmp/branch.jsonl",
		CreatedAt:      time.Now(),
		LastActivityAt: time.Now(),
		SessionType:    SessionTypeBranch,
		Status:         SessionStatusActive,
	})

	key := idx.DefaultSessionKeyForAgent("scout")
	if key != "" {
		t.Errorf("expected empty (no root sessions), got %q", key)
	}
}

func TestDefaultSessionKeyForAgent_ExcludesInactive(t *testing.T) {
	// Proves the fallback skips non-active rows (e.g. archived sessions), so a
	// swept session is never handed out as an agent's default.
	idx := tempIndex(t)

	idx.Upsert(SessionIndexEntry{
		SessionKey:     "scout/c999",
		FilePath:       "/tmp/archived.jsonl",
		CreatedAt:      time.Now(),
		LastActivityAt: time.Now(),
		SessionType:    SessionTypeChat,
		Status:         SessionStatusArchived,
	})

	if key := idx.DefaultSessionKeyForAgent("scout"); key != "" {
		t.Errorf("expected empty (no active roots), got %q", key)
	}
}

// ========== ResolveLooseKey tests ==========

func TestResolveLooseKey_BareAgentName(t *testing.T) {
	// Proves a bare agent name (no slash) dispatches to
	// DefaultSessionKeyForAgent and resolves to the agent's default session.
	idx := tempIndex(t)

	if err := idx.SetDefaultChat("scout", "telegram", 123); err != nil {
		t.Fatalf("set default chat: %v", err)
	}

	if key := idx.ResolveLooseKey("scout"); key != "scout/c123" {
		t.Errorf("bare name: expected scout/c123, got %q", key)
	}
}

func TestResolveLooseKey_FullKeyReturnsEmpty(t *testing.T) {
	// Proves anything containing "/" returns "" — under the stable-identity
	// grammar such strings are already full session keys, handled by
	// ParseSessionKey before the resolver is consulted.
	idx := tempIndex(t)

	if err := idx.SetDefaultChat("scout", "telegram", 123); err != nil {
		t.Fatalf("set default chat: %v", err)
	}

	for _, full := range []string{"scout/c123", "scout/c123/b1709590000", "scout/"} {
		if key := idx.ResolveLooseKey(full); key != "" {
			t.Errorf("ResolveLooseKey(%q): expected empty, got %q", full, key)
		}
	}
}

func TestResolveLooseKey_NoMatch(t *testing.T) {
	// Proves an unknown bare name resolves to "".
	idx := tempIndex(t)

	if key := idx.ResolveLooseKey("ghost"); key != "" {
		t.Errorf("unknown agent: expected empty, got %q", key)
	}
}

// ========== CurrentSessionKeys / PlatformForChat tests ==========

func TestCurrentSessionKeys(t *testing.T) {
	// Proves CurrentSessionKeys derives the protected key set from DISTINCT
	// (agent_id, chat_id) pairs in chat_metadata — one deterministic key per
	// registered chat, regardless of how many metadata rows the chat has, and
	// no keys for agent-less or chat-less rows.
	idx := tempIndex(t)

	// Two rows for the same chat (registered + username) → one key.
	idx.SetChatMetadata("bot", "telegram", 100, "registered", "1")
	idx.SetChatMetadata("bot", "telegram", 100, "username", "rich")
	// Another chat on a different platform.
	idx.SetChatMetadata("bot", "discord", 200, "registered", "1")
	// Different agent, same chat ID.
	idx.SetChatMetadata("other", "telegram", 100, "registered", "1")
	// chat_id 0 rows must not produce keys.
	idx.SetChatMetadata("bot", "telegram", 0, "misc", "x")

	keys, err := idx.CurrentSessionKeys()
	if err != nil {
		t.Fatalf("CurrentSessionKeys: %v", err)
	}
	want := []string{"bot/c100", "bot/c200", "other/c100"}
	if len(keys) != len(want) {
		t.Fatalf("expected %d keys, got %d: %v", len(want), len(keys), keys)
	}
	for _, k := range want {
		if !keys[k] {
			t.Errorf("expected key %q in current set %v", k, keys)
		}
	}
}

func TestPlatformForChat(t *testing.T) {
	// Proves PlatformForChat returns the platform of any chat_metadata row with
	// a non-empty platform (the "registered" row written on first contact), and
	// "" when only empty-platform rows or no rows exist.
	idx := tempIndex(t)

	// Unknown chat → "".
	if p := idx.PlatformForChat("bot", 100); p != "" {
		t.Errorf("unknown chat: expected empty, got %q", p)
	}

	// Only an empty-platform row → still "".
	idx.SetChatMetadata("bot", "", 100, "misc", "x")
	if p := idx.PlatformForChat("bot", 100); p != "" {
		t.Errorf("empty-platform row: expected empty, got %q", p)
	}

	// Registered row establishes ownership.
	idx.SetChatMetadata("bot", "telegram", 100, "registered", "1")
	if p := idx.PlatformForChat("bot", 100); p != "telegram" {
		t.Errorf("expected telegram, got %q", p)
	}

	// Other agent's rows don't leak.
	if p := idx.PlatformForChat("other", 100); p != "" {
		t.Errorf("other agent: expected empty, got %q", p)
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

	idx.SetSessionMetadata("bot/c1", "no_compact", "true")
	idx.SetSessionMetadata("bot/c2", "no_compact", "false")

	v1, _ := idx.GetSessionMetadata("bot/c1", "no_compact")
	v2, _ := idx.GetSessionMetadata("bot/c2", "no_compact")

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
	idx1.SetSessionMetadata("bot/c1", "no_compact", "true")
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
	if v, _ := idx2.GetSessionMetadata("bot/c1", "no_compact"); v != "true" {
		t.Errorf("session metadata not persisted: got %q", v)
	}
	if v, _ := idx2.GetSystemState("version"); v != "1" {
		t.Errorf("system state not persisted: got %q", v)
	}
}

// ========== Metadata tables don't interfere with session index ==========

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
		SessionKey:  "bot/c1",
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

// ========== Reflection tests ==========

func TestStampReflection(t *testing.T) {
	// Proves that StampReflection records a timestamp for a session, and that
	// SessionsNeedingReflection no longer returns it when activity hasn't changed
	// (i.e. last_reflection >= last_activity_at).
	idx := tempIndex(t)

	now := time.Now().UTC().Truncate(time.Second)
	idx.Upsert(SessionIndexEntry{
		SessionKey:     "bot/c100",
		FilePath:       "/data/sessions/bot/c100/root.jsonl",
		CreatedAt:      now.Add(-time.Hour),
		LastActivityAt: now,
		SessionType:    SessionTypeChat,
		Status:         SessionStatusActive,
	})

	// New activity after the seeded last_reflection (#945: Upsert seeds it to the
	// birth activity time) makes the session genuinely due for reflection.
	idx.UpdateActivity("bot/c100", now.Add(time.Minute))

	// Before stamping: activity is newer than last_reflection, so it needs reflection.
	keys, err := idx.SessionsNeedingReflection("bot")
	if err != nil {
		t.Fatalf("SessionsNeedingReflection: %v", err)
	}
	if len(keys) != 1 || keys[0] != "bot/c100" {
		t.Fatalf("expected [bot/c100] before stamp, got %v", keys)
	}

	// Stamp reflection at or after the last activity time.
	idx.StampReflection("bot/c100", now.Add(time.Minute))

	// After stamping: last_reflection >= last_activity_at, so not returned.
	keys, err = idx.SessionsNeedingReflection("bot")
	if err != nil {
		t.Fatalf("SessionsNeedingReflection after stamp: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("expected no sessions needing reflection after stamp, got %v", keys)
	}
}

func TestReflectionRedundant(t *testing.T) {
	// ReflectionRedundant backs the reset-time "no need to reflect twice" guard.
	// It returns true when last_activity <= last_reflection (nothing new to
	// reflect). An UNKNOWN session (no row) defaults to false (reflect). A freshly
	// created session is redundant until new activity arrives, because Upsert seeds
	// last_reflection to its birth activity time (#945).
	idx := tempIndex(t)
	now := time.Now().UTC().Truncate(time.Second)

	mk := func(key string) {
		idx.Upsert(SessionIndexEntry{
			SessionKey:     key,
			FilePath:       "/f/" + key,
			CreatedAt:      now.Add(-time.Hour),
			LastActivityAt: now,
			SessionType:    SessionTypeChat,
			Status:         SessionStatusActive,
		})
	}

	// Unknown session → not redundant (default to reflecting).
	if idx.ReflectionRedundant("bot/c0") {
		t.Error("unknown session: want false, got true")
	}

	// Freshly created, never explicitly reflected, no new activity since birth →
	// REDUNDANT (#945). Upsert seeds last_reflection to the birth activity time, so
	// last_activity == last_reflection and there is nothing new to reflect. (Before
	// the fix this defaulted to NOT redundant and got reflected empty.)
	mk("bot/c1")
	if !idx.ReflectionRedundant("bot/c1") {
		t.Error("fresh session, no new activity: want true (redundant), got false")
	}

	// Reflection ran, then activity since → not redundant.
	mk("bot/c2")
	idx.StampReflection("bot/c2", now.Add(-time.Minute))
	idx.UpdateActivity("bot/c2", now)
	if idx.ReflectionRedundant("bot/c2") {
		t.Error("activity after reflection: want false, got true")
	}

	// Reflection at/after last activity → redundant (the skip case).
	mk("bot/c3")
	idx.StampReflection("bot/c3", now.Add(time.Minute))
	if !idx.ReflectionRedundant("bot/c3") {
		t.Error("reflection newer than activity: want true, got false")
	}

	// Exact tie (last_reflection == last_activity_at) → redundant (<=).
	mk("bot/c4")
	idx.StampReflection("bot/c4", now)
	if !idx.ReflectionRedundant("bot/c4") {
		t.Error("reflection == activity: want true, got false")
	}
}

func TestSessionsNeedingReflection(t *testing.T) {
	// Proves that SessionsNeedingReflection correctly filters sessions based on
	// activity vs reflection timestamps, session type, status, and agent scoping.
	idx := tempIndex(t)

	base := time.Date(2026, 3, 23, 12, 0, 0, 0, time.UTC)

	// Case 1: Activity newer than reflection — should be included.
	idx.Upsert(SessionIndexEntry{
		SessionKey:     "agent1/c1",
		FilePath:       "f1",
		CreatedAt:      base,
		LastActivityAt: base,
		SessionType:    SessionTypeChat,
		Status:         SessionStatusActive,
	})
	idx.StampReflection("agent1/c1", base.Add(-time.Hour))
	idx.UpdateActivity("agent1/c1", base)

	// Case 2: Activity older than reflection — should be excluded.
	idx.Upsert(SessionIndexEntry{
		SessionKey:     "agent1/c2",
		FilePath:       "f2",
		CreatedAt:      base,
		LastActivityAt: base,
		SessionType:    SessionTypeChat,
		Status:         SessionStatusActive,
	})
	idx.StampReflection("agent1/c2", base.Add(time.Hour))

	// Case 3: freshly created, never explicitly reflected, NO activity since
	// creation — should be EXCLUDED (#945). Upsert seeds last_reflection to the
	// creation/activity time, so a just-born / just-compacted session is not
	// immediately due; it becomes due only once real new activity arrives.
	idx.Upsert(SessionIndexEntry{
		SessionKey:     "agent1/c3",
		FilePath:       "f3",
		CreatedAt:      base,
		LastActivityAt: base,
		SessionType:    SessionTypeChat,
		Status:         SessionStatusActive,
	})
	// No StampReflection: last_reflection is seeded == creation, last_activity == creation → not due.

	// Case 3b: same shape but with NEW activity AFTER creation — should be
	// INCLUDED (activity advances last_activity past the seeded last_reflection).
	idx.Upsert(SessionIndexEntry{
		SessionKey:     "agent1/c33",
		FilePath:       "f3b",
		CreatedAt:      base,
		LastActivityAt: base,
		SessionType:    SessionTypeChat,
		Status:         SessionStatusActive,
	})
	idx.UpdateActivity("agent1/c33", base.Add(time.Hour))

	// Case 4: Non-chat session (branch) — should be excluded.
	idx.Upsert(SessionIndexEntry{
		SessionKey:     "agent1/c1/b2000000000",
		FilePath:       "f4",
		CreatedAt:      base,
		LastActivityAt: base,
		SessionType:    SessionTypeBranch,
		Status:         SessionStatusActive,
	})
	// No stamp — would qualify if it were a chat session.

	// Case 5: Non-active session (compacted chat) — should be excluded.
	idx.Upsert(SessionIndexEntry{
		SessionKey:     "agent1/c5",
		FilePath:       "f5",
		CreatedAt:      base,
		LastActivityAt: base,
		SessionType:    SessionTypeChat,
		Status:         SessionStatusCompacted,
	})
	// No stamp — would qualify if it were active.

	// Case 6: Different agent — should be excluded.
	idx.Upsert(SessionIndexEntry{
		SessionKey:     "agent2/c6",
		FilePath:       "f6",
		CreatedAt:      base,
		LastActivityAt: base,
		SessionType:    SessionTypeChat,
		Status:         SessionStatusActive,
	})
	// No stamp — would qualify if it belonged to agent1.

	keys, err := idx.SessionsNeedingReflection("agent1")
	if err != nil {
		t.Fatalf("SessionsNeedingReflection: %v", err)
	}

	// Build a set for easy lookup.
	got := make(map[string]bool, len(keys))
	for _, k := range keys {
		got[k] = true
	}

	// Case 1: activity > reflection → included.
	if !got["agent1/c1"] {
		t.Errorf("case 1 (activity newer than reflection): expected included, missing from %v", keys)
	}
	// Case 2: activity < reflection → excluded.
	if got["agent1/c2"] {
		t.Errorf("case 2 (activity older than reflection): expected excluded, present in %v", keys)
	}
	// Case 3: freshly created, no new activity since creation → excluded (#945).
	if got["agent1/c3"] {
		t.Errorf("case 3 (fresh session, no new activity): expected excluded, present in %v", keys)
	}
	// Case 3b: freshly created but with new activity after creation → included.
	if !got["agent1/c33"] {
		t.Errorf("case 3b (fresh session with new activity): expected included, missing from %v", keys)
	}
	// Case 4: non-chat session → excluded.
	if got["agent1/c1/b2000000000"] {
		t.Errorf("case 4 (branch session): expected excluded, present in %v", keys)
	}
	// Case 5: non-active session → excluded.
	if got["agent1/c5"] {
		t.Errorf("case 5 (compacted session): expected excluded, present in %v", keys)
	}
	// Case 6: different agent → excluded.
	if got["agent2/c6"] {
		t.Errorf("case 6 (different agent): expected excluded, present in %v", keys)
	}

	// Exactly 2 sessions should be returned.
	if len(keys) != 2 {
		t.Errorf("expected exactly 2 sessions needing reflection, got %d: %v", len(keys), keys)
	}
}

// TestRebuildIndex_PreservesBackendSessionRows captures the discord-misroute
// bug: delegated agents' sessions have no session file (empty file_path), so
// a rebuild that wipes the whole table erases their last_activity_at — and
// DefaultSessionKeyForAgent's tiebreak between two is_default chats becomes
// arbitrary right after restart. The rebuild must preserve backend rows (and
// their activity) while file-backed rows are re-derived from the scan.
func TestRebuildIndex_PreservesBackendSessionRows(t *testing.T) {
	idx, err := NewSessionIndex(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close() //nolint:errcheck

	// Two backend sessions (no files): telegram chat user-active recently,
	// discord chat stale. Both chats flagged is_default on their platforms.
	now := time.Now()
	idx.Upsert(SessionIndexEntry{SessionKey: "ag/c111", SessionType: SessionTypeChat, Status: SessionStatusActive,
		CreatedAt: now.Add(-48 * time.Hour), LastActivityAt: now.Add(-time.Hour)})
	idx.Upsert(SessionIndexEntry{SessionKey: "ag/c222", SessionType: SessionTypeChat, Status: SessionStatusActive,
		CreatedAt: now.Add(-24 * time.Hour), LastActivityAt: now.Add(-30 * 24 * time.Hour)})
	idx.TouchUserActivity("ag/c111", now.Add(-time.Hour))
	idx.TouchUserActivity("ag/c222", now.Add(-30*24*time.Hour))
	if err := idx.SetDefaultChat("ag", "telegram", 111); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetDefaultChat("ag", "discord", 222); err != nil {
		t.Fatal(err)
	}

	// Rebuild with an empty scan (no files on disk — the delegated case).
	if _, err := idx.RebuildIndex(nil); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}

	// Backend rows survive with their activity AND user-activity timestamps...
	e, err := idx.Get("ag/c111")
	if err != nil {
		t.Fatalf("backend row wiped by rebuild: %v", err)
	}
	if e.LastActivityAt.Before(now.Add(-2 * time.Hour)) {
		t.Errorf("activity not preserved: %v", e.LastActivityAt)
	}
	// ...so the default-chat tiebreak (now ordered by last_user_activity_at)
	// still picks the recently user-active chat, not an arbitrary one.
	if got := idx.DefaultSessionKeyForAgent("ag"); got != "ag/c111" {
		t.Errorf("DefaultSessionKeyForAgent = %q, want ag/c111 (most recent user activity)", got)
	}
}

func TestRebuildIndex_PreservesActivityStampsForFileBackedRows(t *testing.T) {
	// Proves the rebuild restores last_user_activity_at and last_cache_touch for
	// FILE-BACKED rows — which are DELETEd and re-INSERTed from the disk scan
	// (the scan only re-derives last_activity_at). Without preservation the
	// re-INSERT would null them, wiping the routing order and the --if-active
	// baseline on every rebuild.
	idx := tempIndex(t)
	now := time.Now().UTC().Truncate(time.Second)

	entry := SessionIndexEntry{
		SessionKey: "ag/c777", FilePath: "/tmp/c777.jsonl",
		CreatedAt: now.Add(-time.Hour), LastActivityAt: now.Add(-time.Hour),
		SessionType: SessionTypeChat, Status: SessionStatusActive,
	}
	idx.Upsert(entry)
	userAt := now.Add(-10 * time.Minute)
	cacheAt := now.Add(-2 * time.Minute)
	idx.TouchUserActivity("ag/c777", userAt)
	idx.TouchCacheTouch("ag/c777", cacheAt)

	// Rebuild re-supplies the same file-backed entry from the "scan".
	if _, err := idx.RebuildIndex([]SessionIndexEntry{entry}); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}

	gotUser, ok := idx.LastUserActivityForAgent("ag")
	if !ok || !gotUser.Equal(userAt) {
		t.Errorf("last_user_activity_at not preserved: got %v (ok=%v), want %v", gotUser, ok, userAt)
	}
	gotCache, ok := idx.LastCacheTouch("ag/c777")
	if !ok || !gotCache.Equal(cacheAt) {
		t.Errorf("last_cache_touch not preserved: got %v (ok=%v), want %v", gotCache, ok, cacheAt)
	}
}

// TestDefaultSessionKeyForAgentOn proves the platform-preference rungs: the
// preferred platform's pinned default wins over a more-active pin elsewhere;
// without a pin, the preferred platform's most recently active registered
// chat wins; with no presence on the preferred platform at all, resolution
// falls through to the activity-ordered behavior.
func TestDefaultSessionKeyForAgentOn(t *testing.T) {
	idx, err := NewSessionIndex(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close() //nolint:errcheck

	now := time.Now()
	// Discord chat 222 far more user-active than telegram chat 111; both pinned.
	idx.Upsert(SessionIndexEntry{SessionKey: "ag/c111", SessionType: SessionTypeChat, Status: SessionStatusActive,
		CreatedAt: now.Add(-48 * time.Hour), LastActivityAt: now.Add(-24 * time.Hour)})
	idx.Upsert(SessionIndexEntry{SessionKey: "ag/c222", SessionType: SessionTypeChat, Status: SessionStatusActive,
		CreatedAt: now.Add(-48 * time.Hour), LastActivityAt: now.Add(-time.Minute)})
	idx.TouchUserActivity("ag/c111", now.Add(-24*time.Hour))
	idx.TouchUserActivity("ag/c222", now.Add(-time.Minute))
	if err := idx.SetChatMetadata("ag", "telegram", 111, "registered", "true"); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetChatMetadata("ag", "discord", 222, "registered", "true"); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetDefaultChat("ag", "telegram", 111); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetDefaultChat("ag", "discord", 222); err != nil {
		t.Fatal(err)
	}

	// Preferred platform's pin wins despite lower activity.
	if got := idx.DefaultSessionKeyForAgentOn("ag", "telegram"); got != "ag/c111" {
		t.Errorf("preferred-pin rung = %q, want ag/c111", got)
	}
	// No preference → activity ordering picks discord.
	if got := idx.DefaultSessionKeyForAgentOn("ag", ""); got != "ag/c222" {
		t.Errorf("no-preference = %q, want ag/c222", got)
	}
	// Preferred platform without a pin: most active registered chat there.
	if err := idx.ClearDefaultChat("ag", "telegram"); err != nil {
		t.Fatal(err)
	}
	if got := idx.DefaultSessionKeyForAgentOn("ag", "telegram"); got != "ag/c111" {
		t.Errorf("preferred-registered rung = %q, want ag/c111", got)
	}
	// No presence on the preferred platform → falls through to discord's pin.
	if got := idx.DefaultSessionKeyForAgentOn("ag", "app"); got != "ag/c222" {
		t.Errorf("absent-platform fallthrough = %q, want ag/c222", got)
	}
}

func TestDefaultSessionKeyForAgentOn_RegisteredFilter(t *testing.T) {
	// Proves the preferred-platform "most recently active registered chat" rung
	// only considers chats with registered='true' — a chat carrying some other
	// metadata row (alias/features/etc.) but never registered is NOT eligible,
	// even if it is the more recently user-active of the two.
	idx := tempIndex(t)
	now := time.Now().UTC().Truncate(time.Second)

	idx.Upsert(SessionIndexEntry{SessionKey: "ag/c111", SessionType: SessionTypeChat,
		Status: SessionStatusActive, CreatedAt: now.Add(-2 * time.Hour), LastActivityAt: now.Add(-time.Hour)})
	idx.Upsert(SessionIndexEntry{SessionKey: "ag/c999", SessionType: SessionTypeChat,
		Status: SessionStatusActive, CreatedAt: now.Add(-2 * time.Hour), LastActivityAt: now})
	idx.TouchUserActivity("ag/c111", now.Add(-time.Hour))
	idx.TouchUserActivity("ag/c999", now) // more recent, but not registered

	// 111 is a registered telegram chat; 999 only ever got an alias row.
	if err := idx.SetChatMetadata("ag", "telegram", 111, "registered", "true"); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetChatMetadata("ag", "telegram", 999, "alias", "scratch"); err != nil {
		t.Fatal(err)
	}

	if got := idx.DefaultSessionKeyForAgentOn("ag", "telegram"); got != "ag/c111" {
		t.Errorf("registered-filter rung = %q, want ag/c111 (c999 excluded: not registered)", got)
	}
}

func TestConvRefs_ReturnsPersistedConvIDRows(t *testing.T) {
	// Proves ConvRefs returns exactly the platform's conv_id rows (the chatID
	// hash preimages written at binding creation), skipping other keys, other
	// platforms, and rows with an empty agent or value.
	idx := tempIndex(t)
	for _, row := range []struct {
		agent, platform string
		chatID          int64
		key, value      string
	}{
		{"ag1", "app", 42, "conv_id", "01AAA"},
		{"ag2", "app", 43, "conv_id", "01BBB"},
		{"ag1", "app", 44, "alias", "not-a-conv"},
		{"ag1", "telegram", 45, "conv_id", "01CCC"},
		{"", "app", 46, "conv_id", "01DDD"},
		{"ag1", "app", 47, "conv_id", ""},
	} {
		if err := idx.SetChatMetadata(row.agent, row.platform, row.chatID, row.key, row.value); err != nil {
			t.Fatal(err)
		}
	}

	refs, err := idx.ConvRefs("app")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range refs {
		got[r.ConvID] = r.AgentID
	}
	if len(got) != 2 || got["01AAA"] != "ag1" || got["01BBB"] != "ag2" {
		t.Fatalf("ConvRefs = %v, want {01AAA: ag1, 01BBB: ag2}", got)
	}
}

func TestSessionIndex_LastUserActivityForAgent(t *testing.T) {
	// Proves the agent-level user-activity signal is a max over that agent's
	// sessions (derived, not separately stored), scoped per agent, and using
	// unixepoch ordering (not lexical) so DST offset changes sort correctly.
	idx := tempIndex(t)
	now := time.Now().UTC().Truncate(time.Second)

	idx.Upsert(SessionIndexEntry{SessionKey: "bot/c1", CreatedAt: now, SessionType: SessionTypeChat, Status: SessionStatusActive})
	idx.Upsert(SessionIndexEntry{SessionKey: "bot/c2", CreatedAt: now, SessionType: SessionTypeChat, Status: SessionStatusActive})
	idx.Upsert(SessionIndexEntry{SessionKey: "other/c1", CreatedAt: now, SessionType: SessionTypeChat, Status: SessionStatusActive})

	// No user activity recorded yet.
	if _, ok := idx.LastUserActivityForAgent("bot"); ok {
		t.Fatalf("expected no user activity for bot, got ok")
	}

	older := now.Add(-time.Hour)
	newer := now.Add(-time.Minute)
	idx.TouchUserActivity("bot/c1", older)
	idx.TouchUserActivity("bot/c2", newer)
	idx.TouchUserActivity("other/c1", now) // different agent — must not leak

	got, ok := idx.LastUserActivityForAgent("bot")
	if !ok {
		t.Fatalf("expected user activity for bot after touches")
	}
	if diff := got.Sub(newer); diff > time.Second || diff < -time.Second {
		t.Fatalf("LastUserActivityForAgent(bot) = %v, want ~%v (max over bot's sessions)", got, newer)
	}
}

func TestRecordTurnActivity_MergedWrite(t *testing.T) {
	// Proves the single merged upsert: last_cache_touch always advances;
	// last_activity_at advances only when bumpActivity; last_user_activity_at
	// advances only when bumpUser; the row is created if missing; and identity
	// columns (created_at) are preserved across turns.
	idx := tempIndex(t)
	key := "bot/c1"

	// First turn (interactive): creates the row, sets all three.
	idx.RecordTurnActivity(SessionIndexEntry{
		SessionKey: key, CreatedAt: time.Now().UTC(), SessionType: SessionTypeChat, Status: SessionStatusActive,
	}, true, true)
	e, err := idx.Get(key)
	if err != nil {
		t.Fatalf("row not created: %v", err)
	}
	createdAt := e.CreatedAt
	if _, ok := idx.LastCacheTouch(key); !ok {
		t.Fatal("cache touch not set on first turn")
	}
	act1 := e.LastActivityAt
	if _, ok := idx.LastUserActivity(key); !ok {
		t.Fatal("user activity not set on interactive first turn")
	}

	// Memory turn (bumpActivity=false, bumpUser=false): cache advances, activity
	// and user do NOT; created_at preserved.
	u1, _ := idx.LastUserActivity(key)
	time.Sleep(1100 * time.Millisecond)
	idx.RecordTurnActivity(SessionIndexEntry{
		SessionKey: key, CreatedAt: time.Now().UTC(), SessionType: SessionTypeChat, Status: SessionStatusActive,
	}, false, false)
	e, _ = idx.Get(key)
	if !e.LastActivityAt.Equal(act1) {
		t.Errorf("memory turn advanced last_activity_at: %v != %v", e.LastActivityAt, act1)
	}
	if u2, _ := idx.LastUserActivity(key); !u2.Equal(u1) {
		t.Errorf("memory turn advanced last_user_activity_at: %v != %v", u2, u1)
	}
	if !e.CreatedAt.Equal(createdAt) {
		t.Errorf("created_at not preserved across turns: %v != %v", e.CreatedAt, createdAt)
	}
	c1, _ := idx.LastCacheTouch(key)
	if !c1.After(act1) {
		t.Errorf("cache touch did not advance on memory turn: %v", c1)
	}

	// Non-interactive substantive turn (bumpActivity=true, bumpUser=false):
	// activity advances, user does NOT.
	time.Sleep(1100 * time.Millisecond)
	idx.RecordTurnActivity(SessionIndexEntry{
		SessionKey: key, CreatedAt: time.Now().UTC(), SessionType: SessionTypeChat, Status: SessionStatusActive,
	}, true, false)
	e, _ = idx.Get(key)
	if !e.LastActivityAt.After(act1) {
		t.Errorf("substantive turn did not advance last_activity_at: %v", e.LastActivityAt)
	}
	if u3, _ := idx.LastUserActivity(key); !u3.Equal(u1) {
		t.Errorf("non-interactive turn advanced last_user_activity_at: %v != %v", u3, u1)
	}
}

func TestSessionIndex_LastUserActivity_PerSession(t *testing.T) {
	// Proves LastUserActivity is per-session (NOT the agent-wide max): it backs
	// the session-scoped --if-user-active gate. bot/c1 and bot/c2 report their
	// OWN times, and a session with no user activity reports (_, false) even
	// though a sibling session of the same agent is active.
	idx := tempIndex(t)
	now := time.Now().UTC().Truncate(time.Second)

	idx.Upsert(SessionIndexEntry{SessionKey: "bot/c1", CreatedAt: now, SessionType: SessionTypeChat, Status: SessionStatusActive})
	idx.Upsert(SessionIndexEntry{SessionKey: "bot/c2", CreatedAt: now, SessionType: SessionTypeChat, Status: SessionStatusActive})

	older := now.Add(-time.Hour)
	idx.TouchUserActivity("bot/c1", older)
	// bot/c2 gets NO user activity.

	if got, ok := idx.LastUserActivity("bot/c1"); !ok || got.Sub(older) > time.Second || got.Sub(older) < -time.Second {
		t.Fatalf("LastUserActivity(bot/c1) = %v ok=%v, want ~%v", got, ok, older)
	}
	if _, ok := idx.LastUserActivity("bot/c2"); ok {
		t.Errorf("LastUserActivity(bot/c2) reported activity; want none (per-session, not agent-max)")
	}
	if _, ok := idx.LastUserActivity("bot/nonexistent"); ok {
		t.Errorf("LastUserActivity(unknown) reported activity; want none")
	}
}
