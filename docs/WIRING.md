# Clod — Wiring Diagram

How the pieces connect. Read this before touching the code.

## Startup Flow (`main.go`)

```
config.Load(path)                                        ← validates values; logs to stderr + buffer
  → log.Init(cfg.Logging)                                ← opens event file, replays buffered events
  → log.InitConversation(cfg.Logging.ConversationFile)   ← SQLite
  → secrets.Load(secretsPath)                            ← secrets.toml overrides clod.toml

  Shared resources (created once):
  → configDir = filepath.Dir(configPath)                  ← base for relative paths
  → cfg.DataPath(configDir, file)                         ← resolves DB paths via data_dir or configDir
  → anthropic.NewClient(token)
  → session.NewStore(dir)
  → sessions.RepairOrphans()                             ← fix interrupted tool calls before agents start
  → memory: ReminderStore + Scratchpad                   ← shared across agents
  → memory.NewIndex                                      ← shared OR per-agent (see below)
  → voice STT/TTS providers                              ← shared across agents
  → telegram.NewBotManager()

  Per-agent loop (for each cfg.Agents[i]):
  → setupAgent(params) → agentInstance{ag, cmds, registry, bootstrap, heartbeat}
    → tools.NewAsyncNotifier()                             ← shared by exec + tmux
    → tools.NewRegistry() + register all tools             ← per-agent registry
    → workspace.NewBootstrap(agent.Workspace, agent.SystemFiles)
    → buildEnvironmentBlock(acfg, configPath, cfg)           ← if [environment] enabled
    → skills.Load(cfg.Skills.Dirs)
    → compaction.NewCompactor(client, sessions, model, threshold)
    → agent.Agent{Client, Sessions, Tools, Bootstrap, EnvironmentBlock, ...}
    → command.NewRegistry() + register built-ins + custom scripts + skill commands
    → auto-expose all commands as tools
    → telegram.NewBot → botMgr.AddPrimary(agentID, bot)
    → optional: multiball bot → botMgr.AddMultiball(agentID, mbBot)
    → agent.NewHeartbeat(agent, sessionKey, interval)

  → botMgr.StartAll(ctx)                                  ← starts all bots
  → start all heartbeats
  → http.Server{"/send", "/status", "/command", "/wake"}  ← routes by agent param
  → injectWelcomeFile()                                    ← setup.sh changelog injection
  → signal.Notify(SIGINT, SIGTERM) → shutdown
```

**Multi-agent:** Each agent gets its own tool registry, command registry, workspace bootstrap, compactor, heartbeat, and Telegram bot(s). Shared resources (anthropic client, session store, voice providers) are passed to each agent.

**Per-agent memory:** When any agent has `[[agents.memory.sources]]` configured, each agent gets its own FTS5 index (`memory-{agentID}.db`) combining global `[memory]` sources with agent-specific sources. Agent-specific sources receive a weight boost of +1.0. When no per-agent memory is configured, all agents share a single `memory.db` index (backward compat). Reminder and scratchpad stores are always shared.

**Agent routing:** `agentInstance` map keyed by agent ID. HTTP endpoints use `resolveAgent(id)` — returns first agent when ID is empty (backward compat).

## Package Dependency Graph

```
main
 ├── config        (no deps)
 ├── log           → modernc.org/sqlite
 ├── secrets       → BurntSushi/toml
 ├── anthropic     (no deps)
 ├── session       → anthropic
 ├── memory        → modernc.org/sqlite, fsnotify/v4 (file watching for auto-reindex)
 ├── voice         (no deps — uses net/http only)
 ├── skills        → log (leaf package)
 ├── tools         → anthropic, log, memory, secrets, voice
 ├── workspace     → anthropic
 ├── compaction    → anthropic, session, log
 ├── command       (no deps)
 ├── agent         → anthropic, compaction, session, tools, workspace, log
 └── telegram      → agent, command, log, voice
```

No circular dependencies. `config`, `log`, `secrets`, `memory`, `skills`, and `command` are leaf packages.

## The Agent Loop (`agent/agent.go`)

The core of the system. Two entry points:
- `HandleMessage(ctx, sessionKey, text)` — text-only, delegates to `HandleMessageWithImages`
- `HandleMessageWithImages(ctx, sessionKey, text, images)` — full version with optional image attachments

**Tool execution guarding:**
- After a tool executes, `guardToolResult()` checks if result exceeds `MaxResultChars`
- If exceeded, writes full result to temp file and returns truncated message
- Prevents large tool outputs from permanently bloating session history

```
1. sessions.LoadFull(sessionKey)          ← parent[:branchPoint] + own msgs
2. buildMetaPrefix() + prepend to user message text
3. build content blocks: image block(s) first, then text block (with metadata)
4. append user message
5. bootstrap.SystemBlocks()               ← workspace/*.md → []SystemBlock
   prepend EnvironmentBlock if set        ← runtime context block
   append ExtraSystemBlocks               ← skills, etc.
6. tools.ToolDefs()                       ← registry → []ToolDef
7. LOOP (max 25 iterations):
   a. logCacheDebug(system, messages, model)  ← warns if system < min threshold
   b. client.SendMessage(system, messages, tools)
   c. log event + log API entry
   d. if stop_reason == "end_turn" → save & check compaction & return text
   e. if stop_reason == "tool_use":
      - execute each tool via registry (check ctx.Err() between calls)
      - append assistant msg + tool_result msg
      - goto 7a
8. sessions.AppendAll(sessionKey, newMessages)
9. if compactor.ShouldCompact(messages, usage) → compactor.Compact(sessionKey)
```

Messages are only saved to disk after the full turn completes (all tool loops resolved). Compaction runs after save, replacing the session with a 3-message summary if the context exceeds the threshold (default 80% of 200k).

### Cache Stability Invariant

**The conversation history sent to the Anthropic API MUST be a strict append-only extension of the previous request.** New messages must only ever appear at the end — never inserted in the middle.

Anthropic's prompt cache is prefix-matched. If any message shifts position (because an injected message was inserted before it), all cached tokens after that point are invalidated. A single cache bust can cost $1+ in re-tokenization.

**Per-session turn lock:** `HandleMessageWithImages` acquires a per-session mutex (`turnLock(sessionKey)`) before doing any work. This serializes all turns on the same session — concurrent callers (heartbeat, `AsyncNotifier`, scheduled wakes, HTTP `/send`) wait until the current turn completes. Each turn loads the full session history (including messages saved by the previous turn), processes, and saves — guaranteeing strict append-only ordering.

**Concurrent callers that are serialized by the turn lock:**
- Telegram bot worker (user messages)
- Heartbeat goroutine (`[HEARTBEAT]`)
- `AsyncNotifier` (`[TMUX WATCH]` inactivity, `[EXEC RESULT]` auto-background completion)
- Scheduled wakes (`[SCHEDULED WAKE]`, fires from `go func()`)
- HTTP `/send` endpoint

**Different sessions run concurrently** — the lock is per-session, not global. Branch sessions and parent sessions have different keys and do not block each other.

## Message Metadata

Before metadata is added, **prompt rules** (`[[prompt_rules]]` in config) run regex find/replace on the raw user message. Rules run in sequence. This happens before duplication (`DuplicateMessages`), before metadata prefix, and after STT transcription.

Each user message then gets a metadata line prepended (NOT in system prompt — that would bust cache):

```
[meta] time=2026-02-21T05:30:00Z gap=3h12m model=claude-haiku-4-5 prev_cost=$0.0430 prev_tokens=in:2400/out:312/cR:18000/cW:200
```

- `time` — current UTC timestamp
- `gap` — human-readable time since previous message ("3h12m", "2d4h", "38s", "none")
- `model` — current model name (e.g., "claude-haiku-4-5", "claude-opus-4-6")
- `prev_cost` / `prev_tokens` — cost and token breakdown of the previous turn (omitted on first message)

Per-session state is tracked in `sessionMeta` (in-memory map on Agent). The metadata goes past the cache breakpoint, so it doesn't affect prompt caching.

## Deferred Replies

When the model responds with text alongside `tool_use` blocks (e.g., "Looking into this..."), the text is sent immediately via `ReplyFunc` before tool execution begins. This allows the agent to acknowledge a message and deliver the full response later.

**Flow:**
1. Telegram bot creates a `TurnCallbacks` struct and attaches it to the turn context via `agent.WithTurnCallbacks(ctx, cb)`
2. Agent loop detects text in a `tool_use` response
3. `sendIntermediateCtx(ctx)` extracts the ReplyFunc from context and calls it
4. Agent continues executing tools
5. Final `end_turn` response is returned from `HandleMessage` as usual

Callbacks are **context-scoped**, not agent-global. Each turn gets its own isolated callbacks. Async callers (heartbeat, tmux watch, exec auto-background, scheduled wakes) that don't set callbacks get nil — no Telegram side effects. No cross-turn state corruption.

## Tool Call Visibility

Tool calls are shown in Telegram via a send+edit pattern using `ToolCallObserver`. The first tool call in a turn sends a new message; subsequent tool calls edit that same message. The final response then edits the tool message with the answer (or falls back to a new message if too long). Both `ToolCallObserver` and `ReplyFunc` are part of the context-scoped `TurnCallbacks` struct — per-turn, not agent-global.

**Ordering with deferred replies:** When intermediate text fires between tool loops, `ReplyFunc` resets `toolMsgID` to 0. This forces the next tool call to create a fresh message below the text, preserving chronological order in chat.

**Flow (multi-loop turn):**
1. Loop 1: API returns `[tool_use(exec)]` — `notifyToolCall` sends message A (`toolMsgID=A`)
2. Loop 2: API returns `[text("Checking..."), tool_use(read)]`
   - `sendIntermediate` fires `ReplyFunc` → sends message B, resets `toolMsgID=0`
   - `notifyToolCall` sends message C (`toolMsgID=C`, fresh because reset)
3. Final: `end_turn` response edits message C with the answer

**Chat order:** A ("🔧 exec") → B ("Checking...") → C ("🔧 read" → final answer) ✓

## Thought Queue (Reminders)

The agent can defer thoughts for later via the `memory_remind` tool. Reminders are stored in SQLite (`reminders.db`) and surfaced as injected context when due.

**Storage:** `ReminderStore` in `memory/remind.go`. Table `reminders` with columns: `id`, `text`, `due_at`, `due_tag`, `created`.

**Time resolution (`resolveWhen`):**
- `next_heartbeat`, `next_session`, `now` → immediate
- `tomorrow` → midnight tomorrow UTC
- `YYYY-MM-DD` → that date at midnight UTC
- Go duration (e.g., `2h`, `30m`) → now + duration

**Injection:** At the start of each `HandleMessage`, `collectReminders()` checks for due reminders. If any exist, they're appended to the metadata line as a `[reminders]` block in the user message (past the cache breakpoint, so caching is unaffected). Due reminders are auto-dismissed after surfacing.

**Example injected message:**
```
[meta] time=2026-02-21T05:30:00Z gap=45m0s
[reminders]
- Look into FTS5 phrase boosting (set next_heartbeat, due: 2026-02-21 05:00)
Hello, what should I work on?
```

## Scratchpad

Working state that survives compaction but isn't permanent memory. The agent writes notes during investigations and clears them when done.

**Storage:** `Scratchpad` in `memory/scratchpad.go`. SQLite table `scratchpad` with columns: `key` (primary key), `content`, `updated`. Stored in `scratchpad.db`.

**Tools:** `scratchpad_write(key, content)`, `scratchpad_read(key)`, `scratchpad_clear(key)`.

**Compaction survival:** When compaction fires (`compaction/compact.go`), all scratchpad entries are serialized and appended to the post-compaction handoff message as a `[scratchpad]` block. This prevents compaction from eating working state mid-investigation.

**Example post-compaction message:**
```
[Compaction complete. The conversation continues from here. You have full access to your tools and memory.]

[scratchpad — working state preserved through compaction]
--- investigation ---
Checking whether FTS5 supports phrase boosting — preliminary answer is yes via NEAR queries.
--- debug_notes ---
The cache miss on branch sessions was caused by a trailing newline difference.
```

## Session Storage

**Format:** JSONL files, one JSON-encoded `anthropic.Message` per line.

**Key → Path mapping:**
```
agent:main:main           → {dir}/agent/main/main.jsonl
agent:main:cron:morning   → {dir}/agent/main/cron/morning.jsonl
```

**Branching:** Branch files start with a `{"type":"branch_meta",...}` line containing `parent_key` and `branch_point`. `LoadFull()` reads parent[:branch_point] + branch's own messages. This is what makes cache sharing work — the API sees the same prefix bytes.

## System Prompt Assembly (`workspace/bootstrap.go`, `agent/agent.go`)

System blocks are assembled in this order:

1. **Environment block** (`agent.EnvironmentBlock`) — programmatically built at startup from config values. Contains workspace path, agent ID, platform URL, messaging platform, config/log paths, message metadata docs, and session structure. Built by `buildEnvironmentBlock()` in `main.go`, stored as a string on the Agent struct, prepended as the first `SystemBlock` in `HandleMessageWithImages`. Omitted when `[environment] enabled = false` (empty string).

2. **Character files** (`workspace/bootstrap.go`) — reads markdown files from workspace dir in order:
```
IDENTITY.md → SOUL.md → COHERENCE.md → AGENTS.md → TOOLS.md → USER.md → MEMORY.md → HEARTBEAT.md
```

Each becomes a `SystemBlock{type:"text", text:content}`. Missing/empty files are silently skipped.

3. **Secrets block** — appended by `Bootstrap.SystemBlocks()` if secret names are available. Lists available `{{secret:NAME}}` template keys.

4. **Extra system blocks** — skills list and other injected blocks (`agent.ExtraSystemBlocks`).

The **last** block gets `cache_control: {type: "ephemeral"}`. Order matters: most-stable blocks first maximizes cache prefix reuse. The environment block is highly stable (only changes on restart), making it a good cache prefix leader.

## Anthropic API Client (`anthropic/`)

Two clients:

1. **MessageClient** (`client.go`) — messages API with prompt caching
   - Sends model requests with system prompt + conversation history
   - Sets `anthropic-beta: prompt-caching-2024-07-31` for cache control
   - OAuth tokens also include `oauth-2025-04-20` in beta header

2. **UsageClient** (`usage.go`) — OAuth usage API
   - Queries `/api/oauth/usage` endpoint
   - Requires OAuth token (`sk-ant-oat01-...`)
   - Returns utilization for 5-hour window, 7-day limits, extra usage billing

## Prompt Caching

Two cache breakpoints per API request:

1. **System prompt** — `cache_control: ephemeral` on the last `SystemBlock` (set by `bootstrap.SystemBlocks()`). Caches the entire system prompt so it's not re-tokenized each turn.

2. **Conversation history** — `cache_control: ephemeral` on the last content block of the second-to-last message (set by `withCacheBreakpoint()` in `agent.go`). Caches system prompt + conversation history up to the previous turn.

Cache breakpoints are added **only to the API request payload**, never persisted to session storage. The `withCacheBreakpoint()` function returns a shallow copy of the messages slice.

**Branch cache sharing:** When a branch session's `LoadFull()` builds a message list starting with the parent's prefix, the cache breakpoint lands on the same byte-identical prefix. The API hits cache (read pricing) instead of re-tokenizing (write pricing).

**Requirements for cache hits:**
- System prompt must be byte-identical across turns (workspace files don't change mid-conversation)
- `anthropic-beta: prompt-caching-2024-07-31` header (set in `client.go`)
- OAuth tokens also need `oauth-2025-04-20` in the beta header

**Verify in `api.jsonl`:** `cache_read > 0` on the second message in a session means caching is working.

## Secrets (`secrets/`)

Loaded from `secrets.toml` (same directory as `clod.toml`). Format:

```toml
[anthropic]
token = "sk-ant-oat01-..."

[telegram]
bot_token = "123:ABC"

[custom]
github_token = "ghp_..."
```

Stored as flat keys: `anthropic.token`, `custom.github_token`, etc. Overrides `clod.toml` credentials at startup.

Features:
- **Template resolution:** `{{secret:custom.github_token}}` in exec commands → replaced with actual value before execution
- **Output redaction:** Secret values in command output → `[REDACTED]` (skips values < 4 chars)
- **Path blocking:** Commands referencing `secrets.toml` or `/proc/self/environ` are refused

## Logging (`log/`)

**Two-phase init:** Before `log.Init()`, events go to stderr and are buffered in memory. When `Init()` opens the event file, buffered events are replayed to it. This ensures config-load warnings (e.g. unknown keys) appear in the log file despite being emitted before the file path is known.

Three outputs:

1. **Event log** (`clod.log` + stderr): `2026-02-21T03:52:39Z INFO  [telegram] message from rich: hello`
   - Use: `log.Infof("component", "format", args...)`
   - Levels: DEBUG < INFO < WARN < ERROR

2. **API log** (`api.jsonl`): One JSON object per Anthropic API call with ts, session, model, token counts, cost_usd, duration_ms.
   - Use: `log.API(log.APIEntry{...})`
   - Queryable with `jq`

3. **Conversation log** (`conversation.db`): SQLite database logging exact Telegram messages sent and received. Table `messages` with columns: `id`, `ts`, `direction` (recv/sent), `user_id`, `username`, `chat_id`, `text`, `parse_mode`, `session`, `error`.
   - Use: `log.Conversation(log.ConversationEntry{...})`
   - Queryable with `sqlite3 conversation.db "SELECT * FROM messages"`
   - Useful for debugging formatting (see exact markdown sent vs plain text fallback)

## Tool System (`tools/`)

Each tool is a `Tool` struct with `Execute func(ctx, params) (string, error)`. Registry maps name → tool. Tools available:

| Tool | File | What it does |
|------|------|-------------|
| `exec` | exec.go | Shell commands via `sh -c`, process group kill on timeout, secret template resolution + output redaction |
| `tmux` | tmux.go | Manage tmux sessions — start, send keys, read pane output, list, kill, watch for inactivity, unwatch. Owned sessions persist across app restarts via state store. |
| `read` | files.go | File contents with line numbers, truncates at 2000 lines |
| `write` | files.go | Create/overwrite files |
| `edit` | files.go | Find-and-replace (old_string must be unique) |
| `web_fetch` | web.go | HTTP GET, strip HTML tags |
| `web_search` | web.go | Brave Search API |
| `memory_search` | memory.go | FTS5 full-text search over memory files + conversation history (porter stemming, memory weighted 2x) |
| `memory_remind` | remind.go | Defer a thought for later; stored in SQLite, surfaced as injected context when due |
| `scratchpad_write` | scratchpad.go | Write working notes (key + content); survives compaction |
| `scratchpad_read` | scratchpad.go | Read a scratchpad entry by key |
| `scratchpad_clear` | scratchpad.go | Clear a scratchpad entry when done with it |
| `request_model` | model.go | Synchronous one-shot call to a different model. Sends prompt, returns response as tool result. Supports prompt weight: full (character files), light (minimal), none. Session's own model/cache unaffected. |
| `schedule_wake` | schedule.go | Schedule message injection at specified time or delay. One-shot, auto-cleaned after firing. |
| `tts` | voice.go | Convert text to speech via OpenRouter TTS API. Sends audio as Telegram voice note. Used when the agent wants to reply with voice explicitly. |

### Tool Result Guard

If a tool result exceeds `agent.MaxResultChars` (from config, default 10,000), the result is written to `agent.ToolResultTempDir` instead of injected directly. The agent receives a truncated message with the file path and read instructions. This prevents large results from bloating session history indefinitely.

## Slash Commands (`command/`)

Messages starting with `/` are intercepted at the Telegram router level before reaching the agent. They execute immediately — never queued behind an in-flight agent turn.

**Dispatch flow:** Telegram message → auth check → if `/`: `registry.Dispatch()` → execute → reply. Never touches agent session or message history.

**Commands exposed as tools:** All registered commands are automatically exposed to the agent as tools with the same name (without the `/` prefix). This allows the agent to invoke commands programmatically. Each command tool accepts an optional `args` string parameter. The tool wrapper converts the JSON params to command arguments and passes through the result or error. Naming collisions between tool names and command names cause a fatal startup error.

**Two types:**
1. **Built-in** (code-defined in `command/builtins.go`): `/ping`, `/status`, `/cache`, `/last`, `/cost`, `/usage`, `/reset`, `/reload`, `/model`, `/session`, `/tools`, `/config`, `/log`, `/errors`, `/version`, `/uptime`, `/voice`, `/multiball` (alias `/mb`)
   - `/usage` — check Claude subscription usage (requires OAuth token)
   - `/reload` — reload workspace files, skills, and system blocks from disk
2. **Custom** (script-defined in `clod.toml` via `[[commands]]`): runs a shell script, returns stdout. Timeout default 10s.

Commands use callbacks (closures) to access internal state, avoiding package dependencies on `session`, `agent`, etc.

## Config (`config/config.go`)

Single `clod.toml` parsed with BurntSushi/toml. Defaults applied for missing fields.

**Multi-agent config:** Two formats supported:

1. **Legacy (single agent):** `[agent]` table — backward compatible, auto-promoted to single-element `Agents` slice.
2. **Multi-agent:** `[[agents]]` array — each agent has its own `id`, `model`, `workspace`, `telegram_bot`, `multiball_bot`.

When both `[agent]` and `[[agents]]` are present, `[[agents]]` wins.

`cfg.Agent` always mirrors `cfg.Agents[0]` so legacy code paths work unchanged.

**Telegram bots config:** Two formats:

1. **Legacy:** `[telegram]` with `bot_token` and `secondary_bots` — single bot, tokens inline or in secrets.
2. **Multi-agent:** `[telegram.bots]` map of named bots. Each entry has `token_secret` referencing a key in `secrets.toml`. Agents reference bots by name via `telegram_bot` and `multiball_bot` fields.

**Token resolution:** `Config.ResolveBotToken(botName, secrets)` checks `[telegram.bots.<name>].token_secret` → secrets store first, then falls back to legacy `telegram.bot_token`.

**Example multi-agent config:**
```toml
[[agents]]
id = "clutch"
model = "claude-sonnet-4-6"
workspace = "/home/rich/workspace1"
telegram_bot = "primary"
multiball_bot = "secondary"

[[agents]]
id = "scout"
workspace = "/home/rich/workspace2"
telegram_bot = "scout"

[telegram]
allowed_users = ["5970082313"]

[telegram.bots]
primary = { token_secret = "telegram.primary" }
secondary = { token_secret = "telegram.secondary" }
scout = { token_secret = "telegram.scout" }
```

## Telegram Bot (`telegram/bot.go`)

Two goroutines:
```
[receiver goroutine]   →  receive msg  →  slash command?  →  yes: execute, reply
                                       →  voice note?     →  download OGG, transcribe via Whisper → text
                                       →  photo/doc?      →  download image via Telegram file API
                                                           →  enqueue (buffered chan) with text + images
[agent worker goroutine]  →  dequeue msg  →  create turn context  →  HandleMessage[WithImages]  →  reply
```

The receiver never blocks on the agent. Slash commands (including `/stop`) execute immediately on the receiver goroutine. Agent messages are processed sequentially by the worker.

**Image handling:** Photos (`msg.Photo`, largest size selected) and image documents (`msg.Document` with image MIME type) are downloaded via `GetFile()` + HTTP GET. The raw bytes are queued as `imageAttachment` structs alongside the message text (which may come from `msg.Caption` for photos). The agent worker converts these to `agent.ImageData` and calls `HandleMessageWithImages`.

**Turn cancellation:** Each agent turn gets its own `context.WithCancel`. `/stop` calls `turnCancel()`, which propagates to in-flight API calls (HTTP client context) and tool executions (process group kill). The agent loop checks `ctx.Err()` after API responses and between tool calls.

**Reset guard:** `/reset` refuses when `agent.IsProcessing()` is true — prevents clearing an active conversation mid-turn.

## Voice (`voice/`, `telegram/bot.go`)

**Inbound (Whisper transcription):**
```
Telegram voice note → downloadFile(voice.FileID) → voice.Transcriber.Transcribe()
  → Groq Whisper API (multipart/form-data, whisper-large-v3)
  → "[voice] transcript text" queued as regular message
```

API key from `secrets.toml` under `[groq] api_key`. Endpoint and model configurable in `[voice]` config section (defaults: `https://api.groq.com/openai/v1/audio/transcriptions`, `whisper-large-v3`).

**Outbound (TTS):**
Two paths:
1. **Voice mode** — session-level flag toggled via `/voice`. When on, all agent text replies are converted to voice notes via `voice.TTS.Synthesize()` before sending.
2. **TTS tool** — the agent can explicitly call `tts(text)` to send a voice note. Works regardless of voice mode.

```
voice.TTS.Synthesize(text) → OpenRouter TTS API (openai/tts-1-mini)
  → raw MP3 bytes → tgbotapi.NewVoice(chatID, FileBytes{mp3})
```

API key from `secrets.toml` under `[openrouter] api_key`. Endpoint/model/voice configurable in `[voice]` config section (defaults: `https://openrouter.ai/api/v1/audio/speech`, `openai/tts-1-mini`, `alloy`).

**Voice mode metadata:** When voice mode is on, the metadata prefix includes `voice=on`:
```
[meta] time=2026-02-21T05:30:00Z gap=3h12m voice=on model=claude-haiku-4-5
```

The agent sees this and adjusts its style (shorter, conversational, no markdown).

## Multiball (`telegram/pool.go`, `telegram/bot.go`)

Fork the current session to a secondary Telegram bot for parallel conversations. Each fork shares the parent's cache prefix.

**Config** (`secrets.toml`):
```toml
[telegram]
bot_token = "primary-bot-token"
secondary_bots = "token-1,token-2"   # comma-separated
```

**Flow:**
```
/multiball → sessions.CreateBranch(main, multiball:mb-TIMESTAMP)
           → pool.Acquire() → least-recently-used idle secondary bot
           → bot.SetSessionKey(branchKey)
           → bot.SendNotification("🎱 Forked from main.")
```

Messages to the secondary bot route to the forked session. `/done` on the secondary bot detaches it and returns it to the pool.

**Bot pool** (`telegram/pool.go`): Tracks secondary bots, acquires LRU idle bot, releases on `/done`.

**Bot changes** (`telegram/bot.go`):
- `SessionKey()` / `SetSessionKey()` — thread-safe mutable session key
- `isSecondary` flag — enables `/done` handling, idle message rejection
- `/done` handled as special case alongside `/stop` (bypasses command registry)
- Idle secondary bots respond with "This bot is idle. Use /multiball..." to non-command messages

**Special commands on secondary bots:**
- `/done` — detach from forked session, return to pool
- `/stop` — cancel current agent turn (same as primary)
- All other slash commands — shared registry (operate on main session's context)

## HTTP Gateway (`main.go`)

Endpoints for external integration (used by `clod` CLI). All endpoints accept an optional `agent` parameter (JSON body or query string) to target a specific agent. When omitted, defaults to the first configured agent (backward compat).

- `POST /send` — `{"agent": "clutch", "text": "..."}` — message to agent session
- `GET /status?agent=clutch` — dispatches `/status` for the specified agent
- `POST /command` — `{"agent": "clutch", "command": "/ping"}` — dispatches slash command
- `POST /wake` — `{"agent": "clutch", "text": "morning routine"}` — branch session for cron

## CLI Tool (`cmd/clod/`)

Separate binary (`go build ./cmd/clod`) for scripts, cron jobs, and external tools. Binary name: `clod`. Commands: `send`, `branch`, `status`, `eval`, `command`, `ping`. Talks to the HTTP gateway (`clodgw`) at `CLOD_ADDR` (default `127.0.0.1:18791`).

## Heartbeat & Wake

- **Heartbeat** (`agent/heartbeat.go`): Timer goroutine, fires after idle duration, injects `[HEARTBEAT]` message into main session. Resets on any activity.
- **HTTP Wake** (`POST /wake`): Creates a branch session from the agent's main session, injects the text, runs the agent on the branch.
- **Scheduled Wakes** (`schedule_wake` tool): Agent-initiated timer that fires message injection at specified delay or timestamp. One-shot, background goroutine, auto-cleaned after firing.

## Compaction (`compaction/compact.go`)

Checks token usage against threshold (default 80% of context window). When triggered:
1. Asks model (configurable) to summarize history using configurable prompt
2. Replaces session with 3-message compacted version (context note + summary + continuation note)
3. Appends any scratchpad entries to preservation message

**Configurable via `Compactor.WithConfig()`:**
- `model` — summarization model (default: agent model)
- `maxTokens` — max output tokens for summary (default: 4096)
- `minMessages` — min messages before compacting (default: 4)

**Passed to `Compact()` at call time** (not stored on the Compactor):
- `summaryPrompt` — custom summary prompt (empty uses `DefaultSummaryPrompt`)
- `handoffMessage` — message after compaction completes (empty uses `DefaultHandoffMessage`)

## Deployment & Migrations

### setup.sh

`/home/rich/git/clod/setup.sh -u clod` — builds Go binaries, installs to `/usr/local/bin`, restarts service. Allowlisted in aisudo (no approval needed). Uses `--no-block` restart to avoid deadlock when run from clod's own exec.

### Migrations

Numbered scripts in `migrations/` (e.g. `001-homedir-restructure.sh`). Run manually during deploys that require filesystem or config changes beyond what the binary handles.

**Convention:**
- Scripts are idempotent (safe to run twice)
- Include `--dry-run` and `-h`/`--help`
- Must run while clod is stopped (script handles stop/start)
- Require root (`sudo`) for service control and file ownership

**Planned integration:** `setup.sh` will check for and run pending migrations between building binaries and restarting the service. A state file tracks which migrations have been applied.

**Current migrations:**
- `001-homedir-restructure.sh` — Moves flat home dir into `config/`, `data/`, `logs/`, `shared/` layout. Updates clod.toml paths, systemd unit, and crontab.

## Testing

```
go test ./...           # all tests (~66, runs in ~1s)
go test ./... -v        # verbose
go test ./session/...   # single package
```

The cache_test.go in `anthropic/` requires `ANTHROPIC_API_KEY` env var and hits the real API. All other tests are self-contained.
