package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
	return OpenInitPragmas(path, nil, stmts...)
}

// OpenInitPragmas is OpenInit with extra connection pragmas on the DSN. Every
// connection the pool opens runs them, so they must be cheap per-connection
// settings (foreign_keys, query_only). A pragma that only matters when the file
// is created (auto_vacuum) does NOT belong here: use OpenInitCreatePragmas.
func OpenInitPragmas(path string, pragmas []string, stmts ...string) (*sql.DB, error) {
	db, err := openDSN(path, pragmas...)
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

// OpenInitCreatePragmas is OpenInit for a database whose file-header pragmas
// (auto_vacuum(incremental) is the case this exists for) must be set when the
// file is created and never again.
//
// Such a pragma can only take effect while the file is empty, so it has to reach
// the database as a connection pragma, before the CREATEs (#1942). But it must
// not stay on the pool's DSN: on a non-empty file, auto_vacuum takes the WRITE
// lock, so every new pooled connection — a plain read included — queued behind
// any writer and failed with SQLITE_BUSY after busy_timeout (#2698). So when the
// file is absent or empty, one single-connection open applies createPragmas and
// runs stmts; the returned pool then opens with the standard pragmas only.
func OpenInitCreatePragmas(path string, createPragmas []string, stmts ...string) (*sql.DB, error) {
	fi, err := os.Stat(path)
	isNew := errors.Is(err, fs.ErrNotExist) || (err == nil && fi.Size() == 0)
	if isNew && len(createPragmas) > 0 {
		db, err := openDSN(path, createPragmas...)
		if err != nil {
			return nil, err
		}
		// One connection, so the CREATEs run on the connection that applied the
		// pragmas (openDSN's Ping left it idle in the pool).
		db.SetMaxOpenConns(1)
		for _, s := range stmts {
			if _, err := db.Exec(s); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("init %s: %w", filepath.Base(path), err)
			}
		}
		if err := db.Close(); err != nil {
			return nil, fmt.Errorf("init %s: %w", filepath.Base(path), err)
		}
	}
	return OpenInitPragmas(path, nil, stmts...)
}
