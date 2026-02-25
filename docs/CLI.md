# Clod CLI Reference

The `clod` CLI is a lightweight client that talks to the `clodgw` HTTP gateway. It is built separately:

```
go build ./cmd/clod
```

All commands communicate over HTTP to the gateway at `CLOD_ADDR` (default `127.0.0.1:18791`).

---

## Global Flags

These flags are accepted by all commands:

| Flag | Short | Env var | Description |
|------|-------|---------|-------------|
| `--addr <host:port>` | | `CLOD_ADDR` | Gateway address. Default: `127.0.0.1:18791`. |
| `--agent <id>` | `-a` | `CLOD_AGENT` | Target a specific agent. Default: first configured agent. |
| `--session <id>` | `-s` | `CLOD_SESSION` | Target session type. Default: `main`. |
| `--if-active <dur>` | | `CLOD_IF_ACTIVE` | Skip if no user activity within duration (e.g. `8h`, `30m`). |
| `--message-text <text>` | `-mt` | `CLOD_MESSAGE_TEXT` | Explicit message text (alternative to trailing args). |
| `--message-file <path>` | `-mf` | `CLOD_MESSAGE_FILE` | Read message from file path. |
| `--no-compact` | | `CLOD_NO_COMPACT` | Skip compaction (branch only, non-empty = true). |
| `--no-reset-hook` | | `CLOD_NO_RESET_HOOK` | Skip reset hook (branch only, non-empty = true). |
| `--oneshot` | | `CLOD_ONESHOT` | No compaction + no reset hook (branch only, non-empty = true). |

**Resolution order:** explicit flag > env var > default. Every flag has a corresponding `CLOD_` env var and vice versa.

## Environment Variables

Setting env vars is useful for crontab entries where the same agent/session is targeted repeatedly:

```crontab
CLOD_AGENT=clutch
CLOD_IF_ACTIVE=4h
*/30 * * * * clod send -mf /home/clod/shared/prompts/memory-formation.md
0 7 * * * clod branch --oneshot -mf /home/clod/shared/prompts/morning-routine.md
```

---

## Commands

### `send` — Send a message to the agent

Sends a text message to the agent's default session (or a named session). The message is queued and processed asynchronously — the CLI returns immediately after the HTTP request completes.

**Usage:**
```
clod send [-a agent] [-s session] [--if-active <duration>] [-mt text | -mf file] [message text]
```

**Flags:**

| Flag | Short | Description |
|------|-------|-------------|
| `--agent <id>` | `-a` | Target agent. |
| `--session <id>` | `-s` | Target session type (e.g. `main`, `research`). Produces session key `agent:<id>:<session>`. Default: `main`. |
| `--if-active <dur>` | | Skip if no real Telegram user activity within duration. Go duration format (e.g. `8h`, `30m`). |
| `--message-text <text>` | `-mt` | Explicit message text (alternative to trailing args). |
| `--message-file <path>` | `-mf` | Read message from file. Sends the file contents as the message. |

Trailing args without a flag are treated as implicit `--message-text`. Cannot use both `-mt` and `-mf`.

**Examples:**
```bash
# Send a message to the default agent's main session
clod send "check the weather forecast"

# Equivalent using explicit flag
clod send -mt "check the weather forecast"

# Send file contents as the message
clod send -mf /home/clod/shared/prompts/memory-formation.md

# Send to a specific agent
clod send -a research "summarize today's news"

# Send to a named session
clod send -a clutch -s research "continue the analysis"

# Only send if user was active in the last 8 hours (for cron jobs)
clod send --if-active 8h "daily health check"

# Send file contents with activity gating
clod send -a clutch --if-active 4h -mf tasks/review.md
```

**Exit codes:** 0 on success, 1 on error (network failure, HTTP error).

---

### `branch` — Fork a branch session

Creates a branch session from the agent's default chat session, optionally injects a message, and runs the agent on the branch. Used for cron jobs and background tasks that shouldn't pollute the main conversation.

Aliased as `wake` for backward compatibility.

**Usage:**
```
clod branch [-a agent] [--if-active <duration>] [--no-compact] [--no-reset-hook] [--oneshot] [-mt text | -mf file] [text]
```

**Flags:**

| Flag | Description |
|------|-------------|
| `--agent <id>` / `-a` | Target agent. |
| `--if-active <dur>` | Skip if no real user activity within duration. |
| `--no-compact` | Skip compaction if context limit is reached during the branch. |
| `--no-reset-hook` | Skip the pre-reset memory hook when the branch session is reclaimed. |
| `--oneshot` | Shorthand for `--no-compact --no-reset-hook`. For quick fire-and-forget tasks. |
| `--message-text <text>` / `-mt` | Explicit message text (alternative to trailing args). |
| `--message-file <path>` / `-mf` | Read message from file. Sends the file contents as the message. |

**Examples:**
```bash
# Fork a branch for a background task
clod branch -a clutch "run your morning routine"

# Quick one-shot task (no compaction, no reset hook)
clod branch --oneshot -a clutch "check disk space and report"

# Branch with message from file
clod branch --oneshot -a scout -mf /home/clod/shared/prompts/daily-health-check.md

# Only branch if user was active recently
clod branch --if-active 12h -a clutch "daily memory review"

# Empty branch (agent wakes up with fork context only)
clod branch -a research
```

**Exit codes:** 0 on success, 1 on error. Returns HTTP 412 if the agent has no default session yet (no Telegram chat has been started).

---

### `status` — Query agent status

Returns the agent's current status including session info, model, uptime, and whether the agent is processing.

**Usage:**
```
clod status [-a agent]
```

**Examples:**
```bash
clod status
clod status -a research
```

---

### `eval` — Run a shell command via the agent

Wraps a shell command in a prompt asking the agent to execute it and show the output. The command is sent as a message to the agent's main session.

**Usage:**
```
clod eval [-a agent] <shell command>
```

**Examples:**
```bash
clod eval "df -h"
clod eval -a research "git log --oneline -5"
```

Note: The agent must have the `exec` tool available to actually run the command.

---

### `command` — Dispatch a slash command

Sends a slash command to the agent. The leading `/` is added automatically if omitted.

**Usage:**
```
clod command [-a agent] </cmd> [args]
```

**Examples:**
```bash
clod command /cache
clod command -a research /status
clod command /config available
```

---

### `ping` — Liveness check

Shorthand for `clod command /ping`. Returns "pong" with a timestamp if the gateway and agent are running.

**Usage:**
```
clod ping [-a agent]
```

**Examples:**
```bash
clod ping
clod ping -a research
```

---

## Cron Integration

The CLI is designed for cron jobs. Typical patterns:

```crontab
# Daily morning routine (only if user was active yesterday)
0 7 * * * /home/clod/bin/clod branch --if-active 24h -a clutch "run your morning routine"

# Hourly health check (only if user is around)
0 * * * * /home/clod/bin/clod send --if-active 8h -a clutch "quick health check"

# Nightly one-shot task (no compaction overhead)
0 2 * * * /home/clod/bin/clod branch --oneshot -a clutch "nightly cleanup"
```

The `--if-active` flag prevents cron jobs from running when the user hasn't interacted recently, saving API tokens and avoiding pointless background work.

## HTTP API

The CLI is a thin wrapper around the HTTP gateway. For direct integration, see the endpoint documentation in [WIRING.md](WIRING.md#http-gateway-maingo).
