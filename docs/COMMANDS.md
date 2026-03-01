# Slash Commands Reference

Messages starting with `/` are intercepted at the Telegram router level before reaching the agent — they execute immediately and never enter session history.

All registered slash commands are also available to the agent as tools (without the `/` prefix), except those marked **CLI-only** below. See [TOOLS.md](TOOLS.md) for details.

---

## Observability

### `/status`
Dashboard overview — session info, uptime, model, context usage, cost.

### `/cache`
Last 5 API calls with cache token breakdown (input, output, cache read, cache write).

### `/last`
Details of the last API request.

### `/cost`
Cumulative API cost summary.

### `/context`
Context window breakdown — system prompt size, conversation size, compaction status.

### `/mana`
Check current mana/quota remaining. The command name is configurable via `mana_command_name` in config (e.g. `/juice`, `/credits`). `/usage` is a hidden alias.

### `/todo [search <query> | all]`
List open todo items.
- `/todo` — show open items (excludes background-tagged)
- `/todo all` — show all open items including background-tagged
- `/todo search <query>` — search todos by text

---

## Operations

### `/reset`
Clear session history. Fires session-end memory formation (async) before clearing.

### `/model [name]`
Show or switch the model for the current session.
- `/model` — show current model
- `/model haiku` — switch to haiku (supports aliases from `[models.aliases]` config)

### `/effort [level]`
Show or set the effort/budget level.
- `/effort` — show current level
- `/effort low` — set to low (alias: `1`)
- `/effort medium` — set to medium (alias: `2`)
- `/effort high` — set to high (alias: `3`)
- `/effort none` or `/effort off` — clear override

### `/thinking [mode]`
Show or set extended thinking mode.
- `/thinking` — show current mode
- `/thinking off` — disable (alias: `0`)
- `/thinking adaptive` — enable adaptive thinking (alias: `1`)

### `/voice`
Toggle voice mode — when on, all agent replies are sent as voice notes via TTS.

### `/reload`
Reload config, skills, and system prompt from disk. **CLI-only** — not available to the agent as a tool.

### `/compact`
Trigger manual context compaction.

### `/restart`
Restart the foci service via `systemctl restart foci`.

### `/multiball`
Fork the current session to a secondary Telegram bot for parallel conversation. Alias: `/mb`. See [MULTIBALL.md](MULTIBALL.md).

### `/secrets <subcommand>`
Manage secrets. **CLI-only**.
- `/secrets list` — show all secret names grouped by section (values never displayed)
- `/secrets set <section.key> <value>` — add or update a secret
- `/secrets remove <section.key>` — delete a secret
- `/secrets hosts <section>` — show allowed hosts for a section
- `/secrets hosts <section> add <host>` — add an allowed host
- `/secrets hosts <section> remove <host>` — remove an allowed host
- `/secrets hosts <section> clear` — remove all allowed hosts

### `/bitwarden <subcommand>`
Bitwarden vault integration. **CLI-only**.
- `/bitwarden setup` — check prerequisites (bw CLI, bitwarden user, login status)
- `/bitwarden status` — show current state: enabled/disabled, item count, cache age, unlocked secrets

### `/tmux <operation>`
Manage tmux sessions. **CLI-only** (the `tmux` tool is the agent-facing equivalent).
- `/tmux list` — list active sessions
- `/tmux start [name] [command]` — start a session (auto-watches by default; `--no-watch` to disable)
- `/tmux send <name> <keys>` — send keystrokes to a pane
- `/tmux read <name> [lines]` — read pane output
- `/tmux kill <name>` — kill a session
- `/tmux watch <name> [threshold_secs]` — monitor for inactivity
- `/tmux unwatch <name>` — stop monitoring

---

## Diagnostics

### `/log [N]`
Show recent event log lines. Default: 20 lines.

### `/errors [N]`
Show recent error/warning log lines. Default: 10 lines.

### `/config [subcommand]`
Show running configuration.
- `/config` — summary view
- `/config toml` — full config as TOML
- `/config table` — config as formatted table
- `/config available` — all available config keys with types and defaults

### `/prompts`
Show configured prompts and prompt files on disk.

### `/version`
Build version info (version, commit, build date, Go version).

---

## Session

### `/ping`
Liveness check — returns "pong" with timestamp.

### `/sessions <subcommand>`
List and manage per-chat sessions.
- `/sessions list` — list all chat sessions for this agent
- `/sessions default <chat_id>` — set the default session (used by keepalive, cron)
- `/sessions info` — show details for the current chat's session
- `/sessions index [type] [status]` — query the session metadata index (all agents)

### `/agents [new]`
List active agent sessions.
- `/agents` — show all agents with session info
- `/agents new` — launch the interactive agent creation wizard

### `/tools`
List all registered tools.

### `/help`
List available commands grouped by category.

---

## Hidden Commands

### `//`
Repeat the last message. Not shown in `/help`.

---

## Dynamic Commands

### Script commands
Custom commands defined in the `[[commands]]` config section. Each runs a shell script with a configurable timeout.

```toml
[[commands]]
name = "df"
description = "Disk usage"
script = "df -h"
timeout = "5s"
```

### Skill commands
Skills with `command` and `script` in their frontmatter are registered as slash commands automatically.

---

## Special Bot Commands

These are handled directly by the Telegram bot layer, not the command registry:

- **`/stop`** — cancel the current agent turn (works on both primary and secondary bots)
- **`/done`** — detach a multiball secondary bot and return it to the pool
