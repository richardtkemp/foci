package log

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"foci/internal/modelinfo"
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
		// Column SCOPES differ, and nothing in the schema types says so
		// (#1806). Before differencing or summing two columns, check they
		// accumulate at the same scope:
		//   cost_usd             — CUMULATIVE per CC PROCESS (ccstream rows);
		//                          the backend's figure verbatim. Never SUM.
		//   calculated_cost_usd  — per TURN (one row per turn since #1856).
		//                          foci's own priced figure. SUM this.
		//   output_tokens        — per TURN sum of every cycle's output, for the
		//                          PARENT model only.
		//   turn_output_tokens   — per TURN sum across EVERY model (#1891).
		//                          Differs from output_tokens exactly when a
		//                          subagent ran on another model.
		//   input_tokens, cache_read_tokens, cache_write_tokens
		//                        — the turn's FINAL cycle context fill (a
		//                          snapshot, what /context reads); NOT what
		//                          the cost was priced from.
		//   turn_input_tokens, turn_cache_read_tokens, turn_cache_write_tokens
		//                        — per TURN sums across every model (#1854,
		//                          made cross-model by #1866 P3); with
		//                          turn_output_tokens
		//                          these are what calculated_cost_usd priced.
		//                          NULL as a group where the writer measured
		//                          no turn total.
		//   turn_id              — names the turn a row belongs to
		//                          ("<session>@<UnixNano>"); the only durable
		//                          turn identity (#1695). A turn is no longer
		//                          always ONE row: since #1880 phase C a turn
		//                          with subagents writes one parent row plus a
		//                          call_type='subagent_turn' row per subagent,
		//                          all sharing this id. SUM per turn_id to get
		//                          what a single row used to hold.
		//   agent_id             — the subagent that did the work, on a
		//                          subagent_turn row; empty on a parent row.
		// docs/WIRING.md "Cost columns" has the full table and history.
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
	// #1674: foci's own priced cost. cost_usd keeps its historical meaning (the
	// backend's PROVIDED figure) so existing rows are untouched — but for CC
	// rows that figure is cumulative per process and must not be summed.
	_, _ = db.Exec(`ALTER TABLE api_calls ADD COLUMN calculated_cost_usd REAL`)
	// new_session was wired end-to-end but never written by any producer (dead
	// plumbing for a compaction-rotation feature that never landed). Drop it from
	// existing DBs; the Exec is a no-op (ignored error) once the column is gone.
	_, _ = db.Exec(`ALTER TABLE api_calls DROP COLUMN new_session`)
	// #1854: the turn-summed token counts calculated_cost_usd was priced from.
	// The four original token columns keep their meaning — a delegated turn's
	// FINAL cycle context fill, which /context reads — and cannot be priced:
	// they recover ~20% of the recorded cost. These three plus output_tokens
	// can: output_tokens was already the turn sum, so it has no turn_ twin.
	// NULL for rows written before the change and for backends that do not
	// measure them.
	_, _ = db.Exec(`ALTER TABLE api_calls ADD COLUMN turn_input_tokens INTEGER`)
	_, _ = db.Exec(`ALTER TABLE api_calls ADD COLUMN turn_cache_read_tokens INTEGER`)
	_, _ = db.Exec(`ALTER TABLE api_calls ADD COLUMN turn_cache_write_tokens INTEGER`)
	// turn_output_tokens: DROP then ADD, in that order and for a reason.
	//
	// #1854 gave the group no output twin because output_tokens already held the
	// turn sum — true then, and a column added at that time was correctly dropped
	// as a duplicate. #1866 P3 made pricing CROSS-MODEL, so the three turn_
	// columns became all-model sums while output_tokens stayed parent-only.
	// Measured on a live turn: 27,305 output tokens priced, 10,212 stored, and
	// the #1854 re-price identity failed by the difference (#1891).
	//
	// The DROP clears any stale duplicate still carrying parent-only values, so
	// the ADD yields a column that is NULL — "not measured" — for every historical
	// row rather than silently wrong for the cross-model ones. Do NOT collapse
	// these into one statement, and do NOT re-drop this column as a duplicate: it
	// is only a duplicate on single-model turns.
	_, _ = db.Exec(`ALTER TABLE api_calls DROP COLUMN turn_output_tokens`)
	_, _ = db.Exec(`ALTER TABLE api_calls ADD COLUMN turn_output_tokens INTEGER`)
	// #1880 phase C / #1695. Historical rows get NULL: their turn identity was
	// never recorded and cannot be reconstructed, so "" would assert a turn
	// that is not knowable. The index is on turn_id alone because the only
	// query shape is "give me every row of this turn".
	_, _ = db.Exec(`ALTER TABLE api_calls ADD COLUMN turn_id TEXT`)
	_, _ = db.Exec(`ALTER TABLE api_calls ADD COLUMN agent_id TEXT`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_api_calls_turn_id ON api_calls(turn_id)`)

	stmt, err := db.Prepare(`INSERT INTO api_calls
		(ts, provider, session, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		 cost_usd, duration_ms, stop_reason, call_type, session_file, session_line, pre_messages,
		 calculated_cost_usd,
		 turn_input_tokens, turn_cache_read_tokens, turn_cache_write_tokens, turn_output_tokens,
		 turn_id, agent_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
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

	// Aggregate call counts + timestamps in one query. cost_usd is deliberately
	// NOT summed here: it's now the golden (provider-reported) cost only —
	// NULL for calls with no golden figure — so a SQL SUM would silently
	// undercount every NULL row as $0. TotalCost is computed below in Go via
	// APIEntry.EffectiveCost, which live-calculates from tokens for the NULL
	// rows (foci_todo #1407).
	err := apiLog.db.QueryRow(`
		SELECT
			COUNT(*) AS total_calls,
			COUNT(CASE WHEN call_type IN ('conversation', 'delegated_turn') THEN 1 END) AS turn_count,
			MIN(ts) AS created_at,
			MAX(ts) AS last_activity
		FROM api_calls
		WHERE session = ?`, sessionKey,
	).Scan(&stats.TotalCalls, &stats.TurnCount, &createdStr, &activeStr)
	if err != nil {
		return nil, fmt.Errorf("query session stats: %w", err)
	}

	if createdStr.Valid {
		stats.CreatedAt, _ = time.Parse(time.RFC3339, createdStr.String)
	}
	if activeStr.Valid {
		stats.LastActivity, _ = time.Parse(time.RFC3339, activeStr.String)
	}

	for _, e := range querySessionCostRows(sessionKey) {
		stats.TotalCost += e.EffectiveCost()
	}

	// Context tokens from the most recent turn (conversation or delegated)
	// that actually consumed context. Synthetic turns (no-inference turns
	// like [[NO_RESPONSE]], model "<synthetic>") log zero tokens; without the
	// >0 filter, a synthetic turn landing on top of a real one would zero out
	// contextTokens and suppress the /status Context line entirely.
	var ctxTokens sql.NullInt64
	_ = apiLog.db.QueryRow(`
		SELECT COALESCE(input_tokens, 0) + COALESCE(cache_read_tokens, 0) + COALESCE(cache_write_tokens, 0) AS ctx
		FROM api_calls
		WHERE session = ? AND call_type IN ('conversation', 'delegated_turn', '')
		  AND COALESCE(input_tokens, 0) + COALESCE(cache_read_tokens, 0) + COALESCE(cache_write_tokens, 0) > 0
		ORDER BY ts DESC
		LIMIT 1`, sessionKey,
	).Scan(&ctxTokens)
	if ctxTokens.Valid {
		stats.ContextTokens = int(ctxTokens.Int64)
	}

	return &stats, nil
}

// apiRowCols is the column list shared by ReadAPIDBLog and
// querySessionCostRows — kept in one place so both stay in sync.
const apiRowCols = `ts, COALESCE(provider, ''), session, model,
	       COALESCE(input_tokens, 0), COALESCE(output_tokens, 0),
	       COALESCE(cache_read_tokens, 0), COALESCE(cache_write_tokens, 0),
	       cost_usd, COALESCE(duration_ms, 0),
	       COALESCE(stop_reason, ''), call_type,
	       COALESCE(session_file, ''), COALESCE(session_line, 0),
	       COALESCE(pre_messages, 0), calculated_cost_usd,
	       turn_input_tokens, turn_cache_read_tokens, turn_cache_write_tokens,
	       turn_output_tokens,
	       COALESCE(turn_id, ''), COALESCE(agent_id, '')`

// scanAPIRows drains rows selected via apiRowCols into []APIEntry. Both cost
// columns are nullable: cost_usd (ProvidedCostUSD) is NULL when the backend
// reported none, and calculated_cost_usd is NULL for rows written before
// #1674. Callers needing a display cost must call APIEntry.EffectiveCost —
// never read either column directly, and never SUM cost_usd, which for CC rows
// holds a per-process running total rather than a per-turn cost.
func scanAPIRows(rows *sql.Rows) []APIEntry {
	var entries []APIEntry
	for rows.Next() {
		var e APIEntry
		var tsStr string
		var providedCost, calculatedCost sql.NullFloat64
		var turnIn, turnCR, turnCW, turnOut sql.NullInt64
		if err := rows.Scan(
			&tsStr, &e.Provider, &e.Session, &e.Model,
			&e.Input, &e.Output, &e.CacheRead, &e.CacheWrite,
			&providedCost, &e.DurationMS, &e.StopReason, &e.CallType,
			&e.SessionFile, &e.SessionLine, &e.PreMessages, &calculatedCost,
			&turnIn, &turnCR, &turnCW, &turnOut,
			&e.TurnID, &e.AgentID,
		); err != nil {
			continue
		}
		// All four are written together or not at all (see insert), so one
		// column's validity speaks for the group.
		//
		// Output falls back to output_tokens when turn_output_tokens is NULL:
		// every row written before #1891 has the parent-only figure there, and on
		// a single-model turn the two are the same number. That keeps historical
		// rows re-priceable instead of reading as zero output.
		if turnIn.Valid {
			out := e.Output
			if turnOut.Valid {
				out = int(turnOut.Int64)
			}
			e.Turn = &modelinfo.TokenCounts{
				Input:      int(turnIn.Int64),
				Output:     out,
				CacheRead:  int(turnCR.Int64),
				CacheWrite: int(turnCW.Int64),
			}
		}
		if providedCost.Valid {
			v := providedCost.Float64
			e.ProvidedCostUSD = &v
		}
		if calculatedCost.Valid {
			v := calculatedCost.Float64
			e.CalculatedCostUSD = &v
		}
		// ts is written via timeutil.Format (RFC3339), so it round-trips here.
		e.Timestamp, _ = time.Parse(time.RFC3339, tsStr)
		entries = append(entries, e)
	}
	return entries
}

// ReadAPIDBLog returns all API call entries from the SQLite api.db in
// chronological order (ts ASC), mapped to []APIEntry.
//
// Unlike ReadAPILog — which reads the api.jsonl file that is reset on every
// service restart — this draws on the durable database, so cost summaries span
// restarts. The db is a superset of the JSONL (both are written per call at
// insert time), so callers should prefer it. Returns nil if the db is not
// initialised (e.g. in tests), letting callers fall back to ReadAPILog.
func ReadAPIDBLog() []APIEntry {
	if apiLog == nil || apiLog.db == nil {
		return nil
	}

	apiLog.mu.Lock()
	defer apiLog.mu.Unlock()

	rows, err := apiLog.db.Query(`SELECT ` + apiRowCols + ` FROM api_calls ORDER BY ts ASC`)
	if err != nil {
		std.event(ERROR, "api_db", "read log query error: %v", err)
		return nil
	}
	defer func() { _ = rows.Close() }()

	return scanAPIRows(rows)
}

// querySessionCostRows returns the columns EffectiveCost needs for every call
// in a session — used by QuerySessionStats to total cost with the live
// as-of-request-time fallback applied per row (see the SUM(cost_usd) comment
// in QuerySessionStats).
func querySessionCostRows(sessionKey string) []APIEntry {
	if apiLog == nil || apiLog.db == nil {
		return nil
	}
	apiLog.mu.Lock()
	defer apiLog.mu.Unlock()

	rows, err := apiLog.db.Query(`SELECT `+apiRowCols+` FROM api_calls WHERE session = ?`, sessionKey)
	if err != nil {
		std.event(ERROR, "api_db", "query session cost rows error: %v", err)
		return nil
	}
	defer func() { _ = rows.Close() }()

	return scanAPIRows(rows)
}

// nullIfEmpty maps "" to a SQL NULL. An empty turn_id or agent_id means the
// writer had no turn identity to record, which is "unknown", not "the turn
// whose id is the empty string" — and NULL is what every historical row holds,
// so a query for un-attributed rows finds one population, not two.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
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

	// The three turn columns are NULL as a group when the writer measured no
	// turn total — a zero there would price as "free", which is a wrong
	// answer, where NULL is "not measured". Turn.Output is not stored:
	// output_tokens already holds the turn sum for every writer.
	var turnIn, turnCR, turnCW, turnOut *int
	if t := entry.Turn; t != nil {
		turnIn, turnCR, turnCW, turnOut = &t.Input, &t.CacheRead, &t.CacheWrite, &t.Output
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	_, err := a.stmt.Exec(
		ts, entry.Provider, entry.Session, entry.Model,
		entry.Input, entry.Output, entry.CacheRead, entry.CacheWrite,
		entry.ProvidedCostUSD, entry.DurationMS, entry.StopReason,
		entry.CallType, sessionFile, sessionLine,
		preMessages, entry.CalculatedCostUSD,
		turnIn, turnCR, turnCW, turnOut,
		nullIfEmpty(entry.TurnID), nullIfEmpty(entry.AgentID),
	)
	if err != nil {
		std.event(ERROR, "api_db", "insert error: %v", err)
	}
}
