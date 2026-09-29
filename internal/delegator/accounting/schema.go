package accounting

import (
	"database/sql"
	"fmt"

	"foci/internal/modelinfo"
)

// schemaStmts is the ledger schema (#2111 §2.2). Every statement is idempotent,
// so ensureSchema runs on every open. The CHECK constraints are the ledger's
// contracts in the one place no writer can route around.
var schemaStmts = []string{
	// api_calls: one row per API call, on every backend (R3). No cost column:
	// counts live in call_tokens and cost is computed by the views (R5).
	//
	// call_key is the provider's message id, or a stable id the adapter builds;
	// the unique index on (backend, call_key) IS the dedup. It is NULL only on
	// legacy rows, which sit outside that index. turn_id is NULL only on
	// overhead calls (R6), which no turn can own.
	//
	// cost_basis says, per call, where the views take its cost from (Dick,
	// 2026-09-29 12:08): 'counts' prices call_tokens at the dated rates;
	// 'recorded' takes legacy_calculated_cost_usd verbatim. 'recorded' is only
	// for a legacy row whose stored counts are NOT what was billed — a
	// delegated row with no turn totals, whose un-suffixed columns are a
	// context-fill snapshot — and only when it has a recorded figure.
	`CREATE TABLE IF NOT EXISTS api_calls (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		call_key     TEXT,
		billed_at    TEXT NOT NULL,
		backend      TEXT NOT NULL CHECK (backend IN ('ccstream', 'codex', 'opencode', 'api')),
		provider     TEXT NOT NULL DEFAULT '',
		model        TEXT NOT NULL,
		rate_model   TEXT,
		session      TEXT NOT NULL,
		agent_id     TEXT,
		turn_id      TEXT REFERENCES turns(turn_id),
		actor        TEXT NOT NULL DEFAULT '',
		kind         TEXT NOT NULL CHECK (kind IN ('call', 'compaction', 'overhead', 'summary', 'spawn', 'legacy')),
		finality     TEXT NOT NULL CHECK (finality IN ('completed', 'stopless', 'interrupted', 'derived', 'legacy')),
		class_method TEXT NOT NULL CHECK (class_method IN ('observed', 'backend_rule', 'solved', 'kind', 'unknown')),
		stop_reason  TEXT,
		session_file TEXT,
		session_line INTEGER,
		detail       TEXT,
		legacy_calculated_cost_usd REAL,
		cost_basis   TEXT NOT NULL DEFAULT 'counts' CHECK (cost_basis IN ('counts', 'recorded')),
		CHECK ((kind = 'legacy') = (call_key IS NULL)),
		CHECK ((kind = 'legacy') = (finality = 'legacy')),
		CHECK ((kind = 'overhead') = (turn_id IS NULL)),
		CHECK (legacy_calculated_cost_usd IS NULL OR kind = 'legacy'),
		CHECK (cost_basis = 'counts' OR (kind = 'legacy' AND legacy_calculated_cost_usd IS NOT NULL))
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS api_calls_key ON api_calls(backend, call_key) WHERE call_key IS NOT NULL`,
	`CREATE INDEX IF NOT EXISTS api_calls_billed_at ON api_calls(billed_at)`,
	`CREATE INDEX IF NOT EXISTS api_calls_session ON api_calls(session, billed_at)`,
	`CREATE INDEX IF NOT EXISTS api_calls_turn ON api_calls(turn_id)`,

	// call_tokens: a call's counts, one row per non-zero class (R1). Counts are
	// never negative: a negative count would price as a credit.
	`CREATE TABLE IF NOT EXISTS call_tokens (
		call_id INTEGER NOT NULL REFERENCES api_calls(id),
		class   TEXT NOT NULL,
		count   INTEGER NOT NULL CHECK (count > 0),
		PRIMARY KEY (call_id, class)
	) WITHOUT ROWID`,

	// turns: per-turn facts, never cost (R3). legacy_* hold context fill COPIED
	// from pre-ledger rows (R4): a summed row is not a context size, so these
	// are NULL for every turn the ledger books itself. activity_closed_at NULL
	// means spend may still arrive (R8).
	`CREATE TABLE IF NOT EXISTS turns (
		turn_id            TEXT PRIMARY KEY,
		session            TEXT NOT NULL,
		agent_id           TEXT,
		backend            TEXT NOT NULL CHECK (backend IN ('ccstream', 'codex', 'opencode', 'api')),
		source             TEXT NOT NULL CHECK (source IN ('user', 'autonomous', 'keepalive', 'compaction', 'batch', 'system')),
		purpose            TEXT,
		started_at         TEXT NOT NULL,
		ended_at           TEXT,
		ask_cycles         INTEGER,
		stop_reason        TEXT,
		final_model        TEXT,
		activity_closed_at TEXT,
		legacy_input       INTEGER,
		legacy_output      INTEGER,
		legacy_cache_read  INTEGER,
		legacy_cache_write INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS turns_session ON turns(session, started_at)`,

	// backend_reports: each backend's OWN reported totals, at its native grain
	// (R5) — what the reconciliation checks compare the ledger against.
	`CREATE TABLE IF NOT EXISTS backend_reports (
		id        INTEGER PRIMARY KEY AUTOINCREMENT,
		at        TEXT NOT NULL,
		backend   TEXT NOT NULL CHECK (backend IN ('ccstream', 'codex', 'opencode', 'api')),
		session   TEXT NOT NULL,
		scope_key TEXT NOT NULL,
		model     TEXT NOT NULL,
		grain     TEXT NOT NULL CHECK (grain IN ('cumulative', 'per_call')),
		cost_usd  REAL,
		tokens    TEXT NOT NULL DEFAULT '{}'
	)`,
	`CREATE INDEX IF NOT EXISTS backend_reports_scope ON backend_reports(backend, scope_key, at)`,

	// token_classes and token_rates are RENDERED from modelinfo (renderRates),
	// never written by anything else: modelinfo is the source of truth and
	// these are a copy SQL can join (#2111 §13.3). usd_per_unit NULL means the
	// row in effect publishes no rate for the class.
	`CREATE TABLE IF NOT EXISTS token_classes (
		class      TEXT PRIMARY KEY,
		unit       TEXT NOT NULL,
		per_units  REAL NOT NULL,
		in_context INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS token_rates (
		rate_model     TEXT NOT NULL,
		class          TEXT NOT NULL,
		effective_from TEXT NOT NULL,
		usd_per_unit   REAL,
		PRIMARY KEY (rate_model, class, effective_from)
	) WITHOUT ROWID`,

	// call_class_costs: one call's cost in one class, priced at the rate row in
	// effect on the call's billing date (UTC; billed_at is stored in UTC). NULL
	// when the call has no rate_model, the class is outside the vocabulary, or
	// the rate row in effect publishes no rate for it. The term order (count /
	// per_units * rate) is modelinfo.CostAsOf's, so the two agree exactly.
	`DROP VIEW IF EXISTS call_class_costs`,
	`CREATE VIEW call_class_costs AS
	SELECT t.call_id, t.class, t.count,
		CAST(t.count AS REAL) / k.per_units * (
			SELECT r.usd_per_unit FROM token_rates r
			WHERE r.rate_model = c.rate_model AND r.class = t.class
			  AND r.effective_from <= substr(c.billed_at, 1, 10)
			ORDER BY r.effective_from DESC LIMIT 1
		) AS cost_usd
	FROM call_tokens t
	JOIN api_calls c ON c.id = t.call_id
	LEFT JOIN token_classes k ON k.class = t.class`,

	// call_costs: each call and its cost, from the call's recorded cost_basis
	// — never a COALESCE, so a counts-priced call can never silently fall back
	// to a recorded figure or the reverse. On the counts basis a call with no
	// counts costs 0 and a call with ANY unpriced class costs NULL — never a
	// silent 0 (R9) — and unpriced_classes says how many classes caused it. Every
	// finality is priced, 'interrupted' included (Dick, 2026-09-29 12:08, #2111
	// §13.8: those calls were sent and billed).
	`DROP VIEW IF EXISTS call_costs`,
	`CREATE VIEW call_costs AS
	SELECT c.id, c.call_key, c.billed_at, c.backend, c.provider, c.model, c.rate_model,
		c.session, c.agent_id, c.turn_id, c.actor, c.kind, c.finality, c.class_method, c.cost_basis,
		CASE c.cost_basis
			WHEN 'recorded' THEN c.legacy_calculated_cost_usd
			ELSE (SELECT CASE WHEN COUNT(*) = 0 THEN 0.0
			                  WHEN SUM(x.cost_usd IS NULL) > 0 THEN NULL
			                  ELSE SUM(x.cost_usd) END
			        FROM call_class_costs x WHERE x.call_id = c.id)
		END AS cost_usd,
		CASE c.cost_basis
			WHEN 'recorded' THEN 0
			ELSE (SELECT COUNT(*) FROM call_class_costs x
			        WHERE x.call_id = c.id AND x.cost_usd IS NULL)
		END AS unpriced_classes
	FROM api_calls c`,

	// turn_costs: per turn. cost_usd sums the PRICED calls; unpriced_calls
	// counts the rest, so an aggregate is never silently short. still_running
	// is live (R8). context_fill is the last parent call's context-class counts,
	// or the copied legacy figures for a legacy turn.
	`DROP VIEW IF EXISTS turn_costs`,
	`CREATE VIEW turn_costs AS
	SELECT t.turn_id, t.session, t.agent_id, t.backend, t.source, t.purpose,
		t.started_at, t.ended_at, t.stop_reason, t.final_model, t.ask_cycles,
		(SELECT TOTAL(cost_usd) FROM call_costs c WHERE c.turn_id = t.turn_id) AS cost_usd,
		(SELECT TOTAL(cost_usd) FROM call_costs c WHERE c.turn_id = t.turn_id AND c.actor = '') AS parent_cost_usd,
		(SELECT TOTAL(cost_usd) FROM call_costs c WHERE c.turn_id = t.turn_id AND c.actor <> '') AS subagent_cost_usd,
		(SELECT COUNT(*) FROM call_costs c WHERE c.turn_id = t.turn_id AND c.cost_usd IS NULL) AS unpriced_calls,
		(SELECT COUNT(*) FROM api_calls c WHERE c.turn_id = t.turn_id) AS calls,
		t.activity_closed_at IS NULL AS still_running,
		CASE WHEN t.legacy_input IS NOT NULL OR t.legacy_cache_read IS NOT NULL OR t.legacy_cache_write IS NOT NULL
			THEN COALESCE(t.legacy_input, 0) + COALESCE(t.legacy_cache_read, 0) + COALESCE(t.legacy_cache_write, 0)
			ELSE (SELECT COALESCE(SUM(k.count), 0) FROM call_tokens k JOIN token_classes tc ON tc.class = k.class
			      WHERE tc.in_context AND k.call_id = (
			          SELECT c.id FROM api_calls c
			          WHERE c.turn_id = t.turn_id AND c.actor = '' AND c.kind = 'call'
			          ORDER BY c.billed_at DESC, c.id DESC LIMIT 1))
		END AS context_fill
	FROM turns t`,

	// turn_class_costs: a turn's counts and cost split by class and actor,
	// over counts-basis calls only: a recorded-basis call has one figure and
	// no class split (its stored counts are not what it was billed for).
	`DROP VIEW IF EXISTS turn_class_costs`,
	`CREATE VIEW turn_class_costs AS
	SELECT c.turn_id, c.actor, x.class, SUM(x.count) AS count,
		TOTAL(x.cost_usd) AS cost_usd, SUM(x.cost_usd IS NULL) AS unpriced_calls
	FROM api_calls c JOIN call_class_costs x ON x.call_id = c.id
	WHERE c.turn_id IS NOT NULL AND c.cost_basis = 'counts'
	GROUP BY c.turn_id, c.actor, x.class`,

	// daily_costs and session_costs: every call is dated by when it was BILLED,
	// so late spend can never land on the wrong day (#1929). Days are UTC.
	`DROP VIEW IF EXISTS daily_costs`,
	`CREATE VIEW daily_costs AS
	SELECT substr(billed_at, 1, 10) AS day, COUNT(*) AS calls,
		TOTAL(cost_usd) AS cost_usd, SUM(cost_usd IS NULL) AS unpriced_calls
	FROM call_costs GROUP BY day`,
	`DROP VIEW IF EXISTS session_costs`,
	`CREATE VIEW session_costs AS
	SELECT session, COUNT(*) AS calls, TOTAL(cost_usd) AS cost_usd,
		SUM(cost_usd IS NULL) AS unpriced_calls,
		MIN(billed_at) AS first_billed_at, MAX(billed_at) AS last_billed_at
	FROM call_costs GROUP BY session`,
}

// ensureSchema creates whatever of the ledger schema is missing and rebuilds
// the views, inside tx.
func ensureSchema(tx *sql.Tx) error {
	for _, s := range schemaStmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("ledger schema: %w", err)
		}
	}
	return nil
}

// renderRates replaces token_classes and token_rates with modelinfo's current
// vocabulary and rate history, inside tx — a full replace, so the copy always
// equals modelinfo (#2111 §11.11).
func renderRates(tx *sql.Tx) error {
	if _, err := tx.Exec(`DELETE FROM token_classes`); err != nil {
		return fmt.Errorf("clear token_classes: %w", err)
	}
	for _, c := range modelinfo.Classes() {
		unit, _ := modelinfo.ClassUnit(c)
		if _, err := tx.Exec(`INSERT INTO token_classes (class, unit, per_units, in_context) VALUES (?, ?, ?, ?)`,
			string(c), string(unit), unit.PerUnits(), modelinfo.InContext(c)); err != nil {
			return fmt.Errorf("render token_classes: %w", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM token_rates`); err != nil {
		return fmt.Errorf("clear token_rates: %w", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO token_rates (rate_model, class, effective_from, usd_per_unit) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare token_rates: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range modelinfo.RateTable() {
		if _, err := stmt.Exec(r.RateModel, string(r.Class), r.EffectiveFrom, r.USDPerUnit); err != nil {
			return fmt.Errorf("render token_rates: %w", err)
		}
	}
	return nil
}
