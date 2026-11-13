package session

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"foci/internal/sqlite"
	"foci/internal/timeutil"
)

// SessionType classifies the purpose of a session. It is stamped once at
// session creation (derived from the key for roots, from the branch_type for
// branches — see ClassifySessionKey / SessionTypeForBranch) and preserved
// thereafter: a per-turn write never re-derives it, so an in-place memory pass
// running on a chat can never restamp the chat as "reflection".
type SessionType string

const (
	// Conversational — reflect-eligible (see reflectableTypes).
	SessionTypeChat        SessionType = "chat"        // human chat, starts with no history (c-root)
	SessionTypeFacet       SessionType = "facet"       // human chat forked from another chat/facet's history
	SessionTypeIndependent SessionType = "independent" // user-initiated headless work, no presentation layer (i-root)

	// Ephemeral agent work — not reflected.
	SessionTypeSpawn SessionType = "spawn" // agent-initiated, single task, returns to the agent
	// SessionTypeDepth is one node of a depth-mode fan-out (#2588): a forked
	// branch whose chat surfaces inside the root's app conversation. The user
	// talks to it, but it is an event inside another session, not a
	// conversation of its own: never reflected, and swept like a spawn.
	SessionTypeDepth SessionType = "depth"

	// System — not reflected.
	SessionTypeReflection     SessionType = "reflection"      // memory-formation pass (reflection/consolidation/compaction/session-end); must not self-induce
	SessionTypeKeepalive      SessionType = "keepalive"       // cache-warming periodic turn
	SessionTypeBackgroundTask SessionType = "background-task" // foci-internal task (nudge-extraction, background, /branch endpoint)

	SessionTypeUnknown SessionType = "unknown" // legacy / unclassifiable
)

// reflectableTypes is the allowlist of session types eligible for reflection /
// memory formation: genuine conversations (human- or self-driven) that
// accumulate context worth summarizing. Everything else — spawns, memory
// passes, keepalive, background tasks — is excluded. Allowlist not denylist, so
// a NEW system type defaults to not-reflected; a new conversational type must
// be added here deliberately (guarded by TestSessionTypesCategorised).
var reflectableTypes = map[SessionType]bool{
	SessionTypeChat:        true,
	SessionTypeFacet:       true,
	SessionTypeIndependent: true,
}

// IsReflectable reports whether sessions of this type are eligible for
// reflection / memory formation. It is the single source of truth for the
// reflection predicates (see reflectableTypesSQL).
func (t SessionType) IsReflectable() bool { return reflectableTypes[t] }

// IsUserFacing reports whether a human is (or was) the interlocutor — chats,
// facets, and independent (user-initiated headless) sessions. Drives the
// /sessions recency sort: user-facing sessions rank by human activity, others
// by any-turn activity.
func (t SessionType) IsUserFacing() bool {
	return t == SessionTypeChat || t == SessionTypeFacet || t == SessionTypeIndependent
}

// IsOneshot reports whether this session type runs a single headless turn and
// then terminates (reflection/consolidation, batch runs, keepalive, background-
// task, spawn) — as opposed to a persistent interactive session (chat/facet/
// independent). A oneshot cannot meaningfully RECEIVE an async reply: the relay
// would spin up a fresh, unusable turn whose output has nowhere sane to go
// (#1429/#1430). 'unknown' (legacy) is deliberately NOT oneshot.
func (t SessionType) IsOneshot() bool {
	return t == SessionTypeReflection || t == SessionTypeKeepalive ||
		t == SessionTypeBackgroundTask || t == SessionTypeSpawn
}

// barredSenderTypes is the set of session types that must never message
// ANOTHER session via send_to_session (#1409). A reflection/consolidation/
// compaction pass (SessionTypeReflection) or a keepalive cache-warm turn
// (SessionTypeKeepalive) runs headless, with no human waiting and no
// perspective its parent/peers don't already have — pushing an unsolicited
// status update into another session (typically the parent) only confuses
// the recipient: a reflection branch once injected an in-flight-bisect
// status handoff into the main clutch session, which then mischaracterized
// it as routing to a different agent. If such a branch has state worth
// preserving, it belongs in a handoff FILE on disk, not a cross-session
// message.
//
// Deliberately narrower than IsOneshot(): SessionTypeSpawn and
// SessionTypeBackgroundTask are also oneshot (can't meaningfully receive an
// async reply, see IsOneshot) but are NOT barred from sending — a spawn's
// whole purpose can be to report a result back, and background-task covers
// /branch and nudge-extraction work that may legitimately need to notify
// another session. Only reflection/keepalive are barred outright.
var barredSenderTypes = map[SessionType]bool{
	SessionTypeReflection: true,
	SessionTypeKeepalive:  true,
}

// IsBarredFromSessionSend reports whether this session type must not send
// messages to other sessions via send_to_session (#1409). See
// barredSenderTypes for which types and why.
func (t SessionType) IsBarredFromSessionSend() bool { return barredSenderTypes[t] }

// reflectableTypesSQL renders reflectableTypes as a SQL-quoted, comma-separated
// list for IN clauses, sorted for stable output. Built from the trusted
// reflectableTypes constant (never user input), so string interpolation into
// SQL is safe. Single source of truth shared by the reflection predicates.
func reflectableTypesSQL() string {
	vals := make([]string, 0, len(reflectableTypes))
	for t := range reflectableTypes {
		vals = append(vals, "'"+string(t)+"'")
	}
	sort.Strings(vals)
	return strings.Join(vals, ", ")
}

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
	SessionKey         string
	FilePath           string
	CreatedAt          time.Time
	LastActivityAt     time.Time // updated on every session append; zero if never set
	LastUserActivityAt time.Time // last human interaction (interactive only); zero if never
	ParentSessionKey   string    // empty if root session
	SessionType        SessionType
	Status             SessionStatus
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
			last_cache_touch   TEXT,
			last_user_activity_at TEXT,
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

	// Remap session_type from the old coarse vocabulary to the unified
	// taxonomy (chat/facet/independent/spawn/reflection/keepalive/
	// background-task/unknown). Idempotent.
	migrateSessionTypeTaxonomy(db)

	// Backfill any null last_reflection to now so a fresh install doesn't
	// stampede all sessions into reflection on first run.
	_, _ = db.Exec(`UPDATE session_index SET last_reflection = ? WHERE last_reflection IS NULL`,
		timeutil.Format(timeutil.Now()))

	// Expression index for correct cross-timezone last_activity_at ordering.
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_session_activity_unix ON session_index(unixepoch(last_activity_at))`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_session_agent ON session_index(agent_id, is_root, status)`)
	// ArchiveSweep asks, per candidate, whether any active row names it as
	// parent (HasActiveChild, #3079).
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_session_parent ON session_index(parent_session_key, status)`)

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
		sessionLog(e.SessionKey).Errorf("upsert index entry %q: %v", e.SessionKey, err)
	}
}

// RecordTurnReceipt performs a turn's per-turn timestamp write in one
// statement: it upserts the session row (creating it if missing) and sets,
// atomically,
//   - last_activity_at   → only when bumpActivity (false for memory-formation
//     and keepalive turns, which must not defeat the reflection-skip guard);
//     kept monotonic,
//   - last_user_activity_at → only when bumpUser (interactive human turns —
//     a platform/voice trigger, or an HTTP request declared human).
//
// It never writes last_cache_touch (NULL on insert, kept on conflict): that is
// stamped separately (TouchCacheTouch) only from a model response's cache
// usage, so a turn that never got one claims no cache warmth (#2446, #2509).
// It records that a turn's input ARRIVED, and per mid-turn round that the
// turn is progressing. e.CreatedAt carries the turn's `now`. On CONFLICT the
// IDENTITY columns (created_at, parent, type, agent, chat, is_root) are
// PRESERVED, not overwritten — so created_at stays the true creation time and
// a branch's parent is never nulled by a turn. last_reflection is seeded on
// INSERT only (#945).
func (idx *SessionIndex) RecordTurnReceipt(e SessionIndexEntry, bumpActivity, bumpUser bool) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	nowStr := timeutil.Format(e.CreatedAt)
	agentID, chatID, isRoot := keyColumns(e.SessionKey)
	var userInsert interface{} // NULL on insert unless this is an interactive turn
	if bumpUser {
		userInsert = nowStr
	}
	_, err := idx.db.Exec(
		`INSERT INTO session_index
		   (session_key, file_path, created_at, last_activity_at, last_user_activity_at, last_reflection, parent_session_key, session_type, status, agent_id, chat_id, is_root)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(session_key) DO UPDATE SET
		   file_path = excluded.file_path,
		   status = excluded.status,
		   last_activity_at = CASE
		     WHEN ? AND unixepoch(excluded.last_activity_at) > unixepoch(session_index.last_activity_at) THEN excluded.last_activity_at
		     ELSE session_index.last_activity_at
		   END,
		   last_user_activity_at = CASE
		     WHEN ? THEN excluded.last_user_activity_at
		     ELSE session_index.last_user_activity_at
		   END`,
		e.SessionKey, e.FilePath, nowStr, nowStr, userInsert, nowStr,
		nullableString(e.ParentSessionKey), e.SessionType, e.Status, agentID, chatID, isRoot,
		bumpActivity, bumpUser,
	)
	if err != nil {
		sessionLog(e.SessionKey).Errorf("record turn activity %q: %v", e.SessionKey, err)
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
		sessionLog(sessionKey).Warnf("update activity for %q: %v", sessionKey, err)
	}
}

// TouchCacheTouch records that the session's cached context was hit by a turn
// at time `at`. Unlike last_activity_at this is memory-INCLUDED and bumped by
// branch turns against their root: it answers "was this session's prompt cache
// warmed recently?" for the --if-active / --if-inactive send gate, where a
// reflection/branch turn genuinely refreshes the shared cache and so counts.
// No-op if the row doesn't exist yet (UPDATE affects 0 rows) — the gate treats
// an absent value as inactive, same as before.
func (idx *SessionIndex) TouchCacheTouch(sessionKey string, at time.Time) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	_, err := idx.db.Exec(
		`UPDATE session_index SET last_cache_touch = ? WHERE session_key = ?`,
		timeutil.Format(at),
		sessionKey,
	)
	if err != nil {
		sessionLog(sessionKey).Warnf("touch cache for %q: %v", sessionKey, err)
	}
}

// ClearCacheTouch nulls a session's last_cache_touch, marking "no live cache".
// Called on reset: the backend is gone and the prompt cache with it, so keepalive
// must not try to warm a corpse until a real turn re-establishes one (LastCacheTouch
// then reports absent → keepalive skips the session). No-op if the row is absent.
func (idx *SessionIndex) ClearCacheTouch(sessionKey string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	_, err := idx.db.Exec(
		`UPDATE session_index SET last_cache_touch = NULL WHERE session_key = ?`,
		sessionKey,
	)
	if err != nil {
		sessionLog(sessionKey).Warnf("clear cache touch for %q: %v", sessionKey, err)
	}
}

// lastSessionTimestamp reads one RFC3339 timestamp column for a single session
// and reports whether a usable value is present. `column` is an internal
// constant identifier (never user input), so interpolating it is safe.
func (idx *SessionIndex) lastSessionTimestamp(sessionKey, column string) (time.Time, bool) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var raw sql.NullString
	err := idx.db.QueryRow(
		"SELECT "+column+" FROM session_index WHERE session_key = ?", //nolint:gosec // column is a fixed internal identifier
		sessionKey,
	).Scan(&raw)
	if err != nil || !raw.Valid || raw.String == "" {
		return time.Time{}, false
	}
	t, perr := time.Parse(time.RFC3339, raw.String)
	if perr != nil {
		return time.Time{}, false
	}
	return t, true
}

// LastCacheTouch returns the most recent cache-touch time for a session and
// whether one is recorded. Backs the --if-warm / --if-cold send gate.
func (idx *SessionIndex) LastCacheTouch(sessionKey string) (time.Time, bool) {
	return idx.lastSessionTimestamp(sessionKey, "last_cache_touch")
}

// LastUserActivity returns the most recent human-interaction time for a SINGLE
// session (not the agent-wide max — that is LastUserActivityForAgent). Backs the
// --if-user-active / --if-user-inactive send gate, which scopes to the resolved
// target session: "did a human touch THIS session recently", not "anywhere on
// this agent".
func (idx *SessionIndex) LastUserActivity(sessionKey string) (time.Time, bool) {
	return idx.lastSessionTimestamp(sessionKey, "last_user_activity_at")
}

// TouchUserActivity records that a human interacted with this session at time
// `at`. Written on user-triggered turns (telegram/app/discord/voice — or an
// HTTP request declared human), so unlike last_cache_touch it excludes
// cron/keepalive/agent/memory turns — it is the clean "a human spoke
// recently" signal. No-op if the row doesn't exist.
func (idx *SessionIndex) TouchUserActivity(sessionKey string, at time.Time) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	_, err := idx.db.Exec(
		`UPDATE session_index SET last_user_activity_at = ? WHERE session_key = ?`,
		timeutil.Format(at),
		sessionKey,
	)
	if err != nil {
		sessionLog(sessionKey).Warnf("touch user activity for %q: %v", sessionKey, err)
	}
}

// LastUserActivityForAgent returns the most recent human-interaction time across
// all of an agent's sessions (a derived max over last_user_activity_at), and
// whether any is recorded. Backs the --if-user-active gate and the warning
// dispatcher's active/inactive cadence choice. Agent-scoped by design: "has a
// human interacted with this agent recently?" regardless of which session.
func (idx *SessionIndex) LastUserActivityForAgent(agentID string) (time.Time, bool) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// max over unixepoch, not the raw text: RFC3339 strings with different
	// timezone offsets (GMT/BST across a DST boundary) don't sort lexically.
	var epoch sql.NullInt64
	err := idx.db.QueryRow(
		`SELECT max(unixepoch(last_user_activity_at)) FROM session_index
		 WHERE agent_id = ? AND last_user_activity_at IS NOT NULL`,
		agentID,
	).Scan(&epoch)
	if err != nil || !epoch.Valid {
		return time.Time{}, false
	}
	return time.Unix(epoch.Int64, 0), true
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
		sessionLog(sessionKey).Warnf("stamp reflection for %q: %v", sessionKey, err)
	}
}

// LastReflection returns when a reflection pass was last dispatched for the
// session (StampReflection), and false when none was or the session is
// unknown. The value an INSERT seeds at the session's birth (last_reflection
// = its first activity, see upsertLocked) is not a reflection: a stamp no
// later than created_at reads as none.
func (idx *SessionIndex) LastReflection(sessionKey string) (time.Time, bool) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var epoch sql.NullInt64
	err := idx.db.QueryRow(
		`SELECT unixepoch(last_reflection) FROM session_index
		 WHERE session_key = ? AND unixepoch(last_reflection) > unixepoch(created_at)`,
		sessionKey,
	).Scan(&epoch)
	if err != nil || !epoch.Valid {
		return time.Time{}, false
	}
	return time.Unix(epoch.Int64, 0), true
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

// SessionsNeedingReflection returns active reflect-eligible session keys for an
// agent where activity has occurred since the last reflection pass (or it has
// never run). Reflect-eligible = the conversational types (chat/facet/
// independent); see reflectableTypes.
func (idx *SessionIndex) SessionsNeedingReflection(agentID string) ([]string, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	return idx.querySessionKeysLocked(
		`SELECT session_key FROM session_index
		 WHERE agent_id = ? AND session_type IN (`+reflectableTypesSQL()+`) AND status = 'active'
		   AND (last_reflection IS NULL OR unixepoch(last_activity_at) > unixepoch(last_reflection))`,
		agentID,
	)
}

// EphemeralCleanupTypes are the session_type values the daily GC reclaims:
// pure system / ephemeral agent work whose backend transcripts are throwaway.
// Conversational types (chat/facet/independent) and legacy 'unknown' are never
// touched, regardless of age.
var EphemeralCleanupTypes = []SessionType{
	SessionTypeReflection,
	SessionTypeKeepalive,
	SessionTypeBackgroundTask,
	SessionTypeSpawn,
	SessionTypeDepth,
}

// EphemeralSessionsOlderThan returns the keys of ephemeral system/agent sessions
// (EphemeralCleanupTypes) for agentID whose last activity — or creation, if
// never active — predates cutoff. Used by the daily cleanup to find stale
// throwaway sessions whose backend transcripts can be reclaimed. Gating on
// session_type (not is_root) keeps user conversations and their branches safe.
// EphemeralSessionsOlderThan returns the ephemeral sessions past the cutoff
// that still have at least one UNSWEPT transcript.
//
// #1801: age alone is not enough. session_index rows are kept forever as a
// historical record, so selecting purely on age re-selected the same expired
// sessions every night for the life of the install. The EXISTS clause also
// excludes sessions that never recorded a transcript at all, which in the live
// DB is most of them (backend_resume_history covers ~3,632 sessions against
// 6,726 index rows) — they contributed nothing but kept the candidate list
// non-empty, which is what made an otherwise-idle agent open a backend cleanup
// scope, and for codex and opencode that scope spawns an app-server.
func (idx *SessionIndex) EphemeralSessionsOlderThan(agentID string, cutoff time.Time) ([]string, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	args := []interface{}{agentID}
	placeholders := make([]string, len(EphemeralCleanupTypes))
	for i, t := range EphemeralCleanupTypes {
		placeholders[i] = "?"
		args = append(args, string(t))
	}
	args = append(args, timeutil.Format(cutoff))

	return idx.querySessionKeysLocked(
		`SELECT session_key FROM session_index
		 WHERE agent_id = ?
		   AND session_type IN (`+strings.Join(placeholders, ",")+`)
		   AND unixepoch(COALESCE(NULLIF(last_activity_at, ''), created_at)) < unixepoch(?)
		   AND EXISTS (SELECT 1 FROM backend_resume_history h
		                WHERE h.session_key = session_index.session_key
		                  AND h.swept_at IS NULL)`,
		args...)
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

// SessionNeedsReflection reports whether a single reflect-eligible session is
// due for reflection by the same rule as SessionsNeedingReflection — it has had
// activity since its last reflection (or never reflected). Unlike the bulk query
// it does NOT filter on status='active': its caller (the final reflection fired
// when an app session is archived) invokes it as the session transitions out of
// active, and the "new activity since last reflection" gate is the real
// condition. It still gates on reflect-eligible type (chat/facet/independent).
func (idx *SessionIndex) SessionNeedsReflection(sessionKey string) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var due int
	err := idx.db.QueryRow(
		`SELECT 1 FROM session_index
		 WHERE session_key = ? AND session_type IN (`+reflectableTypesSQL()+`)
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

// escapeLike escapes LIKE wildcards so a session key is matched literally.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// IndexRow returns every column of the session_index row for sessionKey as an
// ordered (column, value) pair list, reading the schema dynamically so new
// columns surface without code changes. NULLs render as "null". found is false
// (with nil error) when no row exists.
func (idx *SessionIndex) IndexRow(sessionKey string) (columns, values []string, found bool, err error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	rows, err := idx.db.Query(`SELECT * FROM session_index WHERE session_key = ?`, sessionKey)
	if err != nil {
		return nil, nil, false, err
	}
	defer rows.Close() //nolint:errcheck

	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, false, err
	}
	if !rows.Next() {
		return nil, nil, false, rows.Err()
	}
	raw := make([]sql.NullString, len(cols))
	scan := make([]interface{}, len(cols))
	for i := range raw {
		scan[i] = &raw[i]
	}
	if err := rows.Scan(scan...); err != nil {
		return nil, nil, false, err
	}
	vals := make([]string, len(cols))
	for i, v := range raw {
		if v.Valid {
			vals[i] = v.String
		} else {
			vals[i] = "null"
		}
	}
	return cols, vals, true, nil
}

// QueryOptions configures session index queries.
type QueryOptions struct {
	AgentID     string        // filter by agent ID (empty = all)
	SessionType string        // filter by type (empty = all)
	Status      string        // filter by status (empty = all)
	MaxAge      time.Duration // only sessions with activity within this duration (0 = no limit)
	Limit       int           // max results (0 = unlimited)
	// RootKey scopes the query to a session "family": the root key itself and
	// every branch beneath it (keys equal to RootKey or prefixed by RootKey+"/").
	// Empty = no family scoping. Callers pass a ROOT key (see SessionKey.Root);
	// a branch key would match only its own descendants, not its siblings.
	RootKey string
}

// Query retrieves session index entries matching the given options.
func (idx *SessionIndex) Query(opts QueryOptions) ([]SessionIndexEntry, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	query := `SELECT session_key, file_path, created_at, last_activity_at, last_user_activity_at, parent_session_key, session_type, status
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
	if opts.RootKey != "" {
		query += ` AND (session_key = ? OR session_key LIKE ? ESCAPE '\')`
		args = append(args, opts.RootKey, escapeLike(opts.RootKey)+`/%`)
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
		var userActivityStr, parentKey sql.NullString

		if err := rows.Scan(&e.SessionKey, &e.FilePath, &createdStr, &activityStr, &userActivityStr, &parentKey, &e.SessionType, &e.Status); err != nil {
			return nil, err
		}
		e.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
		e.LastActivityAt, _ = time.Parse(time.RFC3339, activityStr)
		if userActivityStr.Valid {
			e.LastUserActivityAt, _ = time.Parse(time.RFC3339, userActivityStr.String)
		}
		if parentKey.Valid {
			e.ParentSessionKey = parentKey.String
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// Delete removes a session from the index — and cascades to its metadata and
// resume-history via purgeLocked, so a deleted session leaves no orphan rows.
func (idx *SessionIndex) Delete(sessionKey string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.purgeLocked(sessionKey)
}

// PurgeSession removes every trace of a session key from the mutable session
// tables (index row, all session_metadata, backend_resume_history) in one locked
// pass — the cascade a bare `DELETE FROM session_index` lacks. Deleting an
// index row alone stranded its metadata + resume-history (the orphan rows that
// accumulated behind reinstalled/pruned app chats, surfaced 2026-07-11).
// session_archives is deliberately NOT purged: it is a durable historical
// record with its own retention lifecycle.
func (idx *SessionIndex) PurgeSession(sessionKey string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.purgeLocked(sessionKey)
}

// purgeLocked is the cascade body; the caller MUST hold idx.mu. Shared by
// Delete, PurgeSession, and the PruneOrphans loop (which already holds the
// lock). Each delete is independently idempotent; all writes to the index
// serialise on idx.mu, so the sequence is atomic w.r.t. other index ops.
func (idx *SessionIndex) purgeLocked(sessionKey string) {
	for _, stmt := range []string{
		`DELETE FROM session_index     WHERE session_key = ?`,
		`DELETE FROM session_metadata  WHERE session_key = ?`,
		`DELETE FROM backend_resume_history WHERE session_key = ?`,
	} {
		if _, err := idx.db.Exec(stmt, sessionKey); err != nil {
			sessionLog(sessionKey).Errorf("purge session %q: %v", sessionKey, err)
		}
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
		sessionLog(sessionKey).Warnf("update status for %q: %v", sessionKey, err)
	}
}

// TouchActivity updates the last_activity_at timestamp to now.
func (idx *SessionIndex) TouchActivity(sessionKey string) {
	idx.UpdateActivity(sessionKey, time.Now())
}

// RebuildIndex clears the rows the scan owns — those whose file lives under
// storeDir — and repopulates them from entries (the scan of storeDir).
// Preserves last_reflection timestamps across the rebuild.
// Wrapped in a single transaction for performance (~3000x fewer fsyncs).
func (idx *SessionIndex) RebuildIndex(storeDir string, entries []SessionIndexEntry) (int, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Preserve the activity timestamps before clearing — the rebuild can't
	// reconstruct any of these from disk (the scan only re-derives
	// last_activity_at from file modtime). Losing last_reflection resets
	// reflection scheduling; losing last_user_activity_at resets the
	// default-session routing order; losing last_cache_touch resets the
	// --if-active gate and cache-bust idle baseline. File-backed rows are
	// DELETEd and re-INSERTed below (which would null these columns), so
	// snapshot and restore them for every row that has any set.
	type preservedStamps struct{ reflection, userActivity, cacheTouch sql.NullString }
	saved := make(map[string]preservedStamps)
	rows, err := idx.db.Query(
		`SELECT session_key, last_reflection, last_user_activity_at, last_cache_touch
		   FROM session_index
		  WHERE last_reflection IS NOT NULL
		     OR last_user_activity_at IS NOT NULL
		     OR last_cache_touch IS NOT NULL`)
	if err != nil {
		// Fail closed: if we can't read the timestamps we're about to clear,
		// don't proceed to DELETE — that would silently wipe every session's
		// unreconstructable activity timestamps. Abort and keep the existing
		// index intact for this cycle.
		return 0, fmt.Errorf("preserve activity timestamps: %w", err)
	}
	for rows.Next() {
		var key string
		var p preservedStamps
		if err := rows.Scan(&key, &p.reflection, &p.userActivity, &p.cacheTouch); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("preserve activity timestamps (scan): %w", err)
		}
		saved[key] = p
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("preserve activity timestamps (rows): %w", err)
	}
	_ = rows.Close()

	tx, err := idx.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after commit

	// Clear only rows whose file lives under the store — the scan below
	// re-derives exactly those from disk. Every other row is a BACKEND
	// session (a delegated agent whose conversation lives in the backend's
	// own store) that the scan can never reproduce:
	//   - empty file_path (same rule as PruneOrphans): deleting these wiped
	//     last_activity_at, which the default-chat routing tiebreak orders
	//     by, so a rebuild right after restart picked an arbitrary default
	//     chat (the discord-misroute bug);
	//   - file_path outside the store — CC turns record the backend
	//     transcript (~/.claude/projects/…). Deleting these dropped every CC
	//     root row on a crash/reboot restart, keeping its session_metadata
	//     (the delete does not cascade), and the missing last_cache_touch
	//     then showed a cold chat as warm (#2061).
	// A row whose store file has vanished is still dropped here; one whose
	// backend transcript vanished is PruneOrphans' job. An empty storeDir
	// owns nothing, so nothing is cleared.
	if storeDir != "" {
		prefix := filepath.Clean(storeDir) + string(filepath.Separator)
		// substr, not LIKE: '_' and '%' are ordinary path characters.
		if _, err := tx.Exec(
			`DELETE FROM session_index WHERE substr(file_path, 1, length(?)) = ?`,
			prefix, prefix); err != nil {
			return 0, fmt.Errorf("clear index: %w", err)
		}
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
			sessLog.Errorf("insert index entry %q: %v", e.SessionKey, err)
		}
		count++
	}

	// Restore preserved activity timestamps. COALESCE keeps a re-derived value
	// if the saved one is NULL (only overwrites when we actually snapshotted a
	// non-NULL stamp), so a column set by the re-INSERT isn't clobbered to NULL.
	if len(saved) > 0 {
		updateStmt, err := tx.Prepare(
			`UPDATE session_index
			    SET last_reflection        = COALESCE(?, last_reflection),
			        last_user_activity_at  = COALESCE(?, last_user_activity_at),
			        last_cache_touch       = COALESCE(?, last_cache_touch)
			  WHERE session_key = ?`)
		if err == nil {
			defer func() { _ = updateStmt.Close() }()
			for key, p := range saved {
				_, _ = updateStmt.Exec(p.reflection, p.userActivity, p.cacheTouch, key)
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
	return idx.RebuildIndex(store.dir, entries)
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
		// Cascade: an orphaned session's metadata + resume-history must go with
		// its index row, else they strand (the accumulation this fixes). We hold
		// idx.mu, so call the lock-free body directly.
		idx.purgeLocked(key)
		sessLog.Infof("pruned orphan index entry: %s", key)
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

// ChatMetaNeverDefault is the chat_metadata key (value "true") marking a chat
// that must never be resolved as an agent's default session. Written by
// transports whose conversations belong to other people (a colleague's Slack
// DM or channel thread, #2459) at first contact; consulted by every rung of
// DefaultSessionKeyForAgentOn, so agent-wide traffic (startup notices, cron,
// keepalive, approval prompts) never lands in such a chat however recently it
// was active.
const ChatMetaNeverDefault = "never_default"

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
//
// '/' and ':' are allowed ("OCN: topic", #2157). An alias cannot shadow a
// session key: every resolver (route.Resolver, send_to_session) tries the
// exact key and the named session before the alias, so a key always beats an
// alias that spells it. ':' is not part of the key grammar at all.
func (idx *SessionIndex) SetChatAliasUnique(agentID, platform string, chatID int64, alias string) error {
	trimmed := strings.TrimSpace(alias)
	if trimmed == "" {
		return idx.SetChatMetadata(agentID, platform, chatID, "alias", "")
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

// ErrAliasChanged is returned by SetChatAliasIfUnchanged when the chat's
// alias is no longer the one the caller expected.
var ErrAliasChanged = errors.New("chat alias changed")

// SetChatAliasIfUnchanged sets chatID's alias to alias, and each of meta's
// keys to its value, in one transaction, but only while the stored alias
// (trimmed) still equals expect: a compare-and-set, so a name another writer
// (the user's rename, an agent's set_session_alias) stored after the caller
// read it is never overwritten. It returns ErrAliasChanged when the alias
// moved and ErrAliasTaken when another chat of the agent on the platform
// holds alias (case-insensitive), like SetChatAliasUnique. Used by the app's
// automatic chat name (#2962).
func (idx *SessionIndex) SetChatAliasIfUnchanged(agentID, platform string, chatID int64, expect, alias string, meta map[string]string) error {
	trimmed := strings.TrimSpace(alias)
	if trimmed == "" {
		return errors.New("empty alias")
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var current string
	err := idx.db.QueryRow(chatMetaTable.selectSQL, agentID, platform, chatID, "alias").Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if strings.TrimSpace(current) != strings.TrimSpace(expect) {
		return ErrAliasChanged
	}
	holders, err := idx.chatsHoldingAliasLocked(agentID, platform, aliasNorm(trimmed), chatID)
	if err != nil {
		return err
	}
	if len(holders) > 0 {
		return ErrAliasTaken
	}
	tx, err := idx.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(chatMetaTable.upsertSQL, agentID, platform, chatID, "alias", trimmed); err != nil {
		return err
	}
	for k, v := range meta {
		if _, err := tx.Exec(chatMetaTable.upsertSQL, agentID, platform, chatID, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ChatsHoldingAlias returns the chats, other than exceptChatID, whose alias
// matches alias (case-insensitive, trimmed) under the same agent and platform,
// each mapped to the alias as that chat stores it.
// It is the read half of a claim (#2709): the caller derives each holder's
// displaced name from state the index does not own (last activity), then
// passes those names to ClaimChatAlias.
func (idx *SessionIndex) ChatsHoldingAlias(agentID, platform, alias string, exceptChatID int64) (map[int64]string, error) {
	norm := aliasNorm(alias)
	if norm == "" {
		return nil, nil
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.chatsHoldingAliasLocked(agentID, platform, norm, exceptChatID)
}

func (idx *SessionIndex) chatsHoldingAliasLocked(agentID, platform, norm string, exceptChatID int64) (map[int64]string, error) {
	rows, err := idx.db.Query(
		`SELECT chat_id, value FROM chat_metadata WHERE agent_id = ? AND platform = ? AND key = 'alias'
		   AND lower(trim(value)) = ? AND chat_id != ? ORDER BY chat_id`,
		agentID, platform, norm, exceptChatID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	out := make(map[int64]string)
	for rows.Next() {
		var id int64
		var v string
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = strings.TrimSpace(v)
	}
	return out, rows.Err()
}

// ClaimChatAlias gives alias to chatID and renames every other chat that holds
// it (#2709). displaced maps each holder to the base of its new alias (the
// caller builds "<alias> <last activity>"). A base that is itself taken gets a
// " (2)", " (3)", … suffix, so the claim never creates a new collision.
//
// The holder set is re-read under the index mutex. A holder missing from
// displaced (it took the alias after the caller's ChatsHoldingAlias) fails the
// claim with ErrAliasTaken and writes nothing. All writes are one transaction.
// Returns the alias each displaced chat now has.
func (idx *SessionIndex) ClaimChatAlias(agentID, platform string, chatID int64, alias string, displaced map[int64]string) (map[int64]string, error) {
	trimmed := strings.TrimSpace(alias)
	if trimmed == "" {
		return nil, idx.SetChatMetadata(agentID, platform, chatID, "alias", "")
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	holders, err := idx.chatsHoldingAliasLocked(agentID, platform, aliasNorm(trimmed), chatID)
	if err != nil {
		return nil, err
	}
	taken, err := idx.aliasesLocked(agentID, platform)
	if err != nil {
		return nil, err
	}
	// The claimed alias leaves its holders and goes to chatID.
	delete(taken, aliasNorm(trimmed))
	renamed := make(map[int64]string, len(holders))
	for h := range holders {
		base, ok := displaced[h]
		if !ok || strings.TrimSpace(base) == "" {
			return nil, ErrAliasTaken
		}
		name := uniqueAlias(strings.TrimSpace(base), trimmed, taken)
		taken[aliasNorm(name)] = true
		renamed[h] = name
	}
	tx, err := idx.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	for h, name := range renamed {
		if _, err := tx.Exec(chatMetaTable.upsertSQL, agentID, platform, h, "alias", name); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(chatMetaTable.upsertSQL, agentID, platform, chatID, "alias", trimmed); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return renamed, nil
}

// aliasesLocked returns the normalised set of every alias held under the agent
// and platform.
func (idx *SessionIndex) aliasesLocked(agentID, platform string) (map[string]bool, error) {
	rows, err := idx.db.Query(
		`SELECT value FROM chat_metadata WHERE agent_id = ? AND platform = ? AND key = 'alias' AND trim(value) != ''`,
		agentID, platform,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	out := make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[aliasNorm(v)] = true
	}
	return out, rows.Err()
}

// uniqueAlias returns base, or base with the first " (n)" suffix (n ≥ 2) that
// is neither in taken nor equal to claimed.
func uniqueAlias(base, claimed string, taken map[string]bool) string {
	free := func(s string) bool { return !taken[aliasNorm(s)] && aliasNorm(s) != aliasNorm(claimed) }
	if free(base) {
		return base
	}
	for n := 2; ; n++ {
		if s := fmt.Sprintf("%s (%d)", base, n); free(s) {
			return s
		}
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

// AgentSessionKeysWithMetadata returns every distinct session key (root AND
// branch) belonging to agentID that has at least one persisted
// session_metadata row — e.g. a /model, /effort, or /thinking override.
// Used at startup to restore overrides regardless of transport: unlike
// Telegram's fixed default chat, app-transport connections have no
// connection-level "default session" to enumerate (appConn.bound is only
// set per-request), so driving restoration off connections silently skips
// every app-based agent. Querying persisted metadata directly is
// transport-agnostic and also covers branch-specific overrides that a
// connection-default-only walk would miss.
func (idx *SessionIndex) AgentSessionKeysWithMetadata(agentID string) ([]string, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	return idx.querySessionKeysLocked(
		`SELECT DISTINCT session_key FROM session_metadata WHERE session_key = ? OR session_key LIKE ? ESCAPE '\'`,
		agentID, escapeLike(agentID)+`/%`,
	)
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

// SwapSystemState sets key to newValue only if its current value is oldValue,
// and reports whether it did. An oldValue of "" means "only if the key is
// absent". The check and the write are one statement under idx.mu, so of
// several concurrent callers with the same oldValue exactly one wins.
func (idx *SessionIndex) SwapSystemState(key, oldValue, newValue string) (bool, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var (
		res sql.Result
		err error
	)
	if oldValue == "" {
		res, err = idx.db.Exec(`INSERT INTO system_state (key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`, key, newValue)
	} else {
		res, err = idx.db.Exec(`UPDATE system_state SET value = ? WHERE key = ? AND value = ?`, newValue, key, oldValue)
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// SetDefaultChat marks a chat as the default for an agent on a specific platform.
// Clears any previous default for that agent on the same platform, then sets
// the new one via an is_default=true row in chat_metadata.
// Each platform maintains its own independent default.
//
// A chat carrying the never_default flag (a conversation that belongs to
// someone else, e.g. a colleague's Slack DM or channel thread, #2459) is
// refused: agent-wide traffic (startup notices, cron, keepalive, approval
// prompts) must never land in it, so it cannot be pinned as a default.
func (idx *SessionIndex) SetDefaultChat(agentID, platform string, chatID int64) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var neverDefault int
	err := idx.db.QueryRow(
		`SELECT 1 FROM chat_metadata WHERE agent_id = ? AND chat_id = ? AND key = '`+ChatMetaNeverDefault+`' AND value = 'true' LIMIT 1`,
		agentID, chatID,
	).Scan(&neverDefault)
	if err == nil {
		return fmt.Errorf("chat %d is never a default chat (a conversation that must not receive agent-wide traffic, e.g. a Slack conversation); it cannot be pinned", chatID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// Clear any existing default for this agent on this platform.
	if _, err := idx.db.Exec(
		`DELETE FROM chat_metadata WHERE agent_id = ? AND platform = ? AND key = 'is_default'`,
		agentID, platform,
	); err != nil {
		return err
	}

	// Set new default.
	_, err = idx.db.Exec(
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
	// Exclude an archived chat: a default that was later archived must never be
	// resolved (archived = hidden from the client, so delivery there is silently
	// invisible). Mirrors the is_archived guard on the DefaultSessionKeyForAgentOn
	// ladder.
	err := idx.db.QueryRow(
		`SELECT chat_id FROM chat_metadata cm WHERE cm.agent_id = ? AND cm.platform = ? AND cm.key = 'is_default' AND cm.value = 'true'
		   AND NOT EXISTS (SELECT 1 FROM chat_metadata arch
		     WHERE arch.agent_id = cm.agent_id AND arch.platform = cm.platform
		       AND arch.chat_id = cm.chat_id AND arch.key = 'is_archived' AND arch.value = 'true')`,
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

// ErrSessionArchived is returned for a send that names a session whose app
// conversation is archived (#3024). Archived = hidden from the app, so a turn
// run there would be delivered where nobody sees it.
var ErrSessionArchived = errors.New("session has been archived")

// ArchivedSessionError names the archived session a send was refused for. It
// matches ErrSessionArchived under errors.Is.
type ArchivedSessionError struct{ SessionKey string }

func (e *ArchivedSessionError) Error() string {
	return fmt.Sprintf("session %s has been archived: its conversation is hidden from the user, so nobody would see the reply. The message was not sent. It can be sent once the user unarchives the conversation", e.SessionKey)
}

// Is makes errors.Is(err, ErrSessionArchived) hold.
func (e *ArchivedSessionError) Is(target error) bool { return target == ErrSessionArchived }

// SessionArchived reports whether sessionKey's app conversation is archived
// (#3024): the session has at least one app conversation, and every one of
// them is archived. A conversation counts as archived when it carries the
// is_archived flag itself, or when it is a depth leaf (depth_root row, #2611)
// whose root conversation is archived (#2613 archives leaves with their root).
//
// A session's app conversations are the chats whose session_key row names it
// (an adopted named session, a depth leaf), plus its own chat for a root chat
// key (agent/c<chatID>) when that chat has app rows and no session_key row
// pointing elsewhere. A session with no app conversation (a telegram chat, a
// spawn, a plain branch) is never archived. A query error reads as not
// archived: refusing a send on a database fault would hide the fault behind a
// false reason.
func (idx *SessionIndex) SessionArchived(sessionKey string) bool {
	agentID := AgentIDFromKey(sessionKey)
	if agentID == "" {
		return false
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()

	chats := map[int64]bool{}
	rows, err := idx.db.Query(
		`SELECT DISTINCT chat_id FROM chat_metadata WHERE agent_id = ? AND platform = 'app' AND key = 'session_key' AND value = ?`,
		agentID, sessionKey,
	)
	if err != nil {
		return false
	}
	for rows.Next() {
		var chatID int64
		if err := rows.Scan(&chatID); err != nil {
			_ = rows.Close()
			return false
		}
		chats[chatID] = true
	}
	_ = rows.Close()
	if rows.Err() != nil {
		return false
	}
	// Only a root chat key owns its chat: a branch key (agent/c<id>/b<n>)
	// parses to the same chat id, but its conversation, if any, is its own.
	var own int64
	if sk, err := ParseSessionKey(sessionKey); err == nil && sk.IsRoot() {
		own = sk.ChatID()
	}
	if own != 0 && !chats[own] {
		var appRows, adoptedElsewhere int
		err := idx.db.QueryRow(
			`SELECT COUNT(*), COALESCE(SUM(key = 'session_key' AND value != ?), 0) FROM chat_metadata
			 WHERE agent_id = ? AND platform = 'app' AND chat_id = ?`,
			sessionKey, agentID, own,
		).Scan(&appRows, &adoptedElsewhere)
		if err != nil {
			return false
		}
		if appRows > 0 && adoptedElsewhere == 0 {
			chats[own] = true
		}
	}
	if len(chats) == 0 {
		return false
	}
	for chatID := range chats {
		var archived bool
		err := idx.db.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM chat_metadata
			   WHERE agent_id = ?1 AND platform = 'app' AND chat_id = ?2 AND key = 'is_archived' AND value = 'true')
			 OR EXISTS (SELECT 1 FROM chat_metadata leaf
			   JOIN chat_metadata conv ON conv.agent_id = leaf.agent_id AND conv.platform = 'app'
			     AND conv.key = 'conv_id' AND conv.value = leaf.value
			   JOIN chat_metadata arch ON arch.agent_id = conv.agent_id AND arch.platform = 'app'
			     AND arch.chat_id = conv.chat_id AND arch.key = 'is_archived' AND arch.value = 'true'
			   WHERE leaf.agent_id = ?1 AND leaf.platform = 'app' AND leaf.chat_id = ?2 AND leaf.key = 'depth_root')`,
			agentID, chatID,
		).Scan(&archived)
		if err != nil || !archived {
			return false
		}
	}
	return true
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

	// Every rung excludes archived chats: an archived conversation is hidden from
	// the client, so resolving a default to it makes delivery silently invisible.
	// Correlated on the chat_metadata alias 'cm' present in each rung's query.
	const notArchived = `AND NOT EXISTS (SELECT 1 FROM chat_metadata arch
		 WHERE arch.agent_id = cm.agent_id AND arch.platform = cm.platform
		   AND arch.chat_id = cm.chat_id AND arch.key = 'is_archived' AND arch.value = 'true') `

	// Every rung also excludes never-default chats: conversations on
	// transports whose chats must never become an agent's default (a
	// colleague's Slack DM or channel thread, #2459) carry a never_default
	// chat_metadata row, and agent-wide traffic (startup notices, cron,
	// keepalive, approval prompts) must never post into them.
	notNeverDefault := func(agentCol, chatCol string) string {
		return `AND NOT EXISTS (SELECT 1 FROM chat_metadata nd
		 WHERE nd.agent_id = ` + agentCol + ` AND nd.chat_id = ` + chatCol + `
		   AND nd.key = '` + ChatMetaNeverDefault + `' AND nd.value = 'true') `
	}

	if preferredPlatform != "" {
		// Preferred platform's pinned default chat.
		var chatID int64
		err := idx.db.QueryRow(
			`SELECT cm.chat_id FROM chat_metadata cm
			 WHERE cm.agent_id = ? AND cm.platform = ? AND cm.key = 'is_default' AND cm.value = 'true' `+notArchived+notNeverDefault("cm.agent_id", "cm.chat_id"),
			agentID, preferredPlatform,
		).Scan(&chatID)
		if err == nil && chatID != 0 {
			return NewChatSessionKey(agentID, chatID)
		}
		// Preferred platform's most recently active registered chat. Filter on
		// registered='true' (the universal real-chat signal set on first contact
		// by every platform) so a stray alias/features/archived row can't qualify.
		//
		// Reachability guard (app only): a "registered" app chat is a real,
		// deliverable conversation only once it has a persisted conv_id. A chat
		// registered but never opened (no conv_id) is a phantom — resolving the
		// default to it yields a session with no binding, so interactive prompts
		// fail (errNoBinding) and plain messages misdeliver via the fallback
		// (helen vocab-cron landed on such a phantom, 2026-07-11). conv_id is an
		// app concept; telegram/discord treat 'registered' as reachable, so the
		// guard is scoped to the app platform and other platforms are unaffected.
		convGuard := ""
		if preferredPlatform == "app" {
			convGuard = `AND EXISTS (SELECT 1 FROM chat_metadata cid
			     WHERE cid.agent_id = cm.agent_id AND cid.platform = cm.platform
			       AND cid.chat_id = cm.chat_id AND cid.key = 'conv_id') `
		}
		err = idx.db.QueryRow(
			`SELECT cm.chat_id FROM (SELECT DISTINCT agent_id, platform, chat_id FROM chat_metadata WHERE key = 'registered' AND value = 'true') cm
			 LEFT JOIN session_index si
			   ON si.agent_id = cm.agent_id AND si.chat_id = cm.chat_id AND si.is_root = 1
			 WHERE cm.agent_id = ? AND cm.platform = ? AND cm.chat_id != 0 `+convGuard+notArchived+notNeverDefault("cm.agent_id", "cm.chat_id")+`
			 ORDER BY unixepoch(si.last_user_activity_at) DESC NULLS LAST
			 LIMIT 1`,
			agentID, preferredPlatform,
		).Scan(&chatID)
		if err == nil && chatID != 0 {
			return NewChatSessionKey(agentID, chatID)
		}
		// No presence on the preferred platform — fall through.
	}

	// Any default chat. The is_default flag is platform-scoped; the session
	// key is derived from (agent, chat). Order by the derived session's user
	// activity so an agent with defaults on several platforms resolves to the
	// one a human touched most recently.
	var chatID int64
	err := idx.db.QueryRow(
		`SELECT cm.chat_id FROM chat_metadata cm
		 LEFT JOIN session_index si
		   ON si.agent_id = cm.agent_id AND si.chat_id = cm.chat_id AND si.is_root = 1
		 WHERE cm.agent_id = ? AND cm.key = 'is_default' AND cm.value = 'true' `+notArchived+notNeverDefault("cm.agent_id", "cm.chat_id")+`
		 ORDER BY unixepoch(si.last_user_activity_at) DESC NULLS LAST
		 LIMIT 1`,
		agentID,
	).Scan(&chatID)
	if err == nil && chatID != 0 {
		return NewChatSessionKey(agentID, chatID)
	}

	// Fallback: most recently user-active active ROOT for the agent. is_root=1
	// already excludes branches AND independent spawns (both ChildType != 0, so
	// is_root=0) — the only roots this can return are chat roots ('c') and an
	// agent's primary instance root ('i', classified 'unknown'), both of which
	// are legitimate user-facing default sessions. No session_type filter: it
	// would wrongly drop the instance root (e.g. an agent that received /send
	// before any chat binding), breaking default resolution. A never-default
	// chat is excluded here too (rung 4 is the one a merely-active Slack
	// session would otherwise win).
	var key string
	err = idx.db.QueryRow(
		`SELECT session_key FROM session_index si
		 WHERE si.agent_id = ? AND si.is_root = 1 AND si.status = 'active'
		   AND NOT EXISTS (SELECT 1 FROM chat_metadata arch
		     WHERE arch.agent_id = si.agent_id AND arch.chat_id = si.chat_id
		       AND arch.key = 'is_archived' AND arch.value = 'true')
		   `+notNeverDefault("si.agent_id", "si.chat_id")+`
		 ORDER BY unixepoch(si.last_user_activity_at) DESC NULLS LAST, unixepoch(si.created_at) DESC
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

// AnyAgentMetadataEquals reports whether any agent has metadata key set to
// value (e.g. whether any agent on this install has ever completed onboarding).
func (idx *SessionIndex) AnyAgentMetadataEquals(key, value string) (bool, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var one int
	err := idx.db.QueryRow(`SELECT 1 FROM agent_metadata WHERE key = ? AND value = ? LIMIT 1`, key, value).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
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

// HasActiveChild reports whether any active index row has parentKey as its
// parent_session_key. Served by idx_session_parent, so it costs one index
// probe instead of a scan of the whole index.
func (idx *SessionIndex) HasActiveChild(parentKey string) (bool, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var exists int
	err := idx.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM session_index WHERE parent_session_key = ? AND status = ?)`,
		parentKey, string(SessionStatusActive),
	).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists == 1, nil
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
