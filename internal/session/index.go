package session

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"foci/internal/log"
	"foci/internal/sqlite"
	"foci/internal/timeutil"
)

// SessionType classifies the purpose of a session.
type SessionType string

const (
	SessionTypeChat    SessionType = "chat"
	SessionTypeFacet   SessionType = "facet"
	SessionTypeSpawn   SessionType = "spawn"
	SessionTypeCron    SessionType = "cron"
	SessionTypeBranch  SessionType = "branch"
	SessionTypeUnknown SessionType = "unknown"
)

// SessionStatus tracks the lifecycle state of a session.
type SessionStatus string

const (
	SessionStatusActive    SessionStatus = "active"
	SessionStatusCompacted SessionStatus = "compacted"
	SessionStatusCleared   SessionStatus = "cleared"
	SessionStatusArchived  SessionStatus = "archived"
	SessionStatusReset     SessionStatus = "reset"
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

// OpenSessionIndexReadOnly opens the session index database in read-only mode.
// Used by CLI tools that only need to query the index without modifying it.
func OpenSessionIndexReadOnly(dbPath string) (*SessionIndex, error) {
	db, err := sqlite.OpenReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	return &SessionIndex{db: db}, nil
}

// NewSessionIndex opens (or creates) the SQLite database for the session index.
func NewSessionIndex(dbPath string) (*SessionIndex, error) {
	db, err := sqlite.OpenInit(dbPath,
		`CREATE TABLE IF NOT EXISTS session_index (
			session_key        TEXT PRIMARY KEY,
			file_path          TEXT NOT NULL,
			created_at         TEXT NOT NULL,
			last_activity_at   TEXT,
			last_reflection    TEXT,
			parent_session_key TEXT,
			session_type       TEXT NOT NULL,
			status             TEXT NOT NULL DEFAULT 'active',
			agent_id           TEXT NOT NULL DEFAULT '',
			chat_id            INTEGER NOT NULL DEFAULT 0,
			is_root            INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS agent_metadata (
			agent_id TEXT NOT NULL,
			key      TEXT NOT NULL,
			value    TEXT,
			PRIMARY KEY (agent_id, key)
		)`,
		`CREATE TABLE IF NOT EXISTS chat_metadata (
			agent_id TEXT NOT NULL,
			platform TEXT NOT NULL DEFAULT '',
			chat_id  INTEGER NOT NULL,
			key      TEXT NOT NULL,
			value    TEXT,
			PRIMARY KEY (agent_id, platform, chat_id, key)
		)`,
		`CREATE TABLE IF NOT EXISTS session_metadata (
			session_key TEXT NOT NULL,
			key         TEXT NOT NULL,
			value       TEXT,
			PRIMARY KEY (session_key, key)
		)`,
		`CREATE TABLE IF NOT EXISTS system_state (
			key   TEXT PRIMARY KEY,
			value TEXT
		)`,
		// Per-backend live model-capability catalogue (#840), persisted so a
		// restart restores the last-known caps immediately instead of waiting
		// for the first background fetch. Process-wide (keyed by backend type,
		// not agent). effort/thinking are JSON-encoded string arrays.
		`CREATE TABLE IF NOT EXISTS model_caps (
			backend        TEXT NOT NULL,
			model          TEXT NOT NULL,
			context_window INTEGER NOT NULL DEFAULT 0,
			max_output     INTEGER NOT NULL DEFAULT 0,
			effort_json    TEXT NOT NULL DEFAULT '',
			thinking_json  TEXT NOT NULL DEFAULT '',
			fetched_at     TEXT NOT NULL,
			PRIMARY KEY (backend, model)
		)`,
	)
	if err != nil {
		return nil, err
	}

	// Provenance tables: archive rotations + CC resume-ID history for
	// point-in-time debugging (see provenance.go).
	initProvenanceSchema(db)

	// One-shot legacy migration: pre-stable-identity installs get their
	// rows re-keyed (and old schemas the new columns). Idempotent.
	migrateLegacyStateDB(db)

	// Backfill any null last_reflection to now so a fresh install doesn't
	// stampede all sessions into reflection on first run.
	_, _ = db.Exec(`UPDATE session_index SET last_reflection = ? WHERE last_reflection IS NULL`,
		timeutil.Format(timeutil.Now()))

	// Expression index for correct cross-timezone last_activity_at ordering.
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_session_activity_unix ON session_index(unixepoch(last_activity_at))`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_session_agent ON session_index(agent_id, is_root, status)`)

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

	idx.upsertLocked(e)
}

// keyColumns derives the structured columns (agent_id, chat_id, is_root) from
// a session key string. Archive bookkeeping keys (with dotted suffixes) don't
// parse as session keys — they fall back to agent-only attribution.
func keyColumns(sessionKey string) (agentID string, chatID int64, isRoot int) {
	sk, err := ParseSessionKey(sessionKey)
	if err != nil {
		return AgentIDFromKey(sessionKey), 0, 0
	}
	root := 0
	if sk.IsRoot() {
		root = 1
	}
	return sk.AgentID, sk.ChatID(), root
}

// upsertLocked performs the upsert without acquiring the mutex.
// Caller must hold idx.mu.
func (idx *SessionIndex) upsertLocked(e SessionIndexEntry) {
	activityAt := e.LastActivityAt
	if activityAt.IsZero() {
		activityAt = e.CreatedAt
	}
	activityStr := timeutil.Format(activityAt)
	createdStr := timeutil.Format(e.CreatedAt)
	agentID, chatID, isRoot := keyColumns(e.SessionKey)
	// Seed last_reflection to the row's activity time on INSERT so a freshly
	// created/activated session is NOT immediately "due" for reflection. The
	// SessionsNeedingReflection query treats last_reflection IS NULL as due, so
	// without this a session born from compaction (a new key minted with no new
	// content) was reflected the instant it appeared — an empty, just-rotated
	// session (#945). With the seed, last_reflection == last_activity at birth, so
	// the session becomes due only once REAL new activity advances past it. On
	// CONFLICT (existing row) last_reflection is intentionally NOT touched, so a
	// real prior reflection timestamp is preserved.
	_, err := idx.db.Exec(
		`INSERT INTO session_index (session_key, file_path, created_at, last_activity_at, last_reflection, parent_session_key, session_type, status, agent_id, chat_id, is_root)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(session_key) DO UPDATE SET
		   file_path = excluded.file_path,
		   created_at = excluded.created_at,
		   last_activity_at = CASE
		     WHEN unixepoch(excluded.last_activity_at) > unixepoch(session_index.last_activity_at) THEN excluded.last_activity_at
		     ELSE session_index.last_activity_at
		   END,
		   parent_session_key = excluded.parent_session_key,
		   session_type = excluded.session_type,
		   status = excluded.status,
		   agent_id = excluded.agent_id,
		   chat_id = excluded.chat_id,
		   is_root = excluded.is_root`,
		e.SessionKey,
		e.FilePath,
		createdStr,
		activityStr,
		activityStr, // last_reflection seeded = activity time at birth
		nullableString(e.ParentSessionKey),
		e.SessionType,
		e.Status,
		agentID,
		chatID,
		isRoot,
	)
	if err != nil {
		log.Errorf("session", "upsert index entry %q: %v", e.SessionKey, err)
	}
}

// UpdateActivity updates the last_activity_at timestamp for a session without a full upsert.
func (idx *SessionIndex) UpdateActivity(sessionKey string, activityAt time.Time) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	activityStr := timeutil.Format(activityAt)
	_, err := idx.db.Exec(
		`UPDATE session_index SET last_activity_at = ? WHERE session_key = ?`,
		activityStr,
		sessionKey,
	)
	if err != nil {
		log.Warnf("session", "update activity for %q: %v", sessionKey, err)
	}
}

// StampReflection records when the reflection pass was dispatched for a session.
func (idx *SessionIndex) StampReflection(sessionKey string, at time.Time) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	stamp := timeutil.Format(at)
	_, err := idx.db.Exec(
		`UPDATE session_index SET last_reflection = ? WHERE session_key = ?`,
		stamp,
		sessionKey,
	)
	if err != nil {
		log.Warnf("session", "stamp reflection for %q: %v", sessionKey, err)
	}
}

// ReflectionRedundant reports whether a reflection pass would be redundant for
// the given session: a reflection has already run AND no substantive activity
// has occurred since (last_activity_at <= last_reflection).
//
// This is the single-session inverse of the SessionsNeedingReflection
// predicate. It backs the reset-time skip guard in Agent.FireSessionEndMemory
// — "no need to reflect twice" when nothing happened since the last pass.
//
// Correctness depends on last_activity_at excluding memory-formation turns
// (see isMemoryTrigger): without that, a reflection's own turn would bump
// last_activity_at past last_reflection and this would always return false.
//
// Returns false (→ do reflect) when the session is unknown or has never been
// reflected, so callers default to reflecting rather than silently skipping.
func (idx *SessionIndex) ReflectionRedundant(sessionKey string) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var redundant bool
	err := idx.db.QueryRow(
		`SELECT last_reflection IS NOT NULL
		        AND unixepoch(last_activity_at) <= unixepoch(last_reflection)
		 FROM session_index WHERE session_key = ?`,
		sessionKey,
	).Scan(&redundant)
	if err != nil {
		// sql.ErrNoRows (unknown session) or any error → default to reflecting.
		return false
	}
	return redundant
}

// SessionsNeedingReflection returns active chat session keys for an agent where
// activity has occurred since the last reflection pass (or it has never run).
func (idx *SessionIndex) SessionsNeedingReflection(agentID string) ([]string, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	return idx.querySessionKeysLocked(
		`SELECT session_key FROM session_index
		 WHERE agent_id = ? AND session_type = 'chat' AND status = 'active'
		   AND (last_reflection IS NULL OR unixepoch(last_activity_at) > unixepoch(last_reflection))`,
		agentID,
	)
}

// querySessionKeysLocked runs a query whose result is a single session_key
// column and collects the values. Caller must hold idx.mu.
func (idx *SessionIndex) querySessionKeysLocked(query string, args ...interface{}) ([]string, error) {
	rows, err := idx.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() // nolint:errcheck

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// SessionNeedsReflection reports whether a single chat session is due for
// reflection by the same rule as SessionsNeedingReflection — it has had activity
// since its last reflection (or never reflected). Unlike the bulk query it does
// NOT filter on status='active': its caller (the final reflection fired when an
// app session is archived) invokes it as the session transitions out of active,
// and the "new activity since last reflection" gate is the real condition.
func (idx *SessionIndex) SessionNeedsReflection(sessionKey string) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var due int
	err := idx.db.QueryRow(
		`SELECT 1 FROM session_index
		 WHERE session_key = ? AND session_type = 'chat'
		   AND (last_reflection IS NULL OR unixepoch(last_activity_at) > unixepoch(last_reflection))`,
		sessionKey,
	).Scan(&due)
	return err == nil && due == 1
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
		query += ` AND agent_id = ?`
		args = append(args, opts.AgentID)
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
		cutoff := timeutil.Format(time.Now().Add(-opts.MaxAge))
		query += ` AND unixepoch(last_activity_at) >= unixepoch(?)`
		args = append(args, cutoff)
	}

	query += ` ORDER BY unixepoch(created_at) DESC, created_at DESC`

	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}

	rows, err := idx.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() // nolint:errcheck

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

// Count returns the total number of sessions in the index.
func (idx *SessionIndex) Count() (int, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var count int
	err := idx.db.QueryRow(`SELECT COUNT(*) FROM session_index`).Scan(&count)
	return count, err
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

// TouchActivity updates the last_activity_at timestamp to now.
func (idx *SessionIndex) TouchActivity(sessionKey string) {
	idx.UpdateActivity(sessionKey, time.Now())
}

// RebuildIndex clears and repopulates the session index from the given entries.
// Preserves last_reflection timestamps across the rebuild.
// Wrapped in a single transaction for performance (~3000x fewer fsyncs).
func (idx *SessionIndex) RebuildIndex(entries []SessionIndexEntry) (int, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Preserve last_reflection before clearing — the rebuild can't
	// reconstruct this from disk, and losing it resets reflection
	// scheduling for all sessions.
	savedReflection := make(map[string]string)
	rows, err := idx.db.Query(`SELECT session_key, last_reflection FROM session_index WHERE last_reflection IS NOT NULL`)
	if err != nil {
		// Fail closed: if we can't read the timestamps we're about to clear,
		// don't proceed to DELETE — that would silently wipe every session's
		// last_reflection (unreconstructable from disk). Abort and keep the
		// existing index intact for this cycle.
		return 0, fmt.Errorf("preserve last_reflection: %w", err)
	}
	for rows.Next() {
		var key, stamp string
		if err := rows.Scan(&key, &stamp); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("preserve last_reflection (scan): %w", err)
		}
		savedReflection[key] = stamp
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("preserve last_reflection (rows): %w", err)
	}
	_ = rows.Close()

	tx, err := idx.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after commit

	// Clear only file-backed rows — the scan below re-derives them from
	// disk. Rows with an empty file_path are BACKEND sessions (delegated
	// agents whose conversation lives in CC's own store; same rule as
	// PruneOrphans): they have no file to rescan, and deleting them wipes
	// their last_activity_at — which is what the default-chat routing
	// tiebreak orders by. A rebuild right after restart then picked an
	// arbitrary default chat (the discord-misroute bug).
	if _, err := tx.Exec(`DELETE FROM session_index WHERE file_path != ''`); err != nil {
		return 0, fmt.Errorf("clear index: %w", err)
	}

	stmt, err := tx.Prepare(
		`INSERT OR REPLACE INTO session_index (session_key, file_path, created_at, last_activity_at, parent_session_key, session_type, status, agent_id, chat_id, is_root)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	count := 0
	for _, e := range entries {
		activityAt := e.LastActivityAt
		if activityAt.IsZero() {
			activityAt = e.CreatedAt
		}
		agentID, chatID, isRoot := keyColumns(e.SessionKey)
		_, err := stmt.Exec(
			e.SessionKey,
			e.FilePath,
			timeutil.Format(e.CreatedAt),
			timeutil.Format(activityAt),
			nullableString(e.ParentSessionKey),
			e.SessionType,
			e.Status,
			agentID,
			chatID,
			isRoot,
		)
		if err != nil {
			log.Errorf("session", "insert index entry %q: %v", e.SessionKey, err)
		}
		count++
	}

	// Restore preserved last_reflection timestamps.
	if len(savedReflection) > 0 {
		updateStmt, err := tx.Prepare(`UPDATE session_index SET last_reflection = ? WHERE session_key = ?`)
		if err == nil {
			defer func() { _ = updateStmt.Close() }()
			for key, stamp := range savedReflection {
				_, _ = updateStmt.Exec(stamp, key)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
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

// IndexCount returns the number of entries in the session index.
func (idx *SessionIndex) IndexCount() int {
	if idx == nil || idx.db == nil {
		return 0
	}
	var count int
	_ = idx.db.QueryRow(`SELECT COUNT(*) FROM session_index`).Scan(&count)
	return count
}

// PruneOrphans removes index entries for session files that no longer exist on disk.
// Safe to call concurrently — acquires its own lock.
func (idx *SessionIndex) PruneOrphans() int {
	if idx == nil || idx.db == nil {
		return 0
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()

	rows, err := idx.db.Query(`SELECT session_key, file_path FROM session_index WHERE status = 'active'`)
	if err != nil {
		return 0
	}
	defer func() { _ = rows.Close() }()

	var orphans []string
	for rows.Next() {
		var key, path string
		if rows.Scan(&key, &path) != nil {
			continue
		}
		// Empty path = backend session (CC JSONL) that hasn't had its
		// path populated yet. Not an orphan — skip.
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			orphans = append(orphans, key)
		}
	}

	for _, key := range orphans {
		_, _ = idx.db.Exec(`DELETE FROM session_index WHERE session_key = ?`, key)
		log.Infof("session", "pruned orphan index entry: %s", key)
	}
	return len(orphans)
}

// nullableString returns nil for empty strings, the string otherwise.
func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// metadataTable holds precomputed SQL for a metadata table's CRUD operations.
type metadataTable struct {
	upsertSQL string
	selectSQL string
	deleteSQL string
}

var (
	agentMetaTable = metadataTable{
		upsertSQL: `INSERT INTO agent_metadata (agent_id, key, value) VALUES (?, ?, ?) ON CONFLICT(agent_id, key) DO UPDATE SET value = excluded.value`,
		selectSQL: `SELECT value FROM agent_metadata WHERE agent_id = ? AND key = ?`,
		deleteSQL: `DELETE FROM agent_metadata WHERE agent_id = ? AND key = ?`,
	}
	chatMetaTable = metadataTable{
		upsertSQL: `INSERT INTO chat_metadata (agent_id, platform, chat_id, key, value) VALUES (?, ?, ?, ?, ?) ON CONFLICT(agent_id, platform, chat_id, key) DO UPDATE SET value = excluded.value`,
		selectSQL: `SELECT value FROM chat_metadata WHERE agent_id = ? AND platform = ? AND chat_id = ? AND key = ?`,
		deleteSQL: `DELETE FROM chat_metadata WHERE agent_id = ? AND platform = ? AND chat_id = ? AND key = ?`,
	}
	sessionMetaTable = metadataTable{
		upsertSQL: `INSERT INTO session_metadata (session_key, key, value) VALUES (?, ?, ?) ON CONFLICT(session_key, key) DO UPDATE SET value = excluded.value`,
		selectSQL: `SELECT value FROM session_metadata WHERE session_key = ? AND key = ?`,
		deleteSQL: `DELETE FROM session_metadata WHERE session_key = ? AND key = ?`,
	}
	systemStateTable = metadataTable{
		upsertSQL: `INSERT INTO system_state (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		selectSQL: `SELECT value FROM system_state WHERE key = ?`,
		deleteSQL: `DELETE FROM system_state WHERE key = ?`,
	}
)

// metaSet executes an upsert for a metadata table.
// The value must be the last element of args.
func (idx *SessionIndex) metaSet(mt metadataTable, args ...interface{}) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	_, err := idx.db.Exec(mt.upsertSQL, args...)
	return err
}

// metaGet retrieves a single value from a metadata table.
// Returns empty string if the key is not found.
func (idx *SessionIndex) metaGet(mt metadataTable, args ...interface{}) (string, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var value string
	err := idx.db.QueryRow(mt.selectSQL, args...).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// metaDelete removes a row from a metadata table.
func (idx *SessionIndex) metaDelete(mt metadataTable, args ...interface{}) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	_, err := idx.db.Exec(mt.deleteSQL, args...)
	return err
}

// Agent Metadata Methods

// SetAgentMetadata stores a metadata value for an agent.
func (idx *SessionIndex) SetAgentMetadata(agentID, key, value string) error {
	return idx.metaSet(agentMetaTable, agentID, key, value)
}

// GetAgentMetadata retrieves a metadata value for an agent.
// Returns empty string if not found.
func (idx *SessionIndex) GetAgentMetadata(agentID, key string) (string, error) {
	return idx.metaGet(agentMetaTable, agentID, key)
}

// DeleteAgentMetadata removes a metadata key for an agent.
func (idx *SessionIndex) DeleteAgentMetadata(agentID, key string) error {
	return idx.metaDelete(agentMetaTable, agentID, key)
}

// Chat Metadata Methods

// SetChatMetadata stores a metadata value for a chat.
// Platform identifies the source platform (e.g. "telegram", "discord").
// Use "" for platform-agnostic lookups (e.g. legacy migration, cross-platform queries).
func (idx *SessionIndex) SetChatMetadata(agentID, platform string, chatID int64, key, value string) error {
	return idx.metaSet(chatMetaTable, agentID, platform, chatID, key, value)
}

// GetChatMetadata retrieves a metadata value for a chat.
// Returns empty string if not found.
func (idx *SessionIndex) GetChatMetadata(agentID, platform string, chatID int64, key string) (string, error) {
	return idx.metaGet(chatMetaTable, agentID, platform, chatID, key)
}

// GetChatMetadataAnyPlatform retrieves a metadata value for a chat across all platforms.
// Returns the first match found. Used when the caller doesn't know which platform
// the chat belongs to (e.g. username lookups in /sessions).
func (idx *SessionIndex) GetChatMetadataAnyPlatform(agentID string, chatID int64, key string) (string, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var value string
	err := idx.db.QueryRow(
		`SELECT value FROM chat_metadata WHERE agent_id = ? AND chat_id = ? AND key = ? LIMIT 1`,
		agentID, chatID, key,
	).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// CurrentSessionKeys returns the set of session keys that are the current
// session for any known agent+chat combination. Keys are derived from the
// registered chats in chat_metadata (deterministic agent/c<chatID> keys).
func (idx *SessionIndex) CurrentSessionKeys() (map[string]bool, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	rows, err := idx.db.Query(
		`SELECT DISTINCT agent_id, chat_id FROM chat_metadata WHERE agent_id != '' AND chat_id != 0`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() // nolint:errcheck

	keys := make(map[string]bool)
	for rows.Next() {
		var agentID string
		var chatID int64
		if err := rows.Scan(&agentID, &chatID); err != nil {
			return nil, err
		}
		keys[NewChatSessionKey(agentID, chatID)] = true
	}
	return keys, rows.Err()
}

// DeleteChatMetadata removes a metadata key for a chat.
func (idx *SessionIndex) DeleteChatMetadata(agentID, platform string, chatID int64, key string) error {
	return idx.metaDelete(chatMetaTable, agentID, platform, chatID, key)
}

// ConvRef identifies a persisted platform conversation: the owning agent and
// the platform-native conversation ID (the preimage of the numeric chatID).
type ConvRef struct {
	AgentID string
	ConvID  string
}

// ConvRefs returns every persisted 'conv_id' row for a platform. The row is
// written at binding creation (the app's ensureBinding) and makes a
// conversation durable independently of its frames: startup restore unions
// these with the frame store's restorable set, and default-chat resolution
// uses the per-chat row to reverse the one-way chatID hash.
func (idx *SessionIndex) ConvRefs(platform string) ([]ConvRef, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	rows, err := idx.db.Query(
		`SELECT agent_id, value FROM chat_metadata
		 WHERE platform = ? AND key = 'conv_id' AND agent_id != '' AND value != ''`,
		platform,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []ConvRef
	for rows.Next() {
		var r ConvRef
		if err := rows.Scan(&r.AgentID, &r.ConvID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Chat alias resolution — mapping a user-facing chat alias to a session key so
// `foci send -s <alias>` can target a specific chat.

var (
	ErrAliasNotFound  = errors.New("no chat with that alias")
	ErrAliasAmbiguous = errors.New("alias resolves to multiple chats")
	ErrAliasTaken     = errors.New("alias already in use by another chat")
)

func aliasNorm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ResolveChatAlias maps a chat alias to that chat's session key. Chat keys are
// deterministic (agent/c<chatID>), so the key is derived from the aliased chat
// — unless a 'session_key' adoption override row exists (an app conversation
// pointed at a named session), which wins. Matching is case-insensitive on the
// trimmed alias. Returns ErrAliasNotFound or ErrAliasAmbiguous (the latter only
// for duplicate aliases predating uniqueness).
func (idx *SessionIndex) ResolveChatAlias(agentID, alias string) (string, error) {
	norm := aliasNorm(alias)
	if norm == "" {
		return "", ErrAliasNotFound
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	rows, err := idx.db.Query(
		`SELECT DISTINCT a.chat_id, COALESCE(sk.value, '') FROM chat_metadata a
		 LEFT JOIN chat_metadata sk
		   ON a.agent_id = sk.agent_id AND a.platform = sk.platform
		  AND a.chat_id = sk.chat_id AND sk.key = 'session_key'
		 WHERE a.agent_id = ? AND a.platform = 'app' AND a.key = 'alias'
		   AND lower(trim(a.value)) = ?`,
		agentID, norm,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close() //nolint:errcheck
	// Dedup on the RESOLVED key: distinct chats adopted onto the same named
	// session are one destination, not an ambiguity.
	seen := make(map[string]bool)
	var keys []string
	for rows.Next() {
		var chatID int64
		var override string
		if err := rows.Scan(&chatID, &override); err != nil {
			return "", err
		}
		key := override
		if key == "" {
			key = NewChatSessionKey(agentID, chatID)
		}
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(keys) {
	case 0:
		return "", ErrAliasNotFound
	case 1:
		return keys[0], nil
	default:
		return "", ErrAliasAmbiguous
	}
}

// SetChatAliasUnique persists a chat's alias, rejecting with ErrAliasTaken an
// alias already held by a different chat under the same agent. An empty alias
// clears it. The index mutex serialises this check-then-set with all other
// metadata writes, so it is race-free within the process.
func (idx *SessionIndex) SetChatAliasUnique(agentID, platform string, chatID int64, alias string) error {
	trimmed := strings.TrimSpace(alias)
	if trimmed == "" {
		return idx.SetChatMetadata(agentID, platform, chatID, "alias", "")
	}
	// '/' and ':' are session-key structure; an alias containing them could shadow
	// a real key form at resolution time.
	if strings.ContainsAny(trimmed, "/:") {
		return fmt.Errorf("alias may not contain '/' or ':'")
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var other int64
	err := idx.db.QueryRow(
		`SELECT chat_id FROM chat_metadata WHERE agent_id = ? AND platform = ? AND key = 'alias'
		   AND lower(trim(value)) = ? AND chat_id != ? LIMIT 1`,
		agentID, platform, aliasNorm(trimmed), chatID,
	).Scan(&other)
	switch {
	case err == nil:
		return ErrAliasTaken
	case errors.Is(err, sql.ErrNoRows):
		_, err = idx.db.Exec(chatMetaTable.upsertSQL, agentID, platform, chatID, "alias", trimmed)
		return err
	default:
		return err
	}
}

// PlatformForChat returns the platform name that owns a given chat.
// Any chat_metadata row with a non-empty platform (the "registered" row
// written on first contact, is_default, username, …) establishes ownership.
// Returns "" if no platform-specific mapping exists (first message not yet
// processed).
func (idx *SessionIndex) PlatformForChat(agentID string, chatID int64) string {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var platform string
	err := idx.db.QueryRow(
		`SELECT platform FROM chat_metadata WHERE agent_id = ? AND chat_id = ? AND platform != '' LIMIT 1`,
		agentID, chatID,
	).Scan(&platform)
	if err != nil {
		return ""
	}
	return platform
}

// Session Metadata Methods

// SetSessionMetadata stores a metadata value for a session.
func (idx *SessionIndex) SetSessionMetadata(sessionKey, key, value string) error {
	return idx.metaSet(sessionMetaTable, sessionKey, key, value)
}

// GetSessionMetadata retrieves a metadata value for a session.
// Returns empty string if not found.
func (idx *SessionIndex) GetSessionMetadata(sessionKey, key string) (string, error) {
	return idx.metaGet(sessionMetaTable, sessionKey, key)
}

// DeleteSessionMetadata removes a metadata key for a session.
func (idx *SessionIndex) DeleteSessionMetadata(sessionKey, key string) error {
	return idx.metaDelete(sessionMetaTable, sessionKey, key)
}

// AllSessionMetadata returns all metadata key-value pairs for a session.
func (idx *SessionIndex) AllSessionMetadata(sessionKey string) (map[string]string, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	rows, err := idx.db.Query(
		`SELECT key, value FROM session_metadata WHERE session_key = ?`,
		sessionKey,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	result := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		result[k] = v
	}
	return result, rows.Err()
}

// System State Methods

// SetSystemState stores a system state value.
func (idx *SessionIndex) SetSystemState(key, value string) error {
	return idx.metaSet(systemStateTable, key, value)
}

// GetSystemState retrieves a system state value.
// Returns empty string if not found.
func (idx *SessionIndex) GetSystemState(key string) (string, error) {
	return idx.metaGet(systemStateTable, key)
}

// DeleteSystemState removes a system state key.
func (idx *SessionIndex) DeleteSystemState(key string) error {
	return idx.metaDelete(systemStateTable, key)
}

// SetDefaultChat marks a chat as the default for an agent on a specific platform.
// Clears any previous default for that agent on the same platform, then sets
// the new one via an is_default=true row in chat_metadata.
// Each platform maintains its own independent default.
func (idx *SessionIndex) SetDefaultChat(agentID, platform string, chatID int64) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Clear any existing default for this agent on this platform.
	if _, err := idx.db.Exec(
		`DELETE FROM chat_metadata WHERE agent_id = ? AND platform = ? AND key = 'is_default'`,
		agentID, platform,
	); err != nil {
		return err
	}

	// Set new default.
	_, err := idx.db.Exec(
		chatMetaTable.upsertSQL,
		agentID, platform, chatID, "is_default", "true",
	)
	return err
}

// DefaultChatForAgent returns the default chatID for an agent on a specific platform.
// Returns 0 if no default is set for that platform.
func (idx *SessionIndex) DefaultChatForAgent(agentID, platform string) int64 {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var chatID int64
	err := idx.db.QueryRow(
		`SELECT chat_id FROM chat_metadata WHERE agent_id = ? AND platform = ? AND key = 'is_default' AND value = 'true'`,
		agentID, platform,
	).Scan(&chatID)
	if err != nil {
		return 0
	}
	return chatID
}

// ClearDefaultChat removes the default chat for an agent on a specific platform.
func (idx *SessionIndex) ClearDefaultChat(agentID, platform string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	_, err := idx.db.Exec(
		`DELETE FROM chat_metadata WHERE agent_id = ? AND platform = ? AND key = 'is_default'`,
		agentID, platform,
	)
	return err
}

// SetArchivedChat sets or clears the archived flag for a chat on a specific
// platform. The flag is a sibling of is_default in chat_metadata (key
// 'is_archived'); archived conversations are hidden from the app roster but
// retain their replay frames, binding, and session — unarchive is reversible.
// Removing the row (archived=false) rather than storing 'false' keeps the table
// sparse: absence means not-archived, matching how is_default absence means
// no default.
func (idx *SessionIndex) SetArchivedChat(agentID, platform string, chatID int64, archived bool) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	if !archived {
		_, err := idx.db.Exec(
			`DELETE FROM chat_metadata WHERE agent_id = ? AND platform = ? AND chat_id = ? AND key = 'is_archived'`,
			agentID, platform, chatID,
		)
		return err
	}
	_, err := idx.db.Exec(
		chatMetaTable.upsertSQL,
		agentID, platform, chatID, "is_archived", "true",
	)
	return err
}

// ArchivedChatsForAgent returns the set of archived chatIDs for an agent on a
// specific platform. Used by the app roster builder to flag archived rows
// (mirroring DefaultChatForAgent). Empty (nil) map means none archived.
func (idx *SessionIndex) ArchivedChatsForAgent(agentID, platform string) map[int64]bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	rows, err := idx.db.Query(
		`SELECT chat_id FROM chat_metadata WHERE agent_id = ? AND platform = ? AND key = 'is_archived' AND value = 'true'`,
		agentID, platform,
	)
	if err != nil {
		return nil
	}
	defer rows.Close() //nolint:errcheck

	out := make(map[int64]bool)
	for rows.Next() {
		var chatID int64
		if err := rows.Scan(&chatID); err != nil {
			return out
		}
		out[chatID] = true
	}
	return out
}

// DefaultChatIDs returns all default chat IDs for an agent across all platforms.
// Used by /sessions to mark defaults with ★ regardless of which platform the
// command is invoked from.
func (idx *SessionIndex) DefaultChatIDs(agentID string) map[int64]bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	rows, err := idx.db.Query(
		`SELECT chat_id FROM chat_metadata WHERE agent_id = ? AND key = 'is_default' AND value = 'true'`,
		agentID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close() //nolint:errcheck

	result := make(map[int64]bool)
	for rows.Next() {
		var chatID int64
		if err := rows.Scan(&chatID); err == nil {
			result[chatID] = true
		}
	}
	return result
}

// DefaultSessionKeyForAgent resolves the most recently active session key for
// an agent with no platform preference. See DefaultSessionKeyForAgentOn.
func (idx *SessionIndex) DefaultSessionKeyForAgent(agentID string) string {
	return idx.DefaultSessionKeyForAgentOn(agentID, "")
}

// DefaultSessionKeyForAgentOn resolves an agent's default session, preferring
// the given platform when non-empty (the configured default_platform):
//
//  1. The preferred platform's is_default chat.
//  2. The preferred platform's most recently active registered chat.
//  3. Any is_default chat, ordered by derived-session activity.
//  4. The most recently active root session for the agent.
//
// Chat session keys are deterministic (agent/c<chatID>), so chats resolve by
// derivation.
func (idx *SessionIndex) DefaultSessionKeyForAgentOn(agentID, preferredPlatform string) string {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	if preferredPlatform != "" {
		// Preferred platform's pinned default chat.
		var chatID int64
		err := idx.db.QueryRow(
			`SELECT chat_id FROM chat_metadata
			 WHERE agent_id = ? AND platform = ? AND key = 'is_default' AND value = 'true'`,
			agentID, preferredPlatform,
		).Scan(&chatID)
		if err == nil && chatID != 0 {
			return NewChatSessionKey(agentID, chatID)
		}
		// Preferred platform's most recently active registered chat.
		err = idx.db.QueryRow(
			`SELECT cm.chat_id FROM (SELECT DISTINCT agent_id, platform, chat_id FROM chat_metadata) cm
			 LEFT JOIN session_index si
			   ON si.agent_id = cm.agent_id AND si.chat_id = cm.chat_id AND si.is_root = 1
			 WHERE cm.agent_id = ? AND cm.platform = ? AND cm.chat_id != 0
			 ORDER BY unixepoch(si.last_activity_at) DESC NULLS LAST
			 LIMIT 1`,
			agentID, preferredPlatform,
		).Scan(&chatID)
		if err == nil && chatID != 0 {
			return NewChatSessionKey(agentID, chatID)
		}
		// No presence on the preferred platform — fall through.
	}

	// Any default chat. The is_default flag is platform-scoped; the session
	// key is derived from (agent, chat). Order by the derived session's
	// activity so an agent with defaults on several platforms resolves to the
	// live one.
	var chatID int64
	err := idx.db.QueryRow(
		`SELECT cm.chat_id FROM chat_metadata cm
		 LEFT JOIN session_index si
		   ON si.agent_id = cm.agent_id AND si.chat_id = cm.chat_id AND si.is_root = 1
		 WHERE cm.agent_id = ? AND cm.key = 'is_default' AND cm.value = 'true'
		 ORDER BY unixepoch(si.last_activity_at) DESC NULLS LAST
		 LIMIT 1`,
		agentID,
	).Scan(&chatID)
	if err == nil && chatID != 0 {
		return NewChatSessionKey(agentID, chatID)
	}

	// Fallback: most recently active root session for the agent.
	var key string
	err = idx.db.QueryRow(
		`SELECT session_key FROM session_index
		 WHERE agent_id = ? AND is_root = 1 AND status = 'active'
		 ORDER BY unixepoch(last_activity_at) DESC, unixepoch(created_at) DESC
		 LIMIT 1`,
		agentID,
	).Scan(&key)
	if err != nil {
		return ""
	}
	return key
}

// DeleteAllSessionMetadata removes every metadata row for a session key. Used
// to clean up rows left under a defunct key (e.g. after an async reset rotates
// away and then reflects on the old key).
func (idx *SessionIndex) DeleteAllSessionMetadata(sessionKey string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	_, err := idx.db.Exec(
		`DELETE FROM session_metadata WHERE session_key = ?`, sessionKey,
	)
	return err
}

// SessionKeysWithMetadata returns all session keys that have a given metadata key set.
// Used for cleanup of stale session metadata (e.g. no_compact entries for defunct sessions).
func (idx *SessionIndex) SessionKeysWithMetadata(key string) ([]string, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	return idx.querySessionKeysLocked(
		`SELECT session_key FROM session_metadata WHERE key = ?`, key,
	)
}

// AgentMetadataByPrefix returns all metadata entries for an agent whose key starts with prefix.
// Used for facet session restoration (prefix="facet:") and similar bulk lookups.
func (idx *SessionIndex) AgentMetadataByPrefix(agentID, prefix string) (map[string]string, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	rows, err := idx.db.Query(
		`SELECT key, value FROM agent_metadata WHERE agent_id = ? AND key LIKE ?`,
		agentID, prefix+"%",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	result := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		result[k] = v
	}
	return result, rows.Err()
}

// SessionExists reports whether a session key has an index row.
func (idx *SessionIndex) SessionExists(sessionKey string) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var one int
	err := idx.db.QueryRow(
		`SELECT 1 FROM session_index WHERE session_key = ?`, sessionKey,
	).Scan(&one)
	return err == nil
}

// ResolveLooseKey resolves a "loose" target — a bare agent name ("scout") —
// to a full active session key via DefaultSessionKeyForAgent. Anything
// containing "/" is already a full session key under the stable-identity
// grammar and returns "" — callers that accept full keys should
// ParseSessionKey them first. Returns "" when nothing resolves.
func (idx *SessionIndex) ResolveLooseKey(key string) string {
	if strings.Contains(key, "/") {
		return ""
	}
	return idx.DefaultSessionKeyForAgent(key)
}
