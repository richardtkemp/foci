# Clod Configuration Reference

Clod uses two TOML files: `clod.toml` (main config) and `secrets.toml` (credentials). Pass the config path with `-config`:

```
clodgw -config /home/clod/clod.toml
```

Secrets are loaded from `secrets.toml` in the same directory as the config file. Values in `secrets.toml` override matching fields in `clod.toml`.

---

## `[agent]` / `[[agents]]`

Core agent settings. Use `[agent]` for a single agent (legacy) or `[[agents]]` for multiple agents. When both are present, `[[agents]]` takes precedence.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `id` | string | `""` | Agent identifier. Used in session keys (`agent:ID:main`). |
| `model` | string | `"claude-haiku-4-5"` | Anthropic model ID for API calls. |
| `workspace` | string | `""` | Path to workspace directory containing character files (IDENTITY.md, SOUL.md, etc.). |
| `heartbeat_interval` | string | `"45m"` | Duration between idle heartbeats. Go duration format (`30s`, `5m`, `2h`). |
| `system_files` | string[] | see below | Ordered list of workspace files to load as system prompt blocks. |
| `duplicate_messages` | bool | `false` | Send user text twice per API call. Can improve instruction following. |
| `fork_prompt` | string | `""` | Prompt injected into multiball branch sessions to inform the agent of the fork. Empty disables. |
| `telegram_bot` | string | `""` | References a key in `[telegram.bots]` map. Assigns this bot to the agent. |
| `multiball_bot` | string | `""` | References a key in `[telegram.bots]` map. Used for multiball (branch) sessions. |
| `memory.sources` | array | `[]` | Per-agent memory directories (see below). Combined with global `[memory]` sources. |
| `max_tool_loops` | int | `25` | Maximum tool iterations per agent turn. Complex tasks may need more. |
| `max_output_tokens` | int | `8192` | Maximum tokens in model response. Larger values allow longer responses. |

Default `system_files` order (most-stable first for cache efficiency):
```
["IDENTITY.md", "SOUL.md", "COHERENCE.md", "AGENTS.md", "TOOLS.md", "USER.md", "MEMORY.md", "HEARTBEAT.md"]
```

Missing files are silently skipped. The last file gets the cache breakpoint marker.

Multi-agent example:
```toml
[[agents]]
id = "main"
model = "claude-sonnet-4-5"
workspace = "/home/clod/character"
telegram_bot = "primary"

[[agents]]
id = "research"
model = "claude-haiku-4-5"
workspace = "/home/clod/character"
telegram_bot = "secondary"
```

---

## `[anthropic]`

Anthropic API credentials. Prefer `secrets.toml` for tokens.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `token` | string | `""` | Anthropic API key. Overridden by `secrets.toml` `[anthropic] token`. |
| `oauth_token` | string | `""` | OAuth access token for the usage API. Overridden by `secrets.toml` `[anthropic] oauth_token`. |
| `brave_api_key` | string | `""` | Brave Search API key for `web_search` tool. Overridden by `secrets.toml` `[brave] api_key`. |
| `http_timeout` | string | `"120s"` | HTTP timeout for Anthropic API calls. Go duration format. |
| `usage_api_timeout` | string | `"10s"` | HTTP timeout for usage API calls. Go duration format. |

---

## `[telegram]`

Telegram bot configuration.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `bot_token` | string | `""` | Legacy single-bot token. Overridden by `secrets.toml` `[telegram] bot_token`. |
| `allowed_users` | string[] | `[]` | Telegram user IDs allowed to interact with the bot. |
| `secondary_bots` | string[] | `[]` | Legacy: tokens for secondary bots (multiball feature). |
| `multiball_session_ttl` | string | `"60m"` | Idle TTL before a multiball bot can be reclaimed by a new `/multiball` call. If no messages to/from the bot within this window, it's considered abandoned and available for reuse. Set to `"0"` to disable auto-reclaim. Go duration format (`30m`, `2h`). |
| `message_queue_size` | int | `64` | Outbound message queue buffer size. High-traffic bots may need larger queues. |
| `long_poll_timeout` | string | `"65s"` | Long-poll timeout for Telegram `getUpdates`. Should exceed 60s. Go duration format. |

### `[telegram.bots.<name>]`

Named bot configuration for multi-agent setups. Each bot is referenced by name from `[agent] telegram_bot` or `multiball_bot`.

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
| `dir` | string | `""` | Directory for JSONL session files. |
| `compaction_threshold` | float | `0.8` | Trigger compaction when context usage exceeds this fraction (0.0–1.0). |
| `compaction_model` | string | agent model | Model to use for summarization. Defaults to the agent's own model. |
| `compaction_max_tokens` | int | `4096` | Max output tokens for the compaction summary. |
| `compaction_min_messages` | int | `4` | Minimum messages in session before compaction is allowed. |
| `compaction_summary_prompt` | string | see below | Prompt sent to the model to generate the summary. |
| `compaction_handoff_msg` | string | see below | Message injected after the summary to orient the agent post-compaction. |

Sessions are stored as JSONL files at `{dir}/agent/{id}/{type}.jsonl`.

Default `compaction_summary_prompt`:
```
Please provide a concise summary of our entire conversation so far, capturing all key decisions, context, and important details. This summary will replace the conversation history.
```

Default `compaction_handoff_msg`:
```
[Compaction complete. The conversation continues from here. You have full access to your tools and memory.]
```

---

## `[memory]`

Memory system (FTS5 search over markdown files + conversation history).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `dir` | string | `""` | Legacy: single directory containing memory markdown files. Enables `memory_search`, `memory_remind`, and scratchpad tools. |
| `reindex_debounce` | string | `"0s"` | Delay before reindexing after file changes. Go duration format (`500ms`, `2s`). |
| `conversation_weight` | float | `0.1` | Weight multiplier for conversation search results (0.0–1.0). Lower = conversation appears further down in results. |
| `search_limit` | int | `20` | Maximum number of search results to return. |

When set, creates SQLite databases alongside the config file: `memory.db`, `reminders.db`, `scratchpad.db`.

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
dir = "/home/clod/character/memory"
weight = 1.0

[[memory.sources]]
name = "docs"
dir = "/home/clod/project/docs"
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
dir = "/home/clod/shared/memory"
weight = 1.0

# Agent-specific memory
[[agents]]
id = "clutch"
model = "claude-sonnet-4-6"

[[agents.memory.sources]]
name = "workspace"
dir = "/home/clod/clutch/memory"
weight = 1.0    # effective weight: 2.0 (1.0 + 1.0 boost)

[[agents]]
id = "scout"
model = "claude-haiku-4-5"

[[agents.memory.sources]]
name = "workspace"
dir = "/home/clod/scout/memory"
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
| `graceful_shutdown_timeout` | string | `"5s"` | Time to wait for in-flight requests on shutdown. Go duration format. |

Endpoints: `POST /send`, `GET /status`, `POST /command`, `POST /wake`.

All endpoints accept an `agent` field (JSON body for POST, query param for GET) to target a specific agent by ID. When empty or omitted, the first configured agent is used. The `/send` endpoint also accepts an optional `session` field to target a specific session key (defaults to `main`).

### CLI (`clod` command)

The `clod` CLI wraps the HTTP API. All subcommands accept `-a <id>` / `--agent <id>` to target a specific agent. The `send` command also accepts `-s <session>` / `--session <id>` to target a specific session:

```
clod send -a research "check the news"
clod send -a clutch -s research "text"  # routes to agent:clutch:research
clod branch -a research
clod status --agent=research
clod ping -a research
clod eval -a research "df -h"
clod command -a research /cache
```

When omitted, the first agent and main session are used (backward compatible).

---

## `[logging]`

Logging and diagnostics.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `level` | string | `"INFO"` | Log level: `DEBUG`, `INFO`, `WARN`, `ERROR`. |
| `event_file` | string | `"clod.log"` | Path to event log file. |
| `api_file` | string | `"api.jsonl"` | Path to API call log (JSONL). One entry per API call with tokens, cost, duration. |
| `conversation_file` | string | `"conversation.db"` | Path to conversation SQLite log. |
| `full_payload` | bool | `false` | Write full API request/response bodies to `payload_file`. |
| `payload_file` | string | `"api-payload.jsonl"` | Path for full payload log. Only used when `full_payload = true`. |
| `cache_bust_detect` | bool | `false` | Alert via Telegram when `cache_read` drops >50% vs previous request (indicates prefix changed). |
| `inject_agent_warnings` | bool | `false` | Feed WARN/ERROR log events into agent conversation as system warnings before each turn. |
| `warning_max_per_window` | int | `3` | Max identical warnings allowed per time window before suppression. Set to `0` to disable rate-limiting. |
| `warning_window_duration` | string | `"5m"` | Time window for warning deduplication. Go duration format (`30s`, `5m`, `1h`). |

When `inject_agent_warnings` is enabled, repeated identical warnings (e.g. polling errors every 2 seconds) are deduplicated: after `warning_max_per_window` occurrences within `warning_window_duration`, further duplicates are suppressed and summarised as "... and N more in last Xm" on the next drain. Warning messages are normalised before comparison — IP addresses, hex strings, and multi-digit numbers are replaced with placeholders so that semantically identical errors (differing only in timestamps or addresses) are grouped together.

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

STT requires a Groq API key in `secrets.toml` (`[groq] api_key`). TTS with OpenAI provider requires an OpenRouter key (`[openrouter] api_key`).

---

## `[cache]`

Prompt caching strategy.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `strategy` | string | `"auto"` | `"auto"`: top-level `cache_control` on the request body — Anthropic automatically caches the optimal prefix. `"explicit"`: manual breakpoints on last system block + second-to-last message (legacy). |

`auto` is recommended. It requires no breakpoint management and handles growing conversations automatically. `explicit` gives fine-grained control but is fragile (breakpoints can accumulate or shift if not carefully managed).

---

## `[tools]`

Tool behavior settings.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `max_result_chars` | int | `10000` | Max characters in a tool result before writing to a temp file and returning a truncated preview. |
| `temp_dir` | string | `"/tmp/clod-tool-results"` | Directory for large tool result files. |
| `tmux_cols` | int | `300` | Window width (columns) applied via `resize-window` after `tmux new-session`. |
| `tmux_rows` | int | `30` | Window height (rows) applied via `resize-window` after `tmux new-session`. |
| `exec_auto_background` | int | `10` | Seconds before auto-backgrounding long-running exec commands. `0` disables. |
| `exec_default_timeout` | int | `30` | Default timeout for exec commands in seconds. |
| `exec_max_output_chars` | int | `100000` | Max characters in exec output before truncation. |
| `tmux_command_timeout` | string | `"5s"` | Timeout for tmux control commands. Go duration format. |
| `web_fetch_timeout` | string | `"30s"` | HTTP timeout for web fetch operations. Go duration format. |
| `web_fetch_max_bytes` | int | `1048576` | Max bytes to read from web fetch (1MB default). |
| `web_fetch_max_chars` | int | `50000` | Max characters in web fetch output before truncation. |
| `web_search_timeout` | string | `"15s"` | HTTP timeout for web search API calls. Go duration format. |

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
script = "/home/clod/scripts/deploy.sh"
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
| `welcome_file` | string | `"WELCOME.md"` | Path to a changelog/welcome file. If this file exists on startup, its contents are injected into the first agent's main session and the file is deleted. Written by `setup.sh` on update (not fresh install). |

---

## `secrets.toml`

Credentials file. Lives alongside `clod.toml`. Should have restricted permissions (`chmod 600`).

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
```

All secrets override their corresponding `clod.toml` values.

---

## Minimal Example

```toml
[agent]
id = "main"
model = "claude-haiku-4-5"
workspace = "/home/clod/character"

[telegram]
allowed_users = ["123456789"]

[sessions]
dir = "/home/clod/sessions"

[memory]
dir = "/home/clod/character/memory"

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
workspace = "/home/clod/character"
heartbeat_interval = "30m"
system_files = ["IDENTITY.md", "SOUL.md", "AGENTS.md", "TOOLS.md", "USER.md", "MEMORY.md", "HEARTBEAT.md"]

[telegram]
allowed_users = ["123456789"]

[telegram.bots.primary]
token_secret = "telegram.primary"

[sessions]
dir = "/home/clod/sessions"
compaction_threshold = 0.8

[memory]
dir = "/home/clod/character/memory"
reindex_debounce = "500ms"

[http]
port = 18791
bind = "127.0.0.1"

[logging]
level = "INFO"
event_file = "/home/clod/clod.log"
api_file = "/home/clod/api.jsonl"
conversation_file = "/home/clod/conversation.db"
full_payload = true
payload_file = "/home/clod/api-payload.jsonl"
cache_bust_detect = true

[voice]
stt_endpoint = "https://api.groq.com/openai/v1/audio/transcriptions"
stt_model = "whisper-large-v3"
tts_provider = "openai"
tts_endpoint = "https://openrouter.ai/api/v1"
tts_model = "openai/tts-1-mini"
tts_voice = "alloy"

[tools]
tmux_cols = 300
tmux_rows = 30

[skills]
dirs = ["/home/clod/skills"]

[[commands]]
name = "reheat"
description = "Clear API cooldowns"
script = "/home/clod/scripts/reheat.sh"
timeout = 10
```
