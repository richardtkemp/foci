package telegram

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempStore(t *testing.T) *ToolDetailStore {
	t.Helper()
	dir := t.TempDir()
	s, err := NewToolDetailStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewToolDetailStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestToolDetailStore_StoreAndLoadAll(t *testing.T) {
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
	if e := m[100]; e.compactText != "compact1" || e.fullInput != "full1" || e.result != "result1" {
		t.Errorf("entry 100 mismatch: %+v", e)
	}
	if e := m[200]; e.compactText != "compact2" || e.fullInput != "full2" || e.result != "result2" {
		t.Errorf("entry 200 mismatch: %+v", e)
	}
}

func TestToolDetailStore_Replace(t *testing.T) {
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
	if e := m[100]; e.compactText != "new" {
		t.Errorf("expected replaced value, got %q", e.compactText)
	}
}

func TestToolDetailStore_Count(t *testing.T) {
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

func TestToolDetailStore_ExpireAndVacuum(t *testing.T) {
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

func TestToolDetailStore_LoadAll_SkipsExpired(t *testing.T) {
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

func TestToolDetailStore_EmptyLoadAll(t *testing.T) {
	s := tempStore(t)

	m, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("expected empty map, got %d entries", len(m))
	}
}

func TestToolDetailStore_InvalidPath(t *testing.T) {
	_, err := NewToolDetailStore("/nonexistent/dir/test.db")
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}

func TestToolDetailStore_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s1, err := NewToolDetailStore(dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	s1.Store(42, "compact", "full", "result")
	s1.Close()

	s2, err := NewToolDetailStore(dbPath)
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
	if e := m[42]; e.compactText != "compact" || e.fullInput != "full" || e.result != "result" {
		t.Errorf("entry mismatch after reopen: %+v", e)
	}
}

func TestToolDetailStore_DbFileCreated(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s, err := NewToolDetailStore(dbPath)
	if err != nil {
		t.Fatalf("NewToolDetailStore: %v", err)
	}
	defer s.Close()

	// Store something to force file creation
	s.Store(1, "a", "b", "c")

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Fatal("database file was not created")
	}
}
