package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// Verifies OpenReadOnly opens an existing database with query_only set:
// reads succeed, writes are rejected, and WAL mode is preserved.
func TestOpenReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ro.db")

	// Create and populate the database with a normal writable handle.
	db, err := OpenInit(path,
		`CREATE TABLE t (id INTEGER PRIMARY KEY, val TEXT)`,
	)
	if err != nil {
		t.Fatalf("OpenInit: %v", err)
	}
	if _, err := db.Exec("INSERT INTO t (val) VALUES ('hello')"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	db.Close()

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()

	// Reads work.
	var val string
	if err := ro.QueryRow("SELECT val FROM t WHERE id = 1").Scan(&val); err != nil {
		t.Fatalf("select: %v", err)
	}
	if val != "hello" {
		t.Errorf("val = %q, want hello", val)
	}

	// Writes are rejected by query_only.
	if _, err := ro.Exec("INSERT INTO t (val) VALUES ('nope')"); err == nil {
		t.Error("expected insert on read-only db to fail")
	}

	// WAL mode is still set for read concurrency.
	var journalMode string
	if err := ro.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}
}

// Verifies OpenReadOnly forwards an Open error when the path is invalid.
func TestOpenReadOnlyBadPath(t *testing.T) {
	_, err := OpenReadOnly("/nonexistent/dir/test.db")
	if err == nil {
		t.Fatal("expected error for path in nonexistent directory")
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

// TestOpenInitCreatePragmas pins both halves of #2698 at the helper: the create
// pragma reaches a new (absent or zero-length) file, and the returned pool does
// not run it, so a fresh connection reads past a held write transaction instead
// of waiting busy_timeout for the write lock auto_vacuum needs on a non-empty file.
func TestOpenInitCreatePragmas(t *testing.T) {
	for _, tc := range []struct {
		name  string
		touch bool
	}{{"absent", false}, {"empty", true}} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "c.db")
			if tc.touch {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			db, err := OpenInitCreatePragmas(path, []string{"auto_vacuum(incremental)"},
				`CREATE TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY)`)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var av int
			if err := db.QueryRow(`PRAGMA auto_vacuum`).Scan(&av); err != nil {
				t.Fatal(err)
			}
			if av != 2 {
				t.Fatalf("auto_vacuum = %d, want 2", av)
			}
			if _, err := db.Exec(`INSERT INTO t (id) VALUES (1)`); err != nil {
				t.Fatal(err)
			}

			w, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			ctx := context.Background()
			wc, err := w.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer wc.Close()
			if _, err := wc.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = wc.ExecContext(ctx, `ROLLBACK`) }()
			if _, err := wc.ExecContext(ctx, `INSERT INTO t (id) VALUES (2)`); err != nil {
				t.Fatal(err)
			}

			db.SetMaxIdleConns(0) // the next query opens a new pooled connection
			start := time.Now()
			var n int
			if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
				t.Fatalf("read on a fresh connection behind a writer: %v (after %v)", err, time.Since(start))
			}
			if n != 1 {
				t.Errorf("count = %d, want 1 (the writer's row is uncommitted)", n)
			}
			if took := time.Since(start); took > time.Second {
				t.Errorf("read took %v behind a writer", took)
			}
		})
	}
}
