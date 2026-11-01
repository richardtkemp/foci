// Package defersend persists "wait-until" requests: a foci send, branch or
// command whose activity gate (--wait-cold/-warm/-user-active/-user-inactive)
// is not yet satisfied is enqueued here and delivered later by a background
// sweep once the condition holds (or, for a send or branch, once the deadline
// expires; a command is dropped at its deadline instead). Persisting to
// SQLite means a pending request survives a gateway restart/deploy — the
// property that distinguishes this from a purely in-memory blocking wait.
package defersend

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"foci/internal/sqlite"
	"foci/internal/timeutil"
)

// Kind identifies what a deferred record is and therefore how the sweep
// delivers it. The empty Kind is the legacy value from before the column
// existed and means a send — normalise through Record.EffectiveKind.
type Kind string

const (
	KindSend    Kind = "send"    // deliver as a user turn on SessionKey
	KindBranch  Kind = "branch"  // create a branch of SessionKey and run the turn on it
	KindCommand Kind = "command" // dispatch Text as a slash command on SessionKey
)

// Record is one pending deferred request. The Wait* fields carry the same
// duration strings as the wire gate keys; an empty field means that condition
// is not requested. The session key is resolved at enqueue time and stored
// verbatim, so a later default-session change does not redirect an
// already-queued request. For a send, Text is the message; for a command it
// is the command string; for a branch it is the branch text and SessionKey is
// the PARENT session (the fork happens at delivery time, so the branch
// inherits the parent's state then, not at enqueue time). Model carries the
// model override (sends and branches; commands are not turns and ignore it).
// Steer carries the per-message delivery choice ("steer"/"queue"/"" — the
// wire value, mapped at delivery time) for a send (#1318), so the choice
// survives both the wait and a restart.
type Record struct {
	ID               int64
	Kind             Kind
	AgentID          string
	SessionKey       string
	Text             string
	Policy           string
	Model            string
	Steer            string
	WaitWarm         string
	WaitCold         string
	WaitUserActive   string
	WaitUserInactive string
	NoCompact        bool
	NoResetHook      bool
	Silent           bool
	CreatedAt        time.Time
	DeadlineAt       time.Time
	// Attempts counts the delivery attempts that failed TRANSIENTLY (a full
	// inbox) — the sweep retries such a record on a later tick and drops it
	// after a bounded number (#2220). Persisted, so a gateway restart does
	// not reset the count. A row written before the column existed reads
	// back as 0.
	Attempts int
}

// EffectiveKind normalises the stored kind: a row written before the kind
// column existed reads back as "" and means a plain send. This is the single
// "" → KindSend mapping — consumers must not re-derive it.
func (r Record) EffectiveKind() Kind {
	if r.Kind == "" {
		return KindSend
	}
	return r.Kind
}

// Store is the SQLite-backed queue of pending deferred requests.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// NewStore opens (or creates) the deferred-request database. A database from
// before the kind/branch-option columns is migrated in place: the columns are
// added idempotently with send defaults, so its rows survive and read back as
// sends. Opening an already-current file is a no-op.
func NewStore(path string) (*Store, error) {
	db, err := sqlite.OpenInit(path,
		`CREATE TABLE IF NOT EXISTS deferred_sends (
			id                 INTEGER PRIMARY KEY AUTOINCREMENT,
			agent_id           TEXT NOT NULL,
			session_key        TEXT NOT NULL,
			text               TEXT NOT NULL,
			policy             TEXT NOT NULL DEFAULT '',
			model              TEXT NOT NULL DEFAULT '',
			steer              TEXT NOT NULL DEFAULT '',
			wait_warm          TEXT NOT NULL DEFAULT '',
			wait_cold          TEXT NOT NULL DEFAULT '',
			wait_user_active   TEXT NOT NULL DEFAULT '',
			wait_user_inactive TEXT NOT NULL DEFAULT '',
			created_at         TEXT NOT NULL,
			deadline_at        TEXT NOT NULL,
			kind               TEXT NOT NULL DEFAULT '',
			no_compact         INTEGER NOT NULL DEFAULT 0,
			no_reset_hook      INTEGER NOT NULL DEFAULT 0,
			silent             INTEGER NOT NULL DEFAULT 0,
			attempts           INTEGER NOT NULL DEFAULT 0
		)`,
	)
	if err != nil {
		return nil, err
	}
	// Pre-kind databases predate the kind/branch-option columns; add them so
	// the new queries work and the old rows read back as sends. (Fresh
	// databases get them from CREATE TABLE.) Only the expected "duplicate
	// column" (already migrated) is ignored — any other failure (locked file,
	// I/O error) must surface HERE, not later as a "no such column" on the
	// first Enqueue/All.
	for _, col := range []struct{ name, ddl string }{
		{"kind", `ALTER TABLE deferred_sends ADD COLUMN kind TEXT NOT NULL DEFAULT ''`},
		{"no_compact", `ALTER TABLE deferred_sends ADD COLUMN no_compact INTEGER NOT NULL DEFAULT 0`},
		{"no_reset_hook", `ALTER TABLE deferred_sends ADD COLUMN no_reset_hook INTEGER NOT NULL DEFAULT 0`},
		{"silent", `ALTER TABLE deferred_sends ADD COLUMN silent INTEGER NOT NULL DEFAULT 0`},
		{"steer", `ALTER TABLE deferred_sends ADD COLUMN steer TEXT NOT NULL DEFAULT ''`},
		{"attempts", `ALTER TABLE deferred_sends ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0`},
	} {
		if _, err := db.Exec(col.ddl); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			_ = db.Close()
			return nil, fmt.Errorf("add %s column: %w", col.name, err)
		}
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// insert writes r as a new row inside tx and returns its id.
func insert(tx *sql.Tx, r Record) (int64, error) {
	res, err := tx.Exec(
		`INSERT INTO deferred_sends
		   (agent_id, session_key, text, policy, model, steer,
		    wait_warm, wait_cold, wait_user_active, wait_user_inactive,
		    kind, no_compact, no_reset_hook, silent, created_at, deadline_at, attempts)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.AgentID, r.SessionKey, r.Text, r.Policy, r.Model, r.Steer,
		r.WaitWarm, r.WaitCold, r.WaitUserActive, r.WaitUserInactive,
		string(r.Kind), r.NoCompact, r.NoResetHook, r.Silent,
		timeutil.Format(r.CreatedAt), timeutil.Format(r.DeadlineAt), r.Attempts,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Enqueue persists r and, in the same transaction, removes every
// pending record IDENTICAL to it (#2220 dedupe): a repeated request — a cron
// re-issuing the same deferred `/reset` every 15 minutes — replaces the
// queued one instead of piling up beside it. Identical means the same
// effective kind (a legacy "" row is a send), agent, session, text, model,
// policy, all four wait conditions, no_compact, no_reset_hook and silent;
// any other field (steer, created/deadline times, attempts) is not part of
// the identity, and the new record carries its own. Returns the new id and
// the ids it replaced (oldest first).
func (s *Store) Enqueue(r Record) (int64, []int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	rows, err := tx.Query(
		`SELECT id FROM deferred_sends
		  WHERE (CASE kind WHEN '' THEN ? ELSE kind END) = ?
		    AND agent_id = ? AND session_key = ? AND text = ? AND model = ? AND policy = ?
		    AND wait_warm = ? AND wait_cold = ? AND wait_user_active = ? AND wait_user_inactive = ?
		    AND no_compact = ? AND no_reset_hook = ? AND silent = ?
		  ORDER BY id`,
		string(KindSend), string(r.EffectiveKind()),
		r.AgentID, r.SessionKey, r.Text, r.Model, r.Policy,
		r.WaitWarm, r.WaitCold, r.WaitUserActive, r.WaitUserInactive,
		r.NoCompact, r.NoResetHook, r.Silent)
	if err != nil {
		return 0, nil, err
	}
	var replaced []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, nil, err
		}
		replaced = append(replaced, id)
	}
	if err := rows.Close(); err != nil {
		return 0, nil, err
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	for _, id := range replaced {
		if _, err := tx.Exec(`DELETE FROM deferred_sends WHERE id = ?`, id); err != nil {
			return 0, nil, err
		}
	}
	id, err := insert(tx, r)
	if err != nil {
		return 0, nil, err
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, err
	}
	return id, replaced, nil
}

// RecordFailedAttempt increments a pending record's transient-failure count
// and returns the new count. A record that no longer exists returns
// (0, sql.ErrNoRows).
func (s *Store) RecordFailedAttempt(id int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`UPDATE deferred_sends SET attempts = attempts + 1 WHERE id = ?`, id); err != nil {
		return 0, err
	}
	var n int
	if err := s.db.QueryRow(`SELECT attempts FROM deferred_sends WHERE id = ?`, id).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// All returns every pending request, oldest first.
func (s *Store) All() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT id, agent_id, session_key, text, policy, model, steer,
		        wait_warm, wait_cold, wait_user_active, wait_user_inactive,
		        kind, no_compact, no_reset_hook, silent, created_at, deadline_at, attempts
		   FROM deferred_sends ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []Record
	for rows.Next() {
		var r Record
		var kind string
		var created, deadline string
		if err := rows.Scan(&r.ID, &r.AgentID, &r.SessionKey, &r.Text, &r.Policy, &r.Model, &r.Steer,
			&r.WaitWarm, &r.WaitCold, &r.WaitUserActive, &r.WaitUserInactive,
			&kind, &r.NoCompact, &r.NoResetHook, &r.Silent, &created, &deadline, &r.Attempts); err != nil {
			return nil, err
		}
		r.Kind = Kind(kind) // read verbatim; normalise via EffectiveKind
		r.CreatedAt, _ = time.Parse(time.RFC3339, created)
		r.DeadlineAt, _ = time.Parse(time.RFC3339, deadline)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Delete removes a pending request by id (idempotent).
func (s *Store) Delete(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM deferred_sends WHERE id = ?`, id)
	return err
}
