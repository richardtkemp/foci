# Foci CLI Reference

The `foci` CLI is a lightweight client that talks to the `foci-gw` HTTP gateway. It is built separately:

```
go build ./cmd/foci
```

All commands communicate over HTTP to the gateway. The CLI auto-discovers the gateway's Unix socket at `~/data/foci-gw.sock` (same-user auth via kernel peer credentials — no API key needed). Falls back to TCP at `FOCI_ADDR` (default `127.0.0.1:18791`) with `FOCI_API_KEY` for remote/cross-user access.

---

## Global Flags

These flags are accepted by all commands:

| Flag | Short | Env var | Description |
|------|-------|---------|-------------|
| `--help` | `-h` | | Show usage for any command (e.g. `foci send -h`). |
| `--socket <path>` | | `FOCI_GW_SOCK` | Gateway Unix socket path. Auto-detected from `~/data/foci-gw.sock`. No API key needed. |
| `--addr <host:port>` | | `FOCI_ADDR` | Gateway TCP address. Default: `127.0.0.1:18791`. Used when no Unix socket is available. |
| `--agent <id>` | `-a` | `FOCI_AGENT` | Target a specific agent. Default: first configured agent. |
| `--session <sel>` | `-s` | `FOCI_SESSION` | Target session selector: full session key, session name, or chat alias. Empty = the agent's default session. |
| `--model <model>` | `-m` | `FOCI_MODEL` | Model override: group name (`powerful`/`fast`/`cheap`), model name, or `developer/model_id`. See [MODELS.md](MODELS.md). |
| `--if-warm <dur>` (`--if-active`) | | `FOCI_IF_WARM` (`FOCI_IF_ACTIVE`) | **Session-level**: skip if the target session has not run a turn within duration (e.g. `8h`, `30m`). A turn currently in flight always counts as active. |
| `--if-cold <dur>` (`--if-inactive`) | | `FOCI_IF_COLD` (`FOCI_IF_INACTIVE`) | **Session-level**: skip if the target session has run a turn within duration (e.g. `30m`, `1h`). Opposite of `--if-warm`; in-flight always counts. Standard keepalive shape. |
| `--if-user-active <dur>` | | `FOCI_IF_USER_ACTIVE` | **User-attention**: skip if the user has not touched this agent within duration. In-flight counts as user attention. |
| `--if-user-inactive <dur>` | | `FOCI_IF_USER_INACTIVE` | **User-attention**: skip if the user has touched this agent within duration. In-flight counts as user attention. |
| `--message-text <text>` | `-mt` | `FOCI_MESSAGE_TEXT` | Explicit message text (alternative to trailing args). |
| `--message-file <path>` | `-mf` | `FOCI_MESSAGE_FILE` | Read message from file path. |
| `--sync` / `--wait` | | `FOCI_SYNC` | Wait for response (send/branch only, non-empty = true). |
| `--async` / `--no-wait` | | `FOCI_ASYNC` | Fire-and-forget (send/branch default, non-empty = true). |
| `--no-compact` | | `FOCI_NO_COMPACT` | Skip compaction (branch only, non-empty = true). |
| `--no-reset-hook` | | `FOCI_NO_RESET_HOOK` | Skip reset hook (branch only, non-empty = true). |
| `--oneshot` | | `FOCI_ONESHOT` | No compaction + no reset hook (branch only, non-empty = true). |
| `--wait-warm <dur>` | | `FOCI_WAIT_WARM` | **Send-only**: wait (block) until the target session has run a turn within duration before sending. Session-level. |
| `--wait-cold <dur>` | | `FOCI_WAIT_COLD` | **Send-only**: wait (block) until the target session has not run a turn within duration before sending. Opposite of `--wait-warm`. |
| `--wait-user-active <dur>` | | `FOCI_WAIT_USER_ACTIVE` | **Send-only**: wait (block) until the user has touched this agent within duration before sending. User-attention. |
| `--wait-user-inactive <dur>` | | `FOCI_WAIT_USER_INACTIVE` | **Send-only**: wait (block) until the user has not touched this agent within duration before sending. Opposite of `--wait-user-active`. |
| `--wait-timeout <dur>` | | `FOCI_WAIT_TIMEOUT` | **Send-only**: max time to wait for a `--wait-*` gate to be satisfied. Default `0` = no limit. |
| `--no-gate` | | `FOCI_NO_GATE` | **Send-only**: disable all gate checks (ignore `--if-*` / `--wait-*` / `FOCI_IF_*` / `FOCI_WAIT_*`). |

**Resolution order:** explicit flag > env var > default. Every flag has a corresponding `FOCI_` env var and vice versa.

## Environment Variables

Setting env vars is useful for crontab entries where the same agent/session is targeted repeatedly:

```crontab
FOCI_AGENT=clutch
FOCI_IF_WARM=4h
*/30 * * * * foci send -mf /home/foci/shared/prompts/reflection.md
0 7 * * * foci branch --oneshot -mf /home/foci/shared/prompts/morning-routine.md
```

---

## Commands

### `send` — Send a message to the agent

Sends a text message to the agent's default session (or a named session / chat alias / exact session key). By default, send is **asynchronous** (fire-and-forget): the CLI returns immediately with "queued" and the agent's response is delivered to the chat. Use `--sync`/`--wait` to block until the response is available.

`--broadcast` delivers the agent's response to **every surface** for the agent — each platform's default destination (telegram default chat, the app's default conversation, else its newest — auto-created if the agent has none) — instead of only the target session's chat. Useful for "heads up" crons that must reach you wherever you are. Equivalent to `policy=broadcast` in the request (or embedded in the selector: `-s 'research?policy=broadcast'`).

Every response carries a **routing receipt** — which session the message resolved to and which resolution rung matched (`exact`, `named`, `alias`, `created`, `default`). The CLI prints it to stderr (`session: clutch/iresearch (named)`), so cron logs show where a send actually landed instead of trusting silent fallbacks.

**Usage:**
```
foci send [-a agent] [-s session] [-m model] [--if-warm <duration>] [--if-cold <duration>] [--if-user-active <duration>] [--if-user-inactive <duration>] [--sync] [-mt text | -mf file] [message text]
```

**Flags:**

| Flag | Short | Description |
|------|-------|-------------|
| `--agent <id>` | `-a` | Target agent. |
| `--session <sel>` | `-s` | Target session selector, resolved through one ladder on the server: full session key (`clutch/c123`) → existing named session (`research` → `clutch/iresearch`) → chat alias → create-named. Empty = the agent's default session. |
| `--model <model>` | `-m` | Model override for this request. Group name (`powerful`/`fast`/`cheap`), model name (`opus`), or `developer/model_id`. |
| `--if-warm <dur>` (`--if-active`) | | **Session-level gate**: skip if the target session has not run a turn within duration. Go duration format (e.g. `8h`, `30m`). A turn currently in flight always counts as active. |
| `--if-cold <dur>` (`--if-inactive`) | | **Session-level gate**: skip if the target session has run a turn within duration. Opposite of `--if-warm` — keepalive shape; in-flight always counts. |
| `--if-user-active <dur>` | | **User-attention gate**: skip if the user has not touched this agent within duration. CLI/cron/agent-to-agent traffic does not count. |
| `--if-user-inactive <dur>` | | **User-attention gate**: skip if the user has touched this agent within duration. |
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
foci send -mf /home/foci/shared/prompts/reflection.md

# Send to a specific agent
foci send -a research "summarize today's news"

# Send to a named session
foci send -a clutch -s research "continue the analysis"

# Only send if the session ran a turn in the last 8 hours (for cron jobs)
foci send --if-warm 8h "daily health check"

# Only send if the user has touched this agent in the last 4 hours
foci send --if-user-active 4h "follow-up question"

# Send with a model override (use fast group model)
foci send --model fast "quick question"

# Send with a specific model name
foci send -m opus "think carefully about this"

# Send file contents with session-level activity gating
foci send -a clutch --if-warm 4h -mf tasks/review.md
```

**Exit codes:** 0 on success, 1 on error (network failure, HTTP error).

---

### `branch` — Fork a branch session

Creates a branch session from the agent's default chat session, optionally injects a message, and runs the agent on the branch. Used for cron jobs and background tasks that shouldn't pollute the main conversation.

By default, branch is **asynchronous** (fire-and-forget): the CLI returns immediately with "queued" and the agent's response is delivered to Telegram. Use `--sync`/`--wait` to block until the response is available.

**Usage:**
```
foci branch [-a agent] [-m model] [--if-warm <duration>] [--if-cold <duration>] [--if-user-active <duration>] [--if-user-inactive <duration>] [--no-compact] [--no-reset-hook] [--oneshot] [--sync] [-mt text | -mf file] [text]
```

**Flags:**

| Flag | Description |
|------|-------------|
| `--agent <id>` / `-a` | Target agent. |
| `--model <model>` / `-m` | Model override for this branch. Group name, model name, or `developer/model_id`. |
| `--if-warm <dur>` (`--if-active`) | **Session-level gate**: skip if the target session has not run a turn within duration. In-flight counts. |
| `--if-cold <dur>` (`--if-inactive`) | **Session-level gate**: skip if the target session has run a turn within duration. In-flight counts. Keepalive shape. |
| `--if-user-active <dur>` | **User-attention gate**: skip if the user has not touched this agent within duration. |
| `--if-user-inactive <dur>` | **User-attention gate**: skip if the user has touched this agent within duration. |
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

# Branch with a cheap model for routine work
foci branch --oneshot -m cheap -a clutch "run health check"

# Branch with message from file
foci branch --oneshot -a scout -mf /home/foci/shared/prompts/daily-health-check.md

# Only branch if the session has been active recently (in-flight counts)
foci branch --if-warm 12h -a clutch "daily memory review"

# Only branch if the user has reached out recently
foci branch --if-user-active 4h -a clutch "follow-up on this morning's chat"

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
foci command [-a agent] [--if-warm <dur>] [--if-cold <dur>] [--if-user-active <dur>] [--if-user-inactive <dur>] </cmd> [args]
```

**Activity gates:** `command` accepts the same four activity-gate flags as `send` (and reads the same `FOCI_IF_*` env vars). The gate is evaluated server-side against the session the command targets; a turn in flight always counts as active, so a gated command never interrupts mid-turn work. This is what makes an unattended `/reset` safe — see the example below.

**Examples:**
```bash
foci command /cache
foci command -a research /status
foci command /config available
foci command /cost today

# Overnight reset that skips if the session ran a turn within 55m (or one is
# in flight) — the shape used by an unattended reset cron:
foci command --if-cold 55m -a helen /reset
```

---

### `secrets` — Manage secrets

Manage secrets in `secrets.toml` without a running gateway. Useful for initial setup, scripting, and CI.

**Usage:**
```
foci secrets <subcommand> [--config <path>] [args...]
```

**Subcommands:**

| Subcommand | Description |
|-----------|-------------|
| `list` | List all secret names (no values printed) |
| `get <section.key>` | Print secret value to stdout (pipe-friendly, no decoration) |
| `set <section.key> <value>` | Add or update a secret |
| `delete <section.key>` | Remove a secret |

**Flags:**
- `--config` — path to `foci.toml` (secrets.toml is resolved alongside it). Default: `~/config/secrets.toml`.

**Examples:**
```bash
# List all secret names
foci secrets list

# Set a secret
foci secrets set custom.github_token ghp_abc123

# Read a secret (pipe-friendly)
foci secrets get custom.github_token | pbcopy

# Delete a secret
foci secrets delete custom.github_token

# Use a custom config directory
foci secrets --config /etc/foci/foci.toml list
```

---

### `auth` — Set API key for an LLM provider

Save an API key to `secrets.toml`. If a gateway is running, the new credentials are hot-reloaded immediately.

**Usage:**
```
foci auth [--provider NAME] [--api-key KEY] [--config PATH] [--addr HOST:PORT]
```

**Flags:**
- `--provider` — provider name: `anthropic`, `gemini`, `openai`, `openrouter` (default: `anthropic`).
- `--api-key` — API key (prompted interactively if omitted).
- `--config` — path to foci.toml (secrets.toml is written alongside it). Default: `~/config/secrets.toml`.
- `--addr` — gateway address for hot-reload notification. Env: `FOCI_ADDR`. Default: `127.0.0.1:18791`.

**Examples:**
```bash
foci auth                                          # interactive: prompts for API key
foci auth --provider openai --api-key sk-...       # non-interactive
foci auth --config /etc/foci/foci.toml             # custom config directory
foci auth --addr 10.0.0.1:18791      # notify remote gateway
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

### `debug` — Tail session files

Tails the session files for a given session key with formatted output. Useful for live debugging of what an agent is writing to its session JSONL.

**Usage:**
```
foci debug session <key>
```

**Examples:**
```bash
foci debug session clutch/c12345
```

---

### `pair` — Mint Android pairing key

Mints a single-use Android pairing key for registering a new device with the gateway. The key can be used once to complete the `/android` onboarding flow.

**Usage:**
```
foci pair
```

---

## Cron Integration

The CLI is designed for cron jobs. Both `send` and `branch` default to async mode — the cron job returns immediately and the agent's response is delivered to Telegram. Typical patterns:

```crontab
# Daily morning routine — only if the user has reached out in the last 24h
0 7 * * * /home/foci/bin/foci branch --if-user-active 24h -a clutch "run your morning routine"

# Hourly health check — only if the user has been around recently
0 * * * * /home/foci/bin/foci send --if-user-active 8h -a clutch "quick health check"

# Nightly one-shot task (no compaction overhead)
0 2 * * * /home/foci/bin/foci branch --oneshot -a clutch "nightly cleanup"

# Keepalive — only if the session has been idle for 30+ minutes (yields to in-flight turns and recent work)
*/30 * * * * /home/foci/bin/foci branch --oneshot --if-cold 30m -a clutch "Check emails and calendar"

# Force sync if you need the output in the cron log
0 6 * * * /home/foci/bin/foci send --sync -a clutch "morning report" >> /var/log/foci-report.log
```

**Choosing the right gate:**

- `--if-user-active` / `--if-user-inactive` track *user attention* (real platform inbound — Telegram/Discord). Use these for nudges that should only fire when the user is engaged or specifically away.
- `--if-warm` (`--if-active`) / `--if-cold` (`--if-inactive`) track *session activity* — whether any turn (user, cron, CLI, agent-to-agent) ran on the session, plus an in-flight short-circuit so a turn currently running always counts as active. Use these for keepalives that must yield to running work — they prevent crons piling up behind a long turn.

## HTTP API

The CLI is a thin wrapper around the HTTP gateway. For direct integration, see the endpoint documentation in [WIRING.md](WIRING.md#http-gateway-maingo).
