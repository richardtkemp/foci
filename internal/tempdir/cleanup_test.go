package tempdir

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCleanOldFiles verifies that files older than maxAge are removed,
// recent files are kept, directories are skipped, and non-matching
// files are unaffected.
func TestCleanOldFiles(t *testing.T) {
	dir := t.TempDir()

	// Create an old file (backdate modification time).
	oldFile := filepath.Join(dir, "spawn-result-grep-001.txt")
	if err := os.WriteFile(oldFile, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * 24 * time.Hour) // 10 days ago
	if err := os.Chtimes(oldFile, old, old); err != nil {
		t.Fatal(err)
	}

	// Create a recent file.
	recentFile := filepath.Join(dir, "spawn-result-grep-002.txt")
	if err := os.WriteFile(recentFile, []byte("recent"), 0600); err != nil {
		t.Fatal(err)
	}

	// Create a non-matching file that should not be touched.
	otherFile := filepath.Join(dir, "other.txt")
	if err := os.WriteFile(otherFile, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(otherFile, old, old); err != nil {
		t.Fatal(err)
	}

	// Create a directory matching the glob — should be skipped.
	dirMatch := filepath.Join(dir, "spawn-result-dir-003.txt")
	if err := os.Mkdir(dirMatch, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dirMatch, old, old); err != nil {
		t.Fatal(err)
	}

	n, err := CleanOldFiles(dir, "spawn-result-*.txt", 7*24*time.Hour)
	if err != nil {
		t.Fatalf("CleanOldFiles: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 removed, got %d", n)
	}

	// Old file should be gone.
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Error("old file should have been removed")
	}
	// Recent file should still exist.
	if _, err := os.Stat(recentFile); err != nil {
		t.Errorf("recent file should still exist: %v", err)
	}
	// Non-matching file should still exist.
	if _, err := os.Stat(otherFile); err != nil {
		t.Errorf("non-matching file should still exist: %v", err)
	}
	// Directory should still exist.
	if _, err := os.Stat(dirMatch); err != nil {
		t.Errorf("directory should still exist: %v", err)
	}
}

// TestCleanOldFilesEmptyDir confirms that running against an empty
// directory succeeds with zero removals.
func TestCleanOldFilesEmptyDir(t *testing.T) {
	dir := t.TempDir()
	n, err := CleanOldFiles(dir, "spawn-result-*.txt", time.Hour)
	if err != nil {
		t.Fatalf("CleanOldFiles: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 removed, got %d", n)
	}
}
