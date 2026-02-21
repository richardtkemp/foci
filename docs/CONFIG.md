# Clod Configuration Reference

Clod uses two TOML files: `clod.toml` (main config) and `secrets.toml` (credentials). Pass the config path with `-config`:

```
clodgw -config /home/clod/clod.toml
```

Secrets are loaded from `secrets.toml` in the same directory as the config file. Values in `secrets.toml` override matching fields in `clod.toml`.

---

## `[agent]`

Core agent settings.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `id` | string | `""` | Agent identifier. Used in session keys (`agent:ID:main`). |
| `model` | string | `"claude-haiku-4-5"` | Anthropic model ID for API calls. |
| `workspace` | string | `""` | Path to workspace directory containing character files (IDENTITY.md, SOUL.md, etc.). |
| `heartbeat_interval` | string | `"45m"` | Duration between idle heartbeats. Go duration format (`30s`, `5m`, `2h`). |
| `system_files` | string[] | see below | Ordered list of workspace files to load as system prompt blocks. |
| `duplicate_messages` | bool | `false` | Send user text twice per API call. Can improve instruction following. |

Default `system_files` order (most-stable first for cache efficiency):
```
["IDENTITY.md", "SOUL.md", "COHERENCE.md", "AGENTS.md", "TOOLS.md", "USER.md", "MEMORY.md", "HEARTBEAT.md"]
```

Missing files are silently skipped. The last file gets the cache breakpoint marker.

---

## `[anthropic]`

Anthropic API credentials. Prefer `secrets.toml` for tokens.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `token` | string | `""` | Anthropic API key. Overridden by `secrets.toml` `[anthropic] token`. |
| `brave_api_key` | string | `""` | Brave Search API key for `web_search` tool. Overridden by `secrets.toml` `[brave] api_key`. |

---

## `[telegram]`

Telegram bot configuration.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `bot_token` | string | `""` | Telegram bot token. Overridden by `secrets.toml` `[telegram] bot_token`. |
| `allowed_users` | string[] | `[]` | Telegram user IDs allowed to interact with the bot. |
| `secondary_bots` | string[] | `[]` | Tokens for secondary bots (multiball feature). |

---

## `[sessions]`

Session storage and compaction.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `dir` | string | `""` | Directory for JSONL session files. |
| `compaction_threshold` | float | `0.8` | Trigger compaction when context usage exceeds this fraction (0.0–1.0). |

Sessions are stored as JSONL files at `{dir}/agent/{id}/{type}.jsonl`.

---

## `[memory]`

Memory system (FTS5 search over markdown files + conversation history).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `dir` | string | `""` | Directory containing memory markdown files. Also enables `memory_search`, `memory_remind`, and scratchpad tools. |

When set, creates SQLite databases alongside the config file: `memory.db`, `reminders.db`, `scratchpad.db`.

---

## `[http]`

HTTP API server.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `port` | int | `18791` | HTTP server port. |
| `bind` | string | `"127.0.0.1"` | Bind address. Use `0.0.0.0` for external access. |

Endpoints: `POST /send`, `GET /status`, `POST /command`, `POST /wake`.

---

## `[logging]`

Logging and diagnostics.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `level` | string | `"INFO"` | Log level: `DEBUG`, `INFO`, `WARN`, `ERROR`. |
| `event_file` | string | `"clod.log"` | Path to event log file. |
| `api_file` | string | `"api.jsonl"` | Path to API call log (JSONL). One entry per API call with tokens, cost, duration. |
| `conversation_file` | string | `"conversation.db"` | Path to conversation SQLite log. |
| `full_payload` | bool | `false` | Log full API request/response bodies. |
| `payload_file` | string | `"api-payload.jsonl"` | Path for full payload log. Only used when `full_payload = true`. |
| `cache_bust_threshold` | int | `0` | Alert when `cache_write` tokens exceed this value on consecutive requests. `0` disables. |

---

## `[voice]`

Voice support (speech-to-text and text-to-speech).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `stt_endpoint` | string | Groq | OpenAI-compatible Whisper endpoint for speech-to-text. |
| `stt_model` | string | `"whisper-large-v3"` | Whisper model name. |
| `tts_provider` | string | `""` | TTS provider: `"edge-tts"` or `"openai"`. Empty disables TTS. |
| `tts_endpoint` | string | `""` | API endpoint for OpenAI TTS provider. |
| `tts_model` | string | `""` | Model name for OpenAI TTS (e.g. `"tts-1-mini"`). |
| `tts_voice` | string | `"alloy"` | Voice name (provider-specific). |

STT requires a Groq API key in `secrets.toml` (`[groq] api_key`). TTS with OpenAI provider requires an OpenRouter key (`[openrouter] api_key`).

---

## `[cache]`

Prompt caching strategy.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `strategy` | string | `"auto"` | `"auto"`: top-level `cache_control` on the request body — Anthropic automatically caches the optimal prefix. `"explicit"`: manual breakpoints on last system block + second-to-last message (legacy). |

`auto` is recommended. It requires no breakpoint management and handles growing conversations automatically. `explicit` gives fine-grained control but is fragile (breakpoints can accumulate or shift if not carefully managed).

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

## `secrets.toml`

Credentials file. Lives alongside `clod.toml`. Should have restricted permissions (`chmod 600`).

```toml
[anthropic]
token = "sk-ant-..."

[telegram]
bot_token = "123456:ABC..."

[brave]
api_key = "BSA..."

[groq]
api_key = "gsk_..."

[openrouter]
api_key = "sk-or-..."
```

All secrets override their corresponding `clod.toml` values. Secondary bot tokens can also be provided as a comma-separated string under `[telegram] secondary_bots`.

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

[sessions]
dir = "/home/clod/sessions"
compaction_threshold = 0.8

[memory]
dir = "/home/clod/character/memory"

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
cache_bust_threshold = 20000

[voice]
stt_endpoint = "https://api.groq.com/openai/v1"
stt_model = "whisper-large-v3"
tts_provider = "openai"
tts_endpoint = "https://openrouter.ai/api/v1"
tts_model = "openai/tts-1-mini"
tts_voice = "alloy"

[skills]
dirs = ["/home/clod/skills"]

[[commands]]
name = "reheat"
description = "Clear API cooldowns"
script = "/home/clod/scripts/reheat.sh"
timeout = 10
```
