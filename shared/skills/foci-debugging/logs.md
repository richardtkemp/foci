<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Service logs & data-source map

## Data-source scopes

Two scopes for on-disk data:
- **Global** (`~/data/` by default, configurable via `data_dir`): `api.db`, `state.db`, `sessions/`
- **Per-agent** (`<workspace>/.data/`): `conversation.db`, `todo.db`, `scratchpad.db`, `reminders.db`, `tasklist.db`, `memory.db` + `search.bleve`, `tool_details.db`

(API/cost DBs → **api-cost.md**; session/state DBs → **sessions.md**; app frame store → **app-delivery.md**.)

Whichever database you land on, read its shape before querying it — the schemas below drift:

```bash
sqlite3 ~/data/state.db ".schema"        # or .schema <table> for one table
```

### Per-Agent SQLite Databases (`<workspace>/.data/`)

| Database | Table(s) | Contents |
|---|---|---|
| `conversation.db` | `messages` | Chat send/receive log, all transports (NOT session history) |
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

**Reading `messages`.** Empty `user_id` = non-human sender (cron `foci send`, keepalive); filtering on `user_id`/`username` hides every injected prompt. Scope recv→sent correlation by `session`, never a time window: a shared chat carries concurrent peers. **A missing `sent` row does not mean the turn stayed silent** — only some delivery paths write one; read the turn's log lines, not the table.

**Timestamps are `2026-08-26T13:12:03+01:00`** (`T` separator, offset) in foci's SQLite DBs. A space-separated bound (`ts >= '2026-08-26 07:45'`) matches nothing and returns a clean empty result indistinguishable from "no rows". Re-run unbounded before believing a zero.

**Sinks log their own decisions.** When output reached the user but nothing was recorded (or vice versa), grep the turn window for `turn-sink sink=0x…`: it prints each event's type, phase, length and silent verdict — what the sink did, which outranks inferring it from the code.

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

### Searching rotated archives

Today's log only covers today. To answer "when did this last happen?" or "what
was the context around a known timestamp?", you have to reach into `~/logs/archive/`:

```bash
# Most recent occurrence of a pattern across ALL history (when did it last fire?)
zgrep -hE "possible zombie|did not exit after" ~/logs/archive/foci-*.gz | awk '{print $1}' | sort | tail

# Which archive file contains an event at a known timestamp — list ranges and pick
# by eye; do NOT glob on the date you want (see gotcha)
ls ~/logs/archive/ | grep '^foci-'

# Full context inside one archive
zcat ~/logs/archive/foci-<range>.log.gz | grep '<SESSION_KEY>'
```

**Gotchas:**
- **Archives are named `foci-<START>--<END>.log.gz`, by their START.** An event at time T lives in the file whose *range contains* T — normally one starting the previous day. So `ls foci-<the-date-you-want>*` returns nothing and reads exactly like the data was deleted. List the ranges (`ls ~/logs/archive/`) and pick, or `zgrep` across `foci-*.gz`. Same trap after a restart: the live log begins at the restart, so everything earlier is already archived.
- **Archives are `.gz`** — `zgrep`/`zcat` them. A bare `grep -r` over `~/logs/` silently returns 0 on gzipped files (it doesn't decompress), so you'll miss everything in rotated logs. A zero result from `grep -r … *.gz` is a tooling false-negative, **not evidence that the thing never occurred**.
- **Filter errors on the level column** (`awk '$2=="ERROR"'`), not a bare `grep ERROR` — the word "error" appears in plenty of non-error lines (payloads, messages), giving false positives.
- **A real panic** starts with `^panic:` at column 0.

## Crash vs clean restart

When foci restarted, was it a deploy, a crash, or a host reboot? **The systemd journal is authoritative:**

```bash
journalctl -u foci | grep -E 'Deactivated successfully|Stopped|Failed|signal|killed' | tail
```
- `Deactivated successfully` / `Stopped` = a **clean** stop (deploy/restart).
- `Result=signal` / `killed` / `Failed` = a **crash**.

foci also prints its own restart classification at startup (`internal/startup/diagnosis.go`: crash / reboot / clean, from proof-of-life timestamps — `last_startup`, `last_alive`, `last_clean_shutdown` — plus a host-uptime-vs-gap reboot check). **That label can misfire** when the silence gap exceeds foci's proof-of-life window (a long-idle clean process can look like a crash), so cross-check it against the journal rather than trusting it alone.

**Diagnostic order** for "why did it restart":
1. `uptime` — did the *host* reboot?
2. foci binary mtime (`stat -c %y $(which foci)`) — was it a **deploy**?
3. journal stop reason (above) — clean vs signal.
4. foci's own startup label — last, and only cross-checked.

**Goroutine count:** a step-up on an active session *plateaus* — it stays elevated until the driving activity ends (a live quiz, a running build). Don't predict drain while the driver is still active; it's not a leak.
