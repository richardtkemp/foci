package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/provider"
)

// backdateSession makes a session idle since at: the index's last activity
// AND the file's mtime, which the sweep re-checks under the store lock before
// it removes anything (#3066 F11). A missing file is left alone.
func backdateSession(t *testing.T, store *Store, idx *SessionIndex, key string, at time.Time) {
	t.Helper()
	idx.UpdateActivity(key, at)
	path, err := store.SessionPath(key)
	if err != nil {
		return
	}
	if err := os.Chtimes(path, at, at); err != nil && !os.IsNotExist(err) {
		t.Fatalf("backdate %s: %v", path, err)
	}
}

// #3066 F11: the candidate list is a snapshot. A session written after it
// was taken (index activity still stale, file freshly written) must not be
// archived, or its new messages go into a file that is then unlinked.
func TestArchiveSweep_SkipsSessionWrittenSinceSnapshot(t *testing.T) {
	store := NewStore(t.TempDir())
	idx := tempIndex(t)
	key := "bot/c100"
	if err := store.TestAppend(key, msg("user", "before")); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Rebuild(store); err != nil {
		t.Fatal(err)
	}
	backdateSession(t, store, idx, key, time.Now().Add(-48*time.Hour))

	// The session resumes: a real Append moves the file's mtime to now, but
	// the index still says it has been idle for two days.
	if err := store.TestAppend(key, msg("user", "resumed")); err != nil {
		t.Fatal(err)
	}

	n, err := ArchiveSweep(store, idx, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("archived %d, want 0: a session written since the snapshot was archived", n)
	}
	got, err := store.Load(key)
	if err != nil || len(got) != 2 {
		t.Fatalf("Load = %d messages, err %v; want both messages live", len(got), err)
	}
}

// #3066 F11: the sweep must take the store lock Append/Load/Replace use, so
// its compress-then-unlink cannot interleave with a write. While a writer
// holds the lock the sweep may not remove the file; a write made under the
// lock must survive.
func TestArchiveSweep_WaitsForStoreLock(t *testing.T) {
	store := NewStore(t.TempDir())
	idx := tempIndex(t)
	key := "bot/c100"
	if err := store.TestAppend(key, msg("user", "before")); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Rebuild(store); err != nil {
		t.Fatal(err)
	}
	backdateSession(t, store, idx, key, time.Now().Add(-48*time.Hour))
	path, _ := store.SessionPath(key)

	store.mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := ArchiveSweep(store, idx, 24*time.Hour)
		done <- err
	}()

	// A sweep that ignores the lock finishes (and unlinks the file) in
	// milliseconds; give it ample time to do so. Under heavy load this can
	// only make the test pass falsely, never fail falsely.
	select {
	case err := <-done:
		store.mu.Unlock()
		t.Fatalf("sweep finished while the store lock was held (err=%v)", err)
	case <-time.After(500 * time.Millisecond):
	}
	if _, err := os.Stat(path); err != nil {
		store.mu.Unlock()
		t.Fatalf("live file removed while the store lock was held: %v", err)
	}
	// The writer that holds the lock appends, as appendUnlocked would.
	if err := store.appendUnlocked(key, msg("user", "written under lock")); err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	store.mu.Unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("sweep did not finish after the lock was released")
	}
	got, err := store.Load(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Content[0].Text != "written under lock" {
		t.Fatalf("Load = %+v; the write made under the lock was lost", got)
	}
}

// #3066 F12: a replacement that fails part way must leave the original
// history live and create no archive.
func TestReplaceFailureKeepsOriginalLive(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/imain"
	if err := s.TestAppend(key, msg("user", "ORIGINAL")); err != nil {
		t.Fatal(err)
	}
	path, _ := s.SessionPath(key)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	err = s.TestReplace(key, []provider.Message{
		msg("user", "PARTIAL REPLACEMENT"),
		{Role: "assistant", Content: []provider.ContentBlock{{Type: "tool_use", ID: "bad", Input: json.RawMessage(`{`)}}},
	})
	if err == nil {
		t.Fatal("Replace accepted an unmarshalable message")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("live file gone after failed Replace: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("live file changed by a failed Replace:\n%s", after)
	}
	archives, _ := filepath.Glob(strings.TrimSuffix(path, ".jsonl") + ".*.jsonl")
	if len(archives) != 0 {
		t.Fatalf("failed Replace left archives %v", archives)
	}
	if _, err := os.Stat(path + ".replace-tmp"); !os.IsNotExist(err) {
		t.Fatalf("failed Replace left its temp file (stat err %v)", err)
	}
}

// A successful Replace still archives the old history and keeps the session's
// creation time, and the archive is a separate file from the new live one.
func TestReplaceArchivesOldAndPublishesNew(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/imain"
	if err := s.TestAppend(key, msg("user", "OLD")); err != nil {
		t.Fatal(err)
	}
	created := s.CreatedAt(key)

	if err := s.TestReplace(key, []provider.Message{msg("user", "NEW")}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(key)
	if err != nil || len(got) != 1 || got[0].Content[0].Text != "NEW" {
		t.Fatalf("Load = %+v, err %v; want [NEW]", got, err)
	}
	if c := s.CreatedAt(key); c != created {
		t.Fatalf("CreatedAt = %q, want %q kept", c, created)
	}
	path, _ := s.SessionPath(key)
	archive := latestArchivePath(path)
	if archive == "" {
		t.Fatal("no archive written")
	}
	old, err := s.loadArchiveFile(archive, key)
	if err != nil || len(old) != 1 || old[0].Content[0].Text != "OLD" {
		t.Fatalf("archive = %+v, err %v; want [OLD]", old, err)
	}
	// A later append to the live file must not reach the archive (a hard
	// link left in place would share the inode).
	if err := s.TestAppend(key, msg("user", "LATER")); err != nil {
		t.Fatal(err)
	}
	old, err = s.loadArchiveFile(archive, key)
	if err != nil || len(old) != 1 {
		t.Fatalf("archive changed by a live append: %+v, err %v", old, err)
	}
}
