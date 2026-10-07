// Package defersend persists "wait-until" requests: a foci send, branch or
// command whose activity gate (--wait-cold/-warm/-user-active/-user-inactive)
// is not yet satisfied is enqueued here and delivered later by a background
// sweep once the condition holds (or the deadline expires). Persisting to
// SQLite means a pending request survives a gateway restart/deploy — the
// property that distinguishes this from a purely in-memory blocking wait.
package defersend

import (
	"database/sql"
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
type Record struct {
	ID               int64
	Kind             Kind
	AgentID          string
	SessionKey       string
	Text             string
	Policy           string
	Model            string
	WaitWarm         string
	WaitCold         string
	WaitUserActive   string
	WaitUserInactive string
	NoCompact        bool
	NoResetHook      bool
	Silent           bool
	CreatedAt        time.Time
	DeadlineAt       time.Time
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
			wait_warm          TEXT NOT NULL DEFAULT '',
			wait_cold          TEXT NOT NULL DEFAULT '',
			wait_user_active   TEXT NOT NULL DEFAULT '',
			wait_user_inactive TEXT NOT NULL DEFAULT '',
			created_at         TEXT NOT NULL,
			deadline_at        TEXT NOT NULL,
			kind               TEXT NOT NULL DEFAULT '',
			no_compact         INTEGER NOT NULL DEFAULT 0,
			no_reset_hook      INTEGER NOT NULL DEFAULT 0,
			silent             INTEGER NOT NULL DEFAULT 0
		)`,
	)
	if err != nil {
		return nil, err
	}
	// Pre-kind databases predate the kind/branch-option columns; add them so
	// the new queries work and the old rows read back as sends. (Fresh
	// databases get them from CREATE TABLE.)
	for _, ddl := range []string{
		`ALTER TABLE deferred_sends ADD COLUMN kind TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE deferred_sends ADD COLUMN no_compact INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE deferred_sends ADD COLUMN no_reset_hook INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE deferred_sends ADD COLUMN silent INTEGER NOT NULL DEFAULT 0`,
	} {
		_, _ = db.Exec(ddl) // "duplicate column" on current-schema DBs — ignored
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Enqueue persists a pending request and returns its assigned id.
func (s *Store) Enqueue(r Record) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		`INSERT INTO deferred_sends
		   (agent_id, session_key, text, policy, model,
		    wait_warm, wait_cold, wait_user_active, wait_user_inactive,
		    kind, no_compact, no_reset_hook, silent, created_at, deadline_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.AgentID, r.SessionKey, r.Text, r.Policy, r.Model,
		r.WaitWarm, r.WaitCold, r.WaitUserActive, r.WaitUserInactive,
		string(r.Kind), r.NoCompact, r.NoResetHook, r.Silent,
		timeutil.Format(r.CreatedAt), timeutil.Format(r.DeadlineAt),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// All returns every pending request, oldest first.
func (s *Store) All() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT id, agent_id, session_key, text, policy, model,
		        wait_warm, wait_cold, wait_user_active, wait_user_inactive,
		        kind, no_compact, no_reset_hook, silent, created_at, deadline_at
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
		if err := rows.Scan(&r.ID, &r.AgentID, &r.SessionKey, &r.Text, &r.Policy, &r.Model,
			&r.WaitWarm, &r.WaitCold, &r.WaitUserActive, &r.WaitUserInactive,
			&kind, &r.NoCompact, &r.NoResetHook, &r.Silent, &created, &deadline); err != nil {
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
