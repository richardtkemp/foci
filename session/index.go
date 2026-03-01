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
)

// SessionIndexEntry represents a row in the session_index table.
type SessionIndexEntry struct {
	SessionKey       string
	FilePath         string
	CreatedAt        time.Time
	ParentSessionKey string // empty if root session
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
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		db.Close()
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
		db.Close()
		return nil, fmt.Errorf("create session_index table: %w", err)
	}

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

	_, err := idx.db.Exec(
		`INSERT OR REPLACE INTO session_index (session_key, file_path, created_at, parent_session_key, session_type, status)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		e.SessionKey, e.FilePath, e.CreatedAt.UTC().Format(time.RFC3339),
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
	Limit       int           // max results (0 = unlimited)
}

// Query returns session index entries matching the given options, ordered by created_at desc.
func (idx *SessionIndex) Query(opts QueryOptions) ([]SessionIndexEntry, error) {
	query := `SELECT session_key, file_path, created_at, parent_session_key, session_type, status
		FROM session_index WHERE 1=1`
	var args []interface{}

	if opts.AgentID != "" {
		query += ` AND session_key LIKE ?`
		args = append(args, "agent:"+opts.AgentID+":%")
	}
	if opts.SessionType != "" {
		query += ` AND session_type = ?`
		args = append(args, string(opts.SessionType))
	}
	if opts.Status != "" {
		query += ` AND status = ?`
		args = append(args, string(opts.Status))
	}
	query += ` ORDER BY created_at DESC`
	if opts.Limit > 0 {
		query += fmt.Sprintf(` LIMIT %d`, opts.Limit)
	}

	rows, err := idx.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query session index: %w", err)
	}
	defer rows.Close()

	var entries []SessionIndexEntry
	for rows.Next() {
		var e SessionIndexEntry
		var createdStr string
		var parentKey sql.NullString
		var stype, status string
		if err := rows.Scan(&e.SessionKey, &e.FilePath, &createdStr, &parentKey, &stype, &status); err != nil {
			log.Warnf("session_index", "scan row: %v", err)
			continue
		}
		e.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
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
	idx.db.QueryRow("SELECT COUNT(*) FROM session_index").Scan(&n)
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
