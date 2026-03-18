# Slash Commands Reference

Messages starting with `/` are intercepted at the Telegram router level before reaching the agent — they execute immediately and never enter session history. A period (`.`) prefix also works as an alias (e.g. `.mana` → `/mana`), but only when it matches a registered command — `.net` or other non-command text passes through to the agent normally. The `.` prefix exists because it's easier to type on most phone keyboards than `/`.

All registered slash commands are also available to the agent as tools (without the `/` prefix), except those marked **CLI-only** below. See [TOOLS.md](TOOLS.md) for details.

---

## Observability

### `/status`
Dashboard overview — agent ID, model, session key, message count, busy/idle status, session created/active times, uptime, context usage percentage, tokens remaining until compaction, session cost.

### `/cache [N]`
Last N API calls with cache token breakdown (input, cache read, cache write, cost, cache hit %). Default: 5.

### `/last [agent]`
Most recent API call per agent — time, model, tokens, cost, session. Optionally filter to a single agent.

### `/cost <period>`
API cost summary for a time period.
- `/cost today` — today's costs by session
- `/cost 24h` — last 24 hours with category breakdown (cache reads/writes, input, output)
- `/cost week` — 7-day summary with daily breakdown
- `/cost <N>` — total for last N days

### `/context`
Context window breakdown — total vs limit, compaction threshold, tokens until compaction, system prompt breakdown by section (environment, workspace files, skills), tool token count, conversation breakdown (user/assistant/tool results), last API call token breakdown.

### `/mana`
Check current mana/quota remaining (percentage remaining + reset time). The command name is configurable via `mana_command_name` in config (e.g. `/juice`, `/credits`). Hidden aliases: `/usage`, `/m`. Only available when the provider supports usage tracking.

### `/todo [subcommand] [args]`
Manage todo items. Bare `/todo` lists active items sorted by priority (limit 15).

**Subcommands:**

- `/todo new <text> [p:PRIORITY] [t:TAG]` — create a new todo
- `/todo done <id> [id...]` — mark as done (ambiguity: `done` alone lists done items, `done 5` transitions)
- `/todo start <id> [id...]` — mark as started
- `/todo drop <id> [id...]` — mark as dropped
- `/todo reopen <id> [id...]` — reopen to "open"
- `/todo edit <id> [p:PRIORITY] [t:TAG] [new text]` — edit fields
- `/todo show <id>` — full detail for one todo
- `/todo search <query>` — full-text search (bleve, porter stemming)
- `/todo get [filters] [search terms]` — combined filter + search (see below)
- `/todo rm <id> [id...]` — hard-delete
- `/todo stats [filters]` — counts by status and tag

**List filters** (apply to bare `/todo`, `/todo stats`, and the filter side of `/todo get`):

| Filter | Effect |
|---|---|
| `open` | Only open items |
| `done` / `closed` | Only done items |
| `active` | Open + started (default) |
| `started` | Only started items |
| `dropped` | Only dropped items |
| `all` | All statuses |
| `t:TAG` | Only items with this tag (multiple `t:` filters use AND logic) |
| `-t:TAG` / `!t:TAG` / `t:!TAG` | Exclude items with this tag (combinable with other `t:` filters) |
| `p:PRIORITY` | Only items with this priority (`high`, `medium`, `low`) |
| `-p:PRIO` / `!p:PRIO` / `p:!PRIO` | Exclude items with this priority |
| `created` | Sort by creation time |
| `updated` | Sort by last update time |
| `priority` | Sort by priority (default) |
| `reverse` | Flip sort direction (default is descending/newest/highest first) |
| `<N>` | Limit results (e.g. `5`) |

**`/todo get` — combined filter + search:**

Bridges list filters and full-text search. Recognised filter tokens are extracted; remaining tokens become the search query.

```
/todo get t:work deploy              # tag filter + search "deploy"
/todo get t:foci t:bug               # AND: items must have both tags
/todo get t:work -t:background       # items with "work" but not "background"
/todo get p:high created server      # priority filter, sort by created, search "server"
/todo get t:daily                    # pure filter (no search → falls back to list)
/todo get running                    # pure search for "running"
```

Use `/` as an explicit delimiter when search terms collide with filter keywords:

```
/todo get t:work / deploy server -old    # "deploy server -old" is the search query
/todo get all / open questions           # "all" is a status filter, "open questions" is search
```

**Search query syntax** (bleve query string):

| Syntax | Effect |
|---|---|
| `deploy server` | Match either term (OR) |
| `+deploy +server` | Both terms required (AND) |
| `"deploy server"` | Exact phrase match |
| `-database` | Exclude term |
| `deploy*` | Prefix match |

Porter stemming is active: "running" matches "run", "deployed" matches "deploy".

### `/tmux <subcommand>`
Manage tmux sessions. **CLI-only** (the `tmux` tool is the agent-facing equivalent). Only available when the tmux tool is registered.
- `/tmux list` — list owned/watched sessions
- `/tmux start <name> [command] [--no-watch]` — start a session (auto-watches by default)
- `/tmux send <name> <keys...>` — send keystrokes
- `/tmux read <name> [lines]` — read pane output
- `/tmux kill <name>` — kill a session
- `/tmux watch <name> [threshold_secs]` — monitor for inactivity
- `/tmux unwatch <name>` — stop monitoring

---

## Operations

### `/model [alias-or-id]`
Show or switch the model for the current session.
- `/model` — show current model
- `/model haiku` — switch to haiku (supports aliases from `[models.aliases]` config)
- `/model gemini:flash` — switch with explicit endpoint via `endpoint:alias` syntax

### `/effort [level]`
Show or set the thinking effort level. Only visible when the current model supports effort.
- `/effort` — show current level
- `/effort low` — set to low (alias: `1`)
- `/effort medium` — set to medium (alias: `2`)
- `/effort high` — set to high (alias: `3`)
- `/effort none` or `/effort off` — clear override

### `/thinking [mode]`
Show or set extended thinking mode. Only visible when the current model supports thinking.
- `/thinking` — show current mode
- `/thinking off` — disable (alias: `0`)
- `/thinking adaptive` — enable adaptive thinking (alias: `1`)

### `/speed [mode]`
Show or set speed mode (Anthropic fast mode — 6x pricing, separate prompt cache). Only visible for supported models.
- `/speed` — show current mode
- `/speed standard` — standard mode (alias: `0`)
- `/speed fast` — fast mode (alias: `1`)

### `/display [key] [value]`
Show or set per-session display options.
- `/display` — show all current display settings
- `/display show_tool_calls off|preview|full` — tool call visibility
- `/display show_thinking off|compact|true` — thinking block visibility
- `/display stream_output on|off` — streaming output (alias: `stream`)
- `/display display_width <20-200>` — output width in characters (alias: `width`)
- `/display reset` — clear all per-session display overrides

### `/voice`
Toggle voice mode — when on, all agent replies are sent as voice notes via TTS.

### `/reset`
Clear session history. Fires session-end memory formation (async) before clearing, rotates the session key, reloads bootstrap. Refuses if the agent is currently processing.

### `/compact [dry-run]`
Trigger manual context compaction.
- `/compact` — compact now
- `/compact dry-run` — show what would happen and send the summary as a document without compacting

### `/reload`
Reload workspace files (system prompt) and skills from disk. **CLI-only**. Config file (`foci.toml`) changes still require a full service restart.

### `/restart`
Restart the foci service. Tries `systemctl restart foci`; falls back to SIGTERM (relies on process supervisor or Docker restart policy).

### `/facet`
Fork the current session to a secondary Telegram bot for parallel conversation. See [FACET.md](FACET.md).

### `/secrets <subcommand>`
Manage secrets. **CLI-only**.
- `/secrets list` — show all secrets grouped by section/key with allowed hosts (values never displayed)
- `/secrets set <section.key> <value>` — add or update a secret
- `/secrets remove <section.key>` — delete a secret
- `/secrets hosts <section>` — show allowed hosts for a section
- `/secrets hosts <section> add <host>` — add an allowed host
- `/secrets hosts <section> remove <host>` — remove an allowed host
- `/secrets hosts <section> clear` — remove all allowed hosts

### `/bitwarden <subcommand>`
Bitwarden vault integration. **CLI-only**.
- `/bitwarden setup` — check prerequisites (bw CLI, bitwarden system user), attempt to create user if missing
- `/bitwarden status` — show current state: enabled/disabled, item count, cache age, unlocked secrets

---

## Diagnostics

### `/log [N]`
Show recent event log lines. Default: 20.

### `/errors [N]`
Show recent error/warning log lines. Default: 10.

### `/config <subcommand>`
Show or edit configuration.
- `/config toml` — raw TOML of the running config (secrets redacted)
- `/config table` — formatted grouped table of all current values
- `/config available` — unset options with their defaults
- `/config set [section.key=value]` — edit the config file (direct mode with `=`, or interactive wizard)

### `/prompts <subcommand>`
Show configured prompts and prompt files on disk.
- `/prompts list` — all prompts with default/custom/disabled status and file paths
- `/prompts reinstall` — write all embedded default prompts to `{workspace}/prompts/`
- `/prompts diff <name>` — unified diff between the resolved prompt and the embedded default, with an AI-generated summary. Name matching is fuzzy: accepts labels (`compaction_summary`), filenames (`compaction-summary.md`), or partial matches (`keepalive`)

### `/version`
Build version info — version, commit, build date, Go version.

---

## Session

### `/ping`
Liveness check — returns "pong" with timestamp.

### `/sessions <subcommand>`
List and manage per-chat sessions.
- `/sessions list` — all chat sessions for this agent (chat ID, user, message count, last active, current/default flags)
- `/sessions default <chat_id>` — set the default session (used by keepalive, cron)
- `/sessions info` — details for the current chat's session
- `/sessions index [filters...]` — query the session metadata index across all agents. Filters: type (`chat`/`spawn`/`cron`/`facet`/`branch`), status (`active`/`compacted`/`archived`/`cleared`/`all`), duration (e.g. `3d`, `4h`), count (e.g. `10`)

### `/agents [new]`
List active agent sessions.
- `/agents` — all agents with ID, session key, status, model, message count
- `/agents new` — interactive 3-step wizard to create a new agent (name → model → character files), appends to `foci.toml` and sets up workspace

### `/tools`
List all registered tools (name and description).

### `/help`
List available commands grouped by category. Commands whose features aren't supported by the current model (e.g. `/effort`, `/thinking`, `/speed`) are hidden automatically.

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
Skills with `command` and `script` in their frontmatter are registered as slash commands automatically (30-second timeout).

---

## Special Bot Commands

These are handled directly by the Telegram bot layer, not the command registry:

- **`/stop`** — cancel the current agent turn (works on both primary and secondary bots)
- **`/done`** — detach a facet secondary bot and return it to the pool
