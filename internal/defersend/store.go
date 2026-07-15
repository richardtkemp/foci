// Package defersend persists "wait-until" sends: a foci send whose activity
// gate (--wait-cold/-warm/-user-active/-user-inactive) is not yet satisfied is
// enqueued here and delivered later by a background sweep once the condition
// holds (or the deadline expires). Persisting to SQLite means a pending send
// survives a gateway restart/deploy — the property that distinguishes this from
// a purely in-memory blocking wait.
package defersend

import (
	"database/sql"
	"sync"
	"time"

	"foci/internal/sqlite"
	"foci/internal/timeutil"
)

// Record is one pending deferred send. The Wait* fields carry the same duration
// strings as the wire gate keys; an empty field means that condition is not
// requested. The session key is resolved at enqueue time and stored verbatim,
// so a later default-session change does not redirect an already-queued send.
type Record struct {
	ID               int64
	AgentID          string
	SessionKey       string
	Text             string
	Policy           string
	Model            string
	WaitWarm         string
	WaitCold         string
	WaitUserActive   string
	WaitUserInactive string
	CreatedAt        time.Time
	DeadlineAt       time.Time
}

// Store is the SQLite-backed queue of pending deferred sends.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// NewStore opens (or creates) the deferred-send database.
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
			deadline_at        TEXT NOT NULL
		)`,
	)
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Enqueue persists a pending send and returns its assigned id.
func (s *Store) Enqueue(r Record) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		`INSERT INTO deferred_sends
		   (agent_id, session_key, text, policy, model,
		    wait_warm, wait_cold, wait_user_active, wait_user_inactive, created_at, deadline_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.AgentID, r.SessionKey, r.Text, r.Policy, r.Model,
		r.WaitWarm, r.WaitCold, r.WaitUserActive, r.WaitUserInactive,
		timeutil.Format(r.CreatedAt), timeutil.Format(r.DeadlineAt),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// All returns every pending send, oldest first.
func (s *Store) All() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT id, agent_id, session_key, text, policy, model,
		        wait_warm, wait_cold, wait_user_active, wait_user_inactive, created_at, deadline_at
		   FROM deferred_sends ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []Record
	for rows.Next() {
		var r Record
		var created, deadline string
		if err := rows.Scan(&r.ID, &r.AgentID, &r.SessionKey, &r.Text, &r.Policy, &r.Model,
			&r.WaitWarm, &r.WaitCold, &r.WaitUserActive, &r.WaitUserInactive, &created, &deadline); err != nil {
			return nil, err
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339, created)
		r.DeadlineAt, _ = time.Parse(time.RFC3339, deadline)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Delete removes a pending send by id (idempotent).
func (s *Store) Delete(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM deferred_sends WHERE id = ?`, id)
	return err
}
