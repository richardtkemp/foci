<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Service logs & data-source map

## Data-source scopes

- **Global** (`~/data/`, config `data_dir`): `api.db`, `state.db`, `sessions/`, `app-frames.db`
- **Per-agent** (`<workspace>/.data/`): see the table below

(API/cost → **api-cost.md**; sessions/state → **sessions.md**; app frames → **app-delivery.md**.) The schemas drift: read `.schema` before you query.

| Per-agent DB | Table(s) | Contents |
|---|---|---|
| `conversation.db` | `messages` | Chat send/receive log, all transports (NOT session history) |
| `todo.db` | `todos` | Todo items |
| `tasklist.db` | `tasklist` | Task list items |
| `scratchpad.db` | `scratchpad` | Working notes |
| `reminders.db` | `reminders` | Scheduled reminders |
| `tool_details.db` | `tool_call_details` | Tool-call display data for Telegram inline buttons |
| `memory.db` | `memory_fts`, `memory_meta` | FTS5 index |
| `search.bleve/` | — | Bleve search index |

**Reading `messages`.** Empty `user_id` = non-human sender (cron `foci send`, keepalive); filtering on `user_id`/`username` hides every injected prompt. Correlate recv→sent by `session`, never by time window: a shared chat carries concurrent peers. **A missing `sent` row does mean no chat text was emitted** (every delivery path wraps the conversation logger). Exception: a turn whose whole output is a stripped sentinel (`HEARTBEAT_OK`) writes nothing. Confirm from the turn's log lines.

**Timestamps are `2026-08-26T13:12:03+01:00`** (`T` separator, offset) in foci's SQLite DBs. A space-separated bound (`ts >= '2026-08-26 07:45'`) matches nothing and looks like "no rows". Re-run unbounded before you believe a zero.

**Sinks log their own decisions.** When output reached the user but nothing was recorded (or the reverse), grep the turn window for `turn-sink sink=0x…`: it prints each event's type, phase, length and silent verdict. This outranks inference from code.

## Service logs

`~/logs/foci.log` (also in the journal, `journalctl -u foci`). Rotated daily; gzipped archives in `~/logs/archive/`.

```bash
awk '$2=="WARN" || $2=="ERROR"' ~/logs/foci.log | tail -20     # filter the level column, not grep ERROR
awk '$1>="<ts1>" && $1<="<ts2>" && $2!="DEBUG"' ~/logs/foci.log  # time slice: drop DEBUG keepalive noise (~50 lines/min)
grep '<SESSION_KEY>' ~/logs/foci.log | grep -E 'branch created|turn_lock' | tail -20
zgrep -hE "possible zombie|did not exit after" ~/logs/archive/foci-*.gz | awk '{print $1}' | sort | tail   # last occurrence ever
```

**Gotchas:**
- **Archives are named `foci-<START>--<END>.log.gz`, by their START.** An event at T is in the file whose range contains T, normally one that starts the previous day. `ls foci-<the-date-you-want>*` returns nothing and looks like deleted data. List `~/logs/archive/` and pick, or `zgrep` all `foci-*.gz`. After a restart the live log starts at the restart; everything earlier is archived.
- **A bare `grep -r` over `~/logs/` returns 0 on the `.gz` archives.** A zero is a tooling false-negative, not evidence. Use `zgrep`/`zcat`.
- **A Go crash is not always a `panic:`.** Runtime fatals print `fatal error:` at column 0 with no `panic:` line (stack overflow, OOM). Search both. The dump goes to the **journal**, not `foci.log`.

## Crash vs clean restart

**The systemd journal is authoritative:** `journalctl -u foci | grep -E 'Deactivated successfully|Stopped|Failed|signal|killed' | tail`
- `Deactivated successfully` / `Stopped` = clean stop (deploy/restart).
- `Result=signal` / `killed` / `Failed` = crash.
- `status=2/INVALIDARGUMENT` = a **Go crash** (exit 2 = unrecovered panic/fatal; systemd only labels code 2 that way).

foci's own startup label (`internal/startup/diagnosis.go`, from proof-of-life timestamps) **can misfire**: a long-idle clean process looks like a crash once the gap exceeds its window. Never trust it alone.

**Order** for "why did it restart": 1. `uptime` (host reboot?) 2. foci binary mtime (`stat -c %y $(which foci)`: deploy?) 3. journal stop reason 4. foci's startup label, last and cross-checked.

**Goroutine count:** a step-up on an active session *plateaus* until the driving activity ends (a live quiz, a build). That is not a leak.
