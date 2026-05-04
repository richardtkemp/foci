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
2. **Global-or-agent** — set globally (in `[defaults.*]` sub-sections or a parent section like `[sessions]`, `[tools]`) and optionally overridden per-agent in `[[agents]]`. Documented once below.
3. **Agent-only** — set only per-agent in `[[agents]]`. No global equivalent.

**Resolution order** for global-or-agent fields: agent value > `[defaults.*]` value > global section value > hardcoded default.

**`[defaults]` uses named sub-sections.** Fields are grouped by concern: `[defaults.loop]`, `[defaults.nudge]`, `[defaults.voice]`, `[defaults.display]`, `[defaults.notify]`, `[defaults.behavior]`, `[defaults.system]`. Per-agent overrides use the same sub-section names directly on `[[agents]]` (e.g. `[agents.loop]`, `[agents.nudge]`).

**Unset convention:** Throughout this document, `unset` means the field is not present in TOML. For optional/pointer fields, `unset` triggers inheritance from the parent section. For value fields, the listed default applies. Zero values (`0`, `""`, `[]`) that mean "inherit from global" are noted explicitly in the description.

---

## 1. Global-Only Configuration

Fields that exist only at the top level or in dedicated global sections. These cannot be overridden per-agent.

### Top-Level Keys

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `data_dir` | string | `$HOME/data` | Directory for shared databases (api.db, state.db), sessions, and state files. Per-agent databases (reminders, scratchpad, todo, tasklist, conversation, memory indices) are stored in each agent's `workspace/.data/` directory. Relative paths resolve against `$HOME`. Absolute paths used as-is. |
| `welcome_file` | string | `"data/WELCOME.md"` | Path to a changelog/welcome file. If this file exists on startup, its contents are injected into the first agent's main session and the file is deleted. Relative paths resolve against `$HOME`. |
| `skip_security_checks` | bool | `false` | Skip startup security checks for `secrets.toml` (ownership, permissions, group membership). Useful for development environments. See [SECRETS.md](SECRETS.md). |
| `file_mode` | string | `"0640"` | Octal file permissions for workspace and content files (character files, prompts, skills, tool-written files, media saves, config edits). Session files have their own `[sessions] file_mode`. |
| `timezone` | string | `""` | IANA timezone for timestamps (e.g. `"Europe/Athens"`, `"UTC"`, `"Local"`). Empty defaults to machine local time. |

### `[anthropic]`

Anthropic API settings. API keys go in `secrets.toml` — see [AUTH.md](AUTH.md) for setup guide.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `usage_api_timeout` | string | `"10s"` | HTTP timeout for usage API calls. Go duration format. |
| `usage_cache_ttl` | string | `"10m"` | Cache TTL for usage API responses. All callers (mana monitor, turn metadata, /mana command) share a single cache. On fetch errors, retries use exponential backoff (starting at cache TTL, doubling up to 1h). |
| `cc_expiry_threshold` | string | `"5m"` | How far before expiry to trigger a proactive token refresh. Credentials are read lazily from `~/.claude/.credentials.json` on each API call. |
See [AUTH.md](AUTH.md) for token resolution order and setup guide.

### `[gemini]`

No fields remain — `cache_ttl` is now per-model via `[models.*]`. Requires `gemini.api_key` in `secrets.toml`. Set `powerful = "gemini/gemini-2.5-flash"` in `[groups]` to use.

### `[openai]`

No fields — `base_url` is configured per-endpoint via `[endpoints.openai].url` (or custom endpoints). Requires `openai.api_key` in `secrets.toml`. Set `powerful = "openai/gpt-4o"` in `[groups]` to use. The SDK provides built-in retries with exponential backoff on 429/5xx errors.

### `[[platforms]]`

Platform configuration. Each entry defines a platform (telegram, discord, etc.) with an `id` field. All fields follow the 5-level cascade: per-agent platform → per-agent → global platform → `[defaults.*]` → code default.

```toml
[[platforms]]
id = "telegram"
[platforms.access]
allowed_users = ["123456"]
[platforms.display]
show_tool_calls = "preview"
[platforms.telegram]
long_poll_timeout = "65s"

[[platforms]]
id = "discord"
[platforms.access]
allowed_users = ["789012"]
[platforms.discord]
auto_thread = true
```

#### Direct fields (no subsection)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `id` | string | required | Platform identifier: `"telegram"`, `"discord"`, etc. |
| `bot` | string | `""` | Bot name. Token resolved from secret `"<platform>.<bot>"`. |
| `bot_secret` | string | `""` | Override secret key for bot token. `""` uses `"<platform>.<bot>"`. |
| `facet_bots` | string[] | `[]` | Shared facet bot pool. Bot tokens resolved via `"<platform>.<name>"` secret convention. |
| `facet_session_ttl` | string | `"60m"` | Idle TTL before a facet bot/thread can be reclaimed. `"0"` disables. |
| `message_queue_size` | int | `64` | Message queue buffer size. |

#### Access fields (`[platforms.access]`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `allowed_users_only` | bool | `true` | When true, `allowed_users` must be non-empty or the platform won't start. When false, the platform starts without `allowed_users` and accepts messages from any user. |
| `allowed_users` | string[] | `[]` | User IDs allowed to interact with the bot. |
| `require_mention` | bool | `true` | Require @mention in group chats. DMs are always processed. |

#### Display fields (`[platforms.display]`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `show_tool_calls` | string | `"off"` | Tool call display: `"off"`, `"preview"`, `"full"`. |
| `show_thinking` | string | `"off"` | Thinking display: `"off"`, `"compact"`, `"true"`. |
| `display_width` | int | `44`/`60` | Character width for dividers. Default varies by platform. |
| `stream_output` | bool | `false` | Stream model output in real-time. |
| `stream_interval` | string | `"250ms"`/`"1200ms"` | Duration between message edits during streaming. Default varies by platform. |
| `streaming` | bool | `false` | Use streaming API. Per-platform override for `[display] streaming`. |
| `received_files_dir` | string | `""` | Save received files to this directory. Empty disables. |
| `injected_message_header` | string | `"[[ System message ]]"` | Header prepended to injected/system messages. Empty disables. |

#### Notify fields (`[platforms.notify]`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `startup_notify` | bool | `true` | Send notification on startup. |
| `compaction_notify` | bool | `true` | Send notification on compaction. |
| `task_list_notify` | bool | `true` | Send notification on task list changes. |
| `compaction_debug` | bool | `false` | Send compaction summary as file attachment. |
| `warning_max_per_window` | int | `3` | Max identical warnings allowed per time window before suppression. `0` disables rate-limiting. |

#### Debug fields (`[platforms.debug]`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `inject_agent_warnings` | string | `"off"` | Inject warnings into agent session: `"all"`, `"errors"`, `"off"`. From `[debug]`. |
| `inject_chat_warnings` | string | `"off"` | Send warnings as chat notifications: `"all"`, `"errors"`, `"off"`. From `[debug]`. |
| `compaction_debug` | bool | `false` | Send compaction summary as file attachment. From `[debug]`. |
| `log_api_key_suffix` | bool | `false` | Log last 4 chars of API keys. From `[debug]`. |
| `messages_in_log` | bool | `false` | Log user message content. From `[debug]`. |

#### Telegram-specific fields (`[platforms.telegram]`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `long_poll_timeout` | string | `"65s"` | Long-poll timeout for `getUpdates`. Should exceed 60s. |
| `table_wrap_lines` | int | `5` | Max wrapped lines per table cell. `0` truncates with `…`. |
| `table_style` | string | `"pretty"` | Table style: `"pretty"` or `"markdown"`. |

#### Discord-specific fields (`[platforms.discord]`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `auto_thread` | bool | `true` | Create threads for facet sessions. |
| `guild_id` | string | `""` | Restrict to a single guild. Empty allows all guilds. |

#### Bot token resolution

Bot tokens are resolved by convention: `"<platform>.<botname>"` in `secrets.toml`. No explicit bot map is needed.

For example, an agent with `bot = "primary"` on a telegram platform resolves its token from the secret key `telegram.primary`. To override the convention, set `bot_secret` on the platform entry.

`secrets.toml`:
```toml
[telegram]
primary = "123456:ABC..."
secondary = "789012:DEF..."

[discord]
primary = "MTIzNDU2Nzg5..."
```

#### Provider-driven defaults

Platform defaults (display_width, stream_interval, etc.) are supplied by each platform's provider implementation, not hardcoded in config loading. Adding a new platform just requires implementing the `MessagingProvider` interface — no config loader changes needed.

### `[http]`

HTTP API server.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `port` | int | `18791` | HTTP server port. |
| `bind` | string | `"127.0.0.1"` | Bind address. Use `0.0.0.0` for external access. |
| `graceful_shutdown_timeout` | string | `"30s"` | Time to wait for in-flight requests on shutdown. Go duration format. |
| `ws_enabled` | bool | `false` | Enable `/voice` WebSocket endpoint. |
| `socket_path` | string | `""` | Unix socket path for same-user auth. Empty (default) auto-resolves to `~/data/foci-gw.sock`. Same-user connections via this socket are authenticated by the kernel using `SO_PEERCRED` — no API key needed. |

Endpoints: `POST /send`, `GET /status`, `POST /command`, `POST /wake`, `POST /webhook/{agent}/{hookid}`, `GET /voice` (WebSocket, when `[http] ws_enabled = true`).

All endpoints accept an `agent` field (JSON body for POST, query param for GET) to target a specific agent by ID. When empty or omitted, the first configured agent is used. The `/send` endpoint also accepts an optional `session` field to target a specific session key (defaults to `main`).

#### CLI (`foci` command)

The `foci` CLI wraps the HTTP API. All subcommands accept `-a <id>` / `--agent <id>` to target a specific agent. The `send` command also accepts `-s <session>` / `--session <id>` to target a specific session:

```
foci send -a research "check the news"
foci send -a clutch -s research "text"  # routes to clutch/i0/0 (research session)
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
| `conversation_log` | bool | `true` | Enable per-agent conversation logging. Each agent's database is stored at `workspace/.data/conversation.db`. |
| `full_payload` | bool | `false` | Write full API request/response bodies to `payload_file`. |
| `payload_file` | string | `"logs/api-payload.jsonl"` | Path for full payload log. Only used when `full_payload = true`. Relative paths resolve against `$HOME`. |


| `warning_window_duration` | string | `"5m"` | Time window for warning deduplication. Go duration format. |
| `warning_proactive_active_interval` | string | `"5m"` | Min interval between proactive warning turns when user is active. Go duration format. |
| `warning_proactive_inactive_interval` | string | `"1h"` | Min interval between proactive warning turns when user is inactive. Go duration format. |
| `warning_proactive_activity_threshold` | string | `"10m"` | User is "active" if last message within this window. Go duration format. |
| `log_rotation` | bool | `true` | Enable built-in log rotation. |
| `rotation_period` | string | `"24h"` | How often to check and rotate logs. Go duration format. |
| `retention_period` | string | `"48h"` | Keep lines newer than this in the active log. Older lines archived to gzip. |
| `rotation_max_line_size` | string | `"64MB"` | Max line size for the rotation scanner buffer. Accepts `KB`, `MB`, `GB` suffixes. If a log line exceeds this size, rotation fails and that log file won't be rotated. |
| `archive_dir` | string | `""` | Directory for gzip archives. `""` uses `logs/archive/`. |
| `log_file_mode` | string | `"0600"` | Octal file permissions for log files (event, API, payload). Applied on creation and after rotation. Use `"0640"` for group-readable logs. |

When `inject_agent_warnings` or `inject_chat_warnings` is enabled (per-agent), repeated identical warnings are deduplicated: after `warning_max_per_window` occurrences within `warning_window_duration`, further duplicates are suppressed and summarised as "... and N more in last Xm" on the next drain. Warning messages are normalised — IP addresses, hex strings, and multi-digit numbers are replaced with placeholders so semantically identical errors are grouped together.

### `[sessions]`

Session storage. Compaction and prompt fields that can be overridden per-agent are in [Global-or-Agent: Compaction & Sessions](#compaction--sessions).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `dir` | string | `$data_dir/sessions` | Directory for JSONL session files. Relative paths resolve against `$HOME`. |
| `compaction_max_tokens` | int | `4096` | Max output tokens for the compaction summary. |
| `compaction_min_messages` | int | `4` | Minimum messages in session before compaction is allowed. |
| `max_system_prompt_chars_file` | int | `20000` | Warn at startup and `/reload` if any system prompt file exceeds this many chars. `0` disables. |
| `max_system_prompt_chars_total` | int | `80000` | Warn at startup and `/reload` if total system prompt exceeds this many chars. `0` disables. |
| `archive_after` | string | `"24h"` | Gzip idle session files after this duration of inactivity. Go duration format. Each agent's most recently created chat session is never archived regardless of age. Sessions with active branches are also skipped. Archived sessions are transparently decompressed when accessed. `"0"` effectively disables (no sessions will be old enough). |
| `file_mode` | string | `"0600"` | Octal file permissions for session files (branches, appends, compacted rewrites). Applied at file creation time. Example: `"0640"` for owner read/write + group read. |

Sessions are stored as JSONL files at `{dir}/agent/{id}/{type}.jsonl`.

All prompt fields (`compaction_summary_prompt`, `branch_orientation_facet_prompt`, `branch_orientation_headless_prompt`) are file paths, not inline strings. If the file can't be read, a warning is logged and the embedded default is used. Prompt files are read live at the point of use — edits take effect immediately without restart or `/reload`.

When no config override is set, embedded defaults from `shared/prompts/` are used:
- `shared/prompts/branch-orientation-headless.md` — headless branches (cron, spawn, keepalive)
- `shared/prompts/branch-orientation-facet.md` — user-attached facet branches
- `shared/prompts/compaction-summary.md` — compaction summary prompt
- `shared/prompts/compaction-handoff.md` — post-compaction handoff message
- `shared/prompts/keepalive.md` — keepalive ping prompt
- `shared/prompts/background.md` — background work prompt
- `shared/prompts/reflection.md` — reflection pass prompt (memory + skill formation; interval, session-end, and compaction)
- `shared/prompts/memory-consolidation.md` — MEMORY.md consolidation prompt

### `[memory]`

Memory system (full-text search over markdown files + conversation history).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `search_backend` | string | `"bleve"` | Search backend: `"fts5"` (SQLite FTS5) or `"bleve"` (blevesearch/bleve). |
| `reindex_debounce` | string | `"0s"` | Delay before reindexing after file changes. Go duration format. |
| `conversation_weight` | float | `0.1` | Weight multiplier for conversation search results (0.0–1.0). Lower = conversation appears further down in results. FTS5 only — bleve does not index conversations. |
| `search_limit` | int | `20` | Maximum number of search results to return. |
| `sweep_interval` | string | `"1h"` | Periodic full reindex interval. Catches files added via git, rsync, or other mechanisms that bypass fsnotify. Go duration format. `"0"` disables. First sweep runs 30s after startup. |

When set, creates databases in the data directory (`$HOME/data/` by default): `memory.db` (FTS5), `memory.bleve/` (bleve), `reminders.db`, `scratchpad.db`. Only the active backends' databases are created.

When multiple backends are active, the `memory_search` tool exposes a `backend` parameter so the agent can choose which to query. When only one is active, the parameter is hidden.

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

### `[[tts]]`

Text-to-speech provider entries. Multiple entries are supported; the first is the default. Agents override by id via `tts = "id"` in their config.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `id` | string | `""` | Lookup key for agent overrides. |
| `format` | string | `""` | Provider format: `"openai"` or `"edge-tts"`. |
| `endpoint` | string | `""` | API endpoint URL (ignored for edge-tts). |
| `model` | string | `""` | Model name (ignored for edge-tts). |
| `voice` | string | `""` | Voice name (format-specific). `""` defaults to `"alloy"` for OpenAI. |
| `rate` | float | `0` | Speed multiplier: `1.3` = 30% faster, `0.8` = 20% slower. `0` means omit/default. |
| `secret` | string | `""` | Secret name in secrets.toml (e.g. `"groq.api_key"`). If empty, auto-detected from endpoint hostname. |
| `command` | string | `"edge-tts"` | Binary for edge-tts format. |
| `response_format` | string | `"wav"` | Audio format for OpenAI-compatible APIs: `"mp3"`, `"wav"`, `"opus"`, `"aac"`, `"flac"`. Groq only supports `"wav"`. |
| `replacements` | map | `{}` | Word replacements applied to text before synthesis. Case-insensitive whole-word matching; preserves original case pattern. Example: `{ foci = "foki" }`. |

### `[[stt]]`

Speech-to-text provider entries. Multiple entries are supported; the first is the default. Agents override by id via `stt = "id"` in their config.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `id` | string | `""` | Lookup key for agent overrides. |
| `format` | string | `""` | Provider format: `"openai"` (only supported format). |
| `endpoint` | string | `""` | API endpoint URL. |
| `model` | string | `""` | Model name (e.g. `"whisper-large-v3"`). |
| `secret` | string | `""` | Secret name in secrets.toml. If empty, auto-detected from endpoint hostname. |
| `replacements` | map | `{}` | Word replacements applied to transcribed text after transcription. Case-insensitive whole-word matching; preserves original case pattern. Example: `{ foki = "foci" }` (reverse of TTS replacements). |

API keys are resolved via the `secret` field or auto-detected from the endpoint hostname (e.g. `https://api.groq.com/...` → `groq.api_key` in secrets.toml). The `/voice` WebSocket endpoint is enabled via `[http] ws_enabled = true`.

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

### `mcp.toml`

Separate config file in the same directory as `foci.toml`. Defines MCP server connections. Missing file = no MCP servers (no error). The file is re-read on every MCP tool call, so changes take effect without restarting.

```toml
[[servers]]
name = "filesystem"
command = "npx"
args = ["-y", "@modelcontextprotocol/server-filesystem", "/home/user/docs"]

[[servers]]
name = "remote"
url = "https://mcp.example.com/sse"
agents = ["research", "assistant"]
```

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `name` | string | required | Unique server name (used in tool calls). |
| `command` | string | `""` | Command to start a stdio MCP server. Mutually exclusive with `url`. |
| `args` | string[] | `[]` | Arguments passed to `command`. |
| `env` | string[] | `[]` | Extra environment variables (`KEY=VALUE`). |
| `url` | string | `""` | HTTP endpoint for Streamable HTTP MCP server. Mutually exclusive with `command`. |
| `agents` | string[] | `[]` | Agent IDs that can use this server. Empty = all agents. |

### `[environment]`

Environment block injected as the first system prompt block, providing the agent with runtime context (workspace, paths, messaging platform, message metadata format).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `true` | Inject environment block as the first system block. `false` disables. |
| `docs_path` | string | `"shared/docs"` | Path to platform docs directory. Shown in environment block. Relative paths resolve against `$HOME`. |

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
| `goroutine_monitor_interval` | string | `"60s"` | Goroutine count check interval. Set to `"0"` to disable. Go duration format. |
| `goroutine_monitor_threshold` | int | `0` (auto) | Warn when `runtime.NumGoroutine()` exceeds this value. `0` = auto: `30 + 25×agents + 5×telegram_bots`. |

Both thresholds require memory pressure (PSI `avg10` from `/proc/pressure/memory` exceeding `memory_pressure_threshold`) before acting. This avoids false alarms when the system has ample free RAM despite high RSS. The guard reads `/proc` directly — no external commands.

### `[debug]`

Developer and debugging knobs. All off by default.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `log_api_key_suffix` | bool | `false` | Log the last 4 characters of API keys at DEBUG level on each provider API call. Applies to all providers (Anthropic, OpenAI, Gemini, voice) and secrets used in `http_request` tool calls. Useful for diagnosing which credential is being used when multiple keys are configured. |
| `messages_in_log` | bool | `false` | Log user message content to the event log. When `false`, messages are logged at DEBUG level with no content for privacy. When `true`, messages are logged at INFO level with content (truncated to 100 chars). Per-agent override via `[agents.debug]`. |
| `cache_bust_detect` | bool | `false` | Alert via Telegram when `cache_read` drops >50% vs previous request (indicates prefix changed). Per-agent override via `[agents.debug]`. |
| `cache_bust_idle_minutes` | int | `10` | Suppress cache bust alerts if the session was idle longer than this many minutes. Anthropic's cache TTL is 5 min, so any gap >10 min means the cache expired naturally — not a genuine bust. Per-agent override via `[agents.debug]`. |

### `[database]`

SQLite database settings.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `busy_timeout` | string | `"5s"` | SQLite busy timeout for concurrent access. Go duration format. High-load systems may need longer waits. |

### `[models.*]`

Named model definitions with per-model settings. Each section defines an alias and its parameters. Aliases can be used anywhere a model is referenced (groups, fallbacks, `/model` command).

```toml
[models.glmturbo]
model = "openrouter/z-ai/glm-5-turbo"
thinking = true
effort = "high"
enable_keepalive = true
context = "202k"

[models.kimi]
model = "openrouter/moonshotai/kimi-k2.5"
thinking = true
enable_keepalive = true
```

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `model` | string | *(required)* | Full `developer/model_id` string |
| `endpoint` | string | `""` | Explicit endpoint override (empty = auto-detect from developer) |
| `thinking` | string/bool | `""` | Thinking mode: `"adaptive"`, `"off"`, `true`, `false` |
| `effort` | string | `""` | Effort level: `"low"`, `"medium"`, `"high"` |
| `speed` | string | `""` | Speed mode: `"fast"` or empty |
| `context` | int/string | `0` | Context window size in tokens (e.g. `262000` or `"262k"`) |
| `enable_keepalive` | bool | `nil` | Enable keepalive pings (`nil` = auto-detect from developer) |
| `cache_ttl` | string | `""` | Cache TTL as Go duration. For Anthropic: `"5m"` or `"1h"` (sent as cache control TTL, also drives keepalive interval). For Gemini: any Go duration (context cache TTL, `"0"` disables). Empty = auto-detect from developer defaults. |
| `cache_strategy` | string | `""` | Cache marker strategy (Anthropic only): `"auto"` (top-level, API decides breakpoints) or `"explicit"` (manual breakpoints on system prompt and second-to-last message). Empty = `"auto"`. |

**Model defaults hierarchy:** Per-model settings (thinking, effort, speed) act as defaults for any session using that model. Session-level overrides via `/thinking`, `/effort`, `/speed` take precedence. Clearing a session override (e.g. `/effort none`) reverts to the model default.

### `[groups]`

Model group assignments, call site overrides, and fallbacks. Group names are arbitrary string keys; values are `developer/model_id` strings or `[models.*]` alias names.

The three built-in groups are `powerful` (required), `fast`, and `cheap`. `fast` and `cheap` default to `powerful` when unset. You can also define custom groups.

```toml
[groups]
powerful = "haiku"
fast = "haiku"
cheap = "haiku"
reasoning = "opus"      # user-defined group
```

**`[groups.calls]`** — Override which group a specific call site uses. Keys are call site names, values are group names (including user-defined ones).

Default call site → group assignments:

| Group | Call sites |
|-------|-----------|
| **powerful** | `chat`, `spawn-clone`, `background`, `compaction`, `memory-capture`, `memory-consolidate` |
| **fast** | `spawn-raw`, `spawn-character` |
| **cheap** | `spawn-explore`, `summarize-tool`, `summarize-file`, `prompt-diff` |

Ungrouped call sites (`keepalive`) always use the session model regardless of group configuration.

**`[groups.fallbacks]`** — Automatic model failover on transient errors. When a model returns 529 (overloaded), 5xx (server error), or times out (`context.DeadlineExceeded`), the agent automatically retries the request with the configured fallback model. Fallback is per-request — the primary model is always tried first on the next turn.

Keys and values are `developer/model_id` strings. Chains are supported (e.g., opus → sonnet → haiku) up to a maximum depth of 3. Cycles are detected and broken at startup.

Not triggered by: 401 auth errors, 400 bad requests, 429 rate limits.

```toml
[groups.fallbacks]
opus = "sonnet"                                          # name → name
sonnet = "haiku"                                         # chains: opus → sonnet → haiku
"google/gemini-2.5-pro" = "anthropic/claude-sonnet-4-6"  # cross-endpoint fallback
```

All `[groups]` keys can be overridden per-agent via `[agents.groups]`. Per-agent group values override global; `calls` and `fallbacks` maps are merged (per-agent keys win).

```toml
[[agents]]
id = "research"
[agents.groups]
powerful = "anthropic/claude-opus-4-6"  # this agent uses opus instead of global powerful
cheap = "google/gemini-2.5-flash"       # different cheap model for this agent
[agents.groups.fallbacks]
opus = "google/gemini-2.5-pro"          # override global fallback for this agent
```

### `[endpoints]`

Named API endpoints. Built-in defaults (anthropic, gemini, openai, openrouter) are populated automatically if not present. Users can override built-ins or add custom endpoints.

Three independent concepts drive model routing:

| Concept | Example | Determines |
|---------|---------|------------|
| **Endpoint** | `openrouter` | Base URL, API key |
| **Wire format** | `anthropic`, `openai`, `gemini` | Which client serializes the request |
| **Model ID** | `claude-opus-4-6` | String passed in the API call |

Format is auto-inferred from model name: `claude-*` → anthropic, `gemini-*` → gemini, `gpt-*`/`o3*`/`o4*` → openai. Unknown models fall back to openai.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `format` | string | `""` | Wire format for single-format endpoints: `"anthropic"`, `"openai"`, or `"gemini"`. |
| `url` | string | `""` | Base URL. Empty uses SDK default. |
| `anthropic_url` | string | `""` | Anthropic-format URL for multi-format endpoints. |
| `openai_url` | string | `""` | OpenAI-format URL for multi-format endpoints. |
| `gemini_url` | string | `""` | Gemini-format URL for multi-format endpoints. |
| `api_key` | string | `""` | Secret name in secrets store (e.g. `"openrouter.api_key"`). |
| `http_timeout` | string | varies | HTTP timeout. Go duration format. Default `"120s"` for most endpoints, `"600s"` for anthropic (extended thinking). |

Built-in endpoint defaults:
- `anthropic` — `format = "anthropic"`, `api_key = "anthropic.api_key"`, `http_timeout = "600s"`
- `gemini` — `format = "gemini"`, `api_key = "gemini.api_key"`, `http_timeout = "120s"`
- `openai` — `format = "openai"`, `api_key = "openai.api_key"`, `http_timeout = "120s"`
- `openrouter` — multi-format (`anthropic_url` + `openai_url` both set to `https://openrouter.ai/api/v1`), `api_key = "openrouter.api_key"`, `http_timeout = "120s"`

Example custom endpoint:
```toml
[endpoints.local]
format = "openai"
url = "http://localhost:8080/v1"
api_key = "local.api_key"
```

Then use it: `model = "local/my-fine-tuned-model"`.

Clients are lazy-initialized on first use — endpoints that are never referenced don't create connections.

### `[tools]`

Tool behavior settings (global-only fields). Fields that can be overridden per-agent are in [Global-or-Agent: Tool Behavior](#tool-behavior).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `temp_dir` | string | `"/tmp/foci/tool-results"` | Directory for large tool result files. |
| `tmux_cols` | int | `300` | Window width (columns) applied via `resize-window` after `tmux new-session`. |
| `tmux_rows` | int | `30` | Window height (rows) applied via `resize-window` after `tmux new-session`. |
| `exec_default_timeout` | int | `30` | Default timeout for exec commands in seconds. |
| `tmux_command_timeout` | string | `"5s"` | Timeout for tmux control commands. Go duration format. |
| `web_fetch_timeout` | string | `"30s"` | HTTP timeout for web fetch operations. Go duration format. |
| `web_fetch_max_bytes` | int | `1048576` | Max bytes to read from web fetch (1MB default). |
| `web_search_timeout` | string | `"15s"` | HTTP timeout for web search API calls. Go duration format. |
| `summary_context_turns` | int | `5` | Number of recent conversation turns included as context when auto-summarising oversized tool results. |
| `summary_context_chars` | int | `6000` | Max characters of conversation context sent to the cheap model for auto-summary. |
| `tmux_memory_check_interval` | string | `"5m"` | How often to check tmux server RSS. Go duration format. `"0"` disables monitoring. |
| `tmux_memory_warn` | string | `"10%"` | Warn threshold. Sends Telegram notification. Formats: `"N%"` (% of RAM), `"Nmb"`, `"Ngb"`. |
| `tmux_memory_critical` | string | `"20%"` | Critical threshold. Sends Telegram notification with stronger message. Same formats. |
| `tmux_memory_kill` | string | `"30%"` | Kill threshold. Kills tmux server, notifies, cleans up tool state. Same formats. |
| `tmux_autopilot` | bool | `true` | Auto-unwatch sessions after inactivity notification, auto-watch on send. |
| `tmux_watch_threshold` | string | `"30s"` | Default inactivity watch threshold. Go duration format. |
| `tmux_session_ttl` | string | `"24h"` | Auto-kill idle tmux sessions after this duration of no agent interaction. Go duration format. `"0"` disables. |
| `web_search_max_uses` | int | `0` | Max Anthropic web searches per API call. `0` = unlimited. Only applies when `search_provider = "anthropic"`. |
| `web_search_allowed_domains` | string[] | `[]` | Domain whitelist for Anthropic web search. Mutually exclusive with `web_search_blocked_domains`. |
| `web_search_blocked_domains` | string[] | `[]` | Domain blacklist for Anthropic web search. Mutually exclusive with `web_search_allowed_domains`. |
| `web_fetch_max_uses` | int | `0` | Max Anthropic web fetches per API call. `0` = unlimited. Only applies when `fetch_provider = "anthropic"`. |
| `web_fetch_allowed_domains` | string[] | `[]` | Domain whitelist for Anthropic web fetch. Mutually exclusive with `web_fetch_blocked_domains`. |
| `web_fetch_blocked_domains` | string[] | `[]` | Domain blacklist for Anthropic web fetch. Mutually exclusive with `web_fetch_allowed_domains`. |

The `summary` tool uses the **cheap** model group (call site: `summarize-file`). Configure via `[groups]` cheap or `[groups.calls]` overrides.

Tmux memory monitoring detects runaway memory from long-running tmux sessions (glibc malloc fragmentation). Notifications are sent to agents whose `inject_agent_warnings` is `"off"` — agents with injection enabled already see log warnings in their session.

### `[browser]`

Browser automation tool configuration. Enabled by default. Agents get a `browser` tool that uses accessibility tree snapshots with element refs for interaction.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `true` | Enable browser tool for all agents. |
| `headless` | bool | `true` | Run browser in headless mode. Set `false` for debugging. |
| `timeout_sec` | int | `30` | Default timeout for page operations in seconds. |
| `user_data_dir` | string | `""` | Chrome user data directory. Empty uses a temp profile. Only used when incognito mode is off (controlled at runtime via the browser tool's `incognito` parameter). |
| `executable_path` | string | `""` | Path to Chrome/Chromium binary. Empty uses auto-detection via go-rod launcher. |
| `dom_stable_sec` | float | `1.0` | DOM stability check interval in seconds before capturing auto-snapshots. |
| `dom_stable_diff` | float | `0.2` | DOM change threshold (0.0–1.0) for stability detection. Lower = stricter. |

Per-agent override: `[agents.browser] enabled` overrides `[browser] enabled`.

Example:
```toml
[browser]
enabled = true
headless = true
timeout_sec = 30
```

### `[permissions]`

Controls foci-level auto-approval of delegated backend permission requests. When a delegated coding agent (e.g. Claude Code) requests permission for a tool invocation, foci checks these rules before prompting the user. Matched requests are auto-approved silently with an INFO log entry.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `auto_approve` | string[] | `[]` | Patterns to auto-approve. Format: `"ToolName"` (any input) or `"ToolName:pattern"` (match input). See below for pattern matching and Bash safety details. |
| `auto_approve_common_readonly` | bool | `true` | Enable built-in allowlist of read-only tools (Search, Glob, Grep, Read, WebSearch, WebFetch) and safe shell commands (ls, cat, grep, jq, etc.). |
| `auto_approve_common_safe_write` | bool | `false` | Enable built-in allowlist of side-effecting commands that are typically low-risk for a coding agent: `curl`, `wget`, `mkdir`, `touch`. **Opt-in** — see the warning below. |

Rules from global `[permissions]` and per-agent `[[agents]].permissions` are combined (union) — both sets apply. Both bools follow standard cascade (per-agent overrides global).

> **⚠ Safe-write rules are not path-scoped.** Unlike `Edit`/`Write` rules, which can be pinned to a workspace (`Edit:/path/to/workspace/*`), Bash-command rules are prefix-matched on the command string. Enabling `auto_approve_common_safe_write` means `mkdir ./build` and `mkdir /etc/foo` are both auto-approved — the allowlist trusts the agent not to target paths outside its workspace. Leave this off unless you've reasoned about that trust boundary for your deployment.

Delegated backends also auto-approve workspace Edit/Write access (scoped to the agent's workspace directory).

#### Pattern matching

Each rule is `"ToolName"` (match any invocation) or `"ToolName:pattern"` (match the tool's key input field):

| Tool | Matched field |
|------|--------------|
| Bash | `command` |
| Read, Edit, Write, NotebookEdit | `file_path` |
| Glob, Grep | `pattern` |
| WebFetch | `url` |
| WebSearch | `query` |

Patterns with `*` or `?` use glob matching (`*` = any characters, `?` = single character). Without glob characters, prefix matching with word boundary is used — `ls` matches `ls -la` but not `lsblk`.

#### Bash command chaining safety

Bash commands containing shell operators (`&&`, `||`, `;`, `|`) are split into segments and **every segment must independently match a rule**. This means rules are composable — if `cd /tmp`, `ls`, and `git *` are all approved, then `cd /tmp && ls && git status` is also approved, but `cd /tmp && rm -rf /` is rejected because `rm -rf /` doesn't match any rule.

Commands containing subshell injection (`$(...)` or backticks) are always rejected and fall through to the user prompt.

```toml
[permissions]
auto_approve = [
  "Bash:cd /home/rich/git/foci",     # composable with other rules via &&
  "Bash:git -C /home/rich/git/foci *",
  "Bash:gcalcli *",
]
auto_approve_common_readonly  = true    # default
auto_approve_common_safe_write = false  # default — opt-in, not path-scoped
```

### `[skills]`

Override for the shared skills directory. By default, skills are loaded from two directories in order:

1. **Shared:** `$home/shared/skills/` (where `$home` is the parent of the agent's workspace)
2. **Per-agent:** `$workspace/skills/`

When both directories contain a skill with the same name, the per-agent version wins.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `dir` | string | `""` | Shared skills directory. Empty uses the default `$home/shared/skills/`. |

Per-agent override: `skills_dir` in `[[agents]]` overrides the per-agent directory (default: `$workspace/skills/`).

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

**Inline keyboards:** Built-in commands with parameters (`/model`, `/thinking`, `/effort`, `/speed`, `/display`, `/config`, `/sessions`, `/tmux`) show inline keyboard buttons when invoked bare. No configuration needed. `/effort`, `/thinking`, and `/speed` are hidden from `/help` and keyboards when the current model doesn't support them.

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

**Resolution order:** agent value > `[defaults.*]` value > global section value > hardcoded default.

Set global defaults in `[defaults.*]` sub-sections:
```toml
[defaults.loop]
max_tool_loops = 50

[defaults.system]
system_files = ["IDENTITY.md", "SOUL.md", "COHERENCE.md"]
```

Effort, thinking, and speed defaults are set in `[defaults.loop]` or per-agent in `[agents.loop]`. At runtime, unsupported params are skipped with a warning; if a model returns a 400 error about thinking/effort/speed, the params are stripped and the request is retried once. Override at runtime via `/effort`, `/thinking`, `/speed`.

Override per-agent using the same sub-section names:
```toml
[[agents]]
id = "research"

[agents.loop]
max_tool_loops = 25
```

### Model & Response

Models are configured via `[groups]` (group assignments with `developer/model_id` strings). See [`[groups]`](#groups). Thinking, effort, and speed are set in `[defaults.loop]` or per-agent in `[agents.loop]`. Loop and system fields are set in their respective sub-sections.

| Key | Type | Default | Section | Description |
|-----|------|---------|---------|-------------|
| `max_output_tokens` | int | `16384` | `[defaults.loop]` | Maximum tokens in model response. Larger values allow longer responses. |
| `max_tool_loops` | int | `25` | `[defaults.loop]` | Maximum tool iterations per agent turn. Complex tasks may need more. |
| `streaming` | bool | `false` | `[defaults.display]` | Use streaming API. Text and thinking deltas are delivered incrementally. Works with any provider that implements streaming (Anthropic, OpenAI). |
| `cache_ttl` | string | `""` | `[defaults.loop]` | Anthropic prompt cache TTL override. Must be `"5m"` or `"1h"`. Empty inherits from `[cache] ttl` (default `"1h"`). Only applied to Anthropic API requests. |
| `system_files` | string[] | see below | `[defaults.system]` | Ordered list of workspace files to load as system prompt blocks. |

Default `system_files` order (most-stable first for cache efficiency):
```
["IDENTITY.md", "SOUL.md", "COHERENCE.md", "AGENTS.md", "TOOLS.md", "USER.md", "MEMORY.md", "KEEPALIVE.md"]
```

Missing files are silently skipped. The last file gets the cache breakpoint marker.

### Braindead Warning

Implemented as a built-in nudge rule with an `every_n_tools` trigger. When tool calls reach the threshold, a warning is injected via the nudge system. Subject to the same cooldown and rate-limiting as other nudge rules. Set in `[defaults.nudge]`, overridable per-agent via `[agents.nudge]`.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `nudge_default_braindead_threshold` | int | `10` | Tool calls before injecting a braindead warning. `0` disables. |
| `nudge_default_braindead_prompt` | string | `""` | Custom warning text injected when the threshold is hit. `""` uses a hardcoded default. |
| `turn_lock_warn_threshold` | string | `"3m"` | Warn if turn lock wait exceeds this duration. Go duration format. `proactive_warning` triggers are excluded. |

### Nudge System

Mid-turn behavioral reminders extracted from character files. Rules are extracted by an LLM from the agent's character files (system prompt) and stored in `{workspace}/nudge-rules.json` (or `{workspace}/character/nudge-rules.json` if the `character/` directory exists). Rules are re-extracted when character files change (detected via content hash on `/reload` or compaction).

Available in both `[defaults.nudge]` and `[agents.nudge]`.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `nudge_enable` | bool | `true` | Enable the nudge system. When enabled, loads rules from disk and injects reminders during the agent loop. |
| `nudge_auto_extract` | bool | `true` | Auto-extract rules from character files via LLM when they change. When false, nudges still fire from an existing `nudge-rules.json` but the LLM is never called to create or update it. |
| `nudge_cooldown` | int | `5` | Minimum tool calls between repeating the same reminder. Prevents spam. |
| `nudge_max_per_batch` | int | `1` | Maximum reminders injected per tool batch. |
| `nudge_pre_answer_gate` | bool | `false` | Enable pre-answer verification gate. When the model wants to end a turn after 2+ tool calls, inject pre_answer reminders and let it reconsider once. |
| `nudge_pre_answer_min_tools` | int | `2` | Minimum tool call iterations before the pre-answer gate fires. |
| `nudge_default_enable` | bool | `true` | Enable built-in tool/skill reminders. When enabled, periodically reminds the agent which tools and skills are available. |
| `nudge_default_frequency` | int | `50` | User turns between tool/skill reminders. The turn counter is a lifetime counter (never reset). |
| `nudge_default_scratchpad_frequency` | int | `20` | User turns between scratchpad review reminders. Only fires when scratchpad entries exist, prompting the agent to update or clear stale entries. `0` disables. |

**Trigger types** (configured per-rule in `nudge-rules.json`):
- `every_n_tools(N)` — remind every N individual tool calls during a turn (default N=5)
- `every_n_turns(N)` — remind every N user turns; lifetime counter, never reset (used by default nudges)
- `pre_answer` — remind just before the model returns a final answer
- `after_error` — remind when a tool call returns an error
- `regex(pattern)` — remind when the user's message matches a Go regex pattern

### Display

Set in `[platforms.display]`, overridable per-agent via `[agents.platforms.display]`. At runtime, the `/display` command sets per-session overrides without modifying the config file:

```
/display                          # show current effective values
/display show_tool_calls preview  # set per-session override
/display stream_output on         # set per-session override
/display display_width 80         # set per-session override
/display reset                    # clear all overrides back to config defaults
```

Supported keys: `show_tool_calls`, `show_thinking`, `stream_output`, `display_width`. Aliases: `stream` → `stream_output`, `width` → `display_width`.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `show_tool_calls` | string | `"off"` | Tool call display mode: `"off"` (hidden), `"preview"` (shown then overwritten by reply), `"full"` (shown and kept; reply is a separate message). Accepts bool for backwards compat (`true` → `"preview"`, `false` → `"off"`). Overridable at runtime via `/display`. |
| `show_thinking` | string | `"off"` | Thinking block display mode: `"off"` (stripped), `"compact"` (toggle button), `"true"` (always shown). Accepts bool (`true` → `"true"`, `false` → `"off"`). Overridable at runtime via `/display`. |
| `injected_message_header` | string | `"[[ System message ]]"` | Header prepended to injected/system messages (keepalive, async notifier, HTTP API, proactive warnings) so users can distinguish them from agent replies. Empty string disables the header. |

### Message Handling

Set in `[defaults.loop]`, overridable per-agent via `[agents.loop]`.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `duplicate_messages` | bool | `false` | Send user text twice per API call. Can improve instruction following. Automatically suppressed when extended thinking is enabled with effort above "low", since thinking already produces high-quality responses. |
| `batch_partial_assistant_messages` | bool | `false` | When `false`, text in mid-turn responses (alongside tool calls) is sent to Telegram immediately. When `true`, text is accumulated and returned concatenated when the turn completes. |
| `batch_partial_joiner` | string | `""` | Separator inserted between batched partial messages when `batch_partial_assistant_messages` is `true`. |

### Compaction & Sessions

Global defaults set in `[sessions]`, overridable per-agent. Per-agent `unset` inherits from `[sessions]`.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `compaction_threshold` | float | `0.8` | Trigger compaction when context usage exceeds this fraction (0.0–1.0). |
| `compaction_summary_prompt` | string | `""` | Path to prompt file for compaction summary. Read live at compaction time (edits take effect immediately). `""` uses embedded default. |
| `compaction_handoff_msg` | string | see below | Message injected after the summary to orient the agent post-compaction. |
| `compaction_preserve_messages` | int | `25` | Preserve the last N messages through compaction. Preserved messages are appended verbatim after the summary + handoff, keeping their original roles. `0` disables (summary only). The summarizer only sees messages *before* the preserved window. |
| `compaction_effort` | string | `""` | Effort level for compaction API calls: `"low"`, `"medium"`, `"high"`. `""` uses session effort. Useful when agent uses low effort for chat but needs higher quality for compaction. |
| `autocompact_before_mana_refresh` | bool | `true` | Master switch for mana-refresh compaction. `false` disables entirely (replaces the old `"0"` disable convention). |
| `autocompact_before_mana_refresh_threshold` | string | `"5m"` | Trigger mana-refresh compaction when mana reset is within this duration. Format: Go duration string. |
| `autocompact_before_mana_refresh_factor` | float | `0.5` | Secondary compaction threshold for mana-refresh mode, as a fraction of the main `compaction_threshold`. E.g. with threshold 0.8 and factor 0.5, mana-refresh triggers at 40% context usage. Range: 0.0–1.0. |
| `autocompact_before_mana_refresh_preserve` | int | unset | Explicit message count to preserve during mana-refresh compaction. Overrides the percentage-based default. `0` uses normal preservation count. |
| `autocompact_before_mana_refresh_preserve_pct` | float | `0.5` | Fraction of messages to preserve during mana-refresh compaction (0.0–1.0). Default 0.5 preserves 50% of messages, summarising the older half. Only used when `autocompact_before_mana_refresh_preserve` is unset. |
| `branch_orientation_facet_prompt` | string | `""` | Path to prompt file for user-attached facet branches. Supports template variables `{branch_key}`, `{parent_key}`, `{branch_type}`, `{direct_chat}`. `""` uses embedded default from `shared/prompts/branch-orientation-facet.md`. |
| `branch_orientation_headless_prompt` | string | `""` | Path to prompt file for headless branches (cron, spawn, keepalive). Same template variables. `""` uses embedded default from `shared/prompts/branch-orientation-headless.md`. |

#### Mana-Refresh Compaction

Compaction triggers in exactly two automatic modes:

1. **Main threshold** — compact when context exceeds `compaction_threshold` (default 80%).
2. **Mana-refresh** — when `autocompact_before_mana_refresh` is enabled (default true), compact when the mana reset is within `autocompact_before_mana_refresh_threshold` (default 5m) AND context exceeds a secondary threshold (`compaction_threshold × autocompact_before_mana_refresh_factor`, default 40%). This re-summarises before the new mana window starts. Preserves `autocompact_before_mana_refresh_preserve_pct` of messages (default 50%), summarising the older half. An explicit `autocompact_before_mana_refresh_preserve` count overrides the percentage.

A third mode is manual: the user can run `/compact` at any time.

Only Anthropic-endpoint sessions have mana tracking. Sessions switched to Gemini/OpenAI skip the mana-refresh check (no spurious compactions from the wrong budget).

```toml
# Example: tune mana-refresh for a specific agent
[[agents]]
id = "research"

[agents.sessions]
autocompact_before_mana_refresh = true           # master switch (default true)
autocompact_before_mana_refresh_threshold = "10m" # wider window
autocompact_before_mana_refresh_factor = 0.3      # trigger at 24% context
autocompact_before_mana_refresh_preserve = 50     # preserve last 50 messages
```

### Tool Behavior

Global defaults set in `[tools]`, overridable per-agent via `[agents.tools]`. Per-agent `0` inherits from `[tools]`.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `max_result_chars` | int | `15000` | Max characters in a tool result before writing to a temp file and returning a guard message (no partial content). Global: `[tools]`. |
| `max_summary_chars` | int | `300000` | Max chars to auto-summarise via the cheap model. Results larger than this are saved to file with hints but skip the summary call. Global: `[tools]`. |
| `auto_summarise` | bool | `true` | Auto-summarise oversized tool results via the cheap model. `false` skips summary calls entirely (results are saved to file with hints instead). Global: `[tools]`. Per-agent `unset` inherits from `[tools]`. |
| `max_summary_input_chars` | int | `100000` | Max chars of tool result text embedded in the summary prompt. Larger results are truncated in the prompt (the full output is on disk). Prevents excessive memory use and token cost during auto-summarisation. Global: `[tools]`. |
| `max_image_pixels` | int | `2073600` | Max pixels (width × height) for images before downscaling. Images exceeding this are proportionally resized and re-encoded as JPEG (quality 85). Default is 1920×1080. `0` disables downscaling. Global: `[tools]`. |
| `exec_auto_background` | int | `10` | Seconds before auto-backgrounding long-running exec and http_request calls. `0` disables. Global: `[tools]`. |
| `max_concurrent_spawns` | int | `3` | Max concurrent `spawn` clone sessions per agent. Global: `[tools]`. |
| `explore_max_depth` | int | `100` | Max tool loops for `spawn` explore mode. Explore agents do multi-step research so this is higher than the default `max_tool_loops`. Global: `[tools]`. |
| `tool_call_preview_chars` | int | `450` | Max characters for tool call parameter preview in Telegram `show_tool_calls = "preview"` mode. Global: `[tools]`. |
| `max_upload_file_size` | int | `52428800` | Max file size in bytes for multipart/form-data file uploads (default 50MB). Global: `[tools]`. |
| `search_provider` | string | `"brave"` | Web search provider: `"brave"` (client-side, needs `brave_api_key`) or `"anthropic"` (server-side). Brave is recommended: Anthropic's server-side search returns encrypted content blobs that massively inflate token counts (observed: 256k tokens from just two searches) and bypass the tool result size guard entirely. Brave results are client-side, guardable, and far more token-efficient. Global: `[tools]`. |
| `fetch_provider` | string | `"builtin"` | Web fetch provider. See [TOOLS.md](TOOLS.md) for provider details. Global: `[tools]`. |
| `todo_format` | string | `"lines"` | Todo list rendering format: `"lines"` (one item per line) or `"table"` (tabular layout). Global: `[tools]`. |

### Notifications & Logging

Notification fields (`startup_notify`, `inject_agent_warnings`, etc.) are part of `NotifyConfig` and follow the 5-level cascade: per-agent platform → per-agent → global platform (`[[platforms]]`) → `[defaults.notify]` → code default. See the `[[platforms]]` section for the full list.

| Key | Type | Default | Global location | Description |
|-----|------|---------|-----------------|-------------|
| `messages_in_log` | bool | `false` | `[debug]` | Log user message content to the event log. When `false`, messages are logged at DEBUG level with no content for privacy. When `true`, messages are logged at INFO level with content (truncated to 100 chars). Per-agent override via `[agents.debug]`. |
| `steer_mode` | bool | `true` | `[defaults.behavior]` | When enabled and the agent is mid-turn (executing tool calls), user messages are injected between tool calls at the next tool boundary as `[user]` content blocks instead of queuing behind the turn lock. This lets users redirect a runaway agent without `/stop`. System messages (keepalive, warnings) are unaffected. |
| `group_throttle` | string | `""` | `[defaults.behavior]` | Group chat throttle window. Non-mention messages accumulate silently and are delivered as a batch when the timer fires. @mentions flush all buffered messages immediately and reset the timer. Go duration format (e.g. `"30s"`, `"1m"`). Empty or `"0"` disables (default). Works with both `require_mention = true` (non-mentions buffered instead of dropped) and `false` (non-mentions buffered instead of processed immediately). |
| `stream_output` | bool | `false` | `[[platforms]]` | Stream model output in real-time. Requires `streaming = true` for API-level delta callbacks. |
| `stream_interval` | string | `"250ms"`/`"1200ms"` | `[[platforms]]` | Duration between message edits during streaming. Default varies by platform. |
| `compaction_debug` | bool | `false` | `[defaults.notify]` | Send the compaction summary to Telegram as a markdown file attachment after compaction completes. Useful for verifying what survived the cut. Part of `NotifyConfig` — follows the 5-level cascade. |
| `warning_max_per_window` | int | `3` | `[defaults.notify]` | Max identical warnings per time window before suppression. `0` disables rate-limiting. Part of `NotifyConfig` — follows the 5-level cascade. |
| `cache_bust_detect` | bool | `false` | `[debug]` | Alert when `cache_read` drops >50% vs previous request. Part of `DebugConfig` — per-agent override via `[agents.debug]`. |
| `cache_bust_idle_minutes` | int | `10` | `[debug]` | Suppress cache bust alerts if session idle longer than N minutes. Part of `DebugConfig` — per-agent override via `[agents.debug]`. |
| `facet_no_compact` | bool | `true` | `[sessions]` | Set `no_compact` on facet sessions. Facet sessions are short-lived parallel forks that shouldn't trigger compaction. Set to `false` if you want facet sessions to compact normally. |

### Per-agent platform overrides

All platform fields from `[[platforms]]` can be overridden per-agent via `[[agents.platforms]]`, using the same subsection structure (`[agents.platforms.display]`, `[agents.platforms.access]`, `[agents.platforms.notify]`, `[agents.platforms.debug]`). The 5-level cascade handles resolution. See the `[[platforms]]` section for the full list of available fields.

### Voice

| Key | Type | Default | Global location | Description |
|-----|------|---------|-----------------|-------------|
| `tts_rate` | float | `0` | `[defaults.voice]` | Per-agent TTS speech rate multiplier. Combined with entry rate: effective = entry.rate × agent.tts_rate (0 treated as 1.0). |
| `tts` | string | `""` | `[defaults.voice]` | Override TTS entry by id (empty = default entry). |
| `stt` | string | `""` | `[defaults.voice]` | Override STT entry by id (empty = default entry). |
| `tts_replacements` | map | `{}` | `[defaults.voice]` | TTS word replacements (merged with `[[tts]]` entry replacements; per-agent wins). Case-insensitive whole-word matching. |
| `stt_replacements` | map | `{}` | `[defaults.voice]` | STT word replacements (merged with `[[stt]]` entry replacements; per-agent wins). Case-insensitive whole-word matching. |

### Keepalive (`[keepalive]` / `[[agents.keepalive]]`)

Cache keepalive timer. Fires a lightweight branch session to keep the prompt cache warm. For Anthropic, the interval defaults to 55 minutes (just under the 1-hour cache TTL). For OpenAI and DeepSeek models, keepalive is auto-detected by developer name — these developers have a 5-minute prompt cache TTL, so keepalive fires every ~4m45s.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Enable keepalive timer. |
| `interval` | string | `"55m"` | Time since cache last warmed before firing. Should be less than `[cache] ttl` (default 1h). |
| `prompt` | string | `""` | Prompt file path. `""` = embedded default, `"default"` = embedded, `"none"` = disabled, `/path` = custom file. |

### Background (`[background]` / `[[agents.background]]`)

Mana-gated background work timer. Fires when the user is idle, there are open background-tagged todos, and the manamometer says spending is wise.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Enable background work timer. |
| `interval` | string | `"15m"` | Time since last interaction before firing. |
| `prompt` | string | `""` | Prompt file path. `""` = embedded default, `"default"` = embedded, `"none"` = disabled, `/path` = custom file. |

**Validation warnings:**
- `background.interval > keepalive.interval` — keepalive resets the cache timer; background work may never trigger.
- `keepalive.interval > [cache] ttl` — cache may expire between keepalives (default TTL is 1 hour).

### Mana (`[mana]` / `[[agents.mana]]`)

Controls mana budget behavior and usage warning thresholds. All fields overridable per-agent.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `name` | string | `"mana"` | What to call the quota in user-facing messages (e.g. `"mana"`, `"juice"`). |
| `thresholds` | int[] | `[]` | Mana percentages to warn at (e.g. `[50, 25, 10, 5]`). Per-agent values completely replace global. |
| `restore_threshold` | int | `0` | Inject session notice when mana restores to 100% after being below this threshold. `0` disables. Range: 0–100. |
| `invest_interval` | string | `"30m"` | Quiet period after mana reset before spending. The manamometer prevents background work from running during this period to allow cache building. |

See [HEARTBEAT.md](HEARTBEAT.md) for full details on the manamometer and timer logic.

### Reflection (`[reflection]` / `[[agents.reflection]]`)

The periodic reflection pass captures both factual memory (into daily memory files) and procedural knowledge (autogenerated skills under `workspace/skills/`) from recent session activity. MEMORY.md consolidation also runs here. All sub-features default to enabled.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `interval_enabled` | bool | `true` | Enable periodic reflection pass on timer. |
| `interval` | string | `"1h"` | Time between reflection passes. |
| `interval_prompt` | string | `""` | Prompt override. `""` = embedded `reflection.md`, `"none"` = disabled, `/path` = custom file. |
| `consolidation_enabled` | bool | `true` | Enable periodic MEMORY.md curation. |
| `consolidation_interval` | string | `"20h"` | Minimum time between consolidation runs. Persisted across restarts. |
| `consolidation_prompt` | string | `""` | Prompt override. `""` = embedded `memory-consolidation.md`, `"none"` = disabled, `/path` = custom file. |
| `session_end_enabled` | bool | `true` | Run reflection on `/reset` and facet reclaim. |
| `session_end_prompt` | string | `""` | Prompt override. `""` = embedded `reflection.md`, `"none"` = disabled, `/path` = custom file. |
| `compaction_enabled` | bool | `true` | Run reflection before compaction summarises context. |
| `compaction_prompt` | string | `""` | Prompt override. `""` = embedded `reflection.md`, `"none"` = disabled, `/path` = custom file. |
| `backend_quiet_period` | string | `"5m"` | Minimum idle time before reflection fires in backend/delegated mode. Prevents reflection from triggering while the backend agent is still actively working. |

All prompt fields use 3-state resolution: `""` or `"default"` → embedded default from `shared/prompts/`, `"none"` → disabled, file path → read file with embedded fallback on error.

**Interval reflection** runs in the keepalive timer loop. Fires when:
1. `interval` has elapsed since the last reflection
2. There's been user activity since the last reflection
3. The user has been active within the interval window

**Consolidation** reviews daily memory files and curates MEMORY.md. The last-run timestamp is persisted in state, so it survives restarts. Only fires when there's been user activity within the last hour.

**Session-end** fires asynchronously on `/reset` and facet reclaim. Creates a branch from the expiring session (preserving conversation history) so the caller doesn't block.

**Compaction** fires immediately before compaction summarises and replaces context. Creates a branch from the pre-compaction session so the reflection agent sees the full conversation history that's about to be summarised. The branch is created synchronously (capturing the branch point) before compaction starts; the reflection agent runs asynchronously in a goroutine.

### Per-Agent Mana (`[agents.mana]`)

Per-agent mana overrides. When set, completely replaces the global `[mana]` values for this agent.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `name` | string | `""` | Override quota name. `""` inherits from global `[mana]`. |
| `thresholds` | int[] | `[]` | Mana warning thresholds. `[]` inherits from global `[mana]`. When set, completely replaces global thresholds for this agent. |
| `restore_threshold` | int | `0` | Inject session notice when mana restores to 100% after being below this threshold. `0` disables. |
| `invest_interval` | string | `""` | Override invest interval. `""` inherits from global `[mana]`. |

### Skills & Message Transforms

| Key | Type | Default | Global location | Description |
|-----|------|---------|-----------------|-------------|
| `skills_dir` | string | `""` | `[skills] dir` | Per-agent skills directory. Empty uses default `$workspace/skills/`. |
| `message_transforms` | array | `[]` | `[[message_transforms]]` | Regex find/replace rules applied to inbound messages. `[]` inherits from global `[[message_transforms]]`. |
| `blocked_paths` | array | `[]` | `[[blocked_paths]]` | Path prefixes blocked for write/edit tools. `[]` inherits from global `[[blocked_paths]]`. Per-agent replaces global (not merged). |

### Webhooks

| Key | Type | Default | Global location | Description |
|-----|------|---------|-----------------|-------------|
| `webhooks` | map[string]string | `{}` | `[defaults.system]` | Maps webhook hook IDs to prompt file paths. Used by `POST /webhook/{agent}/{hookid}`. Per-agent merges with global (agent keys override matching global keys; unmatched global keys are preserved). |

Prompt paths are resolved via `prompts.ResolvePrompt`: bare filenames (e.g. `"deploy.md"`) are searched in `{workspace}/shared/prompts/` then `{shared}/prompts/`; absolute paths are read directly.

```toml
[defaults.system]
webhooks = { new_commit = "new_commit.md", deploy = "deploy.md" }

[[agents]]
id = "scout"
[agents.system]
webhooks = { alert = "alert-handler.md" }  # adds alert; inherits new_commit, deploy from defaults
```

---

## 3. Agent-Only Configuration

Fields that only exist per-agent in `[[agents]]`. These have no global equivalent.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `id` | string | `""` | Agent identifier. Used in session keys (`ID/c{chatID}/{versionTS}`). |
| `name` | string | capitalised `id` | Human-readable name (e.g. `"Clutch"`). Defaults to capitalised agent ID (e.g. `clutch` → `Clutch`). Used in `/voice` WebSocket agent list. |
| `emoji` | string | `""` | Emoji for agent (e.g. `"🥔"`). Used in `/voice` WebSocket agent list. |
| `workspace` | string | `$HOME/$id` | Path to workspace directory containing character files (IDENTITY.md, SOUL.md, etc.). Defaults to `$HOME/<agent-id>` if not set. |
| `backend` | string | `""` | Backend selection. Empty or `"api"` = traditional agent loop (Foci calls API, executes tools). A coding agent name (`"claude-code-tmux"`, `"codex"`, `"opencode"`) delegates entire turns to an external agent subprocess. |
| `backend_config` | table | `{}` | Backend-specific settings. Interpreted by the backend implementation. See [Coding Agent Backends](#coding-agent-backends). |

### Per-agent platform configuration (`[[agents.platforms]]`)

Per-agent platform settings override the global `[[platforms]]` entries. All shared and platform-specific fields from `[[platforms]]` are available here.

```toml
[[agents]]
id = "myagent"

[[agents.platforms]]
id = "telegram"
bot = "myagent"
facet_bots = ["spare1"]
[agents.platforms.display]
show_tool_calls = "preview"
[agents.platforms.notify]
startup_notify = true
[agents.platforms.telegram]
table_wrap_lines = 5

[[agents.platforms]]
id = "discord"
bot = "myagent"
[agents.platforms.notify]
startup_notify = false
[agents.platforms.discord]
auto_thread = false
```

Agent-specific fields:

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `bot` | string | `$id` | Bot name for this agent. Token resolved from secret `"<platform>.<bot>"`. |
| `bot_secret` | string | `""` | Override secret key for bot token. `""` uses `"<platform>.<bot>"`. |

All other fields (display, access, notification, platform-specific) inherit from the global `[[platforms]]` entry with the same ID, then from `[defaults.*]`, then from code defaults.

### Memory (`[[agents]].memory`)

All `[memory]` settings can be overridden per-agent. Sources are combined additively (global + agent-specific); all other fields use the standard Merge cascade (per-agent → global).

When any agent has per-agent memory sources or overrides index-creation settings (`search_backend`, `reindex_debounce`, `conversation_weight`, `sweep_interval`), each agent gets its own index. Agent-specific sources automatically receive a weight boost of +1.0 and are prefixed with `agent/` in results.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `memory.sources` | array | see below | Per-agent memory directories. Combined with global `[memory]` sources. When empty, defaults to a single source: `{name: $id, dir: $workspace/memory, weight: 1.0}`. |
| `memory.search_backend` | string | (inherit) | Override search backend for this agent (`"fts5"` or `"bleve"`). |
| `memory.reindex_debounce` | string | (inherit) | Override reindex debounce delay for this agent. |
| `memory.conversation_weight` | float | (inherit) | Override conversation search weight for this agent. |
| `memory.search_limit` | int | (inherit) | Override max search results for this agent. |
| `memory.sweep_interval` | string | (inherit) | Override periodic reindex interval for this agent. |

Each source entry:

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `name` | string | required | Source identifier (prefixed with `agent/` in results). |
| `dir` | string | required | Directory path to index. |
| `weight` | float | `1.0` | Base weight (boosted by +1.0 automatically). |

When no agent has per-agent memory sources or setting overrides, a single shared index (`memory.db`) is used — fully backward compatible.

### Multi-Agent Example

```toml
# Global memory (shared by all agents)
[[memory.sources]]
name = "shared"
dir = "/home/foci/shared/memory"
weight = 1.0

# Shared facet pool (fallback for any agent)
[[platforms]]
id = "telegram"
facet_bots = ["spare1"]

[[agents]]
id = "main"
model = "anthropic/claude-sonnet-4-6"
workspace = "/home/foci/character"

[[agents.platforms]]
id = "telegram"
bot = "primary"
facet_bots = ["mainling"]  # per-agent facet pool

[[agents.memory.sources]]
name = "workspace"
dir = "/home/foci/clutch/memory"
weight = 1.0    # effective weight: 2.0 (1.0 + 1.0 boost)

[[agents]]
id = "research"
model = "google/gemini-2.5-flash"
workspace = "/home/foci/character"

[[agents.platforms]]
id = "telegram"
bot = "secondary"
# no facet_bots — uses shared pool only

[agents.memory]
conversation_weight = 0.3  # research agent weighs conversations higher

[[agents.memory.sources]]
name = "workspace"
dir = "/home/foci/scout/memory"
weight = 1.0
```

**Facet acquisition priority:** When `/facet` is invoked, per-agent pool is tried first. If all per-agent bots are busy (or none configured), the shared pool is used as fallback. Released bots return to whichever pool they came from.

---

## Coding Agent Backends

Instead of calling an LLM API directly (the default `"api"` backend), an agent can delegate entire turns to a coding agent subprocess. The coding agent handles inference, tool execution, and context management; Foci handles platform delivery, prompt enrichment (metadata, reminders, nudges), and command dispatch.

For a conceptual overview of the two paths and how to choose between them, see [BACKENDS.md](BACKENDS.md).

Set `backend` on an `[[agents]]` entry to enable:

```toml
[[agents]]
id = "coder"
backend = "claude-code-tmux"
workspace = "/home/coder/projects/myapp"

[agents.backend_config]
# model = "sonnet"            # CC model name (optional — omit for CC default)
# skip_permissions = true     # --dangerously-skip-permissions (no approval prompts)
# allowed_tools = ["Bash(git:*)", "Read"]  # --allowedTools: per-agent CC permission rules
# socket_path = ""            # tmux socket override (empty = default, cctmux only)
```

Per-agent `allowed_tools` accepts either a comma-separated string (`"Bash(git:*), Read"`) or a TOML array (`["Bash(git:*)", "Read"]`). It is merged with the global `[cc_backend] default_allowed_tools` list (see below) before launch — you don't need to repeat the defaults here.

### Global CC backend defaults — `[cc_backend]`

Applies to every agent whose `backend` is `claude-code` or `claude-code-tmux`:

```toml
[cc_backend]
# default_allowed_tools — permission rules that every CC agent receives via
# --allowedTools, merged with per-agent backend_config.allowed_tools. Uses the
# same rule syntax as settings.json permissions.allow.
#
# Factory default (applied when the key is not set in TOML):
#   ["Read(/tmp/**)", "Write(/tmp/**)", "Edit(/tmp/**)", "MultiEdit(/tmp/**)"]
#
# Set to an empty list to disable, or override with your own rules:
# default_allowed_tools = ["Write(/tmp/**)", "Bash(git:*)"]
```

The factory default grants CC agents free read/write access to `/tmp` so they can use the system scratch directory without a permission round-trip. Override if your deployment uses a different scratch path or wants a tighter default.

### Available backends

| Backend | Description |
|---|---|
| `"api"` (default) | Traditional agent loop — Foci calls LLM API, executes tools, manages sessions. |
| `"claude-code"` | Claude Code via stream-json protocol (ccstream). Structured NDJSON stdin/stdout; no tmux. |
| `"claude-code-tmux"` | Claude Code running interactively in a tmux pane (cctmux). Input via paste-buffer, output via session JSONL file watcher. |

Codex and OpenCode backends are planned but not yet implemented.

### See also

For protocol details, what runs on the delegated path vs. what's skipped, command passthrough behaviour, and choosing between flavours, see [BACKENDS.md](BACKENDS.md). For the wiring (process spawn, JSONL watcher, paste-buffer mechanism, permission flow), see [WIRING.md](WIRING.md).

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
  data/sessions/         ← session JSONL files
  data/state.db          ← persistent state
  data/memory.db         ← memory FTS index (shared mode only)
  data/WELCOME.md        ← welcome/changelog file
  <agent-workspace>/
    .data/
      conversation.db    ← conversation SQLite log
      reminders.db       ← reminder store
      scratchpad.db       ← scratchpad store
      todo.db            ← todo store
      tasklist.db        ← task list store
      memory.db          ← memory FTS index (per-agent mode)
      search.bleve       ← bleve search index (per-agent mode)
```

Per-agent databases are automatically migrated from the old shared `data_dir` layout on first startup.

### Overriding with `data_dir`

```toml
data_dir = "/opt/foci/data"
```

All data files (`*.db`, `sessions/`) resolve under `/opt/foci/data/`. Log files are unaffected — they use their own paths.

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
conversation_log = true

[sessions]
dir = "/var/data/foci/sessions"
```

---

## Recommended Directory Layout

For new installs, `setup.sh` creates this structure:

```
/home/foci/
  config/            — foci.toml, secrets.toml
  data/              — api.db, state.db, sessions/, WELCOME.md (shared databases)
  logs/              — foci.log, api.jsonl, api-payload.jsonl
  shared/            — skills/, scripts/
  <agent-id>/        — agent workspace (IDENTITY.md, SOUL.md, memory/, etc.)
    .data/           — per-agent databases (conversation.db, reminders.db, etc.)
```

The key config fields that wire this up:

```toml
data_dir = "/home/foci/data"

[sessions]
dir = "/home/foci/data/sessions"

[logging]
event_file = "/home/foci/logs/foci.log"
api_file = "/home/foci/logs/api.jsonl"
conversation_log = true

[skills]
dir = "/home/foci/shared/skills"

welcome_file = "/home/foci/data/WELCOME.md"
```

Existing flat-layout installs continue to work unchanged. To migrate, run `scripts/migrate-homedir.sh`.

---

## `secrets.toml`

Credentials file. Lives alongside `foci.toml`. Protected at the OS level by the `foci-secrets` group — see [SECRETS.md](SECRETS.md) for the full security model and setup instructions.

```toml
[anthropic]
api_key = "sk-ant-api03-..."

[telegram]
bot_token = "123456:ABC..."
primary = "123456:ABC..."
secondary = "789012:DEF..."

[brave]
api_key = "BSA..."

[openai]
api_key = "sk-..."

[groq]
api_key = "gsk_..."

[openrouter]
api_key = "sk-or-..."

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

### `allowed_agents` / `denied_agents`

Each global section can restrict which agents may access it using a whitelist (`allowed_agents`) or blacklist (`denied_agents`). Restrictions are optional — by default all agents see all global sections.

```toml
[shared_api]
token = "shared_token"
allowed_agents = ["alice", "bob"]    # only these agents see this section

[internal]
token = "internal_token"
denied_agents = ["untrusted"]        # everyone except these agents
```

Rules:
- `allowed_agents` — only listed agents can access the section (whitelist)
- `denied_agents` — listed agents are excluded, all others can access (blacklist)
- A section **cannot** have both `allowed_agents` and `denied_agents` — this is a load error
- Restrictions apply to global sections only. Agent-specific `[agents.ID.section]` values are always visible to that agent, even if the global section denies them
- When multiple agents exist but no sections have any agent restrictions, a startup warning is logged

For agent-specific secret values (rather than restricting a shared section), use per-agent overrides instead — see below.

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

Per-agent scoping applies to: exec `{{secret:NAME}}` templates, `http_request` secret resolution, output redaction, and system prompt secret names. Built-in credential resolution (anthropic.api_key, telegram bot tokens, brave API key) remains global — these are process-wide settings.

---

## Minimal Example

```toml
[agent]
id = "main"
model = "anthropic/claude-haiku-4-5"
workspace = "/home/foci/character"

[[platforms]]
id = "telegram"
[platforms.access]
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
api_key = "sk-ant-api03-..."

[telegram]
bot_token = "123456:ABC..."
```

---

## Full Example

```toml
[agent]
id = "main"
workspace = "/home/foci/character"

[agent.system]
system_files = ["IDENTITY.md", "SOUL.md", "AGENTS.md", "TOOLS.md", "USER.md", "MEMORY.md", "KEEPALIVE.md"]

[[platforms]]
id = "telegram"
[platforms.access]
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
conversation_log = true
full_payload = true
payload_file = "/home/foci/api-payload.jsonl"

[debug]
cache_bust_detect = true

[[tts]]
id = "openrouter"
format = "openai"
endpoint = "https://openrouter.ai/api/v1/audio/speech"
model = "openai/tts-1-mini"
voice = "alloy"
rate = 1.2
secret = "openrouter.api_key"

[[stt]]
id = "groq-whisper"
format = "openai"
endpoint = "https://api.groq.com/openai/v1/audio/transcriptions"
model = "whisper-large-v3"
secret = "groq.api_key"

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
