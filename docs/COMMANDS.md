# Slash Commands Reference

Messages starting with `/` are intercepted at the Telegram router level before reaching the agent — they execute immediately and never enter session history. A period (`.`) prefix also works as an alias (e.g. `.status` → `/status`), but only when it matches a registered command — `.net` or other non-command text passes through to the agent normally. The `.` prefix exists because it's easier to type on most phone keyboards than `/`.

**Two `/`-words are delivery modes, not commands (#1318):** `/steer <message>` and `/queue <message>` (Telegram/Discord, case-insensitive, first word only). The word is stripped and the rest is delivered with that mode — `/steer` redirects the running turn now, `/queue` waits for it to finish and runs as its own fresh turn. A bare `/steer` or `/queue` (no message text) answers with one usage line and delivers nothing. They are not registered commands and never reach the agent as text.

**Who can run a command.** You (the user) can run every command below by typing it in chat on any platform (app, Telegram, Discord) or with `foci command "/<command> …"`. The agent can also call each one as a tool (without the `/` prefix), except those marked **User only** below: those are never offered to the agent. See [TOOLS.md](TOOLS.md) for details.

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
- `/cost <N>` or `/cost <N>d` — total for last N days

### `/context`
Context window breakdown — total vs limit, compaction threshold, tokens until compaction, system prompt breakdown by section (environment, workspace files, skills), tool token count, conversation breakdown (user/assistant/tool results), last API call token breakdown.

### `/mana`
Alias: `/usage`. The agent's backend plan and rate-limit usage report, headed by the backend it describes. Claude Code (`claude-code`): remaining plan quota, rate-limit windows, and recent usage of the agent's own Claude Code account (its login: read with the agent's `backend_config.env` and `binary`), and the account's name (its email, else its folder). Codex (`codex`): this agent's codex account allowance — each bucket's windows (used %, reset time), credits when codex reports a balance, and whether a limit is reached; read fresh when the agent's codex process is running, else the last numbers codex reported, with their time. opencode and API-mode agents have no usage source, so they don't get the command. On a Claude Code agent, when the read shows usage available (it carries the rate-limit windows, and every window it reports is under `[usage_hold] threshold`), the command also lifts the rate-limit gate and that account's usage hold as `/limit_reset` does, and says so (a read of one account never lifts another); when usage is not available it changes nothing.

`/mana cached` (`/usage cached`) answers at once from the usage figures foci already holds, without a fresh read: each limit window's newest percent used, its reset time and how old the figure is, or that no percent is held for the current window (a figure measured before the window reset is not shown). A figure from any source counts: the spawn reserve's scheduled reads, a spawn's read, a fresh `/mana` read, and the backend's own usage events (Claude Code `rate_limit_event`, codex `account/rateLimits/updated` and its start-up read). It never lifts a limit. `/mana cached json` prints one JSON object for a script; `/help mana` lists its keys (`account`, `account_name`, `backend`, `now`, `has_figures`, and per window `window`, `length_seconds`, `used_percent` (0-100, null when none), `used_received_at`, `used_age_seconds`, `used_source`, `resets_at`, `received_at`, `age_seconds`, `source`, `status`, `limit_reached`; times are Unix seconds). It works through `foci command -a <agent> /mana cached json`. Every usage figure foci receives is kept forever in the usage history (`<data>/usage-history.db`, WIRING "Usage history").

### `/todo [subcommand] [args]`
Manage todo items. Bare `/todo` shows usage (available verbs). Use `/todo list` or `/todo active` to list items.

**Subcommands:**

- `/todo new <text> [p:PRIORITY] [t:TAG]` — create a new todo
- `/todo done <id> [reason]` / `/todo done <id> <id>...` — mark as done, recording the reason. When text follows, only the FIRST word is an id and the rest is the close reason (`/todo done 2457 2 commits landed` closes #2457 alone, reason `2 commits landed`; leading and trailing whitespace is trimmed, everything between the first and last word kept exactly). Ids only (`/todo done 7 8`) closes them all with an empty reason. `done` means close and nothing else: a first word that is not a digits-only id (`done 14x landed abc`, `done t:x`, `done reverse 5`, `done +14 for the fix`) or a bare `done` is an error naming the word (the usage line, for the bare form) and changes nothing, never a listing; done todos list through `/todo list done`
- `/todo start <id> [id...]` — mark as started
- `/todo drop <id> [id...]` — mark as dropped
- `/todo reopen <id> [id...]` — reopen to "open"
- `/todo edit <id> [p:PRIORITY] [t:TAG] [new text]` — edit fields
- `/todo show <id>` — full detail for one todo
- `/todo search <query>` — full-text search (bleve, porter stemming)
- `/todo get [filters] [search terms]` — combined filter + search (see below)
- `/todo rm <id> [id...]` — hard-delete
- `/todo stats [filters]` — counts by status and tag

**List filters** (apply to `/todo list`, `/todo stats`, and the filter side of `/todo get`):

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
Manage tmux sessions. **User only** (the agent uses the `tmux` tool instead). Only available when the tmux tool is registered.
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
- `/model` — shows a keyboard with one button per model the agent's backend advertises (live catalogue), the current model marked with a ✓. Cold catalogue (no models known) falls back to type-the-name.
- `/model anthropic/claude-haiku-4-5` — switch to a specific model using `developer/model_id` format
- `/model gemini:flash` — switch with explicit endpoint via `endpoint:name` syntax

### `/effort [level]`
Show or set the thinking effort level. Only visible when the current model supports effort. Levels are sourced live from the model catalogue, so e.g. opus-4-8 offers `low`/`medium`/`high`/`xhigh`/`max` rather than a fixed set; a cold/unknown catalogue falls back to the static `low`/`medium`/`high`. Numeric aliases follow catalogue order (`1` = first level).
- `/effort` — show current level (bare invocation also shows the level keyboard)
- `/effort low` — set to low (alias: `1`)
- `/effort medium` — set to medium (alias: `2`)
- `/effort high` — set to high (alias: `3`)
- `/effort none` or `/effort off` — clear override
- On a ccstream (CC) session, setting a concrete level pushes live to the running CC via `apply_flag_settings` — the next turn runs at the new effort with no session bounce. The level also persists across bounces/restarts: it's re-injected at cold launch via CC's `--effort` flag. (`clear`/`off` skip the live push and take effect on next launch.)

### `/thinking [mode]`
Show or set extended thinking mode. Only visible when the current model supports thinking. Hidden and rejected on the ccstream (CC) backend — CC has no thinking control (unsupported since ~4.5/4.6) and effort subsumes it; api agents keep it.
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

### `/mode [mode]`
Show or set a delegated backend's permission mode. Availability, buttons, aliases and help come from that backend's `Spec.PermissionModes`; API agents and backends without declared modes do not get this command. With no session override, status says "backend default" rather than guessing its configuration. Native mode values and numbers in the displayed order are accepted as aliases.

| Backend | Choices |
|---|---|
| Claude Code | `normal` (default), `accept` (auto-accept edits), `plan` (read-only), `auto` (classifier review) |
| Codex | `normal` / `user` (human review), `auto` / `auto_review` (automatic review) |
| opencode | `normal` (ask), `accept` (allow edits), `plan` (deny edits), `allow` (allow every tool without review) |

Codex changes take effect on the next turn and subsequent turns by explicitly setting both `approvalPolicy=on-request` and `approvalsReviewer`; they do not change the sandbox or disable approval checks. Configuration sets the default when a backend is created/resumed; `/mode` is a session override, not a config-file edit. opencode deliberately calls its unrestricted mode `allow`, not `auto`: it has no automatic risk reviewer. Invalid values are rejected with only that backend's applicable choices.

### `/plan`
Put a coding-agent backend into plan mode. The agent explores and reasons read-only without making changes. Only available for agents with a delegated coding backend.

### `/overrides [subcommand]`
Session overrides management. Inspect and clear per-session overrides previously set via `/display`, `/model`, `/thinking`, `/effort`, `/speed`, etc.
- `/overrides` — list active per-session overrides
- `/overrides clear` — clear all per-session overrides

### `/pause` / `/resume` / `/complete`
Answer-capture control for a pending `foci ask`. When an ask is waiting for an answer, these commands control whether/how the captured reply is delivered:
- `/pause` — pause answer capture for the pending ask
- `/resume` — resume answer capture
- `/complete` — finalise and deliver the captured answer

### `/reset [hard]`
Clear session history.
- `/reset` — fires session-end memory formation (async for API agents, blocking for delegated) before clearing, rotates the session key, reloads bootstrap. Refuses if the agent is currently processing.
- `/reset hard` — cancels any in-flight turn, skips memory formation, destroys the backend, rotates the session key. Use when the agent is stuck or you want a clean reset without saving memories. Dispatched immediately so it can interrupt a live turn.

### `/compact [dry-run]`
Trigger manual context compaction.
- `/compact` — bare invocation shows a run/dry-run confirmation keyboard (does NOT compact directly); pick an option to proceed
- `/compact dry-run` — show what would happen and send the summary as a document without compacting
- Any other non-empty unmatched args (e.g. `/compact foo`) run compaction directly via the default handler

### `/pass <command>`
Forward a raw command directly to the configured delegated backend (shown by name, e.g. "Claude Code" or "Codex CLI"), bypassing foci's command dispatch. Useful for running backend-native slash commands that foci would otherwise intercept (e.g. `/pass /context`, `/pass /model opus`, `/pass /help`). Only available for agents with a delegated backend — returns an error for API-mode agents. Returns immediately; any output arrives asynchronously through the backend's normal stream events.

### `/limit_reset`
Force-lift the agent's rate-limit gate and its account's usage hold, for when the Claude Code account's limit was reset but foci still holds work. Both normally clear only at their reset time or on a new usage report, and during a usage stop nothing runs that could bring one. The lift clears the stored state too, so a restart does not bring the gate back; held messages and paused work run as at a natural lift. The lift reaches every agent on the same Claude Code account (its login; agents on other accounts are untouched): each opens its own rate-limit gate, and a turn whose Claude Code is sleeping in a rate-limit (429) retry is interrupted and resumed with a hidden resume line (other running turns are left alone; a keepalive in such a retry is stopped with no resume) (#3044). If the limit is still in force, the next usage report holds the account again. Claude Code (`claude-code`) agents only (#3043).

### `/login`
Manually trigger Claude Code re-authentication. Drives a `claude /login` TUI in tmux, relays the login URL back to the chat that ran `/login`, then treats your next message as the login code — message processing is paused throughout. Normally this flow fires automatically on a 401 auth failure; this command exposes the same trigger on demand. ccstream backend only (reports unavailable on other backends and API); returns "already in progress" if a re-login is already running.

### `/restart`
Restart the foci service. Tries `systemctl restart foci`; falls back to SIGTERM (relies on process supervisor or Docker restart policy).

### `/facet [message]`
Fork the current session to a secondary Telegram bot for parallel conversation. See [FACET.md](FACET.md). Listed only when the agent can branch: an API agent, or a delegated backend that can fork its conversation (Claude Code, Codex, opencode). On any other backend it is hidden and refused. In the app, any text after `/facet` is sent into the new facet chat as your first message there, and the agent answers there. On Telegram and Discord, `/facet <text>` is refused: use `/facet` alone, then message the facet bot.

### `/fork [message]`
Fork the current conversation into a new app chat. The new chat is an ordinary chat (no orientation prompt, compaction on) whose history is the current conversation; the current chat is not changed. Works from any platform; the new chat appears in the app. Same listing rule as `/facet`. Any text after `/fork` is sent into the new chat as your first message there, as if typed in it, and the agent answers in the new chat. With no text, the new chat waits for you.

### `/secrets <subcommand>`
Manage secrets. **User only**: type it in chat; the agent cannot call it.
- `/secrets list` — show all secrets grouped by section/key with allowed hosts (values never displayed)
- `/secrets set <section.key> <value>` — add or update a secret
- `/secrets remove <section.key>` — delete a secret
- `/secrets hosts <section>` — show allowed hosts for a section
- `/secrets hosts <section> add <host>` — add an allowed host
- `/secrets hosts <section> remove <host>` — remove an allowed host
- `/secrets hosts <section> clear` — remove all allowed hosts

### `/bitwarden <subcommand>`
Bitwarden vault integration. **User only**: type it in chat; the agent cannot call it.
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
- `/config get <agent> <key>` — read a named agent's current value for a per-agent key (spelled as under `[[agents]]`, e.g. `keepalive.interval`; a leading `agent.` is accepted). The value is read from the config file at call time, and the reply says where it comes from — set for this agent in foci.toml, inherited from the global section, the running config's value (used when the key is in neither the agent block nor the global section, before falling back further), or the built-in default — plus whether the field can be set live. `<agent>` must exactly match a running agent id. `/config get <agent>` lists the per-agent keys that can be set live; `/config get` shows usage.
- `/config set [section.key=value]` — edit the config file (direct mode with `=`, or interactive wizard). The reply says `Applied live.` for a field that can apply live and `Restart to take effect.` for one that cannot. `/config set` and the app's config editor are the ONLY ways to apply a change live: foci does not watch `foci.toml`, so a hand edit to the file takes effect only at the next restart, for every field.
- `/config set <agent> <key>=<value>` — edit a NAMED agent's per-agent setting from any agent's chat (e.g. `/config set clutch keepalive.interval=1m`). `<agent>` must exactly match a running agent id; the value is everything after the first `=` (so it may contain spaces and further `=`); a leading `agent.` on the key is accepted. Only live-appliable (hot) fields can be written this way — a restart-only field is refused with a pointer at editing foci.toml, and a key with no per-agent registry row (e.g. `debug.enable_pprof` or `backend`) is reported as not a per-agent setting. Successful writes apply live immediately. As with every `set` form, the value is validated before anything is written: an invalid value (e.g. a duration the runtime cannot parse, like `keepalive.interval=banana`) is refused with the reason and writes nothing.
- `/config unset <section>.<key>` — remove an explicitly-written key from `foci.toml`, so the field reverts to its built-in default (the chat counterpart of the app editor's `config.unset`). Uses the same comment-preserving writer and live apply as `/config set`: the reply names the removed value and says `Applied live.` or `Restart to take effect.`. `agent.<key>` removes the key from the current agent's `[[agents]]` block, so it inherits the global value or the default. A key that is not written in the file is reported as nothing to unset, and the file is not touched.
- `/config unset <agent> <key>` — remove a NAMED agent's per-agent override (e.g. `/config unset clutch keepalive.interval`). Same rules as `/config set <agent> …`: `<agent>` must be a running agent, the key must be a per-agent setting, and only live-appliable fields are accepted. The reply shows the value the agent now resolves to and where it comes from (the global section in `foci.toml`, or the built-in default).

When `/config set` adds a key that is not yet in its section, it goes directly after the section's last key, not at the end of the section below any trailing comment block.

The native app can also view and edit the config: its Settings → "Server config"
page renders the same field registry over `config.get`/`config.put`/`config.unset`
frames (FAP wire §13), split into "set in foci.toml" vs defaults, per scope
(global / per-agent `[[agents]]` overrides). Named model definitions
(`[models.*]`) and endpoints (`[endpoints.*]`) are editable there too: the page
lists every entry, and can add one, change an entry's sub-fields, or delete an
entry. Edits use the same comment-preserving file writer as `/config set`;
endpoint edits apply live, model edits take effect on restart. An edit whose result would stop the server from
starting (e.g. deleting a model one of your groups still uses) is refused with
the reason. The one exception: when foci.toml already fails to load (e.g. a
hand edit broke it), edits are written unchecked so the file can be repaired
from the app. Entries whose names are not bare TOML keys, or that live in
foci.toml as inline/dotted tables rather than `[models.<name>]` tables, must be
edited by hand. Gated on the client's `configEdit` capability; see
`internal/app/configedit.go`.

### `/prompts <subcommand>`
Show configured prompts and prompt files on disk.
- `/prompts list` — all prompts with default/custom/disabled status and file paths
- `/prompts reinstall` — write all embedded default prompts to `{workspace}/shared/prompts/`
- `/prompts diff <name>` — unified diff between the resolved prompt and the embedded default, with an AI-generated summary. Name matching is fuzzy: accepts labels (`compaction_summary`), filenames (`compaction-summary.md`), or partial matches (`keepalive`)

### `/version`
Build version info — version, commit, build date, Go version.

### `/misc <subcommand>`
Miscellaneous operations.
- `/misc pprof [on|off]` — toggle `net/http/pprof` profiling endpoints on the gateway HTTP server at `/debug/pprof/*`.

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

### `/agents [new|rename]`
List active agent sessions.
- `/agents` — all agents with ID, session key, status, model, message count
- `/agents new` — interactive wizard to create a new agent (name → backend → model → character mode). The backend step offers the live set of registered delegated backends (claude-code, codex, opencode) plus `api` for the in-process loop; empty input picks `claude-code`. The model step is **skipped for `api`** — API agents have no per-agent model (it resolves globally via `[groups]`/`[models]`), so the wizard goes straight from backend to character mode. The choice is written explicitly to `foci.toml` (delegated → `backend = "<name>"` + `backend_config.model = "<alias>"`; api → `backend = "api"`), and the workspace is set up
- `/agents rename OLD NEW [--move-workspace] [--yes]` — rename an agent inside the running gateway (#2787): only OLD stops while it runs, and NEW starts with OLD's history in OLD's place. Without `--yes` it replies with the plan. Over the HTTP API (`foci command`, an agent) it sends the owner an Approve/Deny prompt instead of running. The result is posted to the chat it came from. Undo: `/agents rename NEW OLD --yes`. See CONFIG.md "Renaming an agent".

### `/tools`
List all registered tools (name and description).

### `/help`
List available commands grouped by category. Commands whose features aren't supported by the current model (e.g. `/effort`, `/thinking`, `/speed`) are hidden automatically.

`/help <command>` shows that command's description, its aliases and its subcommands, each taken from the command's own registration (the same text the table and `/<command>` usage show). The leading `/` is optional. A registered command that the table hides from you is still described, with a line saying it isn't available on this agent (and why, when it needs a different transport). Only a name that isn't registered at all gets "Unknown command" followed by the full table.

---

## Hidden Commands

### `//`
Repeat the last message. Not shown in `/help`.

---

## Operator-Only Commands (#2279)

An agent can name its **operators** per chat platform (`operator_users` under `[platforms.access]`, agent and global lists add up). Everyone else in `allowed_users` may still chat with the agent, but can only run the commands in `non_operator_commands` — by default `/help`, `/reset` and `/stop`. A forbidden command answers "`/tmux` is only for this agent's operators." (typed, `.dot` form, an alias, or a button press — a press is judged by who pressed it). `/help` and the "Did you mean" suggestions only list what the caller may run. Unknown commands keep the usual "Unknown command" reply.

Notes: an entry can name a subcommand, and a bare name permits only the command itself (`reset` permits `/reset` but not `/reset hard`, which needs `"reset hard"`); the gate covers Telegram and Discord chats only (not the app or the HTTP endpoints); with no operators configured nothing changes. See [CONFIG.md](CONFIG.md#access-fields-platformsaccess) for the settings.

---

## Onboarding

### `/android`
Android onboarding. Initiates the pairing flow for the Android companion app.

### `/pair`
Aliases: `/pairkey`, `/pair-key`. Mint a single-use Android pairing key. The key can be used once to register a new Android device with the gateway.

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

- **`/stop`** — cancel the current agent turn (works on both primary and secondary bots). On Claude Code this also stops the subagents and the running foreground command, but not a background command. `/stop subagents` stops only the running subagents, and `/stop commands` only the agent's own shell commands (foreground and background); neither ends the turn (claude-code stream backend). `/stop all` stops the subagents, the commands and the turn (#2138, #2140). `/stop foreground` stops only the agent's foreground shell commands, and `/stop item <id>` stops one entry of the app's running list, a subagent, a background command or a spawn; the app sends them from a long press in its stop dialog (#2901). Background `spawn`s (clone mode) count as subagents: plain `/stop`, `/stop subagents` and `/stop all` stop them too (#2139)
- **`/done`** — detach a facet secondary bot and return it to the pool
