<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Service logs & data-source map

## Data-source scopes

Two scopes for on-disk data:
- **Global** (`~/data/` by default, configurable via `data_dir`): `api.db`, `state.db`, `sessions/`
- **Per-agent** (`<workspace>/.data/`): `conversation.db`, `todo.db`, `scratchpad.db`, `reminders.db`, `tasklist.db`, `memory.db` + `search.bleve`, `tool_details.db`

(API/cost DBs → **api-cost.md**; session/state DBs → **sessions.md**.)

### Per-Agent SQLite Databases (`<workspace>/.data/`)

| Database | Table(s) | Contents |
|---|---|---|
| `conversation.db` | `messages` | Telegram send/receive log (NOT session history) |
| `todo.db` | `todos` | Todo items |
| `tasklist.db` | `tasklist` | Task list items |
| `scratchpad.db` | `scratchpad` | Working notes |
| `reminders.db` | `reminders` | Scheduled reminders |
| `tool_details.db` | `tool_call_details` | Tool-call display data for Telegram inline buttons |
| `memory.db` | `memory_fts`, `memory_meta` | FTS5 full-text search index |
| `search.bleve/` | — | Bleve search index |

```bash
# Example: conversation log (replace <workspace> with the agent's workspace path)
sqlite3 <workspace>/.data/conversation.db "SELECT * FROM messages ORDER BY rowid DESC LIMIT 5"
```

## Service logs

The foci service log is `~/logs/foci.log` (also on the systemd journal). Rotated daily; archives are gzipped under `~/logs/archive/`.

```bash
# Recent foci logs (journal)
journalctl -u foci --since "1 hour ago" --no-pager

# Warnings and errors — use awk on the level column, not a bare grep (see gotcha)
awk '$2=="WARN" || $2=="ERROR"' ~/logs/foci.log | tail -20

# Compaction events
grep 'compact' ~/logs/foci.log | tail -20

# Session lifecycle for one session
grep '<SESSION_KEY>' ~/logs/foci.log | grep -E 'branch created|turn_lock' | tail -20
```

**Gotchas:**
- **Archives are `.gz`** — `zgrep`/`zcat` them. A bare `grep -r` over `~/logs/` silently returns 0 on gzipped files (it doesn't decompress), so you'll miss everything in rotated logs.
- **Filter errors on the level column** (`awk '$2=="ERROR"'`), not a bare `grep ERROR` — the word "error" appears in plenty of non-error lines (payloads, messages), giving false positives.
- **A real panic** starts with `^panic:` at column 0.
