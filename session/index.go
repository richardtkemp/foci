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

	// Agent metadata table (replaces state.json for agent-level keys)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS agent_metadata (
		agent_id TEXT NOT NULL,
		key      TEXT NOT NULL,
		value    TEXT,
		PRIMARY KEY (agent_id, key)
	)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create agent_metadata table: %w", err)
	}

	// Chat metadata table (replaces state.json for chat-level keys)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS chat_metadata (
		agent_id TEXT NOT NULL,
		chat_id  INTEGER NOT NULL,
		key      TEXT NOT NULL,
		value    TEXT,
		PRIMARY KEY (agent_id, chat_id, key)
	)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create chat_metadata table: %w", err)
	}

	// Session metadata table (replaces state.json for session-level keys like no_compact)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS session_metadata (
		session_key TEXT NOT NULL,
		key         TEXT NOT NULL,
		value       TEXT,
		PRIMARY KEY (session_key, key)
	)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create session_metadata table: %w", err)
	}

	// System state table (replaces state.json for system-level keys)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS system_state (
		key   TEXT PRIMARY KEY,
		value TEXT
	)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create system_state table: %w", err)
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
		     WHEN excluded.last_activity_at > session_index.last_activity_at THEN excluded.last_activity_at
		     ELSE session_index.last_activity_at
		   END,
		   parent_session_key = excluded.parent_session_key,
		   session_type = excluded.session_type,
		   status = excluded.status`,
		e.SessionKey,
		e.FilePath,
		createdStr,
		activityStr,
		nullableString(e.ParentSessionKey),
		e.SessionType,
		e.Status,
	)
	if err != nil {
		log.Errorf("session", "upsert index entry %q: %v", e.SessionKey, err)
	}
}

// UpdateActivity updates the last_activity_at timestamp for a session without a full upsert.
func (idx *SessionIndex) UpdateActivity(sessionKey string, activityAt time.Time) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	activityStr := activityAt.UTC().Format(time.RFC3339)
	_, err := idx.db.Exec(
		`UPDATE session_index SET last_activity_at = ? WHERE session_key = ?`,
		activityStr,
		sessionKey,
	)
	if err != nil {
		log.Warnf("session", "update activity for %q: %v", sessionKey, err)
	}
}

// Get retrieves a session index entry by key.
func (idx *SessionIndex) Get(sessionKey string) (SessionIndexEntry, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var e SessionIndexEntry
	var createdStr, activityStr string
	var parentKey sql.NullString

	err := idx.db.QueryRow(
		`SELECT session_key, file_path, created_at, last_activity_at, parent_session_key, session_type, status
		 FROM session_index WHERE session_key = ?`,
		sessionKey,
	).Scan(&e.SessionKey, &e.FilePath, &createdStr, &activityStr, &parentKey, &e.SessionType, &e.Status)
	if err != nil {
		return e, err
	}

	e.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	e.LastActivityAt, _ = time.Parse(time.RFC3339, activityStr)
	if parentKey.Valid {
		e.ParentSessionKey = parentKey.String
	}
	return e, nil
}

// QueryOptions configures session index queries.
type QueryOptions struct {
	AgentID     string        // filter by agent ID (empty = all)
	SessionType string        // filter by type (empty = all)
	Status      string        // filter by status (empty = all)
	MaxAge      time.Duration // only sessions with activity within this duration (0 = no limit)
	Limit       int           // max results (0 = unlimited)
}

// Query retrieves session index entries matching the given options.
func (idx *SessionIndex) Query(opts QueryOptions) ([]SessionIndexEntry, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	query := `SELECT session_key, file_path, created_at, last_activity_at, parent_session_key, session_type, status
	          FROM session_index WHERE 1=1`
	var args []interface{}

	if opts.AgentID != "" {
		query += ` AND session_key LIKE ?`
		args = append(args, opts.AgentID+"/%")
	}
	if opts.SessionType != "" {
		query += ` AND session_type = ?`
		args = append(args, opts.SessionType)
	}
	if opts.Status != "" {
		query += ` AND status = ?`
		args = append(args, opts.Status)
	}
	if opts.MaxAge > 0 {
		cutoff := time.Now().UTC().Add(-opts.MaxAge).Format(time.RFC3339)
		query += ` AND last_activity_at >= ?`
		args = append(args, cutoff)
	}

	query += ` ORDER BY created_at DESC`

	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}

	rows, err := idx.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []SessionIndexEntry
	for rows.Next() {
		var e SessionIndexEntry
		var createdStr, activityStr string
		var parentKey sql.NullString

		if err := rows.Scan(&e.SessionKey, &e.FilePath, &createdStr, &activityStr, &parentKey, &e.SessionType, &e.Status); err != nil {
			return nil, err
		}
		e.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
		e.LastActivityAt, _ = time.Parse(time.RFC3339, activityStr)
		if parentKey.Valid {
			e.ParentSessionKey = parentKey.String
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// Delete removes a session from the index.
func (idx *SessionIndex) Delete(sessionKey string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	_, err := idx.db.Exec(`DELETE FROM session_index WHERE session_key = ?`, sessionKey)
	if err != nil {
		log.Errorf("session", "delete index entry %q: %v", sessionKey, err)
	}
}

// UpdateStatus updates the status field for a session.
func (idx *SessionIndex) UpdateStatus(sessionKey string, status SessionStatus) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	_, err := idx.db.Exec(
		`UPDATE session_index SET status = ? WHERE session_key = ?`,
		status,
		sessionKey,
	)
	if err != nil {
		log.Warnf("session", "update status for %q: %v", sessionKey, err)
	}
}

// SetStatus is an alias for UpdateStatus (for backwards compatibility).
func (idx *SessionIndex) SetStatus(sessionKey string, status SessionStatus) {
	idx.UpdateStatus(sessionKey, status)
}

// TouchActivity updates the last_activity_at timestamp to now.
func (idx *SessionIndex) TouchActivity(sessionKey string) {
	idx.UpdateActivity(sessionKey, time.Now())
}

// RebuildIndex scans all session files and rebuilds the index.
func (idx *SessionIndex) RebuildIndex(entries []SessionIndexEntry) (int, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	if _, err := idx.db.Exec(`DELETE FROM session_index`); err != nil {
		return 0, fmt.Errorf("clear index: %w", err)
	}

	count := 0
	for _, e := range entries {
		idx.Upsert(e)
		count++
	}
	return count, nil
}

// Rebuild scans all session files from the store and rebuilds the index.
func (idx *SessionIndex) Rebuild(store *Store) (int, error) {
	entries, err := store.ScanAllSessions()
	if err != nil {
		return 0, err
	}
	return idx.RebuildIndex(entries)
}

// nullableString returns nil for empty strings, the string otherwise.
func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// Agent Metadata Methods
// These replace state.json storage for agent-level metadata.

// SetAgentMetadata stores a metadata value for an agent.
func (idx *SessionIndex) SetAgentMetadata(agentID, key, value string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	_, err := idx.db.Exec(
		`INSERT INTO agent_metadata (agent_id, key, value) VALUES (?, ?, ?)
		 ON CONFLICT(agent_id, key) DO UPDATE SET value = excluded.value`,
		agentID, key, value,
	)
	return err
}

// GetAgentMetadata retrieves a metadata value for an agent.
// Returns empty string if not found.
func (idx *SessionIndex) GetAgentMetadata(agentID, key string) (string, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var value string
	err := idx.db.QueryRow(
		`SELECT value FROM agent_metadata WHERE agent_id = ? AND key = ?`,
		agentID, key,
	).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// DeleteAgentMetadata removes a metadata key for an agent.
func (idx *SessionIndex) DeleteAgentMetadata(agentID, key string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	_, err := idx.db.Exec(
		`DELETE FROM agent_metadata WHERE agent_id = ? AND key = ?`,
		agentID, key,
	)
	return err
}

// Chat Metadata Methods
// These replace state.json storage for chat-level metadata.

// SetChatMetadata stores a metadata value for a chat.
func (idx *SessionIndex) SetChatMetadata(agentID string, chatID int64, key, value string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	_, err := idx.db.Exec(
		`INSERT INTO chat_metadata (agent_id, chat_id, key, value) VALUES (?, ?, ?, ?)
		 ON CONFLICT(agent_id, chat_id, key) DO UPDATE SET value = excluded.value`,
		agentID, chatID, key, value,
	)
	return err
}

// GetChatMetadata retrieves a metadata value for a chat.
// Returns empty string if not found.
func (idx *SessionIndex) GetChatMetadata(agentID string, chatID int64, key string) (string, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var value string
	err := idx.db.QueryRow(
		`SELECT value FROM chat_metadata WHERE agent_id = ? AND chat_id = ? AND key = ?`,
		agentID, chatID, key,
	).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// DeleteChatMetadata removes a metadata key for a chat.
func (idx *SessionIndex) DeleteChatMetadata(agentID string, chatID int64, key string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	_, err := idx.db.Exec(
		`DELETE FROM chat_metadata WHERE agent_id = ? AND chat_id = ? AND key = ?`,
		agentID, chatID, key,
	)
	return err
}

// Session Metadata Methods
// These replace state.json storage for session-level settings.

// SetSessionMetadata stores a metadata value for a session.
func (idx *SessionIndex) SetSessionMetadata(sessionKey, key, value string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	_, err := idx.db.Exec(
		`INSERT INTO session_metadata (session_key, key, value) VALUES (?, ?, ?)
		 ON CONFLICT(session_key, key) DO UPDATE SET value = excluded.value`,
		sessionKey, key, value,
	)
	return err
}

// GetSessionMetadata retrieves a metadata value for a session.
// Returns empty string if not found.
func (idx *SessionIndex) GetSessionMetadata(sessionKey, key string) (string, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var value string
	err := idx.db.QueryRow(
		`SELECT value FROM session_metadata WHERE session_key = ? AND key = ?`,
		sessionKey, key,
	).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// DeleteSessionMetadata removes a metadata key for a session.
func (idx *SessionIndex) DeleteSessionMetadata(sessionKey, key string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	_, err := idx.db.Exec(
		`DELETE FROM session_metadata WHERE session_key = ? AND key = ?`,
		sessionKey, key,
	)
	return err
}

// System State Methods
// These replace state.json storage for system-level state.

// SetSystemState stores a system state value.
func (idx *SessionIndex) SetSystemState(key, value string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	_, err := idx.db.Exec(
		`INSERT INTO system_state (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	return err
}

// GetSystemState retrieves a system state value.
// Returns empty string if not found.
func (idx *SessionIndex) GetSystemState(key string) (string, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var value string
	err := idx.db.QueryRow(
		`SELECT value FROM system_state WHERE key = ?`,
		key,
	).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// DeleteSystemState removes a system state key.
func (idx *SessionIndex) DeleteSystemState(key string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	_, err := idx.db.Exec(
		`DELETE FROM system_state WHERE key = ?`,
		key,
	)
	return err
}
