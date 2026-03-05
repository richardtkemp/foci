package memory

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func testScratchpad(t *testing.T) *Scratchpad {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := NewScratchpad(dbPath)
	if err != nil {
		t.Fatalf("NewScratchpad: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestScratchpadWriteRead(t *testing.T) {
	s := testScratchpad(t)

	if err := s.Write("test", "investigation", "checking FTS5 phrase boosting"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	content, err := s.Read("test", "investigation")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if content != "checking FTS5 phrase boosting" {
		t.Errorf("content = %q", content)
	}
}

func TestScratchpadReadMissing(t *testing.T) {
	s := testScratchpad(t)

	content, err := s.Read("test", "nonexistent")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if content != "" {
		t.Errorf("expected empty for missing key, got %q", content)
	}
}

func TestScratchpadOverwrite(t *testing.T) {
	s := testScratchpad(t)

	s.Write("test", "notes", "first version")
	s.Write("test", "notes", "updated version")

	content, _ := s.Read("test", "notes")
	if content != "updated version" {
		t.Errorf("content = %q, want updated version", content)
	}
}

func TestScratchpadClear(t *testing.T) {
	s := testScratchpad(t)

	s.Write("test", "temp", "temporary data")
	if err := s.Clear("test", "temp"); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	content, _ := s.Read("test", "temp")
	if content != "" {
		t.Errorf("expected empty after clear, got %q", content)
	}
}

func TestScratchpadClearNonexistent(t *testing.T) {
	s := testScratchpad(t)

	// Should not error
	if err := s.Clear("test", "nonexistent"); err != nil {
		t.Fatalf("Clear nonexistent: %v", err)
	}
}

func TestScratchpadAll(t *testing.T) {
	s := testScratchpad(t)

	s.Write("test", "alpha", "first")
	s.Write("test", "beta", "second")
	s.Write("test", "gamma", "third")

	entries, err := s.All("test")
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Ordered by key
	if entries[0].Key != "alpha" || entries[1].Key != "beta" || entries[2].Key != "gamma" {
		t.Errorf("unexpected order: %v, %v, %v", entries[0].Key, entries[1].Key, entries[2].Key)
	}
}

func TestScratchpadAllEmpty(t *testing.T) {
	s := testScratchpad(t)

	entries, err := s.All("test")
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestScratchpadMultipleKeys(t *testing.T) {
	s := testScratchpad(t)

	s.Write("test", "task1", "working on auth")
	s.Write("test", "task2", "investigating cache bug")

	c1, _ := s.Read("test", "task1")
	c2, _ := s.Read("test", "task2")

	if c1 != "working on auth" {
		t.Errorf("task1 = %q", c1)
	}
	if c2 != "investigating cache bug" {
		t.Errorf("task2 = %q", c2)
	}

	// Clear one, other remains
	s.Clear("test", "task1")
	c1, _ = s.Read("test", "task1")
	c2, _ = s.Read("test", "task2")
	if c1 != "" {
		t.Errorf("task1 should be cleared, got %q", c1)
	}
	if c2 != "investigating cache bug" {
		t.Errorf("task2 should remain, got %q", c2)
	}
}

func TestScratchpadAgentIsolation(t *testing.T) {
	s := testScratchpad(t)

	s.Write("agent1", "key1", "agent 1 data")
	s.Write("agent2", "key1", "agent 2 data")

	c1, _ := s.Read("agent1", "key1")
	c2, _ := s.Read("agent2", "key1")

	if c1 != "agent 1 data" {
		t.Errorf("agent1 key1 = %q, want 'agent 1 data'", c1)
	}
	if c2 != "agent 2 data" {
		t.Errorf("agent2 key1 = %q, want 'agent 2 data'", c2)
	}

	entries1, _ := s.All("agent1")
	entries2, _ := s.All("agent2")
	if len(entries1) != 1 {
		t.Errorf("agent1 entries = %d, want 1", len(entries1))
	}
	if len(entries2) != 1 {
		t.Errorf("agent2 entries = %d, want 1", len(entries2))
	}
}

func TestScratchpadBusyTimeout(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := NewScratchpad(dbPath)
	if err != nil {
		t.Fatalf("NewScratchpad: %v", err)
	}
	defer s.Close()

	var timeout int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", timeout)
	}
}



func TestScratchpadList(t *testing.T) {
	s := testScratchpad(t)

	s.Write("test", "alpha", "short")
	s.Write("test", "beta", "a longer piece of content")

	entries, err := s.List("test")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// Ordered by key
	if entries[0].Key != "alpha" || entries[1].Key != "beta" {
		t.Errorf("keys = %q, %q", entries[0].Key, entries[1].Key)
	}
	if entries[0].SizeBytes != len("short") {
		t.Errorf("alpha size = %d, want %d", entries[0].SizeBytes, len("short"))
	}
	if entries[1].SizeBytes != len("a longer piece of content") {
		t.Errorf("beta size = %d, want %d", entries[1].SizeBytes, len("a longer piece of content"))
	}
	if entries[0].Updated.IsZero() {
		t.Error("alpha Updated should not be zero")
	}
}

func TestScratchpadListEmpty(t *testing.T) {
	s := testScratchpad(t)

	entries, err := s.List("test")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func openTestDB(path string) (*sql.DB, error) {
	return sql.Open("sqlite", path)
}
