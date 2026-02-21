# Clod — Wiring Diagram

How the pieces connect. Read this before touching the code.

## Startup Flow (`main.go`)

```
config.Load(path)
  → log.Init(cfg.Logging)
  → log.InitConversation(cfg.Logging.ConversationFile)  ← SQLite
  → secrets.Load(secretsPath)                            ← secrets.toml overrides clod.toml
  → anthropic.NewClient(token)
  → session.NewStore(dir)
  → tools.NewRegistry() + register all tools             ← exec gets secrets.Store
  → workspace.NewBootstrap(dir, fileOrder)
  → compaction.NewCompactor(client, sessions, model, threshold)
  → agent.Agent{Client, Sessions, Tools, Bootstrap, Compactor, Model}
  → command.NewRegistry() + register built-ins + custom scripts
  → telegram.NewBot(token, allowedUsers, agent, cmds, sessionKey)  → goroutine
  → agent.NewHeartbeat(agent, sessionKey, interval)           → goroutine
  → http.Server{"/send", "/status", "/command", "/wake"}      → goroutine
  → signal.Notify(SIGINT, SIGTERM) → shutdown
```

## Package Dependency Graph

```
main
 ├── config        (no deps)
 ├── log           → modernc.org/sqlite
 ├── secrets       → BurntSushi/toml
 ├── anthropic     (no deps)
 ├── session       → anthropic
 ├── memory        → modernc.org/sqlite
 ├── tools         → anthropic, log, memory, secrets
 ├── workspace     → anthropic
 ├── compaction    → anthropic, session, log
 ├── command       (no deps)
 ├── agent         → anthropic, compaction, session, tools, workspace, log
 └── telegram      → agent, command, log
```

No circular dependencies. `config`, `log`, `secrets`, `memory`, and `command` are leaf packages.

## The Agent Loop (`agent/agent.go`)

The core of the system. `HandleMessage(ctx, sessionKey, userMessage)`:

```
1. sessions.LoadFull(sessionKey)          ← parent[:branchPoint] + own msgs
2. buildMetaPrefix() + prepend to user message text
3. append user message (with metadata)
4. bootstrap.SystemBlocks()               ← workspace/*.md → []SystemBlock
4. tools.ToolDefs()                       ← registry → []ToolDef
5. LOOP (max 25 iterations):
   a. logCacheDebug(system, messages, model)  ← warns if system < min threshold
   b. client.SendMessage(system, messages, tools)
   c. log event + log API entry
   d. if stop_reason == "end_turn" → save & check compaction & return text
   e. if stop_reason == "tool_use":
      - execute each tool via registry (check ctx.Err() between calls)
      - append assistant msg + tool_result msg
      - goto 5a
6. sessions.AppendAll(sessionKey, newMessages)
7. if compactor.ShouldCompact(messages, usage) → compactor.Compact(sessionKey)
```

Messages are only saved to disk after the full turn completes (all tool loops resolved). Compaction runs after save, replacing the session with a 3-message summary if the context exceeds the threshold (default 80% of 200k).

## Message Metadata

Each user message gets a metadata line prepended (NOT in system prompt — that would bust cache):

```
[meta] time=2026-02-21T05:30:00Z gap=3h12m prev_cost=$0.0430 prev_tokens=in:2400/out:312/cR:18000/cW:200
```

- `time` — current UTC timestamp
- `gap` — human-readable time since previous message ("3h12m", "2d4h", "38s", "none")
- `prev_cost` / `prev_tokens` — cost and token breakdown of the previous turn (omitted on first message)

Per-session state is tracked in `sessionMeta` (in-memory map on Agent). The metadata goes past the cache breakpoint, so it doesn't affect prompt caching.

## Deferred Replies

When the model responds with text alongside `tool_use` blocks (e.g., "Looking into this..."), the text is sent immediately via `ReplyFunc` before tool execution begins. This allows the agent to acknowledge a message and deliver the full response later.

**Flow:**
1. Telegram bot sets `agent.SetReplyFunc()` before calling `HandleMessage`
2. Agent loop detects text in a `tool_use` response
3. `sendIntermediate()` calls the ReplyFunc, which sends the text to Telegram
4. Agent continues executing tools
5. Final `end_turn` response is returned from `HandleMessage` as usual

The `ReplyFunc` is cleared after each turn via `defer agent.SetReplyFunc(nil)`.

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

## Session Storage

**Format:** JSONL files, one JSON-encoded `anthropic.Message` per line.

**Key → Path mapping:**
```
agent:main:main           → {dir}/agent/main/main.jsonl
agent:main:cron:morning   → {dir}/agent/main/cron/morning.jsonl
```

**Branching:** Branch files start with a `{"type":"branch_meta",...}` line containing `parent_key` and `branch_point`. `LoadFull()` reads parent[:branch_point] + branch's own messages. This is what makes cache sharing work — the API sees the same prefix bytes.

## System Prompt Assembly (`workspace/bootstrap.go`)

Reads markdown files from workspace dir in order:
```
IDENTITY.md → SOUL.md → COHERENCE.md → AGENTS.md → TOOLS.md → USER.md → MEMORY.md → HEARTBEAT.md
```

Each becomes a `SystemBlock{type:"text", text:content}`. The **last** block gets `cache_control: {type: "ephemeral"}`. Order matters: most-stable files first maximizes cache prefix reuse.

Missing/empty files are silently skipped.

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
| `read` | files.go | File contents with line numbers, truncates at 2000 lines |
| `write` | files.go | Create/overwrite files |
| `edit` | files.go | Find-and-replace (old_string must be unique) |
| `web_fetch` | web.go | HTTP GET, strip HTML tags |
| `web_search` | web.go | Brave Search API |
| `memory_search` | memory.go | FTS5 full-text search over memory files + conversation history (porter stemming, memory weighted 2x) |
| `memory_remind` | remind.go | Defer a thought for later; stored in SQLite, surfaced as injected context when due |

## Slash Commands (`command/`)

Messages starting with `/` are intercepted at the Telegram router level before reaching the agent. They execute immediately — never queued behind an in-flight agent turn.

**Dispatch flow:** Telegram message → auth check → if `/`: `registry.Dispatch()` → execute → reply. Never touches agent session or message history.

**Two types:**
1. **Built-in** (code-defined in `command/builtins.go`): `/ping`, `/status`, `/cache`, `/last`, `/cost`, `/reset`, `/model`, `/session`, `/tools`, `/config`, `/log`, `/errors`, `/version`, `/uptime`
2. **Custom** (script-defined in `clod.toml` via `[[commands]]`): runs a shell script, returns stdout. Timeout default 10s.

Commands use callbacks (closures) to access internal state, avoiding package dependencies on `session`, `agent`, etc.

## Config (`config/config.go`)

Single `clod.toml` parsed with BurntSushi/toml. Sections: `[agent]`, `[anthropic]`, `[telegram]`, `[sessions]`, `[memory]`, `[http]`, `[logging]`, `[[commands]]`. Defaults applied for missing fields.

## Telegram Bot (`telegram/bot.go`)

Two goroutines:
```
[receiver goroutine]   →  receive msg  →  slash command?  →  yes: execute, reply
                                                           →  no:  enqueue (buffered chan)
[agent worker goroutine]  →  dequeue msg  →  create turn context  →  HandleMessage  →  reply
```

The receiver never blocks on the agent. Slash commands (including `/stop`) execute immediately on the receiver goroutine. Agent messages are processed sequentially by the worker.

**Turn cancellation:** Each agent turn gets its own `context.WithCancel`. `/stop` calls `turnCancel()`, which propagates to in-flight API calls (HTTP client context) and tool executions (process group kill). The agent loop checks `ctx.Err()` after API responses and between tool calls.

**Reset guard:** `/reset` refuses when `agent.IsProcessing()` is true — prevents clearing an active conversation mid-turn.

## HTTP Gateway (`main.go`)

Endpoints for external integration (used by `clod` CLI):
- `POST /send` — message to main session, returns response
- `GET /status` — dispatches `/status` command
- `POST /command` — dispatches any slash command
- `POST /wake` — branch session for cron/external triggers

## CLI Tool (`cmd/clod/`)

Separate binary (`go build ./cmd/clod`) for scripts, cron jobs, and external tools. Binary name: `clod`. Commands: `send`, `wake`, `status`, `eval`, `command`, `ping`. Talks to the HTTP gateway (`clodgw`) at `CLOD_ADDR` (default `127.0.0.1:18791`).

## Heartbeat & Wake

- **Heartbeat** (`agent/heartbeat.go`): Timer goroutine, fires after idle duration, injects `[HEARTBEAT]` message into main session. Resets on any activity.
- **Wake** (`POST /wake`): Creates a branch session from the agent's main session, injects the text, runs the agent on the branch.

## Compaction (`compaction/compact.go`)

Checks token usage against threshold (default 80% of 200k). When triggered: asks model to summarize history, replaces session with 3-message compacted version (context note + summary + continuation note).

## Testing

```
go test ./...           # all tests (~66, runs in ~1s)
go test ./... -v        # verbose
go test ./session/...   # single package
```

The cache_test.go in `anthropic/` requires `ANTHROPIC_API_KEY` env var and hits the real API. All other tests are self-contained.
