package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/provider"
)

func TestReset_ArchivesInPlace(t *testing.T) {
	// Proves that Store.Reset archives the live root.jsonl in place (as a
	// root.<ts>.jsonl sibling), leaves the session KEY unchanged, fires a
	// SessionStatusReset event carrying the archive path, and that Load on the
	// same key afterwards returns nil (history archived, identity stable).
	dir := t.TempDir()
	store := NewStore(dir)

	key := "bot/c100"
	store.TestAppend(key, msg("user", "hello"))
	store.TestAppend(key, msg("assistant", "hi"))

	livePath := mustSessionPath(t, store, key)
	if _, err := os.Stat(livePath); err != nil {
		t.Fatalf("live file should exist before reset: %v", err)
	}

	var event SessionEvent
	store.OnSessionEvent(func(e SessionEvent) { event = e })

	if err := store.Reset(key); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	// Live file gone; a timestamped archive sibling exists in the same dir.
	if _, err := os.Stat(livePath); !os.IsNotExist(err) {
		t.Error("root.jsonl should not exist after reset")
	}
	entries, err := os.ReadDir(filepath.Dir(livePath))
	if err != nil {
		t.Fatalf("read session dir: %v", err)
	}
	found := ""
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "root.") && isArchiveFile(e.Name()) {
			found = e.Name()
			break
		}
	}
	if found == "" {
		t.Fatal("expected root.<ts>.jsonl archive in session directory")
	}

	// Event: same key (no rotation), reset status, archive path set.
	if event.Status != SessionStatusReset {
		t.Errorf("event.Status = %q, want reset", event.Status)
	}
	if event.Key != key {
		t.Errorf("event.Key = %q, want %q (key must not change)", event.Key, key)
	}
	if filepath.Base(event.ArchivePath) != found {
		t.Errorf("event.ArchivePath = %q, want archive %q", event.ArchivePath, found)
	}

	// The key still loads — as an empty session.
	msgs, err := store.Load(key)
	if err != nil {
		t.Fatalf("Load after reset: %v", err)
	}
	if msgs != nil {
		t.Errorf("Load after reset = %v, want nil", msgs)
	}
}

func TestReset_ReactivatesOnNextAppend(t *testing.T) {
	// Proves the reset re-activation flow: after Reset, the next Append lazily
	// recreates the session file under the SAME key and fires a fresh
	// SessionStatusActive (created) event, so index listeners flip the row back
	// to active. Old history stays in the archive; the new file starts clean.
	dir := t.TempDir()
	store := NewStore(dir)

	key := "bot/c100"
	store.TestAppend(key, msg("user", "before reset"))
	if err := store.Reset(key); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	var events []SessionEvent
	store.OnSessionEvent(func(e SessionEvent) { events = append(events, e) })

	store.TestAppend(key, msg("user", "after reset"))

	if len(events) != 1 || events[0].Status != SessionStatusActive || events[0].Key != key {
		t.Fatalf("expected one active (created) event for %q, got %+v", key, events)
	}

	msgs, err := store.Load(key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 1 || provider.TextOf(msgs[0].Content) != "after reset" {
		t.Errorf("expected only post-reset message, got %+v", msgs)
	}
}

func TestReset_NoFile(t *testing.T) {
	// Proves that Reset on a session with no file is a no-op that still fires
	// the reset event (with empty ArchivePath) and returns no error — /reset on
	// a never-used or already-reset session must not fail.
	store := NewStore(t.TempDir())

	var event SessionEvent
	store.OnSessionEvent(func(e SessionEvent) { event = e })

	if err := store.Reset("bot/c100"); err != nil {
		t.Fatalf("Reset on missing file: %v", err)
	}
	if event.Status != SessionStatusReset {
		t.Errorf("event.Status = %q, want reset", event.Status)
	}
	if event.ArchivePath != "" {
		t.Errorf("event.ArchivePath = %q, want empty (nothing archived)", event.ArchivePath)
	}
}
