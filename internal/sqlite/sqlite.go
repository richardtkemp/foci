package sqlite

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Open opens a SQLite database with standard settings (WAL mode, 5s busy timeout).
// On error the database is closed before returning.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", filepath.Base(path), err)
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite %q on %s: %w", pragma, filepath.Base(path), err)
		}
	}
	return db, nil
}

// OpenInit opens a database and executes DDL statements (e.g. CREATE TABLE).
// If any statement fails, the database is closed and the error returned.
func OpenInit(path string, stmts ...string) (*sql.DB, error) {
	db, err := Open(path)
	if err != nil {
		return nil, err
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("init %s: %w", filepath.Base(path), err)
		}
	}
	return db, nil
}

// AgentPath returns a per-agent database path by inserting the agent ID
// before the extension: dir/name.db → dir/name-agentID.db.
func AgentPath(basePath, agentID string) string {
	ext := filepath.Ext(basePath)
	return strings.TrimSuffix(basePath, ext) + "-" + agentID + ext
}

// MigrateFile moves a file from oldPath to newPath if oldPath exists and
// newPath does not. Creates the parent directory of newPath if needed.
// For SQLite databases, also migrates WAL and SHM sidecar files.
// Returns true if migration occurred, false if skipped (already at new
// location or old doesn't exist).
func MigrateFile(oldPath, newPath string) (bool, error) {
	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		return false, nil
	}
	if _, err := os.Stat(newPath); err == nil {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(newPath), 0755); err != nil {
		return false, fmt.Errorf("create directory for %s: %w", newPath, err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return false, fmt.Errorf("migrate %s → %s: %w", oldPath, newPath, err)
	}
	// Migrate SQLite sidecar files (WAL, SHM) if they exist
	for _, suffix := range []string{"-wal", "-shm"} {
		old := oldPath + suffix
		if _, err := os.Stat(old); err == nil {
			_ = os.Rename(old, newPath+suffix)
		}
	}
	return true, nil
}
