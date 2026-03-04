package session

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"foci/log"

	_ "modernc.org/sqlite"
)

// SessionType classifies the purpose of a session.
type SessionType string

const (
	SessionTypeChat       SessionType = "chat"
	SessionTypeMultiball  SessionType = "multiball"
	SessionTypeSpawn      SessionType = "spawn"
	SessionTypeCron       SessionType = "cron"
	SessionTypeBranch     SessionType = "branch"
	SessionTypeUnknown    SessionType = "unknown"
)

// SessionStatus tracks the lifecycle state of a session.
type SessionStatus string

const (
	SessionStatusActive    SessionStatus = "active"
	SessionStatusCompacted SessionStatus = "compacted"
	SessionStatusCleared   SessionStatus = "cleared"
	SessionStatusArchived  SessionStatus = "archived"
)

// SessionIndexEntry represents a row in the session_index table.
type SessionIndexEntry struct {
	SessionKey       string
	FilePath         string
	CreatedAt        time.Time
	LastActivityAt   time.Time // updated on every session append; zero if never set
	ParentSessionKey string    // empty if root session
	SessionType      SessionType
	Status           SessionStatus
}

// SessionIndex maintains a SQLite index of all session files.
type SessionIndex struct {
	db *sql.DB
	mu sync.Mutex
}

// NewSessionIndex opens (or creates) the SQLite database for the session index.
func NewSessionIndex(dbPath string) (*SessionIndex, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open session index db: %w", err)
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS session_index (
		session_key        TEXT PRIMARY KEY,
		file_path          TEXT NOT NULL,
		created_at         TEXT NOT NULL,
		parent_session_key TEXT,
		session_type       TEXT NOT NULL,
		status             TEXT NOT NULL DEFAULT 'active'
	)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create session_index table: %w", err)
	}

	// Migration: add last_activity_at column if missing.
	_, _ = db.Exec(`ALTER TABLE session_index ADD COLUMN last_activity_at TEXT`)

	return &SessionIndex{db: db}, nil
}

// Close closes the underlying database.
func (idx *SessionIndex) Close() error {
	return idx.db.Close()
}

// Upsert inserts or updates a session index entry.
func (idx *SessionIndex) Upsert(e SessionIndexEntry) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	activityAt := e.LastActivityAt
	if activityAt.IsZero() {
		activityAt = e.CreatedAt
	}
	activityStr := activityAt.UTC().Format(time.RFC3339)
	createdStr := e.CreatedAt.UTC().Format(time.RFC3339)
	_, err := idx.db.Exec(
		`INSERT INTO session_index (session_key, file_path, created_at, last_activity_at, parent_session_key, session_type, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(session_key) DO UPDATE SET
		   file_path = excluded.file_path,
		   created_at = excluded.created_at,
		   last_activity_at = CASE
		     WHEN excluded.last_activity_at = excluded.created_at
		     THEN COALESCE(session_index.last_activity_at, excluded.last_activity_at)
		     ELSE excluded.last_activity_at
		   END,
		   parent_session_key = excluded.parent_session_key,
		   session_type = excluded.session_type,
		   status = excluded.status`,
		e.SessionKey, e.FilePath, createdStr,
		activityStr,
		nullableString(e.ParentSessionKey), string(e.SessionType), string(e.Status))
	if err != nil {
		log.Warnf("session_index", "upsert %s: %v", e.SessionKey, err)
	}
}

// SetStatus updates the status of an existing session.
func (idx *SessionIndex) SetStatus(sessionKey string, status SessionStatus) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	_, err := idx.db.Exec(`UPDATE session_index SET status = ? WHERE session_key = ?`,
		string(status), sessionKey)
	if err != nil {
		log.Warnf("session_index", "set status %s=%s: %v", sessionKey, status, err)
	}
}

// TouchActivity updates the last_activity_at timestamp for a session.
func (idx *SessionIndex) TouchActivity(sessionKey string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	_, err := idx.db.Exec(`UPDATE session_index SET last_activity_at = ? WHERE session_key = ?`,
		time.Now().UTC().Format(time.RFC3339), sessionKey)
	if err != nil {
		log.Warnf("session_index", "touch activity %s: %v", sessionKey, err)
	}
}

// Delete removes a session from the index.
func (idx *SessionIndex) Delete(sessionKey string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	_, err := idx.db.Exec(`DELETE FROM session_index WHERE session_key = ?`, sessionKey)
	if err != nil {
		log.Warnf("session_index", "delete %s: %v", sessionKey, err)
	}
}

// QueryOptions controls which sessions are returned by Query.
type QueryOptions struct {
	AgentID     string        // filter by agent ID (empty = all)
	SessionType SessionType   // filter by type (empty = all)
	Status      SessionStatus // filter by status (empty = all)
	MaxAge      time.Duration // only sessions with activity within this duration (0 = no limit)
	Limit       int           // max results (0 = unlimited)
}

// Query returns session index entries matching the given options, ordered by created_at desc.
func (idx *SessionIndex) Query(opts QueryOptions) ([]SessionIndexEntry, error) {
	query := `SELECT session_key, file_path, created_at, last_activity_at, parent_session_key, session_type, status
		FROM session_index WHERE 1=1`
	var args []interface{}

	if opts.AgentID != "" {
		query += ` AND session_key LIKE ?`
		args = append(args, opts.AgentID+"/%")
	}
	if opts.SessionType != "" {
		query += ` AND session_type = ?`
		args = append(args, string(opts.SessionType))
	}
	if opts.Status != "" {
		query += ` AND status = ?`
		args = append(args, string(opts.Status))
	}
	if opts.MaxAge > 0 {
		cutoff := time.Now().UTC().Add(-opts.MaxAge).Format(time.RFC3339)
		query += ` AND COALESCE(last_activity_at, created_at) >= ?`
		args = append(args, cutoff)
	}
	query += ` ORDER BY COALESCE(last_activity_at, created_at) DESC`
	if opts.Limit > 0 {
		query += fmt.Sprintf(` LIMIT %d`, opts.Limit)
	}

	rows, err := idx.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query session index: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []SessionIndexEntry
	for rows.Next() {
		var e SessionIndexEntry
		var createdStr string
		var activityStr sql.NullString
		var parentKey sql.NullString
		var stype, status string
		if err := rows.Scan(&e.SessionKey, &e.FilePath, &createdStr, &activityStr, &parentKey, &stype, &status); err != nil {
			log.Warnf("session_index", "scan row: %v", err)
			continue
		}
		e.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
		if activityStr.Valid {
			e.LastActivityAt, _ = time.Parse(time.RFC3339, activityStr.String)
		}
		if parentKey.Valid {
			e.ParentSessionKey = parentKey.String
		}
		e.SessionType = SessionType(stype)
		e.Status = SessionStatus(status)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// Count returns the total number of entries in the index.
func (idx *SessionIndex) Count() int {
	var n int
	if err := idx.db.QueryRow("SELECT COUNT(*) FROM session_index").Scan(&n); err != nil {
		log.Warnf("session_index", "count: %v", err)
	}
	return n
}

// Rebuild populates the index by scanning all session files on disk.
// Existing entries are preserved (upsert) so status is not lost for known sessions.
func (idx *SessionIndex) Rebuild(store *Store) (int, error) {
	entries, err := store.ScanAllSessions()
	if err != nil {
		return 0, fmt.Errorf("scan sessions: %w", err)
	}

	count := 0
	for _, e := range entries {
		idx.Upsert(e)
		count++
	}
	return count, nil
}

// nullableString returns nil for empty strings, the string otherwise.
func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
