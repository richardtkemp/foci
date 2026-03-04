package session

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/provider"
)

func TestArchiveSweep_GzipsIdleSessions(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	idx := tempIndex(t)

	// Create two sessions
	store.Append("bot/c100/1000000000", msg("user", "hello"))
	store.Append("bot/c200/1000000000", msg("user", "world"))

	// Rebuild index
	n, err := idx.Rebuild(store)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2, got %d", n)
	}

	// Set last activity to the past so both qualify for archival.
	// LastActivityAt must differ from CreatedAt to trigger the Upsert update path.
	past := time.Now().UTC().Add(-48 * time.Hour)
	for _, key := range []string{"bot/c100/1000000000", "bot/c200/1000000000"} {
		idx.Upsert(SessionIndexEntry{
			SessionKey:     key,
			FilePath:       mustSessionPath(t, store, key),
			CreatedAt:      past.Add(-time.Hour),
			LastActivityAt: past,
			SessionType:    SessionTypeChat,
			Status:         SessionStatusActive,
		})
	}

	// Run sweep with 24h threshold
	archived, err := ArchiveSweep(store, idx, 24*time.Hour)
	if err != nil {
		t.Fatalf("ArchiveSweep: %v", err)
	}
	if archived != 2 {
		t.Fatalf("expected 2 archived, got %d", archived)
	}

	// Verify .jsonl files are gone and .jsonl.gz files exist
	for _, key := range []string{"bot/c100/1000000000", "bot/c200/1000000000"} {
		path := mustSessionPath(t, store, key)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed, but it exists", path)
		}
		if _, err := os.Stat(path + ".gz"); err != nil {
			t.Errorf("expected %s.gz to exist: %v", path, err)
		}
	}

	// Verify index status
	entries, _ := idx.Query(QueryOptions{Status: string(SessionStatusArchived)})
	if len(entries) != 2 {
		t.Fatalf("expected 2 archived entries, got %d", len(entries))
	}
}

func TestArchiveSweep_SkipsRecentSessions(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	idx := tempIndex(t)

	store.Append("bot/c100/1000000000", msg("user", "hello"))
	idx.Rebuild(store)

	// Last activity is now (recent), so it should not be archived
	archived, err := ArchiveSweep(store, idx, 24*time.Hour)
	if err != nil {
		t.Fatalf("ArchiveSweep: %v", err)
	}
	if archived != 0 {
		t.Fatalf("expected 0 archived (recent session), got %d", archived)
	}
}

func TestArchiveSweep_SkipsSessionsWithActiveBranches(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	idx := tempIndex(t)

	// Create parent and branch
	store.Append("bot/c100/1000000000", msg("user", "hello"))
	store.CreateBranchWithOptions("bot/c100/1000000000", "bot/imb-1/1000000000", BranchOptions{})
	idx.Rebuild(store)

	// Set parent to old, but branch is active
	past := time.Now().UTC().Add(-48 * time.Hour)
	parentPath := mustSessionPath(t, store, "agent:bot:chat:100")
	idx.Upsert(SessionIndexEntry{
		SessionKey:     "bot/c100/1000000000",
		FilePath:       parentPath,
		CreatedAt:      past.Add(-time.Hour),
		LastActivityAt: past,
		SessionType:    SessionTypeChat,
		Status:         SessionStatusActive,
	})

	archived, err := ArchiveSweep(store, idx, 24*time.Hour)
	if err != nil {
		t.Fatalf("ArchiveSweep: %v", err)
	}
	if archived != 0 {
		t.Fatalf("expected 0 archived (has active branch), got %d", archived)
	}
}

func TestArchiveSweep_GzipsArchiveFiles(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	idx := tempIndex(t)

	// Create a session and compact it (creates .1.jsonl archive)
	store.Append("bot/c100/1000000000", msg("user", "hello"))
	store.Replace("agent:bot:chat:100", []provider.Message{msg("user", "compacted")})
	idx.Rebuild(store)

	// Set last activity to past
	past := time.Now().UTC().Add(-48 * time.Hour)
	path := mustSessionPath(t, store, "agent:bot:chat:100")
	idx.Upsert(SessionIndexEntry{
		SessionKey:     "bot/c100/1000000000",
		FilePath:       path,
		CreatedAt:      past.Add(-time.Hour),
		LastActivityAt: past,
		SessionType:    SessionTypeChat,
		Status:         SessionStatusActive,
	})

	// Verify archive file exists before sweep - find the timestamp archive
	sessionDir := filepath.Dir(path)
	entries, err2 := os.ReadDir(sessionDir)
	if err2 != nil {
		t.Fatalf("read dir: %v", err2)
	}
	var archivePath string
	for _, e := range entries {
		if isArchiveFile(e.Name()) && strings.HasPrefix(e.Name(), "100.") {
			archivePath = filepath.Join(sessionDir, e.Name())
			break
		}
	}
	if archivePath == "" {
		t.Fatal("expected archive file to exist before sweep")
	}

	archived, err := ArchiveSweep(store, idx, 24*time.Hour)
	if err != nil {
		t.Fatalf("ArchiveSweep: %v", err)
	}
	if archived != 1 {
		t.Fatalf("expected 1 archived, got %d", archived)
	}

	// Verify both .jsonl and .1.jsonl are gzipped
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected main session file to be removed")
	}
	if _, err := os.Stat(path + ".gz"); err != nil {
		t.Errorf("expected main .gz to exist: %v", err)
	}
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Errorf("expected archive file to be removed")
	}
	if _, err := os.Stat(archivePath + ".gz"); err != nil {
		t.Errorf("expected archive .gz to exist: %v", err)
	}
}

func TestDecompressIfGzipped(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	// Create a session, then manually gzip it
	store.Append("bot/c100/1000000000", msg("user", "hello"))
	path := mustSessionPath(t, store, "agent:bot:chat:100")

	// Read original content
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}

	// Gzip the file
	gzPath := path + ".gz"
	gf, err := os.Create(gzPath)
	if err != nil {
		t.Fatalf("create gz: %v", err)
	}
	gw := gzip.NewWriter(gf)
	gw.Write(original)
	gw.Close()
	gf.Close()
	os.Remove(path) // remove original

	// Verify .jsonl is gone
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected .jsonl to be removed")
	}

	// Load should transparently decompress
	msgs, err := store.Load("agent:bot:chat:100")
	if err != nil {
		t.Fatalf("Load after gzip: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message after decompression, got %d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("expected user message, got %s", msgs[0].Role)
	}

	// Verify .jsonl is restored and .gz is removed
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected .jsonl to be restored: %v", err)
	}
	if _, err := os.Stat(gzPath); !os.IsNotExist(err) {
		t.Errorf("expected .gz to be removed after decompression")
	}
}

func TestScanAllSessions_IncludesArchivesAndGzipped(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	// Create a session with an archive
	store.Append("bot/c100/1000000000", msg("user", "hello"))
	store.Replace("agent:bot:chat:100", []provider.Message{msg("user", "compacted")})

	entries, err := store.ScanAllSessions()
	if err != nil {
		t.Fatalf("ScanAllSessions: %v", err)
	}

	// Should have 2 entries: the current file (active) and the archive (compacted)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	var activeCount, compactedCount int
	for _, e := range entries {
		switch e.Status {
		case SessionStatusActive:
			activeCount++
		case SessionStatusCompacted:
			compactedCount++
		}
	}
	if activeCount != 1 {
		t.Errorf("expected 1 active, got %d", activeCount)
	}
	if compactedCount != 1 {
		t.Errorf("expected 1 compacted, got %d", compactedCount)
	}
}

func TestScanAllSessions_CurrentFileAlwaysActive(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	// Create a session with archives — current file should still be active
	store.Append("agent:bot:chat:100", msg("user", "v1"))
	store.Replace("agent:bot:chat:100", []provider.Message{msg("user", "v2")})
	store.Replace("agent:bot:chat:100", []provider.Message{msg("user", "v3")})

	entries, err := store.ScanAllSessions()
	if err != nil {
		t.Fatalf("ScanAllSessions: %v", err)
	}

	// 1 active + 2 compacted
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	for _, e := range entries {
		if e.SessionKey == "agent:bot:chat:100" && e.Status != SessionStatusActive {
			t.Errorf("current file should be active, got %s", e.Status)
		}
	}
}

func mustSessionPath(t *testing.T, store *Store, key string) string {
	t.Helper()
	path, err := store.SessionPath(key)
	if err != nil {
		t.Fatalf("SessionPath(%s): %v", key, err)
	}
	return path
}
