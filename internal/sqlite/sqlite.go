package sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// openDSN opens a SQLite DB applying pragmas via the DSN. Pragmas MUST go on the
// DSN, not a post-open db.Exec: database/sql pools connections, and a PRAGMA run
// through db.Exec only sticks to whichever pooled connection executed it — other
// connections stay at the sqlite defaults (busy_timeout=0 → immediate
// SQLITE_BUSY under write contention). The DSN applies them to every connection
// the pool opens.
func openDSN(path string, extraPragmas ...string) (*sql.DB, error) {
	pragmas := append([]string{"busy_timeout(5000)", "journal_mode(WAL)"}, extraPragmas...)
	dsn := "file:" + path + "?_pragma=" + strings.Join(pragmas, "&_pragma=")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", filepath.Base(path), err)
	}
	// sql.Open is lazy — force a connection so a bad path/permission fails here,
	// not on the first query (the pre-DSN code failed fast via its PRAGMA Exec).
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite %s: %w", filepath.Base(path), err)
	}
	return db, nil
}

// Open opens a SQLite database with standard settings (WAL mode, 5s busy timeout).
func Open(path string) (*sql.DB, error) {
	return openDSN(path)
}

// OpenReadOnly opens a SQLite database in read-only mode (query_only pragma).
// WAL mode and busy timeout are still set for read concurrency with a writer.
func OpenReadOnly(path string) (*sql.DB, error) {
	return openDSN(path, "query_only(true)")
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
