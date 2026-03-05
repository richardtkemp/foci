# Foci — Specification

A minimal, maintainable agent platform in Go. One binary, no framework, no bloat.

## Philosophy

- **Simple over powerful.** If a feature needs complex config, rethink the feature.
- **Explicit over clever.** No plugin architectures, no hook systems, no middleware chains.
- **Own every line.** No 5.4GB node_modules. Dependencies are standard library + a few well-chosen packages.
- **Cache-aware from day zero.** Anthropic prompt caching drives architectural decisions.

## Sessions & State

### Session Keys

Format: `agent:AGENTID:TYPE[:BRANCHID]`

- `agent:fotini:chat:123456789` — per-chat DM session (keyed by Telegram chat ID)
- `agent:main:cron:morning-routine` — cron-triggered branch
- `agent:main:subagent:research-task` — sub-agent branch
- `agent:main:multiball:mb-1709123456` — multiball fork
- `agent:clutch:voice:1709123456` — WebSocket voice session (ephemeral, one per connection)

Each Telegram DM gets its own session, keyed by chat ID. One session is designated as the "default" — this is what cron (`foci send`/`foci branch`) and proactive features target. If no default is set, the first chat session created becomes the default. The default can be changed via `/sessions default <chat_id>`.

The 4th segment (branch ID) is optional. Branch sessions inherit the parent's message prefix for cache sharing.

### Session Branching (Cache Sharing)

A branch session copies the parent's system prompt + message history at a point in time. The shared prefix hits the cache instead of being re-tokenized. See [docs/CACHING.md](docs/CACHING.md) for pricing details and cache requirements.

**Rules:**
- Parent session: append-only, owns canonical history
- Branch session: snapshot of parent messages at branch point + own appended messages
- Branch never writes back to parent history
- Branch result delivered as a message to the parent session or via Telegram

**Storage:** A branch record holds:
```
parent_key: "agent:main:main"
branch_point: <message index>
messages: [only messages after branch point]
```

API payload assembly: system prompt + parent.messages[:branch_point] + branch.messages

**Branch orientation:** All branches (multiball, cron/wake, spawn) receive an orientation message as their first user message. The orientation tells the branch its type, keys, and communication rules:
- **Headless** (cron, spawn, keepalive — `direct_chat=false`): must NEVER use `send_telegram`; reports significant work or errors to parent via `send_to_session`; stays silent when nothing happened.
- **Multiball** (`direct_chat=true`): has its own Telegram bot for direct user replies; keeps the main session informed of visible work via `send_to_session`; sends a completion summary before going idle.

Default orientation text is embedded in `prompts/branch-orientation-headless.md` and `prompts/branch-orientation-multiball.md`. Config override via `branch_orientation_prompt` (per-agent or global) takes precedence. Template variables `{branch_key}`, `{parent_key}`, `{branch_type}`, `{direct_chat}` are replaced at branch creation time.

### Compaction

**Alpha:** Threshold-based with fully configurable parameters.
- When context exceeds N% of model's context window, trigger compaction
- Pre-compaction: inject system message "save important context to memory now", let agent write to memory files
- Pre-session-end: same memory prompt fires before a session goes inactive — e.g. when the user runs `/new`, or after N minutes of inactivity. The agent gets a chance to persist anything important before the session is replaced or archived.
- Compaction: call model with configurable summary prompt, replace history with summary
- Post-compaction: inject handoff note so agent knows compaction occurred
- Scratchpad preserved through compaction (appended to handoff)
- Last N messages preserved verbatim after the summary (configurable, default 25) — gives the agent access to the actual recent conversation, not just a summary of it
- Branch sessions preserve `branch_meta` through compaction (branch_point set to 0 since compacted messages are self-contained)
- Session file rotation: on compaction, the pre-compaction file is renamed to a timestamp-based archive (e.g. `5970082313.2026-03-04T02-30-00Z.jsonl`) before writing the new compacted session. If multiple compactions occur within the same second, a counter is added (e.g. `.2026-03-04T02-30-00Z.2.jsonl`). Archives are preserved for usage tracking and audit — nothing reads them during normal operation.
- Async-pending guard: compaction is deferred while a session has pending async tool results (spawn clone, auto-backgrounded shell/http). This prevents compacting away the context that the async result relates to. Compaction fires naturally on a later turn once all results have been delivered.

All compaction parameters are configurable per-agent or globally: threshold, max tokens, min messages, summary/handoff/reset prompts, debug mode, and preserve-messages count. Prompt files are read live — edits take effect without restart. See [CONFIG.md](CONFIG.md).

### Session Metadata Index

A SQLite index (`session_index.db`) tracks all session files with metadata: session key, file path, created timestamp, parent session key (for branches), session type (chat/multiball/spawn/cron/branch), and status (active/compacted/cleared). Rebuilt from disk on startup by scanning all non-archive `.jsonl` files. Updated in real-time via lifecycle hooks on the session store (create, compact, clear). Queryable via `/sessions index [type] [status]`.

## Communication

### Anthropic API

- **Auth:** Setup token (from `claude setup-token`), API key, or Claude Code credentials fallback. See [docs/AUTH.md](docs/AUTH.md).
- **Model:** Haiku (`claude-haiku-4-5`) for foci itself; configurable per agent
- **Prompt caching:** Two cache breakpoints per API request (system prompt + conversation history). See [docs/CACHING.md](docs/CACHING.md).
- **Streaming:** Server-sent events for responses

### Telegram Bot

- Long-polling (not webhooks) for simplicity
- Receive: text messages, voice notes, file attachments (beta)
- Send: text messages, markdown formatting, voice notes, file attachments (beta)
- Route incoming messages to the correct agent session
- DM only for alpha; group chat support in beta
- Startup notification: sends "botname restarted at HH:MM:SS" to the last active chat. Controlled by global `enable_startup_notify` (default true) with per-agent override via `startup_notification`. Set to `false` for silent bots (e.g., cron-only agents).
- Crash/reboot detection: on startup, classifies restart as clean/crash/reboot by comparing last shutdown timestamp with system uptime. Unexpected restarts include diagnostic findings (ERROR/FATAL lines from logs) in the notification. Clean shutdown timestamp recorded on graceful exit via signal handler.

### Multi-Bot Sessions (/multiball)

`/multiball` forks a session to a secondary Telegram bot — same agent, same context snapshot, parallel thread. Bots can be per-agent or shared pool. See [docs/MULTIBALL.md](docs/MULTIBALL.md) for bot pool config, session lifecycle, routing, and use cases.

Per-agent multiball pools are configured in the agent's `multiball_bots` list. A shared fallback pool is configured globally under `[telegram]`. See [CONFIG.md](CONFIG.md).

**Acquisition priority:** per-agent pool first, shared pool as fallback. Released bots return to whichever pool they came from.

**Restart survival:** The `bot → session_key` mapping is persisted in the state store. On restart, mappings are restored if the session file still exists on disk. Stale mappings are cleaned up automatically.

### Voice (Telegram Voice Notes)

**Inbound:** Receive Telegram voice notes → transcribe via STT provider (OpenAI-compatible, e.g. Groq Whisper) → inject transcript as the user message with a `[voice]` tag. The agent sees text, doesn't need to handle audio.

**Outbound:** Agent can send voice replies via `send_telegram(text="...", send_as="voice")`. Text → TTS engine (Edge TTS, OpenAI, or similar) → send as Telegram voice note. Good for when the human is mobile/driving.

### WebSocket Voice Endpoint (`/voice`)

A WebSocket endpoint for real-time two-way voice conversation with an agent. Used by the FOCI Android app.

**Connection:** `GET /voice?api_key=KEY` → auth middleware → upgrade to WebSocket. Server sends a `connected` message with the available agent list. Client sends `select_agent` to pick an agent, server responds with `session_ready` and an ephemeral session key (`agent:ID:voice:CONN_ID`).

**Audio flow:** Client sends `audio_start` → binary Opus frames → `audio_end`. Server transcribes via STT, sends `transcription`, processes with agent, sends `response_start` → `response_text` (final=true) → `audio_start` + binary MP3 chunks (4KB) + `audio_end` → `response_end`. Text input (`text` message) skips STT, same pipeline from agent call onward.

**Concurrency:** Three mutexes per connection — `writeMu` (all WebSocket writes), `turnMu` (serializes agent turns), `audioMu` (recording state). TTS failures are non-fatal (text response still delivered).

**Auth:** Uses `http.api_key` (same as all other endpoints). Enabled when `[http] ws_enabled = true` AND an `[[stt]]` entry is configured.

### Media Persistence

When `received_files_dir` is configured (global or per-agent), received media files are saved to disk:
- **Images:** `YYYY-MM-DDTHH-MM-SSZ_chat-CHATID.ext` (jpg, png, gif, webp)
- **Videos:** `YYYY-MM-DDTHH-MM-SSZ_video_chat-CHATID.ext` (mp4, mov, webm, mkv, avi)
- **Video notes:** `YYYY-MM-DDTHH-MM-SSZ_videonote_chat-CHATID.mp4` (circular video messages)
- **Documents:** `YYYY-MM-DDTHH-MM-SSZ_document_chat-CHATID.ext` (pdf, xlsx, docx, etc.)

The saved path is injected into the user message text as `[Image saved to: /path/to/file]`, `[Video saved to: /path/to/file]`, or `[Document saved to: /path/to/file]` so the agent can reference, copy, or process the file.

**Size limits:** Telegram Bot API has a 20MB download limit. Files exceeding this show `[Video too large to download (N MB)]` or `[Document too large to download (N MB)]` instead of a path.

Saving is non-fatal — errors are logged as warnings. Images are still sent to the API for vision processing; videos and documents are saved only (no API processing).

### Message Metadata

Each user message injected into the conversation carries metadata the agent can see. This is NOT in the system prompt (that would bust cache) — it's prepended to the user message content.

```
[meta] time=2026-02-21T05:30:00Z gap=3h12m model=claude-haiku-4-5 prev_cost=$0.043 prev_tokens=in:2400/out:312/cR:18000/cW:200
```

Fields:
- `time` — current UTC timestamp
- `gap` — time since the previous message in this session (human-readable: "3h12m", "2d4h", "38s")
- `model` — current model name (so the agent knows its own capabilities)
- `prev_cost` — total cost of the previous agent turn (API call that generated the last response)
- `prev_tokens` — token breakdown of the previous turn (input/output/cache_read/cache_write)

**Why metadata on messages, not system prompt:** Dynamic values in the system prompt would bust the cache every turn. See [docs/CACHING.md](docs/CACHING.md).

**Why previous turn's cost, not current:** The current turn's cost isn't known until after the API responds. So each message carries the cost of the turn that came before it. The agent always knows what its last response cost.

### Deferred Replies

The agent can acknowledge a message and deliver a full response later. For complex questions requiring research or long tool chains:

1. Agent sends an immediate short reply ("Looking into this, give me a minute")
2. Agent continues working (tool calls, research, etc.)
3. Agent sends the full response when ready

Implementation: The agent turn can produce multiple Telegram messages. The first is sent immediately. Subsequent messages are sent as the agent completes tool calls. This is just streaming tool results to Telegram rather than batching everything into one final response.

Controlled by `batch_partial_assistant_messages` (bool, default `false`):
- **false:** Text in mid-turn responses is sent to Telegram immediately via `ReplyFunc`. The user sees text as it's generated, even if more tool calls follow.
- **true:** Text is accumulated across all responses in the turn chain and sent concatenated when the turn completes (end_turn with no more tool calls).

Both system-triggered turns (async_notify) and Telegram-triggered turns support deferred replies. The async_notify path resolves the Telegram bot early and attaches a `ReplyFunc` callback so intermediate text is delivered during the turn.

## Agent Behaviour

### Scratchpad

Working notes that survive compaction but aren't permanent memory. For when the agent is mid-investigation and building up context that would be catastrophic to lose but isn't worth saving to memory files.

Single `scratchpad` tool with `action` parameter (write/read/clear/list), `key`, and `content`.

Stored in SQLite, scoped per-agent via `agent_id` column. On compaction, scratchpad contents are injected back into the post-compaction context as a system message. The agent is responsible for clearing it when done — it's working state, not knowledge.

### Spawn (Model Escalation + Self-Fork)

The `spawn` tool is a unified sub-call mechanism with three context modes:

```
spawn(prompt="Evaluate this architecture", model="opus", context="raw")
spawn(prompt="Research this topic thoroughly", context="clone")
```

**Context modes:**

- **`raw`** — just the prompt, no system context. One-shot cold call with tool access. Tools run in an isolated temp directory (`/tmp/foci-spawn-*`). File writes are sandboxed: absolute paths and `../` traversal are blocked. Any created files are listed in the spawn result with sizes. Good for tasks that need file output without workspace access.
- **`character`** — character files + prompt. One-shot call with full personality context. Good for tasks that need "you".
- **`clone`** (default) — creates a branch session with full tool access. A headless self-fork: the spawned session inherits the parent's context, tools, and model. Always runs asynchronously — returns an immediate acknowledgment and delivers the result via `AsyncNotifier` when complete.
- **`explore`** — safe exploration agent with `ls`, `find`, `grep`, `read`, `memory_search`, `web_search`, `web_fetch`. One-shot, no file mutation, no shell exec, no messaging. Always runs on haiku. Exploration tools are created fresh (not in the main registry) and use direct `exec.CommandContext` (no shell). Good for codebase research tasks where the parent wants to delegate exploration without risk.

**Clone mode details:**
- Creates a branch session: `agent:AGENTID:spawn:spawn-TIMESTAMP`
- Branch has `NoResetHook` set (ephemeral, no memory formation on cleanup)
- Recursive clone spawns are blocked — a spawned session can use `raw`/`character` but not `clone`
- Concurrent clone spawns are limited by `max_concurrent_spawns` (default 3)
- Runs as a full agent turn with all tools available
- Always async: returns `"Spawn started in background."` immediately, delivers `[SPAWN RESULT]` via notifier on completion (matching the `[EXEC RESULT]`/`[HTTP RESULT]` pattern)

**Model resolution:** Short names (`opus`, `sonnet`, `haiku`) resolve to full model IDs. Empty model defaults to the parent's model. Model is ignored for clone mode (inherits parent model).

### Thought Queue

The agent can defer thoughts for later via the `remind` tool:

```
remind(text="Look into whether FTS5 supports phrase boosting", when="2h")
remind(text="Ask Dick about the Greece decision", when="tomorrow")
remind(text="Check deploy status", when="30m", wake=true)
```

By default (wake=false), reminders surface as injected context at the specified time. With wake=true, the session is actively woken — a message is fired to the agent at the specified time. Stored in SQLite, scoped per-agent via `agent_id` column. Lightweight — not a full task system, just "future me should think about this."

### Effort Parameter

Controls how much work Claude does per turn. Lower effort = shorter responses, fewer tool calls, less thinking. Configurable globally and per-agent. Overridable at runtime via `/effort`.

### Adaptive Thinking

Enables extended thinking (Opus 4.6). In adaptive mode, the model decides when and how much to think. Thinking blocks are interleaved between tool calls. Thinking content is preserved in session history. Thinking tokens count toward mana — opt-in per agent.

Configurable globally and per-agent. Runtime toggle: `/thinking adaptive` or `/thinking off`.

#### Showing thinking in Telegram

By default, thinking blocks are stripped from Telegram messages. The `show_thinking` config controls visibility:

- `"off"` (default) — thinking stripped, not shown to user
- `"compact"` — response sent with a "Show thinking" toggle button
- `"true"` — thinking always prepended to response (italic), separated by a divider

The `display_width` config (default 32) controls the character width of divider lines used in thinking display.

Configurable globally and per-agent via `show_thinking`. The `display_width` config controls divider line width.

Valid levels: `"low"`, `"medium"`, `"high"`. Empty = omit from request (API default). The `/effort` command shows or changes the level for the current session (runtime only, not persisted to config).

## Tools

### Tool System

Tools are Go functions registered at compile time. No dynamic loading, no plugin discovery. See [docs/TOOLS.md](docs/TOOLS.md) for the canonical tool reference.

**Alpha tools:**
- `shell` — run shell commands (with timeout, background, auto-background)
- `tmux` — manage tmux sessions (start, send keys, read pane output, list, kill)
- `read` — read file contents
- `write` — create/overwrite files
- `edit` — find-and-replace in files (with syntax validation for .json, .toml, .go, .yaml/.yml, .xml, .py, .sh/.bash)
- `web_fetch` — fetch web content. Default: Anthropic server-side tool. Fallback: client-side HTTP GET with readability extraction (`fetch_provider = "builtin"`)
- `web_search` — search the web. Default: Anthropic server-side tool. Fallback: Brave Search API (`search_provider = "brave"`)
- `summary` — summarize/extract from a file via Haiku without loading it into context
- `memory_search` — FTS5 search over memory files + conversation history (sort by relevance or recency)
- `remind` — defer a thought for later (delay, tomorrow, specific date); wake=true actively wakes the session
- `scratchpad` — working notes that survive compaction (write/read/clear/list)
- `spawn` — sub-call to a model (raw/character: one-shot, clone: branch session with full tools, explore: read-only codebase research)
- `send_telegram` — send proactive Telegram messages and media. `send_as` parameter controls file type: `"document"` (default), `"voice"`, `"video"`, `"photo"`, `"audio"`, `"animation"` (GIF). With `send_as="voice"` and text (no file_path), synthesizes speech via TTS and sends as a voice note.
- `send_to_session` — inject a message into another session (cross-session communication). `reply_to` param: `"caller"` (default) routes response back to calling session, `"session"` sends response to the target session's own Telegram chat
- `todo` — manage a per-agent task list (add, list, complete, remove) with priority ordering
- `bitwarden_search` — search Bitwarden vault items by name/URI/folder (metadata only, no passwords)
- `bitwarden_unlock` — unlock a vault item (requires admin approval via aisudo/Telegram), caches for TTL

### Tool Piping (Exec Bridge)

Selected tools are exposed as shell functions inside `shell` commands. A per-shell unix socket bridges the subprocess back to the foci process, enabling unix-style composition without consuming inference passes for intermediate results.

**How it works:**
- Each non-background shell call creates an `ExecBridge` with a unique unix socket
- Shell functions (`foci_web_search`, `foci_http_request`, etc.) are generated and sourced at startup
- Functions use `jq` for safe JSON argument construction and `foci-call` for socket communication
- `set -o pipefail` is prepended to all shell commands

**Exported tools** (controlled by `ExecExport: true` on the Tool struct):
- `foci_http_request <url> [--method M] [--header 'K: V'] [--body B] [--save-to P]`
- `foci_web_fetch <url> [--raw]`
- `foci_web_search <query>`
- `foci_memory_search <query>`
- `foci_todo <action> [args...]`
- `foci_send_telegram <text>` (also reads stdin when no args)
- `foci_spawn <prompt> [--model M] [--context C]`

**Example:** `foci_web_search "golang generics" | head -3 | foci_send_telegram`

**Dependencies:** `jq` (for JSON construction in shell functions), `foci-call` binary (installed to `/usr/local/bin` by setup.sh).

### Tmux Session Monitoring

The `tmux` tool includes operations for monitoring pane inactivity:

- `watch` — monitor a pane for inactivity; fires if content unchanged for threshold seconds (default 30s). Watches persist across restarts.
  - Parameters: `session` (required), `window` (default 0), `threshold_seconds` (default 30)
  - Tracks content with MD5 hash; timer resets on change
  - Runs as background goroutine, one-shot alert mechanism
- `unwatch` — stop monitoring a session

**Use case:** Long-running background tasks. Start a build/deploy with `tmux start`, then `watch` to be notified when it completes.

### Tmux Memory Monitor

Background goroutine checking the tmux server's RSS at configurable intervals. Long-running tmux sessions (especially hosting TUI apps) accumulate memory via glibc malloc fragmentation.

Three thresholds (all configurable as `%` of RAM, `mb`, or `gb`):
- **warn** (default 10%) — log WARN, send Telegram notification
- **critical** (default 20%) — log WARN (stronger), send Telegram notification
- **kill** (default 30%) — log ERROR, send Telegram notification, `tmux kill-server`, clean up tool state

Notifications go to agents whose `inject_agent_warnings` is false. Dedup prevents spam: same threshold only fires once until memory drops below it or tmux is killed.

### Warning Injection

When `inject_agent_warnings` is enabled, WARN/ERROR log events are pushed into the agent's `WarningQueue` and surfaced in two ways:

- **Passive:** warnings are drained and prepended to the next user message as `[system warnings]` blocks. This is the default path — warnings piggyback on existing interaction.
- **Proactive:** the keepalive runner checks `WarningQueue.Pending()` every 30s and, if warnings are waiting, injects them as a `[proactive system warnings]` user message that triggers a full agent turn. Rate limited by user activity: 1 per `warning_proactive_active_interval` (default 5m) if the user is active, 1 per `warning_proactive_inactive_interval` (default 1h) if inactive. Activity is determined by `LastUserMessageTime()` vs `warning_proactive_activity_threshold` (default 10m). The agent response is delivered to Telegram.

Proactive dispatch ensures critical warnings (disk full, tmux OOM) reach the agent immediately rather than sitting unnoticed until the next user message.

### System Memory Guard

Background goroutine monitoring total RSS of all processes owned by the foci system user. Reads `/proc/[pid]/status` directly — no external commands.

Two thresholds (configurable as `%` of RAM):
- **warn** (default 25%) — log WARN, inject warning to agent session via `WarningQueue` (surfaces via proactive warning dispatch)
- **kill** (default 40%) — find largest non-foci process by RSS (excluding `os.Getpid()`), SIGTERM, wait 5s, SIGKILL if needed

Both thresholds require **memory pressure** (PSI `avg10` > configurable threshold, default 10.0) via `/proc/pressure/memory`. This prevents false alarms when the system has plenty of free RAM — high RSS alone doesn't indicate a problem if there's no actual pressure.

Warn dedup: fires once per threshold crossing, resets when RSS drops below warn threshold.

Entire feature is disableable via `memory_guard_enabled = false`.

### Tool Result Guard

When a tool returns a result exceeding a configurable character threshold (default: 5,000 chars), foci does NOT inject the full result into session history. Instead:

1. Write the full result to a temp file: `{temp_dir}/tool-result-{tool}-{random}.txt`
2. Return only a guard message — no partial content is included:
   ```
   Result too large (47231 chars, limit 5000). Full output saved to /tmp/foci-tool-results/tool-result-shell-a1b2c3d4.txt.
   Use `head -n 50` to preview, or `grep`/`ack` to search for specific content.
   ```

The tool hint is contextual: `jq` for JSON results, `mdq` for markdown, `grep`/`head`/`tail` otherwise.

Before returning the guard message, the agent makes a side-call to Haiku to auto-summarise the oversized content. Recent conversation turns are included as context so the summary focuses on what the agent likely needs. If the Haiku call fails, the agent falls back to the guard message with hints. This eliminates the wasted turns the agent would otherwise spend re-reading the saved file.

This prevents large tool results (e.g. `shell cat bigfile.txt`) from permanently bloating session history. The agent can still access the full result via the saved file — it just doesn't sit in context forever.

Configurable: max result chars, temp directory, summary context turns, and summary context chars. See [CONFIG.md](CONFIG.md).

**http_request — file saves, binary handling, and auto-background:**
- `save_to` — save response body to a specific file path (returns status + headers + path, not body)
- `save_from_json_path` — extract a value from JSON response by dot path (e.g. `data.0.url`); if it's a `data:` URI, decodes base64 to binary. Requires `save_to`. Designed for image generation APIs that return base64 data URIs.
- Binary content types (`image/*`, `audio/*`, `video/*`, etc.) auto-save to temp file when `save_to` is not set
- `background` parameter — if `true`, request runs immediately in background and result is delivered asynchronously
- Auto-background — if a request exceeds the `exec_auto_background` threshold, it auto-backgrounds and the result is delivered when complete (same mechanism as shell)

**http_request — body_file (large payload support):**
- `body_file` — read request body from a local file path instead of inline `body`. Solves the problem of large payloads (e.g. 1.7MB base64 audio JSON) that can't be passed as inline string parameters.
- File contents support `{{secret:NAME}}` templates (resolved before sending).
- `body`, `body_file`, and `files` are all mutually exclusive.
- File must exist, be readable, not be a directory, and not exceed `max_upload_file_size`.

**http_request — multipart/form-data file uploads:**
- `files` — array of file attachments. Each has `field_name` (form field name), `file_path` (local path), and optional `filename` (override, defaults to basename). When present, the request is sent as `multipart/form-data`.
- `form_fields` — object of additional text form fields for multipart requests. Values support `{{secret:NAME}}` templates. Requires `files`.
- `body` and `files` are mutually exclusive — error if both set.
- Files are validated: must exist, be readable, and not exceed `max_upload_file_size` (default 50MB, configurable globally in `[tools]` and per-agent).
- Content-Type is set automatically from the multipart writer (includes boundary); agent-set Content-Type is overridden when files are present.

**Each tool is a function with signature:**
```go
type Tool struct {
    Name        string
    Description string
    Parameters  json.RawMessage  // JSON Schema
    Execute     func(ctx context.Context, params json.RawMessage) (ToolResult, error)
}
```

## Identity & Knowledge

### Workspace Bootstrap

On session start, read markdown files from the workspace directory and inject them as system prompt blocks. Files are read in a fixed order (configurable in TOML):

```
IDENTITY.md, SOUL.md, COHERENCE.md, AGENTS.md, TOOLS.md, USER.md, MEMORY.md, HEARTBEAT.md
```

Order matters for cache efficiency — see [docs/CACHING.md](docs/CACHING.md).

### Skills

Skills extend the agent without code changes. A skill is a directory containing a `SKILL.md` file with YAML frontmatter and markdown instructions:

```
/home/foci/skills/
├── reheat/
│   ├── SKILL.md
│   └── reheat.sh
└── research/
    └── SKILL.md
```

**SKILL.md format:**
```yaml
---
name: reheat
description: Clear API cooldowns
command: /reheat
script: reheat.sh
---

Instructions the agent follows when this skill is activated.
The agent reads this file with the `read` tool.
```

**Frontmatter fields:**
- `name` (required) — skill identifier
- `description` (required) — one-line description, shown in system prompt
- `command` (optional) — slash command to register (e.g. `/reheat`)
- `script` (optional) — script to run when the command fires (path relative to skill dir)

**How it works:**
1. Config lists directories to scan: `[skills] dirs = ["/home/foci/skills"]`
2. On startup, scan each dir for subdirectories containing `SKILL.md`
3. Parse frontmatter, collect name + description into a registry
4. Inject skill list (name, description, SKILL.md path) as a system prompt block — the agent knows what's available but doesn't load full instructions until needed
5. The agent reads the full `SKILL.md` with the `read` tool when it decides a skill applies
6. If `command` + `script` are both present, auto-register as a slash command (runs the script directly, no agent turn)

Skills are not dynamic plugins — no code loading, no compilation. Just directories of files the agent can read, with optional shell scripts for slash commands.

### Memory System

**Alpha:** File-based with FTS5 search and multiple weighted sources.
- Memory files in `workspace/memory/YYYY-MM-DD.md`
- Curated long-term memory in `workspace/MEMORY.md`
- `MEMORY.md` injected into system prompt on each turn

**Multiple Sources with Weights:**

Configured via `[[memory.sources]]` entries, each with a name, directory, and weight multiplier. See [CONFIG.md](CONFIG.md).

Each source is indexed with `source={sourceName}` and searched with weight multiplier: `rank * (1.0 + weight)`.

**Backward Compatibility:**

If `sources` is empty, falls back to a single `dir` field with default weight.

**Search:** Pluggable search backends — FTS5 (default) and bleve. Both can run simultaneously for A/B comparison.

**FTS5 backend** — SQLite FTS5 index over multiple sources with conversation history:

```sql
CREATE VIRTUAL TABLE memory_fts USING fts5(
  content, path, source,    -- source: 'canonical'|'code'|'docs'|'conversation'
  tokenize='porter unicode61'
);

-- Search with per-source weights
SELECT path, snippet(memory_fts, 0, '→', '←', '...', 30),
       CASE source
         WHEN 'canonical' THEN rank * 2.0    -- (1.0 + 1.0)
         WHEN 'code' THEN rank * 1.3         -- (1.0 + 0.3)
         WHEN 'docs' THEN rank * 1.5         -- (1.0 + 0.5)
         WHEN 'conversation' THEN rank * 1.0 -- default
       END AS weighted_rank
FROM memory_fts
WHERE memory_fts MATCH ?
ORDER BY weighted_rank;
```

**Bleve backend** — blevesearch/bleve full-text index. Files only (no conversation history). English analyzer with Porter stemming, per-source weighted ranking, highlighted snippets. Index stored at `{data_dir}/memory.bleve`. Clean rebuild on each reindex (close → remove → recreate).

Active backends are listed in `search_backends` (default: `["fts5"]`).

When multiple backends are active, the `memory_search` tool exposes a `backend` parameter so the agent can choose which to query. When only one backend is active, the parameter is hidden.

**Indexing and Auto-Reindex:**

- Memory files: re-indexed on startup
- File watching: optional auto-reindex when `.md` files change via fsnotify
- Debounce delay is configurable (default: immediate).
- Conversation history: indexed as messages are logged (FTS5 only — bleve skips conversations)

**Why FTS5 over vector embeddings:**
- Zero dependencies (built into SQLite, which we already use)
- Instant queries, no API calls
- Deterministic, debuggable
- Covers 90% of memory recall — you usually remember roughly what you wrote

**Why bleve as an alternative:**
- Pure Go, no CGo — simpler builds
- Smaller index without conversation history (files only)
- Richer analysis pipeline (built-in English analyzer, term vectors, highlighting)
- Can run alongside FTS5 for A/B comparison

**Maybe later:** Vector embeddings for semantic search when keyword search proves insufficient.

## Scheduling & Background

### Scheduled Wakes

**HTTP endpoint (for cron jobs):**
```
POST /wake
{"agent": "main", "text": "morning routine", "no_compact": true}
```
Injects text as a user message into a branch session. When `no_compact` is true, the session returns its result instead of triggering compaction if the context limit is reached — useful for cron jobs that inherit a large parent context and shouldn't waste mana compacting.

**Tool-based scheduling:**
The `remind` tool with `wake=true` allows the agent to schedule messages to itself:
- `remind(text="check status", when="30m", wake=true)` — wake after a duration
- `remind(text="meeting", when="2026-02-21T15:30:00Z", wake=true)` — wake at ISO timestamp
- One-shot, auto-cleaned after firing
- Useful for self-reminders, follow-ups, or timed actions

System crontab can trigger `/wake` endpoint for external scheduling. For agent-initiated delays, use the `remind` tool with `wake=true`.

### Activity gating

Both `POST /send` and `POST /wake` accept optional activity gating fields:
- `if_active` (Go duration, e.g. `"8h"`) — skip if no user activity within the window ("skipped: no recent user activity")
- `if_inactive` (Go duration, e.g. `"30m"`) — skip if user WAS active within the window ("skipped: session recently active")

"Real user activity" means messages from allowed Telegram users via the primary bot. It explicitly excludes: CLI-injected messages (`foci send`/`foci branch`), async notifications, agent-to-agent messages, and system-injected messages. This prevents the gate from defeating itself — a cron send cannot reset the activity timer.

The timestamp is stored per-agent in the state store (`agent:<id>:last_user_activity`). The CLI exposes this as `--if-active <duration>` and `--if-inactive <duration>` on `send` and `branch` commands. See [docs/CLI.md](docs/CLI.md) for full CLI reference.

### Keepalive & Background Work

Four timer-driven mechanisms run on a ~30s tick loop per agent:

**Keepalive** — Cache keepalive. Fires when `time_since(lastCacheWarmed) >= keepalive.interval`. Creates a lightweight branch session with `no_compact` to keep the Anthropic cache prefix warm. Does no real work.

**Background work** — Mana-gated task execution. Fires when:
1. User has been idle for `background.interval`
2. Open todos tagged "background" exist
3. Manamometer says we can afford it

Creates a branch session that picks up the highest-priority background todo item.

**Memory formation** — Periodic memory capture. Fires when `interval` (default 1h) has elapsed since last formation and user activity occurred within that window. Captures conversation memories to daily files.

**Memory consolidation** — MEMORY.md curation. Fires when `consolidation_interval` (default 20h) has elapsed since last run and user was active within the last hour. Reviews daily memory files and curates MEMORY.md. Last-run timestamp persisted in state store.

**Manamometer** — Linear interpolation of expected mana over the 5-hour budget window. After `invest_interval` (default 30m) of quiet to let the cache build, the expected mana line drops linearly from 100% to 0% at window end. Work fires when actual mana exceeds expected mana. Near reset, even tiny mana is "in credit" since the budget resets soon.

Config: `[keepalive]`, `[background]`, and `[memory_formation]` sections. See [docs/HEARTBEAT.md](docs/HEARTBEAT.md) for full details.

## Secrets

Secrets never pass through agent context. The agent cannot read, echo, or exfiltrate credentials. See [docs/SECRETS.md](docs/SECRETS.md) for the full security model, OS-level protection, domain locking, Bitwarden integration, and setup instructions.

### Principle
Credentials are loaded once at startup into process memory. Built-in integrations (Anthropic, Telegram, Brave Search) use them directly from Go structs. The agent interacts with tools, tools use credentials internally — the agent never constructs auth headers or sees token values.

### Per-agent secrets

Secrets in `secrets.toml` are global by default. Agents can have their own overrides via `[agents.ID]` sections.

Resolution order: agent-specific value wins over global. Keys not overridden in the agent section fall back to globals. Each agent only sees its own overrides — agent A cannot see agent B's secrets. Built-in credential resolution (anthropic.setup_token, telegram, brave) stays global (process-wide); per-agent scoping applies to tool-visible secrets (shell templates, http_request, redaction, system prompt secret names).

### What the agent knows about secrets
- That secrets exist (by name): "anthropic", "telegram", "brave", "custom.github_token"
  - Available secret names are injected into the system prompt at startup so the agent can discover what's available
  - Per-agent overrides add or replace names visible to that agent
  - Unresolved secret references in shell commands are errors (not silently passed through)
- If bitwarden is enabled, the agent knows it can search the vault and request unlocks
  - The agent never sees password values — only template references `{{secret:bw.ID}}`
- How to reference them: `{{secret:NAME}}` (static) or `{{secret:bw.UUID}}` (bitwarden)
- Nothing about their values

## Concurrency & Interrupts

Hard constraints learned from OpenClaw's failure modes. These aren't nice-to-haves.

### Message receiving never blocks

The Telegram listener runs on its own goroutine. It receives and queues messages regardless of what the agent is doing. Even if the agent is mid-way through a 5-minute tool call, incoming messages are received, logged, and — if they're slash commands — executed immediately.

```
[telegram goroutine]  →  receive msg  →  slash command?  →  yes: execute, reply
                                                          →  no:  enqueue for agent
[agent goroutine]     →  dequeue msg  →  build turn  →  call API  →  run tools  →  reply
```

Two goroutines, one channel. The agent pulls from the queue at its own pace. The receiver never waits on the agent.

**HTTP connection pool:** Both goroutines share a single `http.Client` for Telegram API calls. The transport is configured with `MaxIdleConnsPerHost=8` to prevent connection pool exhaustion — the long-poll `GetUpdates` holds one connection indefinitely, and the agent worker sends typing indicators + tool call messages concurrently. With Go's default of 2 connections per host, the receiver goroutine would block waiting for a free connection whenever the agent worker was also making a request.

### Agent turns are cancellable

Every agent turn gets a `context.Context`. When a cancel signal arrives (new `/stop` command, shutdown, timeout), the context is cancelled and:

- In-flight Anthropic API calls abort via the HTTP client's context
- In-flight tool executions (exec, web_fetch) abort via process kill
- The agent loop checks `ctx.Err()` between tool calls and after API responses

**Stop means stop, immediately.** Not "after the current tool finishes." If shell is running a 3-minute command and the user sends `/stop`, the process is killed within seconds. This is a first-class design constraint.

```go
func (a *Agent) RunTurn(ctx context.Context, msg string) error {
    // Every API call and tool execution passes ctx
    resp, err := a.client.Send(ctx, messages)
    if ctx.Err() != nil {
        return ctx.Err() // cancelled mid-API-call
    }
    for _, tool := range resp.ToolCalls {
        result, err := a.tools.Execute(ctx, tool)
        if ctx.Err() != nil {
            return ctx.Err() // cancelled mid-tool
        }
    }
}
```

### Long-running tools yield control

Tool executions that may block (exec with long commands, web_fetch on slow endpoints) must be interruptible via context cancellation. The exec tool runs commands in a child process and kills the process group on context cancel.

No tool call should prevent the system from responding to interrupts. If it does, that's a bug.

### Session reset guard

`/reset` refuses when the agent is mid-turn, preventing accidental data loss. This is the only reset mechanism — foci has no automatic daily/idle session resets. Sessions persist until explicitly reset by the user or the process restarts.

**Session-end memory formation:** Before clearing the session, memory formation fires asynchronously — creating a branch from the expiring session to preserve conversation history. Configured via `[memory_formation]` section (`session_end_enabled`, `session_end_prompt`). The branch has a 120-second timeout and is non-fatal — if it fails, the reset has already proceeded. Branch sessions can opt out via `NoResetHook` in their branch metadata. The same hook fires on multiball TTL reclaim.

If automatic resets are added later: never reset an active session. A session is "active" if the agent is processing a turn OR the last message was received less than N minutes ago. OpenClaw's blunt `updatedAt < dailyResetAt` check wiped an active conversation mid-flow — that's the failure to avoid.

## Logging

Two log outputs, both plain files on disk. No systemd journal dependency.

### Event log (`foci.log`)
Human-readable, one line per event. Timestamp + level + component + message.

```
2026-02-21T03:52:39Z INFO  [telegram] bot started as @rk_focibot
2026-02-21T03:52:41Z INFO  [agent] stop_reason=end_turn input=1119 output=164 cache_read=0 cache_write=1119
2026-02-21T03:53:01Z WARN  [exec] command timed out after 30s
2026-02-21T03:53:05Z ERROR [anthropic] 529 overloaded, retrying in 5s
```

Levels: DEBUG, INFO, WARN, ERROR. Default: INFO. Configurable in TOML.

Also writes to stderr so `tmux capture-pane` and `journalctl` (if run as a unit) work naturally.

### API log (`api.jsonl` + `api.db`)
Structured JSONL, one object per API request. For debugging cache behaviour, tracking costs, auditing usage. Also written to SQLite (`api.db`) with indexes on `ts` and `session`, plus a `call_type` column distinguishing conversation, compaction, summary, and spawn calls.

```json
{"ts":"2026-02-21T03:52:41Z","session":"agent:main:main","model":"claude-haiku-4-5","input":1119,"output":164,"cache_read":0,"cache_write":1119,"cost_usd":0.003,"duration_ms":1240,"call_type":"conversation"}
```

Searchable with `jq` (JSONL) or `sqlite3 api.db` (SQLite). The agent can query its own API logs via tools.

**Full payload logging:** Optional — records complete API request/response bodies (system prompt, messages, tool calls, full response). Off by default (large files, contains conversation content). Configurable via `full_payload` in `[logging]`.

Useful during development and debugging. The agent and `/last` can reference it for detailed inspection of what was actually sent to Anthropic.

### Cache bust alerts

When `cache_read` drops significantly vs the previous request, foci sends an immediate Telegram notification — zero tokens spent. See [docs/CACHING.md](docs/CACHING.md) for configuration and details.

### Log rotation

Built-in rotation runs every `rotation_period` (default 24h). For each log file (foci.log, api.jsonl, api-payload.jsonl):

1. Stream line-by-line (memory-efficient — handles multi-GB files)
2. Lines older than `retention_period` (default 48h) → compressed to `archive/{name}-{date}.gz`
3. Recent lines → kept in active log
4. Logger file handles reopened atomically after swap

Enabled by default (`log_rotation = true`). Disable with `log_rotation = false` if using external logrotate. The scanner buffer size is configurable via `rotation_max_line_size` (default "64MB") — API payload lines can be very large with full request/response JSON.

## Slash Commands

Messages starting with `/` are intercepted before reaching the agent. They execute immediately - never queued behind an in-flight agent turn. This is a hard architectural constraint: commands must bypass the agent reply pipeline entirely.

**Inline keyboards:** Commands that accept parameters (`/model`, `/thinking`, `/effort`, `/config`, `/sessions`, `/tmux`) show an inline keyboard with available options when invoked bare (no arguments). Tapping a button executes the command with that argument and edits the message to show the result. Typed commands with arguments (e.g. `/model sonnet`) still work as before — the keyboard is only shown when no args are provided. Uses the same `InlineKeyboardMarkup` + callback query mechanism as tool call expansion. Callback data format: `cmd:/name args`.

### Built-in commands

**Session:**
- `/status` - session key, message count, total tokens (input/output/cache read/write), model, uptime, current agent turn status (idle/processing)
- `/reset` - clear session history, start fresh. Confirms before acting.
- `/model [name]` - show current model, or switch to `name` for this session
- `/session` - dump raw session metadata (message count, created at, last activity, compaction count)

**Debug & inspection:**
- `/cache` - last 5 API calls with cache hit/miss breakdown from api.jsonl. Shows: tokens in, cache read, cache write, cost. Quick way to verify caching is working.
- `/last` - show the last API request/response: model, stop reason, token usage, duration, cost. The single most useful debug command.
- `/tools` - list registered tools with enabled/disabled status
- `/config` - show usage. `/config toml` for raw TOML output. `/config table` for formatted config table. `/config available` to discover unset options.
- `/ping` - return "pong" with timestamp. Simplest possible liveness check.
- `/prompts` - show configured prompt paths (compaction summary, session reset, handoff message, fork prompt) with existence checks, plus prompt files found on disk in workspace and shared directories.

**Logs:**
- `/log [n]` - last `n` lines from foci.log (default 20)
- `/errors [n]` - last `n` ERROR/WARN lines from foci.log (default 10)
- `/cost <subcommand>` - API cost from api.jsonl. No args: show usage. `today`: per-session table. `24h`: rolling 24h with per-category table. `week`: 7-day daily table. `<days>`: total for last N days.

**Context:**
- `/context` - full context window breakdown. Uses the Anthropic token counting API (`/v1/messages/count_tokens`) for exact per-component token counts: total, per-file system prompt sections, tools, and conversation. Makes parallel API calls (one per system section plus baseline/full/system-only) and caches results until context changes (message count or system prompt content). Falls back to character-based estimates (~chars/4) if the counting API fails. All counts shown as tokens (exact "N tokens" or estimated "~N tokens"). Per-role conversation breakdown (user, assistant, tool results) is always estimated. Last API call token details (input, cache_read, cache_write, output) shown separately.

**Sessions:**
- `/sessions` or `/sessions list` — list all per-chat sessions for this agent. Shows chat ID, username, message count, last active time, and which is the default (★).
- `/sessions default <chat_id>` — set a specific chat as the default session (used by heartbeats, cron, proactive features).
- `/sessions info` — show details for the current chat's session (chat ID, default status, message count, username).
- `/sessions index [type] [status]` — query the session metadata index (all agents). Optional filters: type (chat/multiball/spawn/cron/branch), status (active/compacted/cleared). Shows session key, type, status, created time, and parent session.

**Agents:**
- `/agents` - list active agent sessions with status, model, and message counts
- `/agents new` - interactive wizard for creating a new agent. Walks through: agent ID, display name, emoji, model, bot token secret, character file mode. Creates workspace, appends config to foci.toml, adds crontab entries. Requires restart to activate.

**System:**
- `/version` - binary version, go version, build time, git commit
- `/uptime` - process uptime, system load, memory usage
- `/reload` - reload config and workspace files (IDENTITY.md, SOUL.md, etc.) without restarting

### Custom commands (TOML config)

Custom commands are defined as `[[commands]]` entries with a name, description, shell script, and optional timeout. See [CONFIG.md](CONFIG.md).

Each custom command runs a shell script and returns stdout as a Telegram message. Timeout: 10s default, configurable per command.

### Code-defined commands

Commands can also be registered in Go for anything that needs access to internal state:

```go
type Command struct {
    Name        string
    Description string
    Execute     func(ctx context.Context, args string) (string, error)
}
```

Built-in commands are code-defined. Custom commands from TOML are script-defined. Both share the same dispatch path.

### Dispatch

1. Telegram message arrives starting with `/`
2. Router matches command name (before any agent processing)
3. Execute immediately, return result to Telegram
4. Never touches the agent session or message history

If the agent is mid-turn processing a previous message, `/status` still returns instantly. That's the point.

**Unknown commands** — if a `/` message doesn't match any registered command, the command system catches it and replies with a suggestion instead of passing it to the agent. Uses Levenshtein distance (≤ 2) and prefix matching (≥ 3 chars) to suggest close matches: "Unknown command `/statsu`. Did you mean `/status`?" If no close matches, points to `/help`.

## Config

Single TOML file. Flat, commented, no deep nesting. Supports single-agent (`[agent]`) and multi-agent (`[[agents]]`) formats. Bot tokens are resolved from `secrets.toml` via convention. See [CONFIG.md](CONFIG.md) for the full reference.


## Testing Priority

**✅ PASSED: Session branching cache sharing.**
Confirmed working — wake/cron branches read ~63k cached tokens on first request with zero cold starts. Multiball branches also share parent cache prefix.
6. Send another request on parent → observe parent cache still works (branch didn't bust it)

If step 5 shows a full cache write instead of a read, the branching architecture doesn't work and we need to rethink.

## Dependencies (Go)

Minimal:
- `github.com/BurntSushi/toml` — config parsing
- `github.com/go-telegram-bot-api/telegram-bot-api/v5` — Telegram (or hand-roll, it's just HTTP)
- Standard library for everything else (net/http, encoding/json, os/exec, etc.)

## Setup Script (`setup.sh`)

Idempotent. Run it once to install, run it again to update. Safe to re-run.

### What it does

1. **System user:** Create `foci` user if it doesn't exist (no login shell, home at `/home/foci`)
2. **Binary:** Build from source (`go build`) or download prebuilt release. Install `foci-gw`, `foci`, and `foci-call` to `/usr/local/bin/`
3. **systemd service:** Install `/etc/systemd/system/foci.service` if it doesn't exist. `User=foci`, `WorkingDirectory=/home/foci`, restart on failure. Enable and start.
4. **Config:** Write `/home/foci/foci.toml` if it doesn't exist. Prompt interactively for:
   - Telegram bot token
   - Anthropic auth (via `foci auth` setup token or API key)
   - Telegram user ID (allowed_users)
   - Agent model (default: claude-haiku-4-5)
5. **Character files:** Create `~/character/` with template content if files don't exist:
   - `identity.md` — name, vibe, emoji
   - `soul.md` — inner life, what you notice
   - `user.md` — about your human
   - `agents.md` — how you work
   - `tools.md` — what you use
   - `memory.md` — what you've learned
6. **Directories:** Create `~/sessions/`, `~/workspace/memory/`, `~/character/` under foci's home
7. **Log rotation:** Install logrotate config for `foci.log` and `api.jsonl` (weekly, keep 4, compress)
8. **PATH:** Symlinks already in `/usr/local/bin/`, nothing extra needed

### Config references character files

The agent's `workspace` path points to the character file directory. Bootstrap loads system files in the configured order.

### Template content

Character file templates are minimal starters — just enough structure for the agent to understand what goes where, with placeholder text encouraging the human to fill them in. Not our files — generic ones.

### Update mode

When binaries already exist: rebuild/re-download, restart service. When config already exists: don't touch it. When character files already exist: don't touch them. Idempotent means safe.

### What it doesn't do

- No reverse proxy setup (that's deployment-specific)
- No DNS/domain config
- No external port exposure (binds to localhost by default)
- No automatic Telegram webhook (uses long-polling)

## Directory Structure

```
foci/
├── foci.toml
├── go.mod
├── go.sum
├── main.go              # entry point, wire everything together
├── anthropic/           # API client, streaming, caching
│   ├── client.go
│   ├── types.go
│   └── cache_test.go   # THE critical test
├── session/             # session store, branching
│   ├── store.go
│   ├── branch.go
│   └── store_test.go
├── telegram/            # bot, message routing
│   └── bot.go
├── tools/               # tool implementations
│   ├── registry.go
│   ├── exec.go
│   ├── files.go
│   ├── web.go
│   └── memory.go
├── workspace/           # bootstrap file loading
│   └── bootstrap.go
├── compaction/          # simple compaction
│   └── compact.go
└── config/              # TOML config loading
    └── config.go
```

---

_This spec describes what we're building, not how to build it. Implementation decisions belong in the code._
