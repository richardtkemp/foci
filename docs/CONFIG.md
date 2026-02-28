# Foci Configuration Reference

Foci uses two TOML files: `foci.toml` (main config) and `secrets.toml` (credentials). Pass the config path with `-config`:

```
focigw -config /home/foci/foci.toml
```

Secrets are loaded from `secrets.toml` in the same directory as the config file. Values in `secrets.toml` override matching fields in `foci.toml`.

---

## `[agent]` / `[[agents]]`

Core agent settings. Use `[agent]` for a single agent (legacy) or `[[agents]]` for multiple agents. When both are present, `[[agents]]` takes precedence.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `id` | string | `""` | Agent identifier. Used in session keys (`agent:ID:main`). |
| `name` | string | `""` | Human-readable name (e.g. `"Clutch"`). Used in `/voice` WebSocket agent list. |
| `emoji` | string | `""` | Emoji for agent (e.g. `"🥔"`). Used in `/voice` WebSocket agent list. |
| `model` | string | `"claude-haiku-4-5"` | Anthropic model ID for API calls. |
| `workspace` | string | `$HOME/$id` | Path to workspace directory containing character files (IDENTITY.md, SOUL.md, etc.). Defaults to `$HOME/<agent-id>` if not set. |
| `system_files` | string[] | see below | Ordered list of workspace files to load as system prompt blocks. |
| `duplicate_messages` | bool | `false` | Send user text twice per API call. Can improve instruction following. |
| `branch_orientation_prompt` | string | `""` | Path to prompt file injected into all branch sessions (multiball, cron, spawn). Supports template variables `{branch_key}`, `{parent_key}`, `{branch_type}`, `{direct_chat}`. If empty, embedded defaults from `prompts/branch-orientation-headless.md` or `prompts/branch-orientation-multiball.md` are used. |
| `fork_prompt` | string | `""` | **Deprecated:** use `branch_orientation_prompt`. Path to prompt file injected into branch sessions. If `branch_orientation_prompt` is set, `fork_prompt` is ignored. |
| `telegram_bot` | string | `$id` | References a key in `[telegram.bots]` map. Assigns this bot to the agent. Defaults to the agent ID if a matching key exists in `[telegram.bots]`. |
| `multiball_bots` | string[] | `[]` | References keys in `[telegram.bots]` map. Per-agent multiball pool for `/multiball` sessions. |
| `multiball_bot` | string | `""` | **Deprecated:** use `multiball_bots`. If set and `multiball_bots` is empty, promoted to a single-element list with a warning. |
| `memory.sources` | array | `[]` | Per-agent memory directories (see below). Combined with global `[memory]` sources. |
| `max_tool_loops` | int | `25` | Maximum tool iterations per agent turn. Complex tasks may need more. |
| `max_output_tokens` | int | `8192` | Maximum tokens in model response. Larger values allow longer responses. |
| `effort` | string | `""` | Effort level for API requests: `"low"`, `"medium"`, `"high"`. Empty = omit (use API default). Overridable at runtime via `/effort` command. Per-session overrides persist across restarts via state store and reset when a new session starts. |
| `thinking` | string | `""` | Thinking mode: `"adaptive"` enables adaptive extended thinking (Opus 4.6). Empty or `"off"` = disabled. Overridable at runtime via `/thinking` command. Per-session overrides persist across restarts via state store. Thinking tokens count toward mana. |
| `inject_agent_warnings` | bool | `false` | Feed WARN/ERROR log events into this agent's conversation as system warnings before each turn. Per-agent — some agents can have injection enabled while others rely on Telegram notifications. |
| `startup_notification` | bool | `true` | Send a startup notification ("botname restarted at HH:MM:SS") when the service starts. Per-agent override of global `enable_startup_notify`. Set to `false` for silent bots (e.g. cron-only agents). |
| `show_tool_calls` | string | `"off"` | Tool call display mode: `"off"` (hidden), `"preview"` (shown then overwritten by reply), `"full"` (shown and kept; reply is a separate message). Per-agent override of global `[telegram] show_tool_calls`. Accepts bool for backwards compat (`true` → `"preview"`, `false` → `"off"`). |
| `show_thinking` | string | `"off"` | Thinking block display mode: `"off"` (stripped), `"compact"` (toggle button), `"true"` (always shown). Per-agent override of global `[telegram] show_thinking`. Accepts bool (`true` → `"true"`, `false` → `"off"`). |
| `display_width` | int | `44` | Character width for divider lines in thinking display. Per-agent override of global `[telegram] display_width`. |
| `messages_in_log` | bool | nil | Per-agent override of global `[logging] messages_in_log`. Nil = use global. |
| `received_files_dir` | string | `$workspace/received_files` | Save received media (images, videos, video notes, documents) to this directory. Defaults to `$workspace/received_files`. Per-agent value overrides `[telegram] received_files_dir`. Relative paths resolve against `$HOME`. Images: `YYYY-MM-DDTHH-MM-SSZ_chat-CHATID.ext`. Videos: `YYYY-MM-DDTHH-MM-SSZ_video_chat-CHATID.ext`. Video notes: `YYYY-MM-DDTHH-MM-SSZ_videonote_chat-CHATID.mp4`. Documents: `YYYY-MM-DDTHH-MM-SSZ_document_chat-CHATID.ext`. The agent sees `[Image/Video/Document saved to: /path/to/file]` in the message text. Files over 20MB (Telegram Bot API limit) show `[Video/Document too large to download (N MB)]` instead. |
| `allowed_users` | string[] | `[]` | Per-agent allowed Telegram user IDs. If set, only these users can message this agent's bot. If empty, falls back to global `[telegram] allowed_users`. |
| `compaction_threshold` | float | nil | Per-agent compaction threshold (0.0–1.0). Nil = use global `[sessions] compaction_threshold`. |
| `compaction_summary_prompt` | string | `""` | Per-agent compaction summary prompt path. Empty = use global. |
| `compaction_handoff_msg` | string | `""` | Per-agent compaction handoff message. Empty = use global. |
| `compaction_notify` | bool | nil | Per-agent compaction notification. Nil = use global. |
| `compaction_debug` | bool | nil | Per-agent compaction debug. Nil = use global. When true, sends compaction summary as a Telegram file attachment. |
| `compaction_preserve_messages` | int | nil | Per-agent message preservation count. Nil = use global `[sessions] compaction_preserve_messages`. |
| `session_reset_prompt` | string | `""` | Per-agent reset prompt path. Empty = use global `[sessions] session_reset_prompt`. |
| `skills_dirs` | string[] | `[]` | Per-agent skill directories. Empty = use global `[skills] dirs`. |
| `prompt_rules` | array | `[]` | Per-agent prompt rules. Empty = use global `[[prompt_rules]]`. |
| `exec_auto_background` | int | `0` | Per-agent exec auto-background seconds. 0 = use global `[tools] exec_auto_background`. |
| `max_concurrent_spawns` | int | `0` | Per-agent max concurrent spawns. 0 = use global `[tools] max_concurrent_spawns`. |
| `max_upload_file_size` | int | `0` | Per-agent max file size for multipart uploads in bytes. 0 = use global `[tools] max_upload_file_size`. |
| `usage_warnings.thresholds` | int[] | `[]` | Per-agent mana warning thresholds. When set, completely replaces global `[usage_warnings] thresholds` for this agent. |
| `keepalive.enabled` | bool | (global) | Per-agent keepalive override. Inherits from global `[keepalive]` if not set. |
| `keepalive.interval` | string | (global) | Per-agent keepalive interval. |
| `keepalive.prompt` | string | (global) | Per-agent keepalive prompt file. |
| `background.enabled` | bool | (global) | Per-agent background override. Inherits from global `[background]` if not set. |
| `background.interval` | string | (global) | Per-agent background interval. |
| `background.prompt` | string | (global) | Per-agent background prompt file. |
| `background.invest_interval` | string | (global) | Per-agent invest interval. |

Default `system_files` order (most-stable first for cache efficiency):
```
["IDENTITY.md", "SOUL.md", "COHERENCE.md", "AGENTS.md", "TOOLS.md", "USER.md", "MEMORY.md", "KEEPALIVE.md"]
```

Missing files are silently skipped. The last file gets the cache breakpoint marker.

Multi-agent example:
```toml
[[agents]]
id = "main"
model = "claude-sonnet-4-5"
workspace = "/home/foci/character"
telegram_bot = "primary"
multiball_bots = ["mainling"]  # per-agent multiball pool

[[agents]]
id = "research"
model = "claude-haiku-4-5"
workspace = "/home/foci/character"
telegram_bot = "secondary"
# no multiball_bots — uses shared pool only

[telegram]
multiball_bots = ["spare1"]  # shared pool (fallback for any agent)
```

**Multiball acquisition priority:** When `/multiball` is invoked, per-agent pool is tried first. If all per-agent bots are busy (or none configured), the shared pool is used as fallback. Released bots return to whichever pool they came from.

---

## `[defaults]`

Global defaults for agent-specific fields. Agents inherit these values unless they set their own. This avoids repeating the same configuration across multiple agents.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `model` | string | `"claude-haiku-4-5"` | Default model for all agents. |
| `duplicate_messages` | bool | `false` | Default duplicate_messages setting. |
| `inject_agent_warnings` | bool | `false` | Default inject_agent_warnings setting. |
| `max_tool_loops` | int | `25` | Default max tool iterations per turn. |
| `max_output_tokens` | int | `8192` | Default max tokens in model response. |
| `effort` | string | `""` | Default effort level: `"low"`, `"medium"`, `"high"`. Empty = omit. |
| `thinking` | string | `""` | Default thinking mode: `"adaptive"` or empty/`"off"`. |
| `tts_rate` | float | `0` | Default TTS speech rate (0 = use `[voice]` config). |
| `show_tool_calls` | string | nil | Default tool call display mode. Per-agent `show_tool_calls` overrides this, then falls back to `[telegram] show_tool_calls`. |
| `show_thinking` | string | nil | Default thinking display mode. Per-agent `show_thinking` overrides this, then falls back to `[telegram] show_thinking`. |
| `display_width` | int | nil | Default display width for dividers. Per-agent `display_width` overrides this, then falls back to `[telegram] display_width`. |
| `system_files` | string[] | `[]` | Default system file list (empty = per-agent only). |

Example:
```toml
[defaults]
model = "claude-sonnet-4-5"
max_tool_loops = 50
system_files = ["IDENTITY.md", "SOUL.md", "COHERENCE.md"]
```

Resolution order: **agent field > `[defaults]` > hardcoded default**.

2. The | `tts_rate` | float | `0` | Default TTS speech rate (0 = use `[voice]` config). |
| `system_files` | string[] | `[]` | Default system file list (empty = per-agent only). |

## `[models]

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

## `[anthropic]`

Anthropic API credentials. Prefer `secrets.toml` for tokens.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `token` | string | `""` | Anthropic API key. Overridden by `secrets.toml` `[anthropic] token`. |
| `oauth_token` | string | `""` | OAuth access token for the usage API. Overridden by `secrets.toml` `[anthropic] oauth_token`. |
| `brave_api_key` | string | `""` | Brave Search API key for `web_search` tool. Overridden by `secrets.toml` `[brave] api_key`. |
| `credentials_file` | string | `"~/.claude/.credentials.json"` | Path to Claude Code OAuth credentials file. Used for OAuth token auto-refresh and usage API. |
| `auto_refresh` | bool | `nil` | Enable OAuth token auto-refresh. When `nil` (default), auto-refresh is enabled if `credentials_file` exists. Set to `false` to disable. |
| `http_timeout` | string | `"600s"` | HTTP timeout for Anthropic API calls. Go duration format. Increased to support extended thinking responses. |
| `usage_api_timeout` | string | `"10s"` | HTTP timeout for usage API calls. Go duration format. |

---

## `[telegram]`

Telegram bot configuration.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `bot_token` | string | `""` | Legacy single-bot token. Overridden by `secrets.toml` `[telegram] bot_token`. |
| `allowed_users` | string[] | `[]` | Global default: Telegram user IDs allowed to interact with bots. Per-agent `allowed_users` overrides this. |
| `enable_startup_notify` | bool | `true` | Send a startup notification when the service starts. Can be overridden per-agent with `startup_notification`. |
| `multiball_bots` | string[] | `[]` | Shared multiball pool: references keys in `[telegram.bots]` map. Fallback for any agent whose per-agent pool is exhausted (or has no per-agent pool). |
| `multiball_session_ttl` | string | `"60m"` | Idle TTL before a multiball bot can be reclaimed by a new `/multiball` call. If no messages to/from the bot within this window, it's considered abandoned and available for reuse. Set to `"0"` to disable auto-reclaim. Go duration format (`30m`, `2h`). Applies to both per-agent and shared pools. |
| `message_queue_size` | int | `64` | Outbound message queue buffer size. High-traffic bots may need larger queues. |
| `long_poll_timeout` | string | `"65s"` | Long-poll timeout for Telegram `getUpdates`. Should exceed 60s. Go duration format. |
| `show_tool_calls` | string | `"off"` | Tool call display mode: `"off"` (hidden, default), `"preview"` (shown then overwritten by reply), `"full"` (shown and kept; reply is a separate message). Per-agent `show_tool_calls` overrides this. Accepts bool for backwards compat (`true` → `"preview"`, `false` → `"off"`). |
| `show_thinking` | string | `"off"` | Thinking block display mode: `"off"` (stripped, default), `"compact"` (response with toggle button), `"true"` (thinking always prepended). Per-agent `show_thinking` overrides this. Accepts bool (`true` → `"true"`, `false` → `"off"`). |
| `display_width` | int | `44` | Character width for divider lines in thinking display. Per-agent `display_width` overrides this. |
| `received_files_dir` | string | `""` | Save received media (images, videos, video notes, documents) to this directory. Empty disables. Per-agent `received_files_dir` overrides this. Relative paths resolve against `$HOME`. See agent `received_files_dir` for filename formats. |

### `[telegram.bots.<name>]`

Named bot configuration for multi-agent setups. Each bot is referenced by name from `telegram_bot`, `multiball_bots` (per-agent), or `[telegram] multiball_bots` (shared pool).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `token_secret` | string | `""` | Key in `secrets.toml` to resolve the bot token (e.g. `"telegram.primary"`). |

Example:
```toml
[telegram.bots.primary]
token_secret = "telegram.primary"

[telegram.bots.secondary]
token_secret = "telegram.secondary"
```

With `secrets.toml`:
```toml
[telegram]
primary = "123456:ABC..."
secondary = "789012:DEF..."
```

---

## `[sessions]`

Session storage and compaction.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `dir` | string | `""` | Directory for JSONL session files. Defaults to `data/sessions/` via `data_dir`. Relative paths resolve against `$HOME`. |
| `compaction_threshold` | float | `0.8` | Trigger compaction when context usage exceeds this fraction (0.0–1.0). |
| `compaction_max_tokens` | int | `4096` | Max output tokens for the compaction summary. |
| `compaction_min_messages` | int | `4` | Minimum messages in session before compaction is allowed. |
| `compaction_summary_prompt` | string | `""` | Path to prompt file for compaction summary. Read live at compaction time (edits take effect immediately). Empty disables custom prompt (compactor uses a minimal fallback). |
| `compaction_handoff_msg` | string | see below | Message injected after the summary to orient the agent post-compaction. |
| `compaction_notify` | bool | `true` | Send a Telegram notification when compaction occurs. |
| `compaction_debug` | bool | `false` | Send the compaction summary to Telegram as a markdown file attachment after compaction completes. Useful for verifying what survived the cut. |
| `compaction_preserve_messages` | int | `25` | Preserve the last N messages through compaction. Preserved messages are appended verbatim after the summary + handoff, keeping their original roles. `0` disables (summary only). The summarizer only sees messages *before* the preserved window, so it won't duplicate them. |
| `session_reset_prompt` | string | `""` | Path to prompt file sent to the agent before session clear (`/reset` or multiball reclaim). Read at fire-time. Empty disables the reset hook. |
| `branch_orientation_prompt` | string | `""` | Global default for branch orientation prompt file. Per-agent `branch_orientation_prompt` overrides this. If empty, embedded defaults from `prompts/` are used. |
| `max_system_prompt_chars_file` | int | `20000` | Warn at startup and `/reload` if any system prompt file exceeds this many chars. `0` disables. |
| `max_system_prompt_chars_total` | int | `80000` | Warn at startup and `/reload` if total system prompt exceeds this many chars. `0` disables. |

Sessions are stored as JSONL files at `{dir}/agent/{id}/{type}.jsonl`.

All prompt fields (`compaction_summary_prompt`, `session_reset_prompt`, `branch_orientation_prompt`) are file paths, not inline strings. If the file can't be read, an error is logged and the feature is skipped. Prompt files are read live at the point of use — edits take effect immediately without restart or `/reload`.

When no config override is set, embedded defaults from `prompts/` are used:
- `prompts/branch-orientation-headless.md` — headless branches (cron, spawn, keepalive)
- `prompts/branch-orientation-multiball.md` — user-attached multiball branches
- `prompts/compaction-summary.md` — compaction summary prompt
- `prompts/compaction-handoff.md` — post-compaction handoff message (`compaction_handoff_msg` default)

---

## `[memory]`

Memory system (FTS5 search over markdown files + conversation history).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `dir` | string | `""` | Legacy: single directory containing memory markdown files. Enables `memory_search`, `remind`, and `scratchpad` tools. |
| `reindex_debounce` | string | `"0s"` | Delay before reindexing after file changes. Go duration format (`500ms`, `2s`). |
| `conversation_weight` | float | `0.1` | Weight multiplier for conversation search results (0.0–1.0). Lower = conversation appears further down in results. |
| `search_limit` | int | `20` | Maximum number of search results to return. |

When set, creates SQLite databases in the data directory (`$HOME/data/` by default): `memory.db`, `reminders.db`, `scratchpad.db`.

### `[[memory.sources]]`

Multiple memory sources with weighted relevance. When specified, `dir` is ignored.

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

### Per-agent memory (`[[agents.memory.sources]]`)

Agents can have their own memory directories in addition to the global sources. When any agent has per-agent memory configured, each agent gets its own FTS5 index (`memory-{agentID}.db`) combining global + agent-specific sources.

Agent-specific sources automatically receive a weight boost of +1.0, so they rank higher than global sources with the same base weight. Source names are prefixed with `agent:` in search results.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `name` | string | required | Source identifier (prefixed with `agent:` in results). |
| `dir` | string | required | Directory path to index. |
| `weight` | float | `1.0` | Base weight (boosted by +1.0 automatically). |

Example:
```toml
# Global memory (shared by all agents)
[[memory.sources]]
name = "shared"
dir = "/home/foci/shared/memory"
weight = 1.0

# Agent-specific memory
[[agents]]
id = "clutch"
model = "claude-sonnet-4-6"

[[agents.memory.sources]]
name = "workspace"
dir = "/home/foci/clutch/memory"
weight = 1.0    # effective weight: 2.0 (1.0 + 1.0 boost)

[[agents]]
id = "scout"
model = "claude-haiku-4-5"

[[agents.memory.sources]]
name = "workspace"
dir = "/home/foci/scout/memory"
weight = 1.0
```

When no agent has per-agent memory sources, a single shared index (`memory.db`) is used — fully backward compatible.

---

## `[http]`

HTTP API server.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `port` | int | `18791` | HTTP server port. |
| `bind` | string | `"127.0.0.1"` | Bind address. Use `0.0.0.0` for external access. |
| `graceful_shutdown_timeout` | string | `"30s"` | Time to wait for in-flight requests on shutdown. Go duration format. |

Endpoints: `POST /send`, `GET /status`, `POST /command`, `POST /wake`, `GET /voice` (WebSocket, when `[voice] ws_enabled = true`).

All endpoints accept an `agent` field (JSON body for POST, query param for GET) to target a specific agent by ID. When empty or omitted, the first configured agent is used. The `/send` endpoint also accepts an optional `session` field to target a specific session key (defaults to `main`).

### CLI (`foci` command)

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

---

## `[logging]`

Logging and diagnostics.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `level` | string | `"INFO"` | Log level: `DEBUG`, `INFO`, `WARN`, `ERROR`. |
| `event_file` | string | `"logs/foci.log"` | Path to event log file. Relative paths resolve against `$HOME`. |
| `api_file` | string | `"logs/api.jsonl"` | Path to API call log (JSONL). One entry per API call with tokens, cost, duration. Relative paths resolve against `$HOME`. |
| `conversation_file` | string | `""` | Path to conversation SQLite log. Defaults to `data/conversation.db` via `data_dir`. Relative paths resolve against `$HOME`. |
| `full_payload` | bool | `false` | Write full API request/response bodies to `payload_file`. |
| `payload_file` | string | `"logs/api-payload.jsonl"` | Path for full payload log. Only used when `full_payload = true`. Relative paths resolve against `$HOME`. |
| `messages_in_log` | bool | `false` | Log user message content to the event log. When false (default), messages are logged at DEBUG level with no content for privacy. When true, messages are logged at INFO level with content (truncated to 100 chars). |
| `cache_bust_detect` | bool | `false` | Alert via Telegram when `cache_read` drops >50% vs previous request (indicates prefix changed). |
| `cache_bust_idle_minutes` | int | `10` | Suppress cache bust alerts if the session was idle longer than this many minutes. Anthropic's cache TTL is 5 min, so any gap >10 min means the cache expired naturally — not a genuine bust. |
| `warning_max_per_window` | int | `3` | Max identical warnings allowed per time window before suppression. Set to `0` to disable rate-limiting. |
| `warning_window_duration` | string | `"5m"` | Time window for warning deduplication. Go duration format (`30s`, `5m`, `1h`). |
| `log_rotation` | bool | `true` | Enable built-in log rotation. |
| `rotation_period` | string | `"24h"` | How often to check and rotate logs. Go duration format. |
| `retention_period` | string | `"48h"` | Keep lines newer than this in the active log. Older lines archived to gzip. |
| `rotation_max_line_size` | string | `"64MB"` | Max line size for the rotation scanner buffer. Accepts `KB`, `MB`, `GB` suffixes. If a log line exceeds this size, rotation fails with "bufio.Scanner: token too long" and that log file won't be rotated. |
| `archive_dir` | string | `""` | Directory for gzip archives. Default: `logs/archive/`. |

When `inject_agent_warnings` is enabled (per-agent), repeated identical warnings (e.g. polling errors every 2 seconds) are deduplicated: after `warning_max_per_window` occurrences within `warning_window_duration`, further duplicates are suppressed and summarised as "... and N more in last Xm" on the next drain. Warning messages are normalised before comparison — IP addresses, hex strings, and multi-digit numbers are replaced with placeholders so that semantically identical errors (differing only in timestamps or addresses) are grouped together.

---

## `[voice]`

Voice support (speech-to-text and text-to-speech).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `stt_endpoint` | string | `"https://api.groq.com/openai/v1/audio/transcriptions"` | OpenAI-compatible Whisper endpoint for speech-to-text. |
| `stt_model` | string | `"whisper-large-v3"` | Whisper model name. |
| `tts_provider` | string | `""` | TTS provider: `"edge-tts"` or `"openai"`. Empty disables TTS. |
| `tts_endpoint` | string | `""` | API endpoint for OpenAI TTS provider. |
| `tts_model` | string | `""` | Model name for OpenAI TTS (e.g. `"tts-1-mini"`). |
| `tts_voice` | string | `""` | Voice name (provider-specific). Defaults to `"alloy"` for OpenAI provider. |
| `tts_rate` | float | `0` | Speech rate multiplier. `1.3` = 30% faster, `0.8` = 20% slower. `0` uses provider default. Translated automatically for each provider (edge-tts `--rate "+30%"`, openai `speed: 1.3`). |
| `ws_enabled` | bool | `false` | Enable the `/voice` WebSocket endpoint for real-time two-way voice conversation (FOCI app). Requires `voice.api_key` in `secrets.toml` and a configured STT provider. |

STT requires a Groq API key in `secrets.toml` (`[groq] api_key`). TTS with OpenAI provider requires an OpenRouter key (`[openrouter] api_key`). The `/voice` WebSocket endpoint requires an additional `voice.api_key` in `secrets.toml`.

---

## `[bitwarden]`

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

See [docs/SECRETS.md](SECRETS.md) for the full security model and URI-based host validation.

---

## `[cache]`

Prompt caching strategy.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `strategy` | string | `"auto"` | `"auto"`: top-level `cache_control` on the request body — Anthropic automatically caches the optimal prefix. `"explicit"`: manual breakpoints on last system block + second-to-last message (legacy). |

`auto` is recommended. It requires no breakpoint management and handles growing conversations automatically. `explicit` gives fine-grained control but is fragile (breakpoints can accumulate or shift if not carefully managed).

---

## `[environment]`

Environment block injected as the first system prompt block, providing the agent with runtime context (workspace, paths, messaging platform, message metadata format).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `true` | Inject environment block as the first system block. Set to `false` to disable. |
| `docs_path` | string | `""` | Path to platform docs directory. Shown in environment block when set. Relative paths resolve against `$HOME`. |

When enabled, a text block is programmatically built at startup and prepended before character files. It contains:

- **Workspace** — workspace path, agent ID, platform URL, docs path (if configured), messaging platform
- **Paths** — config file, log directory
- **Message Metadata** — documents the `[meta]` header fields (time, gap, model, prev_cost, prev_tokens, mana)
- **Session Structure** — lists character files and explains what the human can/cannot see

The block is built once per agent at startup from config values — no runtime overhead. It does not include secrets, character identity, or skill lists (those have their own blocks).

---

## `[tools]`

Tool behavior settings.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `max_result_chars` | int | `15000` | Max characters in a tool result before writing to a temp file and returning a guard message (no partial content). |
| `temp_dir` | string | `"/tmp/foci-tool-results"` | Directory for large tool result files. |
| `tmux_cols` | int | `300` | Window width (columns) applied via `resize-window` after `tmux new-session`. |
| `tmux_rows` | int | `30` | Window height (rows) applied via `resize-window` after `tmux new-session`. |
| `exec_auto_background` | int | `10` | Seconds before auto-backgrounding long-running exec and http_request calls. `0` disables. |
| `exec_default_timeout` | int | `30` | Default timeout for exec commands in seconds. |
| `exec_max_output_chars` | int | `100000` | Max characters in exec output before truncation. |
| `tmux_command_timeout` | string | `"5s"` | Timeout for tmux control commands. Go duration format. |
| `web_fetch_timeout` | string | `"30s"` | HTTP timeout for web fetch operations. Go duration format. |
| `web_fetch_max_bytes` | int | `1048576` | Max bytes to read from web fetch (1MB default). |
| `web_fetch_max_chars` | int | `50000` | Max characters in web fetch output before truncation. |
| `web_search_timeout` | string | `"15s"` | HTTP timeout for web search API calls. Go duration format. |
| `max_concurrent_spawns` | int | `3` | Max concurrent `spawn` clone_current sessions per agent. Limits how many headless self-forks can run simultaneously. |
| `max_upload_file_size` | int | `52428800` | Max file size in bytes for multipart/form-data file uploads (default 50MB). |
| `tmux_memory_check_interval` | string | `"5m"` | How often to check tmux server RSS. Go duration format. `"0"` disables monitoring. |
| `tmux_memory_warn` | string | `"10%"` | Warn threshold. Sends Telegram notification. Formats: `"N%"` (% of RAM), `"Nmb"`, `"Ngb"`. |
| `tmux_memory_critical` | string | `"20%"` | Critical threshold. Sends Telegram notification with stronger message. Same formats. |
| `tmux_memory_kill` | string | `"30%"` | Kill threshold. Kills tmux server, notifies, cleans up tool state. Same formats. |
| `tmux_autopilot` | bool | `true` | Auto-unwatch sessions after inactivity notification, auto-watch on send. |
| `tmux_watch_threshold` | string | `"30s"` | Default inactivity watch threshold. Go duration format. |

Tmux memory monitoring detects runaway memory from long-running tmux sessions (glibc malloc fragmentation). Notifications are sent to agents whose `inject_agent_warnings` is `false` — agents with injection enabled already see log warnings in their session.

---

## `[database]`

SQLite database settings.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `busy_timeout` | string | `"5s"` | SQLite busy timeout for concurrent access. Go duration format. High-load systems may need longer waits. |

---

## `[skills]`

Skill directories to scan on startup.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `dirs` | string[] | `[]` | Directories to scan for skill subdirectories containing `SKILL.md` files. |

Each subdirectory with a `SKILL.md` is loaded. The skill name and description (from YAML frontmatter) are injected into the system prompt. Skills with `command` + `script` frontmatter auto-register as slash commands.

---

## `[keepalive]`

Cache keepalive timer. Fires a lightweight branch session to keep the Anthropic cache prefix warm. Per-agent `[agents.keepalive]` overrides these globals.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Enable keepalive timer. |
| `interval` | string | `"55m"` | Time since cache last warmed before firing. Should be < 1h (Anthropic cache TTL). |
| `prompt` | string | `"prompts/keepalive.md"` | Path to keepalive prompt file. |

---

## `[background]`

Mana-gated background work timer. Fires when the user is idle, there are open background-tagged todos, and the manamometer says spending is wise. Per-agent `[agents.background]` overrides these globals.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Enable background work timer. |
| `interval` | string | `"5m"` | Time since last interaction before firing. |
| `prompt` | string | `"prompts/background.md"` | Path to background work prompt file. |
| `invest_interval` | string | `"30m"` | Quiet period after mana reset to let cache invest before spending. |

**Validation warnings:**
- `background.interval > keepalive.interval` — keepalive resets the cache timer; background work may never trigger.
- `keepalive.interval > 1h` — Anthropic cache TTL is 1 hour; cache may expire between keepalives.

See [HEARTBEAT.md](HEARTBEAT.md) for full details on the manamometer and timer logic.

---

## `[[commands]]`

Custom slash commands. Each entry is a `[[commands]]` table array.

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

---

## `[[prompt_rules]]`

Regex find/replace rules applied to inbound user messages before the agent sees them. Each rule runs in sequence — the output of one becomes the input of the next. Applied before meta prefix and before message duplication.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `find` | string | required | Go regex pattern to match. |
| `replace` | string | required | Replacement string. Supports `$1`, `$2`, etc. for capture groups. |

Example:
```toml
[[prompt_rules]]
find = '(?is)^((why|when|what|how|where|who|did|does|do|is|are|was|were|can|could|would|should)\b.*\?\s*)$'
replace = "Questions are just requests for information.\n-------\n$1"

[[prompt_rules]]
find = '(?i)^((can we|could we|should we)\b.*)'
replace = "This is a question, not an instruction.\n-------\n$1"
```

Invalid regex patterns are logged as errors and skipped.

---

## Top-level keys

Miscellaneous top-level config keys (not in any section).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `data_dir` | string | `""` | Directory for databases, sessions, and state files. When empty, defaults to `$HOME/data/`. Relative paths resolve against `$HOME`. Absolute paths used as-is. |
| `welcome_file` | string | `"data/WELCOME.md"` | Path to a changelog/welcome file. If this file exists on startup, its contents are injected into the first agent's main session and the file is deleted. Relative paths resolve against `$HOME`. |
| `skip_security_checks` | bool | `false` | Skip startup security checks for `secrets.toml` (ownership, permissions, group membership). Useful for development environments. See [docs/SECRETS.md](SECRETS.md). |

---

## Path Resolution

All path config fields are resolved at startup:

1. **Absolute paths** are used as-is
2. **Relative paths** resolve against `$HOME` (not the config directory, not CWD)
3. **`data_dir`** controls data file placement — DB, state, and session files resolve against it. When empty, defaults to `$HOME/data/`

### Default zero-config layout

With no path fields set, files auto-organize under `$HOME`:

```
$HOME/
  logs/foci.log          ← event log
  logs/api.jsonl         ← API call log
  logs/api-payload.jsonl ← full payload log (if enabled)
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

Credentials file. Lives alongside `foci.toml`. Protected at the OS level by the `foci-secrets` group — see [docs/SECRETS.md](SECRETS.md) for the full security model and setup instructions.

```toml
[anthropic]
token = "sk-ant-..."
oauth_token = "sk-ant-oat01-..."

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

Each section can include an `allowed_hosts` array restricting which hosts that section's secrets can be sent to via the `http_request` tool. Secrets without `allowed_hosts` can only be used in exec commands (deprecated).

```toml
[myapi]
token = "sk-..."
allowed_hosts = ["api.example.com", "api.backup.example.com"]
```

Host matching is case-insensitive (per RFC 4343). Ports are ignored — `api.example.com:8443` matches `api.example.com`. See [docs/SECRETS.md](SECRETS.md) for the full security model.

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

Per-agent scoping applies to: exec `{{secret:NAME}}` templates, `http_request` secret resolution, output redaction, and system prompt secret names. Built-in credential resolution (anthropic.token, telegram bot tokens, brave API key) remains global — these are process-wide settings.

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
token = "sk-ant-..."

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

[telegram.bots.primary]
token_secret = "telegram.primary"

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
