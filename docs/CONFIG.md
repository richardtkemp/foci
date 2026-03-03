# Foci Configuration Reference

Foci uses two TOML files: `foci.toml` (main config) and `secrets.toml` (credentials). By default, foci looks for `foci.toml` in the current working directory. Override with `--config`:

```
foci-gw --config /home/foci/config/foci.toml
```

Secrets are loaded from `secrets.toml` in the same directory as the config file. Values in `secrets.toml` override matching fields in `foci.toml`.

---

## Scope and Conventions

Config fields fall into three categories based on where they can be set:

1. **Global-only** — set at the top level or in a dedicated section. Not overridable per-agent.
2. **Global-or-agent** — set globally (in `[defaults]` or a parent section like `[sessions]`, `[tools]`) and optionally overridden per-agent in `[[agents]]`. Documented once below.
3. **Agent-only** — set only per-agent in `[[agents]]`. No global equivalent.

**Resolution order** for global-or-agent fields: agent value > `[defaults]` value > global section value > hardcoded default.

**Unset convention:** Throughout this document, `unset` means the field is not present in TOML. For optional/pointer fields, `unset` triggers inheritance from the parent section. For value fields, the listed default applies. Zero values (`0`, `""`, `[]`) that mean "inherit from global" are noted explicitly in the description.

---

## 1. Global-Only Configuration

Fields that exist only at the top level or in dedicated global sections. These cannot be overridden per-agent.

### Top-Level Keys

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `data_dir` | string | `$HOME/data` | Directory for databases, sessions, and state files. Relative paths resolve against `$HOME`. Absolute paths used as-is. |
| `welcome_file` | string | `"data/WELCOME.md"` | Path to a changelog/welcome file. If this file exists on startup, its contents are injected into the first agent's main session and the file is deleted. Relative paths resolve against `$HOME`. |
| `skip_security_checks` | bool | `false` | Skip startup security checks for `secrets.toml` (ownership, permissions, group membership). Useful for development environments. See [SECRETS.md](SECRETS.md). |

### `[anthropic]`

Anthropic API credentials. Prefer `secrets.toml` for tokens. See [AUTH.md](AUTH.md) for setup guide.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `setup_token` | string | `""` | Setup token from `claude setup-token`. Written by `foci auth`. Overridden by `secrets.toml` `[anthropic] setup_token`. Highest priority credential source. |
| `api_key` | string | `""` | Anthropic API key. Overridden by `secrets.toml` `[anthropic] api_key`. Used when setup token is unavailable. |
| `brave_api_key` | string | `""` | Brave Search API key for `web_search` tool. Overridden by `secrets.toml` `[brave] api_key`. |
| `http_timeout` | string | `"600s"` | HTTP timeout for Anthropic API calls. Go duration format. Increased to support extended thinking responses. |
| `usage_api_timeout` | string | `"10s"` | HTTP timeout for usage API calls. Go duration format. |
| `cc_credentials_poll_interval` | string | `"30s"` | How often to re-read Claude Code credentials from `~/.claude/.credentials.json`. |

See [AUTH.md](AUTH.md) for token resolution order and setup guide.

### `[telegram]`

Telegram bot configuration. Fields `allowed_users`, `received_files_dir`, and `enable_startup_notify` can be overridden per-agent — see [Global-or-Agent: Telegram](#telegram-overrides).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `multiball_bots` | string[] | `[]` | Shared multiball pool: bot names whose tokens are resolved via `"telegram.<name>"` secret convention. Fallback for any agent whose per-agent pool is exhausted (or has no per-agent pool). |
| `multiball_session_ttl` | string | `"60m"` | Idle TTL before a multiball bot can be reclaimed by a new `/multiball` call. If no messages to/from the bot within this window, it's considered abandoned and available for reuse. `"0"` disables auto-reclaim. Go duration format. Applies to both per-agent and shared pools. |
| `message_queue_size` | int | `64` | Outbound message queue buffer size. High-traffic bots may need larger queues. |
| `long_poll_timeout` | string | `"65s"` | Long-poll timeout for Telegram `getUpdates`. Should exceed 60s. Go duration format. |

#### Bot token resolution

Bot tokens are resolved by convention: `"telegram.<botname>"` in `secrets.toml`. No explicit bot map is needed.

For example, an agent with `telegram_bot = "primary"` resolves its token from the secret key `telegram.primary`. To override the convention, set `bot_secret` on the agent.

`secrets.toml`:
```toml
[telegram]
primary = "123456:ABC..."
secondary = "789012:DEF..."
```

### `[http]`

HTTP API server.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `port` | int | `18791` | HTTP server port. |
| `bind` | string | `"127.0.0.1"` | Bind address. Use `0.0.0.0` for external access. |
| `graceful_shutdown_timeout` | string | `"30s"` | Time to wait for in-flight requests on shutdown. Go duration format. |

Endpoints: `POST /send`, `GET /status`, `POST /command`, `POST /wake`, `GET /voice` (WebSocket, when `[voice] ws_enabled = true`).

All endpoints accept an `agent` field (JSON body for POST, query param for GET) to target a specific agent by ID. When empty or omitted, the first configured agent is used. The `/send` endpoint also accepts an optional `session` field to target a specific session key (defaults to `main`).

#### CLI (`foci` command)

The `foci` CLI wraps the HTTP API. All subcommands accept `-a <id>` / `--agent <id>` to target a specific agent. The `send` command also accepts `-s <session>` / `--session <id>` to target a specific session:

```
foci send -a research "check the news"
foci send -a clutch -s research "text"  # routes to agent:clutch:research
foci branch -a research
foci status --agent=research
foci ping -a research
foci eval -a research "df -h"
foci command -a research /cache
```

When omitted, the first agent and main session are used (backward compatible).

### `[logging]`

Logging and diagnostics. The `messages_in_log` field can be overridden per-agent — see [Global-or-Agent: Notifications & Logging](#notifications--logging).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `level` | string | `"INFO"` | Log level: `DEBUG`, `INFO`, `WARN`, `ERROR`. |
| `event_file` | string | `"logs/foci.log"` | Path to event log file. Relative paths resolve against `$HOME`. |
| `api_file` | string | `"logs/api.jsonl"` | Path to API call log (JSONL). One entry per API call with tokens, cost, duration. Relative paths resolve against `$HOME`. |
| `api_db` | string | `$data_dir/api.db` | SQLite API call log. All API calls logged with `call_type` (conversation, compaction, summary, spawn). `""` disables. |
| `conversation_file` | string | `$data_dir/conversation.db` | Path to conversation SQLite log. Relative paths resolve against `$HOME`. |
| `full_payload` | bool | `false` | Write full API request/response bodies to `payload_file`. |
| `payload_file` | string | `"logs/api-payload.jsonl"` | Path for full payload log. Only used when `full_payload = true`. Relative paths resolve against `$HOME`. |
| `cache_bust_detect` | bool | `false` | Alert via Telegram when `cache_read` drops >50% vs previous request (indicates prefix changed). |
| `cache_bust_idle_minutes` | int | `10` | Suppress cache bust alerts if the session was idle longer than this many minutes. Anthropic's cache TTL is 5 min, so any gap >10 min means the cache expired naturally — not a genuine bust. |
| `warning_max_per_window` | int | `3` | Max identical warnings allowed per time window before suppression. `0` disables rate-limiting. |
| `warning_window_duration` | string | `"5m"` | Time window for warning deduplication. Go duration format. |
| `warning_proactive_active_interval` | string | `"5m"` | Min interval between proactive warning turns when user is active. Go duration format. |
| `warning_proactive_inactive_interval` | string | `"1h"` | Min interval between proactive warning turns when user is inactive. Go duration format. |
| `warning_proactive_activity_threshold` | string | `"10m"` | User is "active" if last message within this window. Go duration format. |
| `log_rotation` | bool | `true` | Enable built-in log rotation. |
| `rotation_period` | string | `"24h"` | How often to check and rotate logs. Go duration format. |
| `retention_period` | string | `"48h"` | Keep lines newer than this in the active log. Older lines archived to gzip. |
| `rotation_max_line_size` | string | `"64MB"` | Max line size for the rotation scanner buffer. Accepts `KB`, `MB`, `GB` suffixes. If a log line exceeds this size, rotation fails and that log file won't be rotated. |
| `archive_dir` | string | `""` | Directory for gzip archives. `""` uses `logs/archive/`. |

When `inject_agent_warnings` is enabled (per-agent), repeated identical warnings are deduplicated: after `warning_max_per_window` occurrences within `warning_window_duration`, further duplicates are suppressed and summarised as "... and N more in last Xm" on the next drain. Warning messages are normalised — IP addresses, hex strings, and multi-digit numbers are replaced with placeholders so semantically identical errors are grouped together.

### `[sessions]`

Session storage. Compaction and prompt fields that can be overridden per-agent are in [Global-or-Agent: Compaction & Sessions](#compaction--sessions).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `dir` | string | `$data_dir/sessions` | Directory for JSONL session files. Relative paths resolve against `$HOME`. |
| `compaction_max_tokens` | int | `4096` | Max output tokens for the compaction summary. |
| `compaction_min_messages` | int | `4` | Minimum messages in session before compaction is allowed. |
| `max_system_prompt_chars_file` | int | `20000` | Warn at startup and `/reload` if any system prompt file exceeds this many chars. `0` disables. |
| `max_system_prompt_chars_total` | int | `80000` | Warn at startup and `/reload` if total system prompt exceeds this many chars. `0` disables. |
| `archive_after` | string | `"168h"` | Gzip idle session files after this duration of inactivity. Go duration format. Sessions with active branches are skipped. Archived sessions are transparently decompressed when accessed. `"0"` effectively disables (no sessions will be old enough). |

Sessions are stored as JSONL files at `{dir}/agent/{id}/{type}.jsonl`.

All prompt fields (`compaction_summary_prompt`, `branch_orientation_prompt`) are file paths, not inline strings. If the file can't be read, a warning is logged and the embedded default is used. Prompt files are read live at the point of use — edits take effect immediately without restart or `/reload`.

When no config override is set, embedded defaults from `prompts/` are used:
- `prompts/branch-orientation-headless.md` — headless branches (cron, spawn, keepalive)
- `prompts/branch-orientation-multiball.md` — user-attached multiball branches
- `prompts/compaction-summary.md` — compaction summary prompt
- `prompts/compaction-handoff.md` — post-compaction handoff message
- `prompts/keepalive.md` — keepalive ping prompt
- `prompts/background.md` — background work prompt
- `prompts/memory-formation.md` — memory formation prompt (interval + session-end)
- `prompts/memory-consolidation.md` — MEMORY.md consolidation prompt

### `[memory]`

Memory system (FTS5 search over markdown files + conversation history).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `reindex_debounce` | string | `"0s"` | Delay before reindexing after file changes. Go duration format. |
| `conversation_weight` | float | `0.1` | Weight multiplier for conversation search results (0.0–1.0). Lower = conversation appears further down in results. |
| `search_limit` | int | `20` | Maximum number of search results to return. |

When set, creates SQLite databases in the data directory (`$HOME/data/` by default): `memory.db`, `reminders.db`, `scratchpad.db`.

#### `[[memory.sources]]`

Multiple memory sources with weighted relevance.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `name` | string | required | Unique identifier (e.g. `"canonical"`, `"code"`, `"docs"`). |
| `dir` | string | required | Directory path to index. |
| `weight` | float | `1.0` | Weight multiplier for search ranking (0.0–1.0). Higher = more relevant. |

Example:
```toml
[[memory.sources]]
name = "canonical"
dir = "/home/foci/character/memory"
weight = 1.0

[[memory.sources]]
name = "docs"
dir = "/home/foci/project/docs"
weight = 0.5
```

Per-agent memory sources (`[[agents.memory.sources]]`) are documented in [Agent-Only: Memory](#memory).

### `[voice]`

Voice support (speech-to-text and text-to-speech). The `tts_rate` field can be overridden per-agent via `[defaults]` — see [Global-or-Agent: Voice](#voice-1).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `stt_endpoint` | string | `"https://api.groq.com/openai/v1/audio/transcriptions"` | OpenAI-compatible Whisper endpoint for speech-to-text. |
| `stt_model` | string | `"whisper-large-v3"` | Whisper model name. |
| `tts_provider` | string | `""` | TTS provider: `"edge-tts"` or `"openai"`. `""` disables TTS. |
| `tts_endpoint` | string | `""` | API endpoint for OpenAI TTS provider. |
| `tts_model` | string | `""` | Model name for OpenAI TTS (e.g. `"tts-1-mini"`). |
| `tts_voice` | string | `""` | Voice name (provider-specific). `""` defaults to `"alloy"` for OpenAI provider. |
| `ws_enabled` | bool | `false` | Enable the `/voice` WebSocket endpoint for real-time two-way voice conversation (FOCI app). Requires `voice.api_key` in `secrets.toml` and a configured STT provider. |

STT requires a Groq API key in `secrets.toml` (`[groq] api_key`). TTS with OpenAI provider requires an OpenRouter key (`[openrouter] api_key`). The `/voice` WebSocket endpoint requires an additional `voice.api_key` in `secrets.toml`.

### `[bitwarden]`

Bitwarden vault integration. Provides dynamic, approval-gated access to vault credentials via the `bw` CLI running as a dedicated `bitwarden` system user through aisudo.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Enable Bitwarden integration. Requires `bw` CLI installed and session file configured. |
| `session_file` | string | `"/home/bitwarden/.bw_session"` | Path to BW session token file. Read by the bitwarden user at execution time — foci never reads this file. |
| `refresh_interval` | string | `"15m"` | How often to refresh vault item metadata. Go duration format. |
| `secret_ttl` | string | `"30m"` | How long unlocked passwords stay cached before requiring re-approval. Go duration format. |
| `cleanup_interval` | string | `"1m"` | How often to purge expired cached values. Go duration format. |

Two-tier security model:
- **`bw list items`** runs via `sudo -u bitwarden sh -c 'export BW_SESSION=$(cat FILE) && bw list items'` (allowlisted in aisudo, auto-approved)
- **`bw get password <id>`** runs via the same wrapper (requires Telegram approval via aisudo)

The bitwarden user reads its own session file at each invocation — foci never sees the session token. This means vault re-locks are handled gracefully (just update the session file).

Example:
```toml
[bitwarden]
enabled = true
session_file = "/home/bitwarden/.bw_session"
refresh_interval = "15m"
secret_ttl = "30m"
```

See [SECRETS.md](SECRETS.md) for the full security model and URI-based host validation.

### `[environment]`

Environment block injected as the first system prompt block, providing the agent with runtime context (workspace, paths, messaging platform, message metadata format).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `true` | Inject environment block as the first system block. `false` disables. |
| `docs_path` | string | `""` | Path to platform docs directory. Shown in environment block when set. Relative paths resolve against `$HOME`. |

When enabled, a text block is programmatically built at startup and prepended before character files. It contains:

- **Workspace** — workspace path, agent ID, platform URL, docs path (if configured), messaging platform
- **Paths** — config file, log directory
- **Message Metadata** — documents the `[meta]` header fields (time, gap, model, prev_cost, prev_tokens, mana)
- **Session Structure** — lists character files and explains what the human can/cannot see

The block is built once per agent at startup from config values — no runtime overhead. It does not include secrets, character identity, or skill lists (those have their own blocks).

### `[resources]`

System resource monitoring.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `memory_guard_enabled` | bool | `true` | Enable system memory guard. Monitors total RSS of all foci user processes, warns/kills under memory pressure. |
| `memory_guard_interval` | string | `"60s"` | Check interval. Go duration format. |
| `memory_warn_percent` | int | `25` | Warn threshold as % of total RAM. Requires memory pressure (PSI) to fire. |
| `memory_kill_percent` | int | `40` | Kill threshold as % of total RAM. Kills the largest non-foci process owned by the foci user. Requires memory pressure (PSI) to fire. |
| `memory_pressure_threshold` | float | `10.0` | Minimum PSI memory avg10 value required before warn/kill actions fire. Prevents false alarms when RSS is high but free RAM is available. |

Both thresholds require memory pressure (PSI `avg10` from `/proc/pressure/memory` exceeding `memory_pressure_threshold`) before acting. This avoids false alarms when the system has ample free RAM despite high RSS. The guard reads `/proc` directly — no external commands.

### `[database]`

SQLite database settings.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `busy_timeout` | string | `"5s"` | SQLite busy timeout for concurrent access. Go duration format. High-load systems may need longer waits. |

### `[models]`

Model aliases and related configuration.

The `aliases` map allows shorthand names to be resolved to full model IDs in both `/model` command and the agent wizard. These are the built-in defaults if not configured.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `aliases` | map | see below | Shorthand → full model ID mapping. |

Default aliases (used when `[models]` section is not configured):
- `opus` → `claude-opus-4-6`
- `sonnet` → `claude-sonnet-4-6`
- `haiku` → `claude-haiku-4-5`

Example with custom model aliases:
```toml
[models.aliases]
opus = "claude-opus-5-0"
sonnet = "claude-sonnet-5-0"
haiku = "claude-haiku-5-0"
custom = "claude-custom-model"
```

### `[tools]`

Tool behavior settings (global-only fields). Fields that can be overridden per-agent are in [Global-or-Agent: Tool Behavior](#tool-behavior).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `temp_dir` | string | `"/tmp/foci-tool-results"` | Directory for large tool result files. |
| `tmux_cols` | int | `300` | Window width (columns) applied via `resize-window` after `tmux new-session`. |
| `tmux_rows` | int | `30` | Window height (rows) applied via `resize-window` after `tmux new-session`. |
| `exec_default_timeout` | int | `30` | Default timeout for exec commands in seconds. |
| `tmux_command_timeout` | string | `"5s"` | Timeout for tmux control commands. Go duration format. |
| `web_fetch_timeout` | string | `"30s"` | HTTP timeout for web fetch operations. Go duration format. |
| `web_fetch_max_bytes` | int | `1048576` | Max bytes to read from web fetch (1MB default). |
| `web_search_timeout` | string | `"15s"` | HTTP timeout for web search API calls. Go duration format. |
| `summary_context_turns` | int | `5` | Number of recent conversation turns included as context when auto-summarising oversized tool results. |
| `summary_context_chars` | int | `6000` | Max characters of conversation context sent to Haiku for auto-summary. |
| `tmux_memory_check_interval` | string | `"5m"` | How often to check tmux server RSS. Go duration format. `"0"` disables monitoring. |
| `tmux_memory_warn` | string | `"10%"` | Warn threshold. Sends Telegram notification. Formats: `"N%"` (% of RAM), `"Nmb"`, `"Ngb"`. |
| `tmux_memory_critical` | string | `"20%"` | Critical threshold. Sends Telegram notification with stronger message. Same formats. |
| `tmux_memory_kill` | string | `"30%"` | Kill threshold. Kills tmux server, notifies, cleans up tool state. Same formats. |
| `tmux_autopilot` | bool | `true` | Auto-unwatch sessions after inactivity notification, auto-watch on send. |
| `tmux_watch_threshold` | string | `"30s"` | Default inactivity watch threshold. Go duration format. |
| `web_search_max_uses` | int | `0` | Max Anthropic web searches per API call. `0` = unlimited. Only applies when `search_provider = "anthropic"`. |
| `web_search_allowed_domains` | string[] | `[]` | Domain whitelist for Anthropic web search. Mutually exclusive with `web_search_blocked_domains`. |
| `web_search_blocked_domains` | string[] | `[]` | Domain blacklist for Anthropic web search. Mutually exclusive with `web_search_allowed_domains`. |
| `web_fetch_max_uses` | int | `0` | Max Anthropic web fetches per API call. `0` = unlimited. Only applies when `fetch_provider = "anthropic"`. |
| `web_fetch_allowed_domains` | string[] | `[]` | Domain whitelist for Anthropic web fetch. Mutually exclusive with `web_fetch_blocked_domains`. |
| `web_fetch_blocked_domains` | string[] | `[]` | Domain blacklist for Anthropic web fetch. Mutually exclusive with `web_fetch_allowed_domains`. |

The `summary` tool uses `claude-haiku-4-5` hardcoded (always cheap/fast) and has no configurable options.

Tmux memory monitoring detects runaway memory from long-running tmux sessions (glibc malloc fragmentation). Notifications are sent to agents whose `inject_agent_warnings` is `false` — agents with injection enabled already see log warnings in their session.

### `[skills]`

Skill directories to scan on startup. Per-agent override: `skills_dirs` in `[[agents]]` — see [Global-or-Agent: Skills & Message Transforms](#skills--message-transforms).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `dirs` | string[] | `[]` | Directories to scan for skill subdirectories containing `SKILL.md` files. |

Each subdirectory with a `SKILL.md` is loaded. The skill name and description (from YAML frontmatter) are injected into the system prompt. Skills with `command` + `script` frontmatter auto-register as slash commands.

### `[[blocked_paths]]`

Configurable path prefixes that the `write` and `edit` tools will refuse to modify. When a write or edit targets a path under a blocked prefix, the tool returns the `rebuke` message as a successful result (not an error), nudging the agent to use a different approach (e.g. delegating to `claude` via tmux).

Per-agent override: `blocked_paths` in `[[agents]]` — see [Global-or-Agent: Skills & Message Transforms](#skills--message-transforms). Per-agent values replace (not merge with) global values.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `path` | string | required | Directory or file prefix to block. Resolved to absolute for matching. |
| `rebuke` | string | required | Message returned as the tool result when a blocked write/edit is attempted. |

Example:
```toml
[[blocked_paths]]
path = "/home/foci/myagent/code"
rebuke = "Do not write code directly. Use claude via tmux to make changes in the code directory."

[[blocked_paths]]
path = "/home/foci/myagent/config"
rebuke = "Config files are managed externally. Describe the change you want and the human will apply it."
```

This is separate from the security-based path blocking in `secrets.toml` (which returns hard errors). Config blocked paths are a soft operational guardrail.

### `[[commands]]`

Custom slash commands. Each entry is a `[[commands]]` table array.

**Inline keyboards:** Built-in commands with parameters (`/model`, `/thinking`, `/effort`, `/config`, `/sessions`, `/tmux`) show inline keyboard buttons when invoked bare. No configuration needed.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `name` | string | required | Command name (without `/`). |
| `description` | string | `""` | Shown in `/help` output. |
| `script` | string | required | Shell command to execute. |
| `timeout` | int | `10` | Timeout in seconds. |

Example:
```toml
[[commands]]
name = "deploy"
description = "Deploy the latest build"
script = "/home/foci/scripts/deploy.sh"
timeout = 30
```

### `[[message_transforms]]`

Global regex find/replace rules applied to inbound user messages before command dispatch. Per-agent override: `message_transforms` in `[[agents]]` — see [Global-or-Agent: Skills & Message Transforms](#skills--message-transforms).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `find` | string | required | Go regex pattern to match. |
| `replace` | string | required | Replacement string. Supports `$1`, `$2`, etc. for capture groups. |

Rules run in sequence — the output of one becomes the input of the next. Transforms fire before command dispatch, so a transform can produce a command (e.g. `m` → `/mana`). Messages that are already recognized commands are not transformed.

Example:
```toml
[[message_transforms]]
find = '(?is)^((why|when|what|how|where|who|did|does|do|is|are|was|were|can|could|would|should)\b.*\?\s*)$'
replace = "Questions are just requests for information.\n-------\n$1"

[[message_transforms]]
find = '(?i)^((can we|could we|should we)\b.*)'
replace = "This is a question, not an instruction.\n-------\n$1"
```

Invalid regex patterns are logged as errors and skipped.

---

## 2. Global-or-Agent Configuration

Fields that can be set globally and overridden per-agent in `[[agents]]`. Each field is documented once.

**Resolution order:** agent value > `[defaults]` value > global section value > hardcoded default.

Set global defaults in `[defaults]`:
```toml
[defaults]
model = "claude-sonnet-4-5"
max_tool_loops = 50
effort = "low"
thinking = "adaptive"
system_files = ["IDENTITY.md", "SOUL.md", "COHERENCE.md"]
```

Override per-agent in `[[agents]]`:
```toml
[[agents]]
id = "research"
model = "claude-haiku-4-5"
max_tool_loops = 25
effort = "high"
```

### Model & Response

Set in `[defaults]`, overridable per-agent.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `model` | string | `"claude-haiku-4-5"` | Anthropic model ID for API calls. |
| `max_output_tokens` | int | `8192` | Maximum tokens in model response. Larger values allow longer responses. |
| `max_tool_loops` | int | `25` | Maximum tool iterations per agent turn. Complex tasks may need more. |
| `effort` | string | `""` | Effort level for API requests: `"low"`, `"medium"`, `"high"`. `""` omits (uses API default). Overridable at runtime via `/effort`. Per-session overrides persist across restarts via state store and reset when a new session starts. |
| `thinking` | string | `""` | Thinking mode: `"adaptive"` enables adaptive extended thinking (Opus 4.6). `""` or `"off"` = disabled. Overridable at runtime via `/thinking`. Per-session overrides persist across restarts via state store. Thinking tokens count toward mana. |
| `system_files` | string[] | see below | Ordered list of workspace files to load as system prompt blocks. |

Default `system_files` order (most-stable first for cache efficiency):
```
["IDENTITY.md", "SOUL.md", "COHERENCE.md", "AGENTS.md", "TOOLS.md", "USER.md", "MEMORY.md", "KEEPALIVE.md"]
```

Missing files are silently skipped. The last file gets the cache breakpoint marker.

### Braindead Warning

Set in `[defaults]`, overridable per-agent.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `braindead_warning_enable` | bool | `true` | Enable braindead warning injection. `false` disables. |
| `braindead_warning_threshold` | int | `10` | Consecutive tool-call loops before injecting a braindead warning. `0` disables. |
| `braindead_warning_prompt` | string | `""` | Custom warning text injected when the threshold is hit. `""` uses a hardcoded default. |
| `turn_lock_warn_threshold` | string | `"3m"` | Warn if turn lock wait exceeds this duration. Go duration format. `proactive_warning` triggers are excluded. |

### Display

Set in `[defaults]`, overridable per-agent.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `show_tool_calls` | string | `"off"` | Tool call display mode: `"off"` (hidden), `"preview"` (shown then overwritten by reply), `"full"` (shown and kept; reply is a separate message). Accepts bool for backwards compat (`true` → `"preview"`, `false` → `"off"`). |
| `show_thinking` | string | `"off"` | Thinking block display mode: `"off"` (stripped), `"compact"` (toggle button), `"true"` (always shown). Accepts bool (`true` → `"true"`, `false` → `"off"`). |
| `display_width` | int | `44` | Character width for divider lines in thinking display. |

### Message Handling

Set in `[defaults]`, overridable per-agent.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `duplicate_messages` | bool | `false` | Send user text twice per API call. Can improve instruction following. |
| `batch_partial_assistant_messages` | bool | `false` | When `false`, text in mid-turn responses (alongside tool calls) is sent to Telegram immediately. When `true`, text is accumulated and returned concatenated when the turn completes. |

### Compaction & Sessions

Global defaults set in `[sessions]`, overridable per-agent. Per-agent `unset` inherits from `[sessions]`.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `compaction_threshold` | float | `0.8` | Trigger compaction when context usage exceeds this fraction (0.0–1.0). |
| `compaction_summary_prompt` | string | `""` | Path to prompt file for compaction summary. Read live at compaction time (edits take effect immediately). `""` uses embedded default. |
| `compaction_handoff_msg` | string | see below | Message injected after the summary to orient the agent post-compaction. |
| `compaction_notify` | bool | `true` | Send a Telegram notification when compaction occurs. |
| `compaction_debug` | bool | `false` | Send the compaction summary to Telegram as a markdown file attachment after compaction completes. Useful for verifying what survived the cut. |
| `compaction_preserve_messages` | int | `25` | Preserve the last N messages through compaction. Preserved messages are appended verbatim after the summary + handoff, keeping their original roles. `0` disables (summary only). The summarizer only sees messages *before* the preserved window. |
| `compaction_effort` | string | `""` | Effort level for compaction API calls: `"low"`, `"medium"`, `"high"`. `""` uses session effort. Useful when agent uses low effort for chat but needs higher quality for compaction. |
| `session_reset_prompt` | string | `""` | Path to session reset prompt file. `""` uses embedded default. |
| `branch_orientation_prompt` | string | `""` | Path to prompt file injected into all branch sessions (multiball, cron, spawn). Supports template variables `{branch_key}`, `{parent_key}`, `{branch_type}`, `{direct_chat}`. `""` uses embedded defaults from `prompts/branch-orientation-headless.md` or `prompts/branch-orientation-multiball.md`. |

### Tool Behavior

Global defaults set in `[tools]` (or `[defaults]` where noted), overridable per-agent. Per-agent `0` inherits from `[tools]`.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `max_result_chars` | int | `15000` | Max characters in a tool result before writing to a temp file and returning a guard message (no partial content). Global: `[tools]` or `[defaults]`. |
| `max_summary_chars` | int | `300000` | Max chars to auto-summarise via Haiku. Results larger than this are saved to file with hints but skip the summary call. Global: `[tools]` or `[defaults]`. |
| `auto_summarise` | bool | `true` | Auto-summarise oversized tool results via Haiku. `false` skips summary calls entirely (results are saved to file with hints instead). Global: `[tools]` or `[defaults]`. Per-agent `unset` inherits from `[tools]`. |
| `exec_auto_background` | int | `10` | Seconds before auto-backgrounding long-running exec and http_request calls. `0` disables. Global: `[tools]`. |
| `max_concurrent_spawns` | int | `3` | Max concurrent `spawn` clone_current sessions per agent. Global: `[tools]`. |
| `max_upload_file_size` | int | `52428800` | Max file size in bytes for multipart/form-data file uploads (default 50MB). Global: `[tools]`. |
| `search_provider` | string | `"brave"` | Web search provider: `"brave"` (client-side, needs `brave_api_key`) or `"anthropic"` (server-side). Brave is recommended: Anthropic's server-side search returns encrypted content blobs that massively inflate token counts (observed: 256k tokens from just two searches) and bypass the tool result size guard entirely. Brave results are client-side, guardable, and far more token-efficient. Global: `[tools]` or `[defaults]`. |
| `fetch_provider` | string | `"builtin"` | Web fetch provider. See [TOOLS.md](TOOLS.md) for provider details. Global: `[tools]` or `[defaults]`. |

### Notifications & Logging

| Key | Type | Default | Global location | Description |
|-----|------|---------|-----------------|-------------|
| `startup_notification` | bool | `true` | `[telegram] enable_startup_notify` | Send a startup notification when the service starts. `false` for silent bots (e.g. cron-only agents). |
| `inject_agent_warnings` | bool | `false` | `[defaults]` | Feed WARN/ERROR log events into this agent's conversation as system warnings before each turn. Per-agent — some agents can have injection enabled while others rely on Telegram notifications. |
| `messages_in_log` | bool | `false` | `[logging]` | Log user message content to the event log. When `false`, messages are logged at DEBUG level with no content for privacy. When `true`, messages are logged at INFO level with content (truncated to 100 chars). Per-agent `unset` inherits from global. |

### Telegram Overrides

| Key | Type | Default | Global location | Description |
|-----|------|---------|-----------------|-------------|
| `allowed_users` | string[] | `[]` | `[telegram]` | Telegram user IDs allowed to interact with bots. `[]` falls back to global `[telegram] allowed_users`. |
| `received_files_dir` | string | `$workspace/received_files` | `[telegram]` | Save received media (images, videos, video notes, documents) to this directory. `""` in global disables. Per-agent defaults to `$workspace/received_files`. Relative paths resolve against `$HOME`. Filename formats — Images: `YYYY-MM-DDTHH-MM-SSZ_chat-CHATID.ext`. Videos: `YYYY-MM-DDTHH-MM-SSZ_video_chat-CHATID.ext`. Video notes: `YYYY-MM-DDTHH-MM-SSZ_videonote_chat-CHATID.mp4`. Documents: `YYYY-MM-DDTHH-MM-SSZ_document_chat-CHATID.ext`. The agent sees `[Image/Video/Document saved to: /path/to/file]` in the message text. Files over 20MB (Telegram Bot API limit) show `[Video/Document too large to download (N MB)]` instead. |

### Voice

| Key | Type | Default | Global location | Description |
|-----|------|---------|-----------------|-------------|
| `tts_rate` | float | `0` | `[voice]` / `[defaults]` | TTS speech rate multiplier. `1.3` = 30% faster, `0.8` = 20% slower. `0` uses `[voice] tts_rate` config. Translated automatically for each provider (edge-tts `--rate "+30%"`, openai `speed: 1.3`). |

### Keepalive (`[keepalive]` / `[[agents.keepalive]]`)

Cache keepalive timer. Fires a lightweight branch session to keep the Anthropic cache prefix warm.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Enable keepalive timer. |
| `interval` | string | `"55m"` | Time since cache last warmed before firing. Should be < 1h (Anthropic cache TTL). |
| `prompt` | string | `""` | Prompt file path. `""` = embedded default, `"default"` = embedded, `"none"` = disabled, `/path` = custom file. |

### Background (`[background]` / `[[agents.background]]`)

Mana-gated background work timer. Fires when the user is idle, there are open background-tagged todos, and the manamometer says spending is wise.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Enable background work timer. |
| `interval` | string | `"5m"` | Time since last interaction before firing. |
| `prompt` | string | `""` | Prompt file path. `""` = embedded default, `"default"` = embedded, `"none"` = disabled, `/path` = custom file. |
| `invest_interval` | string | `"30m"` | Quiet period after mana reset to let cache invest before spending. |

The following field is **global-only** (not overridable per-agent):

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `mana_staleness_timeout` | string | `"10m"` | Max age of mana usage reading before considering it stale. Stale readings block background spending. |

**Validation warnings:**
- `background.interval > keepalive.interval` — keepalive resets the cache timer; background work may never trigger.
- `keepalive.interval > 1h` — Anthropic cache TTL is 1 hour; cache may expire between keepalives.

See [HEARTBEAT.md](HEARTBEAT.md) for full details on the manamometer and timer logic.

### Memory Formation (`[memory_formation]` / `[[agents.memory_formation]]`)

Automatic memory capture and MEMORY.md consolidation. All three sub-features default to enabled.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `interval_enabled` | bool | `true` | Enable periodic memory capture on timer. |
| `interval` | string | `"1h"` | Time between interval captures. |
| `interval_prompt` | string | `""` | Prompt override. `""` = embedded `memory-formation.md`, `"none"` = disabled, `/path` = custom file. |
| `consolidation_enabled` | bool | `true` | Enable periodic MEMORY.md curation. |
| `consolidation_interval` | string | `"20h"` | Minimum time between consolidation runs. Persisted across restarts. |
| `consolidation_prompt` | string | `""` | Prompt override. `""` = embedded `memory-consolidation.md`, `"none"` = disabled, `/path` = custom file. |
| `session_end_enabled` | bool | `true` | Run memory formation on `/reset` and multiball reclaim. |
| `session_end_prompt` | string | `""` | Prompt override. `""` = embedded `memory-formation.md`, `"none"` = disabled, `/path` = custom file. |

All prompt fields use 3-state resolution: `""` or `"default"` → embedded default from `prompts/`, `"none"` → disabled, file path → read file with embedded fallback on error.

**Interval memory formation** runs in the keepalive timer loop. Fires when:
1. `interval` has elapsed since the last formation
2. There's been user activity since the last formation
3. The user has been active within the interval window

**Consolidation** reviews daily memory files and curates MEMORY.md. The last-run timestamp is persisted in state, so it survives restarts. Only fires when there's been user activity within the last hour.

**Session-end** fires asynchronously on `/reset` and multiball reclaim. Creates a branch from the expiring session (preserving conversation history) so the caller doesn't block.

### Usage Warnings (`[[agents.usage_warnings]]`)

Per-agent mana warning thresholds. When set, completely replaces the global `[usage_warnings] thresholds` for this agent.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `thresholds` | int[] | `[]` | Mana warning thresholds. `[]` inherits from global `[usage_warnings]`. When set, completely replaces global thresholds for this agent. |
| `restore_threshold` | int | `0` | Inject session notice when mana restores to 100% after being below this threshold. `0` disables. |

### Skills & Message Transforms

| Key | Type | Default | Global location | Description |
|-----|------|---------|-----------------|-------------|
| `skills_dirs` | string[] | `[]` | `[skills] dirs` | Directories to scan for skill subdirectories. `[]` inherits from global `[skills] dirs`. |
| `message_transforms` | array | `[]` | `[[message_transforms]]` | Regex find/replace rules applied to inbound messages. `[]` inherits from global `[[message_transforms]]`. |
| `blocked_paths` | array | `[]` | `[[blocked_paths]]` | Path prefixes blocked for write/edit tools. `[]` inherits from global `[[blocked_paths]]`. Per-agent replaces global (not merged). |

---

## 3. Agent-Only Configuration

Fields that only exist per-agent in `[[agents]]`. These have no global equivalent.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `id` | string | `""` | Agent identifier. Used in session keys (`agent:ID:main`). |
| `name` | string | capitalised `id` | Human-readable name (e.g. `"Clutch"`). Defaults to capitalised agent ID (e.g. `clutch` → `Clutch`). Used in `/voice` WebSocket agent list. |
| `emoji` | string | `""` | Emoji for agent (e.g. `"🥔"`). Used in `/voice` WebSocket agent list. |
| `workspace` | string | `$HOME/$id` | Path to workspace directory containing character files (IDENTITY.md, SOUL.md, etc.). Defaults to `$HOME/<agent-id>` if not set. |
| `telegram_bot` | string | `$id` | Bot name for this agent. Token resolved from secret `"telegram.<bot>"`. Defaults to agent ID. |
| `bot_secret` | string | `""` | Override secret key for bot token. `""` uses `"telegram.<telegram_bot>"`. |
| `multiball_bots` | string[] | `[]` | Per-agent multiball bot pool. Tokens resolved via `"telegram.<name>"` secret convention. Tried before the shared `[telegram] multiball_bots` pool. |

### Memory (`[[agents.memory.sources]]`)

Agents can have their own memory directories in addition to the global sources. Global `[[memory.sources]]` are always prepended to each agent's sources — agents inherit global sources automatically. When any agent has per-agent memory configured, each agent gets its own FTS5 index (`memory-{agentID}.db`) combining global + agent-specific sources.

Agent-specific sources automatically receive a weight boost of +1.0, so they rank higher than global sources with the same base weight. Source names are prefixed with `agent:` in search results.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `memory.sources` | array | see below | Per-agent memory directories. Combined with global `[memory]` sources. When empty, defaults to a single source: `{name: $id, dir: $workspace/memory, weight: 1.0}`. |

Each source entry:

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `name` | string | required | Source identifier (prefixed with `agent:` in results). |
| `dir` | string | required | Directory path to index. |
| `weight` | float | `1.0` | Base weight (boosted by +1.0 automatically). |

When no agent has per-agent memory sources, a single shared index (`memory.db`) is used — fully backward compatible.

### Multi-Agent Example

```toml
# Global memory (shared by all agents)
[[memory.sources]]
name = "shared"
dir = "/home/foci/shared/memory"
weight = 1.0

# Shared multiball pool (fallback for any agent)
[telegram]
multiball_bots = ["spare1"]

[[agents]]
id = "main"
model = "claude-sonnet-4-5"
workspace = "/home/foci/character"
telegram_bot = "primary"
multiball_bots = ["mainling"]  # per-agent multiball pool

[[agents.memory.sources]]
name = "workspace"
dir = "/home/foci/clutch/memory"
weight = 1.0    # effective weight: 2.0 (1.0 + 1.0 boost)

[[agents]]
id = "research"
model = "claude-haiku-4-5"
workspace = "/home/foci/character"
telegram_bot = "secondary"
# no multiball_bots — uses shared pool only

[[agents.memory.sources]]
name = "workspace"
dir = "/home/foci/scout/memory"
weight = 1.0
```

**Multiball acquisition priority:** When `/multiball` is invoked, per-agent pool is tried first. If all per-agent bots are busy (or none configured), the shared pool is used as fallback. Released bots return to whichever pool they came from.

---

## Path Resolution

All path config fields are resolved at startup:

1. **Absolute paths** are used as-is
2. **Relative paths** resolve against `$HOME` (not the config directory, not CWD)
3. **`data_dir`** controls data file placement — DB, state, and session files resolve against it. `""` defaults to `$HOME/data/`

### Default zero-config layout

With no path fields set, files auto-organize under `$HOME`:

```
$HOME/
  logs/foci.log          ← event log
  logs/api.jsonl         ← API call log (JSONL)
  logs/api-payload.jsonl ← full payload log (if enabled)
  data/api.db            ← API call log (SQLite)
  data/conversation.db   ← conversation SQLite log
  data/sessions/         ← session JSONL files
  data/state.json        ← persistent state
  data/memory.db         ← memory FTS index
  data/reminders.db      ← reminder store (per-agent via agent_id)
  data/scratchpad.db     ← scratchpad store (per-agent via agent_id)
  data/todo.db           ← todo store (per-agent via agent_id)
  data/WELCOME.md        ← welcome/changelog file
```

### Overriding with `data_dir`

```toml
data_dir = "/opt/foci/data"
```

All data files (`*.db`, `state.json`, `sessions/`) resolve under `/opt/foci/data/`. Log files are unaffected — they use their own paths.

A relative `data_dir` resolves against `$HOME`:

```toml
data_dir = "myapp/data"   # → $HOME/myapp/data/
```

### Explicit absolute paths

Any field set to an absolute path overrides all resolution:

```toml
[logging]
event_file = "/var/log/foci/foci.log"
api_file = "/var/log/foci/api.jsonl"
conversation_file = "/var/data/foci/conversation.db"

[sessions]
dir = "/var/data/foci/sessions"
```

---

## Recommended Directory Layout

For new installs, `setup.sh` creates this structure:

```
/home/foci/
  config/            — foci.toml, secrets.toml
  data/              — *.db, sessions/, .foci-commit, state.json, WELCOME.md
  logs/              — foci.log, api.jsonl, api-payload.jsonl
  shared/            — skills/, scripts/
  character/         — agent workspace (IDENTITY.md, SOUL.md, memory/, etc.)
```

The key config fields that wire this up:

```toml
data_dir = "/home/foci/data"

[sessions]
dir = "/home/foci/data/sessions"

[logging]
event_file = "/home/foci/logs/foci.log"
api_file = "/home/foci/logs/api.jsonl"
conversation_file = "/home/foci/data/conversation.db"

[skills]
dirs = ["/home/foci/shared/skills"]

welcome_file = "/home/foci/data/WELCOME.md"
```

Existing flat-layout installs continue to work unchanged. To migrate, run `scripts/migrate-homedir.sh`.

---

## `secrets.toml`

Credentials file. Lives alongside `foci.toml`. Protected at the OS level by the `foci-secrets` group — see [SECRETS.md](SECRETS.md) for the full security model and setup instructions.

```toml
[anthropic]
# Written by `foci auth` (from `claude setup-token`):
setup_token = "sk-ant-oat01-..."
# Or standard API key:
# api_key = "sk-ant-api03-..."

[telegram]
bot_token = "123456:ABC..."
primary = "123456:ABC..."
secondary = "789012:DEF..."

[brave]
api_key = "BSA..."

[groq]
api_key = "gsk_..."

[openrouter]
api_key = "sk-or-..."

[voice]
api_key = "your-voice-api-key"

[custom]
github_token = "ghp_..."
allowed_hosts = ["api.github.com"]
```

All secrets override their corresponding `foci.toml` values.

### `allowed_hosts`

Each section can include an `allowed_hosts` array restricting which hosts that section's secrets can be sent to via the `http_request` tool. Secrets without `allowed_hosts` can only be used in exec commands.

```toml
[myapi]
token = "sk-..."
allowed_hosts = ["api.example.com", "api.backup.example.com"]
```

Host matching is case-insensitive (per RFC 4343). Ports are ignored — `api.example.com:8443` matches `api.example.com`. See [SECRETS.md](SECRETS.md) for the full security model.

### Per-agent overrides

Agents can have their own secret values via `[agents.ID.section]` tables. Agent-specific values override globals for the same key; keys not overridden fall back to globals. Each agent only sees its own overrides.

```toml
[custom]
github_token = "ghp_default"
allowed_hosts = ["api.github.com"]

[agents.fotini.custom]
github_token = "ghp_fotini_account"

[agents.fotini.myapi]
token = "sk-fotini-api"
allowed_hosts = ["api.fotini.com"]
```

In this example, agent `fotini` sees `custom.github_token = "ghp_fotini_account"` (override) and inherits `custom.allowed_hosts` from the global section. It also gets the additional `myapi.token` secret. Other agents see the global `ghp_default` value and do not see `myapi.token`.

Per-agent scoping applies to: exec `{{secret:NAME}}` templates, `http_request` secret resolution, output redaction, and system prompt secret names. Built-in credential resolution (anthropic.setup_token, anthropic.api_key, telegram bot tokens, brave API key) remains global — these are process-wide settings.

---

## Minimal Example

```toml
[agent]
id = "main"
model = "claude-haiku-4-5"
workspace = "/home/foci/character"

[telegram]
allowed_users = ["123456789"]

[sessions]
dir = "/home/foci/sessions"

[memory]
dir = "/home/foci/character/memory"

[logging]
level = "INFO"
```

With `secrets.toml`:
```toml
[anthropic]
setup_token = "sk-ant-..."

[telegram]
bot_token = "123456:ABC..."
```

---

## Full Example

```toml
[agent]
id = "main"
model = "claude-sonnet-4-5"
workspace = "/home/foci/character"
system_files = ["IDENTITY.md", "SOUL.md", "AGENTS.md", "TOOLS.md", "USER.md", "MEMORY.md", "KEEPALIVE.md"]

[telegram]
allowed_users = ["123456789"]

[sessions]
dir = "/home/foci/sessions"
compaction_threshold = 0.8

[memory]
dir = "/home/foci/character/memory"
reindex_debounce = "500ms"

[http]
port = 18791
bind = "127.0.0.1"

[logging]
level = "INFO"
event_file = "/home/foci/foci.log"
api_file = "/home/foci/api.jsonl"
conversation_file = "/home/foci/conversation.db"
full_payload = true
payload_file = "/home/foci/api-payload.jsonl"
cache_bust_detect = true

[voice]
stt_endpoint = "https://api.groq.com/openai/v1/audio/transcriptions"
stt_model = "whisper-large-v3"
tts_provider = "openai"
tts_endpoint = "https://openrouter.ai/api/v1"
tts_model = "openai/tts-1-mini"
tts_voice = "alloy"
tts_rate = 1.2

[tools]
tmux_cols = 300
tmux_rows = 30

[skills]
dirs = ["/home/foci/skills"]

[[commands]]
name = "reheat"
description = "Clear API cooldowns"
script = "/home/foci/scripts/reheat.sh"
timeout = 10
```
