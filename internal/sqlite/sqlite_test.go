package sqlite

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpen verifies that Open sets WAL mode and busy_timeout on the database.
func TestOpen(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	var timeout int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", timeout)
	}
}

// TestOpenInit verifies that OpenInit creates tables and closes on DDL failure.
func TestOpenInit(t *testing.T) {
	dir := t.TempDir()

	// Successful init with a CREATE TABLE.
	db, err := OpenInit(filepath.Join(dir, "ok.db"),
		`CREATE TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY, val TEXT)`,
	)
	if err != nil {
		t.Fatalf("OpenInit: %v", err)
	}
	// Verify the table exists by inserting a row.
	if _, err := db.Exec("INSERT INTO t (val) VALUES ('hello')"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	db.Close()

	// Failed init — bad SQL should return an error and close the db.
	_, err = OpenInit(filepath.Join(dir, "bad.db"), "NOT VALID SQL AT ALL")
	if err == nil {
		t.Fatal("expected error for invalid SQL")
	}
}

// Verifies Open returns an error when the path's parent directory doesn't exist
// (PRAGMA fails because sqlite can't create the file).
func TestOpenBadPath(t *testing.T) {
	_, err := Open("/nonexistent/dir/test.db")
	if err == nil {
		t.Fatal("expected error for path in nonexistent directory")
	}
}

// Verifies OpenInit forwards an Open error when the path is invalid.
func TestOpenInitBadPath(t *testing.T) {
	_, err := OpenInit("/nonexistent/dir/test.db", "CREATE TABLE t (id INTEGER)")
	if err == nil {
		t.Fatal("expected error for path in nonexistent directory")
	}
}

// TestMigrateFile verifies that MigrateFile moves files from old to new
// location, creates parent directories, handles sidecar files, and skips
// migration when appropriate.
func TestMigrateFile(t *testing.T) {
	t.Run("migrates file and creates parent dirs", func(t *testing.T) {
		// Verifies basic migration: file moves, parent dir is created.
		dir := t.TempDir()
		oldPath := filepath.Join(dir, "old", "test.db")
		newPath := filepath.Join(dir, "new", "nested", "test.db")

		os.MkdirAll(filepath.Dir(oldPath), 0755)
		os.WriteFile(oldPath, []byte("data"), 0644)

		migrated, err := MigrateFile(oldPath, newPath)
		if err != nil {
			t.Fatalf("MigrateFile: %v", err)
		}
		if !migrated {
			t.Fatal("expected migration to occur")
		}
		if _, err := os.Stat(newPath); err != nil {
			t.Fatalf("new file should exist: %v", err)
		}
		if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
			t.Fatal("old file should not exist after migration")
		}
	})

	t.Run("migrates WAL and SHM sidecars", func(t *testing.T) {
		// Verifies that -wal and -shm sidecar files are also moved.
		dir := t.TempDir()
		oldPath := filepath.Join(dir, "test.db")
		newPath := filepath.Join(dir, "dest", "test.db")

		os.WriteFile(oldPath, []byte("db"), 0644)
		os.WriteFile(oldPath+"-wal", []byte("wal"), 0644)
		os.WriteFile(oldPath+"-shm", []byte("shm"), 0644)

		migrated, err := MigrateFile(oldPath, newPath)
		if err != nil {
			t.Fatalf("MigrateFile: %v", err)
		}
		if !migrated {
			t.Fatal("expected migration")
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if _, err := os.Stat(newPath + suffix); err != nil {
				t.Errorf("expected %s to exist", newPath+suffix)
			}
		}
	})

	t.Run("skips when old does not exist", func(t *testing.T) {
		// Verifies no error and no migration when old path is missing.
		dir := t.TempDir()
		migrated, err := MigrateFile(filepath.Join(dir, "nope.db"), filepath.Join(dir, "dst.db"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if migrated {
			t.Fatal("should not migrate when old file doesn't exist")
		}
	})

	t.Run("skips when new already exists", func(t *testing.T) {
		// Verifies that existing destination prevents migration (no clobber).
		dir := t.TempDir()
		oldPath := filepath.Join(dir, "old.db")
		newPath := filepath.Join(dir, "new.db")

		os.WriteFile(oldPath, []byte("old"), 0644)
		os.WriteFile(newPath, []byte("new"), 0644)

		migrated, err := MigrateFile(oldPath, newPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if migrated {
			t.Fatal("should not migrate when new file already exists")
		}
		// Old file should still be there
		if _, err := os.Stat(oldPath); err != nil {
			t.Fatal("old file should still exist")
		}
	})
}

// TestAgentPath verifies the per-agent path construction.
func TestAgentPath(t *testing.T) {
	tests := []struct {
		base    string
		agentID string
		want    string
	}{
		{"/data/todo.db", "clutch", "/data/todo-clutch.db"},
		{"/data/conversation.db", "otto", "/data/conversation-otto.db"},
		{"memory.db", "agent1", "memory-agent1.db"},
	}
	for _, tt := range tests {
		got := AgentPath(tt.base, tt.agentID)
		if got != tt.want {
			t.Errorf("AgentPath(%q, %q) = %q, want %q", tt.base, tt.agentID, got, tt.want)
		}
	}
}
