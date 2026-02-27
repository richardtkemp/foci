# Foci CLI Reference

The `foci` CLI is a lightweight client that talks to the `focigw` HTTP gateway. It is built separately:

```
go build ./cmd/foci
```

All commands communicate over HTTP to the gateway at `FOCI_ADDR` (default `127.0.0.1:18791`).

---

## Global Flags

These flags are accepted by all commands:

| Flag | Short | Env var | Description |
|------|-------|---------|-------------|
| `--help` | `-h` | | Show usage for any command (e.g. `foci send -h`). |
| `--addr <host:port>` | | `FOCI_ADDR` | Gateway address. Default: `127.0.0.1:18791`. |
| `--agent <id>` | `-a` | `FOCI_AGENT` | Target a specific agent. Default: first configured agent. |
| `--session <id>` | `-s` | `FOCI_SESSION` | Target session type. Default: `main`. |
| `--if-active <dur>` | | `FOCI_IF_ACTIVE` | Skip if no user activity within duration (e.g. `8h`, `30m`). |
| `--if-inactive <dur>` | | `FOCI_IF_INACTIVE` | Skip if user was active within duration (e.g. `30m`, `1h`). Opposite of `--if-active`. |
| `--message-text <text>` | `-mt` | `FOCI_MESSAGE_TEXT` | Explicit message text (alternative to trailing args). |
| `--message-file <path>` | `-mf` | `FOCI_MESSAGE_FILE` | Read message from file path. |
| `--sync` / `--wait` | | `FOCI_SYNC` | Wait for response (send/branch only, non-empty = true). |
| `--async` / `--no-wait` | | `FOCI_ASYNC` | Fire-and-forget (send/branch default, non-empty = true). |
| `--no-compact` | | `FOCI_NO_COMPACT` | Skip compaction (branch only, non-empty = true). |
| `--no-reset-hook` | | `FOCI_NO_RESET_HOOK` | Skip reset hook (branch only, non-empty = true). |
| `--oneshot` | | `FOCI_ONESHOT` | No compaction + no reset hook (branch only, non-empty = true). |

**Resolution order:** explicit flag > env var > default. Every flag has a corresponding `FOCI_` env var and vice versa.

## Environment Variables

Setting env vars is useful for crontab entries where the same agent/session is targeted repeatedly:

```crontab
FOCI_AGENT=clutch
FOCI_IF_ACTIVE=4h
*/30 * * * * foci send -mf /home/foci/shared/prompts/memory-formation.md
0 7 * * * foci branch --oneshot -mf /home/foci/shared/prompts/morning-routine.md
```

---

## Commands

### `send` — Send a message to the agent

Sends a text message to the agent's default session (or a named session). By default, send is **asynchronous** (fire-and-forget): the CLI returns immediately with "queued" and the agent's response is delivered to Telegram. Use `--sync`/`--wait` to block until the response is available.

**Usage:**
```
foci send [-a agent] [-s session] [--if-active <duration>] [--if-inactive <duration>] [--sync] [-mt text | -mf file] [message text]
```

**Flags:**

| Flag | Short | Description |
|------|-------|-------------|
| `--agent <id>` | `-a` | Target agent. |
| `--session <id>` | `-s` | Target session type (e.g. `main`, `research`). Produces session key `agent:<id>:<session>`. Default: `main`. |
| `--if-active <dur>` | | Skip if no real Telegram user activity within duration. Go duration format (e.g. `8h`, `30m`). |
| `--if-inactive <dur>` | | Skip if user was active within duration. Opposite of `--if-active` — for heartbeats that should only fire when idle. |
| `--sync` / `--wait` | | Wait for the agent's response instead of returning immediately. |
| `--async` / `--no-wait` | | Fire-and-forget mode (default). Returns immediately, response goes to Telegram. |
| `--message-text <text>` | `-mt` | Explicit message text (alternative to trailing args). |
| `--message-file <path>` | `-mf` | Read message from file. Sends the file contents as the message. |

Trailing args without a flag are treated as implicit `--message-text`. Cannot use both `-mt` and `-mf`.

**Examples:**
```bash
# Send a message (async, returns immediately with "queued")
foci send "check the weather forecast"

# Send and wait for the response
foci send --sync "check the weather forecast"

# Equivalent using explicit flag
foci send -mt "check the weather forecast"

# Send file contents as the message
foci send -mf /home/foci/shared/prompts/memory-formation.md

# Send to a specific agent
foci send -a research "summarize today's news"

# Send to a named session
foci send -a clutch -s research "continue the analysis"

# Only send if user was active in the last 8 hours (for cron jobs)
foci send --if-active 8h "daily health check"

# Send file contents with activity gating
foci send -a clutch --if-active 4h -mf tasks/review.md
```

**Exit codes:** 0 on success, 1 on error (network failure, HTTP error).

---

### `branch` — Fork a branch session

Creates a branch session from the agent's default chat session, optionally injects a message, and runs the agent on the branch. Used for cron jobs and background tasks that shouldn't pollute the main conversation.

By default, branch is **asynchronous** (fire-and-forget): the CLI returns immediately with "queued" and the agent's response is delivered to Telegram. Use `--sync`/`--wait` to block until the response is available.

Aliased as `wake` for backward compatibility.

**Usage:**
```
foci branch [-a agent] [--if-active <duration>] [--if-inactive <duration>] [--no-compact] [--no-reset-hook] [--oneshot] [--sync] [-mt text | -mf file] [text]
```

**Flags:**

| Flag | Description |
|------|-------------|
| `--agent <id>` / `-a` | Target agent. |
| `--if-active <dur>` | Skip if no real user activity within duration. |
| `--if-inactive <dur>` | Skip if user was active within duration. For heartbeats that should only fire when idle. |
| `--sync` / `--wait` | Wait for the agent's response instead of returning immediately. |
| `--async` / `--no-wait` | Fire-and-forget mode (default). Returns immediately, response goes to Telegram. |
| `--no-compact` | Skip compaction if context limit is reached during the branch. |
| `--no-reset-hook` | Skip the pre-reset memory hook when the branch session is reclaimed. |
| `--oneshot` | Shorthand for `--no-compact --no-reset-hook`. For quick fire-and-forget tasks. |
| `--message-text <text>` / `-mt` | Explicit message text (alternative to trailing args). |
| `--message-file <path>` / `-mf` | Read message from file. Sends the file contents as the message. |

**Examples:**
```bash
# Fork a branch for a background task (returns immediately)
foci branch -a clutch "run your morning routine"

# Fork and wait for the response
foci branch --sync -a clutch "run your morning routine"

# Quick one-shot task (no compaction, no reset hook)
foci branch --oneshot -a clutch "check disk space and report"

# Branch with message from file
foci branch --oneshot -a scout -mf /home/foci/shared/prompts/daily-health-check.md

# Only branch if user was active recently
foci branch --if-active 12h -a clutch "daily memory review"

# Empty branch (agent wakes up with fork context only)
foci branch -a research
```

**Exit codes:** 0 on success, 1 on error. Returns HTTP 412 if the agent has no default session yet (no Telegram chat has been started).

---

### `status` — Query agent status

Returns the agent's current status including session info, model, uptime, and whether the agent is processing.

**Usage:**
```
foci status [-a agent]
```

**Examples:**
```bash
foci status
foci status -a research
```

---

### `eval` — Run a shell command via the agent

Wraps a shell command in a prompt asking the agent to execute it and show the output. The command is sent as a message to the agent's main session. Always synchronous (waits for the response).

**Usage:**
```
foci eval [-a agent] <shell command>
```

**Examples:**
```bash
foci eval "df -h"
foci eval -a research "git log --oneline -5"
```

Note: The agent must have the `exec` tool available to actually run the command.

---

### `command` — Dispatch a slash command

Dispatches a slash command directly via the HTTP API and returns the result to the CLI caller. The leading `/` is added automatically if omitted.

**Important:** This bypasses the agent conversation entirely. The agent never sees the command or its output — the command is executed by the gateway's command handler and the result is returned only to the CLI. This is useful for observability and diagnostics without polluting the chat context.

**Usage:**
```
foci command [-a agent] </cmd> [args]
```

**Examples:**
```bash
foci command /cache
foci command -a research /status
foci command /config available
foci command /cost today
```

---

### `ping` — Liveness check

Shorthand for `foci command /ping`. Returns "pong" with a timestamp if the gateway and agent are running.

**Usage:**
```
foci ping [-a agent]
```

**Examples:**
```bash
foci ping
foci ping -a research
```

---

## Cron Integration

The CLI is designed for cron jobs. Both `send` and `branch` default to async mode — the cron job returns immediately and the agent's response is delivered to Telegram. Typical patterns:

```crontab
# Daily morning routine (returns instantly, response goes to Telegram)
0 7 * * * /home/foci/bin/foci branch --if-active 24h -a clutch "run your morning routine"

# Hourly health check (only if user is around)
0 * * * * /home/foci/bin/foci send --if-active 8h -a clutch "quick health check"

# Nightly one-shot task (no compaction overhead)
0 2 * * * /home/foci/bin/foci branch --oneshot -a clutch "nightly cleanup"

# Heartbeat — only if idle for 30+ minutes (don't interrupt active conversations)
*/30 * * * * /home/foci/bin/foci branch --oneshot --if-inactive 30m -a clutch "Check emails and calendar"

# Force sync if you need the output in the cron log
0 6 * * * /home/foci/bin/foci send --sync -a clutch "morning report" >> /var/log/foci-report.log
```

The `--if-active` flag prevents cron jobs from running when the user hasn't interacted recently. The `--if-inactive` flag is the opposite — it skips when the user IS active, useful for heartbeat-style tasks that shouldn't interrupt conversations.

## HTTP API

The CLI is a thin wrapper around the HTTP gateway. For direct integration, see the endpoint documentation in [WIRING.md](WIRING.md#http-gateway-maingo).
