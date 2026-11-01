package agentrename

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite" // the driver every foci database uses
)

// lockedDB is one SQLite file opened for the rename and held under an
// EXCLUSIVE lock for as long as the command runs. Taking that lock is also
// the "is anything else using this file?" probe: every foci database runs in
// WAL mode, and an open WAL connection — the gateway's, or any other
// process's, even idle — holds a lock that refuses ours. Holding it keeps any
// writer out until the command closes the file.
type lockedDB struct {
	path string
	db   *sql.DB
	conn *sql.Conn
}

// errInUse marks a database another process has open.
var errInUse = errors.New("in use by another process")

// lockDB opens path (which must exist; nothing is created) on one connection
// with locking_mode=EXCLUSIVE and takes the lock at once. No journal-mode
// pragma is set, so a database is never converted (a plan-only run must not
// change a byte). busy_timeout(0): a held lock is reported now, not waited on.
func lockDB(ctx context.Context, path string) (*lockedDB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	esc := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	db, err := sql.Open("sqlite", "file:"+esc+"?mode=rw&_pragma=busy_timeout(0)&_pragma=locking_mode(EXCLUSIVE)")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		_ = conn.Close()
		_ = db.Close()
		if strings.Contains(err.Error(), "locked") || strings.Contains(err.Error(), "busy") {
			return nil, fmt.Errorf("%s: %w (%v)", path, errInUse, err)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		_ = conn.Close()
		_ = db.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return &lockedDB{path: path, db: db, conn: conn}, nil
}

// checkpoint folds the WAL into the main file, so a byte copy of the main
// file is the whole database.
func (l *lockedDB) checkpoint(ctx context.Context) error {
	var busy, logFrames, done int
	if err := l.conn.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &done); err != nil {
		return fmt.Errorf("checkpoint %s: %w", l.path, err)
	}
	if busy != 0 {
		return fmt.Errorf("checkpoint %s: database busy", l.path)
	}
	return nil
}

func (l *lockedDB) close() {
	if l == nil {
		return
	}
	_ = l.conn.Close()
	_ = l.db.Close()
}

// hasColumn reports whether table.col exists (older files may predate a column).
func (l *lockedDB) hasColumn(ctx context.Context, table, col string) (bool, error) {
	var n int
	err := l.conn.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, col).Scan(&n)
	return n > 0, err
}

// queryer is what a rule reads and writes through: the locked connection for
// counting, the step's transaction for applying.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ruleKind is how a rule decides a cell and rewrites it.
type ruleKind int

const (
	// kindEq: the cell equals from; it becomes to (agent-id columns).
	kindEq ruleKind = iota
	// kindPrefix: the cell starts with from; that prefix becomes to (session
	// keys, turn ids, call keys — from = "old/" — and paths).
	kindPrefix
	// kindValue: a cell holding session keys as a plain value or inside JSON
	// (ids.rewriteValue), in rows selected by where.
	kindValue
	// kindMetaKey: an agent_metadata key "<kind>:<session key>", in rows
	// selected by where.
	kindMetaKey
)

// rule rewrites one column of one table.
type rule struct {
	table, col string
	kind       ruleKind
	from, to   string // kindEq, kindPrefix
	// where restricts the rows a kindValue/kindMetaKey rule reads (SQL, with
	// whereArgs). Empty means every row.
	where     string
	whereArgs []any
	// idRule marks a rule whose from is the agent id or its key prefix: the
	// NEW-id collision check runs these with from = the new id.
	idRule bool
	r      ids
}

func (u rule) label() string { return u.table + "." + u.col }

// present reports whether the rule's table and column exist in the file.
func (u rule) present(ctx context.Context, l *lockedDB) (bool, error) {
	return l.hasColumn(ctx, u.table, u.col)
}

// count returns how many rows the rule would change.
func (u rule) count(ctx context.Context, q queryer) (int, error) {
	switch u.kind {
	case kindEq:
		var n int
		err := q.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM %q WHERE %q = ?`, u.table, u.col), u.from).Scan(&n)
		return n, err
	case kindPrefix:
		var n int
		err := q.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM %q WHERE substr(%q, 1, length(?1)) = ?1`, u.table, u.col), u.from).Scan(&n)
		return n, err
	default:
		ch, err := u.valueChanges(ctx, q)
		return len(ch), err
	}
}

// apply rewrites the rule's rows and returns how many changed.
func (u rule) apply(ctx context.Context, q queryer) (int, error) {
	var res sql.Result
	var err error
	switch u.kind {
	case kindEq:
		res, err = q.ExecContext(ctx, fmt.Sprintf(`UPDATE %q SET %q = ?2 WHERE %q = ?1`, u.table, u.col, u.col), u.from, u.to)
	case kindPrefix:
		res, err = q.ExecContext(ctx, fmt.Sprintf(
			`UPDATE %q SET %q = ?2 || substr(%q, length(?1) + 1) WHERE substr(%q, 1, length(?1)) = ?1`,
			u.table, u.col, u.col, u.col), u.from, u.to)
	default:
		ch, cerr := u.valueChanges(ctx, q)
		if cerr != nil {
			return 0, cerr
		}
		for _, c := range ch {
			if _, err := q.ExecContext(ctx, fmt.Sprintf(`UPDATE %q SET %q = ? WHERE rowid = ?`, u.table, u.col), c.value, c.rowid); err != nil {
				return 0, fmt.Errorf("%s: %w", u.label(), err)
			}
		}
		return len(ch), nil
	}
	if err != nil {
		return 0, fmt.Errorf("%s: %w", u.label(), err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

type cellChange struct {
	rowid int64
	value string
}

// valueChanges computes the new value of every row a kindValue/kindMetaKey
// rule changes.
func (u rule) valueChanges(ctx context.Context, q queryer) ([]cellChange, error) {
	query := fmt.Sprintf(`SELECT rowid, %q FROM %q WHERE %q IS NOT NULL`, u.col, u.table, u.col)
	if u.where != "" {
		query += " AND (" + u.where + ")"
	}
	rows, err := q.QueryContext(ctx, query, u.whereArgs...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", u.label(), err)
	}
	defer func() { _ = rows.Close() }()
	var out []cellChange
	for rows.Next() {
		var id int64
		var v string
		if err := rows.Scan(&id, &v); err != nil {
			return nil, fmt.Errorf("%s: %w", u.label(), err)
		}
		var nv string
		var ok bool
		if u.kind == kindMetaKey {
			nv, ok = u.r.rewriteMetaKey(v)
		} else {
			nv, ok = u.r.rewriteValue(v)
		}
		if ok {
			out = append(out, cellChange{id, nv})
		}
	}
	return out, rows.Err()
}
