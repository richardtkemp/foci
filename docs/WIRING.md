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
       → configureDelegated(params, delegator)              ← agents_delegated.go
         → delegator.New(name, config)                      ← create delegator via registry
         → workspace.NewBootstrap → system prompt            ← concatenate workspace *.md files
         → delegator.Start(ctx, opts)                       ← spawn coding agent in tmux pane
         → shared.finalize(ag, params)                      ← commands, platform, nudge (shared postamble)
     → ELSE (traditional API agent):
       → tools.NewAsyncNotifier()                           ← shared by exec + http_request + tmux, routes by session key
       → tools.NewRegistry() + registerTools(pathAPI)        ← unified tool table (tool_table.go), one source of truth shared with exec path
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

  → modelcaps wiring (#840)                               ← main.go, once after the agent loop
    → anthropicResolver.ModelCapsFetcher(15s)             ← /v1/models fetcher (nil if CC OAuth creds absent)
    → for backend in {ccstream, api, opencode}:
      → modelcaps.SetFetcher(backend, fetcher)
      → modelcaps.SetPersister(backend, modelCapsPersister{sessionIndex})  ← state.db model_caps table (modelcaps_persist.go)
      → modelcaps.Restore(backend)                        ← seed cache from DB synchronously (bridges the ~1s fetch gap)
      → go modelcaps.Refresh(ctx, backend)                ← background; on error, serve-stale / static modelinfo fallback
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
  → checkDelegatedReadiness(...)                           ← notifications.go (probe each delegated backend; fire relogin if not ready — gate active before first-run injection)
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
 ├── log           → sqlite, modelinfo (the latter two only for API-call usage logging; conversation storage was extracted to convo)
 ├── convo         → log, sqlite, timeutil (per-agent conversation SQLite store + memory-index Hook; extracted from log so log stays lean)
 ├── display       (no deps — table rendering with Unicode display-width handling)
 ├── secrets       → BurntSushi/toml
 │   └── secrets/bitwarden → log
 ├── provider      (no deps — provider-neutral types and Client interface)
 ├── platform      → config, log, secrets, session, state, voice, warnings
 │                  (messaging types, interfaces, provider registry, Messaging facade,
 │                   MessageQueue thin filter+throttle helper + GroupThrottle for group chat batching)
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
 ├── tools         → anthropic, config, display, log, memory, modelinfo, platform, provider, secrets, secrets/bitwarden, session, state, tempdir, tools/spill, voice (Registry, Tool, shared helpers, the exec-bridge generator, web, http, and most tool impls)
 │     ├── tools/spill    → (stdlib only) shared spill-to-disk writer: bounded in-RAM head + overflow to temp file, optional total cap; used by tools/shell and the http tool
 │     ├── tools/shell    → tools, tools/spill, log, procx, secrets, secrets/bitwarden (the exec/shell tool; execbridge generator stays at root)
 │     ├── tools/tmux     → tools, log, display, session, procx (tmux session tool — 8 files)
 │     ├── tools/browser  → tools, log, tools/browserjs (browser automation — imports root for Tool/ToolResult)
 │     └── tools/browserjs (no foci deps — vendored go-rod JS snippets)
 ├── workspace     → log, provider
 ├── nudge         → log (leaf — rule extraction, scheduling, file I/O)
 ├── prompts       (top-level package, not internal) → log (embedded .md files + ResolveOrientationTemplate helpers)
 ├── modelinfo     (no deps — stdlib-only leaf package for model attributes: context window, capabilities, pricing)
 ├── modelcaps     → modelinfo, log (leaf — per-backend live capability cache; Fetcher + Persister seams injected at startup so it imports no anthropic/session/DB)
 ├── compaction    → log, memory, modelinfo, provider, session, tools
 ├── tempdir       (no deps — stdlib-only leaf package for canonical temp dir)
 ├── provision     (no deps — stdlib-only leaf package for agent creation)
 ├── command       → agent, compaction, config, display, log, mana, memory, platform, provider, provision, session, skills, state, tempdir, tools, workspace
 ├── mana          → anthropic, log, provider (mana budget logic)
 ├── warnings      → log (leaf — warning queue and proactive dispatch)
 ├── messages      → provider (shared message-inspection utilities: HasToolUse, ToolUseIDs)
 ├── timeutil      (no deps — centralised timestamp formatting with configurable timezone)
 ├── delegator     (no deps — Delegator interface, registry, StartOptions, SessionEvents/TurnEvents)
  │   ├── delegator/cctmux     → delegator, fsnotify (tmux-based Claude Code; registers "claude-code-tmux" via init())
  │   ├── delegator/ccstream   → delegator, log (stream-json Claude Code; registers "claude-code" via init())
  │   └── delegator/opencode   → delegator, log (HTTP/SSE OpenCode; registers "opencode" via init())
 ├── agent         → delegator, compaction, config, display, log, mana, memory, nudge, platform, provider, session, state, tools, warnings, workspace
 ├── periodic     → config, log, memory, provider, state, warnings (NO agent, NO session)
 ├── dispatch      → command, session (shared command dispatch logic; platform wrappers delegate here)
 ├── turn          → display, log, toolformat, tooldetail (shared turn rendering, tool call tracking, and tool-result display store for all platforms)
 ├── telegram      → agent, chatmeta, command, config, dispatch, display, log, platform, secrets, session, sqlite, state, tooldetail, toolformat, turn, voice
 │                  (registers via init() → platform.RegisterMessagingProvider; blank-imported in main.go)
 └── discord       → agent, chatmeta, command, config, dispatch, display, log, platform, secrets, session, sqlite, state, tooldetail, toolformat, turn, voice
                    (registers via init() → platform.RegisterMessagingProvider; blank-imported in main.go)
```

No circular dependencies. `provider`, `display`, `log`, `secrets`, `memory`, `skills`, `prompts`, `startup`, `resources`, `provision`, `tempdir`, `mana`, `warnings`, `modelinfo`, `modelcaps`, `messages`, `timeutil`, `turn`, `dispatch` are leaf packages. `platform` depends on leaf packages only (config, log, secrets, session, state, voice, warnings).

**`provider` package:** Defines the neutral types (`Message`, `ContentBlock`, `ToolDef`, etc.) and the `Client` interface (`SendMessage`, `CountTokens`). `anthropic`, `gemini`, and `openai` all implement `provider.Client`, translating between neutral types and their wire formats.

**`platform` package:** Defines platform-agnostic messaging types (`Message`, `Attachment`), the `Connection`/`ConnectionManager` interfaces, the `MessagingProvider` interface for platform implementations, and the `Messaging` facade that manages all active providers. Providers register via `RegisterMessagingProvider()` (called from `init()`) and are activated at startup via `InitMessaging()`. An aggregating `ConnectionManager` merges connections from all providers — `AllForAgent()` returns connections across all platforms, enabling multi-platform fan-out for notifications. `cmd/foci-gw/` uses only the facade; zero platform-specific type references. Also defines the `SetupWizard` interface (optionally implemented by `MessagingProvider`) for contributing interactive setup steps to `foci first-run`. `SetupProviders()` returns all registered providers that implement `SetupWizard`. Types: `SetupFlag` (CLI flag definition), `WizardResult` (config TOML fragment + secrets), `SetupUI` (console interaction primitives).

**`chatmeta` package:** Shared session key management logic extracted from `telegram` and `discord`. Provides `Resolver` — a lightweight struct that looks up, creates, persists, and rotates per-chat session keys via `platform.SessionIndex`. Each platform `Bot` holds a `*chatmeta.Resolver` and delegates `SessionKeyForChat`, `UpdateSessionKey`, `DefaultChatID`, `DefaultSessionKey`, and `RecordUsername` to it. Platform-specific methods (`SessionKey`, `SetSessionKey`, `ChatID`, `SetChatID`, `Username`) remain on each Bot. Imports: `platform`, `session`, `log`. All methods are nil-receiver safe.

Most packages depend on `provider` for types; only `main.go`, `tools`, and `mana` import `anthropic` directly (for Anthropic-specific features like `UsageClient`). `periodic` no longer imports `agent` or `session` — mana monitoring and warning dispatch are handled by the `mana` and `warnings` packages respectively, wired together in `main.go`.

**`provision` package:** Shared agent creation logic used by both `cmd/foci/setup.go` (first-run wizard) and `command/agents_new.go` (`/agents new` runtime command). Stdlib-only, no imports from other foci packages. Provides `AgentSpec` + `Provision()` (workspace creation, character file copying, SOUL.md templating), validation (`IsValidAgentID`), config block generation (`GenerateAgentBlock`), and crontab templating (`GenerateCrontab`, `AppendCrontab`). Platform-specific validators (e.g. `IsValidBotToken`, `IsValidUserID`) live in their respective platform packages (e.g. `internal/telegram/validate.go`).

## Command Dispatch Architecture

Slash commands (`/ping`, `/model`, etc.) are dispatched through a three-layer architecture:

1. **Platform wrapper** (`telegram/dispatch.go`, `discord/dispatch.go`): Thin wrappers that extract `text`, `chatID`, and `userID` from platform-native message types (`gotgbot.Message`, `discordgo.Message`) and delegate to the shared dispatcher.

2. **Shared dispatch** (`dispatch/dispatcher.go`): Platform-agnostic routing logic. Detects dot-commands (`.model`) vs slash-commands (`/model`), resolves session keys, and builds a `command.Request`. Returns a `dispatch.Result` with `Handled`, `Response`, `SessionKey`, `UserID`.

3. **Command layer** (`command/registry.go`): Receives `Request` and `CommandContext` (platform-agnostic dependencies), executes the command, and returns a `Response` with `Text` and optional `DocPath`. When `DocPath` is set, it points to a temp file that the platform layer sends to the originating chat via `SendDocumentToChat(msg's chat ID, path)` and then removes. This keeps the send scoped to the exact chat that invoked the command, avoiding reliance on global "last channel" state. The HTTP `/command` endpoint handles `DocPath` by sending via `ForSessionOrPrimary(sessionKey, agentID)`.

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

The core of the system. Single entry point:
- `HandleMessage(ctx, sessionKey, texts, attachments) error` — accepts one or more user text blocks and optional image/document attachments. Both parameters may be nil/empty for the appropriate caller.

**Output delivery:** Text, thinking, tool calls, tool results, typing indicator, and turn lifecycle are all emitted as `turnevent.Event` values through a `turnevent.Sink` attached to ctx (see the "Turn Event Stream (Sink Architecture)" section). `HandleMessage` emits `TurnStart` at entry and `TurnComplete` via `defer` so consumers always see the terminal event even on error paths. There is no string return value — callers that need the final text wire a `turnevent.BufferSink` and read `buf.FinalText()` after the call.

**Delegated agents:** When `Agent.DelegatedManager != nil`, `HandleMessage` branches to `DelegatedTransport` (`turn_delegated.go`) instead of the traditional API tool loop. See the TurnContract section below for how the transport choice is made and how turns are orchestrated.

### TurnContract Abstraction (`agent/turn_contract.go`)

Both transport paths (API and delegated) are unified under the `TurnContract` interface — 20 methods grouped into four phases. Adding a method to the interface produces a compile error in both transports until implemented.

**Transports:**
- `APITransport` (`turn_api.go`) — traditional API code path: direct provider calls with client-side tool execution loop.
- `DelegatedTransport` (`turn_delegated.go`) — delegated path: the backend (Claude Code) owns inference and tool execution.
- Both embed `sharedTurnOps` (`turn_contract.go`) for shared implementations (7 methods).

**Transport selection:** In `HandleMessage`, if `Agent.DelegatedManager != nil` → `DelegatedTransport`; otherwise → `APITransport`.

**Orchestrator:** `OrchestrateFullTurn` (`turn_orchestrator.go`) calls all 20 methods in canonical order:

```
Phase 1 — Pre-lock gates and registration:
  RateLimitGate         API: per-endpoint rate limit gate     Delegated: no-op (CC has its own)
  AcquireTurnLock       API: per-session serialization lock   Delegated: no-op (CC serializes)
  IncrementProcessing   API: atomic processing counter        Delegated: atomic processing counter
  RegisterTurn          API: TurnDetail for diagnostics       Delegated: no-op
  CheckStaleContext     Shared: return ctx.Err() if cancelled

Phase 1b — Post-lock logging and tracking:
  RegisterSessionIndex  Shared: upsert session into index
  LogConversationRecv   Shared: log inbound message
  TouchActivity         Shared: fire OnActivity callbacks

Phase 2 — Turn preparation:
  LoadSessionMeta       Shared: load per-session metadata
  LoadAndRepairSession  API: load + 3 repair passes           Delegated: no-op (CC owns session)
  ResolveModelEffort    API: full resolution with defaults     Delegated: reads agent-level model
  ComposePrompt         API: rich content blocks               Delegated: flat text via JoinPrompt
  BuildSystemAndTools   API: per-turn system + tool rebuild    Delegated: no-op (set at Start)
  InjectNudges          API: content blocks in user message    Delegated: text prepended + PostToolNudgeFunc + PreAnswerNudgeFunc (see Nudge System)

Phase 3 — Core execution:
  RunInference          API: multi-iteration tool loop         Delegated: Inject(SourceUser) (async)

Phase 4 — Post-turn:
  SaveSession           API: AppendAll to session store        Delegated: no-op (CC owns session)
  UpdateSessionMeta     API: from provider.Usage               Delegated: from backend TurnResult
  LogUsage              API: no-op (logged per-call)           Delegated: called from OnTurnComplete
  RunCompaction         API: direct maybeCompact               Delegated: sends /compact to CC
  LogConversationSent   Shared: log outbound response
  TouchActivityPost     Shared: fire OnActivity callbacks
```

**Post-turn sync/async split** (`runPostTurn`): API turns close `CompletionChan` before `RunInference` returns (synchronous), so post-turn runs inline. Delegated turns block inline waiting for `CompletionChan` with an activity-based timeout — if no stream events arrive for 2 minutes (`streamIdleTimeout`), the wait times out. Activity is tracked by the backend's `LastActivity()` method, seeded at turn start and updated on every stream event. Steered follow-ups (delegated, `IsTurnInFlight() == true`) close `CompletionChan` immediately with no post-turn work.

**Shared prompt composition** (`turn_common.go`): `composeTurnText` assembles metadata prefix, reminders, state dashboard, mana-restore text, attachment paths, and user texts into a `turnTextParts` struct. The API transport converts these to content blocks; the delegated transport joins them into a flat string via `JoinPrompt()`.

### RunOnce Mode (`DelegatedManager.RunOnce`)

Non-interactive backend execution for headless tasks. `RunOnce(ctx, prompt, systemPrompt)` spawns `claude --print --dangerously-skip-permissions --no-session-persistence --model sonnet`, captures stdout synchronously, and returns the response text. No tmux pane, no watcher, no session index — a one-shot subprocess call.

Used by:
- **Nudge extraction** — `ExtractViaRunOnce` sends conversation context to the model and parses structured nudge rules from the response.
- **Consolidation** — The periodic `Runner` is wired with a `RunOnceFunc` for memory consolidation tasks that don't need an interactive session.

### Session Lifecycle Operations (`agent/lifecycle.go`)

The agent exposes three lifecycle methods that encapsulate multi-step sequences previously scattered across command handlers:

- **`ResetSession(ctx, sessionKey)`** — clears session history with memory formation. For API agents: fires memory formation as an async branch, rotates the session key, reloads bootstrap. For delegated agents: rotates the session key and reloads immediately (the chat maps to a fresh session at once; a new CC backend spawns lazily on the next message), then runs memory formation on the old CC session and destroys it in the background (up to 120s). The old CC resume ID is cleared before rotation so the fresh key does not resume the previous conversation. Returns the new session key.
- **`CompactSession(ctx, sessionKey, dryRun)`** — triggers manual compaction. Validates message count (min 5), runs the compaction pipeline, then reloads bootstrap and resets cache baseline. When `dryRun` is true, the full pipeline runs (API call, summary generation) but the session is left unchanged — the summary is returned for inspection.

All three call `reloadAfterMutation()` internally, which reloads bootstrap, refreshes nudges, and invalidates all per-session system prompt caches.

**Delegated system prompt rebuilt from disk at session start (#828 Part A, fixes #706):** the delegated CC system prompt was previously built once at agent setup and frozen into `StartOpts.SystemPrompt` for the process lifetime, so `/reset` and idle respawn never picked up character-file or skill edits. `StartOptions.SystemPromptFunc` fixes this: when set, `DelegatedManager.getOrCreate` calls it at every session start and its non-empty result wins over the static prompt. The closure (wired in `agents_delegated.go`) reloads `Bootstrap` from disk itself and re-runs the skill load, so every respawn — reset, idle, compaction-bounce — gets a fresh prompt regardless of caller. Empty result falls back to the setup snapshot.

### Steer Mode Differences (API vs Delegated)

When `steer_mode` is enabled and a turn is active, user messages are buffered as "steers" and injected mid-turn rather than waiting for completion:

- **API transport:** Steer messages are collected via `steerBlocks(ctx)` and injected as text content blocks in the tool result message between tool execution loops. `steerBlocks` pulls from the `turnevent.Steerer` supplied by `agent.Inbox` (one per session) — the inbox accumulates mid-turn text in its per-session steer buffer when the configured backend is API-mode (no `delegator.Delegator` registered).
- **Delegated transport:** Steer messages are dispatched immediately by `agent.Inbox`. On `Enqueue` of a text-only mid-turn message, the inbox calls `Backend.Inject(ctx, Inject{Source: SourceSteer, Text: env.Text})` directly, looking up the session's backend via the agent's `DelegatedManager`. `Inject(SourceSteer)` sends the steer text as a `type: user` stream-json event at queue priority `"now"`. CC's mid-turn drain (`claude-code/src/query.ts:1570-1589`) folds the message into the current `ask()` as an attachment to the next tool-result batch, so the model responds in the same turn and the original handler's `OnText`/`OnTurnComplete` pipeline carries the response. Steer no longer aborts the in-flight tool — for "stop right now" semantics use `/reset hard`. Mid-turn steer for delegated agents bypasses the steer buffer entirely; the buffer only matters for API-mode agents that have no equivalent stdin protocol primitives.

**Compaction hold (#856):** `Enqueue` gates the steer decision on `Agent.IsCompacting(sessionKey)` — while a `/compact` turn is in flight, a steer would write to CC's stdin mid-compaction and CC folds the raw text into the compaction transcript unframed (no `[meta]` header). The gate routes such messages to the session channel instead. Auto-compaction runs synchronously inside the driven turn (`driveOnce` → `runPostTurn` → `RunCompaction`), so the worker is already blocked and channel-queued messages wait naturally; the session worker adds a `for a.IsCompacting(...)` poll-hold (`compactionHoldPoll`, 100ms) after the #767 in-flight gate as a backstop for the manual-`/compact` path where the worker is free. Held messages dispatch as a clean fresh turn once compaction clears.

**Plan-cancel-by-message (#858):** a pending **ExitPlanMode** permission blocks the session — CC waits for Allow/Deny and ignores stdin until it answers, so a steered or queued message would either hit ignored stdin or wait indefinitely (the ~20-min "hung typing indicator" symptom). UNLIKE a normal tool permission — which a follow-up message keeps queuing behind via `WaitForPermission` — a typed message during *plan* approval is treated as revision feedback. Before the steer/queue routing, `Enqueue` checks (for an active turn, text-only) whether the session backend implements `delegator.PlanResponder` and has a pending plan permission (`HasPendingPlanPermission` scans `pendingPerms` for `toolName=="ExitPlanMode"`); if so it calls `CancelPlanWithFeedback(reqID, text)`, which sends a `PermissionDeny` carrying the text as the rejection `message` (CC stays in plan mode and revises using the feedback), then fires the prompt's cancel listener via `outstanding.Cancel` so the Allow/Deny buttons edit to "❌ Plan cancelled by follow-up message" and `onEmpty` clears `permPending`. The message is consumed (it became the denial feedback) — not also re-sent as a turn. Scope is ExitPlanMode-only by design; every other permission still queues unchanged.

### Backend Watcher — tmux (`internal/delegator/cctmux/watcher.go`)

The tmux backend's session watcher tails Claude Code's JSONL session file via fsnotify. It converts raw JSONL events into structured callbacks (assistant text, turn completion, usage, agent status). For the stream-json backend (ccstream), see the [ccstream Backend](#ccstream-backend-internalbackendccstream) section below — it receives these events directly on stdout rather than from a file watcher.

**Subprocess startup:** On `Backend.Start`, cctmux spawns `claude` in a tmux window named `cc-{agentID}` in the agent's workspace directory via a login shell (`sh -l -c`). The concatenated system prompt (workspace `*.md` files + skills + environment block) is written to `{workspace}/character/.full-prompt` and passed via CC's `--system-prompt-file` flag. Session ID, if known from a previous run, is passed via `--resume <uuid>` so CC reattaches to the existing session rather than starting fresh. User messages and slash commands are paste-buffered into the tmux pane via `tmux load-buffer -` (piped from stdin — no temp files) followed by `paste-buffer -p` to deliver. Sessions are discovered lazily — the JSONL watcher is created on the first message, not at process startup, so launching never depends on knowing the session ID up front.

**Pre-send offset:** Before `Inject(SourceUser)` pastes the prompt into the tmux pane (via the internal `sendToPane` primitive), the watcher records the current JSONL file size. The watcher starts reading from this offset so it doesn't replay old content from earlier turns. Falls back to `-1` (tail from end of file) if the offset discovery fails.

**Synthetic response filter:** Claude Code emits synthetic messages (model: `<synthetic>`) such as `"No response requested."` and `"[[NO_RESPONSE]]"`. The watcher filters these at the event level — they never reach the reply callback.

**Typing indicator:** Both backends use `SetTypingFunc` to register a callback. Set to `true` when a turn begins (via `Inject(SourceUser)` at idle), set to `false` when `OnTurnComplete` fires. The platform `Connection.SetTyping(bool)` is stateful — `true` starts a periodic ticker (Telegram: 4s, Discord: 9s) that keeps the indicator alive until `false` is called. The ccstream backend also restarts the typing indicator on `OnAssistant` (mid-turn text) and `OnToolProgress` (heartbeats during long tools).

**Usage extraction:** Assistant messages in the JSONL carry a `usage` payload. The watcher extracts `TurnUsage` (InputTokens, OutputTokens, CacheCreationInputTokens, CacheReadInputTokens) from the last assistant message in each turn. This is reported via `TurnState.FinalUsage` on completion. The ccstream backend extracts the same from structured `AssistantMessage` objects on stdout.

**Per-turn completion callbacks:** `Inject(SourceUser)`'s begin-turn path registers a one-shot `OnTurnComplete` handler that fires when the turn ends (`end_turn` in JSONL for tmux, `ResultMessage` on stdout for ccstream). The callback sets `TurnState.FinalText` and `TurnState.FinalUsage`, then closes `TurnState.CompletionChan` — triggering the post-turn goroutine (save, metadata, compaction, logging). Both backends carry it on `Inject.Turn` (`TurnEvents` — per-turn bookkeeping); ccstream fires it from `OnResult`, cctmux from its JSONL watcher's `fireTurnComplete` on `end_turn`.

**Agent spawn tracking:** The tmux watcher tracks pending `tool_use` calls for the Agent tool. The ccstream backend receives task lifecycle events (`task_started`, `task_notification`) as system messages. Both report status via the `onAgentStatus` callback, allowing the platform to show agent activity state.

**Permission auto-approval:** When CC sends a `can_use_tool` permission request, the ccstream backend's `handleToolRequest` first checks against compiled auto-approve rules (from `[permissions]` config). Rules are assembled at startup by `buildAutoApproveRules`: built-in common readonly tools/commands (if `auto_approve_common_readonly` is true, default on), an opt-in built-in safe-write list of side-effecting commands (`curl`, `wget`, `mkdir`, `touch`; enabled by `auto_approve_common_safe_write`, default off — these rules are not path-scoped, so the operator must trust the agent not to target paths outside its workspace), workspace-scoped Edit/Write access, and user-configured patterns from global + per-agent config (union). For Bash commands, the command is split on shell operators (`&&`, `||`, `;`, `|`) and every segment must independently match at least one Bash rule — this prevents `git status && rm -rf /` from being auto-approved by a `git *` rule. Matched requests are approved directly via `SendControlResponse` with an INFO log. Unmatched requests are forwarded to the user via the platform connection with an inline keyboard of choices (Allow, Deny, Always Allow).

**AskUserQuestion handling:** When CC's `AskUserQuestion` tool triggers a `can_use_tool` request, `handleToolRequest` routes it to `handleUserQuestion` (`userquestion.go`) instead of the standard permission flow. The handler parses the questions from the tool input, stores a `pendingPermission` with question state (questions, current index, accumulated answers), and presents the first question as an interactive prompt with option buttons plus Cancel. For multi-question sequences, questions are presented one at a time; each answer advances the sequence. The user can also type a custom text answer (intercepted in `RunInference` before `WaitForPermission` blocks) or cancel via the Cancel button or `/stop`. When all questions are answered, the response is sent as `PermissionAllow` with `updatedInput` containing the original input plus an `answers` map (`{question_text: answer}`). CC receives this as the tool's input and returns the formatted answers to the model.

**Elicitation handling (`ccstream/elicitation.go`):** MCP servers can raise an `elicitation` control_request subtype when a tool call needs structured user input mid-turn. The reader dispatches these alongside `can_use_tool` and `OnElicitationRequest` builds a `pendingElicitation` (separate map from `pendingPerms` — elicitations aren't keyed to tool_use_ids). Two modes are supported: **form** walks the `requested_schema` one property at a time, presenting each field through the same `permPromptFn` platform callback used for permissions. Free-text fields accept typed answers via the same text intercept path as AskUserQuestion (`HasPendingElicitation` from `RunInference`); enum properties render as buttons; booleans render as Yes/No; once every field is satisfied, the accumulated answers are marshalled into a `content` object and sent back as a `control_response` with `action: "accept"`. **url** mode surfaces the URL with Done/Decline/Cancel buttons — Done sends `accept` with no content, while an out-of-band `system/elicitation_complete` notification from CC auto-resolves the matching (`mcp_server_name`, `elicitation_id`) entry without the user clicking Done. Unsupported or missing schemas fall back to a Decline/Cancel-only prompt (foci never synthesises field values it didn't collect). Decline and Cancel at any point short-circuit the walk and send the corresponding action with no content. The drain hook fires only when both `pendingPerms` and `pendingElicits` are empty (enforced by the unified `OutstandingRegistry` — see below) so the platform's "has pending prompt" indicator doesn't flap mid-walk. The `delegator.ElicitationResponder` optional interface exposes `RespondToElicitation` / `HasPendingElicitation` to the agent layer, mirroring `QuestionResponder`.

**Outstanding-prompt registry (`ccstream/outstanding.go`):** All user-input prompts (permissions, AskUserQuestion sequences, MCP elicitations) share one `OutstandingRegistry` per Backend. Each `pendingPerms`/`pendingElicits` insertion is paired with a `Register(requestID, kind)` call; resolutions call `Resolve(requestID)`; CC's `control_cancel_request` calls `Cancel(requestID, reason)`. The registry provides three things on top of the kind-specific stores: (1) a multi-listener cancel fanout — the platform layer registers a per-prompt cancel callback via `Backend.RegisterPromptCancelListener` at the same time it sends the interactive UI, and the registry fires those callbacks (in registration order) when CC cancels the prompt before the user responds; (2) a registry-wide `onEmpty` drain hook (`Backend.SetOnPromptsCleared`) that fires only when ALL outstanding prompts have been removed — fixing a pre-Phase-2 asymmetry where `removePendingPerm` could trigger the drain while elicitations were still outstanding; (3) idempotent semantics — cancelling/resolving an unknown requestID is a silent no-op rather than a side-effecting fall-through. `DelegatedManager.RegisterPromptCancelListener(sessionKey, requestID, fn)` exposes the per-prompt registration to the agent layer; in `cmd/foci-gw/agents_delegated.go`, the platform closure that calls `SendInteractiveMessageWithID` registers a cancel listener that invokes `platform.CancelInteractiveMessage` to disable the orphaned inline keyboard.

### Backend Session Lifecycle

**Session ID persistence:** `SetOnSessionReady` registers a callback that fires when the watcher discovers the CC session UUID from the JSONL path. The UUID is persisted in the state store. On restart, `--resume <sessionID>` is passed to the `claude` command to reconnect to the existing CC session rather than starting fresh.

**Stable exec bridge sockets:** The exec bridge socket path for delegated agents is derived from the session key (not a random value). This means CC retains the same `FOCI_SOCK` environment variable path across foci restarts — shell functions piped through the bridge continue to work without re-sourcing.

**Schema-driven shell functions:** Shell functions for `ExecExport: true` tools are emitted by `generateShellFunc` in `internal/tools/execbridge.go`. A small set of tools with custom UX (stdin reading, accumulator flags, subcommand dispatch — `web_search`, `memory_search`, `web_fetch`, `http_request`, `send_to_chat`, `todo`, `summary`, `spawn`, `tmux`) have hand-rolled cases. Every other tool falls through to `generateGenericShellFunc`, which emits a flag-parser for each schema parameter: snake_case keys become kebab-case flags, booleans are presence-only, strings consume two args, and required params trigger a usage line on missing. Both `--help` text (`generateHelpText`) and the body derive from the same JSON schema, so they cannot drift. `writeShellFuncs` calls `validateShellFuncSchemaParity` before writing — any tool whose schema gains a parameter without a matching `--<flag>` case arm in its body returns an error from `NewExecBridge`, surfacing the failure at production startup rather than at runtime.

**Branch rejection:** Delegated agents return HTTP 400 for `/branch` endpoint requests. The three task-type strategies:
- **Inject into main session** — reflection and compaction-memory prompts are sent directly into the running CC session (no branch needed).
- **New independent CC session** — consolidation, background tasks, and nudge extraction use `RunOnce` (see above), which spawns an independent headless CC process.
- **Reject** — the HTTP `/branch` endpoint is explicitly rejected since delegated agents don't support session branching.

**/reset:** Rotates to a fresh session immediately and returns — it does not block on memory formation. In the background it runs the reflection pass on the old CC session, then destroys that backend (kills tmux pane or closes stream subprocess). The old CC resume ID is cleared before rotation so the rotated-to key does not resume the previous conversation; a new CC session spawns lazily on the next message. See `agent/lifecycle.go:resetDelegatedSession`.

**/stop:** Interrupts the current turn. Tmux backend: sends Escape×2 + Ctrl-C via `send-keys`. Stream backend: sends an `interrupt` control message over stdin. Both halt the in-flight inference/tool execution inside Claude Code.

**Compaction reload-bounce + resume nudge (#828 Part B / #845):** after a delegated `/compact`, CC keeps the same frozen system prompt, so memory/skill edits made during the session never reach the post-compaction context. `runDelegatedCompact` (`agent/compaction.go`) fixes this by bouncing the CC session after a successful compaction (gated on the per-agent `ReloadOnCompact` flag, default on):

- **`DelegatedManager.BounceSession(sessionKey)`** closes the backend but *keeps* the saved resume ID (factored out of `ResetSession` via the shared `closeManaged(sessionKey, clearResume=false)` helper), so the next message respawns CC with `--resume <same session>` — resuming the now-compacted conversation. Part A's `SystemPromptFunc` then rebuilds the prompt from disk on that respawn.
- **Prompt-change gate (#828 follow-up):** every CC session start fingerprints the prompt it launched with — `log.SystemHash` of the effective prompt (`buildDelegatedSystemPrompt`: character files + skill blocks) — stored as `systemPromptHash` on the per-session `managedBackend` record in `m.backends[sessionKey]`. At compaction, `BounceSessionIfPromptChanged(sessionKey)` recomputes the hash from disk and bounces *only* when it differs (returns whether it bounced). This catches character-file edits and skill add/remove (the skill list is in the prompt) but **not** skill body edits (bodies load on demand, never in the prompt). With no `SystemPromptFunc` configured it falls back to an unconditional bounce. Unchanged prompt → no restart → seamless compaction (pre-#828 behaviour).
- **Self-injected resume nudge (#845):** a mid-task flow has no next message to drive the post-bounce respawn, so it would silently stall. `maybeInjectCompactionResume(sessionKey)` synthesises one — `compactionResumePrompt` instructs the model to resume if mid-task or emit the `NoResponseSentinel` if idle — injected via `AsyncNotifier.InjectToAgent`. It is gated twice: it fires **only** when `BounceSessionIfPromptChanged` actually bounced (no restart → nothing to recover from), and is suppressed when `Agent.InboxHasPendingInput(sessionKey)` reports queued/steer input (the user's own follow-up will drive continuation with real intent) or async self-injection is unavailable.

**Automated CC re-login on 401 (`internal/relogin`, #843):** when the shared CC OAuth credential can no longer be refreshed, the subprocess returns a 401 ("Failed to authenticate") and every CC agent is dead until re-authentication. The relogin package automates recovery:

- **Detection (ccstream only).** `isAuthFailure` (`ccstream/authfail.go`) matches a 401 at two sites — the error `result` in `OnResult` (`handlers.go`) and the subprocess stderr/exit path (`lifecycle.go`) — since a dead token can surface either way. The backend fires `onAuthFailure(detail)`, wired via `Backend.SetOnAuthFailure` in `agents_delegated.go` to the agent's `triggerRelogin(reason, sessionKey)` closure.
- **Gate (`relogin/gate.go`).** `relogin.G` is a process-wide single-flight drop gate. The first 401 claims it (`G.Start()`); while active, `Agent.Enqueue` drops inbound messages for delegated agents (the `DelegatedManager != nil` check is the cheap "is delegated" test) — *except* a one-shot capture window (`ShouldCapture(agentID)`) where the triggering agent's next message is treated as the pasted-back login code (`SubmitCode`). Every driver exit path releases the gate (`defer G.Release()`), so a failed or timed-out login can never wedge message processing — the backstop, not the happy path.
- **Driver (`relogin/driver.go`).** `relogin.Run` drives an interactive `claude /login` in a dedicated tmux pane (regular TUI, not stream-json; standalone tmux helper, no cctmux dep), extracts the sign-in URL (`extract.go`), relays it to the user, awaits the code through the capture window, feeds it back, and confirms success. Aborts log at ERROR (surface in `/errors`); the just-submitted one-time code is redacted from any diagnostic screen dump.
- **URL routing (`ba3cd05c`).** The triggering session key threads through `ReloginTrigger(reason, sessionKey)` into the relogin `Config`; the URL is delivered via `conn.SendToSession(sessionKey, ...)` (reads the chat ID straight from the key). Manual `/login` passes `req.SessionKey` so the URL returns to whoever ran it; the auto-401 path passes `""` → the agent's primary/default chat. A hardening fix makes `BotForSession("")` return nil in both telegram and discord so an empty key never matches an idle facet bot and correctly falls through to the agent's primary.
- **Manual trigger (`563bac86`).** `/login` (a `RequiresBackend` command, `command/login.go`) invokes the same flow on demand for testing without waiting for a real 401. The trigger is built once in `configureDelegated` and shared between the 401 callback and the command via `Agent.ReloginTrigger` — wired only for ccstream (nil for cctmux; the command reports unavailability otherwise).
- **Startup readiness probe (#853).** A dead OAuth credential at boot would otherwise stay invisible until the first user turn 401'd — and that first turn carries the first-run onboarding, so the onboarding would be lost to the auth failure. `checkDelegatedReadiness` (`cmd/foci-gw/notifications.go`, called from `main.go` after `StartAll`, before `handleRestartAndFirstRun`) probes each delegated backend via the `Delegator.CheckReady(ctx)` interface method (`delegator/backend.go`). ccstream's impl (`ccstream/readiness.go`) shells `claude auth status` — parsing stdout before honoring the exit code, since the binary prints the same JSON whether logged in or not — and fires the existing `onAuthFailure → triggerRelogin` path when not authenticated; cctmux reports ready unconditionally (`cctmux/turn.go`); API agents (`DelegatedManager == nil`) are skipped. Probes run concurrently but the pass waits for all to settle, so a not-ready agent's relogin gate is reliably active before any startup turn is injected.

**First-run onboarding delivery (#853).** For an agent that has never run (`checkFirstRun` against the `first_run_completed` agent metadata), `handleRestartAndFirstRun` stores the onboarding prompt in `Agent.FirstRunMessage` rather than delivering it as a standalone turn. Both turn transports consume it through a single chokepoint, `consumeFirstRunMessage` (`agent/turn_message.go`): the API path prepends it as a content block in `prepareUserMessage`; the delegated/claude-code path prepends it to the flat prompt in `ComposePrompt` (`turn_delegated.go`). Consumption is exactly-once (atomic `CompareAndSwap`) and fires the `OnFirstRunConsumed` hook only on real consumption — that hook is what marks `first_run_completed`. Previously only the API path consumed it while completion was marked by a generic `OnActivity` callback, so every claude-code agent silently lost its onboarding: the first internal turn tripped `OnActivity`, marking the run done while the message was still pending. Tying the completion marker to actual consumption keeps the two in lockstep on both backends.

**Default-chat seeding (#853).** `seedDefaultChatFromAllowedUser` (`internal/telegram/bot.go`, called from `SetSessionIndex`) seeds the session index's default chat from the sole `allowed_users` entry when no default exists yet. Without it, a fresh install or volume wipe has no default chat until the first *inbound* message — leaving proactive sends (the startup relogin URL, keepalive, cron) with nowhere to go. For a Telegram DM the chat ID equals the user ID, so a single allowed user uniquely determines the chat. Guarded to a no-op unless the agent ID and session index are set, no default already exists, there is exactly one allowed user, and its ID is numeric.

**Bounded shutdown contract.** `Backend.Close` (ccstream) returns within ~9s in the worst case: graceful wait → SIGTERM → SIGKILL → bounded final wait on the waiter goroutine. The final wait *also* has a timeout — if the waiter goroutine stalls (observed when `finalizeExit` callbacks block), the OS still reaps the SIGKILL'd process and `Close` abandons the goroutine rather than hanging forever. `DelegatedManager.ResetSession` and `DelegatedManager.Get` consequently mutate `m.backends` under `m.mu` but call `be.Close()` *after* releasing the lock, so a slow shutdown can never freeze inbound message processing for the whole agent. Regression tests: `TestClose_BoundedWaitWhenWaiterStalls`, `TestResetSession_DoesNotHoldManagerLockDuringClose`.

**Tool execution guarding and redaction:**
- After a tool executes, `guardToolResult()` checks if result exceeds `MaxResultChars`
- If exceeded, writes full result to temp file and returns a guard message (no partial content)
- Prevents large tool outputs from permanently bloating session history
- `agent.Redact` is applied to all tool results and error messages (secret redaction)
- Tool errors are logged as WARN in the event log

### ccstream Backend (`internal/delegator/ccstream/`)

The ccstream backend replaces the tmux-based backend with structured NDJSON communication over stdin/stdout. CC runs as a subprocess with `--input-format stream-json --output-format stream-json --permission-prompt-tool stdio` — no pane management, no screen scraping, no JSONL file watching. Registered as `"claude-code"` via `delegator.Register` in `init()`.

**Protocol:** Each line on the wire is a single JSON object. The `type` field (and optionally `subtype`) discriminates the message kind. Foci writes to CC's stdin; CC writes to foci's stdout. All writes are serialised by a mutex on the `Writer` — no interleaving of JSON lines.

**Message types — stdin (foci → CC):**
| Type | Purpose |
|------|---------|
| `user` | Conversational turn (text or content blocks) |
| `control_request` | Control command (initialize, interrupt, set_model, get_context_usage) |
| `control_response` | Answer to CC's control_request (permission allow/deny) |
| `control_cancel_request` | Cancel a pending CC control_request |
| `keep_alive` | Heartbeat (30s interval) |
| `update_environment_variables` | Inject env vars at runtime |

**Message types — stdout (CC → foci):**
| Type | Purpose |
|------|---------|
| `assistant` | Model response with content blocks (text, thinking, tool_use) |
| `result` | Turn completion with accumulated metrics (success, error, max_turns) |
| `system` | Lifecycle events — subtypes: `init`, `status`, `compact_boundary`, `session_state_changed`, `task_*`, `api_retry`, `hook_started` / `hook_progress` / `hook_response` (from `--include-hook-events`), `elicitation_complete` (URL-mode MCP elicitation finished externally) |
| `control_request` | CC requesting user interaction — subtypes: `can_use_tool` (tool permission), `elicitation` (MCP structured-input request) |
| `control_cancel_request` | CC cancelling a pending permission request |
| `tool_progress` | Heartbeat during long-running tool execution |
| `stream_event` | Token-level streaming (with `--include-partial-messages`) — `text_delta` and `thinking_delta` subtypes are extracted |

**Mid-turn injection:** The unified entry point is `Backend.Inject(ctx, Inject{Source: ..., Text: ...})`. A mid-turn **steer** (`SourceSteer`) folds into the *current* `ask()` **without aborting it**: `markFoldedInject()` records the fold, then `sendUserMessagePriority(text, "now")` (→ `writer.SendUserPriority`) writes the user message to CC's stdin at priority `"now"`. CC consumes it, emits the current turn's `result`, then produces the steered reply as a *separate* result — see **Shadow-turn re-arm + watchdog** below. An in-flight `SourceUser` follow-up folds the same way but at default priority `"next"`. The foci-specific `priority` field is therefore live, not removed. The `interrupt` control request (`Backend.Interrupt`, mirrors the Agent SDK's `client.interrupt()`) is wired to `/stop` **only** — it is *not* used by steer.

**Lifecycle:**
1. `Start` spawns `claude` with stream-json flags, creates stdin/stdout/stderr pipes.
2. Sends an `initialize` control request with the system prompt.
3. Reader goroutine dispatches stdout lines to typed handler methods.
4. `OnSystem("init", ...)` fires `readyOnce` (unblocks `WaitReady`) and persists session ID.
5. Keep-alive goroutine sends heartbeats every 30s.
6. `Close` sends interrupt + EOF, waits up to 5s, escalates SIGTERM → SIGKILL.
7. `Restart` calls `Close`, resets state, calls `Start` with saved options.

**Two-lifetime callback split (TODO #747):** ccstream divides backend callbacks across two distinct lifetimes that match the actual semantics — delivery is session-scoped, bookkeeping is per-turn:

- **`SessionEvents`** (delivery: `OnText`, `OnTextDelta`, `OnThinkingDelta`, `OnToolStart`, `OnToolEnd`) — installed once per session via `Backend.AttachSessionEvents`, stored on the backend in an `atomic.Pointer[SessionEvents]` that's never nil after first attach. Text/tool emission paths (`OnAssistant`, `OnStreamEvent`, hook dispatch) read through this pointer without taking `turnMu` and never drop on a nil handler. The agent layer's `RunInference` re-attaches per turn (idempotent — replaces); closures capture the session router (lazy-built once per session in `inbox.sessionRouterFor`), so they remain safe to call any time the backend is alive.
- **`TurnEvents`** (bookkeeping: `OnTurnComplete`, `PostToolNudgeFunc`, `PreAnswerNudgeFunc`) — installed via `Inject.Turn` for begin-turn paths, captured-then-nilled under `turnMu` in `OnResult` for the fire-once invariant. May legitimately be nil between turns; backend tolerates that. The pre-answer second round explicitly preserves `turnEvents` across rounds — only the round-2 `OnResult` clears it.

This divorces "where does this text go?" (session lifetime — always somewhere) from "did this turn finish yet?" (turn lifetime — might be nil). The pre-TODO #747 design bundled both into one per-turn handler pointer that nilled on `OnResult`, which made post-OnResult text drops a structural inevitability rather than a bug. Both backends now implement the split: ccstream routes delivery through the `atomic.Pointer`-stored `SessionEvents` and bookkeeping through `Inject.Turn`; cctmux's JSONL watcher dispatches into the same `SessionEvents` (delivery) and `TurnEvents` (completion) on its `Backend`. The legacy combined `EventHandler` has been removed.

**Turn flow:**
1. `Inject(SourceUser)` at idle calls `sendToPane`, which calls `beginTurn(turnEvents)` (sets `b.turnEvents`, resets text/tools counters, creates result channel). Delivery is unaffected — `b.sessionEvents` was already attached.
2. `Writer.SendUser(prompt)` writes a user message to CC's stdin.
3. CC processes the turn, emitting `assistant`, `tool_progress`, and `stream_event` messages.
4. `OnAssistant` accumulates text, counts tool_use blocks, and fires `SessionEvents.OnText` / `SessionEvents.OnToolStart`. Mid-turn steer dispatch is handled at the agent's per-session inbox (see `agent.Inbox.Enqueue` routing), not at tool boundaries — this lets text-only turns be steered too.
5. `OnResult` captures final text/usage/model, fires `TurnEvents.OnTurnComplete`, clears `b.turnEvents`, stops typing, signals `WaitForTurn`. `b.sessionEvents` is untouched.

**Shadow-turn re-arm + watchdog (#813):** A folded steer/follow-up makes CC emit the current turn's `result`, then produce the real reply as a SEPARATE result. Clearing the turn on the first result would leave that reply as an untracked **shadow turn** (no live `TurnEvents`/sink) that a colliding inject can route to a dead sink and silently lose. `OnResult` instead re-arms:
- **Trigger = CC's `system init` stream + an inject-time gate, NOT a per-steer counter.** Empirically (`clutch/docs/steer-shadow-turn-design-option3.md` §Phase 2, 65-scenario matrix, haiku+sonnet) **`init` count == `result` count, strictly interleaved `IRIR…`**: CC re-initialises at the start of every continuation cycle, then emits that cycle's result. The old per-steer counter (`pendingSteer++` per `markFoldedInject`) over-counted on **bursts** — 2+ steers in one inter-result gap fold into ONE re-init/result, so the counter awaited phantom results → 45s watchdog. The fix reads the cycle count from CC instead:
  - **`foldPending`** (set-ONCE bool in `markFoldedInject`; bursts collapse N→1) gates the *abort* result into a re-arm. It is still needed because the continuation's herald `init` arrives *after* the abort result (strict interleaving), so init-observation alone can't suppress that first completion.
  - **`continuationExpected`** is set in `OnSystem`'s `init` case when an init arrives mid-turn (`turnActive && sawFirstResult`) — CC's own signal that a continuation cycle began. `OnResult` consumes it: the heralded result completes the turn unless another fold is still pending (chained fold → re-arm again). `sawFirstResult` persists across re-arm (`beginTurn(turn, false)`) and resets only on a genuine new turn (`beginTurn(turn, true)`), so the turn-start init is correctly ignored. `had_turn_events`/orphan heuristics are NOT the trigger.
- **`reArmForContinuation`** (`rearm.go`) re-runs `beginTurn` with the SAME `TurnEvents`, holding sink + refcount so the shadow reply lands in a live turn. Two callers: pre-answer re-dispatch (fresh verification `ask()`) and steer re-arm (empty follow-up — just hold the turn open for the folded reply).
- **`heldResult`.** Stashed before a fold re-arm (`beginTurn` resets `turnText`); if no shadow reply ever arrives, this stashed result IS the answer.
- **Watchdog (MANDATORY).** `armReArmWatchdog` (`:622`) starts a `defaultReArmWatchdogBound = 45s` (`:610`) timer; `watchdogTick` (`:638`) delivers `heldResult` and clears the turn if no second result arrived and the turn is idle. Only turn COMPLETION is held ≤45s — reply text still streams live via `OnText`/`OnTextDelta`.
- **`SourceUser` does NOT re-arm by this path** — priority-`"next"` follow-ups fold into a single `OnResult` (verified across 257 archive cases). Only steer's priority-`"now"` produces the shadow turn.

**Permission handling:** CC sends `control_request` with subtype `can_use_tool`. The backend first checks compiled auto-approve rules (`autoApprovePermission`). Unmatched requests are stored as `pendingPermission` entries and forwarded to the platform via `permPromptFn` (interactive buttons: Allow, Deny, Always Allow). The user's response is sent back as a `control_response` with either `PermissionAllow` or `PermissionDeny`. CC can also cancel a pending request via `control_cancel_request` (e.g. when a hook resolves it).

**Static permission pre-approval:** Both CC backends also pass an `--allowedTools` argv to the `claude` binary at launch. The rule list comes from merging global `[cc_backend] default_allowed_tools` with the agent's `[agents.backend_config] allowed_tools`. The merge happens in `cmd/foci-gw/agents_delegated.go` before calling `delegator.New`, so both backends read the final list from `cfg["allowed_tools"]` the same way. Factory default grants `Read/Write/Edit/MultiEdit(/tmp/**)` so agents can use the system scratch dir without a round-trip — see `internal/config/cc_backend.go`.

**`DelegatedManager.WaitForPermission`:** Before `RunInference` sends a new prompt to the backend, it calls `WaitForPermission` which blocks until all outstanding prompts are resolved. Uses `sync.Cond` with a context-cancellation goroutine (since `sync.Cond` doesn't natively support context). The drain hook installed via `Backend.SetOnPromptsCleared` (which routes through `OutstandingRegistry.SetOnEmpty`) signals the condition variable when the last outstanding prompt — permission, AskUserQuestion sequence, or MCP elicitation — is removed.

**ControlSender pattern (`delegator/control.go`, `ccstream/control.go`):** Generic runtime control for delegated backends. Three layers:

1. **Intent types** (`delegator/control.go`) — backend-agnostic request types (`SetModelRequest`, etc.) with a `ControlRequest` marker interface (unexported method prevents arbitrary types).
2. **`ControlSender` interface** (`delegator/backend.go`) — optional interface backends implement: `SendControl(ctx, ControlRequest) error`. The ccstream backend type-switches on intent types and translates to wire format.
3. **Agent routing** (`agent/delegated_control.go`) — `SendBackendControl(ctx, sk, req) (handled, err)`. Gets the backend via `DelegatedManager.Get`, type-asserts to `ControlSender`, calls `SendControl`. Returns `(false, nil)` if no backend or backend doesn't support it.

Adding a new control: define intent type in `delegator/control.go`, add case in ccstream's `SendControl`, add Agent method, register command with appropriate `Requires`.

**Differences from tmux backend:**
- No tmux pane, no `send-keys`, no pane capture — all communication is structured NDJSON.
- Permissions are handled via structured control messages rather than pane scraping.
- `/stop` sends an interrupt control message rather than Escape×2 + Ctrl-C.
- No `SessionFilePath` — the stream backend stores `SessionID` directly.
- `SendKeystroke` and `SendSpecialKey` are no-ops (no TUI).
- `CaptureCommandOutput` is not implemented — local command output arrives as system messages on stdout.
- Typing indicator is restarted on mid-turn events (`OnAssistant`, `OnToolProgress`), not just on the begin-turn `Inject` path.

#### Hook Integration (`internal/delegator/ccstream/hooks.go`)

CC consumes tool_result blocks internally — they never surface on stdout the way assistant messages or stream events do. To get per-tool completion signals (so the tracker can update "Show results" inline buttons and fire result hints), ccstream installs `PostToolUse` and `PostToolUseFailure` hooks on each session that point at the `bin/foci-cc-hook` helper binary. Install is done via CC's `--settings <json>` CLI flag (see `claude-code/src/main.tsx:1000`, `loadSettingsFromFlag` at line 432) — foci **never** mutates the user's `.claude/settings.local.json`.

**Install at `Backend.Start`:**

1. Resolve hook binary path via `os.Executable()` + sibling lookup, falling back to `exec.LookPath("foci-cc-hook")` on `$PATH`. If neither finds an executable (dev builds, broken packaging), log at **Warn** and skip — the backend runs without tool-result display rather than failing to start.
2. Generate a unique 16-hex-char install ID via `crypto/rand`.
3. Build the shell command string: `"<path>" --install <id>` (path double-quoted so spaces survive bash parsing).
4. Build a JSON settings object: `{"hooks": {"PostToolUse": [{"matcher":"*", "hooks":[{"type":"command","command":<cmd>,"timeout":10}]}], "PostToolUseFailure": [...]}}`.
5. Append `--settings <json>` to the claude argv before spawning.
6. Record `hookCmd` / `hookInstallID` on the Backend struct so `handleHookResponse` can filter events by matching install ID.

**Why `--settings` over file mutation:** CC loads the JSON as an additional settings source called `flagSettings` (`constants.ts:159`). `flagSettings` is always enabled regardless of `--setting-sources` filters, and hooks from multiple sources merge rather than replace, so foci's hook coexists automatically with any user hooks in `settings.json` / `settings.local.json`. The JSON lives in a content-hashed temp file CC creates internally (`loadSettingsFromFlag` at `main.tsx:454`) — identical settings produce the same path across process boundaries, so prompt-cache stability is preserved. Foci has **no filesystem footprint** for hook installation.

**No uninstall step:** `Backend.Close` has nothing to clean up. The CC subprocess exits, its temp settings file is CC's concern, and foci's own state (`hookCmd`, `hookInstallID`) disappears with the Backend struct. There's no shared settings.local.json file to unwind, no mutex, no crash-orphan accumulation, no multi-backend race — each Backend passes its own `--settings` argv and each CC subprocess has independent hook state.

**Multi-backend safety:** two foci backends running CC in the same workdir each generate a unique install ID and each passes its own `--settings <json>` argv. CC's subprocesses have no shared state — each reads its own flagSettings from its own temp file. The install ID is still bound into the hook command and echoed back by `foci-cc-hook` so `handleHookResponse` can filter events by origin — not for race protection (there's no race) but to distinguish foci's hook_response events from any user-installed PostToolUse hooks that fire alongside.

**Hook output path:** when CC fires the hook, it pipes an input JSON envelope (`tool_name`, `tool_use_id`, `tool_input`, `tool_response` / `error`, `agent_id`, ...) into `foci-cc-hook`'s stdin. The helper parses its own argv for `--install <id>`, reads the stdin envelope, truncates `tool_response` / `error` to 64 KB (so each emitted stream line stays under ccstream's 1 MB scanner limit — without the cap a multi-MB file read would blow the scanner and tear down the backend via `OnReaderStopped`), and writes a compact JSON object to stdout. CC captures that stdout and emits it as a `system/hook_response` message on its own stdout, where foci's reader picks it up.

**Dispatch path:** `OnSystem("hook_response", ...)` calls `handleHookResponse`, which applies three filters before firing `handler.OnToolEnd`:

1. **Hook event type:** only `PostToolUse` and `PostToolUseFailure` are processed. Other hook events (user-configured `PreToolUse`, lifecycle events) are silently ignored.
2. **Install ID match:** parses `install_id` from the helper's stdout JSON; events whose ID doesn't match the current backend's `hookInstallID` are dropped. This is what keeps user-authored hook responses out of foci's tracker.
3. **Sidechain filter:** events with non-empty `agent_id` are dropped — sub-agent tool calls belong to the sub-agent's own transcript rather than the parent turn, consistent with the `isSidechain` filter in the cctmux backend.

For events that pass all three, `handler.OnToolEnd(tool_use_id, tool_name, tool_response_or_error, is_error)` fires. The id plumbs through `turn_delegated.go` → `turnevent.ToolResult{ID, Name, Output, IsError}` → `StreamingSink.Emit` → `tracker.ObserveToolResult(id, name, result, isError)` which looks up the entry by id (see Tool Call Visibility below) and updates the correct message.

**Required CC flags:** `--include-hook-events` + `--verbose` in `ccstream.go:Start` (both already set) enable the `hook_response` system message subtype on CC's stream-json output. Without them, hooks would run but their output would never reach foci.

### Interactive Messages (`platform/interactive.go`)

Platform-agnostic interactive messages with button callbacks. `SendInteractiveMessage(conn, text, buttons, callback)` sends a message with inline buttons via `ButtonSender`. When a button is pressed, the callback fires and the message is edited with the return value. Falls back to numbered text choices when the connection doesn't support buttons.

Callback data format: `im:<promptID>:<buttonIndex>`. Prompt IDs are atomic uint64 counters. Callbacks are stored in a global `sync.Mutex`-protected map and auto-expire after 24h (`CleanupExpiredInteractive`). Callbacks are one-shot — removed after handling.

Used by permission prompts (delegated backends), config selection menus, and other platform interactions that need structured user choices.

**Permission-prompt attachments (`PermissionPromptFunc` `attachmentPath`):** the `delegator.PermissionPromptFunc` carries an optional `attachmentPath` — a file the platform closure (`agents_delegated.go`) sends via `conn.SendDocument` *before* drawing the keyboard. Populated only by ccstream's `handleToolRequest` for **ExitPlanMode**: the generic formatter would truncate `input.plan` to a 200-char JSON blob, so instead foci attaches the full plan markdown that CC already wrote to `input.planFilePath` (under `~/.claude/plans/`) and replaces the prompt body with a short caption. Allow/Deny choices are unchanged — over the `--permission-prompt-tool stdio` protocol ExitPlanMode is a plain binary gate (the auto-accept/manual/keep-planning menu is CC-TUI-only and not exposed as `permission_suggestions`). Falls back to the generic rendering when the file is absent (`planAttachmentPath` returns `""`).

### Ask Tool (`ask` / `foci_ask`) and shared `internal/question`

`ask` is a foci-native, **backend-agnostic** equivalent of Claude Code's `AskUserQuestion`, with **no 4-item cap** and an **async** delivery model. It works for delegated (CC) and API agents alike.

**Shared core — `internal/question`:** the pure question machinery (types `Question`/`Option`, `Parse`, `FormatText`, `Choices`, `ResolveAnswer` — including the `qa:<index>`/`qa:cancel` button-data convention — `MergeAnswers`, and a sequential `Accumulator`). Zero backend deps. Consumed by **both** `internal/tools/ask.go` (async) and `internal/delegator/ccstream/userquestion.go` (CC's blocking `AskUserQuestion`, which keeps its control-response wiring but parses/formats/accumulates via the shared package). The 4-item cap only ever lived in CC's tool *schema*, never the parser.

**Async flow (`internal/tools/ask.go`):**
1. `Execute` reads the calling session key via `SessionKeyFromContext(ctx)` (set on the exec bridge ctx by `delegated_manager.go` for CC and `turn_api_tools.go` for API agents), parses questions, registers a `pendingAsk` in the in-memory `askState`, presents the first question, and **returns immediately** with `{"status":"asked",...}` — it does not block (sidesteps CC's 600s Bash ceiling).
2. Each question is one one-shot interactive message (via the presenter, wired in `cmd/foci-gw/ask_setup.go` → `platform.SendInteractiveMessageWithID`). A button click runs the generic `imStore` callback → `askState.handleResponse` → `question.ResolveAnswer` → `Accumulator.Record`; the next question is presented or, when done, the batch is delivered.
3. **Answer delivery** uses the same `SessionNotifyFn` (`newSessionNotifyFn`) as `send_to_session` — it calls `HandleMessage` on the asking session, waking the agent in a fresh turn with the `{questions, answers}` batch as a normal inbound user message. This single path works for both backends.

**App-only batched presentation (`AskPresentBatchFn`):** the native app can render a multi-question ask as ONE on-screen form and return all answers at once; chat transports stay one-question-at-a-time, unchanged. This is an **additive, gated** path: `start()` (and `reattach()` for an un-advanced ask) first tries `tryPresentBatch` → the optional `AskPresentBatchFn` (wired via `tools.WithBatchPresent(newAskPresentBatchFn(...))` in `tool_table.go`). `newAskPresentBatchFn` (`ask_setup.go`) resolves the session's connection and type-asserts `platform.BatchButtonSender`; only the app's `*appConn` implements it. `appConn.SendInteractiveBatch` (`internal/app/conn.go`) returns `batched=false` — falling back to the sequential `AskPresentFn` — unless the client advertised the `"interactiveBatch"` capability in its `ClientHello.Features` (stored per-socket on `wsClient.features`, checked via `convBinding.clientHasFeature`). When it batches, it sends one `fap.Interactive{Questions:[...]}` frame and registers a callback in the hub's separate `batchPrompts` registry (keyed by promptID, distinct from the single-prompt `imStore` path). The app replies with one `fap.InteractiveResponse{Answers:[...]}`; `handleInteractiveResponse` (`dispatch.go`) routes any reply carrying `Answers` to that callback → `askState.handleBatchResponse`, which `ResolveAnswer`+`Record`s every answer positionally then shares the sequential path's `deliverBatch`. A `qa:cancel` in any slot cancels the whole ask. Chat platforms and uncapable/legacy app clients never take this path, so their behaviour is byte-for-byte unchanged.

**Restart persistence (`askState` ↔ `agent_metadata`):** in-flight asks are persisted to the session index under key `ask_pending` (24h TTL) on every change and rehydrated on construction (`restorePending`). For each survivor `reattach` rebinds the callback to the buttons still on screen via `AskRestoreFn` → `platform.RestoreInteractiveCallback` (no new message sent). Two wiring details make a restored ask first-class: (a) the interactive store holds a **`platform.ConnResolver` (`func() Connection`)**, not a captured connection — built by the single `connResolver(connMgr, sessionKey, agentID)` helper in `agents_shared.go` and used by `newAskPresentFn`, `newAskRestoreFn`, **and** the permission prompt in `agents_delegated.go` — so it re-resolves at edit time and survives the startup race where the platform connection isn't up yet when restore runs (no more spurious `ambiguous routing` WARN — `ForSessionOrPrimary` now logs that claimed-but-not-live case at DEBUG); (b) the platform-side message id returned by `SendInteractiveMessageWithID` is captured by `AskPresentFn`, persisted (`persistedAsk.PlatformMsgID`), and handed back through `AskRestoreFn` so proactive cancel/expiry edits reach the restored message too.

**Typed ("Other") answers:** a pending ask is also keyed by session (`askState.bySession`, exposed via `AskRouter`, stored on `Agent.AskRouter`). `Agent.RunTurn` (`run_turn.go`, the platform-message path only) checks for a pending ask and routes a typed reply to `AskRouter.HandleResponse` instead of starting a turn. Gating on `RunTurn` (not the shared `HandleMessage`) ensures system injects (keepalive, reflection, `session_notify`) — which call `HandleMessage` directly — are never mistaken for answers. A typed answer routes straight into `handleResponse`, which never touches the on-screen interactive message, so after recording each answer `handleResponse` calls the **`AskCloseFn`** hook (`tool_table.go` → `platform.CancelInteractiveMessage`) to edit that question's message shut (`✅ <answer>`) and drop its stale buttons. On the button path this is an idempotent no-op (the click already deleted the `imStore` entry and edited the message); on the typed path it is what makes the question visibly "close" — most noticeably for an **option-less** question, whose only button is Cancel and which can *only* be answered by typing. The per-question message id is `questionMsgID(requestID, idx)`, shared by present/reattach/close so all three address the same message.

**Pause / resume answer-capture (`/pause`, `/resume`):** typed-answer capture can be suspended per session. `askState` carries a `paused` flag on each `pendingAsk` (persisted via `persistedAsk.Paused`, so a pause survives a restart mid-ask); `AskRouter` exposes `PauseSession`/`ResumeSession`/`IsPaused` (all no-ops returning false when nothing is pending). The `RunTurn` guard (above) additionally skips answer-capture when `IsPaused(sk)` is true, so the user's typed replies run as **normal turns** while the ask stays pending (buttons still resolve it). The commands live in `internal/command/ask_pause.go` (`PauseCommand`/`ResumeCommand`, registered in `commands.go` only when `Agent.AskRouter != nil`); `Visible` hides them unless an ask is pending, and the in-`Execute` "No active question." guard is the real enforcement (Visible is cosmetic — a hidden command can still be typed). The agent is reminded a session is paused by the **`{ask}` statusline field** (`internal/agent/statusline.go`): it renders an `[ask] ⏸ …` line only while paused (rule 3 drops it otherwise) and names the ask id. Only agents on the default statusline template get the line automatically; a custom template must add `{ask}` itself.

**Registration:** the `ask` tool's `pathBoth` row in the unified tool table (`cmd/foci-gw/tool_table.go`) registers it on both the API path and the delegated/exec path, so `foci_ask` is available to every agent. The build closure calls `tools.NewAskTool(newAskPresentFn(...), ...)` and stashes the returned `AskRouter` on `toolOutputs.askRouter`, which each call site assigns to `Agent.AskRouter`. Shell input is JSON-only (positional object, `--json`, or stdin); the hand-rolled shell func lives in `execbridge.go`'s `generateShellFunc` (`questions` is declared positional so the schema-parity validator skips it). `multiSelect` is accepted but currently single-select.

### API Tool Loop Detail

```
1. sessions.LoadFull(sessionKey)          ← parent[:branchPoint] + own msgs
2. renderStatusline() + prepend to user message text   ← statusline template ([meta]/[state]); buildMetaPrefix removed (#831)
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

Conversation history sent to the API must be a strict append-only extension of the previous request — inserting a message in the middle invalidates all cached tokens after that point. `HandleMessage` enforces this via a per-session turn lock that serializes all callers (Telegram, `AsyncNotifier`, scheduled wakes, HTTP `/send`). Different sessions run concurrently. See [CACHING.md](CACHING.md) for the full cache stability contract.

### OpenCode Backend (`internal/delegator/opencode/`)

The opencode backend drives OpenCode as a coding agent via its HTTP server API. Unlike ccstream (one subprocess per session with NDJSON over stdin/stdout), opencode runs **one `opencode serve` subprocess per foci agent**, shared across all of that agent's sessions. The Backend is its HTTP/SSE client. Registered as `"opencode"` via `delegator.Register` in `init()`.

**Architecture — Server vs Backend split:**

- **`Server`** (one per agent, package-level pool keyed by agentID): owns the `opencode serve` subprocess, the `*http.Client`, and a single SSE subscriber goroutine reading `GET /event`. Refcounted — spawned lazily on first `acquireServer`, killed when the last `releaseServer` hits zero. Bounded-shutdown Close mirrors ccstream's kill-ladder (POST /instance/dispose → SIGTERM → SIGKILL → abandon).
- **`Backend`** (one per foci session): holds the opencode sessionID (created via `POST /session`), a buffered events channel (256), and the per-session/per-turn state (SessionEvents, TurnEvents, turnText, steerBuf). The Server's SSE goroutine routes events to the right Backend by sessionID; the Backend's own dispatcher goroutine drains its channel and calls handlers serially.

**Protocol:** HTTP REST for outbound (`POST /session/:id/prompt_async`, `POST /session/:id/command`, `POST /session/:id/permissions/:permID`, `PATCH /config`, `POST /session/:id/abort`) + SSE for inbound (`GET /event` with `Accept: text/event-stream`). The SSE stream is shared — one subscriber per Server, routing to per-Backend channels via `sessionID` extracted from event Properties.

**Event mapping — SSE → SessionEvents/TurnEvents:**

| SSE Event | Handler | SessionEvents/TurnEvents callback |
|-----------|---------|-----------------------------------|
| `message.part.updated` (text, delta) | `onMessagePartUpdated` | `OnTextDelta` (streaming) |
| `message.part.updated` (text, complete) | `onMessagePartUpdated` | `OnText` + accumulate `turnText` |
| `message.part.updated` (reasoning) | `onMessagePartUpdated` | `OnThinkingDelta` |
| `message.part.updated` (tool, running) | `onMessagePartUpdated` | `OnToolStart` + increment `turnTools` |
| `message.part.updated` (tool, completed/error) | `onMessagePartUpdated` | `OnToolEnd` |
| `message.part.updated` (subtask) | `onMessagePartUpdated` | `OnSubagentText` (blockquote) |
| `message.updated` (assistant) | `onMessageUpdated` | Store `lastModel`/`lastUsage` (no callback) |
| `session.idle` | `onSessionIdle` | `OnTurnComplete` + flush `steerBuf`; during an abort drain, counts burst idles and flushes the buffered steer once settled (see Steer divergence) |
| `session.status` (busy) | `onSessionStatus` | `typingFunc(true)` |
| `session.compacted` | `onSessionCompacted` | `onCompactionDone(0)` + close `compactDoneCh` |
| `session.error` (ProviderAuthError) | `onSessionError` | `fanOutAuthFailure` |
| `session.error` (MessageAbortedError) | `onSessionError` → `failInFlightTurn` | completes the aborted turn (steer abort-drain turn 1) |
| `permission.updated` | `onPermissionUpdated` | `permPromptFn` (Allow/Deny/Always) |
| `permission.updated` (type:question) | `handleQuestionPermission` | `permPromptFn` (option buttons) |
| `permission.replied` | `onPermissionReplied` | cancel-listener fanout |

**Divergences from ccstream:**

- **Steer abort-drain (opencode 1.17.11):** opencode has no mid-turn fold queue (CC's `priority:"now"` has no equivalent). Empirically, a mid-turn `prompt_async` is queued behind the active turn, and `POST /abort` **discards** that queue (a turn sent before/during the abort is lost; a turn sent after survives). So a `SourceSteer` arriving mid-turn buffers in `steerBuf`, calls `Interrupt` (POST /abort) to kill the active turn, then — once the abort's event burst drains (`session.error:MessageAbortedError` + 2× `session.idle`, the empirically observed signature) OR a 500ms backstop timer fires, whichever comes first — flushes the buffered steer as a fresh follow-up turn via `flushSteerBuf`. A second steer during the drain just appends (one abort; the flush combines them). A premature-completion watchdog (`onSessionIdle` / `failInFlightTurn`) `Warnf`s if a turn ends with no text/tools outside a drain — catches any stray abort idle mis-attributed to the steered turn. Backend fields (turnMu-guarded): `aborting`, `abortIdlesSeen`, `abortTimer`, `abortDrainTimeout`. The follow-up uses a nil TurnEvents (no `OnTurnComplete`); text arrives via `SessionEvents.OnText`.
- **Question tool:** opencode's built-in `question` tool surfaces as `permission.updated` with `type:"question"`. Metadata carries the question schema (header, text, options). `RespondToQuestion` POSTs the option label or typed text.
- **Plan delivery:** Uses the prompt body's per-request `agent:"plan"` field (no `PATCH /config`, no swap-back). Simpler than ccstream's `EnterPlanMode` turn.
- **No PostToolUse hooks:** opencode emits tool parts directly on its event bus — no external hook-helper binary needed.
- **No elicitation:** opencode's MCP client doesn't advertise the elicitation capability (commented out, issue #23066). `ElicitationResponder` not implemented.
- **Shared-server auth fanout:** A `ProviderAuthError` on one session fans to all Backends on the same Server (account-wide). `fireAuthFailure` is CAS-gated per-Backend (fires once per lifetime).
- **No RateLimitState:** opencode has no `rate_limit` system message. Cost/token data is per-`AssistantMessage`. `RateLimitState` injection in `agents_delegated.go` is ccstream-only.
- **No automated relogin:** `/login` reports "unavailable" for opencode agents. Auth recovery is per-provider (`opencode auth login <provider>`).

**Lifecycle:**
1. `Start`: acquireServer (lazy pool), POST /session, **write per-session env mapping + ensure shell.env plugin** (see below), registerSession (starts dispatcher), inject system prompt (noReply:true), PATCH /config for default_permission.
2. `Inject(SourceUser)` at idle: beginTurn + POST /prompt_async.
3. SSE events arrive → Server.route by sessionID → Backend.dispatchLoop → handleEvent → SessionEvents/TurnEvents callbacks.
4. `session.idle`: build TurnResult from accumulated state, fire OnTurnComplete, flush steerBuf.
5. `Close`: **remove per-session env mapping**, unregisterSession (stops dispatcher), DELETE /session/:id, releaseServer (refcount-- → shutdown if zero).

**Per-session exec-bridge env routing (`session_env.go`):** The shared-server model pins the subprocess env (including `FOCI_SOCK`/`BASH_ENV`) to whichever session launched it first — all subsequent sessions inherit the first session's bridge socket, misrouting session-scoped exec-bridge tools (`foci_ask`, `send_to_session`). The fix uses opencode's `shell.env` plugin hook: before every bash spawn, opencode fires `Plugin.trigger("shell.env", {sessionID}, {env:{}})`, and a foci-generated plugin (`.opencode/plugin/foci-session-env.ts`) reads a per-session JSON file from tempdir and injects the correct `FOCI_SOCK`/`BASH_ENV`. `Backend.Start` writes `{tempdir}/session-env/{sessionID}.json` with the bridge env from `opts.Env`; `Backend.Close` removes it. The plugin is idempotent (same content every time) and self-locating (`import.meta.dir + "/../session-env"`). ccstream is unaffected — each session has its own subprocess and its own bridge baked into the process env.

## Message Metadata

**Message transforms** (`[[message_transforms]]` in config) run regex find/replace on inbound user messages. Transforms fire before command dispatch — if a message is already a recognized command, transforms are skipped. If transforms produce a command (e.g. `m` → `/mana`), it is dispatched as one. Rules run in sequence; each rule's output becomes the next rule's input.

Each user message then gets a metadata line prepended (NOT in system prompt — that would bust cache):

```
[meta] time=2026-02-21T05:30:00Z gap=3h12m model=claude-haiku-4-5 via=telegram prev_cost=$0.0430 prev_tokens=in:2400/out:312/cR:18000/cW:200
```

- `time` — the time the user's message was received at the platform boundary, not the time the turn was composed. Stamped in `toPlatformMessage` as `QueuedMessage.ReceivedAt` (Telegram: `msg.Date`; Discord: `msg.Timestamp`) and threaded through `agent.WithReceivedAt(ctx, …)` → `TurnState.ReceivedAt` → `composeTurnText` so queued or steered messages show the user's send time rather than the drain/inject time. Falls back to wall clock for system-initiated turns with no platform receipt.
- `gap` — human-readable time since previous message ("3h12m", "2d4h", "38s", "none"). Computed from `time` minus `sessionMeta.lastMessageTime`, which is updated to `TurnState.UserMessageTime()` so gaps also measure user-send-to-user-send rather than inject-to-inject.
- `model` — current model name (e.g., "claude-haiku-4-5", "claude-opus-4-6")
- `via` — transport that delivered the message. Derived from the context trigger via `triggerToPlatform()` in `context.go`. Values: `telegram` (Telegram/voice), `discord` (Discord), `android` (Android app), `api` (HTTP /send), `cron` (system-initiated: keepalive, wake, scheduled, etc.)
- `prev_cost` / `prev_tokens` — cost and token breakdown of the previous turn (omitted on first message)

Per-session state is tracked in `sessionMeta` (in-memory map on Agent). The metadata goes past the cache breakpoint, so it doesn't affect prompt caching.

## Turn Event Stream (Sink Architecture)

All per-turn output — text, thinking, tool calls, retries, typing-indicator lifecycle — flows through a single ordered event stream defined in `internal/agent/turnevent`. The agent is the sole producer; consumers attach a `Sink` to the turn context and receive events as they happen.

### Contract

```go
// internal/agent/turnevent/event.go
type Event interface{ turnEvent() }

type (
    TurnStart     struct{}
    TextDelta     struct{ Delta string }
    TextBlock     struct{ Text string; Phase Phase }  // Intermediate | Final
    ThinkingDelta struct{ Delta string }
    ThinkingBlock struct{ Text string }
    ToolCall      struct{ Name, ID string; Args json.RawMessage }
    ToolResult    struct{ Name, ID, Output string; IsError bool }
    RetryNotice   struct{ Attempt int; Endpoint string; Err error }
    RetrySuccess  struct{}
    Activity      struct{}
    TurnComplete  struct{ FinalText string; Usage *provider.Usage; Cost float64; Model string; Err error }
)

type Sink interface {
    Emit(ctx context.Context, ev Event)
}
```

- `TurnStart` opens every turn; `TurnComplete` closes every turn. Both always fire — the agent emits `TurnComplete` via `defer` from `HandleMessage` so error paths still surface final state.
- Emits are sequential within a single turn (single-producer invariant: API path runs on the caller goroutine, delegated path runs on the watcher goroutine). Sinks don't need internal locks.
- `HandleMessage` returns `error` only. Final text, usage, cost, and model are carried on `TurnComplete` — callers attach a `BufferSink` if they want the old string-return shape.

### Where sinks live

| Package | Sinks | Role |
|---|---|---|
| `internal/agent/turnevent` | `BufferSink`, `RecordingSink`, `NopSink`, `TeeSink`, `SinkFunc` | Leaf package: event types, Sink interface, context helpers, and pure-utility sinks. No platform or turn deps. |
| `internal/turn/sink.go` | `StreamingSink`, `SessionSink` | Shared platform sinks. `StreamingSink` wraps a `TurnRenderer`, `SinkTracker`, and `platform.Connection` — used by Telegram and Discord workers. `SessionSink` delivers via `conn.SendToSession` — used by injected-turn and cross-session notify flows. |

### How interactive platforms wire it

After **TODO #746**, the agent owns turn execution; platforms contribute only renderer/tracker construction and a thin lifecycle envelope.

Each platform's `turn.Platform` backend implements the layout primitives of `turn.ChunkWriter` (`ComposeBody`, `Split`, `SendChunk`, `SendChunkWithButton`, `EditChunk`, `EditChunkWithButton`, `DeleteMsg`) — HTML/4096 for Telegram, Markdown/2000 for Discord. The shared send-edit-cache-delete-orphans control flow lives once in `turn.DeliverChunks` / `turn.EditChunksInPlace` (`internal/turn/deliver.go`); the backend's `Deliver`/`EditInPlace` just delegate to it.

The `agent.Driver` interface is three methods:

```go
type Driver interface {
    WrapTurn(fn func() error) error              // platform-side lifecycle envelope
    NewTurnSink(env Envelope) (Sink, func())     // per-turn renderer/tracker/StreamingSink
    Connection() platform.Connection             // delivery interface
}
```

Per-session worker (`agent.driveAndDrainOrphans`) wraps each turn with a cancellable ctx (registered with the inbox so `Agent.CancelSession(sk)` can fire it for `/stop`), then calls `driver.WrapTurn(func() { return a.RunTurn(...) })`. `Agent.RunTurn` does the per-turn work itself: builds turn metadata via `WithTrigger` / `WithTurnMetadata` / `WithReceivedAt`, gets the per-turn sink from `driver.NewTurnSink(envelope)`, registers it with the session router (see below), and calls `turn.RunTurn`.

The platform's `WrapTurn` runs whatever bot-side lifecycle the bot wants — typing-active flag, post-turn notification drain, gateway-set `OnTurnEnd` / `OnTurnComplete` hooks, error sanitisation. Telegram and Discord both implement it as ~25 LOC.

The Steerer parameter, supplied by the agent worker, returns just the text fields of buffered steer entries — mid-turn injection on the API path (`steerBlocks`) never renders a new meta header, so it discards receipt timestamps. The post-turn orphan-drain loop (when a turn finishes and per-session worker rebuilds leftover steers as a follow-up turn) reads `SteerEntry.ReceivedAt` from the inbox so the follow-up turn's meta header reflects the original user send time rather than the drain time. Note: CC-backed agents bypass the buffer entirely via `agent.Inbox`'s `Backend.Inject(SourceSteer)` routing; the buffer only services API-mode agents and the orphan-drain fallback.

`StreamingSink` routes each event type:
- `TurnStart` → `conn.SetTyping(true)`
- `TextDelta` → `renderer.OnTextDelta` (stream writer edit-in-place)
- `TextBlock{Intermediate}` → `renderer.OnReply` (and marks sink as delivered)
- `ThinkingBlock` → `renderer.OnThinking`
- `ToolCall` / `ToolResult` → `tracker.ObserveToolCall` / `ObserveToolResult`
- `RetryNotice` / `RetrySuccess` → `tracker.NotifyRetry` / `ClearRetryNotification`
- `Activity` → `renderer.OnActivity`
- `TurnComplete` → `renderer.Finalize` (if undelivered) or `renderer.Cleanup` + `tracker.CleanupPreview` (if delivered); `conn.SetTyping(false)`

The delivered flag lives on `StreamingSink`, not on `TurnRenderer`. The renderer is now stateless across `OnReply → Finalize` boundaries. Double-delivery suppression for delegated turns (which stream text via `OnText` and also emit a `TurnComplete` with the same final text) happens automatically: the first `TextBlock{Intermediate}` sets `delivered = true`, and the terminal `TurnComplete` falls through to cleanup-only.

**Sentinel silencing — where StripSilencingSuffix / IsSilent / IsSilencingPrefix live.** Agents emit `[[NO_RESPONSE]]` (and CC sometimes emits `"No response requested."`) to indicate the turn produced no user-visible response. `internal/platform/types.go` is the single source of truth: one `silencingSentinels` list drives three derived functions — `StripSilencingSuffix(text)` removes trailing sentinel(s) and surrounding whitespace (idempotent, handles stacked markers); `IsSilent(text)` is `StripSilencingSuffix(text) == ""` (text that is *entirely* sentinel(s)); `IsSilencingPrefix(text)` is the streaming prefix gate. Delivery chokepoints **strip** rather than merely gate: an agent that appends `[[NO_RESPONSE]]` to a real reply still has its real text delivered, with the marker removed. Filtering happens at exactly five places, each guarding a delivery path no other site reaches:
- **`TurnRenderer.OnReply`** — `StripSilencingSuffix` at the top (text and, for the stream-commit branch, the stream buffer). If it strips to `""`, the silent branch cleans up without delivering. Authoritative gate for intermediate-text delivery on interactive turns. Every downstream method (`editToolPreviewWithReply`, `SendReply`, `EditMessage` on the stream message) is reachable only past this check and uses the stripped text.
- **`TurnRenderer.Finalize`** — `StripSilencingSuffix` applied once after the stream-buffer fallback; empty result takes the silent branch. Authoritative gate for final-text delivery on interactive turns: all downstream send/edit calls live below this check and use the stripped `response`.
- **`StreamWriter.OnDelta`** — `IsSilencingPrefix` on the lazy-start branch. Prefix-aware, applied to the streamed buffer; while the buffer could still resolve to a *pure* sentinel, `sendInitial` is held. This is the only place that can prevent the streamed Telegram message from being *created*. It does not strip a *trailing* sentinel appended after real text (the buffer has already diverged and streamed live) — that transient marker is removed by the renderer's commit edit in `OnReply`/`Finalize`, leaving a brief on-screen flash as the only artefact.
- **`SessionSink.Emit`** — `StripSilencingSuffix` on both `TextBlock{Intermediate}` and `TurnComplete`. SessionSink bypasses the renderer entirely (it calls `conn.SendToSession` directly), so it owns its own pair of gates and delivers the stripped text. The intermediate gate explicitly does not set `delivered = true` when the text strips to empty, so a non-silent final text on `TurnComplete` is still permitted.
- **`asyncDispatch`** (`cmd/foci-gw/http.go`) — `StripSilencingSuffix` on the captured `BufferSink.FinalText` before `conn.SendToSession`. This is the gate for the BufferSink→platform forwarding class: the async wrapper around an HTTP/wake request gets the turn's final text out-of-band of any sink chokepoint, so it owns its own gate. Synchronous HTTP handlers that return `buf.FinalText()` as the JSON response body deliberately do *not* apply this gate — API clients receive the raw response, sentinels and all.

What used to be at `StreamingSink.Emit` on `TurnComplete` (a single sink-level `IsSilent` check) was removed when the renderer's gates were added — it was redundant once `Finalize` had its own gate, and the sink-level check missed the case where `FinalText` was a *concatenation* of normal text + sentinel (text-then-tool-then-`[[NO_RESPONSE]]`), which exact-match `IsSilent` rejected but the streaming path had already partially delivered. The renderer's `OnReply` gate catches that case at the segment boundary, and `StripSilencingSuffix` now removes the trailing marker from the committed text rather than leaking it verbatim.

### How headless callers wire it

- **HTTP `/send`, `/wake`, voice, webhook** (`cmd/foci-gw/http_handlers.go`, `http.go`): build a `turnevent.NewBufferSink()`, attach via `WithSink`, call `HandleMessage`, return `buf.FinalText()` as the JSON response.
- **Injected turns** (`cmd/foci-gw/agents_notify.go → deliverInjectedTurn`): build `turn.NewSessionSink(conn, sessionKey, trigger)`, attach, call `HandleMessage`. SessionSink owns its own delivered flag so intermediate text and final text don't double-deliver.
- **Cross-session notify** (`agents_notify.go → newSessionNotifyFn`): same as injected turns — `SessionSink` routing through `conn.SendToSession`.
- **Async notify with response routing** (`agents_notify.go → newAsyncNotifier`): `BufferSink` captures the target session's final text, then the response is routed back to the caller's session via `deliverInjectedTurn`.
- **Internal hooks** (compaction memory, session-end memory, lifecycle, ratelimit replay): call `HandleMessage` without attaching any sink — the `NopSink` fallback absorbs events silently.
- **Spawn tool** (`internal/tools/spawn.go`): `BufferSink` captures the branch session's response so the tool can return it as a `ToolResult` to the parent agent.
- **Nudge extraction** (`internal/nudge/extract.go`): `BufferSink` captures the rule-extraction response for JSON parsing.

### Steering (pull-direction)

Steering is deliberately separate from the event stream because it flows the other way — the agent needs to ask the platform for pending user input at safe points inside the turn and receive a return value.

```go
// internal/agent/turnevent/steerer.go
type Steerer interface {
    PendingSteers() []string
}
```

Interactive platforms supply a `Steerer` indirectly: `agent.driveAndDrainOrphans` constructs the steerer from the inbox's steer buffer and passes it to `Agent.RunTurn`, which forwards it to `turn.RunTurn`. The agent drains steers via `steerBlocks(ctx)` at tool-loop boundaries on the API path. The delegated path bypasses the steerer for mid-turn injection — `agent.Inbox.Enqueue` calls `Backend.Inject(ctx, Inject{Source: SourceSteer, Text: ...})` directly when a steer arrives during an in-flight CC turn. In the opencode backend Inject triggers the abort-drain sequence (Interrupt → drain the abort burst → flush the buffered steer as a fresh turn); in ccstream it folds via `priority:"now"`.

## Deferred Replies

When the model responds with text alongside `tool_use` blocks (e.g., "Looking into this..."), the text is sent to the platform before tool execution begins. This allows the agent to acknowledge a message and deliver the full response later.

Controlled by `batch_partial_assistant_messages` (bool, default `false`):
- **false (default):** Text is sent immediately via a `TextBlock{Intermediate}` event each time it appears in a response.
- **true:** Text is accumulated in a `strings.Builder` and folded into `ts.FinalText` when the turn completes; only the combined text reaches the sink via `TurnComplete.FinalText`.

**Flow (batch=false, default):**
1. Caller attaches a `turnevent.Sink` via `turnevent.WithSink(ctx, sink)`
2. Agent loop detects text in a `tool_use` response
3. `emitIntermediateText(ctx, text)` emits `TextBlock{Intermediate}` through the sink
4. Agent continues executing tools
5. The terminal `TurnComplete` carries the final response text

**Flow (batch=true):**
1. Agent loop detects text in a `tool_use` response and appends to `batchedText`
2. On `end_turn`, batched text is prepended to final text (joined with `\n\n`)
3. Concatenated text is carried as `TurnComplete.FinalText`

Sinks are **context-scoped**, not agent-global. Each turn gets its own sink.

## Tool Call Visibility

Tool call display is controlled by `show_tool_calls` (string: `"off"`, `"preview"`, `"full"`). Configurable globally in `[telegram]` and per-agent in `[[agents]]`. Bool values are accepted for backwards compat (`true` → `"preview"`, `false` → `"off"`).

**Modes:**
- **`"off"`** (default) — Tool calls are hidden. The tracker's `ObserveToolCall` returns immediately.
- **`"preview"`** — Tool calls are shown via send+edit, then the final response **overwrites** the tool message (or falls back to a new message if too long).
- **`"full"`** — Tool calls are shown via send+edit (same as preview), but the final response is always sent as a **separate new message**, preserving the tool call log in chat.

`ToolCall` and `ToolResult` are `turnevent.Event` types routed by `StreamingSink` directly to the platform tracker — there is no separate `BuildTurnObservers` wiring.

**Tracker state machine (`internal/turn/tracker.go`):** `ToolCallTracker` keys its per-tool state by `tool_use_id`, not by insertion order. This matters when Claude batches multiple `tool_use` blocks in a single assistant message (common: three `Read` calls, a `Grep` + `Bash`, etc.) — each tool call gets its own `trackerEntry{msgID, text, fullText, lastParams}` so parallel `ObserveToolResult` calls each update the correct message's hint and store entry, regardless of arrival order. Preview mode uses a sentinel `""` key for the single shared preview message that every call edits in place. `LastMsgID` / `ResetMsgID` return and clear the most-recently-inserted entry respectively, preserving the preview-mode "one message edited by reply" UX.

Cctmux plumbs the id through via `handleAssistant` recording `toolNamesByID` at tool_use time and looking it up in `handleUser` when the tool_result arrives. Ccstream plumbs the id via the CC hook integration described above — both paths feed `handler.OnToolEnd(id, name, output, isError)` which the StreamingSink forwards to the tracker.

**Ordering with deferred replies:** When intermediate text fires between tool loops, `OnReply` resets `toolMsgID` to 0. This forces the next tool call to create a fresh message below the text, preserving chronological order in chat.

**Flow (multi-loop turn, preview/full):**
1. Loop 1: API returns `[tool_use(exec)]` — `notifyToolCall` sends message A (`toolMsgID=A`)
2. Loop 2: API returns `[text("Checking..."), tool_use(read)]`
   - `emitIntermediateText` emits `TextBlock{Intermediate}` → `StreamingSink` calls `renderer.OnReply` → sends message B, resets `toolMsgID=0`
   - `notifyToolCall` sends message C (`toolMsgID=C`, fresh because reset)
3. Final:
   - **preview**: `end_turn` response edits message C with the answer
   - **full**: `end_turn` response sends as message D (new message)

**Chat order (preview):** A ("🔧 exec") → B ("Checking...") → C ("🔧 read" → final answer) ✓
**Chat order (full):** A ("🔧 exec") → B ("Checking...") → C ("🔧 read") → D (final answer) ✓

**Inline result expansion (full mode only):** In "full" mode, each tool call message includes a "Show results" inline keyboard button. Pressing it expands the message to include the tool's output (truncated to fit Telegram's 4096-char limit). "Hide results" collapses back.

- `ToolResultObserver` callback fires after each tool execution (both success and error), storing the result in the bot's shared `turn.ToolResultStore` (in-memory `sync.Map`, message ID string → `turn.ToolResultEntry`; one implementation shared by Telegram and Discord). Write-through: if a `tooldetail.Store` is wired via `SetToolDetailStore`, the store also persists to SQLite (`tool_details.db`) so inline keyboard expansions survive restarts. On startup, `SetToolDetailStore` loads entries <48h old into the in-memory map. Periodic idle cleanup (10min tick, runs when all users idle) expires old entries and runs `PRAGMA incremental_vacuum`.
- `handleCallbackQuery` processes `tc:show:<msgID>` / `tc:hide:<msgID>` button presses, editing the message and answering the callback query. Also handles `cmd:/name args` for inline keyboard command selections.
- `pollUpdates` requests `AllowedUpdates: ["message", "callback_query"]` to receive button press events.

**Inline keyboard commands:** Commands with a `KeyboardOptions` field (`/model`, `/thinking`, `/effort`, `/config`, `/sessions`, `/tmux`) show an inline keyboard when invoked bare. `LookupKeyboard()` checks for this before `Dispatch()`. `sendCommandKeyboard()` builds and sends the keyboard via `platform.ButtonSender`. Callback data format: `cmd:/name args`. `handleCommandCallback()` executes the command and edits the message to show the result. `command.KeyboardOption` is aliased to `platform.ButtonChoice` (Label, Data, Row fields) — the same type used for all button interactions across both Telegram and Discord.

## Thought Queue (Reminders)

The agent can defer thoughts for later via the `remind` tool. Reminders are stored in SQLite (`reminders.db`) and surfaced as injected context when due. With `wake=true`, the session is actively woken at the specified time.

**Tool registration:** `remind` is `ExecExport: true`, so it is exposed both as a native API tool (in API-mode agents) and as a `foci_remind` shell function via the exec bridge (in delegated/Claude Code agents). The wake-scheduling machinery (`buildWakeScheduler` in `cmd/foci-gw/agents_notify.go`) is built once per agent in `setupAgent` — transport-independent — and the resulting `tools.ScheduleWakeFn` is held on `sharedAgentSetup.wakeScheduleFn` and passed into `toolDeps.wakeFn`. The `remind` row in the unified tool table (`cmd/foci-gw/tool_table.go`) is `pathBoth` and gated on `reminderStore != nil && wakeFn != nil`, so the single `registerTools` driver adds it to whichever registry (API or exec) is being built.

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
[state] task: 3/7 "Boil an egg" → Bring water to rolling boil | todos: 2 open (1 high) | scratchpad: 1 entry
[reminders]
- Look into FTS5 phrase boosting (set 2h, due: 2026-02-21 05:00)
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

**State dashboard:** A `[state]` line is rendered as the second line of the statusline template (`[state] {state}` in the default), so it now sits with `[meta]` *before* any `[reminders]` block (#831 merged the old separate `[state]` generator into the template). The `{state}` field calls `stateDashboardBody`, joining components shown only when non-empty: task progress (`tasks: 2/5 → first active`), open todo count, scratchpad entry count. Queries `TaskListStore`, `TodoStore`, and `ScratchpadStore` on the Agent struct. The whole line self-omits when every store is empty (statusline rule 3). The granular `{todos}`, `{tasks}`, `{scratchpad}` fields render these individually — see the `statusline` config in `docs/CONFIG.md`.

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

1. **Environment block** (`agent.EnvironmentBlock`) — programmatically built at startup from config values. Contains workspace path, agent ID, platform URL, messaging platform, config/log paths, message metadata docs, and session structure. Built by `buildEnvironmentBlock()` in `main.go`, stored as a string on the Agent struct, prepended as the first `SystemBlock` in `HandleMessage`. Omitted when `[environment] enabled = false` (empty string).

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

### Live Model Capabilities (`modelcaps`, #840)

`internal/modelcaps` is a leaf cache of `Caps{ContextWindow, MaxOutput, Effort, Thinking}` advertised by the backend's `/v1/models` (the capabilities the SDK's typed `Model` drops). It is the live layer between the per-model config override and the static `modelinfo` registry: context-window and effort lookups prefer it, falling back to the static registry on a cold/empty cache so behaviour is never worse than before.

- **Per-backend registry.** Capabilities are a property of the backend *type*, not the model alone, so the cache is a registry of per-backend stores keyed by backend type (`BackendCCStream`, `BackendAPI`, future `codex`). `BackendKey(configBackend)` maps the config backend string (`""`/`"api"`/`"ccstream"`) to a key. Public API: `LookupFor(backend, model) (Caps, ok)`, `ModelsFor(backend) []string` (sorted ids, cold→nil), `SetFetcher(backend, fn)`, `Refresh(ctx, backend)`. Background single-flight refresh; serve-stale on fetch error; TTL grows 6h→48h.
- **Fetcher seam.** `anthropic.FetchModelCaps` (raw `GET /v1/models`) is injected via `SetFetcher` so the package stays a DB/anthropic-free leaf. `AnthropicResolver.ModelCapsFetcher` supplies it from CC OAuth creds; nil creds → no fetcher → static fallback.
- **DB persistence (`e301379b`).** `SetPersister` + `Restore` bridge the cold-start gap. `session` gains a `model_caps` table (`backend, model, context_window, max_output, effort_json, thinking_json, fetched_at`) with `SaveModelCaps`/`LoadModelCaps` on `SessionIndex`; saves are transactional (delete+insert) so a reader never sees a half-written catalogue. `cmd/foci-gw/modelcaps_persist.go`'s `modelCapsPersister` adapts `SessionIndex`↔`Caps` (the one place that knows both types). `doFetch` persists after each swap (DB write outside the store lock so it never blocks a lookup); `Restore` declines to clobber a cache a fetch already populated (startup race guard). Restart always re-fetches; the DB restore only covers the in-flight gap.
- **Agent routing.** `Agent.BackendType()`, `Agent.ModelCaps(model)`, and `Agent.BackendModels()` route caps reads through the agent's own backend; consumers (session context limit, command context-limit resolver, `/effort` choices, `/model` keyboard) read via the agent. Compaction takes an injected `ModelCapsFn` bound to the agent's backend.

**Effort plumbing.** `/effort`'s level set is resolved per call: `newSessionSettingCommand`'s optional `DynamicChoices` hook reads `modelcaps.LookupFor`, building levels in catalogue order (e.g. opus-4-8: low/medium/high/xhigh/max) with matching numeric aliases; a catalogue miss falls back to the static low/medium/high. Two delivery paths make effort both instant and durable:
- **Live push (`39581989`).** `Agent.SetSessionEffort` persists, then for a delegated session fires `delegator.ApplyFlagSettingsRequest{Settings: {"effortLevel": value}}` in the background → ccstream's `SendControl` emits `{"subtype":"apply_flag_settings","settings":{...}}` so the next turn runs at the new effort with no bounce (mirrors `SetPermissionMode`'s optimistic fire-and-forget). The command layer must reject invalid settings first — CC does not validate. API-loop sessions apply effort at turn time via `output_config`, so no control is sent. `clear`/`off` skip the live push.
- **Cold-launch flag (`1aacc877`).** `apply_flag_settings` is session-local: a bounce (post-compaction reload, idle respawn) drops the override. `StartOptions.Effort` + `EffortFunc(sessionKey)` (mirrors `SystemPromptFunc` — resolved fresh per session start in `getOrCreate`, bound to `ag.SessionEffort`) make ccstream `Start` append `--effort <level>` (empty/`off` omits it). The control is the happy path; the launch flag is the backstop.

**`/thinking` backend gate (`22b3fa19`).** CC exposes no thinking control and effort subsumes it, so `/thinking` is hidden on ccstream via a backend-keyed `BackendGate` on `sessionSettingDef` (distinct from the model-keyed `Capability`), consulted in both `Visible` (hide) and `Execute` (reject). API agents keep it.

**`/model` keyboard (`275b492a`).** `/model`'s `KeyboardOptions` now offers one button per model `Agent.BackendModels()` (→ `modelcaps.ModelsFor`) advertises, marking the current model with a check. A cold catalogue falls back to typing the name.

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
- **Path blocking:** Two layers. `Store.IsBlockedCommand` is an advisory substring scan on shell command lines. `Store.IsBlockedPath` is a canonical-path (absolute, symlink-resolved, component-aligned) enforcement check that every in-process file tool calls via `fileScope.resolveFileArg` (`internal/tools/files.go`) — `read`/`write`/`edit`, `summary`, and `http_request`'s `body_file`/`files[]`/`save_to` — so tools running at gateway privilege still cannot touch `secrets.toml` or `/proc/self/environ`.
- **Subprocess group boundary:** `procx.Setup` (`internal/procx`) drops the `foci-secrets` group from spawned children and, after the startup probe, clears the process ambient capability set so children can't inherit `CAP_SETGID` and re-add the group. The foci user is granted the group per-process (systemd `SupplementaryGroups` / `setpriv --groups` / `runuser --supp-group`), never as an `/etc/group` member. See [SECRETS.md](SECRETS.md).
- **Outbound SSRF:** `web_fetch` and `http_request` share one SSRF-safe `*http.Client` (`internal/tools/safehttp.go`): its dialer validates each resolved IP at connect time (blocking loopback/private/link-local/metadata/unspecified, defeating DNS rebinding) and re-checks every redirect hop.

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
| `http_request` | http.go | Domain-locked HTTP requests over the shared SSRF-safe client (resolved-IP validation + redirect re-check; see safehttp.go). Secrets in headers/body validated against per-section `allowed_hosts` before sending. Cross-domain redirects blocked when secrets present. `body_file`/`files[]`/`save_to` paths run through `fileScope` (blocklist + isolated containment). Response redacted. Binary responses (image/*, audio/*, etc.) auto-saved to temp file. `save_to` saves any response to a specific path. `save_from_json_path` extracts a value from JSON response and decodes data: URIs (base64 images from generation APIs). |
| `tmux` | tmux.go | Manage tmux sessions — start (auto-watches by default), send keys, read pane output, list, kill, watch for inactivity, unwatch. Owned sessions persist across app restarts via state store. Autopilot mode (default on): auto-unwatches after inactivity notification, auto-watches on send. |
| `read` | files.go | File contents with line numbers, truncates at 2000 lines |
| `write` | files.go | Create/overwrite files |
| `edit` | files.go | Find-and-replace (old_string must be unique). Syntax validation for .json, .toml, .go, .yaml/.yml, .xml, .py, .sh/.bash: rejects edits that would break a valid file, warns if file was already invalid. |
| `web_fetch` | web.go / server | Fetch web content (server-side default, client-side fallback). Client-side fetches use the shared SSRF-safe client (safehttp.go) — resolved-IP validation blocks metadata/private/loopback in all modes. |
| `web_search` | web.go / server | Web search (server-side default, Brave fallback) |
| `summary` | summary.go | Summarize/extract from large files via Haiku call |
| `memory_search` | memory.go | Full-text search over memory files (+ conversation history for FTS5). Pluggable backends: FTS5 (default) and bleve. Porter stemming, weighted ranking, sort by relevance or recency. Optional `backend` parameter when multiple backends are active. |
| `remind` | remind.go | Defer a thought for later; stored in SQLite, surfaced as injected context when due. `wake=true` actively wakes the session. |
| `scratchpad` | scratchpad.go | Working notes that survive compaction (write/read/clear/list via `action` parameter) |
| `spawn` | spawn.go | Unified sub-call: four context modes. All modes have tool access with a tool-call loop. `raw`: one-shot, no system prompt (`send_to_chat` and `send_to_session` blacklisted — no character context means no communication awareness). `character`: one-shot with character files (all tools except `send_to_session` — an ephemeral one-shot spawn has no session of its own to receive replies, so it must not inject into other sessions; it returns its result to the caller instead). `clone` (default): branch session — a headless self-fork. `explore`: one-shot safe exploration with `ls`, `find`, `grep`, `read`, `memory_search`, `web_search`, `web_fetch` only — no file mutation, no shell exec, no messaging, always haiku. clone creates branch `{parentKey}/b{TIMESTAMP}`, always runs async via `AsyncNotifier` (returns immediate ack, delivers `[SPAWN RESULT]` on completion). Recursive clone blocked via context key. Concurrent clone limited by `max_concurrent_spawns` (default 3). `spawn` itself is excluded from one-shot tool sets to prevent recursion. |
| `ls` | explore.go | List directory contents. Internal to `explore` spawn mode — not registered in the main tool registry. |
| `find` | explore.go | Search for files in a directory hierarchy. Dangerous predicates (`-exec`, `-delete`, etc.) blocked. Internal to `explore` spawn mode. |
| `grep` | explore.go | Search file contents using the best available binary (rg > ack > ag > grep). Flags are validated and translated to the active binary's dialect. Internal to `explore` spawn mode. |
| `send_to_chat` | telegram.go | Send proactive Telegram messages (text, documents, voice notes). With `send_as="voice"` and text (no file_path), synthesizes speech via TTS. Routes to the chat extracted from the session key (`X/cCHATID/{versionTS}`) so per-chat sessions get messages to the correct user. Falls back to bot's default chat when no chat ID in session key. |
| `send_to_session` | session_send.go | Inject a user-role message into another session. Tags the message with `[Message from session ...]` origin header. Appends to session store and triggers processing via `AsyncNotifier`. Used for cross-session communication (e.g. facet branches talking to main). Target accepts full key, partial key (`agent/typeID`), or bare agent name (`scout`) — non-full keys resolve via `SessionIndex.ResolveLooseKey` (1 seg → `DefaultSessionKeyForAgent`, 2 seg → `ResolvePartialKey`; same dispatcher the `foci debug` CLI uses). |
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

**`foci-call` binary** (`cmd/foci-call/`): Reads `FOCI_SOCK`, connects to unix socket, sends JSON request (newline-terminated), prints result to stdout or error to stderr, exits 0/1. 1MB scanner buffer for the response envelope.

**Large-result spill handoff:** A tool may return its full result on disk via `ToolResult.ResultFile` (set by the http tool when the response body exceeds the inline preview, and by the shell tool when command output overflows — both via `tools/spill`). In that case the bridge response carries a small `{result, result_file, result_size}` envelope rather than inlining megabytes, and `foci-call` streams the file straight to stdout (same-UID, 0600 file). So `foci_http_request url | jq` pipes the complete body without it passing through the socket as one giant JSON line; the calling agent (CC for delegated, the tool-result guard for API) then applies its own output truncation. Falls back to the inline preview if the file can't be opened.

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
1. **Built-in** (code-defined in `command/builtins.go`): `/ping`, `/status`, `/cache`, `/last`, `/cost`, `/mana`, `/reset`, `/model`, `/session`, `/tools`, `/tmux`, `/config`, `/log`, `/errors`, `/version`, `/uptime`, `/voice`, `/facet`, `/pass`, `/login`
   - `/mana` — check quota remaining (`/usage` is a hidden alias)
   - `/login` (`RequiresBackend`, ccstream only) — manually trigger the automated CC re-login flow (see [Automated CC re-login on 401](#backend-session-lifecycle)); URL returns to the chat that ran it
   - `/pass` — forward a command directly to the delegated backend (e.g. `/pass /context`, `/pass /model opus`). Bypasses foci's command dispatch so CC slash commands that would otherwise be intercepted by foci can be sent through. For tmux backends, captures and returns pane output after stabilisation. For stream backends, output arrives normally via the stdout reader. Only available for delegated agents — returns an error for API-mode agents.
2. **Custom** (script-defined in `foci.toml` via `[[commands]]`): runs a shell script, returns stdout. Timeout default 10s.

**`/model` endpoint switching:** Accepts `endpoint:developer/model_id` syntax (e.g. `/model gemini:google/gemini-2.5-flash`, `/model openrouter:anthropic/claude-opus-4-6`). The Execute function calls `config.ResolveModel()` to parse the `developer/model_id` string and `cc.ClientProvider.ResolveEndpointClient(endpoint, format)` to lazy-init the correct client. Calls `cc.Agent.SetModel()` — the orchestrator that updates foci's session metadata AND sends a `set_model` control request to the delegated backend (if any). Sets `modelUserSet` flag to prevent `UpdateSessionMeta` from clobbering the user's explicit choice with the backend's reported model.

**Command `Requires` field:** Commands declare their transport requirement via a static `Requires` field on the `Command` struct (`RequiresNothing`, `RequiresBackend`, `RequiresAPI`). `Dispatch()` checks this before calling `Execute`, rejecting with a clear error. The help renderer also filters by `Requires` — backend-only commands don't appear for API agents.

**Command registration** (`commands.go` in main package): All per-agent slash commands are registered in `registerAgentCommands()`, which builds a `command.CommandContext` struct from agent references, config, clients, and stores. Commands are zero-argument constructors (e.g. `ModelCommand()`, `ResetCommand()`) returning `*Command` structs with an `Execute(ctx, Request, CommandContext)` function. All command logic accesses dependencies through the `CommandContext` parameter — no closures or per-command constructor injection. Commands interact with platforms via `cc.ConnMgr` (a `platform.ConnectionManager` interface) to avoid importing the `telegram` package.

**`/plan` — backend-contributed delivery (#857):** `/plan <request>` puts the coding-agent backend into plan mode, but the mechanism differs per backend: cctmux forwards `/plan <args>` verbatim as a `SourcePass` slash command (CC's TUI handles native `/plan`), while ccstream — where native `/plan` is unavailable headless — drives a fresh EnterPlanMode turn via `AsyncNotifier` (the #845 proper-turn path). Rather than a `switch p.acfg.Backend` at the registration site, each backend *contributes* its behaviour: `ccstream`/`cctmux` call `delegator.RegisterPlan(name, planDelivery)` in `init()` (alongside `delegator.Register`), and `registerAgentCommands()` registers `/plan` iff `delegator.PlanDeliveryFor(p.acfg.Backend)` returns a delivery. The generic command (`command/plan.go`) owns the guards (delegated-only, non-empty args, resolved session) and wires `delegator.PlanDeps` (session key, `AgentInjector` notifier, lazy `Backend()` thunk); the injected `PlanDelivery` owns the backend-specific send. This mirrors the existing "register iff capability present" pattern (`TmuxCommand` gated on `tmuxTool != nil`, pause/resume on `AskRouter != nil`).

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

Three goroutines per bot:
```
[receiver goroutine]   →  receive msg  →  wizard active?  →  yes: route to wizard, reply
                                       →  slash command?  →  yes: execute, reply
                                       →  voice note?     →  download OGG, transcribe via Whisper → text
                                       →  photo/doc/PDF?  →  download attachment via Telegram file API
                                                           →  MessageQueue.Enqueue() routes to:
                                                              - GroupThrottle (group chat + throttle configured)
                                                              - drop (group + require_mention + no throttle + no mention)
                                                              - main channel (everything else)
[agentMessagePump goroutine]  →  drain mq.Chan()  →  build Envelope  →  agent.Enqueue(env)
[commandWorker goroutine]     →  drain mq.CmdChan()  →  execute command  →  reply

[per-session worker goroutines — lazy, one per active session key, owned by agent.Inbox]
  →  batch available Envelopes  →  Agent.RunTurn(ctx, sk, batch, steerer, router, driver)  →  HandleMessage  →  reply
```

`platform.MessageQueue` is a thin filter-and-throttle helper. It wraps a buffered channel (main messages) plus a command channel, with two routing rules:

- **Group throttle** (`group_throttle`): Non-mention group messages accumulate in a `GroupThrottle` per chat ID. A fixed-window timer flushes them as a batch. @mentions flush immediately and reset the cooldown.
- **Require mention** (`require_mention`): Without throttle, non-mention group messages are dropped. With throttle, they're buffered.
- **Sender attribution**: Group chat batches prefix each message with `[senderName]` for multi-user context.

Steer routing moved out of `MessageQueue` and into `agent.Inbox.Enqueue`: mid-turn text-only messages are routed to the per-session steer buffer (API agents) or dispatched directly via `Backend.Inject(SourceSteer)` (CC agents) inside the agent layer, without the platform layer needing to know.

The receiver never blocks on the agent. Slash commands (including `/stop`) execute immediately on the receiver goroutine. Agent messages fan out by session key via `agentMessagePump` → `agent.Enqueue`; per-session workers in `agent.Inbox` serialize turns within a session. Different sessions on the same bot run their turns in parallel.

**Stale command filtering:** Slash commands older than 30s are silently dropped. Safety net for update replay after crashes — prevents stale `/reset` or `/stop` from firing on restart.

**Shutdown ack:** On context cancellation, each bot's poll loop fires one final `GetUpdates` with the last processed offset. This acknowledges processed updates to Telegram, preventing replay on restart. `BotManager.Wait()` blocks main after `cancel()` to ensure all bots complete this ack before process exit.

**Wizard routing (`WizardHandler`):** Interactive wizards (e.g. `/agents new`, `/secrets set`, `/android`) take over message routing via `Registry.HandleMessage()`. When a wizard is active, ALL messages (including non-`/` text) are intercepted by the receiver goroutine before reaching slash command dispatch or the agent queue. `/cancel` and `/stop` abort the active wizard. The wizard is cleared automatically when it signals completion (`done=true`).

**`/android` — native Android onboarding wizard (`command/android_onboard.go`):** `AndroidCommand()` + `androidWizard` walk the user through pairing a device. `Execute` branches on state: app provider disabled → offer to enable (appends `[[platforms]] id="app"` to foci.toml via `appendToFile` and generates `app.api_key` with `secrets.GeneratePassphrase(5)`); enabled + auto-generated key → offer to reveal the key in chat or point at `secrets.toml`; enabled + user-set key → skip to host. Auto-generated detection uses `secrets.IsGeneratedPassphrase` (all-EFF-wordlist hyphenated tokens — no stored marker). The host step emits a `foci://pair?host=…&key=…` string the Android client's `parseQr` accepts. If the wizard enabled the app provider this run (`justEnabled`), it then runs a restart-confirm step — the running server loaded its config before the `id="app"` line was appended, so the `/app` endpoints stay dark (every request 403s on the global auth middleware) until a restart; on `yes` it calls the shared `restartFunc` (same hook as `/restart`). Reads the key via `SecretsStore.Get` (added to the interface); registry handle comes from `cc.AndroidDeps.Registry`. No agent LLM involved — a pure Go wizard like the others.

**Attachment handling:** Photos (`msg.Photo`, largest size selected), image documents (`msg.Document` with image MIME type), and PDF documents (`msg.Document` with `application/pdf` MIME type) are downloaded via `GetFile()` + HTTP GET. The raw bytes are queued as `attachment` structs alongside the message text (which may come from `msg.Caption` for photos). PDFs over 32MB fall back to save-to-disk with a text annotation. The agent worker converts these to `platform.Attachment` and calls `HandleMessage`, which routes images to `ImageBlock()` and PDFs to `DocumentBlock()` content blocks.

**Turn cancellation:** Each agent turn gets its own `context.WithCancel`, owned by `agent.driveOnce` (post-TODO #746) and registered on the session's `sessionInbox.turnCancel`. `/stop` calls `Agent.CancelSession(sk)`, which fires that cancel. Cancellation propagates to in-flight API calls (HTTP client context) and tool executions (process group kill). Multi-user shared bots are precise per session — `/stop` from chat A doesn't affect chat B's in-flight turn.

**Reset guard:** `/reset` refuses when `agent.IsProcessing()` is true — prevents clearing an active conversation mid-turn.

## Streaming Output (`telegram/stream_writer.go`)

When `stream_output = true` and `streaming = true`, model output is shown in Telegram in real-time as tokens arrive, rather than waiting for the full response.

**Lifecycle:**
1. `Bot.NewTurnSink` creates a `streamWriter` with the bot's `tableOpts` (no goroutines started yet) when `Agent.RunTurn` requests the per-turn sink
2. On the first `TextDeltaObserver` delta, the stream writer sends an initial HTML-formatted message and starts a ticker goroutine — gated by `platform.IsSilencingPrefix` (see below)
3. Each tick, if new text has accumulated, the buffer is processed through `closePartialMarkdown` → `ConvertToTelegramHTML` and the message is edited with HTML formatting
4. When `HandleMessage` returns, `Finish()` stops the ticker and returns the message ID
5. The final HTML-formatted response is edited into the stream message (or sent as a new message if too long/has thinking)

**Key design decisions:**
- **HTML formatting during streaming:** Each stream update runs through `closePartialMarkdown` (strips unmatched `**`, `` ` ``, `` ``` ``, `~~`, `__`, `*`, `_`) then `ConvertToTelegramHTML` with `ParseMode: "HTML"`. If the HTML edit fails (malformed output), the stream writer falls back to plain text for that tick.
- **Partial markdown handling:** `closePartialMarkdown` detects unmatched delimiters by parity counting and strips the trailing unmatched instance. For code fences, everything from the unmatched fence onward is removed. This is lightweight (string counting, no regex) and runs on every tick.
- **Truncation at 3900 chars:** Buffer is truncated with `"..."` to stay within Telegram's 4096-char limit (with headroom for HTML tag expansion). Truncation is rune-safe to avoid splitting multi-byte UTF-8 characters. The final response uses the normal chunking path if it exceeds 4096.
- **Lazy start:** No goroutine or message until the first delta. If the agent returns no text (e.g. pure tool calls), the stream writer does nothing.
- **Silencing-prefix gate:** Before the first delta triggers `sendInitial`, the accumulated buffer is checked against `platform.IsSilencingPrefix`. While the buffer is empty/whitespace or could still resolve to a silencing sentinel (`[[NO_RESPONSE]]`, `"No response requested."`), no Telegram message is created. Once the buffer diverges from every sentinel, the gate releases, `sendInitial` fires with the held content, and normal streaming resumes — subsequent deltas are not re-checked. If the stream ends while still in the prefix-ambiguous window (whole turn is `[[NO_RESPONSE]]`), no message is ever created. This is the only way to prevent a streamed message from briefly appearing on screen before being silenced; `IsSilent` at downstream chokepoints (see below) prevents *new* delivery but cannot un-send a message that was incrementally streamed.
- **Stream message as edit target:** When a stream message exists, the final response is edited into it (taking priority over tool call preview messages). If the response can't be edited in-place (too long, has thinking blocks), the stream message is edited to a truncated preview with "(full response below)" and the full response is sent as a new message.

**Config:** `stream_output` (bool) and `stream_update_interval` (string, default `"250ms"`) in `[display]` or `[[platforms]]`, or `stream_output` and `stream_interval` in `[[agents.platforms]]`.

## Discord Bot (`discord/`)

Same architecture as Telegram (receiver + agentMessagePump + commandWorker + per-session agent workers), connected via a single WebSocket gateway instead of HTTP long-polling. Uses the same thin `platform.MessageQueue` filter-and-throttle helper. Commands drain `mq.CmdChan()` before pulling the main channel, preserving the original priority-drain behaviour.

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

## App Provider — FAP WebSocket (`internal/app/`)

Server side of the **Foci App Protocol (FAP v1)** the native Android client
speaks (`github.com/richardtkemp/foci-android`). A `platform.MessagingProvider`
like telegram/discord, but a `Connection` is the server end of one device's
WebSocket rather than a vendor-API client. Built: slice 1 ("echo": text + native
streaming + status `meta`), slice 2 (interactive buttons → permission/ask/plan),
slice 3 (reliability: per-conversation seq/ack/replay + reconnect resume + inbound
dedup), slice 4 (media/blobs over HTTP), slice 5 (FCM offline wake-push), slice 6
(multi-agent/session: client- or server-assigned conversationId, roster, conversation.open,
named sessions, slash commands), slice 7 (auth hardening: pairing + per-device
tokens + revocation + rate-limited auth), slice 8 (voice: inbound STT
transcription). The full §11 build order is implemented
(`foci-android/docs/02-foci-server-changes.md`).

**Wire layer (`internal/app/fap/`):** pure Go mirror of the client's Kotlin
`:protocol` module. `Envelope{t,id,seq,ack,ts,v,d}` wraps a type-specific
payload selected by `t`. `fap.Encode(ServerFrame, seq, ack, id, ts)` →
wire string; `fap.Decode(text)` → `Inbound{…, Frame}` (a concrete `Client*`
value, or nil for an unknown `t` — forward-compat). Field names are the
contract and MUST stay byte-compatible with the Kotlin types. Includes a
dependency-free Crockford `NewULID()`.

**Registration:** `init()` in `provider.go` calls
`platform.RegisterMessagingProvider("app", …)` + `agent.RegisterPlatformTrigger`.
`IsConfigured` = an `[[platforms]] id="app"` entry exists. `Init` resolves the
shared key (secret `app.api_key`), builds the `Hub`, wraps it as a
`ConnectionManager` via the generic `NewConnectionManagerAdapter[*appConn]`, and
publishes it to the HTTP layer via `setActiveHub`.

**HTTP endpoint:** `cmd/foci-gw/http.go registerHTTPHandlers` mounts
`/app/ws` when `app.Enabled()` (hub present + key set). `app.WSHandler()` →
`Hub.ServeWS`, which does its OWN Bearer auth (constant-time vs `app.api_key`,
no shared middleware) then upgrades via gorilla/websocket. Exposed publicly via
Traefik TLS; foci's bind stays localhost.

**Panic isolation (`safe.go`):** the app provider runs in the same process as
telegram/discord, so a panic must not crash the gateway (in Go an unrecovered
panic in ANY goroutine kills the process). `recoverApp(where)` is a deferred
recover-and-log helper; `safeGo(where, fn)` wraps it around a goroutine. All
four app goroutines launch via `safeGo` (blob reaper, ws-writepump, fcm-push,
dispatch-command); `withHub` (endpoint.go) `defer recoverApp`s so all HTTP
routes + the synchronous `readPump`/dispatch are covered. **First-run safety:**
`Init` recovers any panic in `newHub` and returns nil, leaving the provider
inert (nil hub → `Enabled()` false → endpoints 503, `disabledConnMgr` as a
no-op `ConnectionManager`) — so a bug in this new subsystem degrades the app
feature instead of aborting startup of the whole gateway (`InitMessaging` treats
a returned `Init` error as fatal). The decode path itself is panic-safe by
construction (JSON `Unmarshal`, no raw-byte indexing, mutex-guarded maps).

**Hub (`hub.go`)** owns: per-agent `appConn` registry, `convs` (durable
per-conversation state keyed by conversationId), `bySession` binding map, live
sockets, and `prompts` (live interactive promptId→binding). Implements
`ConnectionSource[*appConn]`. Each socket runs a `readPump` (dispatch) +
`writePump` (buffered `send` chan + ping ticker, pongWait dead-socket detection).
A FAP **conversationId** ↔ a foci **session key**: `chatIDForConv` FNV-hashes the
conversationId to a stable int64 chatID, `sessionKeyForChat` resolves+persists
the session key via `SessionIndex` chat-meta (key `"session"`, platform `"app"`)
so history survives reconnects.

**Durable conversation state (`convBinding`, slice 3):** the wire scopes seq per
conversation, not per socket — so `convBinding` is keyed by conversationId in
`convs` and OUTLIVES sockets. It holds the outbound `seq` high-water, the
`clientSeqHW` (stamped into outbound `ack`), a replay `buffer` (depth+TTL
trimmed; trimmed further on inbound ack), and an inbound dedup `seen` set.
`client` is the currently-attached socket (nil = offline; sends still buffer).
`attach`/`detachIf` move it between the sockets a phone churns through;
`removeClient` detaches but RETAINS state for reconnect.

**Inbound (`dispatch.go`):** decode → **reliability gate** (`inboundConvID` →
`convForReliability`: dedup by `(conversationId, envelope id)`, drop resent
outbox entries, fold piggybacked `ack` to trim the replay buffer) → switch.
`hello`→server hello (roster) + `resumeConversations` (re-attach + replay each
resume point's `seq > ack`); `conversation.open`→bind socket to agent;
`message`→`routeUserTurn`→`ensureBinding`→`agent.Enqueue(Envelope{Driver:
appConn})`; `conversation.open`→`handleConversationOpen` (adopts a client-assigned
`conversationId` when the frame carries one — so the app creates + opens the
conversation locally and instantly — else mints one; binds it, optionally adopts
a named `sessionKey`, replies with an updated roster the app upserts; idempotent
via `ensureBinding`, so a reopen of an id the first message already created
reuses it); `conversation.rename`→`handleConversationRename`
(persists a user-friendly session alias in the session index's `chat_metadata`,
keyed by the stable app `chatID` so it survives session-key rotation/restart;
`agentRoster` surfaces it via `aliasFor` as `ConversationInfo.Title`, replacing the
raw conversationId the app would otherwise show); `command`→`routeCommand`→ the agent's
`command.Registry.Dispatch` (captured from `AgentConnectionParams` in
`setupAgent`), response parts sent back as `message` frames. Inbound `voice`
attachments are transcribed by the agent's `voice.STT` (`transcribeVoice`, also
captured in `setupAgent`) and merged into the turn text before `Enqueue`;
`hello.caps.features` advertises `["voice"]` when a transcriber is present.
`interactive.response`→`handleInteractiveResponse`
(`platform.HandleInteractiveCallback` on the echoed `<promptId>:<index>` data →
`interactive.edit` resolution, suppressed when a follow-up question advanced the
binding's seq); `ping`→`pong`; unknown→ignored. No agent → `error` frame.

**Platform lifecycle callbacks:** `SetLifecycleCallback` stores the gateway's
`OnUserMessage`/`OnTurnComplete`/`OnTurnEnd` hooks on the per-agent `appConn`
(`PrimaryBot`), mirroring telegram's `Bot` fields. `OnUserMessage` fires from
`routeUserTurn` (right before `agent.Enqueue`) and `routeCommand` — it is the
**only** signal the periodic runner's `lastInteraction` receives on this
transport, so reflection / consolidation / the reset idle-guard all depend on
it. `OnTurnComplete`/`OnTurnEnd` fire from `appConn.WrapTurn` (complete after
the turn body returns, end deferred last) — same shape as `telegram.Bot.WrapTurn`.

**Outbound — `appConn` (`conn.go`)** implements `platform.Connection`,
`platform.ButtonSender`, and `agent.Driver`. `SendToSession`/`SendText`→`message`;
`SetTyping`→`typing`; `SendNotification`→`notification`;
`UpdateChatSessionKey`→remap binding + `session.update`. `SendTextWithButtons`→
`interactive` (foci pre-encodes each button's Data as `<promptId>:<index>`, so the
app echoes it back for routing); `EditMessageText`/`EditMessageWithButtons`→
`interactive.edit` (addressed via the hub `prompts` map). The `Send{Photo,Document,
Voice,…}` media methods store the payload in the `blobStore` and emit a `media`
frame referencing the blobId. All sends go through `convBinding.send`, which
assigns seq + ack, buffers for replay, and enqueues iff a socket is attached.

**Binding restore across restart + archive (`framestore.go`, `StartAll`, `handleConversationArchive`):** bindings (`h.convs`/`h.bySession`) are in-memory, created only on client frames — so a foci restart empties them and an unsolicited send (`SendToSession`) would `return nil` (drop) until the app reconnects. To close that window, `Hub.StartAll` rebuilds bindings at startup from the durable store: `frameStore.RestorableConvs()` returns every conv with a **visible** frame and a known `agent_id` (a column added to `app_frames`; written by `convBinding.send`), and `ensureBinding(nil, agentID, convID)` recreates each socketless binding (`attach(nil)` is a no-op; seq seeded from `MaxSeq`). **Archive is a reversible flag, not a deletion:** the `conversation.archive` frame carries an `Archived` bool; `handleConversationArchive` persists only an `is_archived` row in `chat_metadata` (keyed by agent+platform+chatID, a sibling of `is_default`) — it does NOT purge frames, drop the binding, flip session status, or fire a reflection. The binding stays live (inbound frames still flow; history retained), and the roster surfaces `ConversationInfo.Archived` (read from `SessionIndex.ArchivedChatsForAgent` by `agentRoster`). Archived convs are therefore still in `RestorableConvs` and get their bindings rebuilt on restart. Unarchive is a real server action (`Archived=false` clears the flag); the updated roster is pushed back to the socket on every archive/unarchive so all devices reconcile.

**Media / blobs (`blob.go`, slice 4):** binary payloads never cross the
WebSocket. `blobStore` keeps blobs on disk under `tempdir.Dir()/app-blobs`
(metadata in memory; size-capped + TTL-reaped). Outbound: a `Send*` media call
→ `putFile`/`putBytes` → `media {blobId,mime,…}` (no `kind` — app clients derive
presentation from `mime`; kind is an internal blob/Telegram-method label only); the app fetches bytes via
`GET /app/blob/<id>` (`ServeBlobGet`, range-capable `http.ServeContent`).
Inbound: the app uploads via `POST /app/blob` (`ServeBlobPost`, returns
`{blobId,size,mime}`), then references the blobId in `message.attachments`;
`resolveAttachments` reads each blob back into a `platform.Attachment`
(small ones into `Data`, `SavedPath` always set). Both endpoints share the
`bearerToken` + `app.api_key` gate; registered in `http.go` alongside `/app/ws`.

**Agent avatars (`avatar.go`):** each agent may have an avatar image, served to
the app at `GET /app/avatar/<agentId>` (`ServeAvatar`, same Bearer gate as blobs,
range-capable `http.ServeContent`, Content-Type from extension). Unlike blobs the
file is persistent (keyed by agent ID, no TTL/reaper). The path comes from
`AgentConfig.Avatar` (toml `avatar`): a configured absolute/foci-home-relative path
(`ResolvePath`), else auto-detected at load (`config.detectAvatar`) from
`$workspace/avatar.{png,jpg,jpeg,webp,gif}` then `$workspace/.data/avatar.{ext}`.
The `hello` roster (`agentRoster` → `fap.AgentInfo`) advertises `avatarUrl`
(`/app/avatar/<id>`) + `avatarVer` (a mtime+size fingerprint, drives client cache
invalidation) when the file exists; `avatar` still carries the emoji fallback.
Each `AgentInfo` also carries `commands` (`[]fap.CommandInfo`: `name`,
`description`, `category`) — the agent's slash-command palette, built by
`commandInfos(conn)` from the connection's `command.Registry.All()`, skipping
`Hidden` commands. This mirrors the Telegram `setMyCommands` menu
(`bot_poll.go:RegisterCommands`): the server is authoritative for the
descriptions, so the app renders what it receives rather than hardcoding its
own copies. Dynamic `Visible` gating is intentionally not evaluated (no
per-session request context at roster-build time) — the full non-hidden set is
advertised and each command no-ops when invoked out of context, exactly as on
Telegram. The list rides the existing `hello`/`conversation.open` roster
frames, so it refreshes whenever the roster does; no dedicated frame type.

**Auth hardening (`devices.go`, slice 7):** the shared master key (`app.api_key`)
remains the bootstrap, but a device pairs once (`POST /app/pair`, master-key only)
to mint a revocable per-device token (`deviceStore`, 256-bit random, persisted to
`<DataDir>/app-devices.json` so pairings survive deploys). Thereafter `/app/ws`
and `/app/blob` accept the master key OR a device token (`Hub.authenticate` →
`authToken`; device-token auth seeds `client.deviceID`). `POST /app/pair/revoke`
(master-key) drops the token and force-closes the device's live socket(s) with
`4403`; `GET /app/devices` lists pairings (tokens omitted). An `authLimiter` locks
out a remote IP (`remoteIP`, X-Forwarded-For-aware) after repeated auth failures
— the endpoint is internet-facing.

**Push (`push.go`, slice 5):** offline wake via FCM v1 data-messages. `fcmPusher`
authenticates with a service-account token source (`golang.org/x/oauth2/google`,
auto-refreshing); the service-account JSON path comes from
`[platforms.app].fcm_credentials` config, falling back to secret
`app.fcm_credentials` (absent or `push=false` → push disabled). The client
registers its FCM token in `ClientHello` (or out-of-band via
`POST /app/push/register` after an OS token rotation)
→ `pushTokens` (in-memory deviceId→token, repopulated each connect). When
`convBinding.send` runs with no attached socket, it buffers the frame and — for
user-visible frames only (`pushPreview` classifies; control/streaming frames are
skipped) — fires `notifyOffline` → `pusher.notify`, which coalesces (≤1 push per
conversation per `push_coalesce` window, default 15s) and sends a hint
(`conversationId` + short preview, never full text). The app wakes, reconnects,
and replays for content. `hello.caps.push` advertises `["fcm"]` when enabled.

**Streaming (`sink.go` + `render.go`):** `appConn.NewTurnSink` builds an
`appSink` (`turnevent.Sink`) per turn, bound to the conversation. The app
**reuses the shared delivery coordination** rather than re-implementing it:
`appSink` is a thin wrapper around `turn.StreamingSink` + `TurnRenderer` (the
same machinery Telegram/Discord use), driving an `appBackend` (`turn.Platform`)
+ `appStreamSink` (`turn.StreamSink`) defined in `render.go`. The renderer owns
the *coordination* — the `delivered`-flag dedup of streamed deltas vs the
intermediate `TextBlock` vs the final text, and `resetStream()` per reply
segment (so a multi-reply turn renders as distinct `turnId` bubbles, not one
appended bubble). Only the *output shape* is app-specific: where Telegram edits
a message with a full snapshot, `appStreamSink.Update` diffs the snapshot and
emits the new suffix as `text.delta`; `appBackend.Deliver` finalizes each
segment with `text.end` (streamed) or a fresh `message` (non-streamed). A
no-op `SinkTracker` (no tool-preview/retry surface) and `ShowThinking="off"`
keep those event paths inert. `appSink` layers on only what has no home in the
renderer: the `typing on`/`off` frames bracketing the turn boundary, dropping
`SubagentText` (no app surface yet — forwarding would prematurely finalize the
in-flight stream), and the structured `meta` status frame on `TurnComplete`
(model/cost/tokens from the turn usage; mana%/state + gap via `Agent.MetaStatus`,
threaded as the sink's `statusFn`). Deltas are batched by the stream pump
(`appStreamInterval`, 50ms) — beneficial: it keeps a fast token stream from
flooding the socket. Per-conversation outbound `seq` is stamped, buffered, and
replayed on reconnect (slice 3).

> **Why a separate sink, not just "different output functions"?** The
> `turnevent.Sink` seam *is* the reuse boundary, and both the app and the
> platforms sit on it. Below it, the renderer's *coordination* is shared
> (above), but its *streaming model* is not: Telegram is edit-a-message
> (snapshot `Update`, rate-limited pump, char-cap rollover, markdown), the app
> is append-a-delta + structured frames. The app implements `turn.Platform`/
> `turn.StreamSink` to express that output shape while reusing everything above
> it. The earlier hand-rolled `appSink` forked the *coordination* too, and got
> it wrong — streamed deltas and the intermediate `TextBlock` were delivered
> independently (double delivery; both replies sharing one `turnId`).

**Config + reconnect/restart wiring (gap-closure):** the typed
`[platforms.app]` config subsection (`config.AppSpecific`: `host`, `push`,
`replay_buffer`, `replay_ttl`, `max_blob_mb`, `blob_ttl`, `push_coalesce`,
`fcm_credentials`, `devices_path`, `allowed_devices`) is read in `newHub` and
threaded to the blob store, each `convBinding` (replay depth/TTL), the pusher
window, and `hello.caps.host`. `allowed_devices` (when set) gates `ServePair`.
The `/app/ws` upgrade now negotiates the `fap.v1` subprotocol. On reconnect,
`evictOtherDeviceSockets` closes any older socket for the same deviceID with
`4409` (exactly-once render). `GET /app/history?conversationId=` returns the
server-side `seq` high-water (0 after a restart drops the in-memory buffers) so
the offline-first app reconciles against its local Room DB. `conversation.open`
with a known `sessionKey` reuses the existing conversation instead of minting a
duplicate; with a client-assigned `conversationId` it adopts that id (the app
creates + opens the conversation locally and instantly, and any message sent
before the server confirms carries the same id and auto-creates the binding). Interactive prompts carry a 24h advisory `expiresAt`; the roster
advertises each agent's config display name + emoji avatar.

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
- `POST /command` — dispatches slash command (bypasses agent context; activity-gated — accepts `if_active`/`if_inactive`/`if_user_active`/`if_user_inactive` in the JSON body, with the same in-flight short-circuit as `/send`, so an unattended `/reset` skips a session that is active or mid-turn)
- `POST /wake` — branch from default session (activity-gated, supports `no_compact`/`no_reset_hook`). Returns 412 if no default session.
- `POST /webhook/{agent}/{hookid}` — trigger agent turn from external events. `{hookid}` must be declared in the agent's `webhooks` config map (global `[system]` merged with per-agent `[[agents]].system`). The mapped prompt path is resolved via `prompts.ResolvePrompt()` (agent workspace/prompts → shared workspace/prompts). Reads request body as payload (max 1 MB), combines prompt + payload under a `## Webhook Payload` heading, and sends to the agent's default session. Async (202) by default; `?sync=true` for synchronous response. Supports four activity gate query params — `?if_active` / `?if_inactive` (session-level, with in-flight short-circuit) and `?if_user_active` / `?if_user_inactive` (user-attention only); see [SPEC.md](SPEC.md) Activity gating. Returns 404 if hookid not in config or prompt file not found, 412 if no default session.
- `GET /voice` — WebSocket upgrade for real-time voice conversation. Enabled when `[http] ws_enabled = true`.
- `POST /-/reload-credentials` — hot-reload API credentials from `secrets.toml`. Called by `foci auth` after saving a new token. Only registered when using static token auth (setup-token or API key), not OAuth fallback.

## CLI Tool (`cmd/foci/`)

Separate binary (`go build ./cmd/foci`) that wraps the HTTP gateway endpoints for scripts and cron jobs. Auto-discovers the gateway Unix socket at `~/data/foci-gw.sock` (`FOCI_GW_SOCK` env var or `--socket` flag) for same-user auth with no API key. Falls back to TCP + `FOCI_API_KEY` for remote/cross-user access. See [docs/CLI.md](CLI.md) for the full command reference, flags, environment variables, and cron integration examples.

**`foci first-run`** — first-run setup wizard. Generic steps (auth, agent ID, model, character files) live in `cmd/foci/setup.go`. Platform-specific steps (e.g. bot token, user ID) are delegated to providers via the `platform.SetupWizard` interface. Each provider returns a `WizardResult` containing a TOML config fragment and secrets map. The generic wizard appends these to the generated `foci.toml` and stores secrets via `secrets.Store`. `cmd/foci/setup.go` has zero direct telegram imports — it blank-imports `internal/telegram` for provider registration and discovers wizards via `platform.SetupProviders()`. Non-interactive mode collects provider flags dynamically from `SetupFlags()`. The `consoleUI` struct implements `platform.SetupUI` for interactive prompts.

## Wake

- **HTTP Wake** (`POST /wake`): Creates a branch session from the agent's default chat session, injects the text, runs the agent on the branch. Supports `no_compact` and `no_reset_hook` flags. `--oneshot` CLI flag sets both. Returns 412 if no default session.
- **Scheduled Wakes** (`remind` tool with `wake=true`): Agent-initiated timer that fires message injection into the default session at specified delay or timestamp. One-shot, background goroutine, auto-cleaned after firing. Skips if no default session.

## Session-End Reflection

Before a session is cleared (`/reset` or facet TTL reclaim), the agent runs the reflection pass asynchronously. Configured via `[reflection]` section (replaces `session_reset_prompt`).

Flow (`agent.FireSessionEndMemory` in `internal/agent/session_end_memory.go`):
1. Check `reflection.session_end_enabled` (nil = true, explicit false skips)
2. **Reflect-twice guard** — `SessionIndex.ReflectionRedundant(sessionKey)`: skip if a reflection has already run AND nothing substantive happened since (`last_activity_at <= last_reflection`). Unknown / never-reflected sessions reflect. Relies on activity tracking excluding memory turns (below).
3. Resolve prompt via `prompts.ResolvePrompt(session_end_prompt, ...)` — embedded default on empty/error
4. If prompt resolves to empty, skip
5. For branch sessions, check `BranchMeta.NoResetHook` — if true, skip (unless skipMetaCheck=true for background branches)
6. Create branch from expiring session (copies conversation history)
7. Return immediately — caller proceeds to clear the main session
8. Async: `HandleMessage(ctx, branchKey, prompt)` with 120s timeout, trigger `"session_end_memory"`, NoCompact

**Activity tracking excludes memory turns.** `last_activity_at` is bumped by `RegisterSessionIndex` / `TouchActivity` (`turn_contract.go`) on every turn *except* those whose trigger is a memory-formation pass — `isMemoryTrigger` returns true for `"reflection"` and `"session_end_memory"` (`internal/agent/context.go`). Without this, a delegated agent's reflection (which injects into the *main* session, not a branch) would bump `last_activity_at` past `last_reflection` and make the reflect-twice guard always fire reflection. Keepalive / background / cron turns still count as activity by design — only the memory passes themselves are excluded.

Entry points:
- `/reset` command → `agent.FireSessionEndMemory` (async) → `RotateKey` → `Reload`
- `Pool.Acquire` (TTL reclaim) → `ReclaimHook` → `agent.FireSessionEndMemory` (async) → clear session key
- Periodic runner (background branch completion) → `agent.FireSessionEndMemory` (async, skipMetaCheck=true)

## Reflection & Consolidation Timers

Reflection and consolidation run in the keepalive timer loop (30s ticks):

**Interval reflection** (`maybeReflection`):
1. Check `interval_enabled` (nil = true)
2. Check wall-clock interval elapsed and user not idle (`sinceLastInteraction` must be ≤ interval; `lastInteraction` is fed by the platform `OnUserMessage` lifecycle callback — wired on telegram, discord, and app providers. A transport that doesn't fire it leaves `lastInteraction` frozen at boot, so this gate skips forever for that agent.)
3. Query `session_index` for active chat sessions with `last_activity_at > last_reflection` (per-session tracking)
4. Resolve prompt via `prompts.ResolvePrompt`
5. Iterate all matching sessions: `branchFn("reflection", sessionKey, promptText, true)` for each
6. On success per session: stamp `last_reflection` at branch creation time

Reflection runs before consolidation so the latest memory content is available. Consolidation is blocked while reflection is running.

**Consolidation** (`maybeConsolidation`) — config now under `[maintenance]` (`r.maintCfg`):
1. Check `consolidation_enabled` (nil = true)
2. Compute next-fire via `parseSchedule(consolidation_time).nextFire(...)` — `consolidation_time` is `"HH:MM"` daily (process tz) or a Go duration; persisted last-run in state store
3. Check recent user activity (within 1h)
4. Check reflection / reset is not running
5. Resolve prompt via `prompts.ResolvePrompt`
6. Fire branch on default session: `branchFn("consolidation", parentKey, promptText, true)`
7. On completion: persist timestamp to state store

**Scheduled reset** (`maybeReset`) — `[maintenance].reset_time` (default off):
1. Skip if `resetFn` nil or `reset_time` empty
2. Compute next-fire via `parseSchedule(reset_time).nextFire(...)` (same dual format as consolidation; `lastReset` anchored to boot, persisted as `reset_last`)
3. Skip if reflection/consolidation/reset already running
4. Inactivity guard: skip if user active within `reset_idle_guard` (default `"55m"`) — mirrors the `foci command --if-inactive` crontab it replaces
5. Skip if no default session or a turn is in flight on it
6. Fire `resetFn(ctx, parentKey)` (→ `Agent.ResetSession`: memory formation + key rotation, the same path as a manual `/reset`) in a goroutine; persist `reset_last` on completion

The shared schedule parser lives in `internal/periodic/schedule.go` (`parseSchedule` / `schedule.nextFire`): a daemon asleep past a clock time fires once on wake (catch-up), never once per missed day; clock times are rebuilt via `time.Date` so they stay stable across DST.

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

**Delegated (CC) compaction:** for delegated agents, compaction runs inside CC (`runDelegatedCompact` sends `/compact`), not via this API pipeline. After a successful CC compaction, foci conditionally bounces the CC session so character/skill edits reload, then self-injects a resume nudge — see [Compaction reload-bounce + resume nudge](#backend-session-lifecycle). The per-agent `reload_on_compact` config (`CompactionConfig`, default ON, overridable at agent or global level) gates the bounce.


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
6. Also re-runs after compaction (character files may have changed)

### Trigger Types

- **`every_n_tools(N)`** — fires every N individual tool calls during a turn (via `CheckAfterTools`)
- **`every_n_turns(N)`** — fires every N user turns; lifetime counter, never reset (via `CheckTurnInterval`, used by default nudges)
- **`after_error`** — fires when the last tool call returned an error (via `CheckAfterTools`)
- **`regex(pattern)`** — regex evaluated once against user message at `StartTurn()`; fires via `CheckAfterTools` on the tools path, or via `CheckRegex()` on the no-tools path (ensures regex triggers fire even when the model answers directly)
- **`pre_answer`** — all pre_answer rules concatenated and injected when the model wants to end the turn (gated by `NudgePreAnswerGate` and `NudgePreAnswerMinTools`)

### Injection

**API transport** (`turn_api.go`): nudge reminders are injected as text ContentBlocks in user messages. After-tools nudges (every_n_tools, after_error, regex) are appended as individual blocks to tool result messages. Regex nudges on no-tools turns and every_n_turns nudges are prepended as ContentBlocks to the user message before the first API call. Pre_answer nudges are injected as standalone user messages that continue the loop. Each injection is one-shot per trigger type per turn to prevent infinite loops.

**Delegated transport** (`turn_delegated.go`): CC owns the inference loop so foci can't edit in-flight messages. Instead:

- **every_n_turns / regex** — prepended to the prompt string in `InjectNudges` before the agent layer's `Inject(SourceUser)` call, same as API content blocks but flattened to text.
- **every_n_tools / after_error** — wired through `delegator.TurnEvents.PostToolNudgeFunc`. ccstream's `handleHookResponse` invokes this callback after each `OnToolEnd` dispatch (once per PostToolUse hook event), and sends any returned reminders to CC as plain `[user] <text>` user messages via `writer.SendUser` at default queue priority. CC's mid-turn drain (`claude-code/src/query.ts:1570-1589`) folds the message into the current `ask()` as an attachment to the next tool-result batch, so the model addresses the nudge in the same turn and its response reaches the user through the always-live `SessionEvents.OnText` path. There is no separate ask/result cycle for the nudge.
- **pre_answer** — wired through `delegator.TurnEvents.PreAnswerNudgeFunc`. On `OnResult`, ccstream gives the bookkeeping callback a chance to return a verification follow-up. When non-empty, ccstream re-runs `beginTurn` with the same `TurnEvents`, sends the follow-up via `writer.SendUser`, and skips `OnTurnComplete` until the second round's `OnResult`. `turn_delegated.go` tracks `preAnswerFired` in a closure local so the gate fires at most once per user turn, stashes round-1 usage/text so the final `OnTurnComplete` can fold usage into `ts.FinalUsage`, and restores the original answer when round 2 echoes `NoResponseSentinel`. Unlike the API path, the round-1 answer has already streamed to the user as intermediate text via `SessionEvents.OnText` — round 2's text becomes the authoritative final reply.

### Trigger Gate (`nudgesAllowed`, #815)

All four nudge paths are gated on `nudgesAllowed(ts) == isUserTrigger(ts.Trigger)` (`context.go`). Nudges shape the agent's user-facing reply, so they fire **only on user-triggered turns**. System-internal turns — `reflection`, `keepalive`, `consolidation`, `session_end_memory` (trigger set in `cmd/foci-gw/agent_sessions.go` via `WithTrigger(ctx, branchType)`) — are exempt at every site:

- `InjectNudges` (both transports) returns *before* `Scheduler.StartTurn`, so system turns also do not advance the `every_n_turns` lifetime counter — the cadence tracks user turns only.
- `PostToolNudgeFunc` / API after-tools loop, and `PreAnswerNudgeFunc` / API pre-answer gate, each short-circuit on a non-user trigger.

Without this gate, the pre-answer gate fired "verify before answering" on reflection turns that wrote memory files (observed 2026-06-05; `ts.Trigger` is populated at `turn_orchestrator.go` before any nudge site runs).

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
