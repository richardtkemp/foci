package session

import (
	"database/sql"
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
	SessionStatusRotated   SessionStatus = "rotated"
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
			parent_session_key TEXT,
			session_type       TEXT NOT NULL,
			status             TEXT NOT NULL DEFAULT 'active'
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

	// Migration: add last_activity_at column if missing (idempotent).
	_, _ = db.Exec(`ALTER TABLE session_index ADD COLUMN last_activity_at TEXT`)

	// Migration: add last_reflection column if missing, copy data from the
	// now-defunct last_memory_formation column (if present) and drop the old
	// column. Backfill any still-null values to now so a fresh install doesn't
	// stampede all sessions into reflection on first run.
	_, _ = db.Exec(`ALTER TABLE session_index ADD COLUMN last_reflection TEXT`)
	migrateLastReflection(db)
	_, _ = db.Exec(`UPDATE session_index SET last_reflection = ? WHERE last_reflection IS NULL`,
		timeutil.Format(timeutil.Now()))

	// Migration: add platform column to chat_metadata if missing (idempotent).
	// Detects old schema by checking column count, then rebuilds the table in a transaction.
	migrateChatMetadataPlatform(db)

	// Migration: move CC resume IDs from agent_metadata (key='cc_session:<sk>')
	// to session_metadata (session_key=<sk>, key='cc_resume_id'). The data is
	// session-scoped (each post-compact JSONL has its own UUID), so it belongs
	// in session_metadata where rotation gets a single-UPDATE rename via
	// RenameSessionMetadata instead of the manual load/save/clear pattern.
	migrateCcResumeIDs(db)

	// Expression index for correct cross-timezone last_activity_at ordering.
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_session_activity_unix ON session_index(unixepoch(last_activity_at))`)

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

// upsertLocked performs the upsert without acquiring the mutex.
// Caller must hold idx.mu.
func (idx *SessionIndex) upsertLocked(e SessionIndexEntry) {
	activityAt := e.LastActivityAt
	if activityAt.IsZero() {
		activityAt = e.CreatedAt
	}
	activityStr := timeutil.Format(activityAt)
	createdStr := timeutil.Format(e.CreatedAt)
	_, err := idx.db.Exec(
		`INSERT INTO session_index (session_key, file_path, created_at, last_activity_at, parent_session_key, session_type, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(session_key) DO UPDATE SET
		   file_path = excluded.file_path,
		   created_at = excluded.created_at,
		   last_activity_at = CASE
		     WHEN unixepoch(excluded.last_activity_at) > unixepoch(session_index.last_activity_at) THEN excluded.last_activity_at
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

	rows, err := idx.db.Query(
		`SELECT session_key FROM session_index
		 WHERE session_key LIKE ? AND session_type = 'chat' AND status = 'active'
		   AND (last_reflection IS NULL OR unixepoch(last_activity_at) > unixepoch(last_reflection))`,
		agentID+"/%",
	)
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

	if _, err := tx.Exec(`DELETE FROM session_index`); err != nil {
		return 0, fmt.Errorf("clear index: %w", err)
	}

	stmt, err := tx.Prepare(
		`INSERT INTO session_index (session_key, file_path, created_at, last_activity_at, parent_session_key, session_type, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`)
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
		_, err := stmt.Exec(
			e.SessionKey,
			e.FilePath,
			timeutil.Format(e.CreatedAt),
			timeutil.Format(activityAt),
			nullableString(e.ParentSessionKey),
			e.SessionType,
			e.Status,
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

// migrateLastReflection copies data from the legacy last_memory_formation column
// into the new last_reflection column, then drops the old column. No-op if the
// old column doesn't exist (fresh install on post-rename schema).
func migrateLastReflection(db *sql.DB) {
	// Probe for old column presence by querying it.
	var dummy sql.NullString
	err := db.QueryRow(`SELECT last_memory_formation FROM session_index LIMIT 1`).Scan(&dummy)
	if err != nil && err != sql.ErrNoRows {
		// "no such column" — nothing to migrate.
		return
	}
	log.Infof("session", "migrating last_memory_formation → last_reflection")
	if _, err := db.Exec(`UPDATE session_index SET last_reflection = last_memory_formation WHERE last_reflection IS NULL AND last_memory_formation IS NOT NULL`); err != nil {
		log.Errorf("session", "copy last_memory_formation: %v", err)
		return
	}
	if _, err := db.Exec(`ALTER TABLE session_index DROP COLUMN last_memory_formation`); err != nil {
		log.Warnf("session", "drop last_memory_formation (SQLite <3.35?): %v — leaving column in place", err)
	}
}

// migrateCcResumeIDs moves CC backend resume UUIDs from agent_metadata
// (where they were stored under key='cc_session:<sessionKey>') into
// session_metadata (session_key=<sessionKey>, key='cc_resume_id'). The
// agent_id is dropped: session keys begin with the agent ID, so no
// information is lost. Idempotent — re-running after a successful migration
// is a no-op because the source rows are deleted in the same transaction.
func migrateCcResumeIDs(db *sql.DB) {
	// Cheap probe: any rows to migrate?
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_metadata WHERE key LIKE 'cc_session:%'`).Scan(&n); err != nil || n == 0 {
		return
	}
	log.Infof("session", "migrating %d cc_session row(s) from agent_metadata to session_metadata", n)
	tx, err := db.Begin()
	if err != nil {
		log.Errorf("session", "cc_resume_id migration: begin tx: %v", err)
		return
	}
	// Copy: session_key is the suffix after 'cc_session:'.
	if _, err := tx.Exec(`
		INSERT OR REPLACE INTO session_metadata (session_key, key, value)
		SELECT substr(key, length('cc_session:')+1), 'cc_resume_id', value
		FROM agent_metadata WHERE key LIKE 'cc_session:%'
	`); err != nil {
		log.Errorf("session", "cc_resume_id migration: copy: %v", err)
		_ = tx.Rollback()
		return
	}
	if _, err := tx.Exec(`DELETE FROM agent_metadata WHERE key LIKE 'cc_session:%'`); err != nil {
		log.Errorf("session", "cc_resume_id migration: delete: %v", err)
		_ = tx.Rollback()
		return
	}
	if err := tx.Commit(); err != nil {
		log.Errorf("session", "cc_resume_id migration: commit: %v", err)
	}
}

// migrateChatMetadataPlatform adds the platform column to chat_metadata if missing.
// Detects old schema by attempting to select the platform column. If it doesn't
// exist, rebuilds the table in a transaction: rename → create new → copy → drop old.
// Old rows get platform=” which won't match explicit platform queries.
func migrateChatMetadataPlatform(db *sql.DB) {
	// Check if platform column already exists by querying it.
	var dummy string
	err := db.QueryRow(`SELECT platform FROM chat_metadata LIMIT 1`).Scan(&dummy)
	if err == nil || err == sql.ErrNoRows {
		return // column exists
	}
	// Column doesn't exist — err is a "no such column" error. Rebuild.
	log.Infof("session", "migrating chat_metadata: adding platform column")
	tx, err := db.Begin()
	if err != nil {
		log.Errorf("session", "chat_metadata migration: begin tx: %v", err)
		return
	}
	stmts := []string{
		`ALTER TABLE chat_metadata RENAME TO chat_metadata_old`,
		`CREATE TABLE chat_metadata (
			agent_id TEXT NOT NULL,
			platform TEXT NOT NULL DEFAULT '',
			chat_id  INTEGER NOT NULL,
			key      TEXT NOT NULL,
			value    TEXT,
			PRIMARY KEY (agent_id, platform, chat_id, key)
		)`,
		`INSERT INTO chat_metadata (agent_id, platform, chat_id, key, value)
		 SELECT agent_id, '', chat_id, key, value FROM chat_metadata_old`,
		`DROP TABLE chat_metadata_old`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			log.Errorf("session", "chat_metadata migration: %v", err)
			_ = tx.Rollback()
			return
		}
	}
	if err := tx.Commit(); err != nil {
		log.Errorf("session", "chat_metadata migration: commit: %v", err)
		return
	}
	log.Infof("session", "chat_metadata migration complete")
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

// CurrentSessionKeys returns the set of session keys that are the active/current
// session for any agent+chat combination (i.e. all "session_key" values in chat_metadata).
func (idx *SessionIndex) CurrentSessionKeys() (map[string]bool, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	rows, err := idx.db.Query(
		`SELECT value FROM chat_metadata WHERE key = 'session_key'`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() // nolint:errcheck

	keys := make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		keys[v] = true
	}
	return keys, rows.Err()
}

// DeleteChatMetadata removes a metadata key for a chat.
func (idx *SessionIndex) DeleteChatMetadata(agentID, platform string, chatID int64, key string) error {
	return idx.metaDelete(chatMetaTable, agentID, platform, chatID, key)
}

// PlatformForChat returns the platform name that owns a given chat's session key.
// Returns "" if no platform-specific mapping exists (e.g. legacy data or first message).
func (idx *SessionIndex) PlatformForChat(agentID string, chatID int64) string {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var platform string
	err := idx.db.QueryRow(
		`SELECT platform FROM chat_metadata WHERE agent_id = ? AND chat_id = ? AND key = 'session_key' AND platform != ''`,
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
// an agent. First checks for default chats (is_default flag, any platform),
// picking the one with the most recent activity. Falls back to chat_metadata
// session keys, and finally queries session_index for the most recently active
// root session.
func (idx *SessionIndex) DefaultSessionKeyForAgent(agentID string) string {
	// Try default chats. The is_default flag is platform-scoped, so the
	// authoritative session_key is the one on the SAME platform as the flag.
	// Resolving "any platform" here could grab a stale empty-platform pointer
	// left by a superseded session — the inter-agent send-routing bug, where a
	// bare `send_to_session <agent>` landed in a dead session instead of the
	// user's live default chat. Join session_key on the is_default row's exact
	// (agent, platform, chat) and pick the most recently active default chat.
	idx.mu.Lock()
	var sk string
	_ = idx.db.QueryRow(
		`SELECT skm.value FROM chat_metadata cm
		 JOIN chat_metadata skm
		   ON skm.agent_id = cm.agent_id
		  AND skm.platform = cm.platform
		  AND skm.chat_id  = cm.chat_id
		  AND skm.key = 'session_key'
		 LEFT JOIN session_index si ON si.session_key = skm.value
		 WHERE cm.agent_id = ? AND cm.key = 'is_default' AND cm.value = 'true'
		 ORDER BY unixepoch(si.last_activity_at) DESC NULLS LAST
		 LIMIT 1`,
		agentID,
	).Scan(&sk)
	idx.mu.Unlock()
	if sk != "" {
		return sk
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Try chat_metadata: find session keys assigned to this agent.
	rows, err := idx.db.Query(
		`SELECT value FROM chat_metadata WHERE agent_id = ? AND key = 'session_key'`,
		agentID,
	)
	if err == nil {
		defer rows.Close() //nolint:errcheck
		var candidates []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err == nil && v != "" {
				candidates = append(candidates, v)
			}
		}
		if len(candidates) == 1 {
			return candidates[0]
		}
		if len(candidates) > 1 {
			// Multiple chats — pick the most recently active via session_index.
			var best string
			err := idx.db.QueryRow(
				`SELECT si.session_key FROM session_index si
				 WHERE si.session_key IN (`+placeholders(len(candidates))+`)
				 ORDER BY unixepoch(si.last_activity_at) DESC
				 LIMIT 1`,
				toArgs(candidates)...,
			).Scan(&best)
			if err == nil {
				return best
			}
			// Fall back to first candidate if index query fails.
			return candidates[0]
		}
	}

	// Fallback: query session_index for most recently active root session.
	// Root sessions have exactly 3 segments (agent/typeID/versionTS).
	var key string
	err = idx.db.QueryRow(
		`SELECT session_key FROM session_index
		 WHERE session_key LIKE ? AND status = 'active'
		   AND session_key NOT LIKE ?
		 ORDER BY unixepoch(last_activity_at) DESC, unixepoch(created_at) DESC
		 LIMIT 1`,
		agentID+"/%",
		agentID+"/%/%/%", // exclude children (4+ segments)
	).Scan(&key)
	if err != nil {
		return ""
	}
	return key
}

// placeholders generates a comma-separated list of ? for SQL IN clauses.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	s := strings.Repeat("?,", n)
	return s[:len(s)-1]
}

// toArgs converts a string slice to []interface{} for sql.Query.
func toArgs(ss []string) []interface{} {
	args := make([]interface{}, len(ss))
	for i, s := range ss {
		args[i] = s
	}
	return args
}

// RotateChatSessionKey updates chat_metadata session_key rows that currently hold oldKey
// to newKey. Uses a conditional UPDATE (WHERE value = oldKey) so it only touches the correct
// row(s) regardless of platform — no spurious rows are created, and rows with a different
// value are left untouched.
func (idx *SessionIndex) RotateChatSessionKey(agentID string, chatID int64, oldKey, newKey string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	_, err := idx.db.Exec(
		`UPDATE chat_metadata SET value = ? WHERE agent_id = ? AND chat_id = ? AND key = 'session_key' AND value = ?`,
		newKey, agentID, chatID, oldKey,
	)
	return err
}

// RenameSessionMetadata atomically renames all session_metadata rows from oldKey to newKey.
// Used by RotateSession to migrate per-session state in a single UPDATE.
func (idx *SessionIndex) RenameSessionMetadata(oldKey, newKey string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	_, err := idx.db.Exec(
		`UPDATE session_metadata SET session_key = ? WHERE session_key = ?`,
		newKey, oldKey,
	)
	return err
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
// Used for cleanup of stale session metadata (e.g. no_compact entries for rotated sessions).
func (idx *SessionIndex) SessionKeysWithMetadata(key string) ([]string, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	rows, err := idx.db.Query(
		`SELECT session_key FROM session_metadata WHERE key = ?`, key,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var keys []string
	for rows.Next() {
		var sk string
		if err := rows.Scan(&sk); err != nil {
			return nil, err
		}
		keys = append(keys, sk)
	}
	return keys, rows.Err()
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

// ResolvePartialKey resolves a partial session key (agent/typeID, e.g.
// "scout/c5970082313") to the most recently active full key with a versionTS
// ("scout/c5970082313/1772794601"). Only accepts keys with exactly 2
// slash-separated segments where the second starts with 'c' or 'i'.
// Returns "" if no match is found or the format is invalid.
func (idx *SessionIndex) ResolvePartialKey(partialKey string) string {
	// Validate format: must be exactly agent/typeID (2 segments)
	parts := strings.Split(partialKey, "/")
	if len(parts) != 2 || len(parts[0]) == 0 || len(parts[1]) < 2 {
		return ""
	}
	// Second segment must start with a valid session type
	switch parts[1][0] {
	case 'c', 'i':
		// valid
	default:
		return ""
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	prefix := partialKey + "/"
	var key string
	err := idx.db.QueryRow(
		`SELECT session_key FROM session_index
		 WHERE session_key LIKE ? AND status = 'active'
		 ORDER BY unixepoch(last_activity_at) DESC, unixepoch(created_at) DESC
		 LIMIT 1`,
		prefix+"%",
	).Scan(&key)
	if err != nil {
		return ""
	}
	return key
}

// ResolveLooseKey resolves a "loose" target — either a bare agent name
// ("scout") or a partial key ("scout/c5970082313") — to a full active session
// key. Bare names dispatch to DefaultSessionKeyForAgent (default-chat aware,
// excludes branch/child sessions); partial keys dispatch to ResolvePartialKey
// (most-recent active session within that chat). Full keys (3+ segments)
// return "" — callers that accept full keys should ParseSessionKey them first.
// Returns "" when nothing resolves.
func (idx *SessionIndex) ResolveLooseKey(key string) string {
	switch strings.Count(key, "/") {
	case 0:
		return idx.DefaultSessionKeyForAgent(key)
	case 1:
		return idx.ResolvePartialKey(key)
	default:
		return ""
	}
}
