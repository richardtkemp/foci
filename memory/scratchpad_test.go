package memory

import (
	"path/filepath"
	"testing"
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

	if err := s.Write("investigation", "checking FTS5 phrase boosting"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	content, err := s.Read("investigation")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if content != "checking FTS5 phrase boosting" {
		t.Errorf("content = %q", content)
	}
}

func TestScratchpadReadMissing(t *testing.T) {
	s := testScratchpad(t)

	content, err := s.Read("nonexistent")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if content != "" {
		t.Errorf("expected empty for missing key, got %q", content)
	}
}

func TestScratchpadOverwrite(t *testing.T) {
	s := testScratchpad(t)

	s.Write("notes", "first version")
	s.Write("notes", "updated version")

	content, _ := s.Read("notes")
	if content != "updated version" {
		t.Errorf("content = %q, want updated version", content)
	}
}

func TestScratchpadClear(t *testing.T) {
	s := testScratchpad(t)

	s.Write("temp", "temporary data")
	if err := s.Clear("temp"); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	content, _ := s.Read("temp")
	if content != "" {
		t.Errorf("expected empty after clear, got %q", content)
	}
}

func TestScratchpadClearNonexistent(t *testing.T) {
	s := testScratchpad(t)

	// Should not error
	if err := s.Clear("nonexistent"); err != nil {
		t.Fatalf("Clear nonexistent: %v", err)
	}
}

func TestScratchpadAll(t *testing.T) {
	s := testScratchpad(t)

	s.Write("alpha", "first")
	s.Write("beta", "second")
	s.Write("gamma", "third")

	entries, err := s.All()
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

	entries, err := s.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestScratchpadMultipleKeys(t *testing.T) {
	s := testScratchpad(t)

	s.Write("task1", "working on auth")
	s.Write("task2", "investigating cache bug")

	c1, _ := s.Read("task1")
	c2, _ := s.Read("task2")

	if c1 != "working on auth" {
		t.Errorf("task1 = %q", c1)
	}
	if c2 != "investigating cache bug" {
		t.Errorf("task2 = %q", c2)
	}

	// Clear one, other remains
	s.Clear("task1")
	c1, _ = s.Read("task1")
	c2, _ = s.Read("task2")
	if c1 != "" {
		t.Errorf("task1 should be cleared, got %q", c1)
	}
	if c2 != "investigating cache bug" {
		t.Errorf("task2 should remain, got %q", c2)
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
