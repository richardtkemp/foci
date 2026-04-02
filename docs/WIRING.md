# Foci — Wiring Diagram

How the pieces connect. Read this before touching the code.

## Startup Flow (`main.go`)

Each phase is extracted into its own file. `main()` is a ~400-line orchestrator.

```
config.Load(path)                                        ← validates values; logs to stderr + buffer

→ initLogging(cfg)                                       ← logging_init.go
  → log.Init, log.InitAPIDB, log.InitConversation, log rotation
  → returns cleanup func

→ initSecrets(configPath, cfg)                           ← secrets_init.go
  → secrets.Load(secretsPath)                            ← secrets.toml overrides foci.toml
  → [if bitwarden.enabled] bitwarden.New(executor, ttl) ← aisudo-backed vault store
  → seedDefaultPrompts (per-agent)
  → returns secretsResult{store, bwStore, httpAPIKey, cleanup}

→ newClientRegistry(cfg, store, ctx)                    ← clients.go
  → Lazy client registry: clients created on first use per endpoint:format pair (sync.Once)
  →   GetClient(endpoint, format) — lazy-init, returns provider.Client
  →   PeekClient(endpoint, format) — no-init check, returns nil if not yet created
  →   ResolveEndpointClient(endpoint, format) — validates format against endpoint support, calls GetClient

→ initSessions(cfg)                                      ← sessions_init.go
  → session.NewStore(dir)
  → sessions.RepairOrphans()                             ← fix interrupted tool calls before agents start
  → session.NewSessionIndex(session_index.db)             ← SQLite index; rebuilt on startup
  → sessions.OnSessionEvent(→ sessionIndex)               ← lifecycle hook: create/compact/clear → update index
  → migrateStateJSON(state.json → SQLite)             ← one-time migration, renames to state.json.migrated
  → returns sessionInfra{sessions, sessionIndex, cleanup}

→ initMemorySystem(cfg)                                  ← memory_init.go
  → memory: ReminderStore + Scratchpad + TodoStore + TaskListStore   ← always created; all per-agent (e.g. reminders-main.db)
  → memory backends (FTS5 and/or bleve)                  ← shared OR per-agent

   Shared resources (created once in main.go):
   → platform.InitMessaging(cfg, deps)                      ← initialises all registered providers (telegram, discord via blank imports)
     → each provider.Init(deps) creates its own bot manager, tool detail store, etc.
     → returns *platform.Messaging facade wrapping all active providers
   → voice STT/TTS providers                              ← shared across agents

   Per-agent loop (for each cfg.Agents[i]):
   → setupAgent(params)                                    ← agents.go → agentInstance{ag, cmds, registry, bootstrap}
     → resolveSharedSetup(params)                           ← agents_shared.go — config cascade, prompt dirs, group resolver
     → IF delegated agent (acfg.Backend != "" && != "api"):
       → configureDelegated(params, backend)                ← agents_delegated.go
         → backend.New(name, config)                        ← create backend via registry
         → workspace.NewBootstrap → system prompt            ← concatenate workspace *.md files
         → backend.Start(ctx, opts)                         ← spawn coding agent in tmux pane
         → shared.finalize(ag, params)                      ← commands, platform, nudge (shared postamble)
     → ELSE (traditional API agent):
       → tools.NewAsyncNotifier()                           ← shared by exec + http_request + tmux, routes by session key
       → tools.NewRegistry() + register all tools           ← per-agent registry (incl. bitwarden_search/unlock, browser if enabled)
       → mcp.NewManagerForAgent(configDir, agentID)         ← dynamic MCP; re-reads mcp.toml on each tool call
       → workspace.NewBootstrap(agent.Workspace, agent.SystemFiles)
       → buildEnvironmentBlock(acfg, configPath, cfg)       ← if [environment] enabled
       → skills.ResolveDirs(home, workspace, cfg.Skills.Dir, acfg.SkillsDir)
       → skills.Load(resolvedDirs)                          ← shared first, then per-agent (overrides on collision)
       → compaction.NewCompactor(sessions, model, threshold)
       → config.NewFallbackResolver(global, perAgent, aliases) ← nil if no fallbacks configured
       → agent.Agent{shared fields + Client, Tools, Bootstrap, EnvironmentBlock, FallbackResolver, ...}
       → shared.finalize(ag, params)                        ← commands, platform, nudge (shared postamble)
         → registerAgentCommands(cmdRegParams)              ← commands.go — all slash command registration
         → plat.SetupAgentConnection(AgentConnectionParams) ← creates platform connections (bots) for all active providers
           → returns []*platform.SetupResult with DefaultSessionKeyFn + ConfigureFacetConn
         → wireAgentPlatformCallbacks(ag, acfg, cfg, plat, connMgr, sessionIndex)
           → ag.AddPlatform() for each connection
           → wires CacheBustAlert, ManaWarnFunc, RateLimitFunc, etc. using plat.NotifyAgent()
  → agent.RestoreSessionOverrides(defaultSessionKey())   ← restore per-session effort/thinking/model from state store (main.go, after setupAgent)
  → agent.SeedSessionMeta(defaultSessionKey())           ← seed gap from session history (correct gap after restart)

  → setupKeepalive(inst, acfg, params)                    ← keepalive_setup.go (per-agent)
  → plat.SetupSharedFacet(...)                         ← shared facet bots (via messaging facade)
  → setupWarningHooks(agents, cfg)                         ← post_agent_setup.go
  → setupTmuxMemoryMonitor(...)                            ← post_agent_setup.go
  → setupMemoryGuard(...)                                  ← post_agent_setup.go

  → signal.Notify(SIGINT, SIGTERM)
  → plat.RestoreFacetSessions(...)                     ← restore bot→session mappings from state store
  → plat.StartAll(ctx)                                     ← starts all provider connections
  → startup notifications (inline in main.go)              ← uses connMgr.AllForAgent() for fan-out
  → http.Server{...}                                       ← http.go (registerHTTPHandlers)
  → handleRestartAndFirstRun(...)                          ← notifications.go (restart + welcome via HandleMessage)
  → block on signal → runShutdown(...)                     ← shutdown.go
```

**Multi-agent:** Each agent gets its own tool registry, command registry, workspace bootstrap, compactor, and platform connection(s). Each agent gets a `provider.Client` resolved from the `[groups]` configuration (the powerful group determines the agent's primary model/endpoint/format). Clients are lazy-initialized — only endpoints actually referenced create connections. Shared resources (session store, voice providers) are passed to each agent.

**Per-agent data:** All per-agent databases (conversation, reminders, scratchpad, todo, tasklist, memory indices) are stored in each agent's `workspace/.data/` directory. On startup, databases at the old shared `data_dir` location are automatically migrated to the workspace. Shared databases (api.db, state.db, sessions/) remain in `data_dir`.

**Per-agent memory:** When any agent has `[[agents.memory.sources]]` or overrides index-creation settings (`search_backend`, `reindex_debounce`, `conversation_weight`, `sweep_interval`), each agent gets its own search indices (`memory.db` for FTS5, `search.bleve` for bleve) in `workspace/.data/`, combining global `[memory]` sources with agent-specific sources. All `[memory]` settings are resolved per-agent via `Merge(acfg.Memory, cfg.Memory)`. Agent-specific sources receive a weight boost of +1.0. When no per-agent memory is configured, all agents share a single index in `data_dir` (backward compat).

**Agent routing:** `agentInstance` map keyed by agent ID. HTTP endpoints use `resolveAgent(id)` — returns first agent when ID is empty (backward compat).

## Shutdown Flow (`shutdown.go`)

```
SIGTERM/SIGINT received
  → runShutdown(agents, httpServer, botMgr, ...)   ← shutdown.go
    → stop keepalive timers (per-agent)
    → close HTTP server
    → gracefulShutdown(agents, timeout)             ← wait for in-flight agent turns
    → startup.RecordCleanShutdown()                 ← record timestamp for crash detection
    → close MCP managers                            ← disconnect from MCP servers
    → cancel context                                ← stops platform poll loops, triggers update ack
    → connMgr.Wait()                                ← block until all platform connections finish
  → deferred closes run (SQLite DBs, log files)
```

## Startup Diagnosis (`startup/diagnosis.go`)

On startup, classifies the restart type and includes diagnostics in the startup notification:

```
DiagnoseRestart(sessionIndex, startTime, logsDir)
  → read last_clean_shutdown from system_state table
  → read /proc/uptime for system uptime
  → classify:
     - clean: shutdown < 5 min before startup
     - crash: shutdown > 5 min, system uptime > gap
     - reboot: system uptime < shutdown gap (system restarted)
     - unknown: no prior shutdown record
  → for crash/reboot: gatherDiagnostics() scans foci.log for ERROR/FATAL lines
  → return DiagnosisResult{Class, Diagnostics, Summary}
```

**Platform notification:** Startup notifications fan out to all connections via `connMgr.AllForAgent()`. The diagnosis text is appended to the restart message. Clean restarts get no extra text. Crashes show "⚠️ Unexpected restart" with error lines. Reboots show "🔄 System reboot detected".

**State key:** `system:last_clean_shutdown` holds Unix timestamp of last graceful shutdown.

## Package Dependency Graph

```
main
 ├── config        → display, modelinfo
 ├── sqlite        → modernc.org/sqlite (shared Open, AgentPath, MigrateFile utilities)
 ├── log           → sqlite, modelinfo
 ├── display       (no deps — table rendering with Unicode display-width handling)
 ├── secrets       → BurntSushi/toml
 │   └── secrets/bitwarden → log
 ├── provider      (no deps — provider-neutral types and Client interface)
 ├── platform      → config, log, secrets, session, state, voice, warnings
 │                  (messaging types, interfaces, provider registry, Messaging facade,
 │                   shared MessageQueue + GroupThrottle for inbound message routing)
 ├── anthropic     → provider, github.com/anthropics/anthropic-sdk-go
 ├── gemini        → provider, google.golang.org/genai
 ├── openai        → provider, github.com/openai/openai-go/v3
 ├── session       → provider, log, sqlite
 ├── memory        → sqlite, fsnotify, blevesearch/bleve/v2 (FTS5 + bleve backends)
 ├── voice         → config, log, session, tempdir, gorilla/websocket
 ├── skills        → log (leaf package)
 ├── startup       → log, state (leaf package for crash detection)
 ├── resources     → log (goroutine monitor, memory guard)
 ├── mcp           → provider, log, tools, BurntSushi/toml, go-sdk/mcp
 ├── tools         → anthropic, config, display, log, memory, modelinfo, platform, provider, secrets, secrets/bitwarden, session, state, tempdir, tools/browserjs, voice
 ├── workspace     → log, provider
 ├── nudge         → log (leaf — rule extraction, scheduling, file I/O)
 ├── prompts       (top-level package, not internal) → log (embedded .md files + ResolveOrientationTemplate helpers)
 ├── modelinfo     (no deps — stdlib-only leaf package for model attributes: context window, capabilities, pricing)
 ├── compaction    → log, memory, modelinfo, provider, session, tools
 ├── tempdir       (no deps — stdlib-only leaf package for canonical temp dir)
 ├── provision     (no deps — stdlib-only leaf package for agent creation)
 ├── command       → agent, compaction, config, display, log, mana, memory, platform, provider, provision, session, skills, state, tempdir, tools, workspace
 ├── mana          → anthropic, log, provider (mana budget logic)
 ├── warnings      → log (leaf — warning queue and proactive dispatch)
 ├── backend       (no deps — Backend interface, registry, StartOptions, EventHandler)
 │   └── backend/claudecode → backend, fsnotify (Claude Code implementation; registers via init())
 ├── agent         → backend, compaction, config, display, log, mana, memory, nudge, platform, provider, session, state, tools, warnings, workspace
 ├── periodic     → config, log, memory, provider, state, warnings (NO agent, NO session)
 ├── dispatch      → command, session (shared command dispatch logic; platform wrappers delegate here)
 ├── turn          → display, log, toolformat (shared turn rendering + tool call tracking for all platforms)
 ├── telegram      → agent, chatmeta, command, config, dispatch, display, log, platform, secrets, session, sqlite, state, tooldetail, toolformat, turn, voice
 │                  (registers via init() → platform.RegisterMessagingProvider; blank-imported in main.go)
 └── discord       → agent, chatmeta, command, config, dispatch, display, log, platform, secrets, session, sqlite, state, tooldetail, toolformat, turn, voice
                    (registers via init() → platform.RegisterMessagingProvider; blank-imported in main.go)
```

No circular dependencies. `provider`, `display`, `log`, `secrets`, `memory`, `skills`, `prompts`, `startup`, `resources`, `provision`, `tempdir`, `mana`, `warnings`, `modelinfo`, `turn`, `dispatch` are leaf packages. `platform` depends on leaf packages only (config, log, secrets, session, state, voice, warnings).

**`provider` package:** Defines the neutral types (`Message`, `ContentBlock`, `ToolDef`, etc.) and the `Client` interface (`SendMessage`, `CountTokens`). `anthropic`, `gemini`, and `openai` all implement `provider.Client`, translating between neutral types and their wire formats.

**`platform` package:** Defines platform-agnostic messaging types (`Message`, `Attachment`), the `Connection`/`ConnectionManager` interfaces, the `MessagingProvider` interface for platform implementations, and the `Messaging` facade that manages all active providers. Providers register via `RegisterMessagingProvider()` (called from `init()`) and are activated at startup via `InitMessaging()`. An aggregating `ConnectionManager` merges connections from all providers — `AllForAgent()` returns connections across all platforms, enabling multi-platform fan-out for notifications. `cmd/foci-gw/` uses only the facade; zero platform-specific type references. Also defines the `SetupWizard` interface (optionally implemented by `MessagingProvider`) for contributing interactive setup steps to `foci first-run`. `SetupProviders()` returns all registered providers that implement `SetupWizard`. Types: `SetupFlag` (CLI flag definition), `WizardResult` (config TOML fragment + secrets), `SetupUI` (console interaction primitives).

**`chatmeta` package:** Shared session key management logic extracted from `telegram` and `discord`. Provides `Resolver` — a lightweight struct that looks up, creates, persists, and rotates per-chat session keys via `platform.SessionIndex`. Each platform `Bot` holds a `*chatmeta.Resolver` and delegates `SessionKeyForChat`, `UpdateSessionKey`, `DefaultChatID`, `DefaultSessionKey`, and `RecordUsername` to it. Platform-specific methods (`SessionKey`, `SetSessionKey`, `ChatID`, `SetChatID`, `Username`) remain on each Bot. Imports: `platform`, `session`, `log`. All methods are nil-receiver safe.

Most packages depend on `provider` for types; only `main.go`, `tools`, and `mana` import `anthropic` directly (for Anthropic-specific features like `UsageClient`). `periodic` no longer imports `agent` or `session` — mana monitoring and warning dispatch are handled by the `mana` and `warnings` packages respectively, wired together in `main.go`.

**`provision` package:** Shared agent creation logic used by both `cmd/foci/setup.go` (first-run wizard) and `command/agents_new.go` (`/agents new` runtime command). Stdlib-only, no imports from other foci packages. Provides `AgentSpec` + `Provision()` (workspace creation, character file copying, SOUL.md templating), validation (`IsValidAgentID`), config block generation (`GenerateAgentBlock`), and crontab templating (`GenerateCrontab`, `AppendCrontab`). Platform-specific validators (e.g. `IsValidBotToken`, `IsValidUserID`) live in their respective platform packages (e.g. `internal/telegram/validate.go`).

## Command Dispatch Architecture

Slash commands (`/ping`, `/model`, etc.) are dispatched through a three-layer architecture:

1. **Platform wrapper** (`telegram/dispatch.go`, `discord/dispatch.go`): Thin wrappers that extract `text`, `chatID`, and `userID` from platform-native message types (`gotgbot.Message`, `discordgo.Message`) and delegate to the shared dispatcher.

2. **Shared dispatch** (`dispatch/dispatcher.go`): Platform-agnostic routing logic. Detects dot-commands (`.model`) vs slash-commands (`/model`), resolves session keys, and builds a `command.Request`. Returns a `dispatch.Result` with `Handled`, `Response`, `SessionKey`, `UserID`.

3. **Command layer** (`command/registry.go`): Receives `Request` and `CommandContext` (platform-agnostic dependencies), executes the command, and returns a `Response` with `Text` and optional `DocPath`.

**Dispatch flow:**
```
Telegram message "/model haiku"
    ↓
telegram.Dispatcher.Dispatch(ctx, msg)
    ↓ extracts msg.Text, msg.Chat.Id, msg.From.Id
dispatch.Dispatcher.DispatchText(ctx, "/model haiku", chatID, userID)
    ↓ parses "/model" + "haiku", resolves session key
command.Request{Name: "model", Args: "haiku", SessionKey: "...", UserID: "..."}
    ↓
command.Registry.Dispatch(ctx, req, cc)
    ↓ executes with command.CommandContext
dispatch.Result{Handled: true, Response: command.Response{Text: "Model set to haiku"}}
    ↓
Telegram renders response (markdown, keyboards, etc.)
```

All commands use a unified signature: `Execute(ctx context.Context, req Request, cc CommandContext) (Response, error)`. The `CommandContext` struct provides all dependencies (Agent, Sessions, Config, client references, etc.) — no per-command closure constructors.

**Key types:**
- `command.Request`: Platform-agnostic command invocation (`Name`, `Args`, `SessionKey`, `UserID`, `ChatID`)
- `command.Response`: Platform-agnostic result (`Text`, `DocPath`)
- `command.CommandContext`: Platform-agnostic dependencies struct (Agent, Sessions, Config, client references, stores, paths, etc.)
- `command.Registry.Dispatch()`: Executes commands using `(ctx, Request, CommandContext)`
- `dispatch.Dispatcher`: Shared routing logic (dot/slash detection, session key resolution, request building)
- `dispatch.Result`: Dispatch outcome (`Handled`, `Response`, `SessionKey`, `UserID`)

**Why this split:** The platform wrappers own only the extraction of text/chatID/userID from native message types — typically 5-10 lines of code each. The shared `dispatch` package owns all routing logic (dot-command detection, slash-command parsing, session key resolution, `command.Request` construction). The `command` layer owns what commands do. Adding a new platform requires only a thin wrapper that extracts three values from the native message type.

## The Agent Loop (`agent/agent.go`)

The core of the system. Two entry points:
- `HandleMessage(ctx, sessionKey, text)` — text-only, delegates to `HandleMessageWithAttachments`
- `HandleMessageWithAttachments(ctx, sessionKey, text, attachments)` — full version with optional image/document attachments

**Delegated agents:** When `Agent.DelegatedManager != nil`, `HandleMessageWithAttachments` branches to `DelegatedTransport` (`turn_delegated.go`) instead of the traditional loop below. The delegated path composes a prompt (metadata, reminders, nudges, state dashboard, attachment paths, user text) via the shared `composeTurnText` (`turn_common.go`), sends it to the coding agent via `Backend.SendToPane()` (fire-and-forget), and returns immediately. Output is delivered asynchronously: the backend's session file watcher calls `SetReplyFunc` which routes text to the correct platform connection via `connMgr.ForSessionOrPrimary()`. Permission prompts are detected by scraping the tmux pane and forwarded to the user. See [docs/CONFIG.md — Coding Agent Backends](CONFIG.md#coding-agent-backends).

### RunOnce Mode (`DelegatedManager.RunOnce`)

Non-interactive backend execution for headless tasks. `RunOnce(ctx, prompt, systemPrompt)` spawns `claude --print --dangerously-skip-permissions --no-session-persistence --model sonnet`, captures stdout synchronously, and returns the response text. No tmux pane, no watcher, no session index — a one-shot subprocess call.

Used by:
- **Nudge extraction** — `ExtractViaRunOnce` sends conversation context to the model and parses structured nudge rules from the response.
- **Consolidation** — The periodic `Runner` is wired with a `RunOnceFunc` for memory consolidation tasks that don't need an interactive session.

### Backend Watcher (`internal/backend/claudecode/watcher.go`)

The session watcher tails Claude Code's JSONL session file via fsnotify. It converts raw JSONL events into structured callbacks (assistant text, turn completion, usage, agent status).

**Pre-send offset:** Before `SendToPane` pastes the prompt into the tmux pane, the watcher records the current JSONL file size. The watcher starts reading from this offset so it doesn't replay old content from earlier turns. Falls back to `-1` (tail from end of file) if the offset discovery fails.

**Synthetic response filter:** Claude Code emits synthetic messages (model: `<synthetic>`) such as `"No response requested."` and `"[[NO_RESPONSE]]"`. The watcher filters these at the event level — they never reach the reply callback.

**Typing indicator:** `SetTypingFunc` on the Backend registers a callback. Set to `true` on `SendToPane` (prompt pasted), set to `false` when `fireTurnComplete` fires (end_turn seen). The platform `Connection.SetTyping(bool)` is stateful — `true` starts a periodic ticker (Telegram: 4s, Discord: 9s) that keeps the indicator alive until `false` is called. This replaces the per-platform manual tickers that previously lived in the bot workers.

**Usage extraction:** Assistant messages in the JSONL carry a `usage` payload. The watcher extracts `TurnUsage` (InputTokens, OutputTokens, CacheCreationInputTokens, CacheReadInputTokens) from the last assistant message in each turn. This is reported via `TurnState.FinalUsage` on completion.

**Per-turn completion callbacks:** `SendToPane` registers a one-shot `turnCompleteFn` that fires when the watcher sees `end_turn` in the JSONL. The callback sets `TurnState.FinalText` and `TurnState.FinalUsage`, then closes `TurnState.CompletionChan` — triggering the post-turn goroutine (save, metadata, compaction, logging).

**Agent spawn tracking:** The watcher tracks pending `tool_use` calls for the Agent tool. When a sub-agent is spawned, status is reported via the `onAgentStatus` callback, allowing the platform to show agent activity state.

**Permission auto-approval:** When CC sends a `can_use_tool` permission request, the ccstream backend first checks against compiled auto-approve rules (from `[permissions]` config). Rules are assembled at startup by `buildAutoApproveRules`: built-in common readonly tools/commands (if `auto_approve_common_readonly` is true), workspace-scoped Edit/Write access, and user-configured patterns from global + per-agent config (union). Matched requests are approved directly via `SendControlResponse` with an INFO log. Unmatched requests are forwarded to the user via the platform connection with an inline keyboard of choices (Allow, Deny, Always Allow).

### Backend Session Lifecycle

**Session ID persistence:** `SetOnSessionReady` registers a callback that fires when the watcher discovers the CC session UUID from the JSONL path. The UUID is persisted in the state store. On restart, `--resume <sessionID>` is passed to the `claude` command to reconnect to the existing CC session rather than starting fresh.

**Stable exec bridge sockets:** The exec bridge socket path for delegated agents is derived from the session key (not a random value). This means CC retains the same `FOCI_SOCK` environment variable path across foci restarts — shell functions piped through the bridge continue to work without re-sourcing.

**Branch rejection:** Delegated agents return HTTP 400 for `/branch` endpoint requests. The three task-type strategies:
- **Inject into main session** — memory-formation and compaction-memory prompts are sent directly into the running CC session (no branch needed).
- **New independent CC session** — consolidation, background tasks, and nudge extraction use `RunOnce` (see above), which spawns an independent headless CC process.
- **Reject** — the HTTP `/branch` endpoint is explicitly rejected since delegated agents don't support session branching.

**/reset:** Sends the memory formation prompt to the CC session, waits for completion, kills the tmux pane, and starts a fresh CC session. The session ID is cleared from the state store and a new one is persisted on reconnection.

**/stop:** Sends Escape×2 + Ctrl-C to the CC TUI via tmux `send-keys` to interrupt the current turn. This halts the in-flight inference/tool execution inside Claude Code.

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
4b. nudge StartTurn + prepend regex/every_n_turns nudge ContentBlocks to user message (if any triggers fire)
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
   f. if stop_reason == "end_turn":
      - if nudge pre-answer gate enabled and not yet verified → inject [system] reminder, continue loop
      - otherwise → save & check compaction & return text
   g. if stop_reason == "tool_use":
      - execute each tool_use via registry (skip server_tool_use — already executed)
      - track tool call count and error state
      - inject nudge reminders based on trigger conditions (every_n_tools for braindead warning, after_error, regex)
      - append assistant msg + tool_result msg
      - goto 7a
8. sessions.AppendAll(sessionKey, newMessages)
9. maybeCompact: main threshold + mana-refresh check → possibly compactor.Compact(sessionKey)
```

Messages are only saved to disk after the full turn completes (all tool loops resolved). Compaction runs after save; two automatic triggers: main threshold and mana-refresh (see below).

**Error handling by status code:**
- **429 (rate limit):** Could be burst rate limit or daily quota exhaustion. `classifyAPIError` fires `RateLimitFunc` callback (Telegram notification with estimated retry time from `Retry-After` header) and returns `"rate limited"`. The rate limit gate closes using the `Retry-After` header duration, or 60s if the header is absent (e.g. streaming SSE errors). No transport-level retry.
- **529 (overloaded):** Anthropic servers are overloaded (their problem, not ours). Two-phase retry in `SendMessage`: phase 1 retries 3× with exponential backoff (2s→4s→8s, same as other retryable errors); phase 2 (529 only) enters an extended duration-based loop retrying up to ~2 hours with 5s base backoff doubling without cap. A cross-goroutine recovery signal on the `Client` wakes all sleeping retry loops when any `SendMessage` succeeds (proving the server has recovered). If still failing after phase 2, `classifyAPIError` returns `"API is overloaded (HTTP 529) — try again shortly"`.
- **500/502/503 (server error):** `SendMessage` retries 3× with backoff. If still failing, `classifyAPIError` fires `RateLimitFunc(0)` and returns a temporary unavailability message.

**Model fallback** (`[groups.fallbacks]`): `provider.Send` handles the full error recovery pipeline: (1) retry with backoff, (2) strip unsupported params (thinking/effort/speed) on 400 and retry, (3) walk the fallback chain on transient errors (529, 5xx, `context.DeadlineExceeded`). Each fallback hop resolves the model's endpoint/format via `ClientProvider.GetClient` and retries. On success, the response is used; subsequent tool-loop iterations rebuild with the primary model (fallback is per-request, not sticky). All API call sites use `provider.Send` — main agent loop, compaction, spawn one-shot, summary tool, auto-summary, and prompt-diff all have fallback support. Not triggered by 401 or 429. Configured via `[groups.fallbacks]` (global) and per-agent `[groups.fallbacks]` override. Max chain depth: 3.

### Cache Stability Invariant

Conversation history sent to the API must be a strict append-only extension of the previous request — inserting a message in the middle invalidates all cached tokens after that point. `HandleMessageWithAttachments` enforces this via a per-session turn lock that serializes all callers (Telegram, `AsyncNotifier`, scheduled wakes, HTTP `/send`). Different sessions run concurrently. See [CACHING.md](CACHING.md) for the full cache stability contract.

## Message Metadata

**Message transforms** (`[[message_transforms]]` in config) run regex find/replace on inbound user messages. Transforms fire before command dispatch — if a message is already a recognized command, transforms are skipped. If transforms produce a command (e.g. `m` → `/mana`), it is dispatched as one. Rules run in sequence; each rule's output becomes the next rule's input.

Each user message then gets a metadata line prepended (NOT in system prompt — that would bust cache):

```
[meta] time=2026-02-21T05:30:00Z gap=3h12m model=claude-haiku-4-5 via=telegram prev_cost=$0.0430 prev_tokens=in:2400/out:312/cR:18000/cW:200
```

- `time` — current UTC timestamp
- `gap` — human-readable time since previous message ("3h12m", "2d4h", "38s", "none")
- `model` — current model name (e.g., "claude-haiku-4-5", "claude-opus-4-6")
- `via` — transport that delivered the message. Derived from the context trigger via `triggerToPlatform()` in `context.go`. Values: `telegram` (Telegram/voice), `discord` (Discord), `android` (Android app), `api` (HTTP /send), `cron` (system-initiated: keepalive, wake, scheduled, etc.)
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

**Inline keyboard commands:** Commands with a `KeyboardOptions` field (`/model`, `/thinking`, `/effort`, `/config`, `/sessions`, `/tmux`) show an inline keyboard when invoked bare. `LookupKeyboard()` checks for this before `Dispatch()`. `sendCommandKeyboard()` builds and sends the keyboard via `platform.ButtonSender`. Callback data format: `cmd:/name args`. `handleCommandCallback()` executes the command and edits the message to show the result. `command.KeyboardOption` is aliased to `platform.ButtonChoice` (Label, Data, Row fields) — the same type used for all button interactions across both Telegram and Discord.

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
[state] task: 3/7 "Boil an egg" → Bring water to rolling boil | todos: 2 open (1 high) | scratchpad: 1 entry
Hello, what should I work on?
```

## Scratchpad

Working state that survives compaction but isn't permanent memory. The agent writes notes during investigations and clears them when done.

**Storage:** `Scratchpad` in `memory/scratchpad.go`. SQLite table `scratchpad` with columns: `agent_id`, `key` (composite primary key), `content`, `updated`. Per-agent database file (`scratchpad-{agentID}.db`).

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

## Task List

CRUD task tracker. Individual tasks with auto-incrementing IDs per agent (not per-session). The agent creates tasks, updates their status, and lists progress.

**Storage:** `TaskListStore` in `memory/tasklist.go`. SQLite table `tasks` with columns: `id` (integer), `agent_id` (text), `subject`, `description`, `status` (pending/in_progress/completed), `created_at`, `updated_at`. Primary key is `(agent_id, id)`. Stored in `tasklist.db`. Scoped per-agent.

**Tool:** `task_list(action, ...)` — actions: create, get, update, list. `update` with `status="deleted"` removes a task. Display uses `→` for in_progress, `✓` for completed.

**Compaction survival:** When compaction fires, active tasks are serialized and appended to the handoff message as a `[task list]` block, similar to scratchpad.

**State dashboard:** A `[state]` line is injected into every user message (in `prepareUserMessage`, after `[reminders]`) showing a one-line summary of active stores. Components shown only when non-empty: task progress (`tasks: 2/5 → first active`), open todo count, scratchpad entry count. Queries `TaskListStore`, `TodoStore`, and `ScratchpadStore` on the Agent struct.

**Example task list display:**
```
Tasks: 2/5 completed
  1. ✓ Fill pot with water
  2. ✓ Place pot on stove
  3. → Bring water to rolling boil
  4.   Gently lower egg into water
  5.   Set timer
```

## Session Storage

**Format:** JSONL files, one JSON-encoded `provider.Message` per line.

**Key format:** `{agentID}/{type}{id}/{versionTS}[/{childType}{childTS}][.{n}]`

**Type codes:**
- `c` — chat (Telegram, external stable ID)
- `i` — independent (HTTP, ephemeral)
- Child types: `b` (branch), `i` (independent spawn)

**Key → Path mapping:**
```
Root sessions:   {key}/root.jsonl
Child sessions:  {key}.jsonl

Examples:
main/c123/1709590000                    → sessions/main/c123/1709590000/root.jsonl
main/c123/1709590000/b1709596800        → sessions/main/c123/1709590000/b1709596800.jsonl
main/i1709596800/1709596800             → sessions/main/i1709596800/1709596800/root.jsonl
```

**Versioning:** Each chat/independent session has version directories (created at first message, incremented on compaction). When compacted, the old `root.jsonl` is rotated to `root.{timestamp}.jsonl` and a new version directory is created. Children remain in their original version directories. This allows stable chat IDs across compactions while preserving compaction history.

**Branching:** Branch files start with a `{"type":"branch_meta",...}` line containing `parent_key` and `branch_point`. `LoadFull()` reads parent[:branch_point] + branch's own messages. This is what makes cache sharing work — the API sees the same prefix bytes.

**See also:** [SESSION_KEYS.md](SESSION_KEYS.md) for complete format specification, migration guide, and API reference.

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

## Provider Interface (`provider/`)

Provider-neutral types and `Client` interface. All packages use `provider.Message`, `provider.ContentBlock`, `provider.ToolDef`, etc. — the concrete API client translates at the wire boundary.

```go
type Client interface {
    SendMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error)
    CountTokens(ctx context.Context, req *MessageRequest) (int, error)
}

type StreamingClient interface {
    StreamMessage(ctx context.Context, req *MessageRequest, handler *StreamHandler) (*MessageResponse, error)
}
```

`StreamingClient` is opt-in — the agent loop type-asserts `provider.StreamingClient` when `Streaming = true`. The Anthropic and OpenAI clients implement it. `StreamHandler` has `OnTextDelta` and `OnThinkingDelta` callbacks for incremental delivery.

### Dynamic Provider Switching

Agents can switch endpoints at runtime via `/model endpoint:name` (e.g. `/model gemini:flash`, `/model anthropic:haiku`, `/model openrouter:opus`). The model field always uses `endpoint:model_id` format.

**Three independent concepts:**

| Concept | Example | Determines |
|---------|---------|------------|
| **Endpoint** | `openrouter` | Base URL, API key |
| **Wire format** | `anthropic`, `openai`, `gemini` | Which Go client serializes the request |
| **Model ID** | `claude-opus-4-6` | String passed in the API call |

**Format resolution:** `config.ResolveModel()` resolves the wire format once at startup (or `/model` switch) from the developer prefix: `anthropic/*` → anthropic format, `google/*` → gemini format, `openai/*` → openai format, unknown → openai (universal fallback). The resolved format is persisted on `Agent.Format` and `sessionMeta.modelFormat` — it is never re-inferred from the model name. Multi-format endpoints (like openrouter with both `anthropic_url` and `openai_url`) auto-select the right URL based on the stored format.

**Resolution chain:**
1. `/model openrouter:anthropic/claude-opus-4-6` → parse developer `anthropic`, but user specified `openrouter` → endpoint=`openrouter`, format=`anthropic`
2. `ResolveEndpointClient("openrouter", "anthropic")` → lazy-init anthropic client for openrouter endpoint
3. Per-session client override stored in `sessionMeta.client`, endpoint in `sessionMeta.modelEndpoint`, format in `sessionMeta.modelFormat`
4. On next API call, `HandleMessage` uses `SessionClient(sessionKey)` → returns per-session client or agent default

**Wiring:** `agent.ClientProvider` implements `provider.ClientProvider` and delegates to the lazy client registry in `main.go`. This is shared with `tools.SpawnDeps` and `tools.NewSummaryTool` so spawns and auto-summaries also route to the correct provider.

**Model Group Resolution:** The `[groups] powerful` key determines the primary model. Per-agent `[groups]` overrides (powerful, fast, cheap, calls, fallbacks) are merged with global via `config.Merge` + `config.MergeMaps` — per-agent wins. A `config.GroupResolver` (created per-agent at startup from the merged `GroupsConfig`) maps call sites to model groups (`powerful`, `fast`, `cheap`), resolving each to a concrete `developer/model_id`. The unified entry point is `agent.ResolveCallSite(callSite, sessionKey)` — it returns a `(client, model, format)` triple. It delegates to `GroupResolver.ResolveCall(callSite)` which looks up the call site's group (with optional per-call overrides from `[groups.calls]`), resolves the group's model, and fetches the appropriate client from `ClientProvider`. All internal call sites (compaction, guard summaries, spawns, prompt-diff) use `ResolveCallSite` instead of directly accessing the session model.

**Per-model defaults:** `[models.*]` config sections define named models with per-model settings (thinking, effort, speed, etc.). These serve as both aliases (usable in `[groups]`, fallbacks, and `/model` command) and default API parameters. At request time, the hierarchy is: session override (via `/effort` etc.) → model config default → empty (API decides). The `ModelDefaultsFn` closure on both `Agent` and `Compactor` performs the reverse lookup from `developer/model_id` to `ModelConfig`.

**Compaction:** `Compactor.Compact()` receives the client, model, and format as parameters (not stored on the struct). The caller resolves these via `agent.ResolveCallSite(config.CallCompaction, sessionKey)`, so compaction uses the group-appropriate model in multi-model mode or the session's active client in single-model mode.

**Keepalive:** For Anthropic endpoints, the keepalive fires on a configurable interval (default 55m, just under the 1h cache TTL). For OpenAI and DeepSeek models, keepalive is auto-detected by developer name via `config.ResolveModelKeepalive()` — these developers have a 5-minute prompt cache TTL, so keepalive fires every ~4m45s. Gemini's `CacheManager` handles its own TTL extension independently.

## Anthropic API Client (`anthropic/`)

Implements `provider.Client` and `provider.StreamingClient`. Uses the official `github.com/anthropics/anthropic-sdk-go` SDK.

**Transport:** `sendOnce()` sends requests via the SDK's `Messages.New()`. Same pattern for `CountTokens` and `ListModels`. The transport is wrapped by two-phase retry logic: Phase 1 (3 retries with exponential backoff on 500/502/503/529) and Phase 2 (extended overload recovery with cross-goroutine signaling on 529). The SDK client is initialized lazily (`sync.Once`) and configured with `WithMaxRetries(0)` since retry logic is handled externally.

**Translation layer** (`translate.go`): converts between provider-neutral types and SDK types at the boundary. `buildSDKParams()` translates `MessageRequest` → `MessageNewParams`. `responseFromSDK()` translates back. `classifySDKError()` maps SDK errors → `provider.APIError`. Custom tools use typed SDK fields; server tools and documents use raw JSON passthrough via `param.Override`.

**Streaming** (`stream.go`): `StreamMessage()` wraps `streamOnce()` with the same two-phase retry logic. Pre-stream errors (before any deltas) are retried; mid-stream errors are not (deltas already emitted). `streamOnce()` calls `Messages.NewStreaming()`, iterates events, fires `StreamHandler.OnTextDelta` / `OnThinkingDelta` callbacks, uses `Message.Accumulate()` for response assembly. Enabled per-agent via `streaming = true`.

Three clients (two token types — see [docs/AUTH.md](AUTH.md)):

1. **Client** (`client.go`) — messages API + token counting + streaming
   - Sends model requests with system prompt + conversation history
   - Also handles `/v1/messages/count_tokens` for `/context` command
   - Supports static token (`NewClientWithTimeout`) or dynamic token func (`NewClientWithTokenFunc`)
   - Per-request auth via `option.WithAuthToken(token)` (SDK path) or manual header (raw path)
   - Sets `anthropic-beta: oauth-2025-04-20` header for OAuth token auth

2. **UsageClient** (`usage.go`) — mana/usage API
   - Queries `/api/oauth/usage` endpoint
   - Supports static token (`NewUsageClient`) or dynamic token func (`NewUsageClientWithFunc`)
   - Returns utilization for 5-hour window, 7-day limits, extra usage billing

3. **CCTokenSource** (`cctoken.go`) — Claude Code credential reader
   - Reads `~/.claude/.credentials.json` lazily on each `Token()` call (no polling)
   - Never refreshes tokens itself — only reads what Claude Code writes
   - If token is expired on read, triggers background refresh (runs `claude`) and returns error
   - `CheckRefresh()` called by UsageClient after successful API fetch — triggers proactive refresh when token is within `cc_expiry_threshold` (default 5m) of expiry
   - Provides `Token()` func used by both Client and UsageClient via tokenFunc

## Gemini API Client (`gemini/`)

Implements `provider.Client` using `google.golang.org/genai` SDK. Translation layer converts between provider-neutral types and Gemini wire format:
- `messagesToGenai()` — role mapping (`assistant` → `model`), content block → Part translation, `tool_use` → `FunctionCall`, `tool_result` → `FunctionResponse`
- `toolsToGenai()` — JSON Schema → `genai.Schema`, server tools filtered out
- `responseFromGenai()` — finish reason mapping, usage extraction, `FunctionCall` → `tool_use` ContentBlock
- `classifyError()` — maps Gemini SDK errors to `provider.APIError` for agent loop retry logic
- `CacheManager` — explicit server-side cache for system prompt + tools (see below)

## OpenAI API Client (`openai/`)

Implements `provider.Client` and `provider.StreamingClient` using `github.com/openai/openai-go/v3` SDK. Translation layer converts between provider-neutral types and OpenAI wire format:
- `messagesToOpenAI()` — system blocks → `DeveloperMessage`, tool results → `ToolMessage`, images → `image_url` parts
- `toolsToOpenAI()` — `ToolDef` → `ChatCompletionFunctionTool`, server tools filtered out
- `responseFromOpenAI()` — finish reason mapping (`"stop"` → `"end_turn"`, `"tool_calls"` → `"tool_use"`), usage extraction, `ToolCalls` → `tool_use` ContentBlock
- `classifyError()` — maps SDK `*openai.Error` to `provider.APIError`
- `CountTokens()` — returns error (no free token counting endpoint); compaction handles gracefully
- Configurable base URL (`[openai] base_url`) enables OpenRouter, Together, Groq, local LLMs

**Streaming** (`stream.go`): `StreamMessage()` wraps `streamOnce()`. Pre-stream errors (before any deltas) are retryable; mid-stream errors are not (deltas already emitted). `streamOnce()` calls `Chat.Completions.NewStreaming()` with `include_usage: true`, iterates chunks, fires `StreamHandler.OnTextDelta` callbacks, uses `ChatCompletionAccumulator` for response assembly. OpenRouter `reasoning_content` extra fields on deltas are accumulated manually and fire `OnThinkingDelta` callbacks. Enabled per-agent via `streaming = true`.

## Prompt Caching

**Anthropic:** Two `cache_control: ephemeral` breakpoints per API request: one on the system prompt (`bootstrap.SystemBlocks()`), one on the second-to-last conversation message (`withCacheBreakpoint()` in `agent.go`). Breakpoints are added only to the API request payload, never persisted to session storage. See [CACHING.md](CACHING.md) for the full cache architecture, stability invariant, and monitoring.

**Gemini:** Explicit cache objects via `CacheManager` in `gemini/cache.go`. The system instruction and tools are hashed (MD5) and cached server-side with a configurable TTL (`[gemini] cache_ttl`, default `"1h"`). When a cache is active, `SendMessage` passes the cache name via `CachedContent` and omits `SystemInstruction`/`Tools` from the request. The cache is extended at the TTL halfway point to prevent expiry during active use, recreated on content change, and deleted on shutdown via `Client.Close()`.

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
     initialized at construction with a prefix like `"agent/mybot"`. This avoids repeating
     the component string at every call site and encodes the agent ID for multi-agent setups.
   - Levels: DEBUG < INFO < WARN < ERROR
   - Newlines in messages are replaced with literal `\n` to guarantee one log line per event

2. **API log — JSONL** (`api.jsonl`): One JSON object per Anthropic API call with ts, session, model, token counts, cost_usd, duration_ms.
   - Use: `log.API(log.APIEntry{...})`
   - Queryable with `jq`

3. **API log — SQLite** (`api.db`): Same data as JSONL but in a `api_calls` table with indexes on `ts` and `session`. Includes `call_type` column (conversation, compaction, summary, spawn).
   - Written automatically by `log.API()` when `api_db` is configured
   - Queryable: `sqlite3 api.db "SELECT call_type, count(*) FROM api_calls GROUP BY call_type"`

4. **Conversation log** (`conversation-{agentID}.db`): Per-agent SQLite databases logging exact Telegram messages sent and received. Entries are routed to the correct agent's database by parsing the session key. Table `messages` with columns: `id`, `ts`, `direction` (recv/sent), `user_id`, `username`, `chat_id`, `text`, `parse_mode`, `session`, `error`.
   - Use: `log.Conversation(log.ConversationEntry{...})`
   - Queryable with `sqlite3 conversation-clutch.db "SELECT * FROM messages"`
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
| `spawn` | spawn.go | Unified sub-call: four context modes. All modes have tool access with a tool-call loop. `raw`: one-shot, no system prompt (`send_to_chat` and `send_to_session` blacklisted — no character context means no communication awareness). `character`: one-shot with character files (all tools). `clone` (default): branch session — a headless self-fork. `explore`: one-shot safe exploration with `ls`, `find`, `grep`, `read`, `memory_search`, `web_search`, `web_fetch` only — no file mutation, no shell exec, no messaging, always haiku. clone creates branch `{parentKey}/b{TIMESTAMP}`, always runs async via `AsyncNotifier` (returns immediate ack, delivers `[SPAWN RESULT]` on completion). Recursive clone blocked via context key. Concurrent clone limited by `max_concurrent_spawns` (default 3). `spawn` itself is excluded from one-shot tool sets to prevent recursion. |
| `ls` | explore.go | List directory contents. Internal to `explore` spawn mode — not registered in the main tool registry. |
| `find` | explore.go | Search for files in a directory hierarchy. Dangerous predicates (`-exec`, `-delete`, etc.) blocked. Internal to `explore` spawn mode. |
| `grep` | explore.go | Search file contents using the best available binary (rg > ack > ag > grep). Flags are validated and translated to the active binary's dialect. Internal to `explore` spawn mode. |
| `send_to_chat` | telegram.go | Send proactive Telegram messages (text, documents, voice notes). With `send_as="voice"` and text (no file_path), synthesizes speech via TTS. Routes to the chat extracted from the session key (`X/cCHATID/{versionTS}`) so per-chat sessions get messages to the correct user. Falls back to bot's default chat when no chat ID in session key. |
| `send_to_session` | session_send.go | Inject a user-role message into another session. Tags the message with `[Message from session ...]` origin header. Appends to session store and triggers processing via `AsyncNotifier`. Used for cross-session communication (e.g. facet branches talking to main). |
| `todo` | todo.go | Per-agent task list (add, list, complete, remove). SQLite backend with priority ordering (high/medium/low). Scoped by `agent_id`. |
| `bitwarden_search` | bitwarden.go | Search Bitwarden vault items by name, URI, folder, username. Returns metadata only (never passwords). Max 5 results. Only registered when `[bitwarden] enabled = true`. |
| `bitwarden_unlock` | bitwarden.go | Unlock a vault item by ID. Calls `sudo -u bitwarden bw get password` via aisudo — blocks until Telegram approval or denial. Caches value for `secret_ttl`. Never returns the actual password. |
| `browser` | browser.go, browser_actions.go, browser_snapshot.go | Browser automation via accessibility tree snapshots. Uses go-rod to control Chrome, captures ARIA snapshot as YAML with numeric refs (`[ref=s1e5]`). Actions: navigate, click, fill, select, press, screenshot, pdf, evaluate, etc. Each mutation auto-captures a fresh snapshot. JS engine vendored from go-rod/rod-mcp (browserjs/). Registered by default; disable with `[tools.browser] enabled = false`. |

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

**Tools with `ExecExport: true`:** `http_request`, `web_fetch`, `web_search`, `memory_search`, `todo`, `send_to_chat`, `spawn`, `tmux`.

**`foci-call` binary** (`cmd/foci-call/`): Reads `FOCI_SOCK`, connects to unix socket, sends JSON request (newline-terminated), prints result to stdout or error to stderr, exits 0/1. 1MB scanner buffer.

### Tmux Memory Monitor (`tools/tmux_memory.go`)

Background goroutine that checks the RSS of the tmux server process at configurable intervals. Three thresholds (warn, critical, kill) fire Telegram notifications and, at the kill threshold, run `tmux kill-server` and call `ClearAll()` on all tmux tool instances. Notifications use dedup — same threshold level won't re-fire until memory drops below it or tmux is killed.

Wired in `main.go` after agent setup. Notification callback sends to agents whose `inject_agent_warnings` is disabled (agents with injection see warnings via their `warnings.Queue` — proactively dispatched as independent agent turns via `warnings.Dispatcher`). Cleanup callback calls `tmuxClearAll` on each agent instance (stored on `agentInstance` struct).

### System Memory Guard (`resources/memory_guard.go`)

Background goroutine monitoring total RSS of all processes owned by the foci user. Reads `/proc/[pid]/status` directly — no external commands. Two thresholds (warn at 25%, kill at 40% of RAM), both gated by memory pressure (PSI `avg10` from `/proc/pressure/memory` > configurable threshold). Warn pushes to all agents' `WarningQueue` (surfaces via proactive warning dispatch). Kill finds the largest non-foci process by RSS (excludes `os.Getpid()`), sends SIGTERM, waits 5s, SIGKILL if still alive.

Wired in `main.go` after tmux memory monitor. Warning callback iterates `agents` map and pushes to any `inst.ag.Warnings` that's non-nil (agents with `inject_agent_warnings` enabled).

### Warning Injection Architecture

Each agent can have two independent warning queues, controlled by `inject_agent_warnings` and `inject_chat_warnings` (both accept `"all"`, `"errors"`, or `"off"`):

- **Agent session queue** (`WarningQueue`): feeds the existing proactive dispatcher which injects warnings as system-initiated turns in the agent's session.
- **Chat notification queue** (`ChatWarningQueue`): feeds a second dispatcher that sends warnings as platform notifications (Telegram messages) directly to the user.

Both queues are independently rate-limited and severity-filtered at push time (`errorsOnly` drops WARN-level entries when the level is `"errors"`). The log hook pushes to all non-nil queues on every agent.

### Tool Result Guard

If a tool result exceeds `agent.MaxResultChars` (from config, default 5,000), the result is written to `agent.ToolResultTempDir` instead of injected directly. Before returning a guard message, the agent makes a side-call to a cheap model to auto-summarise the oversized content, including recent conversation context (configurable via `summary_context_turns` and `summary_context_chars`). The summary model is resolved via `agent.ResolveCallSite(config.CallSummarizeTool, sessionKey)`, which delegates to the `GroupResolver` (see Model Group Resolution below). In multi-model mode this routes to the `cheap` group; in single-model mode it uses the session model. The agent receives the summary plus a reference to the saved file for deeper inspection. If the cheap-model call fails (API error, context cancelled, resolution error), falls back to the original guard message with file path and contextual tool hints (e.g. `jq` for JSON, `mdq` for markdown). This prevents large results from bloating session history while giving the agent useful visibility into the content.

## Slash Commands (`command/`)

Messages starting with `/` are intercepted at the Telegram router level before reaching the agent. They execute immediately — never queued behind an in-flight agent turn.

**Dispatch flow:** Telegram message → auth check → if `/`: `registry.Dispatch()` → execute → reply. Never touches agent session or message history.

**Two types:**
1. **Built-in** (code-defined in `command/builtins.go`): `/ping`, `/status`, `/cache`, `/last`, `/cost`, `/mana`, `/reset`, `/reload`, `/model`, `/session`, `/tools`, `/tmux`, `/config`, `/log`, `/errors`, `/version`, `/uptime`, `/voice`, `/facet`
   - `/mana` — check quota remaining (`/usage` is a hidden alias)
   - `/reload` — reload workspace files, skills, and system blocks from disk
2. **Custom** (script-defined in `foci.toml` via `[[commands]]`): runs a shell script, returns stdout. Timeout default 10s.

**`/model` endpoint switching:** Accepts `endpoint:developer/model_id` syntax (e.g. `/model gemini:google/gemini-2.5-flash`, `/model openrouter:anthropic/claude-opus-4-6`). The Execute function calls `config.ResolveModel()` to parse the `developer/model_id` string and `cc.ClientProvider.ResolveEndpointClient(endpoint, format)` to lazy-init the correct client. Calls `cc.Agent.SetSessionModel(sessionKey, model, endpoint, format, client)` to store the model, endpoint, format, and per-session client override. All three are persisted to state store for restoration across restarts.

**Command registration** (`commands.go` in main package): All per-agent slash commands are registered in `registerAgentCommands()`, which builds a `command.CommandContext` struct from agent references, config, clients, and stores. Commands are zero-argument constructors (e.g. `ModelCommand()`, `ResetCommand()`) returning `*Command` structs with an `Execute(ctx, Request, CommandContext)` function. All command logic accesses dependencies through the `CommandContext` parameter — no closures or per-command constructor injection. Commands interact with platforms via `cc.ConnMgr` (a `platform.ConnectionManager` interface) to avoid importing the `telegram` package.

## Config (`config/config.go`)

Single `foci.toml` parsed with BurntSushi/toml. Defaults applied for missing fields.

**Multi-agent config:** Two formats supported:

1. **Legacy (single agent):** `[agent]` table — backward compatible, auto-promoted to single-element `Agents` slice.
2. **Multi-agent:** `[[agents]]` array — each agent has its own `id`, `workspace`, and platform config.

When both `[agent]` and `[[agents]]` are present, `[[agents]]` wins.

**Platform configuration:** Per-agent platform settings live in `[agents.platforms.telegram]` and `[agents.platforms.discord]`. The old top-level Telegram fields (`telegram_bot`, `allowed_users`, etc.) are migrated to the new structure at load time. Display fields (`show_tool_calls`, `show_thinking`) are synced between agent-level and platform-level by `syncDisplayFields()`.

**Config cascade:** Most config sections support per-agent overrides on global defaults. The cascade is resolved once per agent at startup via `config.Resolve(cfg, acfg)`, which returns a `*ResolvedAgentConfig` with all 2-layer merges (per-agent → global) pre-computed. This is stored on `setupParams`, `agentInstance`, and `CommandContext`. Platform-aware 4-layer cascades (Display, Notify: agent-platform → agent → global-platform → global) remain as separate `Merge` calls at their use sites.

**Bot token resolution:** Telegram: `config.ResolveBotToken(botName, botSecret, secrets)` looks up `"telegram.<botName>"`. Discord: `config.ResolveDiscordToken(botName, botSecret, secrets)` looks up `"discord.<botName>"`. Convention-based — no explicit bot map needed.

**Example multi-agent config:**
```toml
[[agents]]
id = "clutch"
model = "anthropic/claude-sonnet-4-6"
workspace = "/home/rich/workspace1"

[agents.platforms.telegram]
bot = "primary"
facet_bots = ["clutchling"]       # per-agent pool

[[agents]]
id = "scout"
workspace = "/home/rich/workspace2"

[agents.platforms.telegram]
bot = "scout"

[telegram]
allowed_users = ["5970082313"]
facet_bots = ["spare1"]           # shared pool (any agent)
```

**Legacy format (still works):**
```toml
[[agents]]
id = "clutch"
telegram_bot = "primary"
facet_bots = ["clutchling"]
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
                                                           →  MessageQueue.Enqueue() routes to:
                                                              - steer buffer (mention + turn active + steer mode)
                                                              - GroupThrottle (group chat + throttle configured)
                                                              - drop (group + require_mention + no throttle + no mention)
                                                              - main channel (everything else)
[agent worker goroutine]  →  dequeue msg  →  batch with DrainQueue()  →  HandleMessageWithAttachments  →  reply
```

Both platforms use a shared `platform.MessageQueue` that manages inbound message buffering, steer injection, and group chat throttling. The `MessageQueue` wraps a buffered channel with routing logic:

- **Steer mode** (`steer_mode`): During active turns, text-only messages go to the steer buffer for injection between tool calls.
- **Group throttle** (`group_throttle`): Non-mention group messages accumulate in a `GroupThrottle` per chat ID. A fixed-window timer flushes them as a batch. @mentions flush immediately and reset the cooldown.
- **Require mention** (`require_mention`): Without throttle, non-mention group messages are dropped. With throttle, they're buffered.
- **Sender attribution**: Group chat batches prefix each message with `[senderName]` for multi-user context.

The receiver never blocks on the agent. Slash commands (including `/stop`) execute immediately on the receiver goroutine. Agent messages are processed sequentially by the worker.

**Stale command filtering:** Slash commands older than 30s are silently dropped. Safety net for update replay after crashes — prevents stale `/reset` or `/stop` from firing on restart.

**Shutdown ack:** On context cancellation, each bot's poll loop fires one final `GetUpdates` with the last processed offset. This acknowledges processed updates to Telegram, preventing replay on restart. `BotManager.Wait()` blocks main after `cancel()` to ensure all bots complete this ack before process exit.

**Wizard routing (`WizardHandler`):** Interactive wizards (e.g. `/agents new`) take over message routing via `Registry.HandleMessage()`. When a wizard is active, ALL messages (including non-`/` text) are intercepted by the receiver goroutine before reaching slash command dispatch or the agent queue. `/cancel` and `/stop` abort the active wizard. The wizard is cleared automatically when it signals completion (`done=true`).

**Attachment handling:** Photos (`msg.Photo`, largest size selected), image documents (`msg.Document` with image MIME type), and PDF documents (`msg.Document` with `application/pdf` MIME type) are downloaded via `GetFile()` + HTTP GET. The raw bytes are queued as `attachment` structs alongside the message text (which may come from `msg.Caption` for photos). PDFs over 32MB fall back to save-to-disk with a text annotation. The agent worker converts these to `agent.Attachment` and calls `HandleMessageWithAttachments`, which routes images to `ImageBlock()` and PDFs to `DocumentBlock()` content blocks.

**Turn cancellation:** Each agent turn gets its own `context.WithCancel`. `/stop` calls `turnCancel()`, which propagates to in-flight API calls (HTTP client context) and tool executions (process group kill). The agent loop checks `ctx.Err()` after API responses and between tool calls.

**Reset guard:** `/reset` refuses when `agent.IsProcessing()` is true — prevents clearing an active conversation mid-turn.

## Streaming Output (`telegram/stream_writer.go`)

When `stream_output = true` and `streaming = true`, model output is shown in Telegram in real-time as tokens arrive, rather than waiting for the full response.

**Lifecycle:**
1. `processAgentMessage` creates a `streamWriter` with the bot's `tableOpts` (no goroutines started yet)
2. On the first `TextDeltaObserver` delta, the stream writer sends an initial HTML-formatted message and starts a ticker goroutine
3. Each tick, if new text has accumulated, the buffer is processed through `closePartialMarkdown` → `ConvertToTelegramHTML` and the message is edited with HTML formatting
4. When `HandleMessage` returns, `Finish()` stops the ticker and returns the message ID
5. The final HTML-formatted response is edited into the stream message (or sent as a new message if too long/has thinking)

**Key design decisions:**
- **HTML formatting during streaming:** Each stream update runs through `closePartialMarkdown` (strips unmatched `**`, `` ` ``, `` ``` ``, `~~`, `__`, `*`, `_`) then `ConvertToTelegramHTML` with `ParseMode: "HTML"`. If the HTML edit fails (malformed output), the stream writer falls back to plain text for that tick.
- **Partial markdown handling:** `closePartialMarkdown` detects unmatched delimiters by parity counting and strips the trailing unmatched instance. For code fences, everything from the unmatched fence onward is removed. This is lightweight (string counting, no regex) and runs on every tick.
- **Truncation at 3900 chars:** Buffer is truncated with `"..."` to stay within Telegram's 4096-char limit (with headroom for HTML tag expansion). Truncation is rune-safe to avoid splitting multi-byte UTF-8 characters. The final response uses the normal chunking path if it exceeds 4096.
- **Lazy start:** No goroutine or message until the first delta. If the agent returns no text (e.g. pure tool calls), the stream writer does nothing.
- **Stream message as edit target:** When a stream message exists, the final response is edited into it (taking priority over tool call preview messages). If the response can't be edited in-place (too long, has thinking blocks), the stream message is edited to a truncated preview with "(full response below)" and the full response is sent as a new message.

**Config:** `stream_output` (bool) and `stream_update_interval` (string, default `"250ms"`) in `[display]` or `[[platforms]]`, or `stream_output` and `stream_interval` in `[[agents.platforms]]`.

## Discord Bot (`discord/`)

Same two-goroutine architecture as Telegram (receiver + agent worker), connected via a single WebSocket gateway instead of HTTP long-polling. Uses the same shared `platform.MessageQueue` for message routing, steer injection, and group chat throttling.

**Key differences from Telegram:**
- **Gateway:** Single `discordgo.Session` WebSocket connection shared across all agents, vs one HTTP poller per Telegram bot.
- **Message limit:** 2000 chars (vs 4096). `splitMessage` handles Markdown-aware splitting with code fence close/reopen.
- **Formatting:** Discord speaks Markdown natively — no HTML conversion needed. Pass-through from agent output.
- **Streaming:** Default edit interval 1200ms (vs 250ms) due to stricter rate limits. Max 1900 chars per edit.
- **Attachments:** Direct CDN URL download (vs Telegram file API with file ID → download URL).
- **Interactive UI:** Discord message components (buttons) vs Telegram inline keyboards. Same callback data format (`tc:show`, `tc:hide`, `th:show`, `th:hide`, `cmd:/name`). Both platforms implement `platform.ButtonSender` — the single button abstraction. Discord uses `"im:"` callback data prefix for interactive messages (permission prompts from delegated agents).
- **Facets:** Thread-based (vs separate bot tokens). `auto_thread = true` creates private threads for facet sessions.
- **Routing:** `onMessageCreate` routes to correct agent's `Bot` based on channel/DM/user. `onInteractionCreate` handles button callbacks and slash commands.

**Bot token resolution:** `config.ResolveDiscordToken(botName, botSecret, secrets)` looks up `"discord.<botName>"` in the secrets store.

**Session keys:** Same format as Telegram: `agentID/c{channelID}/{versionTS}`. Discord snowflake channel IDs are int64.

**Config:** `[discord]` for global settings, `[agents.platforms.discord]` for per-agent overrides. See [CONFIG.md](CONFIG.md).

## Voice (`voice/`, `telegram/bot.go`)

**Inbound (Whisper transcription):**
```
Telegram voice note → downloadFile(voice.FileID) → voice.Transcriber.Transcribe()
  → Groq Whisper API (multipart/form-data, whisper-large-v3)
  → "[voice] transcript text" queued as regular message
```

API key resolved via `secret` field in `[[stt]]` config or auto-detected from endpoint hostname.

**Outbound (TTS):**
TTS via send_to_chat — the agent can call `send_to_chat(text="...", send_as="voice")` to synthesize speech and send a voice note.

```
voice.TTS.Synthesize(text) → Edge TTS CLI or OpenRouter TTS API
  → raw MP3 bytes → tgbotapi.NewVoice(chatID, FileBytes{mp3})
```

Two TTS providers:
- **Edge TTS** (default, free): Uses `edge-tts` CLI. Configurable voice and rate (`--rate "+20%"`).
- **OpenAI** (via OpenRouter or Groq): API key resolved via `secret` field in `[[tts]]` config or auto-detected from endpoint hostname.

Speech rate configurable via `rate` in `[[tts]]` entries and per-agent `tts_rate` multiplier. Effective rate = entry.rate × agent.tts_rate (0 treated as 1.0). Translated automatically for each provider (edge-tts `--rate "+30%"`, openai `speed: 1.3`).

The agent sees this and adjusts its style (shorter, conversational, no markdown).

### Voice WebSocket (`voice/ws.go`)

Real-time two-way voice conversation via WebSocket at `/voice`. Used by the FOCI Android app.

**Dependencies:** `voice → log, gorilla/websocket`

**Connection flow:**
```
GET /voice?api_key=KEY → auth middleware → upgrade to WebSocket
  → send connected{agents} → client sends select_agent{agent_id}
  → create ephemeral session (ID/iCONN_ID/CONN_ID) → send session_ready
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

**Wiring in `main.go`:** Callback-based (`HandlerConfig`) — `ListAgents` reads `agents` map + `agentOrder`, `HandleMessage` calls `inst.ag.HandleMessage` with `voice` trigger, `AgentTTS` resolves per-agent TTS via `resolveTTS(ttsMap, cfg.TTS, agentTTSID, agentRate, replacements)` which also wraps with word replacements (entry → `[voice]` → per-agent `[voice]`, merged). Gate: `cfg.HTTP.WSEnabled && len(sttMap) > 0`.

## Facet (`telegram/pool.go`, `telegram/manager.go`, `telegram/bot.go`)

Fork the current session to a secondary Telegram bot for parallel conversations. Each fork shares the parent's cache prefix. See [FACET.md](FACET.md) for user-facing docs (bot pool config, session lifecycle, use cases).

**Config** (`foci.toml`):
```toml
[[agents]]
id = "clutch"
facet_bots = ["clutchling"]      # per-agent pool

[telegram]
facet_bots = ["spare1"]          # shared pool (fallback)
```

**Flow:**
```
/facet → botMgr.AcquireFacet(agentID)
               → try per-agent pool first (pool.Acquire())
               → if busy/empty, try shared pool (shared.Acquire())
           → bot.SetHandlerAndCommands(handler, cmds)  // re-wire shared bots
           → sessions.CreateBranchWithOptions(parent, opts) → parent/b{TIMESTAMP}
           → bot.SetSessionKey(branchKey)
           → bot.SendNotification("🎱 Forked from main.")
```

Messages to the secondary bot route to the forked session. `/done` on the secondary bot detaches it and returns it to the pool.

**Bot pool** (`telegram/pool.go`): Tracks secondary bots, acquires LRU idle bot, releases on `/done`.

**Shared pool** (`telegram/manager.go`): `BotManager.shared` is a fallback pool available to any agent. Shared bots are re-wired to the acquiring agent via `SetHandlerAndCommands` at fork time.

**Bot changes** (`telegram/bot.go`):
- Per-chat session routing: primary bots derive session key from `msg.Chat.Id` → `ID/cCHATID/{versionTS}`
- `SessionKey()` — returns override key (secondary bots) or default chat session (primary bots)
- `SetSessionKey()` — thread-safe override (facet fork/done)
- `Bot.SessionKeyForChat(chatID)` — stable cached session key for a chat. On first call for a chat, checks session index for persisted key before generating new one. New keys are persisted to `chat_metadata` table in session index under key `session_key`. This ensures the same session is resumed after restart instead of creating a new timestamped session.
- `NewSessionKeyForChat(agentID, chatID)` — creates a NEW session key with current timestamp (uncached, unpersisted)
- Default chat: first message sets the default; persisted in state store as `agent/ID/default_chat`
- Username recording: persisted per chat for `/sessions list` display
- `isSecondary` flag — enables `/done` handling, idle message rejection
- `/done` handled as special case alongside `/stop` (bypasses command registry)
- Idle secondary bots respond with "This bot is idle. Use /facet..." to non-command messages

**Session persistence across restarts:** The `bot → session_key` mapping is persisted in the state store (JSON key-value file) under `facet:<bot_username>` (the bot's Telegram username). Each `SetSessionKey` call fires an `OnSessionKeyChange` callback (wired in `agent_setup.go`) that writes or deletes the mapping. On startup, `restoreFacetSessions()` iterates all pool bots via `Pool.ForEach`, looks up saved keys, validates the session file still exists via `LastActivity`, and restores via `SetSessionKeyDirect` (bypasses callback). The bot is also re-wired to the correct agent via `SetHandlerAndCommands` and gets the primary bot's chat ID for notifications.

**Per-session override persistence:** Slash command overrides (`/effort`, `/thinking`, `/model`) are stored per-session in the state store under keys `effort/<sessionKey>`, `thinking/<sessionKey>`, `model/<sessionKey>`, `model_endpoint/<sessionKey>`, `model_format/<sessionKey>`. On startup, `RestoreSessionOverrides(sessionKey)` restores all five — for model overrides, it reads the endpoint and format and calls `GetClient(endpoint, format)` to restore the correct client. The `/voice` mode follows the same pattern under `voice/<sessionKey>`. Overrides reset naturally when a new session starts (no state stored for the new key).

**Special commands on secondary bots:**
- `/done` — detach from forked session, return to pool
- `/stop` — cancel current agent turn (same as primary)
- All other slash commands — shared registry (operate on main session's context)

## HTTP Gateway (`main.go`)

**Two listeners:** The gateway listens on both a TCP port (auth via API key) and a Unix domain socket (auth via kernel peer credentials). Same-user connections over the Unix socket require no API key — the kernel verifies the connecting process's UID via `SO_PEERCRED`. The socket file (`~/data/foci-gw.sock`, configurable via `[http] socket_path`) has mode 0600 as defense in depth.

**TCP auth middleware** wraps all TCP HTTP endpoints including `/voice`. Requires `Authorization: Bearer <key>` header or `api_key` query param, validated against `http.api_key` from `secrets.toml` using constant-time comparison. Returns 401 (missing) or 403 (invalid). The key is auto-generated on first startup using a 5-word passphrase (~52 bits entropy).

**Unix socket peer cred middleware** wraps all socket HTTP endpoints. Extracts peer UID from the connection via `SO_PEERCRED` (injected into request context by `ConnContext`). Returns 403 if the UID doesn't match the gateway's UID. No secret is involved — the authentication is based on OS-level process identity, not a portable credential.

**Security rationale:** The API key in child environments or crontab was a portable credential — if leaked by a prompt-injected agent, it could be used from anywhere. The Unix socket eliminates this: `FOCI_GW_SOCK` (a file path) is injected into child env instead of `FOCI_API_KEY`. The agent can *use* the socket (it runs as the same user) but can't *leak* a credential to an external attacker.

Endpoints for external integration. All endpoints accept an optional `agent` parameter (JSON body or query string) to target a specific agent. When omitted, defaults to the first configured agent.

- `POST /send` — message to agent's default session (activity-gated). Returns 412 if no default session.
- `GET /status` — dispatches `/status` for the specified agent
- `POST /command` — dispatches slash command (bypasses agent context)
- `POST /wake` — branch from default session (activity-gated, supports `no_compact`/`no_reset_hook`). Returns 412 if no default session.
- `POST /webhook/{agent}/{hookid}` — trigger agent turn from external events. `{hookid}` must be declared in the agent's `webhooks` config map (global `[system]` merged with per-agent `[[agents]].system`). The mapped prompt path is resolved via `prompts.ResolvePrompt()` (agent workspace/prompts → shared workspace/prompts). Reads request body as payload (max 1 MB), combines prompt + payload under a `## Webhook Payload` heading, and sends to the agent's default session. Async (202) by default; `?sync=true` for synchronous response. Supports `?if_active`/`?if_inactive` activity gates. Returns 404 if hookid not in config or prompt file not found, 412 if no default session.
- `GET /voice` — WebSocket upgrade for real-time voice conversation. Enabled when `[http] ws_enabled = true`.
- `POST /-/reload-credentials` — hot-reload API credentials from `secrets.toml`. Called by `foci auth` after saving a new token. Only registered when using static token auth (setup-token or API key), not OAuth fallback.

## CLI Tool (`cmd/foci/`)

Separate binary (`go build ./cmd/foci`) that wraps the HTTP gateway endpoints for scripts and cron jobs. Auto-discovers the gateway Unix socket at `~/data/foci-gw.sock` (`FOCI_GW_SOCK` env var or `--socket` flag) for same-user auth with no API key. Falls back to TCP + `FOCI_API_KEY` for remote/cross-user access. See [docs/CLI.md](CLI.md) for the full command reference, flags, environment variables, and cron integration examples.

**`foci first-run`** — first-run setup wizard. Generic steps (auth, agent ID, model, character files) live in `cmd/foci/setup.go`. Platform-specific steps (e.g. bot token, user ID) are delegated to providers via the `platform.SetupWizard` interface. Each provider returns a `WizardResult` containing a TOML config fragment and secrets map. The generic wizard appends these to the generated `foci.toml` and stores secrets via `secrets.Store`. `cmd/foci/setup.go` has zero direct telegram imports — it blank-imports `internal/telegram` for provider registration and discovers wizards via `platform.SetupProviders()`. Non-interactive mode collects provider flags dynamically from `SetupFlags()`. The `consoleUI` struct implements `platform.SetupUI` for interactive prompts.

## Wake

- **HTTP Wake** (`POST /wake`): Creates a branch session from the agent's default chat session, injects the text, runs the agent on the branch. Supports `no_compact` and `no_reset_hook` flags. `--oneshot` CLI flag sets both. Returns 412 if no default session.
- **Scheduled Wakes** (`remind` tool with `wake=true`): Agent-initiated timer that fires message injection into the default session at specified delay or timestamp. One-shot, background goroutine, auto-cleaned after firing. Skips if no default session.

## Session-End Memory Formation

Before a session is cleared (`/reset` or facet TTL reclaim), the agent captures memories asynchronously. Configured via `[memory_formation]` section (replaces `session_reset_prompt`).

Flow (`agent.FireSessionEndMemory` in `internal/agent/session_end_memory.go`):
1. Check `memory_formation.session_end_enabled` (nil = true, explicit false skips)
2. Resolve prompt via `prompts.ResolvePrompt(session_end_prompt, ...)` — embedded default on empty/error
3. If prompt resolves to empty, skip
4. For branch sessions, check `BranchMeta.NoResetHook` — if true, skip (unless skipMetaCheck=true for background branches)
5. Create branch from expiring session (copies conversation history)
6. Return immediately — caller proceeds to clear the main session
7. Async: `HandleMessage(ctx, branchKey, prompt)` with 120s timeout, trigger `"session_end_memory"`, NoCompact

Entry points:
- `/reset` command → `agent.FireSessionEndMemory` (async) → `RotateKey` → `Reload`
- `Pool.Acquire` (TTL reclaim) → `ReclaimHook` → `agent.FireSessionEndMemory` (async) → clear session key
- Periodic runner (background branch completion) → `agent.FireSessionEndMemory` (async, skipMetaCheck=true)

## Memory Formation & Consolidation Timers

Memory formation and consolidation run in the keepalive timer loop (30s ticks):

**Interval memory formation** (`maybeMemoryFormation`):
1. Check `interval_enabled` (nil = true)
2. Check wall-clock interval elapsed and user not idle
3. Query `session_index` for active chat sessions with `last_activity_at > last_memory_formation` (per-session tracking)
4. Resolve prompt via `prompts.ResolvePrompt`
5. Iterate all matching sessions: `branchFn("memory-formation", sessionKey, promptText, true)` for each
6. On success per session: stamp `last_memory_formation` at branch creation time

Formation runs before consolidation so the latest memory content is available. Consolidation is blocked while formation is running.

**Consolidation** (`maybeConsolidation`):
1. Check `consolidation_enabled` (nil = true)
2. Check consolidation interval elapsed (persisted in state store)
3. Check recent user activity (within 1h)
4. Check memory formation is not running
5. Resolve prompt via `prompts.ResolvePrompt`
6. Fire branch on default session: `branchFn("consolidation", parentKey, promptText, true)`
7. On completion: persist timestamp to state store

**Proactive warning dispatch** (`warnings.Dispatcher.MaybeFire`):
1. Check `queue != nil` and `dispatchFn != nil` — skip if no injection configured
2. Check `queue.Pending()` — skip if no warnings
3. Check `dispatching` guard — skip if dispatch in flight
4. Determine rate limit interval: call `lastUserMessageTimeFn()`, if within `activityThreshold` → use active interval, else → inactive interval
5. Check `sinceLastDispatch < interval` — skip if too soon
6. Drain warnings, format as `- ...\n- ...`, wrap via `formatFn` (wired to `prompts.FormatInjectedMessage`)
7. Dispatch in goroutine: `dispatchFn(text)`, clear `dispatching` on return

The `warnings.Dispatcher` is created in `main.go` and injected into `periodic.RunnerConfig`. The keepalive timer loop calls `dispatcher.MaybeFire()` each tick. Warnings are only delivered via this proactive dispatch path — they always fire as independent agent turns rather than being bundled into user messages.

## Compaction (`compaction/compact.go`)

Checks token usage against threshold (default 80% of context window). When triggered:
1. Asks model (configurable) to summarize history using configurable prompt
2. Rotates the pre-compaction session file to a timestamp-based archive (e.g. `5970082313.2026-03-04T02-30-00Z.jsonl`) — old messages are preserved for usage tracking and audit
3. Writes the compacted session (context note + summary + continuation note) to the original file path
4. Appends any scratchpad entries to preservation message (scoped to agent via `Compactor.AgentID`)
5. If `CompactionNotifyFunc` is set, sends Telegram notification with session key and pre-compaction message count (configurable via `compaction_notify`, default true)

**Session file rotation:** `Replace()` in `session/store.go` renames the existing file before writing. Archive files use the pattern `{name}.{timestamp}.jsonl` (timestamp in format `YYYY-MM-DDTHH-MM-SSZ`) or `{name}.{timestamp}.{N}.jsonl` if multiple archives have the same timestamp. The active session is always the unnumbered file. `Load`, `LoadFull`, `Append` etc. are unaffected — `keyToPath()` always resolves to the unnumbered path. `ListChatSessions` and `RepairOrphans` skip archive files.

**Session lifecycle events:** `Store.OnSessionEvent(func(SessionEvent))` fires on create (first `Append` to new file), branch create (`CreateBranchWithOptions`), compaction (`Replace`), and clear (`Clear`). Events carry the session key, type, status, parent key, file path, and timestamp. Used by `SessionIndex` to maintain a queryable SQLite index of all sessions.

**Compaction triggers:** `maybeCompact()` in `agent/compaction.go` has two automatic triggers:
1. **Main threshold:** standard `ShouldCompact()` check against base threshold (default 0.8).
2. **Mana refresh:** when `autocompact_before_mana_refresh` is enabled (default true) and mana resets within `autocompact_before_mana_refresh_threshold` (default 5m) AND context exceeds `compaction_threshold × autocompact_before_mana_refresh_factor` (default 0.5, i.e. 40%), triggers compaction. Optionally overrides preserve count via `autocompact_before_mana_refresh_preserve` and preserve percentage via `autocompact_before_mana_refresh_preserve_pct` (default 0.5). The cost is "free" since mana is about to reset. Only fires for sessions with an active Anthropic usage client — sessions switched to Gemini/OpenAI skip this check.

**Async-pending guard:** Compaction is skipped when the session has pending async tool results (`AsyncNotifier.HasPending()`). Tools call `MarkPending()` before dispatching async work (spawn clone, auto-backgrounded exec/http) and `MarkDone()` when the result is delivered via `Notify()`. This prevents compacting away the context that the pending result relates to — compaction fires naturally on a later turn once all results have been delivered.

**No-compact sessions:** When a session with `no_compact` flag (oneshot, wake branches) exceeds the compaction threshold, the context percentage is logged but no compaction or warning occurs. These sessions are expected to be short-lived.


**Branch compaction:** When `Replace()` is called on a branch session (e.g., during compaction), it preserves the `branch_meta` header with `branch_point=0`. The compacted messages are self-contained (the summary includes parent context), so subsequent `LoadFull()` loads `parent[:0] + compacted_msgs` = just the compacted messages.

**Configurable via `Compactor.WithConfig()`:**
- `model` — summarization model (default: agent model)
- `maxTokens` — max output tokens for summary (default: 4096)
- `minMessages` — min messages before compacting (default: 4)

**Passed to `Compact()` at call time** (not stored on the Compactor):
- `summaryPrompt` — read live from file at compaction time via `ReadPromptFile` callback. If empty, falls back to `prompts.CompactionSummary()` (embedded from `shared/prompts/compaction-summary.md`). Edits to the config file take effect immediately.
- `handoffMessage` — message after compaction completes. If empty, uses `DefaultHandoffMessage` (embedded from `shared/prompts/compaction-handoff.md`).
- `dryRun` — when true, runs the full pipeline (API call, summary generation) but skips `sessions.Replace()`. The session is left unchanged. `/compact dry-run` sends the resulting summary as a Telegram document (via `CompactionDebugFunc` if configured, otherwise directly via `primaryBot.SendDocument`) without rewriting history. Useful for iterating on compaction prompts.

## Nudge System (`nudge/`)

Mid-turn behavioral reminders extracted from character files. The nudge package is a leaf dependency (only imports `log`).

### Rule Extraction

Rules are extracted once from character files via an LLM call, then cached in `{workspace}/character/nudge-rules.json` with a content hash. Re-extraction only happens when the hash changes (character files edited).

**Extraction flow:**
1. On first session activity (`OnActivity` hook), `NudgeReloadFunc` fires via `sync.Once`
2. `Extractor.NeedsExtraction()` compares current character file hash against stored hash
3. If changed: spawns a background goroutine that creates a branch session and sends `ExtractionPrompt` to the agent's own model
4. LLM response is parsed as JSON array of rules, each with text, trigger type, source attribution, and priority
5. Rules are saved to disk; scheduler is refreshed with new rules
6. Also re-runs on `/reload` and after compaction (character files may have changed)

### Trigger Types

- **`every_n_tools(N)`** — fires every N individual tool calls during a turn (via `CheckAfterTools`)
- **`every_n_turns(N)`** — fires every N user turns; lifetime counter, never reset (via `CheckTurnInterval`, used by default nudges)
- **`after_error`** — fires when the last tool call returned an error (via `CheckAfterTools`)
- **`regex(pattern)`** — regex evaluated once against user message at `StartTurn()`; fires via `CheckAfterTools` on the tools path, or via `CheckRegex()` on the no-tools path (ensures regex triggers fire even when the model answers directly)
- **`pre_answer`** — all pre_answer rules concatenated and injected when the model wants to end the turn (gated by `NudgePreAnswerGate` and `NudgePreAnswerMinTools`)

### Injection

Nudge reminders are injected as text ContentBlocks in user messages. After-tools nudges (every_n_tools, after_error, regex) are appended as individual blocks to tool result messages. Regex nudges on no-tools turns and every_n_turns nudges are prepended as ContentBlocks to the user message before the first API call. Pre_answer nudges are injected as standalone user messages that continue the loop. Each injection is one-shot per trigger type per turn to prevent infinite loops.

### Configuration

Cooldown (min tool calls between repeating the same rule, default 5) and max-per-batch (max reminders per tool batch, default 1) prevent spam. All config is per-agent via `nudge_enable`, `nudge_cooldown`, `nudge_max_per_batch`, `nudge_pre_answer_gate`, `nudge_pre_answer_min_tools`, `nudge_default_enable`, `nudge_default_frequency`.

## Deployment

### setup.sh

`/home/rich/git/foci/setup.sh -u foci` — builds Go binaries, installs to `/usr/local/bin`, restarts service. Allowlisted in aisudo (no approval needed). Uses `--no-block` restart to avoid deadlock when run from foci's own exec.

## Testing

```
go test ./...           # all tests (~66, runs in ~1s)
go test ./... -v        # verbose
go test ./session/...   # single package
```

The cache_test.go in `anthropic/` requires `ANTHROPIC_API_KEY` env var and hits the real API. All other tests are self-contained.
