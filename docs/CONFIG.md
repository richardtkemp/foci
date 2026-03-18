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
| `data_dir` | string | `$HOME/data` | Directory for shared databases (api.db, state.db), sessions, and state files. Per-agent databases (reminders, scratchpad, todo, tasklist, conversation, memory indices) are stored in each agent's `workspace/.data/` directory. Relative paths resolve against `$HOME`. Absolute paths used as-is. |
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
| `usage_cache_ttl` | string | `"10m"` | Cache TTL for usage API responses. All callers (mana monitor, turn metadata, /mana command) share a single cache. On fetch errors, retries use exponential backoff (starting at cache TTL, doubling up to 1h). |
| `cc_expiry_threshold` | string | `"5m"` | How far before expiry to trigger a proactive token refresh. Credentials are read lazily from `~/.claude/.credentials.json` on each API call. |
| `use_sdk` | bool | `true` | Use official Anthropic SDK for API transport. When `false`, falls back to hand-rolled HTTP (legacy). SDK transport is required for streaming. |
| `streaming` | bool | `false` | Use streaming API for Anthropic requests (global default). Requires `use_sdk = true`. When enabled, text and thinking deltas are delivered incrementally. Per-agent override available in `[defaults]` and `[[agents]]`. |
| `effort` | string | `"low"` | Effort level for Anthropic API requests: `"low"`, `"medium"`, `"high"`. Applied as default for agents using Anthropic models. Per-agent override in `[[agents]]` takes precedence. Overridable at runtime via `/effort`. |
| `thinking` | string | `"adaptive"` | Thinking mode for Anthropic models: `"adaptive"` enables extended thinking. `"off"` disables. Per-agent override in `[[agents]]` takes precedence. Overridable at runtime via `/thinking`. |
| `speed` | string | `""` | Speed mode: `"fast"` enables Anthropic fast mode (beta) for ~2.5x faster output at 6x pricing. Only supported on Opus models. Uses a separate prompt cache from standard requests. Per-agent override in `[[agents]]` takes precedence. Overridable at runtime via `/speed`. |

See [AUTH.md](AUTH.md) for token resolution order and setup guide.

### `[gemini]`

Google Gemini API configuration.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `http_timeout` | string | `"120s"` | HTTP timeout for Gemini API calls. Go duration format. |
| `cache_ttl` | string | `"1h"` | Context cache TTL. System prompt + tools are cached server-side and reused across requests. Set to `"0"` to disable. |
| `thinking` | string | `"adaptive"` | Thinking mode for Gemini models: `"adaptive"` enables extended thinking. `"off"` disables. Per-agent override in `[[agents]]` takes precedence. Overridable at runtime via `/thinking`. |

Requires `gemini.api_key` in `secrets.toml`. Use `model = "gemini/gemini-2.5-flash"` in `[defaults]` or per-agent to use.

### `[openai]`

OpenAI API configuration. Also works with OpenAI-compatible endpoints (OpenRouter, Together, Groq, etc.) via `base_url`.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `base_url` | string | `""` | API base URL. Empty uses the SDK default (`https://api.openai.com`). Override for OpenRouter (`https://openrouter.ai/api/v1`), Together, Groq, local LLMs, etc. |
| `http_timeout` | string | `"120s"` | HTTP timeout for OpenAI API calls. Go duration format. |
| `reasoning` | string | `"off"` | OpenRouter reasoning mode: `"off"` disables, `"adaptive"` enables reasoning for models that support it (e.g. `openrouter/hunter-alpha`). Mapped to `agent.Thinking` via provider defaults, so per-agent `thinking` overrides this. Safe for non-OpenRouter endpoints — unsupported endpoints ignore the parameter, and 400 errors trigger automatic retry without reasoning. |

Requires `openai.api_key` in `secrets.toml`. Use `model = "openai/gpt-4o"` in `[defaults]` or per-agent to use. The SDK provides built-in retries with exponential backoff on 429/5xx errors.

### `[cache]`

Prompt caching strategy and TTL. The `strategy` field is global-only. The `ttl` field is global-or-agent (overridable per-agent via `cache_ttl` in `[defaults]` or `[[agents]]`).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `strategy` | string | `"auto"` | Cache strategy: `"auto"` (top-level, lets the API decide breakpoints) or `"explicit"` (manual breakpoints on system prompt and second-to-last message). |
| `ttl` | string | `"1h"` | Anthropic prompt cache TTL. Must be `"5m"` (5 minutes) or `"1h"` (1 hour). Only applied to Anthropic API requests — other providers ignore it. Default `"1h"` maximises cache lifetime and is recommended for most deployments. Per-agent override via `cache_ttl` in `[defaults]` or `[[agents]]`. |

### `[telegram]`

Telegram bot configuration. Fields `allowed_users` and `received_files_dir` can be overridden per-agent — see [Global-or-Agent: Telegram](#telegram-overrides).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `facet_bots` | string[] | `[]` | Shared facet pool: bot names whose tokens are resolved via `"telegram.<name>"` secret convention. Fallback for any agent whose per-agent pool is exhausted (or has no per-agent pool). |
| `facet_session_ttl` | string | `"60m"` | Idle TTL before a facet bot can be reclaimed by a new `/facet` call. If no messages to/from the bot within this window, it's considered abandoned and available for reuse. `"0"` disables auto-reclaim. Go duration format. Applies to both per-agent and shared pools. |
| `message_queue_size` | int | `64` | Outbound message queue buffer size. High-traffic bots may need larger queues. |
| `long_poll_timeout` | string | `"65s"` | Long-poll timeout for Telegram `getUpdates`. Should exceed 60s. Go duration format. |
| `display_width` | int | `44` | Character width for table width constraint. Tables in `<pre>` blocks are shrunk to fit this width and cells are wrapped or truncated. Overridable per-agent. |
| `table_wrap_lines` | int | `5` | Max wrapped lines per table cell when tables are constrained to `display_width`. `0` truncates with `…` instead of wrapping. Overridable per-agent. |
| `table_style` | string | `"pretty"` | Table rendering style: `"pretty"` (no pipe borders, `─` separator, 2-space column gaps) or `"markdown"` (pipe-delimited `\| col \| col \|`). Overridable per-agent. |

#### Bot token resolution

Bot tokens are resolved by convention: `"telegram.<botname>"` in `secrets.toml`. No explicit bot map is needed.

For example, an agent with `telegram_bot = "primary"` resolves its token from the secret key `telegram.primary`. To override the convention, set `bot_secret` on the agent.

`secrets.toml`:
```toml
[telegram]
primary = "123456:ABC..."
secondary = "789012:DEF..."
```

### `[discord]`

Discord bot configuration. Fields `allowed_users`, `guild_id`, and `received_files_dir` can be overridden per-agent — see [Global-or-Agent: Discord](#discord-overrides).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `allowed_users` | string[] | `[]` | Discord user ID snowflakes allowed to interact with the bot. |
| `guild_id` | string | `""` | Restrict to a single guild. Empty allows all guilds. |
| `require_mention` | bool | `true` | Require @mention in guild channels. DMs are always processed. |
| `auto_thread` | bool | `true` | Create threads for facet sessions. |
| `startup_notify` | bool | `true` | Send notification on startup. |
| `facet_session_ttl` | string | `"60m"` | Idle TTL before a facet thread can be reclaimed. `"0"` disables auto-reclaim. Go duration format. |
| `message_queue_size` | int | `64` | Inbound message queue buffer size. |
| `display_width` | int | `60` | Character width for dividers in Discord messages. Overridable per-agent. |
| `received_files_dir` | string | `""` | Save received files to this directory. Empty disables. Overridable per-agent. |

#### Bot token resolution

Bot tokens are resolved by convention: `"discord.<botname>"` in `secrets.toml`. No explicit bot map is needed.

For example, an agent with `bot = "primary"` in `[agents.platforms.discord]` resolves its token from the secret key `discord.primary`. To override the convention, set `bot_secret` on the agent.

`secrets.toml`:
```toml
[discord]
primary = "MTIzNDU2Nzg5..."
```

### `[http]`

HTTP API server.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `port` | int | `18791` | HTTP server port. |
| `bind` | string | `"127.0.0.1"` | Bind address. Use `0.0.0.0` for external access. |
| `graceful_shutdown_timeout` | string | `"30s"` | Time to wait for in-flight requests on shutdown. Go duration format. |

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
| `conversation_file` | string | `$data_dir/conversation.db` | Base path for per-agent conversation SQLite logs. Each agent's database is stored at `workspace/.data/conversation.db`. Set to `""` to disable conversation logging. On startup, databases at the old shared location (`conversation-{agentID}.db` in `data_dir`) are automatically migrated to the workspace. |
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
| `log_file_mode` | string | `"0600"` | Octal file permissions for log files (event, API, payload). Applied on creation and after rotation. Use `"0640"` for group-readable logs. |

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
| `archive_after` | string | `"24h"` | Gzip idle session files after this duration of inactivity. Go duration format. Each agent's most recently created chat session is never archived regardless of age. Sessions with active branches are also skipped. Archived sessions are transparently decompressed when accessed. `"0"` effectively disables (no sessions will be old enough). |

Sessions are stored as JSONL files at `{dir}/agent/{id}/{type}.jsonl`.

All prompt fields (`compaction_summary_prompt`, `branch_orientation_facet_prompt`, `branch_orientation_headless_prompt`) are file paths, not inline strings. If the file can't be read, a warning is logged and the embedded default is used. Prompt files are read live at the point of use — edits take effect immediately without restart or `/reload`.

When no config override is set, embedded defaults from `prompts/` are used:
- `prompts/branch-orientation-headless.md` — headless branches (cron, spawn, keepalive)
- `prompts/branch-orientation-facet.md` — user-attached facet branches
- `prompts/compaction-summary.md` — compaction summary prompt
- `prompts/compaction-handoff.md` — post-compaction handoff message
- `prompts/keepalive.md` — keepalive ping prompt
- `prompts/background.md` — background work prompt
- `prompts/memory-formation.md` — memory formation prompt (interval + session-end)
- `prompts/memory-consolidation.md` — MEMORY.md consolidation prompt

### `[memory]`

Memory system (full-text search over markdown files + conversation history).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `search_backends` | string[] | `["fts5"]` | Active search backends. Valid values: `"fts5"` (SQLite FTS5), `"bleve"` (blevesearch/bleve). Both can run simultaneously for A/B comparison. |
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
| `goroutine_monitor_threshold` | int | `0` (auto) | Warn when `runtime.NumGoroutine()` exceeds this value. `0` = auto: 35 × number of agents. |

Both thresholds require memory pressure (PSI `avg10` from `/proc/pressure/memory` exceeding `memory_pressure_threshold`) before acting. This avoids false alarms when the system has ample free RAM despite high RSS. The guard reads `/proc` directly — no external commands.

### `[debug]`

Developer and debugging knobs. All off by default.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `log_api_key_suffix` | bool | `false` | Log the last 4 characters of API keys at DEBUG level on each provider API call. Applies to all providers (Anthropic, OpenAI, Gemini, voice) and secrets used in `http_request` tool calls. Useful for diagnosing which credential is being used when multiple keys are configured. |
| `compaction_debug` | bool | `false` | Send the compaction summary to Telegram as a markdown file attachment after compaction completes. Useful for verifying what survived the cut. |

### `[database]`

SQLite database settings.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `busy_timeout` | string | `"5s"` | SQLite busy timeout for concurrent access. Go duration format. High-load systems may need longer waits. |

### `[models]`

Model aliases, model groups, and call site overrides.

The `aliases` map allows shorthand names to be resolved to full `developer/model_id` identifiers in both `/model` command and the agent wizard. These are the built-in defaults if not configured.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `aliases` | map | see below | Shorthand → `developer/model_id` mapping. |
| `powerful` | string | `""` | Model for primary tasks (chat, compaction, memory). Can be an alias (e.g. `"opus"`) or `developer/model_id`. When set, enables **multi-model mode** — other groups default to this model unless explicitly overridden. When empty (default), all call sites use the agent's session model (single-model mode). |
| `fast` | string | `""` | Model for fast tasks (spawn-raw, spawn-character). Defaults to `powerful` when unset. |
| `cheap` | string | `""` | Model for cheap tasks (spawn-explore, summarize-tool, summarize-file, prompt-diff). Defaults to `powerful` when unset. |

**`[models.calls]`** — Override which group a specific call site uses. Keys are call site names, values are group names (`powerful`, `fast`, `cheap`).

Default call site → group assignments:

| Group | Call sites |
|-------|-----------|
| **powerful** | `chat`, `spawn-clone`, `background`, `compaction`, `memory-capture`, `memory-consolidate` |
| **fast** | `spawn-raw`, `spawn-character` |
| **cheap** | `spawn-explore`, `summarize-tool`, `summarize-file`, `prompt-diff` |

Ungrouped call sites (`keepalive`, `count-tokens`) always use the session model regardless of group configuration.

Default aliases (used when `[models]` section is not configured):
- `opus` → `anthropic/claude-opus-4-6`
- `sonnet` → `anthropic/claude-sonnet-4-6`
- `haiku` → `anthropic/claude-haiku-4-5`
- `flash` → `gemini/gemini-2.5-flash`
- `pro` → `gemini/gemini-2.5-pro`
- `gpt4o` → `openai/gpt-4o`
- `o3` → `openai/o3`
- `o4mini` → `openai/o4-mini`

Example — multi-model setup with aliases and a call site override:
```toml
[models]
powerful = "opus"
fast = "sonnet"
cheap = "haiku"

[models.calls]
compaction = "cheap"       # use cheap model for compaction instead of powerful

[models.aliases]
opus = "anthropic/claude-opus-5-0"
sonnet = "anthropic/claude-sonnet-5-0"
local = "local/my-fine-tuned-model"
```

**`[models.fallbacks]`** — Automatic model failover on transient errors. When a model returns 529 (overloaded), 5xx (server error), or times out (`context.DeadlineExceeded`), the agent automatically retries the request with the configured fallback model. Fallback is per-request — the primary model is always tried first on the next turn.

Keys and values can be aliases or `developer/model_id` format. Chains are supported (e.g., opus → sonnet → haiku) up to a maximum depth of 3. Cycles are detected and broken at startup.

Not triggered by: 401 auth errors, 400 bad requests, 429 rate limits.

```toml
[models.fallbacks]
opus = "sonnet"                                          # alias → alias
sonnet = "haiku"                                         # chains: opus → sonnet → haiku
"google/gemini-2.5-pro" = "anthropic/claude-sonnet-4-6"  # cross-endpoint fallback
```

Per-agent overrides via `model_fallbacks` in `[[agents]]`:

```toml
[[agents]]
id = "research"
[agents.model_fallbacks]
opus = "google/gemini-2.5-pro"   # override global fallback for this agent
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
| `http_timeout` | string | `""` | HTTP timeout. Go duration format. Empty uses format-specific default. |

Built-in endpoint defaults:
- `anthropic` — `format = "anthropic"`, `api_key = "anthropic.api_key"`
- `gemini` — `format = "gemini"`, `api_key = "gemini.api_key"`
- `openai` — `format = "openai"`, `api_key = "openai.api_key"`
- `openrouter` — multi-format (`anthropic_url` + `openai_url` both set to `https://openrouter.ai/api/v1`), `api_key = "openrouter.api_key"`

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

The `summary` tool uses the **cheap** model group (call site: `summarize-file`). Configure via `[models]` cheap or `[models.calls]` overrides.

Tmux memory monitoring detects runaway memory from long-running tmux sessions (glibc malloc fragmentation). Notifications are sent to agents whose `inject_agent_warnings` is `false` — agents with injection enabled already see log warnings in their session.

### `[tools.browser]`

Browser automation tool configuration. Enabled by default. Agents get a `browser` tool that uses accessibility tree snapshots with element refs for interaction.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `true` | Enable browser tool for all agents. |
| `headless` | bool | `true` | Run browser in headless mode. Set `false` for debugging. |
| `timeout_sec` | int | `30` | Default timeout for page operations in seconds. |
| `user_data_dir` | string | `""` | Chrome user data directory. Empty uses a temp profile. Ignored when `incognito = true`. |
| `executable_path` | string | `""` | Path to Chrome/Chromium binary. Empty uses auto-detection via go-rod launcher. |
| `incognito` | bool | `true` | Use incognito mode (no persistent cookies/storage). |
| `dom_stable_sec` | float | `1.0` | DOM stability check interval in seconds before capturing auto-snapshots. |
| `dom_stable_diff` | float | `0.2` | DOM change threshold (0.0–1.0) for stability detection. Lower = stricter. |

Per-agent override: `browser_enabled` in `[[agents]]` overrides `tools.browser.enabled`.

Example:
```toml
[tools.browser]
enabled = true
headless = true
timeout_sec = 30
```

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

**Resolution order:** agent value > `[defaults]` value > global section value > hardcoded default.

Set global defaults in `[defaults]`:
```toml
[defaults]
model = "anthropic/claude-sonnet-4-6"
max_tool_loops = 50
system_files = ["IDENTITY.md", "SOUL.md", "COHERENCE.md"]
```

Effort, thinking, and speed defaults are set in provider sections (`[anthropic]`, `[gemini]`, `[openai]`) and automatically applied based on the agent's model format. Per-agent overrides use provider-specific subsections (`[agents.anthropic]`, `[agents.gemini]`, `[agents.openai]`). At runtime, unsupported params are skipped with a warning; if a model returns a 400 error about thinking/effort/speed, the params are stripped and the request is retried once.

Override per-agent in `[[agents]]`:
```toml
[[agents]]
id = "research"
model = "anthropic/claude-sonnet-4-6"
max_tool_loops = 25

[agents.anthropic]
effort = "high"
thinking = "adaptive"
```

### Model & Response

`model` and `max_output_tokens` are set in `[llm]`, overridable per-agent. Other fields are set in `[defaults]`.

| Key | Type | Default | Section | Description |
|-----|------|---------|---------|-------------|
| `model` | string | `"anthropic/claude-haiku-4-5"` | `[llm]` | Model in `developer/model_id` format. The developer prefix selects which API endpoint to use (e.g. `"gemini/gemini-2.5-flash"`, `"openrouter/claude-opus-4-6"`). Wire format is auto-inferred from model name (`claude-*` → anthropic, `gemini-*` → gemini, `gpt-*`/`o3*`/`o4*` → openai). Bare model names without `/` are auto-migrated with an inferred developer. |
| `max_output_tokens` | int | `16384` | `[llm]` | Maximum tokens in model response. Larger values allow longer responses. |
| `max_tool_loops` | int | `25` | `[defaults]` | Maximum tool iterations per agent turn. Complex tasks may need more. |
| `[agents.anthropic] effort` | string | `""` | Effort level: `"low"`, `"medium"`, `"high"`. Per-agent override; defaults from `[anthropic] effort`. Only applied for Anthropic models. Overridable at runtime via `/effort`. |
| `[agents.anthropic] thinking` | string | `""` | Thinking mode: `"adaptive"` or `"off"`. Per-agent override; defaults from `[anthropic] thinking`. Overridable at runtime via `/thinking`. |
| `[agents.anthropic] speed` | string | `""` | Speed mode: `"fast"` for Anthropic fast mode (Opus only, beta, 6x pricing). Per-agent override; defaults from `[anthropic] speed`. Overridable at runtime via `/speed`. |
| `[agents.gemini] thinking` | string | `""` | Gemini thinking mode: `"adaptive"` or `"off"`. Per-agent override; defaults from `[gemini] thinking`. Overridable at runtime via `/thinking`. |
| `[agents.openai] reasoning` | string | `""` | OpenAI/OpenRouter reasoning mode: `"adaptive"` or `"off"`. Per-agent override; defaults from `[openai] reasoning`. Mapped to thinking at runtime. |
| `streaming` | bool | `false` | Use streaming API. Text and thinking deltas are delivered incrementally. Requires Anthropic provider with `use_sdk = true`. Per-agent override; `[anthropic] streaming` sets the global default. |
| `cache_ttl` | string | `""` | Anthropic prompt cache TTL override. Must be `"5m"` or `"1h"`. Empty inherits from `[cache] ttl` (default `"1h"`). Only applied to Anthropic API requests. |
| `system_files` | string[] | see below | Ordered list of workspace files to load as system prompt blocks. |

Default `system_files` order (most-stable first for cache efficiency):
```
["IDENTITY.md", "SOUL.md", "COHERENCE.md", "AGENTS.md", "TOOLS.md", "USER.md", "MEMORY.md", "KEEPALIVE.md"]
```

Missing files are silently skipped. The last file gets the cache breakpoint marker.

### Braindead Warning

Implemented as a built-in nudge rule with an `every_n_tools` trigger. When tool calls reach the threshold, a warning is injected via the nudge system. Subject to the same cooldown and rate-limiting as other nudge rules. Set in `[defaults]`, overridable per-agent.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `braindead_threshold` | int | `10` | Tool calls before injecting a braindead warning. `0` disables. |
| `braindead_prompt` | string | `""` | Custom warning text injected when the threshold is hit. `""` uses a hardcoded default. |
| `turn_lock_warn_threshold` | string | `"3m"` | Warn if turn lock wait exceeds this duration. Go duration format. `proactive_warning` triggers are excluded. |

### Nudge System

Mid-turn behavioral reminders extracted from character files. Rules are extracted by an LLM from the agent's character files (system prompt) and stored in `{workspace}/nudge-rules.json` (or `{workspace}/character/nudge-rules.json` if the `character/` directory exists). Rules are re-extracted when character files change (detected via content hash on `/reload` or compaction).

Available in both `[defaults]` and `[[agents]]`.

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

**Trigger types** (configured per-rule in `nudge-rules.json`):
- `every_n_tools(N)` — remind every N individual tool calls during a turn (default N=5)
- `every_n_turns(N)` — remind every N user turns; lifetime counter, never reset (used by default nudges)
- `pre_answer` — remind just before the model returns a final answer
- `after_error` — remind when a tool call returns an error
- `regex(pattern)` — remind when the user's message matches a Go regex pattern

### Display

Set in `[telegram]`, overridable per-agent via `[agents.platforms.telegram]`. At runtime, the `/display` command sets per-session overrides without modifying the config file:

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

Set in `[defaults]`, overridable per-agent.

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
| `compaction_notify` | bool | `true` | Send a Telegram notification when compaction occurs. |
| `task_list_notify` | bool | `true` | Send Telegram notifications when task list entries are created, started, or completed. Shows progress like "✅ 3/5: Fixed token counting". |
| `compaction_preserve_messages` | int | `25` | Preserve the last N messages through compaction. Preserved messages are appended verbatim after the summary + handoff, keeping their original roles. `0` disables (summary only). The summarizer only sees messages *before* the preserved window. |
| `compaction_effort` | string | `""` | Effort level for compaction API calls: `"low"`, `"medium"`, `"high"`. `""` uses session effort. Useful when agent uses low effort for chat but needs higher quality for compaction. |
| `autocompact_before_mana_refresh` | bool | `true` | Master switch for mana-refresh compaction. `false` disables entirely (replaces the old `"0"` disable convention). |
| `autocompact_before_mana_refresh_threshold` | string | `"5m"` | Trigger mana-refresh compaction when mana reset is within this duration. Format: Go duration string. |
| `autocompact_before_mana_refresh_factor` | float | `0.5` | Secondary compaction threshold for mana-refresh mode, as a fraction of the main `compaction_threshold`. E.g. with threshold 0.8 and factor 0.5, mana-refresh triggers at 40% context usage. Range: 0.0–1.0. |
| `autocompact_before_mana_refresh_preserve` | int | unset | Explicit message count to preserve during mana-refresh compaction. Overrides the percentage-based default. `0` uses normal preservation count. |
| `autocompact_before_mana_refresh_preserve_pct` | float | `0.5` | Fraction of messages to preserve during mana-refresh compaction (0.0–1.0). Default 0.5 preserves 50% of messages, summarising the older half. Only used when `autocompact_before_mana_refresh_preserve` is unset. |
| `session_reset_prompt` | string | `""` | Path to session reset prompt file. `""` uses embedded default. |
| `branch_orientation_facet_prompt` | string | `""` | Path to prompt file for user-attached facet branches. Supports template variables `{branch_key}`, `{parent_key}`, `{branch_type}`, `{direct_chat}`. `""` uses embedded default from `prompts/branch-orientation-facet.md`. |
| `branch_orientation_headless_prompt` | string | `""` | Path to prompt file for headless branches (cron, spawn, keepalive). Same template variables. `""` uses embedded default from `prompts/branch-orientation-headless.md`. |

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
autocompact_before_mana_refresh = true           # master switch (default true)
autocompact_before_mana_refresh_threshold = "10m" # wider window
autocompact_before_mana_refresh_factor = 0.3      # trigger at 24% context
autocompact_before_mana_refresh_preserve = 50     # preserve last 50 messages
```

### Tool Behavior

Global defaults set in `[tools]` (or `[defaults]` where noted), overridable per-agent. Per-agent `0` inherits from `[tools]`.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `max_result_chars` | int | `15000` | Max characters in a tool result before writing to a temp file and returning a guard message (no partial content). Global: `[tools]` or `[defaults]`. |
| `max_summary_chars` | int | `300000` | Max chars to auto-summarise via the cheap model. Results larger than this are saved to file with hints but skip the summary call. Global: `[tools]` or `[defaults]`. |
| `auto_summarise` | bool | `true` | Auto-summarise oversized tool results via the cheap model. `false` skips summary calls entirely (results are saved to file with hints instead). Global: `[tools]` or `[defaults]`. Per-agent `unset` inherits from `[tools]`. |
| `max_summary_input_chars` | int | `100000` | Max chars of tool result text embedded in the summary prompt. Larger results are truncated in the prompt (the full output is on disk). Prevents excessive memory use and token cost during auto-summarisation. Global: `[tools]` or `[defaults]`. |
| `max_image_pixels` | int | `2073600` | Max pixels (width × height) for images before downscaling. Images exceeding this are proportionally resized and re-encoded as JPEG (quality 85). Default is 1920×1080. `0` disables downscaling. Global: `[tools]` or `[defaults]`. |
| `exec_auto_background` | int | `10` | Seconds before auto-backgrounding long-running exec and http_request calls. `0` disables. Global: `[tools]`. |
| `max_concurrent_spawns` | int | `3` | Max concurrent `spawn` clone sessions per agent. Global: `[tools]`. |
| `explore_max_depth` | int | `100` | Max tool loops for `spawn` explore mode. Explore agents do multi-step research so this is higher than the default `max_tool_loops`. Global: `[tools]`. |
| `max_upload_file_size` | int | `52428800` | Max file size in bytes for multipart/form-data file uploads (default 50MB). Global: `[tools]`. |
| `search_provider` | string | `"brave"` | Web search provider: `"brave"` (client-side, needs `brave_api_key`) or `"anthropic"` (server-side). Brave is recommended: Anthropic's server-side search returns encrypted content blobs that massively inflate token counts (observed: 256k tokens from just two searches) and bypass the tool result size guard entirely. Brave results are client-side, guardable, and far more token-efficient. Global: `[tools]` or `[defaults]`. |
| `fetch_provider` | string | `"builtin"` | Web fetch provider. See [TOOLS.md](TOOLS.md) for provider details. Global: `[tools]` or `[defaults]`. |

### Notifications & Logging

| Key | Type | Default | Global location | Description |
|-----|------|---------|-----------------|-------------|
| `startup_notify` | bool | `true` | `[telegram] startup_notify` | Send a startup notification when the service starts. `false` for silent bots (e.g. cron-only agents). |
| `inject_agent_warnings` | bool | `false` | `[defaults]` | Feed WARN/ERROR log events into this agent's conversation as system warnings before each turn. Per-agent — some agents can have injection enabled while others rely on Telegram notifications. |
| `messages_in_log` | bool | `false` | `[logging]` | Log user message content to the event log. When `false`, messages are logged at DEBUG level with no content for privacy. When `true`, messages are logged at INFO level with content (truncated to 100 chars). Per-agent `unset` inherits from global. |
| `steer_mode` | bool | `true` | `[defaults]` | When enabled and the agent is mid-turn (executing tool calls), user messages are injected between tool calls at the next tool boundary as `[user]` content blocks instead of queuing behind the turn lock. This lets users redirect a runaway agent without `/stop`. System messages (keepalive, warnings) are unaffected. |
| `stream_output` | bool | `false` | `[telegram]` / `[agents.platforms.telegram]` | Stream model output to Telegram in real-time with HTML formatting. A message is created on the first text delta and edited periodically as more tokens arrive. Each update strips incomplete markdown delimiters and converts to Telegram HTML, so formatting renders throughout streaming (not just on the final message). Falls back to plain text if HTML parsing fails. Requires `streaming = true` for API-level delta callbacks. Set globally in `[telegram]` or per-agent in platform config. |
| `stream_update_interval` | string | `"250ms"` | `[telegram]` / `[agents.platforms.telegram]` | Duration between Telegram message edits during streaming. Go duration format. Lower values give smoother updates but increase API calls. Per-agent override via `stream_interval` in platform config. |
| `facet_no_compact` | bool | `true` | `[defaults]` | Set `no_compact` on facet sessions. Facet sessions are short-lived parallel forks that shouldn't trigger compaction. Set to `false` if you want facet sessions to compact normally. |

### Telegram Overrides

| Key | Type | Default | Global location | Description |
|-----|------|---------|-----------------|-------------|
| `allowed_users` | string[] | `[]` | `[telegram]` | Telegram user IDs allowed to interact with bots. `[]` falls back to global `[telegram] allowed_users`. |
| `received_files_dir` | string | `$workspace/received_files` | `[telegram]` | Save received media (images, videos, video notes, documents) to this directory. `""` in global disables. Per-agent defaults to `$workspace/received_files`. Relative paths resolve against `$HOME`. Filename formats — Images: `YYYY-MM-DDTHH-MM-SSZ_chat-CHATID.ext`. Videos: `YYYY-MM-DDTHH-MM-SSZ_video_chat-CHATID.ext`. Video notes: `YYYY-MM-DDTHH-MM-SSZ_videonote_chat-CHATID.mp4`. Documents: `YYYY-MM-DDTHH-MM-SSZ_document_chat-CHATID.ext`. The agent sees `[Image/Video/Document saved to: /path/to/file]` in the message text. Files over 20MB (Telegram Bot API limit) show `[Video/Document too large to download (N MB)]` instead. |
| `display_width` | int | `44` | `[telegram]` | Per-agent override for `[telegram] display_width`. |
| `table_wrap_lines` | int | `5` | `[telegram]` | Per-agent override for `[telegram] table_wrap_lines`. |
| `table_style` | string | `"pretty"` | `[telegram]` | Per-agent override for `[telegram] table_style`. |

### Discord Overrides

Set in `[discord]`, overridable per-agent via `[agents.platforms.discord]`.

| Key | Type | Default | Inherits from | Description |
|-----|------|---------|---------------|-------------|
| `allowed_users` | string[] | `[]` | `[discord]` | Discord user IDs allowed to interact with bots. `[]` falls back to global `[discord] allowed_users`. |
| `guild_id` | string | `""` | `[discord]` | Restrict to this guild. Empty uses global. |
| `require_mention` | bool | `true` | `[discord]` | Require @mention in guild channels. |
| `auto_thread` | bool | `true` | `[discord]` | Create threads for facet sessions. |
| `display_width` | int | `60` | `[discord]` | Per-agent override for `[discord] display_width`. |
| `received_files_dir` | string | `""` | `[discord]` | Per-agent directory for saving received files. |

### Voice

| Key | Type | Default | Global location | Description |
|-----|------|---------|-----------------|-------------|
| `tts_rate` | float | `0` | `[defaults]` | Per-agent TTS speech rate multiplier. Combined with entry rate: effective = entry.rate × agent.tts_rate (0 treated as 1.0). |
| `tts` | string | `""` | `[defaults]` | Override TTS entry by id (empty = default entry). |
| `stt` | string | `""` | `[defaults]` | Override STT entry by id (empty = default entry). |
| `tts_replacements` | map | `{}` | `[defaults]` | TTS word replacements (merged with `[[tts]]` entry replacements; per-agent wins). Case-insensitive whole-word matching. |
| `stt_replacements` | map | `{}` | `[defaults]` | STT word replacements (merged with `[[stt]]` entry replacements; per-agent wins). Case-insensitive whole-word matching. |

### Keepalive (`[keepalive]` / `[[agents.keepalive]]`)

Cache keepalive timer. Fires a lightweight branch session to keep the Anthropic cache prefix warm.

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
| `interval` | string | `"5m"` | Time since last interaction before firing. |
| `prompt` | string | `""` | Prompt file path. `""` = embedded default, `"default"` = embedded, `"none"` = disabled, `/path` = custom file. |

**Validation warnings:**
- `background.interval > keepalive.interval` — keepalive resets the cache timer; background work may never trigger.
- `keepalive.interval > [cache] ttl` — cache may expire between keepalives (default TTL is 1 hour).

### Mana (`[mana]`)

Controls mana budget behavior. Global-only (not overridable per-agent).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `invest_interval` | string | `"30m"` | Quiet period after mana reset before spending. The manamometer prevents background work from running during this period to allow cache building. |

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
| `session_end_enabled` | bool | `true` | Run memory formation on `/reset` and facet reclaim. |
| `session_end_prompt` | string | `""` | Prompt override. `""` = embedded `memory-formation.md`, `"none"` = disabled, `/path` = custom file. |
| `compaction_enabled` | bool | `true` | Run memory formation before compaction summarises context. |
| `compaction_prompt` | string | `""` | Prompt override. `""` = embedded `memory-formation.md`, `"none"` = disabled, `/path` = custom file. |

All prompt fields use 3-state resolution: `""` or `"default"` → embedded default from `prompts/`, `"none"` → disabled, file path → read file with embedded fallback on error.

**Interval memory formation** runs in the keepalive timer loop. Fires when:
1. `interval` has elapsed since the last formation
2. There's been user activity since the last formation
3. The user has been active within the interval window

**Consolidation** reviews daily memory files and curates MEMORY.md. The last-run timestamp is persisted in state, so it survives restarts. Only fires when there's been user activity within the last hour.

**Session-end** fires asynchronously on `/reset` and facet reclaim. Creates a branch from the expiring session (preserving conversation history) so the caller doesn't block.

**Compaction** fires immediately before compaction summarises and replaces context. Creates a branch from the pre-compaction session so the memory agent sees the full conversation history that's about to be summarised. The branch is created synchronously (capturing the branch point) before compaction starts; the memory agent runs asynchronously in a goroutine.

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

### Webhooks

| Key | Type | Default | Global location | Description |
|-----|------|---------|-----------------|-------------|
| `webhooks` | map[string]string | `{}` | `[defaults]` | Maps webhook hook IDs to prompt file paths. Used by `POST /webhook/{agent}/{hookid}`. Per-agent merges with global (agent keys override matching global keys; unmatched global keys are preserved). |

Prompt paths are resolved via `prompts.ResolvePrompt`: bare filenames (e.g. `"deploy.md"`) are searched in `{workspace}/prompts/` then `{shared}/prompts/`; absolute paths are read directly.

```toml
[defaults]
webhooks = { new_commit = "new_commit.md", deploy = "deploy.md" }

[[agents]]
id = "scout"
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

### Platform Configuration (`[agents.platforms.telegram]`) — NEW

Per-agent platform settings are configured in the `[agents.platforms.telegram]` section.

```toml
[[agents]]
id = "myagent"

[agents.platforms.telegram]
bot = "myagent"                 # bot name; token via "telegram.<bot>" secret
bot_secret = ""                 # override secret key (default: "telegram.<bot>")
facet_bots = []             # additional bot names for facet
allowed_users = []              # per-agent allowed users (empty = use global)
show_tool_calls = "preview"     # off, preview, full
show_thinking = "off"           # off, compact, true
display_width = 44
table_wrap_lines = 5
table_style = "pretty"
stream_output = false
stream_interval = "250ms"
received_files_dir = ""
```

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `bot` | string | `$id` | Bot name for this agent. Token resolved from secret `"telegram.<bot>"`. |
| `bot_secret` | string | `""` | Override secret key for bot token. `""` uses `"telegram.<bot>"`. |
| `facet_bots` | string[] | `[]` | Per-agent facet bot pool. Tokens resolved via `"telegram.<name>"` secret. |
| `allowed_users` | string[] | `[]` | Per-agent allowed Telegram user IDs. Empty uses global `[telegram] allowed_users`. |
| `show_tool_calls` | string | `[telegram]` | Tool call visibility: `off` (hidden), `preview` (shown then overwritten), `full` (kept). |
| `show_thinking` | string | `[telegram]` | Thinking visibility: `off`, `compact` (toggle button), `true` (inline). |
| `display_width` | int | `[telegram]` | Display width for dividers in Telegram messages. |
| `table_wrap_lines` | int | `[telegram]` | Max wrapped lines per table cell. |
| `table_style` | string | `[telegram]` | Table style: `pretty` or `markdown`. |
| `stream_output` | bool | `[telegram]` | Stream model output to Telegram in real-time. |
| `stream_interval` | duration | `[defaults]` | Duration between message edits during streaming. |
| `received_files_dir` | string | `[telegram]` | Save received files to this directory. |

### Platform Configuration (`[agents.platforms.discord]`)

Per-agent Discord platform settings.

```toml
[[agents]]
id = "myagent"

[agents.platforms.discord]
bot = "myagent"                 # bot name; token via "discord.<bot>" secret
bot_secret = ""                 # override secret key (default: "discord.<bot>")
allowed_users = ["12345"]       # per-agent allowed Discord user IDs
guild_id = ""                   # restrict to this guild
show_tool_calls = "off"         # off, preview, full
show_thinking = "off"           # off, compact, true
display_width = 60
stream_output = false
stream_interval = "1200ms"
require_mention = true
auto_thread = true
received_files_dir = "received"
```

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `bot` | string | `$id` | Bot name for this agent. Token resolved from secret `"discord.<bot>"`. |
| `bot_secret` | string | `""` | Override secret key for bot token. `""` uses `"discord.<bot>"`. |
| `allowed_users` | string[] | `[]` | Per-agent allowed Discord user IDs. Empty uses global `[discord] allowed_users`. |
| `guild_id` | string | `""` | Restrict to this guild. Empty uses global. |
| `show_tool_calls` | string | `[discord]` | Tool call visibility: `off` (hidden), `preview` (shown then overwritten), `full` (kept). |
| `show_thinking` | string | `[discord]` | Thinking visibility: `off`, `compact` (toggle button), `true` (inline). |
| `display_width` | int | `[discord]` | Display width for dividers in Discord messages. |
| `stream_output` | bool | `[discord]` | Stream model output to Discord in real-time. |
| `stream_interval` | string | `[discord]` | Duration between Discord message edits during streaming. Default `1200ms`. |
| `require_mention` | bool | `[discord]` | Require @mention in guild channels. |
| `auto_thread` | bool | `[discord]` | Create threads for facet sessions. |
| `received_files_dir` | string | `[discord]` | Save received files to this directory. |

### Memory (`[[agents.memory.sources]]`)

Agents can have their own memory directories in addition to the global sources. Global `[[memory.sources]]` are always prepended to each agent's sources — agents inherit global sources automatically. When any agent has per-agent memory configured, each agent gets its own FTS5 index (`memory-{agentID}.db`) combining global + agent-specific sources.

Agent-specific sources automatically receive a weight boost of +1.0, so they rank higher than global sources with the same base weight. Source names are prefixed with `agent/` in search results.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `memory.sources` | array | see below | Per-agent memory directories. Combined with global `[memory]` sources. When empty, defaults to a single source: `{name: $id, dir: $workspace/memory, weight: 1.0}`. |

Each source entry:

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `name` | string | required | Source identifier (prefixed with `agent/` in results). |
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

# Shared facet pool (fallback for any agent)
[telegram]
facet_bots = ["spare1"]

[[agents]]
id = "main"
model = "anthropic/claude-sonnet-4-6"
workspace = "/home/foci/character"

[agents.platforms.telegram]
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

[agents.platforms.telegram]
bot = "secondary"
# no facet_bots — uses shared pool only

[[agents.memory.sources]]
name = "workspace"
dir = "/home/foci/scout/memory"
weight = 1.0
```

**Facet acquisition priority:** When `/facet` is invoked, per-agent pool is tried first. If all per-agent bots are busy (or none configured), the shared pool is used as fallback. Released bots return to whichever pool they came from.

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

Per-agent scoping applies to: exec `{{secret:NAME}}` templates, `http_request` secret resolution, output redaction, and system prompt secret names. Built-in credential resolution (anthropic.setup_token, anthropic.api_key, telegram bot tokens, brave API key) remains global — these are process-wide settings.

---

## Minimal Example

```toml
[agent]
id = "main"
model = "anthropic/claude-haiku-4-5"
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
model = "anthropic/claude-sonnet-4-6"
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
