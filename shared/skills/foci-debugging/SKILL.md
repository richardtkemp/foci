---
name: foci-debugging
description: Debug and investigate foci platform internals. API logs, payload logs, session files, CC backend transcripts, cache diagnosis, service logs, and common investigation patterns.
---

# Foci Debugging — Internals & Investigation

## Auth

- Foci uses API keys for LLM providers (Anthropic, Gemini, OpenAI, OpenRouter). Stored in `secrets.toml` alongside `foci.toml`.
- `foci auth` prompts for provider and API key, saves, and signals running gateway to hot-swap credentials (POST `/-/reload-credentials`).
- For Anthropic, Claude Code credentials (`~/.claude/.credentials.json`) are used as a fallback if no API key is configured.

## Anthropic Cache

- Per-session, prefix-matched. Switching models rebuilds cache.
- Multiple prefixes cached simultaneously — switching sessions doesn't evict other sessions' caches.
- Foci caches character files in memory — edits take effect at next compaction or restart.

## Data Sources

There are two scopes for data:
- **Global** (`~/data/` by default, configurable via `data_dir`): `api.db`, `state.db`, sessions
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
Full request/response payloads per API call. Written whenever `payload_file` is non-empty — it defaults to `logs/api-payload.jsonl`, so payload logging is **on by default**; set `payload_file = ""` in `[logging]` to disable. (A separate `full_payload` bool exists in `[logging]` but does NOT gate writing.) Large file — filter with jq, don't cat.

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

**Path:** `~/data/sessions/<AGENT_ID>/<TYPE_ID>/root.jsonl` — `<TYPE_ID>` is `c<chat-id>` for a chat session (e.g. `c5970082313`) or `i<name-or-epoch>` for an independent session. Session keys are **stable identities** (`clutch/c5970082313`, `clutch/iresearch`); compaction and /reset never change the key or the directory.

Branch files sit beside the root as `b<epoch>.jsonl` with a `branch_meta` first line. Compaction/reset archive the live file **in place** with an "archived at" stamp: `root.<STAMP>.jsonl` (e.g. `root.2026-03-04T02-30-00+0000.jsonl`, `.<N>` counter on collision) — that file holds the session's history **up to** the stamp.

**Point-in-time lookup — which file / CC session covers moment T:**

```bash
foci debug at clutch/c5970082313 2026-07-01T12:00:00Z   # RFC3339
foci debug at clutch 3h                                  # duration ago; bare agent = default session
```

Prints the JSONL path covering that moment (live file or archive, with source) and the CC resume ID observed live then. Backed by `session_archives` + `cc_resume_history` in state.db, with archive filename stamps as a state.db-independent fallback.

```bash
# Last few messages
tail -5 /path/to/root.jsonl | jq -r '.role + ": " + (.content[]? | select(.type=="text") | .text)'

# All content (not just text)
tail -5 /path/to/root.jsonl | jq -r '.role + ": " + (.content[]? | tostring)'
```

### CC Backend Transcripts (JSONL)

When foci runs on the Claude Code backend, the raw CC transcript is richer than foci's own session store above: per-block `thinking`/`text`/`tool_use`/`tool_result`, RFC3339 `timestamp`, and a thinking `signature`. Use this (not the foci store) when you need turn-level *structure* — e.g. distinguishing thinking from output, or diagnosing why a turn's text arrived oddly.

**Path:** `<foci-os-user-home>/.claude/projects/<workspace-cwd-slug>/*.jsonl`. The `.claude/` dir lives under the **foci OS user's home** (e.g. `/home/foci`), shared across all agents — NOT inside the agent's own workspace. The project *subdir* is the agent's workspace path slugified (`/` → `-`): workspace `/home/foci/clutch` → `/home/foci/.claude/projects/-home-foci-clutch/`. Most recent session = newest mtime.

```bash
# Map a turn's block structure (the key move)
tail -30 SESS.jsonl | jq -rc 'select(.type=="assistant" or .type=="user") | {ts:.timestamp, type, blocks:((.message.content // []) | if type=="array" then map(if .type=="thinking" then "think("+((.thinking|length)|tostring)+")" elif .type=="text" then "text:"+(.text[0:50]) elif .type=="tool_use" then "tool_use:"+.name else .type end) else ["str"] end)}'
```

**Gotcha:** a redacted/summarised thinking block has `thinking` length 0 but a non-empty `signature` — it's still a thinking block, just with content stripped. Don't mistake an empty thinking block for "no thinking happened." Conversely, conversational preamble before a tool call is a real `text` (output) block, not thinking — foci joins all of a turn's text blocks into one delivered message with **no separator**.

### Global SQLite Databases (`~/data/`)

| Database | Table(s) | Contents |
|---|---|---|
| `api.db` | `api_calls` | Every Anthropic API call with timestamps, cost, cache stats |
| `state.db` | `session_index`, `agent_metadata`, `chat_metadata`, `session_metadata`, `system_state`, `session_archives`, `cc_resume_history` | Unified state: session lifecycle, agent/chat/session metadata, and provenance timelines (archive rotations, CC resume-ID history) |

```bash
# Inspect any database schema
sqlite3 ~/data/state.db ".schema"

# Session awareness — all sessions with status, type, parent, last activity
sqlite3 ~/data/state.db "SELECT session_key, status, session_type, last_activity_at FROM session_index ORDER BY last_activity_at DESC LIMIT 10"

# Active sessions only
sqlite3 ~/data/state.db "SELECT session_key, session_type, last_activity_at FROM session_index WHERE status='active' ORDER BY last_activity_at DESC"

# Archive rotations for a session (when was it compacted/reset, and to which file)
sqlite3 ~/data/state.db "SELECT archived_at, reason, file_path FROM session_archives WHERE session_key='clutch/c5970082313' ORDER BY archived_at"

# CC resume-ID timeline for a session (which CC session was live when)
sqlite3 ~/data/state.db "SELECT observed_at, resume_id FROM cc_resume_history WHERE session_key='clutch/c5970082313' ORDER BY observed_at"
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
# When did compaction happen? (in-place archive; the session key is unchanged)
grep 'compacted from' ~/logs/foci.log | tail -10

# Or query the provenance table directly
sqlite3 ~/data/state.db "SELECT archived_at, reason FROM session_archives WHERE session_key='<KEY>' ORDER BY archived_at DESC LIMIT 10"

# Resets
grep 'session reset key=' ~/logs/foci.log | tail -10
```

### Background Cron Sessions
```bash
# List recent cron sessions
grep 'branch created.*cron' ~/logs/foci.log | tail -10

# Follow a specific cron session
grep '<SESSION_KEY>' ~/logs/foci.log | head -20
```
