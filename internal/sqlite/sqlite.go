package sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// driverName is the SQL driver to use. Overridden in tests.
var driverName = "sqlite"

// Open opens a SQLite database with standard settings (WAL mode, 5s busy timeout).
// On error the database is closed before returning.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open(driverName, path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", filepath.Base(path), err)
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite pragma %s: %w", filepath.Base(path), err)
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
