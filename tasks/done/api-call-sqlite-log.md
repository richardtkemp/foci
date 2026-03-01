# Task: Full API Call Log in SQLite (#268)

## Problem

API calls are logged to `api.jsonl` but:
1. **Summary tool** (`tools/summary.go` line 117) only logs via `log.Infof`, not `log.API`
2. **Spawn one-shot** (`tools/spawn.go` line 289) only logs via `log.Infof`, not `log.API`
3. Only `agent/agent.go:1013` and `compaction/compact.go:341` call `log.API`
4. JSONL is hard to query for analysis (no indexes, no joins)

## Requirements

### SQLite database
- Path: configured via `[logging] api_db` in config, default `{data_dir}/api.db`
- Table: `api_calls`
- Columns mapping to existing `log.APIEntry` fields:
  - `id` INTEGER PRIMARY KEY AUTOINCREMENT
  - `ts` DATETIME NOT NULL (indexed)
  - `session` TEXT NOT NULL (indexed)
  - `model` TEXT NOT NULL
  - `input_tokens` INTEGER
  - `output_tokens` INTEGER  
  - `cache_read_tokens` INTEGER
  - `cache_write_tokens` INTEGER
  - `cost_usd` REAL
  - `duration_ms` INTEGER
  - `stop_reason` TEXT
  - `call_type` TEXT NOT NULL — one of: "conversation", "compaction", "summary", "spawn"
  - `session_file` TEXT — path to the session JSONL file, if applicable
  - `session_line` INTEGER — line number in session file, if applicable (for conversation calls)

### Call sites to update
1. `agent/agent.go:1013` — already calls `log.API`. call_type="conversation". Add session_file + session_line.
2. `compaction/compact.go:341` — already calls `log.API`. Has `IsCompaction: true`. call_type="compaction".
3. `tools/summary.go:117` — currently `log.Infof` only. Add `log.API` call. call_type="summary". session from context.
4. `tools/spawn.go:289` — currently `log.Infof` only. Add `log.API` call. call_type="spawn". session from context.

### Implementation
- Add SQLite writer to `log/log.go` alongside existing JSONL writer
- `log.API()` writes to both JSONL (backward compat) and SQLite
- Add `CallType` field to `APIEntry` (replaces `IsCompaction` bool — "compaction" is now a call_type value)
- Add `SessionFile` and `SessionLine` optional fields to `APIEntry`
- SQLite init in `log.Init()` — create table if not exists
- No migration needed (new database)

### Config
```toml
[logging]
api_db = "/home/foci/data/api.db"  # empty = disabled
```

### Docs
- docs/CONFIG.md — add api_db field
- SPEC.md — mention SQLite API log

## Verification
- `go build ./... && go test ./... && go vet ./...`
- After deploy: verify all 4 call types appear in api.db
- `sqlite3 api.db "SELECT call_type, count(*) FROM api_calls GROUP BY call_type"` should show all types
