# Foci — Specification

A minimal, maintainable agent platform in Go. One binary, no framework, no bloat.

## Philosophy

- **Simple over powerful.** If a feature needs complex config, rethink the feature.
- **Explicit over clever.** No plugin architectures, no hook systems, no middleware chains.
- **Own every line.** No 594MB node_modules. Dependencies are standard library + a few well-chosen packages.
- **Cache-aware from day zero.** Anthropic prompt caching drives architectural decisions.

## Sessions & State

### Session Keys

Format: `{agentID}/{type}{id}[/{childType}{childTS}]` — a stable identity that survives compaction and /reset; see [docs/SESSION_KEYS.md](docs/SESSION_KEYS.md) for full reference.

- `fotini/c123456789` — per-chat DM session (keyed by Telegram chat ID)
- `main/i0/0/b1709123456` — cron-triggered branch (child of default independent session)
- `main/c123/b1709123456` — sub-agent branch or facet fork
- `clutch/i1709123456` — WebSocket voice session (ephemeral, one per connection)

Each Telegram DM gets its own session, keyed by chat ID. One session is designated as the "default" — this is what cron (`foci send`/`foci branch`) and proactive features target. If no default is set, the first chat session created becomes the default. The default can be changed via `/sessions default <chat_id>`.

Child sessions (branches, spawns) append `/{childType}{childTS}` to the parent key. Branch sessions inherit the parent's message prefix for cache sharing.

### Session Branching (Cache Sharing)

A branch session copies the parent's system prompt + message history at a point in time. The shared prefix hits the cache instead of being re-tokenized. See [docs/CACHING.md](docs/CACHING.md) for pricing details and cache requirements.

**Rules:**
- Parent session: append-only, owns canonical history
- Branch session: snapshot of parent messages at branch point + own appended messages
- Branch never writes back to parent history
- Branch result delivered as a message to the parent session or via Telegram

**Storage:** A branch record holds:
```
parent_key: "main/i0/0"
branch_point: <message index>
messages: [only messages after branch point]
```

API payload assembly: system prompt + parent.messages[:branch_point] + branch.messages

**Branch orientation:** All branches (facet, cron/wake, spawn) receive an orientation message as their first user message. The orientation tells the branch its type, keys, and communication rules:
- **Headless** (cron, spawn, keepalive — `direct_chat=false`): must NEVER use `send_to_chat`; reports significant work or errors to parent via `send_to_session`; stays silent when nothing happened.
- **Facet** (`direct_chat=true`): has its own Telegram bot for direct user replies; keeps the main session informed of visible work via `send_to_session`; sends a completion summary before going idle.

Default orientation text is embedded in `shared/prompts/branch-orientation-headless.md` and `shared/prompts/branch-orientation-facet.md`. Config override via `branch_orientation_prompt` (per-agent or global) takes precedence. Template variables `{branch_key}`, `{parent_key}`, `{branch_type}`, `{direct_chat}` are replaced at branch creation time.

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
- Session file rotation: on compaction, the pre-compaction file is renamed in place to a timestamp-based archive (e.g. `root.2026-03-04T02-30-00Z.jsonl`) before writing the new compacted `root.jsonl` — the session key is unchanged. If multiple compactions occur within the same second, a counter is added (e.g. `.2026-03-04T02-30-00Z.2.jsonl`). Archives are read only to recover a branch's parent prefix after the parent was compacted/reset (P2-5); otherwise they exist for usage tracking and audit.
- Async-pending guard: compaction is deferred while a session has pending async tool results (spawn clone, auto-backgrounded shell/http). This prevents compacting away the context that the async result relates to. Compaction fires naturally on a later turn once all results have been delivered.

All compaction parameters are configurable per-agent or globally: threshold, max tokens, min messages, summary/handoff/reset prompts, debug mode, and preserve-messages count. Prompt files are read live — edits take effect without restart. See [CONFIG.md](CONFIG.md).

### Session Metadata Index

A SQLite index (`session_index.db`) tracks all session files with metadata: session key, file path, created timestamp, parent session key (for branches), session type (chat/facet/spawn/cron/branch), and status (active/compacted/cleared). Rebuilt from disk on startup by scanning all non-archive `.jsonl` files. Updated in real-time via lifecycle hooks on the session store (create, compact, clear). Queryable via `/sessions index [type] [status]`.

## Communication

### Anthropic API

- **Auth:** API key or Claude Code credentials fallback. See [docs/AUTH.md](docs/AUTH.md).
- **Model:** Haiku (`claude-haiku-4-5`) for foci itself; configurable per agent
- **Prompt caching:** Two cache breakpoints per API request (system prompt + conversation history). See [docs/CACHING.md](docs/CACHING.md).
- **Streaming:** Server-sent events for responses. Telegram streaming shows HTML-formatted output in real-time — partial markdown delimiters are stripped before conversion so incomplete syntax doesn't break rendering.

### Telegram Bot

- Long-polling (not webhooks) for simplicity
- Receive: text messages, voice notes, file attachments (beta)
- Send: text messages, markdown formatting, voice notes, file attachments (beta)
- Route incoming messages to the correct agent session
- DM only for alpha; group chat support in beta
- Startup notification: sends "botname restarted at HH:MM:SS" to the last active chat. Controlled by global `[telegram] startup_notify` (default true) with per-agent override via `startup_notify`. Set to `false` for silent bots (e.g., cron-only agents).
- Crash/reboot detection: on startup, classifies restart as clean/crash/reboot by comparing last shutdown timestamp with system uptime. Unexpected restarts include diagnostic findings (ERROR/FATAL lines from logs) in the notification. Clean shutdown timestamp recorded on graceful exit via signal handler.

### Multi-Bot Sessions (/facet)

`/facet` forks a session to a secondary Telegram bot — same agent, same context snapshot, parallel thread. Bots can be per-agent or shared pool. See [docs/FACET.md](docs/FACET.md) for bot pool config, session lifecycle, routing, and use cases.

Per-agent facet pools are configured in the agent's `facet_bots` list. A shared fallback pool is configured globally under `[telegram]`. See [CONFIG.md](CONFIG.md).

**Acquisition priority:** per-agent pool first, shared pool as fallback. Released bots return to whichever pool they came from.

**Restart survival:** The `bot → session_key` mapping is persisted in the state store. On restart, mappings are restored if the session file still exists on disk. Stale mappings are cleaned up automatically.

### Platform Button Abstraction

`platform.ButtonSender` is the single interface for sending messages with interactive buttons. Both Telegram (inline keyboards) and Discord (message components) implement it. `command.KeyboardOption` is aliased to `platform.ButtonChoice` (fields: Label, Data, Row). All button construction — command keyboards, permission prompts, tool result expansion — flows through centralized helpers on `ButtonSender`. Discord uses `"im:"` callback data prefix routing for interactive messages (e.g. permission prompts from delegated agents).

### Voice (Telegram Voice Notes)

**Inbound:** Receive Telegram voice notes → transcribe via STT provider (OpenAI-compatible, e.g. Groq Whisper) → inject transcript as the user message with a `[voice]` tag. The agent sees text, doesn't need to handle audio.

**Outbound:** Agent can send voice replies via `send_to_chat(text="...", send_as="voice")`. Text → TTS engine (Edge TTS, OpenAI, or similar) → send as Telegram voice note. Good for when the human is mobile/driving.

### WebSocket Voice Endpoint (`/voice`)

A WebSocket endpoint for real-time two-way voice conversation with an agent. Used by the FOCI Android app.

**Connection:** `GET /voice?api_key=KEY` → auth middleware → upgrade to WebSocket. Server sends a `connected` message with the available agent list. Client sends `select_agent` to pick an agent, server responds with `session_ready` and an ephemeral session key (`ID/iCONN_ID/CONN_ID`).

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

Saving is non-fatal — errors are logged as warnings. Images are sent to the API as image content blocks; PDFs (≤32MB) as document content blocks.

**Document conversion:** Convertible document types are automatically extracted to text and included as text content blocks:
- **CSV, plain text** — passed through as-is (no external tools needed)
- **HTML** — readability extraction → markdown (same pipeline as `web_fetch`)
- **DOCX, PPTX** — converted via `pandoc` (must be installed; agent is told if missing)
- **XLSX** — converted via `ssconvert` (gnumeric) or `pandoc` (agent is told if missing)

Converted text is subject to the tool result size guard (`max_result_chars`): oversized output is truncated with a pointer to the saved file on disk. Videos and unconvertible documents are saved only (no API processing).

### Message Metadata

Each user message injected into the conversation carries metadata the agent can see. This is NOT in the system prompt (that would bust cache) — it's prepended to the user message content. The exact lines are produced by the per-agent `statusline` template (the default reproduces the `[meta]`/`[state]` layout shown below); see `docs/CONFIG.md` for the field list and `${cmd}` embedding.

```
[meta] time={time} gap={gap} model={model} via={via}
```

Fields:
- `time` — current UTC timestamp
- `gap` — time since the previous message in this session (human-readable: "3h12m", "2d4h", "38s")
- `model` — current model name (so the agent knows its own capabilities)
- `via` — transport that delivered the message (telegram, discord, app, voice, api, cron)

**Why metadata on messages, not system prompt:** Dynamic values in the system prompt would bust the cache every turn. See [docs/CACHING.md](docs/CACHING.md).

### Deferred Replies

The agent can acknowledge a message and deliver a full response later. For complex questions requiring research or long tool chains:

1. Agent sends an immediate short reply ("Looking into this, give me a minute")
2. Agent continues working (tool calls, research, etc.)
3. Agent sends the full response when ready

Implementation: The agent turn can produce multiple Telegram messages. The first is sent immediately. Subsequent messages are sent as the agent completes tool calls. This is just streaming tool results to Telegram rather than batching everything into one final response.

Controlled by `batch_partial_assistant_messages` (bool, default `false`):
- **false:** Text in mid-turn responses is emitted as `turnevent.TextBlock{Phase: Intermediate}` events immediately. The user sees text as it's generated, even if more tool calls follow.
- **true:** Text is accumulated across all responses in the turn chain and carried on `turnevent.TurnComplete.FinalText` when the turn completes (end_turn with no more tool calls).

Both system-triggered turns (async_notify) and Telegram-triggered turns support deferred replies. Both paths attach a `turnevent.Sink` to the turn context (via `turn.NewStreamingSink` for interactive platforms, `turn.NewSessionSink` for async_notify) so intermediate text events route to the platform during the turn.

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
- Creates a branch session: `{parentKey}/b{TIMESTAMP}`
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

Enables extended thinking (Opus 4.6). In adaptive mode, the model decides when and how much to think. Thinking blocks are interleaved between tool calls. Thinking content is preserved in session history. Thinking tokens count toward token usage — opt-in per agent.

Configurable globally and per-agent. Runtime toggle: `/thinking adaptive` or `/thinking off`.

#### Showing thinking in Telegram

By default, thinking blocks are stripped from Telegram messages. The `show_thinking` config controls visibility:

- `"off"` (default) — thinking stripped, not shown to user
- `"compact"` — response sent with a "Show thinking" toggle button
- `"true"` — thinking always prepended to response (italic), separated by a divider

The `display_width` config (default 32) controls the character width of divider lines used in thinking display.

Configurable globally and per-agent via `show_thinking`. The `display_width` config controls divider line width.

Valid levels: `"low"`, `"medium"`, `"high"`. Empty = omit from request (API default). The `/effort` command shows or changes the level for the current session (runtime only, not persisted to config).

### Coding Agent Backends (TurnContract)

Foci supports two turn-handling paths, selected per-agent via the `backend` config:

- **API path (`APITransport`)** — Foci calls the LLM API directly and executes tools locally. The traditional path.
- **Delegated path (`DelegatedTransport`)** — A coding agent (Claude Code) runs as a subprocess, handling inference and tool execution. Foci sends composed prompts via stdin and receives streaming JSONL events via stdout (ccstream backend), or via a tmux pane with JSONL session file watcher (cctmux backend).

Both implement the `TurnContract` interface (`internal/agent/turn_contract.go`) — 19 methods covering every concern of a turn (rate limiting, session registration, prompt composition, execution, saving, compaction, etc.). Adding a new concern requires adding a method to the interface, producing compile errors in both transports until implemented.

The orchestrator (`OrchestrateFullTurn` in `turn_orchestrator.go`) calls all 19 methods in a fixed order. Each transport provides real implementations or explicit no-ops. Six methods are shared via `sharedTurnOps` embedding.

The sync/async split is handled by `TurnState.CompletionChan`: API closes it synchronously; delegated closes it when the backend fires `OnTurnComplete` (ccstream: on `result` message; cctmux: on `end_turn` in JSONL). Post-turn methods (save, metadata, compaction, logging) run after `CompletionChan` closes, with an activity-based timeout (2 minutes of stream silence) rather than a fixed deadline.

**RunOnce mode:** `DelegatedManager.RunOnce(ctx, prompt, systemPrompt)` runs `claude --print` synchronously for headless tasks (nudge extraction, consolidation). No tmux, no watcher — one-shot subprocess with stdout capture.

**Session lifecycle:**
- **Session ID persistence:** CC session UUID is persisted on discovery. On restart, `--resume <sessionID>` reconnects to the existing session.
- **Branch rejection:** Delegated agents return HTTP 400 for `/branch`. Three strategies by task type: inject into main session (reflection, compaction-memory), spawn independent `RunOnce` process (consolidation, background, nudge extraction), or reject (HTTP endpoint).
- **/reset:** Sends memory formation prompt, waits for completion, kills tmux pane, starts fresh CC session. `/reset hard` cancels the in-flight turn, skips memory formation, and destroys the backend without saving — used to recover from stuck turns.
- **/stop:** Sends Escape×2 + Ctrl-C to the CC TUI to interrupt the current turn.
- **Stable exec bridge sockets:** Socket path derived from session key (not random), so CC keeps the same `FOCI_SOCK` path across foci restarts.

See [WIRING.md — The Agent Loop](WIRING.md) for the full phase-by-phase breakdown.

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
- `send_to_chat` — send proactive Telegram messages and media. `send_as` parameter controls file type: `"document"` (default), `"voice"`, `"video"`, `"photo"`, `"audio"`, `"animation"` (GIF). With `send_as="voice"` and text (no file_path), synthesizes speech via TTS and sends as a voice note.
- `send_to_session` — inject a message into another session (cross-session communication). `reply_to` param: `"caller"` (default) routes response back to calling session, `"session"` sends response to the target session's own Telegram chat
- `todo` — manage a per-agent task list (add, list, complete, remove) with priority ordering
- `bitwarden_search` — search Bitwarden vault items by name/URI/folder (metadata only, no passwords)
- `bitwarden_unlock` — unlock a vault item (requires admin approval via aisudo/Telegram), caches for TTL
- `http_request` — HTTP client with file saves, binary handling, multipart uploads, and auto-background support
- `task_list` — manage cross-session task tracking
- `ask` — ask the user a question with structured options (backend-agnostic, async, no 4-item cap)
- `browser` — browser automation via go-rod (navigate, click, read, screenshot)

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
- `foci_send_to_chat <text>` (also reads stdin when no args)
- `foci_spawn <prompt> [--model M] [--context C]`

**Example:** `foci_web_search "golang generics" | head -3 | foci_send_to_chat`

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

Notifications go to agents whose `inject_agent_warnings` is disabled. Dedup prevents spam: same threshold only fires once until memory drops below it or tmux is killed.

### Warning Injection

Two independent mechanisms deliver log warnings to agents and users:

**Agent session injection** (`inject_agent_warnings`): WARN/ERROR log events are pushed into the agent's `WarningQueue` and surfaced in two ways:

- **Passive:** warnings are drained and prepended to the next user message as `[system warnings]` blocks. This is the default path — warnings piggyback on existing interaction.
- **Proactive:** the keepalive runner checks `WarningQueue.Pending()` every 30s and, if warnings are waiting, injects them as a `[proactive system warnings]` user message that triggers a full agent turn. Rate limited by user activity: 1 per `warning_proactive_active_interval` (default 5m) if the user is active, 1 per `warning_proactive_inactive_interval` (default 1h) if inactive. Activity is determined by `LastUserMessageTime()` vs `warning_proactive_activity_threshold` (default 10m). The agent response is delivered to Telegram.

**Chat notifications** (`inject_chat_warnings`): log events are pushed into a separate `ChatWarningQueue` and dispatched as platform notifications (Telegram messages) directly to the user. Uses the same rate-limiting intervals as agent injection. The two systems are independent — both can be enabled simultaneously.

Both fields accept `"all"` (WARN+ERROR), `"errors"` (ERROR only), or `"off"` (disabled). Severity filtering happens at push time — queues configured with `"errors"` silently drop WARN-level entries.

Proactive dispatch ensures critical warnings (disk full, tmux OOM) reach the agent and/or user immediately rather than sitting unnoticed until the next user message.

### System Memory Guard

Background goroutine monitoring total RSS of all processes owned by the foci system user. Reads `/proc/[pid]/status` directly — no external commands.

Two thresholds (configurable as `%` of RAM):
- **warn** (default 25%) — log WARN, inject warning to agent session via `WarningQueue` (surfaces via proactive warning dispatch)
- **kill** (default 40%) — find largest non-foci process by RSS (excluding `os.Getpid()`), SIGTERM, wait 5s, SIGKILL if needed

Both thresholds require **memory pressure** (PSI `avg10` > configurable threshold, default 10.0) via `/proc/pressure/memory`. This prevents false alarms when the system has plenty of free RAM — high RSS alone doesn't indicate a problem if there's no actual pressure.

Warn dedup: fires once per threshold crossing, resets when RSS drops below warn threshold.

Entire feature is disableable via `memory_guard_enabled = false`.

### Tool Result Guard

When a tool returns a result exceeding a configurable character threshold (default: 15,000 chars), foci does NOT inject the full result into session history. Instead:

1. Write the full result to a temp file: `{temp_dir}/tool-result-{tool}-{random}.txt`
2. Return only a guard message — no partial content is included:
   ```
   Result too large (47231 chars, limit 15000). Full output saved to /tmp/foci/tool-results/tool-result-shell-a1b2c3d4.txt.
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
- `autogenerated` (optional) — `true` marks a skill drafted by the reflection pass that still needs human review; the SystemBlock appends a "review and refine if used" marker and loader logs a warning

**How it works:**
1. On startup, resolve two skill directories: shared (`$home/shared/skills/`) and per-agent (`$workspace/skills/`). Both are configurable via `[skills] dir` and per-agent `skills_dir`.
2. Scan each dir for subdirectories containing `SKILL.md`. Per-agent skills override shared skills on name collision.
3. Parse frontmatter, collect name + description into a registry
4. Inject skill list (name, description, SKILL.md path) as a system prompt block — the agent knows what's available but doesn't load full instructions until needed
5. The agent reads the full `SKILL.md` with the `read` tool when it decides a skill applies
6. If `command` + `script` are both present, auto-register as a slash command (runs the script directly, no agent turn)

Skills are not dynamic plugins — no code loading, no compilation. Just directories of files the agent can read, with optional shell scripts for slash commands.

**Skill autogeneration.** The periodic reflection pass (`shared/prompts/reflection.md`) instructs the agent to turn replayable workflows into new skills under `workspace/skills/` when it notices 5+ tool calls, error recovery, a user correction, or a non-obvious sequence. New skills are written with `autogenerated: true` in their frontmatter; a human removes the line once the skill has been reviewed. The reflection pass also encourages pairing a `script.sh`/`script.py` with SKILL.md when the workflow has a deterministic core.

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

**Search:** Pluggable search backend — bleve (default) or fts5, selected via `search_backend` (exactly one per index).

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

**Bleve backend** — blevesearch/bleve full-text index. Files and conversation history. English analyzer with Porter stemming, per-source weighted ranking, highlighted snippets. Index stored at `{data_dir}/memory.bleve`. Clean rebuild on each reindex (close → remove → recreate). Conversation messages are indexed in real time via hook and backfilled from SQLite on startup.

The active backend is set by `search_backend` (default: `bleve`); exactly one backend runs per index.

**Indexing and Auto-Reindex:**

- Memory files: re-indexed on startup
- File watching: optional auto-reindex when `.md` files change via fsnotify
- Debounce delay is configurable (default: immediate).
- Conversation history: indexed as messages are logged (both backends). On startup both backends backfill historical conversation from the conversation SQLite DB — bleve per-message (deduped by doc ID), FTS5 via a one-time wipe+rebuild guarded by a marker table.

**Why FTS5 over vector embeddings:**
- Zero dependencies (built into SQLite, which we already use)
- Instant queries, no API calls
- Deterministic, debuggable
- Covers 90% of memory recall — you usually remember roughly what you wrote

**Why bleve (the default):**
- Richer analysis pipeline (built-in English analyzer, term vectors, highlighting)
- Query-string relevance is forgiving of natural-language queries (OR + rank), vs FTS5's stricter implicit-AND
- Pure Go, no CGo — though moot here since foci already links SQLite elsewhere

**Maybe later:** Vector embeddings for semantic search when keyword search proves insufficient.

## Scheduling & Background

### Scheduled Wakes

**HTTP endpoint (for cron jobs):**
```
POST /wake
{"agent": "main", "text": "morning routine", "no_compact": true}
```
Injects text as a user message into a branch session. When `no_compact` is true, the session returns its result instead of triggering compaction if the context limit is reached — useful for cron jobs that inherit a large parent context and shouldn't waste tokens compacting.

**Tool-based scheduling:**
The `remind` tool with `wake=true` allows the agent to schedule messages to itself:
- `remind(text="check status", when="30m", wake=true)` — wake after a duration
- `remind(text="meeting", when="2026-02-21T15:30:00Z", wake=true)` — wake at ISO timestamp
- One-shot, auto-cleaned after firing
- Useful for self-reminders, follow-ups, or timed actions

System crontab can trigger `/wake` endpoint for external scheduling. For agent-initiated delays, use the `remind` tool with `wake=true`.

### Activity gating

Two domains of activity are tracked separately, each with its own pair of gate fields. `POST /send`, `POST /wake`, and `POST /webhook/...` all accept all four:

**Session-level activity** — did THIS session run a turn within the window?

- `if_active` (Go duration, e.g. `"8h"`) — skip unless the session ran a turn within the window ("skipped: no recent activity")
- `if_inactive` (Go duration, e.g. `"30m"`) — skip if the session ran a turn within the window ("skipped: session recently active")

The timestamp is written by `OrchestrateFullTurn` for *every* turn-init path (user inbound, cron, CLI, webhook, agent-to-agent, system-injected) and stored under `session_metadata` keyed by the ROOT session key (branch turns record against their parent root). Use these for keepalive-shaped jobs that should yield to anything currently running on the session.

**User-attention activity** — did the user themselves reach out within the window?

- `if_user_active` (Go duration) — skip unless the user touched this agent within the window ("skipped: no recent user activity")
- `if_user_inactive` (Go duration) — skip if the user was active within the window ("skipped: user recently active")

"User activity" means messages from allowed users via the primary platform (Telegram or Discord). It explicitly excludes: CLI-injected messages (`foci send` / `foci branch`), async notifications, agent-to-agent messages, and system-injected messages. The timestamp is stored per-agent at `agent_metadata.last_user_activity`. Use these for nudges that should only fire when the user is engaged (or specifically away).

**In-flight short-circuit (TODO #753).** A turn currently executing on the target session counts as "active" for *both* gate pairs. So `--if-inactive 30m` will skip while a turn is in flight even if no recent timestamp has been written, and `--if-user-active 1h` will pass during an in-flight turn even if the user has been silent for hours. The principle: never queue a duplicate when something is already running. Without this, keepalive crons would pile up behind a long turn, fire serially as it completes, and waste tokens.

Evaluation order when multiple gates are set: `if_user_active` → `if_user_inactive` → `if_active` → `if_inactive`. The first applicable skip wins; the JSON response body identifies which one fired.

The CLI exposes all four as `--if-active`, `--if-inactive`, `--if-user-active`, `--if-user-inactive` on `send` and `branch`/`wake` commands, with corresponding `FOCI_IF_*` env vars. See [docs/CLI.md](docs/CLI.md) for full CLI reference.

### Keepalive & Background Work

Four timer-driven mechanisms run on a ~30s tick loop per agent:

**Keepalive** — Cache keepalive. Fires when `time_since(lastCacheWarmed) >= keepalive.interval`. Creates a lightweight branch session with `no_compact` to keep the Anthropic cache prefix warm. Does no real work.

**Background work** — Gated task execution. Fires when:
1. User has been idle for `background.interval`
2. Open todos tagged "background" exist
3. The `can_run_background` gate allows it (or is unset)

Creates a branch session that picks up the highest-priority background todo item.

**Memory formation** — Periodic memory capture. Fires when `interval` (default 1h) has elapsed since last formation and user activity occurred within that window. Captures conversation memories to daily files.

**Memory consolidation** — MEMORY.md curation. Fires when `[maintenance].consolidation_time` is due — a daily `"HH:MM"` clock time or an elapsed duration (default `"20h"`) — and the user was active within the last hour. Reviews daily memory files and curates MEMORY.md. Last-run timestamp persisted in state store.

**Scheduled reset** — `[maintenance].reset_time` (default off) fires a daily soft `/reset` (memory formation + key rotation) at an `"HH:MM"` clock time or duration, skipped if the user was active within `reset_idle_guard` (default `"55m"`).

**`can_run_background` gate** — Optional user-provided executable (`can_run_background` under `[background]`/`[agents.background]`). Run before each background operation via `procx.Spawn` with a 10s timeout and `FOCI_SESSION_KEY`/`FOCI_AGENT_ID`/`FOCI_ENDPOINT` in the environment. Exit 0 means background work is allowed; any non-zero exit skips the tick; unset means always allowed. A script that fails to execute is treated as allowed (logged as a warning) so it can't wedge all background work. The real-429 `RateLimitGate` still gates background work independently.

Config: `[keepalive]`, `[background]`, and `[reflection]` sections. See [docs/HEARTBEAT.md](docs/HEARTBEAT.md) for full details.

## Secrets

Secrets never pass through agent context. The agent cannot read, echo, or exfiltrate credentials. See [docs/SECRETS.md](docs/SECRETS.md) for the full security model, OS-level protection, domain locking, Bitwarden integration, and setup instructions.

### Principle
Credentials are loaded once at startup into process memory. Built-in integrations (Anthropic, Telegram, Brave Search) use them directly from Go structs. The agent interacts with tools, tools use credentials internally — the agent never constructs auth headers or sees token values.

### Per-agent secrets

Secrets in `secrets.toml` are global by default. Agents can have their own overrides via `[agents.ID]` sections.

Resolution order: agent-specific value wins over global. Keys not overridden in the agent section fall back to globals. Each agent only sees its own overrides — agent A cannot see agent B's secrets. Built-in credential resolution (anthropic.api_key, telegram, brave) stays global (process-wide); per-agent scoping applies to tool-visible secrets (shell templates, http_request, redaction, system prompt secret names).

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
func (a *Agent) OrchestrateFullTurn(ctx context.Context, msg string) error {
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

`/reset` refuses when the agent is mid-turn, preventing accidental data loss. The escape hatch is `/reset hard`, which cancels the live turn, skips memory formation, and destroys the backend — for recovering from stuck turns or when the user explicitly wants no memories saved. There are no automatic daily/idle session resets; sessions persist until explicitly reset by the user or the process restarts.

**Session-end reflection:** Before clearing the session, the reflection pass fires asynchronously — creating a branch from the expiring session to preserve conversation history. Configured via `[reflection]` section (`session_end_enabled`, `session_end_prompt`). The branch has a 120-second timeout and is non-fatal — if it fails, the reset has already proceeded. Branch sessions can opt out via `NoResetHook` in their branch metadata. The same hook fires on facet TTL reclaim.

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
{"ts":"2026-02-21T03:52:41Z","session":"main/i0/0","model":"claude-haiku-4-5","input":1119,"output":164,"cache_read":0,"cache_write":1119,"cost_usd":0.003,"duration_ms":1240,"call_type":"conversation"}
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
- `/sessions index [type] [status]` — query the session metadata index (all agents). Optional filters: type (chat/facet/spawn/cron/branch), status (active/compacted/cleared). Shows session key, type, status, created time, and parent session.

**Agents:**
- `/agents` - list active agent sessions with status, model, and message counts
- `/agents new` - interactive wizard for creating a new agent. Walks through: agent ID, display name, emoji, model, bot token secret, character file mode. Creates workspace, appends config to foci.toml, adds crontab entries. Requires restart to activate.

**System:**
- `/version` - binary version, go version, build time, git commit
- `/uptime` - process uptime, system load, memory usage

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
Confirmed working — wake/cron branches read ~63k cached tokens on first request with zero cold starts. Facet branches also share parent cache prefix.
6. Send another request on parent → observe parent cache still works (branch didn't bust it)

If step 5 shows a full cache write instead of a read, the branching architecture doesn't work and we need to rethink.

## Dependencies (Go)

Minimal:
- `github.com/BurntSushi/toml` — config parsing
- `github.com/go-telegram-bot-api/telegram-bot-api/v5` — Telegram (or hand-roll, it's just HTTP)
- Standard library for everything else (net/http, encoding/json, os/exec, etc.)

## Setup (`make setup`)

Idempotent. Run it once to install, run it again to update. Safe to re-run.

### What it does

1. **System user:** Create `foci` user if it doesn't exist (no login shell, home at `/home/foci`)
2. **Binaries:** Build from source (`go build`). Install `foci-gw`, `foci`, `foci-call`, and `foci-cc-hook` to `/usr/local/bin/`
3. **systemd service:** Install `/etc/systemd/system/foci.service` if it doesn't exist. `User=foci`, `WorkingDirectory=/home/foci`, restart on failure. Enable and start.
4. **Config:** Write `/home/foci/foci.toml` if it doesn't exist. Prompt interactively for:
   - Telegram bot token
   - LLM provider and API key (via `foci first-run` or `foci auth`)
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

### Update mode

`make update` rebuilds binaries, validates configs with the freshly-built binary before restarting, and restarts the service. When config already exists: don't touch it. When character files already exist: don't touch them. Idempotent means safe.

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
├── cmd/
│   └── foci-gw/
│       └── main.go          # entry point, wire everything together
├── internal/
│   ├── anthropic/           # API client, streaming, caching
│   │   ├── client.go
│   │   ├── types.go
│   │   └── cache_test.go   # THE critical test
│   ├── session/             # session store, branching
│   │   ├── store.go
│   │   ├── branch.go
│   │   └── store_test.go
│   ├── telegram/            # bot, message routing
│   │   └── bot.go
│   ├── tools/               # tool implementations
│   │   ├── registry.go
│   │   ├── shell.go
│   │   ├── files.go
│   │   ├── web.go
│   │   └── memory.go
│   ├── workspace/           # bootstrap file loading
│   │   └── bootstrap.go
│   ├── compaction/          # simple compaction
│   │   └── compact.go
│   └── config/              # TOML config loading
│       └── config.go
```

---

_This spec describes what we're building, not how to build it. Implementation decisions belong in the code._
