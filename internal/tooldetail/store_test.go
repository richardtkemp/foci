package tooldetail

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStore_StoreAndLoadAll(t *testing.T) {
	// Verifies that multiple entries can be stored and then loaded back by LoadAll
	// with all fields (compact text, full input, result) intact.
	s := tempStore(t)

	s.Store(100, "compact1", "full1", "result1")
	s.Store(200, "compact2", "full2", "result2")

	m, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(m) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(m))
	}
	if e := m[100]; e.CompactText != "compact1" || e.FullInput != "full1" || e.Result != "result1" {
		t.Errorf("entry 100 mismatch: %+v", e)
	}
	if e := m[200]; e.CompactText != "compact2" || e.FullInput != "full2" || e.Result != "result2" {
		t.Errorf("entry 200 mismatch: %+v", e)
	}
}

func TestStore_Replace(t *testing.T) {
	// Verifies that storing a second entry with the same message ID replaces
	// the first, so LoadAll returns only one entry with the updated values.
	s := tempStore(t)

	s.Store(100, "old", "old", "old")
	s.Store(100, "new", "new", "new")

	m, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(m) != 1 {
		t.Fatalf("expected 1 entry after replace, got %d", len(m))
	}
	if e := m[100]; e.CompactText != "new" {
		t.Errorf("expected replaced value, got %q", e.CompactText)
	}
}

func TestStore_Count(t *testing.T) {
	// Verifies that Count() accurately reflects the number of stored entries,
	// starting at zero and incrementing with each distinct message ID stored.
	s := tempStore(t)

	if s.Count() != 0 {
		t.Fatalf("expected 0, got %d", s.Count())
	}

	s.Store(1, "a", "b", "c")
	s.Store(2, "d", "e", "f")

	if s.Count() != 2 {
		t.Fatalf("expected 2, got %d", s.Count())
	}
}

func TestStore_ExpireAndVacuum(t *testing.T) {
	// Verifies that ExpireAndVacuum removes entries older than 48 hours while
	// leaving fresh entries intact, by backdating one row directly in the DB.
	s := tempStore(t)

	// Insert a row with a manually backdated created_at to simulate old entry
	s.mu.Lock()
	old := time.Now().Add(-49 * time.Hour).UTC().Format(time.RFC3339Nano)
	s.db.Exec(
		`INSERT INTO tool_call_details (message_id, compact_text, full_input, result, created_at) VALUES (?, ?, ?, ?, ?)`,
		999, "old", "old", "old", old)
	s.mu.Unlock()

	// Also insert a fresh entry
	s.Store(1000, "fresh", "fresh", "fresh")

	if s.Count() != 2 {
		t.Fatalf("expected 2 before expire, got %d", s.Count())
	}

	s.ExpireAndVacuum()

	if s.Count() != 1 {
		t.Fatalf("expected 1 after expire, got %d", s.Count())
	}

	m, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if _, ok := m[1000]; !ok {
		t.Error("fresh entry should survive expire")
	}
	if _, ok := m[999]; ok {
		t.Error("old entry should be expired")
	}
}

func TestStore_LoadAll_SkipsExpired(t *testing.T) {
	// Verifies that LoadAll itself filters out entries older than 48 hours,
	// even without calling ExpireAndVacuum first.
	s := tempStore(t)

	// Insert an old entry directly
	s.mu.Lock()
	old := time.Now().Add(-49 * time.Hour).UTC().Format(time.RFC3339Nano)
	s.db.Exec(
		`INSERT INTO tool_call_details (message_id, compact_text, full_input, result, created_at) VALUES (?, ?, ?, ?, ?)`,
		999, "old", "old", "old", old)
	s.mu.Unlock()

	m, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("LoadAll should skip entries older than 48h, got %d", len(m))
	}
}

func TestStore_EmptyLoadAll(t *testing.T) {
	// Verifies that LoadAll returns an empty map (not an error or nil) when the
	// store has no entries.
	s := tempStore(t)

	m, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("expected empty map, got %d entries", len(m))
	}
}

func TestStore_InvalidPath(t *testing.T) {
	// Verifies that NewStore returns an error when given a path in a
	// non-existent directory, rather than panicking or silently failing.
	_, err := NewStore("/nonexistent/dir/test.db")
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}

func TestStore_PersistsAcrossReopen(t *testing.T) {
	// Verifies that data written to the store persists after Close() and
	// is fully readable when the database is reopened at the same path.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s1, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	s1.Store(42, "compact", "full", "result")
	s1.Close()

	s2, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()

	m, err := s2.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll after reopen: %v", err)
	}
	if len(m) != 1 {
		t.Fatalf("expected 1 entry after reopen, got %d", len(m))
	}
	if e := m[42]; e.CompactText != "compact" || e.FullInput != "full" || e.Result != "result" {
		t.Errorf("entry mismatch after reopen: %+v", e)
	}
}

func TestStore_DbFileCreated(t *testing.T) {
	// Verifies that the SQLite database file is physically created on disk after
	// data is stored, confirming the store is backed by a real file.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	// Store something to force file creation
	s.Store(1, "a", "b", "c")

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Fatal("database file was not created")
	}
}
