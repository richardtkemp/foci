package session

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/provider"
)

func TestArchiveSweep_GzipsIdleSessions(t *testing.T) {
	// Proves that ArchiveSweep gzips all sessions whose last activity exceeds the
	// maxAge threshold, removes the original .jsonl files, and marks them archived
	// in the index.
	dir := t.TempDir()
	store := NewStore(dir)
	idx := tempIndex(t)

	// Create two sessions
	store.TestAppend("bot/c100/1000000000", msg("user", "hello"))
	store.TestAppend("bot/c200/1000000000", msg("user", "world"))

	// Rebuild index
	n, err := idx.Rebuild(store)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2, got %d", n)
	}

	// Force last_activity_at to 48h ago so both qualify for archival.
	// Must use UpdateActivity since Upsert only moves activity forward.
	past := time.Now().UTC().Add(-48 * time.Hour)
	for _, key := range []string{"bot/c100/1000000000", "bot/c200/1000000000"} {
		idx.UpdateActivity(key, past)
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
	// Proves that ArchiveSweep leaves sessions untouched when their last activity
	// is within the maxAge window.
	dir := t.TempDir()
	store := NewStore(dir)
	idx := tempIndex(t)

	store.TestAppend("bot/c100/1000000000", msg("user", "hello"))
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
	// Proves that a parent session is not archived when it has at least one
	// active branch, even if the parent itself is old enough to qualify.
	dir := t.TempDir()
	store := NewStore(dir)
	idx := tempIndex(t)

	// Create parent and branch
	store.TestAppend("bot/c100/1000000000", msg("user", "hello"))
	store.CreateBranchWithOptions("bot/c100/1000000000", "bot/c100/1000000000/b1000000001", BranchOptions{})
	idx.Rebuild(store)

	// Set parent to old, but branch is still active
	past := time.Now().UTC().Add(-48 * time.Hour)
	idx.UpdateActivity("bot/c100/1000000000", past)

	archived, err := ArchiveSweep(store, idx, 24*time.Hour)
	if err != nil {
		t.Fatalf("ArchiveSweep: %v", err)
	}
	if archived != 0 {
		t.Fatalf("expected 0 archived (has active branch), got %d", archived)
	}
}

func TestArchiveSweep_SkipsCurrentChatSession(t *testing.T) {
	// Verifies that a session registered as the current chat session (via
	// chat_metadata) is never archived, even when it exceeds the maxAge threshold.
	dir := t.TempDir()
	store := NewStore(dir)
	idx := tempIndex(t)

	// Create two chat sessions for the same agent+chat (different version timestamps)
	store.TestAppend("bot/c100/1000000000", msg("user", "old"))
	store.TestAppend("bot/c100/2000000000", msg("user", "current"))
	idx.Rebuild(store)

	// Mark the newer one as the current session via chat_metadata
	idx.SetChatMetadata("bot", 100, "session_key", "bot/c100/2000000000")

	// Set both to old activity so they'd normally both qualify
	past := time.Now().UTC().Add(-48 * time.Hour)
	idx.UpdateActivity("bot/c100/1000000000", past)
	idx.UpdateActivity("bot/c100/2000000000", past)

	archived, err := ArchiveSweep(store, idx, 24*time.Hour)
	if err != nil {
		t.Fatalf("ArchiveSweep: %v", err)
	}

	// Only the old session should be archived; the current one is protected
	if archived != 1 {
		t.Fatalf("expected 1 archived, got %d", archived)
	}

	// Verify the current session file still exists uncompressed
	currentPath := mustSessionPath(t, store, "bot/c100/2000000000")
	if _, err := os.Stat(currentPath); err != nil {
		t.Errorf("current session should still exist: %v", err)
	}

	// Verify the old session was archived
	oldPath := mustSessionPath(t, store, "bot/c100/1000000000")
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("old session should be removed")
	}
	if _, err := os.Stat(oldPath + ".gz"); err != nil {
		t.Errorf("old session .gz should exist: %v", err)
	}
}

func TestArchiveSweep_GzipsArchiveFiles(t *testing.T) {
	// Proves that ArchiveSweep also compresses numbered archive files alongside
	// the main .jsonl — both are gzipped and the originals removed.
	dir := t.TempDir()
	store := NewStore(dir)
	idx := tempIndex(t)

	// Create a session and compact it (creates numbered archive)
	store.TestAppend("bot/c100/1000000000", msg("user", "hello"))
	store.TestReplace("bot/c100/1000000000", []provider.Message{msg("user", "compacted")})
	idx.Rebuild(store)

	// Set last activity to past
	past := time.Now().UTC().Add(-48 * time.Hour)
	path := mustSessionPath(t, store, "bot/c100/1000000000")
	idx.UpdateActivity("bot/c100/1000000000", past)

	// Verify archive file exists before sweep
	sessionDir := filepath.Dir(path)
	dirEntries, err2 := os.ReadDir(sessionDir)
	if err2 != nil {
		t.Fatalf("read dir: %v", err2)
	}
	var archivePath string
	for _, e := range dirEntries {
		if isArchiveFile(e.Name()) {
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

	// Verify both .jsonl and archive are gzipped
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
	// Proves that Load transparently decompresses a .jsonl.gz file: the original
	// content is returned, the .jsonl is restored on disk, and the .gz is removed.
	dir := t.TempDir()
	store := NewStore(dir)

	// Create a session, then manually gzip it
	store.TestAppend("bot/c100/1000000000", msg("user", "hello"))
	path := mustSessionPath(t, store, "bot/c100/1000000000")

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
	msgs, err := store.Load("bot/c100/1000000000")
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
	// Proves that ScanAllSessions enumerates both the active current file and
	// compacted archive files within the same session directory, each with the
	// correct status.
	dir := t.TempDir()
	store := NewStore(dir)

	// Create a session with an archive
	store.TestAppend("bot/c100/1000000000", msg("user", "hello"))
	store.TestReplace("bot/c100/1000000000", []provider.Message{msg("user", "compacted")})

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
	// Proves that no matter how many archive rotations have occurred, the
	// current root.jsonl file is always reported as active while archives
	// are reported as compacted.
	dir := t.TempDir()
	store := NewStore(dir)

	// Create a session with archives — current file should still be active
	store.TestAppend("bot/c100/1000000000", msg("user", "v1"))
	store.TestReplace("bot/c100/1000000000", []provider.Message{msg("user", "v2")})
	store.TestReplace("bot/c100/1000000000", []provider.Message{msg("user", "v3")})

	entries, err := store.ScanAllSessions()
	if err != nil {
		t.Fatalf("ScanAllSessions: %v", err)
	}

	// 1 active + 2 compacted
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	for _, e := range entries {
		if strings.HasSuffix(e.SessionKey, "/root") || e.SessionKey == "bot/c100/1000000000" {
			if e.Status != SessionStatusActive {
				t.Errorf("current file should be active, got %s for key %s", e.Status, e.SessionKey)
			}
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
