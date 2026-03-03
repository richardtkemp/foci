# Foci — Wiring Diagram

How the pieces connect. Read this before touching the code.

## Startup Flow (`main.go`)

```
config.Load(path)                                        ← validates values; logs to stderr + buffer
  → log.Init(cfg.Logging)                                ← opens event file, replays buffered events
  → log.InitAPIDB(cfg.Logging.APIDB)                     ← SQLite API call log (api.db)
  → log.InitConversation(cfg.Logging.ConversationFile)   ← SQLite conversation log
  → secrets.Load(secretsPath)                            ← secrets.toml overrides foci.toml
  → [if bitwarden.enabled] bitwarden.New(executor, ttl) ← aisudo-backed vault store
    → DefaultExecutor{SessionFile: cfg.SessionFile} — bitwarden user reads its own session file
    → bwStore.Refresh() → initial metadata load (allowlisted in aisudo)
    → start background refresh ticker (refresh_interval)
    → bwStore.StartCleanup(cleanup_interval)

  Shared resources (created once):
  → configDir = filepath.Dir(configPath)                  ← base for relative paths
  → cfg.DataPath(configDir, file)                         ← resolves DB paths via data_dir or configDir
  → Token resolution (priority order):
  →   1. Setup token: anthropic.setup_token from secrets.toml → tokenHolder + NewClientWithTokenFunc (hot-reloadable)
  →   2. API key: anthropic.api_key from secrets.toml → tokenHolder + NewClientWithTokenFunc (hot-reloadable)
  →   3. Claude Code fallback: NewOAuthManager(~/.claude/.credentials.json) → read-only, auto-refresh
  → session.NewStore(dir)
  → sessions.RepairOrphans()                             ← fix interrupted tool calls before agents start
  → sessions.InjectRestartMarkers(1h)                    ← append "[System restarted]" to recently active sessions
  → session.NewSessionIndex(session_index.db)             ← SQLite index of all session files; rebuilt on startup
  → sessions.OnSessionEvent(→ sessionIndex)               ← lifecycle hook: create/compact/clear → update index
  → memory: ReminderStore + Scratchpad + TodoStore       ← shared across agents (scoped per-agent via agent_id)
  → memory backends (FTS5 and/or bleve)                  ← shared OR per-agent (see below)
  → telegram.NewToolDetailStore(tool_details.db)           ← shared; persists inline keyboard expansion data across restarts
  → voice STT/TTS providers                              ← shared across agents
  → telegram.NewBotManager()

  Per-agent loop (for each cfg.Agents[i]):
  → setupAgent(params) → agentInstance{ag, cmds, registry, bootstrap}
    → tools.NewAsyncNotifier()                             ← shared by exec + http_request + tmux, routes by session key
    → tools.NewRegistry() + register all tools             ← per-agent registry (incl. bitwarden_search/unlock if enabled)
    → workspace.NewBootstrap(agent.Workspace, agent.SystemFiles)
    → buildEnvironmentBlock(acfg, configPath, cfg)           ← if [environment] enabled
    → skills.Load(cfg.Skills.Dirs)
    → compaction.NewCompactor(client, sessions, model, threshold)
    → agent.Agent{Client, Sessions, Tools, Bootstrap, EnvironmentBlock, ...}
    → command.NewRegistry() + register built-ins + custom scripts + skill commands
    → telegram.NewBot → botMgr.AddPrimary(agentID, bot)
    → optional: multiball bot → botMgr.AddMultiball(agentID, mbBot)
    → bot.SetReceivedFilesDir(acfg.ReceivedFilesDir || cfg.Telegram.ReceivedFilesDir)
    → agent.RestoreVoiceMode(defaultSessionKey())           ← deferred until default chat is known
    → agent.RestoreSessionOverrides(defaultSessionKey())   ← restore per-session effort/thinking/model from state store
    → agent.SeedSessionMeta(defaultSessionKey())           ← seed gap from session history (correct gap after restart)

  → signal.Notify(SIGINT, SIGTERM)                         ← must register before goroutines that could trigger SIGTERM
  → restoreMultiballSessions()                             ← restore bot→session mappings from state store
  → botMgr.StartAll(ctx)                                  ← starts all bots
  → http.Server{"/send", "/status", "/command", "/wake", "/voice (ws)", "/-/reload-credentials"}  ← routes by agent param
  → injectWelcomeFile()                                    ← setup.sh changelog injection
  → block on signal → shutdown
```

**Multi-agent:** Each agent gets its own tool registry, command registry, workspace bootstrap, compactor, and Telegram bot(s). Shared resources (anthropic client, session store, voice providers) are passed to each agent.

**Per-agent memory:** When any agent has `[[agents.memory.sources]]` configured, each agent gets its own search indices (`memory-{agentID}.db` for FTS5, `memory-{agentID}.bleve` for bleve) combining global `[memory]` sources with agent-specific sources. Agent-specific sources receive a weight boost of +1.0. When no per-agent memory is configured, all agents share a single index (backward compat). Reminder and scratchpad stores are always shared. Which backends are active is controlled by `search_backends` — both FTS5 and bleve can run simultaneously.

**Agent routing:** `agentInstance` map keyed by agent ID. HTTP endpoints use `resolveAgent(id)` — returns first agent when ID is empty (backward compat).

## Shutdown Flow (`main.go`)

```
SIGTERM/SIGINT received
  → close HTTP server
  → gracefulShutdown(agents, timeout)    ← wait for in-flight agent turns
  → startup.RecordCleanShutdown()        ← record timestamp for crash detection
  → cancel context                        ← stops Telegram poll loops, triggers update ack
  → botMgr.Wait()                         ← block until all bots finish ack
  → deferred closes run (SQLite DBs, log files)
```

## Startup Diagnosis (`startup/diagnosis.go`)

On startup, classifies the restart type and includes diagnostics in the startup notification:

```
DiagnoseRestart(stateStore, startTime, logsDir)
  → read system:last_clean_shutdown from state store
  → read /proc/uptime for system uptime
  → classify:
     - clean: shutdown < 5 min before startup
     - crash: shutdown > 5 min, system uptime > gap
     - reboot: system uptime < shutdown gap (system restarted)
     - unknown: no prior shutdown record
  → for crash/reboot: gatherDiagnostics() scans foci.log for ERROR/FATAL lines
  → return DiagnosisResult{Class, Diagnostics, Summary}
```

**Telegram notification:** `SendStartupNotificationWithDiagnosis` appends the formatted diagnosis to the standard restart message. Clean restarts get no extra text. Crashes show "⚠️ Unexpected restart" with error lines. Reboots show "🔄 System reboot detected".

**State key:** `system:last_clean_shutdown` holds Unix timestamp of last graceful shutdown.

## Package Dependency Graph

```
main
 ├── config        → table
 ├── log           → modernc.org/sqlite
 ├── table         (no deps)
 ├── secrets       → BurntSushi/toml
 │   └── secrets/bitwarden → log
 ├── anthropic     (no deps)
 ├── session       → anthropic, log
 ├── memory        → modernc.org/sqlite, fsnotify/v4, blevesearch/bleve/v2 (FTS5 + bleve backends)
 ├── voice         → log, gorilla/websocket
 ├── skills        → log (leaf package)
 ├── startup       → log, state (leaf package for crash detection)
 ├── tools         → anthropic, log, memory, secrets, voice
 ├── workspace     → anthropic
 ├── prompts       (no deps — embedded .md files)
 ├── compaction    → anthropic, prompts, session, log
 ├── provision     (no deps — stdlib-only leaf package for agent creation)
 ├── command       → table, provision
 ├── mana          → anthropic, log (leaf-ish — pure mana budget logic)
 ├── warnings      → log (leaf — warning queue and proactive dispatch)
 ├── agent         → anthropic, compaction, mana, warnings, session, tools, workspace, log
 ├── keepalive     → mana, warnings, config, log, memory, prompts, state (NO agent, NO session)
 └── telegram      → agent, command, log, table, voice
```

No circular dependencies. `table`, `log`, `secrets`, `memory`, `skills`, `prompts`, `startup`, `provision`, `mana`, `warnings` are leaf packages. `session` and `voice` depend only on `anthropic` / `log`. `keepalive` no longer imports `agent` or `session` — mana monitoring and warning dispatch are handled by the `mana` and `warnings` packages respectively, wired together in `main.go`.

**`provision` package:** Shared agent creation logic used by both `cmd/foci/setup.go` (first-run wizard) and `command/agents_new.go` (`/agents new` runtime command). Stdlib-only, no imports from other foci packages. Provides `AgentSpec` + `Provision()` (workspace creation, character file copying, SOUL.md templating), validation (`IsValidAgentID`, `IsValidBotToken`, `IsValidUserID`), model alias resolution (`ResolveModelAlias`), config block generation (`GenerateAgentBlock`), and crontab templating (`GenerateCrontab`, `AppendCrontab`).

## The Agent Loop (`agent/agent.go`)

The core of the system. Two entry points:
- `HandleMessage(ctx, sessionKey, text)` — text-only, delegates to `HandleMessageWithAttachments`
- `HandleMessageWithAttachments(ctx, sessionKey, text, attachments)` — full version with optional image/document attachments

**Tool execution guarding and redaction:**
- After a tool executes, `guardToolResult()` checks if result exceeds `MaxResultChars`
- If exceeded, writes full result to temp file and returns a guard message (no partial content)
- Prevents large tool outputs from permanently bloating session history
- `agent.Redact` is applied to all tool results and error messages (secret redaction)
- Tool errors are logged as WARN in the event log

```
1. sessions.LoadFull(sessionKey)          ← parent[:branchPoint] + own msgs
2. buildMetaPrefix() + prepend to user message text
3. build content blocks: image/document block(s) first, then text block (with metadata)
4. append user message
5. bootstrap.SystemBlocks()               ← workspace/*.md → []SystemBlock
   prepend EnvironmentBlock if set        ← runtime context block
   append ExtraSystemBlocks               ← skills, etc.
6. tools.ToolDefs() + append ServerTools   ← registry → []ToolDef (includes server tools)
7. LOOP (max 25 iterations):
   a. logCacheDebug(system, messages, model)  ← warns if system < min threshold
   b. client.SendMessage(system, messages, tools)
   c. log event + log API entry
   d. notify observers for server_tool_use / web_search_tool_result / web_fetch_tool_result blocks
   e. if stop_reason == "pause_turn" → append assistant msg, continue loop (server will resume)
   f. if stop_reason == "end_turn" → save & check compaction & return text
   g. if stop_reason == "tool_use":
      - execute each tool_use via registry (skip server_tool_use — already executed)
      - append assistant msg + tool_result msg
      - goto 7a
8. sessions.AppendAll(sessionKey, newMessages)
9. if compactor.ShouldCompact(messages, usage) → compactor.Compact(sessionKey)
```

Messages are only saved to disk after the full turn completes (all tool loops resolved). Compaction runs after save, replacing the session with a 3-message summary if the context exceeds the threshold (default 80% of 200k).

**Error handling by status code:**
- **429 (rate limit):** Our quota is exhausted. `classifyAPIError` fires `RateLimitFunc` callback (Telegram notification with estimated retry time from `Retry-After` header) and returns `"rate limited — mana exhausted"`. No transport-level retry (retrying won't help until the window resets).
- **529 (overloaded):** Anthropic servers are overloaded (their problem, not ours). Two-phase retry in `SendMessage`: phase 1 retries 3× with exponential backoff (2s→4s→8s, same as other retryable errors); phase 2 (529 only) enters an extended duration-based loop retrying up to ~2 hours with 5s base backoff doubling without cap. A cross-goroutine recovery signal on the `Client` wakes all sleeping retry loops when any `SendMessage` succeeds (proving the server has recovered). If still failing after phase 2, `classifyAPIError` returns `"Anthropic API is overloaded — try again shortly"`.
- **500/502/503 (server error):** `SendMessage` retries 3× with backoff. If still failing, `classifyAPIError` fires `RateLimitFunc(0)` and returns a temporary unavailability message.

### Cache Stability Invariant

Conversation history sent to the API must be a strict append-only extension of the previous request — inserting a message in the middle invalidates all cached tokens after that point. `HandleMessageWithAttachments` enforces this via a per-session turn lock that serializes all callers (Telegram, `AsyncNotifier`, scheduled wakes, HTTP `/send`). Different sessions run concurrently. See [CACHING.md](CACHING.md) for the full cache stability contract.

## Message Metadata

**Message transforms** (`[[message_transforms]]` in config) run regex find/replace on inbound user messages. Transforms fire before command dispatch — if a message is already a recognized command, transforms are skipped. If transforms produce a command (e.g. `m` → `/mana`), it is dispatched as one. Rules run in sequence; each rule's output becomes the next rule's input.

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

When the model responds with text alongside `tool_use` blocks (e.g., "Looking into this..."), the text is sent to Telegram before tool execution begins. This allows the agent to acknowledge a message and deliver the full response later.

Controlled by `batch_partial_assistant_messages` (bool, default `false`):
- **false (default):** Text is sent immediately via `ReplyFunc` each time it appears in a response.
- **true:** Text is accumulated in a `strings.Builder` and returned concatenated from `HandleMessage` when the turn completes.

**Flow (batch=false, default):**
1. Caller creates a `TurnCallbacks` struct and attaches it via `agent.WithTurnCallbacks(ctx, cb)`
2. Agent loop detects text in a `tool_use` response
3. `sendIntermediateCtx(ctx)` extracts the ReplyFunc from context and calls it
4. Agent continues executing tools
5. Final `end_turn` response is returned from `HandleMessage` as usual

**Flow (batch=true):**
1. Agent loop detects text in a `tool_use` response and appends to `batchedText`
2. On `end_turn`, batched text is prepended to final text (joined with `\n\n`)
3. Concatenated text is returned from `HandleMessage`

Callbacks are **context-scoped**, not agent-global. Each turn gets its own isolated callbacks.

Both Telegram-triggered and async callers (tmux watch, exec auto-background) now set up `TurnCallbacks` with a `ReplyFunc`. The async_notify path resolves the bot early and attaches callbacks before calling `HandleMessage`, so intermediate text is delivered during system-triggered turns.

## Tool Call Visibility

Tool call display is controlled by `show_tool_calls` (string: `"off"`, `"preview"`, `"full"`). Configurable globally in `[telegram]` and per-agent in `[[agents]]`. Bool values are accepted for backwards compat (`true` → `"preview"`, `false` → `"off"`).

**Modes:**
- **`"off"`** (default) — Tool calls are hidden. `ToolCallObserver` returns immediately.
- **`"preview"`** — Tool calls are shown via send+edit, then the final response **overwrites** the tool message (or falls back to a new message if too long).
- **`"full"`** — Tool calls are shown via send+edit (same as preview), but the final response is always sent as a **separate new message**, preserving the tool call log in chat.

Both `ToolCallObserver` and `ReplyFunc` are part of the context-scoped `TurnCallbacks` struct — per-turn, not agent-global.

**Ordering with deferred replies:** When intermediate text fires between tool loops, `ReplyFunc` resets `toolMsgID` to 0. This forces the next tool call to create a fresh message below the text, preserving chronological order in chat.

**Flow (multi-loop turn, preview/full):**
1. Loop 1: API returns `[tool_use(exec)]` — `notifyToolCall` sends message A (`toolMsgID=A`)
2. Loop 2: API returns `[text("Checking..."), tool_use(read)]`
   - `sendIntermediate` fires `ReplyFunc` → sends message B, resets `toolMsgID=0`
   - `notifyToolCall` sends message C (`toolMsgID=C`, fresh because reset)
3. Final:
   - **preview**: `end_turn` response edits message C with the answer
   - **full**: `end_turn` response sends as message D (new message)

**Chat order (preview):** A ("🔧 exec") → B ("Checking...") → C ("🔧 read" → final answer) ✓
**Chat order (full):** A ("🔧 exec") → B ("Checking...") → C ("🔧 read") → D (final answer) ✓

**Inline result expansion (full mode only):** In "full" mode, each tool call message includes a "Show results" inline keyboard button. Pressing it expands the message to include the tool's output (truncated to fit Telegram's 4096-char limit). "Hide results" collapses back.

- `ToolResultObserver` callback fires after each tool execution (both success and error), storing the result in `Bot.toolResults` (`sync.Map`, message ID → `toolResultEntry`). Write-through: if `ToolDetailStore` is set, also persists to SQLite (`tool_details.db`) so inline keyboard expansions survive restarts. On startup, `SetToolDetailStore` loads entries <48h old into the in-memory map. Periodic idle cleanup (10min tick, runs when all users idle) expires old entries and runs `PRAGMA incremental_vacuum`.
- `handleCallbackQuery` processes `tc:show:<msgID>` / `tc:hide:<msgID>` button presses, editing the message and answering the callback query. Also handles `cmd:/name args` for inline keyboard command selections.
- `pollUpdates` requests `AllowedUpdates: ["message", "callback_query"]` to receive button press events.

**Inline keyboard commands:** Commands with a `KeyboardOptions` field (`/model`, `/thinking`, `/effort`, `/config`, `/sessions`, `/tmux`) show an inline keyboard when invoked bare. `LookupKeyboard()` checks for this before `Dispatch()`. `sendCommandKeyboard()` builds and sends the keyboard. Callback data format: `cmd:/name args`. `handleCommandCallback()` executes the command and edits the message to show the result.

## Thought Queue (Reminders)

The agent can defer thoughts for later via the `remind` tool. Reminders are stored in SQLite (`reminders.db`) and surfaced as injected context when due. With `wake=true`, the session is actively woken at the specified time.

**Storage:** `ReminderStore` in `memory/remind.go`. Table `reminders` with columns: `id`, `agent_id`, `text`, `due_at`, `due_tag`, `created`. Scoped per-agent — each agent sees only its own reminders.

**Time resolution (`resolveWhen`):**
- `next_session`, `now` → immediate
- `tomorrow` → midnight tomorrow UTC
- `YYYY-MM-DD` → that date at midnight UTC
- Go duration (e.g., `2h`, `30m`) → now + duration

**Injection:** At the start of each `HandleMessage`, `collectReminders()` checks for due reminders. If any exist, they're appended to the metadata line as a `[reminders]` block in the user message (past the cache breakpoint, so caching is unaffected). Due reminders are auto-dismissed after surfacing.

**Example injected message:**
```
[meta] time=2026-02-21T05:30:00Z gap=45m0s
[reminders]
- Look into FTS5 phrase boosting (set 2h, due: 2026-02-21 05:00)
Hello, what should I work on?
```

## Scratchpad

Working state that survives compaction but isn't permanent memory. The agent writes notes during investigations and clears them when done.

**Storage:** `Scratchpad` in `memory/scratchpad.go`. SQLite table `scratchpad` with columns: `agent_id`, `key` (composite primary key), `content`, `updated`. Stored in `scratchpad.db`. Scoped per-agent — each agent sees only its own entries.

**Tool:** `scratchpad(action, key, content)` — single tool with action parameter (write/read/clear/list). Agent ID injected at tool creation time.

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

1. **Environment block** (`agent.EnvironmentBlock`) — programmatically built at startup from config values. Contains workspace path, agent ID, platform URL, messaging platform, config/log paths, message metadata docs, and session structure. Built by `buildEnvironmentBlock()` in `main.go`, stored as a string on the Agent struct, prepended as the first `SystemBlock` in `HandleMessageWithAttachments`. Omitted when `[environment] enabled = false` (empty string).

2. **Character files** (`workspace/bootstrap.go`) — reads markdown files from workspace dir in order:
```
IDENTITY.md → SOUL.md → COHERENCE.md → AGENTS.md → TOOLS.md → USER.md → MEMORY.md
```

Each becomes a `SystemBlock{type:"text", text:content}`. Missing/empty files are silently skipped.

3. **Secrets block** — appended by `Bootstrap.SystemBlocks()` if secret names are available. Lists available `{{secret:NAME}}` template keys.

4. **Extra system blocks** — skills list and other injected blocks (`agent.ExtraSystemBlocks`).

The **last** block gets `cache_control: {type: "ephemeral"}`. Order matters: most-stable blocks first maximizes cache prefix reuse. The environment block is highly stable (only changes on restart), making it a good cache prefix leader.

## Anthropic API Client (`anthropic/`)

Three clients (two token types — see [docs/AUTH.md](AUTH.md)):

1. **Client** (`client.go`) — messages API + token counting
   - Sends model requests with system prompt + conversation history
   - Also handles `/v1/messages/count_tokens` for `/context` command
   - Supports static token (`NewClientWithTimeout`) or dynamic token func (`NewClientWithTokenFunc`)
   - Sets `anthropic-beta: oauth-2025-04-20` header for OAuth token auth

2. **UsageClient** (`usage.go`) — mana/usage API
   - Queries `/api/oauth/usage` endpoint
   - Supports static token (`NewUsageClient`) or dynamic token func (`NewUsageClientWithFunc`)
   - Returns utilization for 5-hour window, 7-day limits, extra usage billing

3. **OAuthManager** (`oauth.go`) — OAuth PKCE token lifecycle
   - Loads credentials from disk (foci-native or Claude Code format)
   - Background refresh goroutine refreshes ~5min before expiry
   - Provides `Token()` func used by both Client and UsageClient via tokenFunc

## Prompt Caching

Two `cache_control: ephemeral` breakpoints per API request: one on the system prompt (`bootstrap.SystemBlocks()`), one on the second-to-last conversation message (`withCacheBreakpoint()` in `agent.go`). Breakpoints are added only to the API request payload, never persisted to session storage. See [CACHING.md](CACHING.md) for the full cache architecture, stability invariant, and monitoring.

## Secrets (`secrets/`)

Loaded from `secrets.toml` (same directory as `foci.toml`). Stored as flat keys: `anthropic.setup_token`, `custom.github_token`, etc. Overrides `foci.toml` credentials at startup. See [SECRETS.md](SECRETS.md) for the full security model, OS-level protection, setup, and Bitwarden configuration.

Data flow:
- **Template resolution:** `{{secret:custom.github_token}}` in `http_request` headers/body → replaced with actual value before sending. Regular secret templates are blocked in shell (returns error). Bitwarden `{{secret:bw.*}}` templates are allowed in shell (approval-gated via aisudo).
- **Domain locking:** `allowed_hosts` per section restricts which hosts a secret can be sent to via `http_request`. `secrets.FindSecretRefs()` extracts template refs; `store.CheckHostAllowed()` validates the target URL (userinfo-safe via `url.Parse().Hostname()`)
- **Output redaction:** Secret values in command/response output → `[REDACTED]` (skips values < 4 chars)
- **Path blocking:** Commands referencing `secrets.toml` or `/proc/self/environ` are refused

**Bitwarden integration** (`secrets/bitwarden/`): Optional dynamic secret store. Depends only on `log` (leaf package). Two-tier aisudo model:
- Metadata refresh: `sudo -u bitwarden bw list items` (allowlisted, auto-approved)
- Password fetch: `sudo -u bitwarden bw get password <id>` (requires Telegram approval)
- Template syntax: `{{secret:bw.UUID}}` — resolved in both `http_request` and `shell` (approval-gated, safe for both)
- Host validation: vault item URI fields → allowed hosts (same pattern as `allowed_hosts` in secrets.toml)
- TTL-based caching with background cleanup goroutine

## Logging (`log/`)

**Two-phase init:** Before `log.Init()`, events go to stderr and are buffered in memory. When `Init()` opens the event file, buffered events are replayed to it. This ensures config-load warnings (e.g. unknown keys) appear in the log file despite being emitted before the file path is known.

Four outputs:

1. **Event log** (`foci.log` + stderr): `2026-02-21T03:52:39Z INFO  [telegram:mybot] message from rich: hello`
   - Package-level: `log.Infof("component", "format", args...)`
   - Per-component: `log.NewComponentLogger("telegram:" + agentID)` → `logger.Infof("format", args...)`
   - Major components (Agent, Bot, Keepalive, Compactor) carry a `*log.ComponentLogger` field
     initialized at construction with a prefix like `"agent:mybot"`. This avoids repeating
     the component string at every call site and encodes the agent ID for multi-agent setups.
   - Levels: DEBUG < INFO < WARN < ERROR

2. **API log — JSONL** (`api.jsonl`): One JSON object per Anthropic API call with ts, session, model, token counts, cost_usd, duration_ms.
   - Use: `log.API(log.APIEntry{...})`
   - Queryable with `jq`

3. **API log — SQLite** (`api.db`): Same data as JSONL but in a `api_calls` table with indexes on `ts` and `session`. Includes `call_type` column (conversation, compaction, summary, spawn).
   - Written automatically by `log.API()` when `api_db` is configured
   - Queryable: `sqlite3 api.db "SELECT call_type, count(*) FROM api_calls GROUP BY call_type"`

4. **Conversation log** (`conversation.db`): SQLite database logging exact Telegram messages sent and received. Table `messages` with columns: `id`, `ts`, `direction` (recv/sent), `user_id`, `username`, `chat_id`, `text`, `parse_mode`, `session`, `error`.
   - Use: `log.Conversation(log.ConversationEntry{...})`
   - Queryable with `sqlite3 conversation.db "SELECT * FROM messages"`
   - Useful for debugging formatting (see exact markdown sent vs plain text fallback)

## Tool System (`tools/`)

Each tool is a `Tool` struct with `Execute func(ctx, params) (ToolResult, error)`. `ToolResult` contains `Text` (the tool's text output) and optional `ExtraBlocks` (additional content blocks like document blocks for PDFs). Registry maps name → tool. See [TOOLS.md](TOOLS.md) for the canonical tool reference. Data-flow summary:

| Tool | File | What it does |
|------|------|-------------|
| `shell` | shell.go | Shell commands via `sh -c`, process group kill on timeout, output redaction. Regular `{{secret:}}` templates are blocked (returns error — use http_request). Bitwarden `{{secret:bw.*}}` templates are allowed (approval-gated via aisudo). |
| `http_request` | http.go | Domain-locked HTTP requests. Secrets in headers/body validated against per-section `allowed_hosts` before sending. Cross-domain redirects blocked when secrets present. Response redacted. Binary responses (image/*, audio/*, etc.) auto-saved to temp file. `save_to` saves any response to a specific path. `save_from_json_path` extracts a value from JSON response and decodes data: URIs (base64 images from generation APIs). |
| `tmux` | tmux.go | Manage tmux sessions — start (auto-watches by default), send keys, read pane output, list, kill, watch for inactivity, unwatch. Owned sessions persist across app restarts via state store. Autopilot mode (default on): auto-unwatches after inactivity notification, auto-watches on send. |
| `read` | files.go | File contents with line numbers, truncates at 2000 lines |
| `write` | files.go | Create/overwrite files |
| `edit` | files.go | Find-and-replace (old_string must be unique). Syntax validation for .json, .toml, .go, .yaml/.yml, .xml, .py, .sh/.bash: rejects edits that would break a valid file, warns if file was already invalid. |
| `web_fetch` | web.go / server | Fetch web content (server-side default, client-side fallback) |
| `web_search` | web.go / server | Web search (server-side default, Brave fallback) |
| `summary` | summary.go | Summarize/extract from large files via Haiku call |
| `memory_search` | memory.go | Full-text search over memory files (+ conversation history for FTS5). Pluggable backends: FTS5 (default) and bleve. Porter stemming, weighted ranking, sort by relevance or recency. Optional `backend` parameter when multiple backends are active. |
| `remind` | remind.go | Defer a thought for later; stored in SQLite, surfaced as injected context when due. `wake=true` actively wakes the session. |
| `scratchpad` | scratchpad.go | Working notes that survive compaction (write/read/clear/list via `action` parameter) |
| `spawn` | spawn.go | Unified sub-call: four context modes. All modes have tool access with a tool-call loop. `raw`: one-shot, no system prompt (`send_telegram` and `send_to_session` blacklisted — no character context means no communication awareness). `character`: one-shot with character files (all tools). `clone` (default): branch session — a headless self-fork. `explore`: one-shot safe exploration with `ls`, `find`, `grep`, `read`, `memory_search`, `web_search`, `web_fetch` only — no file mutation, no shell exec, no messaging, always haiku. clone creates branch `agent:ID:spawn:spawn-TIMESTAMP`, always runs async via `AsyncNotifier` (returns immediate ack, delivers `[SPAWN RESULT]` on completion). Recursive clone blocked via context key. Concurrent clone limited by `max_concurrent_spawns` (default 3). `spawn` itself is excluded from one-shot tool sets to prevent recursion. |
| `ls` | explore.go | List directory contents. Internal to `explore` spawn mode — not registered in the main tool registry. |
| `find` | explore.go | Search for files in a directory hierarchy. Dangerous predicates (`-exec`, `-delete`, etc.) blocked. Internal to `explore` spawn mode. |
| `grep` | explore.go | Search file contents using the best available binary (rg > ack > ag > grep). Flags are validated and translated to the active binary's dialect. Internal to `explore` spawn mode. |
| `send_telegram` | telegram.go | Send proactive Telegram messages (text, documents, voice notes). With `send_as="voice"` and text (no file_path), synthesizes speech via TTS. Routes to the chat extracted from the session key (`agent:X:chat:CHATID`) so per-chat sessions get messages to the correct user. Falls back to bot's default chat when no chat ID in session key. |
| `send_to_session` | session_send.go | Inject a user-role message into another session. Tags the message with `[Message from session ...]` origin header. Appends to session store and triggers processing via `AsyncNotifier`. Used for cross-session communication (e.g. multiball branches talking to main). |
| `todo` | todo.go | Per-agent task list (add, list, complete, remove). SQLite backend with priority ordering (high/medium/low). Scoped by `agent_id`. |
| `bitwarden_search` | bitwarden.go | Search Bitwarden vault items by name, URI, folder, username. Returns metadata only (never passwords). Max 5 results. Only registered when `[bitwarden] enabled = true`. |
| `bitwarden_unlock` | bitwarden.go | Unlock a vault item by ID. Calls `sudo -u bitwarden bw get password` via aisudo — blocks until Telegram approval or denial. Caches value for `secret_ttl`. Never returns the actual password. |

### Exec Bridge / Tool Piping (`tools/execbridge.go`)

Exposes selected tools as shell functions inside `shell` calls via a per-shell unix socket. This allows unix-style composition (pipes, filters) in a single shell invocation — intermediate data never enters agent context.

**Architecture:**
```
exec subprocess                       foci process
┌─────────────────────┐               ┌───────────────┐
│ foci_http_request ──┼──connect────▶ │ goroutine/conn │
│ foci_web_fetch    ──┼──connect────▶ │ goroutine/conn │
│ foci_spawn        ──┼──connect────▶ │ goroutine/conn │
└─────────────────────┘               └───────────────┘
    /tmp/foci-exec-<pid>-<n>.sock
```

**How it works:**
1. `execDirect`/`execWithAutoBackground` create an `ExecBridge` before spawning the subprocess
2. Bridge creates a unix socket (`/tmp/foci-exec-<pid>-<n>.sock`, 0600 perms) and a shell functions file
3. `FOCI_SOCK` env var and `source <funcs.sh>` are injected into the command
4. Shell functions use `jq` for JSON construction and `foci-call` binary for socket communication
5. Bridge accepts connections and routes requests to tools with `ExecExport: true`
6. Bridge is closed after the subprocess exits (cleanup: socket + funcs files removed)

**Skipped for:** explicit `background: true` mode (daemons don't need piping).

**For auto-background:** bridge context uses `context.Background()` + session key so it survives agent turn end.

**Tools with `ExecExport: true`:** `http_request`, `web_fetch`, `web_search`, `memory_search`, `todo`, `send_telegram`, `spawn`, `tmux`.

**`foci-call` binary** (`cmd/foci-call/`): Reads `FOCI_SOCK`, connects to unix socket, sends JSON request (newline-terminated), prints result to stdout or error to stderr, exits 0/1. 1MB scanner buffer.

### Tmux Memory Monitor (`tools/tmux_memory.go`)

Background goroutine that checks the RSS of the tmux server process at configurable intervals. Three thresholds (warn, critical, kill) fire Telegram notifications and, at the kill threshold, run `tmux kill-server` and call `ClearAll()` on all tmux tool instances. Notifications use dedup — same threshold level won't re-fire until memory drops below it or tmux is killed.

Wired in `main.go` after agent setup. Notification callback sends to agents whose `inject_agent_warnings` is false (agents with injection see warnings via their `warnings.Queue` — proactively dispatched as independent agent turns via `warnings.Dispatcher`). Cleanup callback calls `tmuxClearAll` on each agent instance (stored on `agentInstance` struct).

### System Memory Guard (`resources/memory_guard.go`)

Background goroutine monitoring total RSS of all processes owned by the foci user. Reads `/proc/[pid]/status` directly — no external commands. Two thresholds (warn at 25%, kill at 40% of RAM), both gated by memory pressure (PSI `avg10` from `/proc/pressure/memory` > configurable threshold). Warn pushes to all agents' `WarningQueue` (surfaces via proactive warning dispatch). Kill finds the largest non-foci process by RSS (excludes `os.Getpid()`), sends SIGTERM, waits 5s, SIGKILL if still alive.

Wired in `main.go` after tmux memory monitor. Warning callback iterates `agents` map and pushes to any `inst.ag.Warnings` that's non-nil (agents with `inject_agent_warnings`).

### Tool Result Guard

If a tool result exceeds `agent.MaxResultChars` (from config, default 5,000), the result is written to `agent.ToolResultTempDir` instead of injected directly. Before returning a guard message, the agent makes a side-call to Haiku to auto-summarise the oversized content, including recent conversation context (configurable via `summary_context_turns` and `summary_context_chars`). The agent receives the summary plus a reference to the saved file for deeper inspection. If the Haiku call fails (API error, context cancelled), falls back to the original guard message with file path and contextual tool hints (e.g. `jq` for JSON, `mdq` for markdown). This prevents large results from bloating session history while giving the agent useful visibility into the content.

## Slash Commands (`command/`)

Messages starting with `/` are intercepted at the Telegram router level before reaching the agent. They execute immediately — never queued behind an in-flight agent turn.

**Dispatch flow:** Telegram message → auth check → if `/`: `registry.Dispatch()` → execute → reply. Never touches agent session or message history.

**Two types:**
1. **Built-in** (code-defined in `command/builtins.go`): `/ping`, `/status`, `/cache`, `/last`, `/cost`, `/mana`, `/reset`, `/reload`, `/model`, `/session`, `/tools`, `/tmux`, `/config`, `/log`, `/errors`, `/version`, `/uptime`, `/voice`, `/multiball` (alias `/mb`)
   - `/mana` — check quota remaining (`/usage` is a hidden alias)
   - `/reload` — reload workspace files, skills, and system blocks from disk
2. **Custom** (script-defined in `foci.toml` via `[[commands]]`): runs a shell script, returns stdout. Timeout default 10s.

Commands use callbacks (closures) to access internal state, avoiding package dependencies on `session`, `agent`, etc.

## Config (`config/config.go`)

Single `foci.toml` parsed with BurntSushi/toml. Defaults applied for missing fields.

**Multi-agent config:** Two formats supported:

1. **Legacy (single agent):** `[agent]` table — backward compatible, auto-promoted to single-element `Agents` slice.
2. **Multi-agent:** `[[agents]]` array — each agent has its own `id`, `model`, `workspace`, `telegram_bot`, `multiball_bots`.

When both `[agent]` and `[[agents]]` are present, `[[agents]]` wins.

`cfg.Agent` always mirrors `cfg.Agents[0]` so legacy code paths work unchanged.

**Telegram bot token resolution:** Bot tokens are resolved by convention: `config.ResolveBotToken(botName, botSecret, secrets)` looks up `"telegram.<botName>"` in the secrets store (or uses `botSecret` as the key if non-empty). No explicit `[telegram.bots]` map is needed. Agents set `telegram_bot` (defaults to agent ID) and optionally `bot_secret` for override.

**Example multi-agent config:**
```toml
[[agents]]
id = "clutch"
model = "claude-sonnet-4-6"
workspace = "/home/rich/workspace1"
telegram_bot = "primary"
multiball_bots = ["clutchling"]       # per-agent pool

[[agents]]
id = "scout"
workspace = "/home/rich/workspace2"
telegram_bot = "scout"

[telegram]
allowed_users = ["5970082313"]
multiball_bots = ["spare1"]           # shared pool (any agent)
```

With `secrets.toml`:
```toml
[telegram]
primary = "123456:ABC..."
clutchling = "234567:DEF..."
scout = "345678:GHI..."
spare1 = "456789:JKL..."
```

## Telegram Bot (`telegram/bot.go`)

Two goroutines:
```
[receiver goroutine]   →  receive msg  →  wizard active?  →  yes: route to wizard, reply
                                       →  slash command?  →  yes: execute, reply
                                       →  voice note?     →  download OGG, transcribe via Whisper → text
                                       →  photo/doc/PDF?  →  download attachment via Telegram file API
                                                           →  enqueue (buffered chan) with text + attachments
[agent worker goroutine]  →  dequeue msg  →  create turn context  →  HandleMessage[WithAttachments]  →  reply
```

The receiver never blocks on the agent. Slash commands (including `/stop`) execute immediately on the receiver goroutine. Agent messages are processed sequentially by the worker.

**Stale command filtering:** Slash commands older than 30s are silently dropped. Safety net for update replay after crashes — prevents stale `/reset` or `/stop` from firing on restart.

**Shutdown ack:** On context cancellation, each bot's poll loop fires one final `GetUpdates` with the last processed offset. This acknowledges processed updates to Telegram, preventing replay on restart. `BotManager.Wait()` blocks main after `cancel()` to ensure all bots complete this ack before process exit.

**Wizard routing (`WizardHandler`):** Interactive wizards (e.g. `/agents new`) take over message routing via `Registry.HandleMessage()`. When a wizard is active, ALL messages (including non-`/` text) are intercepted by the receiver goroutine before reaching slash command dispatch or the agent queue. `/cancel` and `/stop` abort the active wizard. The wizard is cleared automatically when it signals completion (`done=true`).

**Attachment handling:** Photos (`msg.Photo`, largest size selected), image documents (`msg.Document` with image MIME type), and PDF documents (`msg.Document` with `application/pdf` MIME type) are downloaded via `GetFile()` + HTTP GET. The raw bytes are queued as `attachment` structs alongside the message text (which may come from `msg.Caption` for photos). PDFs over 32MB fall back to save-to-disk with a text annotation. The agent worker converts these to `agent.Attachment` and calls `HandleMessageWithAttachments`, which routes images to `ImageBlock()` and PDFs to `DocumentBlock()` content blocks.

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
2. **TTS via send_telegram** — the agent can call `send_telegram(text="...", send_as="voice")` to synthesize speech and send a voice note. Works regardless of voice mode.

```
voice.TTS.Synthesize(text) → Edge TTS CLI or OpenRouter TTS API
  → raw MP3 bytes → tgbotapi.NewVoice(chatID, FileBytes{mp3})
```

Two TTS providers:
- **Edge TTS** (default, free): Uses `edge-tts` CLI. Configurable voice and rate (`--rate "+20%"`).
- **OpenAI** (via OpenRouter): API key from `secrets.toml` under `[openrouter] api_key`. Configurable endpoint/model/voice/speed.

Speech rate configurable via `tts_rate` in `[voice]` config section. For edge-tts: percentage (e.g. `"+20%"`). For openai: float 0.25–4.0 (e.g. `"1.5"`).

**Voice mode metadata:** When voice mode is on, the metadata prefix includes `voice=on`:
```
[meta] time=2026-02-21T05:30:00Z gap=3h12m voice=on model=claude-haiku-4-5
```

The agent sees this and adjusts its style (shorter, conversational, no markdown).

### Voice WebSocket (`voice/ws.go`)

Real-time two-way voice conversation via WebSocket at `/voice`. Used by the FOCI Android app.

**Dependencies:** `voice → log, gorilla/websocket`

**Connection flow:**
```
GET /voice?api_key=KEY → validate key → upgrade to WebSocket
  → send connected{agents} → client sends select_agent{agent_id}
  → create ephemeral session (agent:ID:voice:CONN_ID) → send session_ready
```

**Audio turn flow:**
```
audio_start{sample_rate} → binary frames (raw PCM) → audio_end
  → goroutine with turnMu lock
  → wrap PCM in WAV header (44 bytes, 16-bit mono)
  → STT.Transcribe("voice.wav") → send transcription
  → response_start → HandleMessage(agent, session, text) → response_text (final=true)
  → TTS.Synthesize → audio_start + 4KB binary chunks + audio_end
  → response_end
```

**Concurrency model (three mutexes per connection):**
- `writeMu` — serializes all WebSocket writes (text + binary frames)
- `turnMu` — serializes agent turns (prevents concurrent STT→agent→TTS pipelines)
- `audioMu` — protects recording state and audio buffer

**Wiring in `main.go`:** Callback-based (`HandlerConfig`) — `ListAgents` reads `agents` map + `agentOrder`, `HandleMessage` calls `inst.ag.HandleMessage` with `voice` trigger, `AgentTTS` returns `voice.WithRate(ttsProvider, rate)`. Gate: `cfg.Voice.WSEnabled && voiceAPIKey != "" && sttProvider != nil`.

## Multiball (`telegram/pool.go`, `telegram/manager.go`, `telegram/bot.go`)

Fork the current session to a secondary Telegram bot for parallel conversations. Each fork shares the parent's cache prefix. See [MULTIBALL.md](MULTIBALL.md) for user-facing docs (bot pool config, session lifecycle, use cases).

**Config** (`foci.toml`):
```toml
[[agents]]
id = "clutch"
multiball_bots = ["clutchling"]      # per-agent pool

[telegram]
multiball_bots = ["spare1"]          # shared pool (fallback)
```

**Flow:**
```
/multiball → botMgr.AcquireMultiball(agentID)
               → try per-agent pool first (pool.Acquire())
               → if busy/empty, try shared pool (shared.Acquire())
           → bot.SetAgentAndCommands(ag, cmds)  // re-wire shared bots
           → sessions.CreateBranch(parent, multiball:mb-TIMESTAMP)
           → bot.SetSessionKey(branchKey)
           → bot.SendNotification("🎱 Forked from main.")
```

Messages to the secondary bot route to the forked session. `/done` on the secondary bot detaches it and returns it to the pool.

**Bot pool** (`telegram/pool.go`): Tracks secondary bots, acquires LRU idle bot, releases on `/done`.

**Shared pool** (`telegram/manager.go`): `BotManager.shared` is a fallback pool available to any agent. Shared bots are re-wired to the acquiring agent via `SetAgentAndCommands` at fork time.

**Bot changes** (`telegram/bot.go`):
- Per-chat session routing: primary bots derive session key from `msg.Chat.Id` → `agent:ID:chat:CHATID`
- `SessionKey()` — returns override key (secondary bots) or default chat session (primary bots)
- `SetSessionKey()` — thread-safe override (multiball fork/done)
- `SessionKeyForChat(agentID, chatID)` — public helper for session key derivation
- Default chat: first message sets the default; persisted in state store as `agent:ID:default_chat`
- Username recording: persisted per chat for `/sessions list` display
- `isSecondary` flag — enables `/done` handling, idle message rejection
- `/done` handled as special case alongside `/stop` (bypasses command registry)
- Idle secondary bots respond with "This bot is idle. Use /multiball..." to non-command messages

**Session persistence across restarts:** The `bot → session_key` mapping is persisted in the state store (JSON key-value file) under `multiball:<telegram_username>`. Each `SetSessionKey` call fires an `OnSessionKeyChange` callback (wired in `main.go`) that writes or deletes the mapping. On startup, `restoreMultiballSessions()` iterates all pool bots via `Pool.ForEach`, looks up saved keys, validates the session file still exists via `LastActivity`, and restores via `SetSessionKeyDirect` (bypasses callback). The bot is also re-wired to the correct agent via `SetAgentAndCommands` and gets the primary bot's chat ID for notifications.

**Per-session override persistence:** Slash command overrides (`/effort`, `/thinking`, `/model`) are stored per-session in the state store under keys `effort:<sessionKey>`, `thinking:<sessionKey>`, `model:<sessionKey>`. On startup, `RestoreSessionOverrides(sessionKey)` restores all three. The `/voice` mode follows the same pattern under `voice:<sessionKey>`. Overrides reset naturally when a new session starts (no state stored for the new key).

**Special commands on secondary bots:**
- `/done` — detach from forked session, return to pool
- `/stop` — cancel current agent turn (same as primary)
- All other slash commands — shared registry (operate on main session's context)

## HTTP Gateway (`main.go`)

**Auth middleware** wraps all HTTP endpoints (except `/voice`, which has its own auth via `voice.api_key`). Requires `Authorization: Bearer <key>` header or `api_key` query param, validated against `http.api_key` from `secrets.toml` using constant-time comparison. Returns 401 (missing) or 403 (invalid). The key is auto-generated on first startup using a 5-word passphrase (~52 bits entropy). The CLI reads the key from `--api-key` flag or `FOCI_API_KEY` env var.

Endpoints for external integration. All endpoints accept an optional `agent` parameter (JSON body or query string) to target a specific agent. When omitted, defaults to the first configured agent.

- `POST /send` — message to agent's default session (activity-gated). Returns 412 if no default session.
- `GET /status` — dispatches `/status` for the specified agent
- `POST /command` — dispatches slash command (bypasses agent context)
- `POST /wake` — branch from default session (activity-gated, supports `no_compact`/`no_reset_hook`). Returns 412 if no default session.
- `GET /voice` — WebSocket upgrade for real-time voice conversation. Enabled when `[voice] ws_enabled = true`.
- `POST /-/reload-credentials` — hot-reload API credentials from `secrets.toml`. Called by `foci auth` after saving a new token. Only registered when using static token auth (setup-token or API key), not OAuth fallback.

## CLI Tool (`cmd/foci/`)

Separate binary (`go build ./cmd/foci`) that wraps the HTTP gateway endpoints for scripts and cron jobs. See [docs/CLI.md](CLI.md) for the full command reference, flags, environment variables, and cron integration examples.

## Wake

- **HTTP Wake** (`POST /wake`): Creates a branch session from the agent's default chat session, injects the text, runs the agent on the branch. Supports `no_compact` and `no_reset_hook` flags. `--oneshot` CLI flag sets both. Returns 412 if no default session.
- **Scheduled Wakes** (`remind` tool with `wake=true`): Agent-initiated timer that fires message injection into the default session at specified delay or timestamp. One-shot, background goroutine, auto-cleaned after firing. Skips if no default session.

## Session-End Memory Formation

Before a session is cleared (`/reset` or multiball TTL reclaim), the agent captures memories asynchronously. Configured via `[memory_formation]` section (replaces `session_reset_prompt`).

Flow (`fireSessionEndMemory` in `main.go`):
1. Check `memory_formation.session_end_enabled` (nil = true, explicit false skips)
2. Resolve prompt via `prompts.ResolvePrompt(session_end_prompt, ...)` — embedded default on empty/error
3. If prompt resolves to empty, skip
4. For branch sessions, check `BranchMeta.NoResetHook` — if true, skip
5. Create branch from expiring session (copies conversation history)
6. Return immediately — caller proceeds to clear the main session
7. Async: `HandleMessage(ctx, branchKey, prompt)` with 120s timeout, trigger `"session_end_memory"`, NoCompact

Entry points:
- `/reset` command → `fireSessionEndMemory` (async) → `Clear` → `Reload`
- `Pool.Acquire` (TTL reclaim) → `Pool.ReclaimHook` → `fireSessionEndMemory` (async) → clear session key

## Memory Formation & Consolidation Timers

Memory formation and consolidation run in the keepalive timer loop (30s ticks):

**Interval memory formation** (`maybeMemoryFormation`):
1. Check `interval_enabled` (nil = true)
2. Check interval elapsed and activity occurred since last formation
3. Resolve prompt via `prompts.ResolvePrompt`
4. Fire branch: `branchFn("memory-formation", promptText, true)`

**Consolidation** (`maybeConsolidation`):
1. Check `consolidation_enabled` (nil = true)
2. Check consolidation interval elapsed (persisted in state store)
3. Check recent user activity (within 1h)
4. Resolve prompt via `prompts.ResolvePrompt`
5. Fire branch: `branchFn("consolidation", promptText, true)`
6. On completion: persist timestamp to state store

**Proactive warning dispatch** (`warnings.Dispatcher.MaybeFire`):
1. Check `queue != nil` and `dispatchFn != nil` — skip if no injection configured
2. Check `queue.Pending()` — skip if no warnings
3. Check `dispatching` guard — skip if dispatch in flight
4. Determine rate limit interval: call `lastUserMessageTimeFn()`, if within `activityThreshold` → use active interval, else → inactive interval
5. Check `sinceLastDispatch < interval` — skip if too soon
6. Drain warnings, format as `- ...\n- ...`, wrap via `formatFn` (wired to `prompts.FormatInjectedMessage`)
7. Dispatch in goroutine: `dispatchFn(text)`, clear `dispatching` on return

The `warnings.Dispatcher` is created in `main.go` and injected into `keepalive.RunnerConfig`. The keepalive timer loop calls `dispatcher.MaybeFire()` each tick. Warnings are only delivered via this proactive dispatch path — they always fire as independent agent turns rather than being bundled into user messages.

## Compaction (`compaction/compact.go`)

Checks token usage against threshold (default 80% of context window). When triggered:
1. Asks model (configurable) to summarize history using configurable prompt
2. Rotates the pre-compaction session file to a numbered archive (e.g. `5970082313.1.jsonl`) — old messages are preserved for usage tracking and audit
3. Writes the compacted session (context note + summary + continuation note) to the original file path
4. Appends any scratchpad entries to preservation message (scoped to agent via `Compactor.AgentID`)
5. If `CompactionNotifyFunc` is set, sends Telegram notification with session key and pre-compaction message count (configurable via `compaction_notify`, default true)

**Session file rotation:** `Replace()` in `session/store.go` renames the existing file before writing. Archive files use the pattern `{name}.{N}.jsonl` (N = 1, 2, 3...). The active session is always the unnumbered file. `Load`, `LoadFull`, `Append` etc. are unaffected — `keyToPath()` always resolves to the unnumbered path. `ListChatSessions`, `RepairOrphans`, and `InjectRestartMarkers` skip archive files.

**Session lifecycle events:** `Store.OnSessionEvent(func(SessionEvent))` fires on create (first `Append` to new file), branch create (`CreateBranchWithOptions`), compaction (`Replace`), and clear (`Clear`). Events carry the session key, type, status, parent key, file path, and timestamp. Used by `SessionIndex` to maintain a queryable SQLite index of all sessions.

**Async-pending guard:** Compaction is skipped when the session has pending async tool results (`AsyncNotifier.HasPending()`). Tools call `MarkPending()` before dispatching async work (spawn clone, auto-backgrounded exec/http) and `MarkDone()` when the result is delivered via `Notify()`. This prevents compacting away the context that the pending result relates to — compaction fires naturally on a later turn once all results have been delivered.

**Context warning for no_compact sessions:** When a session with `no_compact` flag (oneshot, wake branches) exceeds the compaction threshold, a warning is injected into the warning queue: "Context at ~X% capacity. This session cannot compact. Consider wrapping up." The agent sees this via `warnings.Dispatcher.MaybeFire()` on the next keepalive tick and can gracefully conclude rather than hitting the context limit unexpectedly.


**Branch compaction:** When `Replace()` is called on a branch session (e.g., during compaction), it preserves the `branch_meta` header with `branch_point=0`. The compacted messages are self-contained (the summary includes parent context), so subsequent `LoadFull()` loads `parent[:0] + compacted_msgs` = just the compacted messages.

**Configurable via `Compactor.WithConfig()`:**
- `model` — summarization model (default: agent model)
- `maxTokens` — max output tokens for summary (default: 4096)
- `minMessages` — min messages before compacting (default: 4)

**Passed to `Compact()` at call time** (not stored on the Compactor):
- `summaryPrompt` — read live from file at compaction time via `ReadPromptFile` callback. If empty, falls back to `prompts.CompactionSummary()` (embedded from `prompts/compaction-summary.md`). Edits to the config file take effect immediately.
- `handoffMessage` — message after compaction completes. If empty, uses `DefaultHandoffMessage` (embedded from `prompts/compaction-handoff.md`).
- `dryRun` — when true, runs the full pipeline (API call, summary generation) but skips `sessions.Replace()`. The session is left unchanged. `/compact dry-run` sends the resulting summary as a Telegram document (via `CompactionDebugFunc` if configured, otherwise directly via `primaryBot.SendDocument`) without rewriting history. Useful for iterating on compaction prompts.

## Deployment & Migrations

### setup.sh

`/home/rich/git/foci/setup.sh -u foci` — builds Go binaries, installs to `/usr/local/bin`, restarts service. Allowlisted in aisudo (no approval needed). Uses `--no-block` restart to avoid deadlock when run from foci's own exec.

### Migrations

Numbered scripts in `migrations/` (e.g. `001-homedir-restructure.sh`). Run manually during deploys that require filesystem or config changes beyond what the binary handles.

**Convention:**
- Scripts are idempotent (safe to run twice)
- Include `--dry-run` and `-h`/`--help`
- Must run while foci is stopped (script handles stop/start)
- Require root (`sudo`) for service control and file ownership

**Planned integration:** `setup.sh` will check for and run pending migrations between building binaries and restarting the service. A state file tracks which migrations have been applied.

**Current migrations:**
- `001-homedir-restructure.sh` — Moves flat home dir into `config/`, `data/`, `logs/`, `shared/` layout. Updates foci.toml paths, systemd unit, and crontab.

## Testing

```
go test ./...           # all tests (~66, runs in ~1s)
go test ./... -v        # verbose
go test ./session/...   # single package
```

The cache_test.go in `anthropic/` requires `ANTHROPIC_API_KEY` env var and hits the real API. All other tests are self-contained.
