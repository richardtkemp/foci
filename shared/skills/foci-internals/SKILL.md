---
name: foci-internals
description: Debug and investigate foci platform internals. API logs, payload logs, session files, cache diagnosis, service logs, and common investigation patterns.
---

# Foci Internals — Debugging & Investigation

## Auth

- Foci uses **setup-tokens** from `claude setup-token` (prefix `sk-ant-oat01-`). Stored in `secrets.toml` alongside `foci.toml`.
- `foci auth` prompts for token, validates, saves, and signals running gateway to hot-swap credentials (POST `/-/reload-credentials`).
- Setup-tokens valid ~1 year. No dashboard UI to revoke — regenerate to invalidate old one.

## Anthropic Cache

- Per-session, prefix-matched. Switching models rebuilds cache.
- Multiple prefixes cached simultaneously — switching sessions doesn't evict other sessions' caches.
- Foci caches character files in memory — edits take effect at next compaction or restart.

## Data Sources

There are two scopes for data:
- **Global** (`~/data/` by default, configurable via `data_dir`): `api.db`, `state.db`, `state.json`, sessions
- **Per-agent** (`<workspace>/.data/`): `conversation.db`, `todo.db`, `scratchpad.db`, `reminders.db`, `tasklist.db`, `memory.db`, `search.bleve`, `tool_details.db`

### API Call Log (SQLite)
All API calls logged to `~/data/api.db` table `api_calls`.

```bash
# Recent calls
sqlite3 ~/data/api.db "SELECT ts, call_type, cost_usd FROM api_calls ORDER BY ts DESC LIMIT 10"

# Filter by type: conversation, compaction, summary, spawn
sqlite3 ~/data/api.db "SELECT ts, cost_usd, cache_read, cache_write FROM api_calls WHERE call_type='conversation' ORDER BY ts DESC LIMIT 10"

# Cost in a time window
sqlite3 ~/data/api.db "SELECT SUM(cost_usd) FROM api_calls WHERE ts > '2026-03-04T06:00'"
```

### Payload Logs (JSONL)
Full request/response payloads per API call. Only written when `full_payload = true` in config. Large file — filter with jq, don't cat.

**Path:** `logs/api-payload.jsonl` (default, configurable via `payload_file`)
**Archives:** `logs/archive/api-payload-*.jsonl.gz`

```bash
# Calls in a time window with cache stats
tail -200 ~/logs/api-payload.jsonl | jq -r '
  select(.ts >= "TIME1" and .ts <= "TIME2") |
  "\(.ts) cache_read=\(.response.usage.cache_read_input_tokens // 0) cache_write=\(.response.usage.cache_creation_input_tokens // 0)"'

# System prompt block sizes for a specific call
tail -200 ~/logs/api-payload.jsonl | jq -c '
  select(.ts >= "TIME") |
  [.request.system[] | {type, text_len: (.text // "" | length), cache: .cache_control}]' | head -1

# Compare system prompts between two calls (extract to files, then diff)
```

### Session Files (JSONL)
Per-session conversation history. No timestamps — just role + content.

**Path:** `~/data/sessions/<AGENT_ID>/<SESSION_TYPE_ID>/<VERSION_TS>/root.jsonl`

Branch files have a `branch_meta` first line. Pre-compaction backups: `root.1.jsonl`, `root.2.jsonl` etc.

```bash
# Last few messages
tail -5 /path/to/root.jsonl | jq -r '.role + ": " + (.content[]? | select(.type=="text") | .text)'

# All content (not just text)
tail -5 /path/to/root.jsonl | jq -r '.role + ": " + (.content[]? | tostring)'
```

### State Store
Runtime state as JSON: `~/data/state.json`

### Global SQLite Databases (`~/data/`)

| Database | Table(s) | Contents |
|---|---|---|
| `api.db` | `api_calls` | Every Anthropic API call with timestamps, cost, cache stats |
| `state.db` | `session_index`, `agent_metadata`, `chat_metadata`, `session_metadata`, `system_state` | Unified state: session lifecycle, agent/chat/session metadata |

```bash
# Inspect any database schema
sqlite3 ~/data/state.db ".schema"

# Session awareness — all sessions with status, type, parent, last activity
sqlite3 ~/data/state.db "SELECT session_key, status, session_type, last_activity_at FROM session_index ORDER BY last_activity_at DESC LIMIT 10"

# Active sessions only
sqlite3 ~/data/state.db "SELECT session_key, session_type, last_activity_at FROM session_index WHERE status='active' ORDER BY last_activity_at DESC"
```

### Per-Agent SQLite Databases (`<workspace>/.data/`)

| Database | Table(s) | Contents |
|---|---|---|
| `conversation.db` | `messages` | Telegram send/receive log (NOT session history) |
| `todo.db` | `todos` | Todo items |
| `tasklist.db` | `tasklist` | Task list items |
| `scratchpad.db` | `scratchpad` | Working notes |
| `reminders.db` | `reminders` | Scheduled reminders |
| `tool_details.db` | `tool_call_details` | Tool call display data for Telegram inline buttons |
| `memory.db` | `memory_fts`, `memory_meta` | FTS5 full-text search index |
| `search.bleve/` | — | Bleve search index |

```bash
# Example: conversation log (replace <workspace> with agent's workspace path)
sqlite3 <workspace>/.data/conversation.db "SELECT * FROM messages ORDER BY rowid DESC LIMIT 5"
```

### Service Logs
```bash
# Recent foci logs
journalctl -u foci --since "1 hour ago" --no-pager

# Compaction events
grep 'compact' ~/logs/foci.log | tail -20

# Warnings and errors
grep -E 'WARN|ERROR' ~/logs/foci.log | tail -20

# Session lifecycle
grep 'branch created\|turn_lock' ~/logs/foci.log | grep '<SESSION_KEY>' | tail -20
```

## Common Investigations

### Cache Bust Diagnosis
Find the last call with `cache_read > 0` before the first `cache_read = 0`, extract and diff their system prompt blocks.

Common causes: character file edit picked up by `bootstrap.Reload()`, model switch, service restart, compaction on another session.

### "Where did the cost go?"
```bash
# Total cost in last N hours
sqlite3 ~/data/api.db "SELECT SUM(cost_usd), COUNT(*) FROM api_calls WHERE ts > datetime('now', '-3 hours')"

# Biggest individual calls
sqlite3 ~/data/api.db "SELECT ts, call_type, cost_usd, cache_read, cache_write FROM api_calls WHERE ts > datetime('now', '-3 hours') ORDER BY cost_usd DESC LIMIT 10"

# Cache busts (cache_read = 0 with large cache_write)
sqlite3 ~/data/api.db "SELECT ts, cost_usd, cache_write FROM api_calls WHERE cache_read = 0 AND cache_write > 10000 ORDER BY ts DESC LIMIT 10"
```

### Session Compaction
```bash
# When did compaction happen?
grep 'compacting session\|compacted from' ~/logs/foci.log | tail -10

# What triggered it?
grep 'should_compact.*result=true' ~/logs/foci.log | tail -10
```

### Background Cron Sessions
```bash
# List recent cron sessions
grep 'branch created.*cron' ~/logs/foci.log | tail -10

# Follow a specific cron session
grep '<SESSION_KEY>' ~/logs/foci.log | head -20
```
