package log

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"foci/internal/sqlite"
	"foci/internal/timeutil"
)

// apiDB is the SQLite API call log (separate from the main Logger to
// match the conversation.go pattern — independent init/close lifecycle).
type apiDB struct {
	db   *sql.DB
	stmt *sql.Stmt
	mu   sync.Mutex
}

var apiLog *apiDB

// InitAPIDB opens (or creates) the SQLite API call log.
func InitAPIDB(path string) error {
	db, err := sqlite.OpenInit(path,
		`CREATE TABLE IF NOT EXISTS api_calls (
			id                 INTEGER PRIMARY KEY AUTOINCREMENT,
			ts                 DATETIME NOT NULL,
			session            TEXT NOT NULL,
			model              TEXT NOT NULL,
			input_tokens       INTEGER,
			output_tokens      INTEGER,
			cache_read_tokens  INTEGER,
			cache_write_tokens INTEGER,
			cost_usd           REAL,
			duration_ms        INTEGER,
			stop_reason        TEXT,
			call_type          TEXT NOT NULL,
			session_file       TEXT,
			session_line       INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_api_calls_ts ON api_calls(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_api_calls_ts_unix ON api_calls(unixepoch(ts))`,
		`CREATE INDEX IF NOT EXISTS idx_api_calls_session ON api_calls(session)`,
	)
	if err != nil {
		return err
	}

	// Migrations for existing DBs (ALTER TABLE is a no-op if column exists).
	_, _ = db.Exec(`ALTER TABLE api_calls ADD COLUMN provider TEXT DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE api_calls ADD COLUMN pre_messages INTEGER`)
	_, _ = db.Exec(`ALTER TABLE api_calls ADD COLUMN new_session TEXT`)

	stmt, err := db.Prepare(`INSERT INTO api_calls
		(ts, provider, session, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		 cost_usd, duration_ms, stop_reason, call_type, session_file, session_line, pre_messages, new_session)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("prepare insert: %w", err)
	}

	apiLog = &apiDB{db: db, stmt: stmt}
	return nil
}

// CloseAPIDB closes the SQLite API call log.
func CloseAPIDB() {
	if apiLog != nil {
		_ = apiLog.stmt.Close()
		_ = apiLog.db.Close()
		apiLog = nil
	}
}

// SessionStats holds aggregated session statistics from the API call log.
type SessionStats struct {
	TurnCount     int       // conversation + delegated_turn calls
	TotalCalls    int       // all call types
	TotalCost     float64   // sum of cost_usd
	CreatedAt     time.Time // earliest timestamp
	LastActivity  time.Time // latest timestamp
	ContextTokens int       // input+cache from most recent turn
}

// QuerySessionStats returns aggregated stats for a session key from api.db.
// Works for both API and delegated (CC backend) sessions.
func QuerySessionStats(sessionKey string) (*SessionStats, error) {
	if apiLog == nil || apiLog.db == nil {
		return nil, fmt.Errorf("api db not initialised")
	}

	var stats SessionStats
	var createdStr, activeStr sql.NullString

	// Aggregate stats in one query.
	err := apiLog.db.QueryRow(`
		SELECT
			COUNT(*) AS total_calls,
			COUNT(CASE WHEN call_type IN ('conversation', 'delegated_turn') THEN 1 END) AS turn_count,
			COALESCE(SUM(cost_usd), 0) AS total_cost,
			MIN(ts) AS created_at,
			MAX(ts) AS last_activity
		FROM api_calls
		WHERE session = ?`, sessionKey,
	).Scan(&stats.TotalCalls, &stats.TurnCount, &stats.TotalCost, &createdStr, &activeStr)
	if err != nil {
		return nil, fmt.Errorf("query session stats: %w", err)
	}

	if createdStr.Valid {
		stats.CreatedAt, _ = time.Parse(time.RFC3339, createdStr.String)
	}
	if activeStr.Valid {
		stats.LastActivity, _ = time.Parse(time.RFC3339, activeStr.String)
	}

	// Context tokens from the most recent turn (conversation or delegated).
	var ctxTokens sql.NullInt64
	_ = apiLog.db.QueryRow(`
		SELECT COALESCE(input_tokens, 0) + COALESCE(cache_read_tokens, 0) + COALESCE(cache_write_tokens, 0)
		FROM api_calls
		WHERE session = ? AND call_type IN ('conversation', 'delegated_turn', '')
		ORDER BY ts DESC
		LIMIT 1`, sessionKey,
	).Scan(&ctxTokens)
	if ctxTokens.Valid {
		stats.ContextTokens = int(ctxTokens.Int64)
	}

	return &stats, nil
}

func (a *apiDB) insert(entry APIEntry) {
	ts := timeutil.Format(entry.Timestamp)

	var sessionFile *string
	if entry.SessionFile != "" {
		sessionFile = &entry.SessionFile
	}
	var sessionLine *int
	if entry.SessionLine > 0 {
		sessionLine = &entry.SessionLine
	}
	var preMessages *int
	if entry.PreMessages > 0 {
		preMessages = &entry.PreMessages
	}
	var newSession *string
	if entry.NewSession != "" {
		newSession = &entry.NewSession
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	_, err := a.stmt.Exec(
		ts, entry.Provider, entry.Session, entry.Model,
		entry.Input, entry.Output, entry.CacheRead, entry.CacheWrite,
		entry.CostUSD, entry.DurationMS, entry.StopReason,
		entry.CallType, sessionFile, sessionLine,
		preMessages, newSession,
	)
	if err != nil {
		std.event(ERROR, "api_db", "insert error: %v", err)
	}
}
