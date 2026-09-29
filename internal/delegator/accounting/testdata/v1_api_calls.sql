-- The pre-ledger api_calls table as internal/log's InitAPIDB left it after its
-- last ALTER (#1962): the shape every live api.db had at the #2111 cutover. The
-- writer is gone, so this is the fixture the migration tests build a v1
-- database from.
CREATE TABLE api_calls (
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
	session_line       INTEGER,
	provider TEXT DEFAULT '',
	pre_messages INTEGER,
	calculated_cost_usd REAL,
	turn_input_tokens INTEGER,
	turn_cache_read_tokens INTEGER,
	turn_cache_write_tokens INTEGER,
	turn_output_tokens INTEGER,
	turn_id TEXT,
	agent_id TEXT,
	subagent_id TEXT,
	turn_web_searches INTEGER,
	purpose TEXT
);
