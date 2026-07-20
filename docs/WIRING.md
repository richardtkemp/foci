# Foci — Wiring Diagram

How the pieces connect. Read this before touching the code.

## Startup Flow (`main.go`)

Each phase is extracted into its own file. `main()` has grown to ~670 lines (still one function, but each phase below is a one/two-line call into its own file).

```
config.Load(path)                                        ← validates values; logs to stderr + buffer
                                                         ← merges [[modelinfo]] entries into modelinfo.registry

→ timeutil.SetLocation(tz)                               ← [timezone], before anything logs/init's
→ shellenv.Apply(cfg.ShellEnvFile)                       ← internal/shellenv; loads the operator's shell rc/env
                                                            file into this process before any backend spawn, so
                                                            tool shells (non-interactive, non-login — only
                                                            $BASH_ENV is sourced) inherit PATH/GOPATH etc.
→ preload.Apply()                                        ← internal/preload; sets LD_PRELOAD to ~/.lib/nosgid.so
                                                            (same inherit-via-os.Environ() mechanism) so shell
                                                            tools + backends silently drop setgid chmod bits
                                                            instead of hitting EPERM under RestrictSUIDSGID=yes.
                                                            No-op if the shim isn't installed.

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

→ warnMissingSecrets(cfg, sec.store) / warnStreamOutputWithoutStreaming(cfg)   ← warn_secrets.go / warn_streaming.go; startup config-sanity WARNs, non-fatal

→ initVoice(cfg, sec.store)                              ← voice_init.go; builds the STT/TTS provider maps shared by voice WS + send_to_chat TTS

→ gwLiveApply = newLiveApply(configPath)                 ← liveapply.go; hot-config-reload registry (created early, appliers registered later via registerLiveAppliers after agent setup — see below)

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
           → wires CacheBustAlert, RateLimitFunc, etc. using plat.NotifyAgent()
  → agent.RestoreSessionOverrides(defaultSessionKey())   ← restore per-session effort/thinking/model from state store (main.go, after setupAgent)
  → agent.SeedSessionMeta(defaultSessionKey())           ← seed gap from session history (correct gap after restart)

  → modelcaps wiring (#840)                               ← main.go, once after the agent loop
    → for backend in {ccstream, api, codex}:
      → modelcaps.SetPersister + Restore                  ← seed state.db snapshot synchronously
    → anthropicResolver.ModelCapsFetcher(15s)             ← /v1/models fetcher (nil if CC OAuth creds absent)
    → for backend in {ccstream, api}:
      → modelcaps.SetFetcher(backend, fetcher)
      → go modelcaps.Refresh(ctx, backend)                ← background; on error, serve-stale / static modelinfo fallback
    → each Codex app-server Start → model/list → modelcaps.Publish(codex)
  → setupPeriodic(inst, acfg, periodicParams{...})        ← periodic_setup.go (per-agent; renamed from setupKeepalive/keepalive_setup.go — the runner now covers keepalive+background+reflection+consolidation+reset, see Reflection & Consolidation Timers)
  → plat.SetupSharedFacet(...)                         ← shared facet bots (via messaging facade)
  → setupWarningHooks(agents, cfg)                         ← post_agent_setup.go
  → setupTmuxMemoryMonitor(...)                            ← post_agent_setup.go
  → setupMemoryGuard(...)                                  ← post_agent_setup.go
  → registerLiveAppliers(gwLiveApply, agents)              ← liveapply.go; wires each agent's hot-reloadable config fields (`hot:"turn"` etc.) into the registry created earlier

  → signal.Notify(SIGINT, SIGTERM)
  → plat.RestoreFacetSessions(...)                     ← restore bot→session mappings from state store
  → plat.StartAll(ctx)                                     ← starts all provider connections
  → startup notifications (inline in main.go)              ← uses connMgr.AllForAgent() for fan-out
  → deferStore = defersend.NewStore(deferred-sends.db)     ← wait_defer.go; SQLite-backed queue for `foci send --wait-*` sends whose activity gate isn't yet satisfied, swept by a background goroutine (10s tick, 2h default send-anyway timeout) — see internal/defersend below
  → http.Server{...}                                       ← http.go (registerHTTPHandlers)
  → startUnixSocket(...)                                   ← unix_socket.go (same-user auth, no API key)
  → setupAskgw(cfg, agents, connMgr)                       ← askgw_setup.go (opt-in [askgw] enabled=true; NDJSON Unix socket for external Apps to ask humans questions via foci's interactive button surface)
  → setupTestharnessControl(ctx, agents)                   ← testharness_control.go; test-only control surface for the L2 integration harness (internal/testharness), no-op in production
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
 ├── log           → sqlite, modelinfo, timeutil (the first two only for API-call usage logging; conversation storage was extracted to convo)
 ├── convo         → log, sqlite, timeutil (per-agent conversation SQLite store + memory-index Hook; extracted from log so log stays lean)
 ├── display       (no deps — table rendering with Unicode display-width handling)
 ├── secrets       → BurntSushi/toml
 │   └── secrets/bitwarden → log
 ├── provider      (no deps — provider-neutral types and Client interface)
 ├── platform      → config, log, secrets, session, voice, warnings
 │                  (messaging types, interfaces, provider registry, Messaging facade,
 │                   MessageQueue thin filter+throttle helper + GroupThrottle for group chat batching)
 ├── anthropic     → provider, github.com/anthropics/anthropic-sdk-go
 ├── gemini        → provider, google.golang.org/genai
 ├── openai        → provider, github.com/openai/openai-go/v3
 ├── session       → provider, log, messages, sqlite, timeutil
 ├── memory        → sqlite, fsnotify, blevesearch/bleve/v2 (FTS5 + bleve backends)
 ├── voice         → config, log, procx, session, tempdir, gorilla/websocket
 ├── skills        → log (leaf package)
 ├── startup       → log, session (leaf package for crash detection)
 ├── resources     → log (goroutine monitor, memory guard)
 ├── procx         (no internal deps — process-spawn helper: strips foci-secrets/foci-askgw supplementary groups from child processes, own process group; used by every subprocess spawn site)
 ├── peercred      (stdlib syscall only — SO_PEERCRED extraction for Unix-socket auth; see HTTP Gateway)
 ├── question      (no internal deps — backend-agnostic AskUserQuestion core: parsing, formatting, choice buttons, answer resolution/merge; shared by ccstream and tools so the two surfaces can't drift)
 ├── defersend     → sqlite, timeutil (leaf — SQLite-backed queue for `foci send --wait-*` deferred sends; a pending send that isn't yet warm/cold/user-active/-inactive is persisted and delivered by a background sweep, surviving a restart. Wired in `cmd/foci-gw/wait_defer.go`.)
 ├── mcp           → log, procx, provider, tools, BurntSushi/toml, go-sdk/mcp
 ├── tools         → agent/turnevent, app/fap, config, convo, display, log, memory, modelinfo, peercred, platform, procx, provider, question, secrets, secrets/bitwarden, session, tempdir, tools/spill, voice (Registry, Tool, shared helpers, the exec-bridge generator, web, http, and most tool impls)
 │     ├── tools/spill    → (stdlib only) shared spill-to-disk writer: bounded in-RAM head + overflow to temp file, optional total cap; used by tools/shell and the http tool
 │     ├── tools/shell    → tools, tools/spill, log, procx, secrets, secrets/bitwarden (the exec/shell tool; execbridge generator stays at root)
 │     ├── tools/tmux     → tools, log, display, session, procx (tmux session tool — 8 files)
 │     ├── tools/browser  → tools, log, tools/browserjs (browser automation — imports root for Tool/ToolResult)
 │     └── tools/browserjs (no foci deps — vendored go-rod JS snippets)
 ├── workspace     → log, provider
 ├── nudge         → log (leaf — rule extraction, scheduling, file I/O)
 ├── prompts       (top-level package, not internal — lives at `shared/prompts/`) → log (embedded .md files + ResolveOrientationTemplate helpers)
 ├── modelinfo     (no deps — stdlib-only leaf package for model attributes: context window, capabilities, pricing)
 ├── ratelimit     (no deps — neutral limit signals + shared reset/fallback policy)
 ├── modelcaps     → modelinfo, log (leaf — per-backend live capability cache; Fetcher + Persister seams injected at startup so it imports no anthropic/session/DB)
 ├── compaction    → config, log, memory, messages, modelcaps, modelinfo, provider, session, tools
 ├── tempdir       (no deps — stdlib-only leaf package for canonical temp dir)
 ├── provision     (no deps — stdlib-only leaf package for agent creation)
 ├── command       → agent, config, delegator, delegator/ccstream, display, log, memory, modelcaps, modelinfo, platform, procx, provider, provision, question, session, tempdir, timeutil, tools, workspace
 ├── warnings      → log (leaf — warning queue and proactive dispatch)
 ├── messages      → provider (shared message-inspection utilities: HasToolUse, ToolUseIDs)
 ├── timeutil      (no deps — centralised timestamp formatting with configurable timezone)
 ├── relogin       → log, procx (automated CC re-login on 401 — see Backend Session Lifecycle)
 ├── delegator     (no deps — Delegator interface, registry, StartOptions, SessionEvents/TurnEvents)
  │   ├── delegator/autoapprove → (shared by ccstream/codex/opencode — auto-approve rule compilation/matching)
  │   ├── delegator/cctmux     → delegator, log, modelinfo, procx, fsnotify (tmux-based Claude Code; registers "claude-code-tmux" via init())
  │   ├── delegator/ccstream   → delegator, delegator/autoapprove, log, modelinfo, procx, question, ratelimit, tempdir, timeutil (stream-json Claude Code; registers "claude-code" via init())
  │   ├── delegator/codex      → delegator, delegator/autoapprove, log, modelcaps, modelinfo, procx, tempdir (Codex app-server JSON-RPC; registers "codex" via init())
  │   └── delegator/opencode   → delegator, delegator/autoapprove, log, procx, ratelimit, tempdir (HTTP/SSE OpenCode; registers "opencode" via init())
 ├── agent         → agent/turnevent, compaction, config, convo, delegator, display, log, memory, messages, modelcaps, modelinfo, nudge, platform, procx, provider, ratelimit, relogin, session, skills, timeutil, tools, turn, warnings, workspace
 ├── periodic      → config, log, memory, provider, session, skills, timeutil, warnings (NO agent)
 ├── dispatch      → command, platform, session, tools (shared command dispatch logic; platform wrappers delegate here)
 ├── turn          → agent/turnevent, display, log, platform, tooldetail (shared turn rendering, tool call tracking, and tool-result display store for all platforms)
 ├── telegram      → agent, agent/turnevent, chatmeta, command, config, dispatch, display, log, platform, provider, secrets, session, timeutil, tooldetail, toolformat, turn, voice
 │                  (registers via init() → platform.RegisterMessagingProvider; blank-imported in main.go)
 ├── discord       → agent, agent/turnevent, chatmeta, command, config, dispatch, display, log, platform, provider, secrets, session, timeutil, tooldetail, toolformat, turn, voice
 │                  (registers via init() → platform.RegisterMessagingProvider; blank-imported in main.go)
 ├── app           → agent, agent/turnevent, app/fap, command, config, dispatch, log, platform, question, secrets, session, sqlite, tempdir, tools, turn, voice (FAP WebSocket native-app provider — see App Provider section; registers via init() like telegram/discord)
 └── askgw         → log, peercred, question (opt-in ask-gateway for external Apps — see Ask Gateway section)
```

No circular dependencies. `provider`, `display`, `log`, `secrets`, `memory`, `skills`, `prompts`, `startup`, `resources`, `provision`, `tempdir`, `warnings`, `modelinfo`, `modelcaps`, `messages`, `ratelimit`, `timeutil`, `turn`, `dispatch`, `procx`, `peercred`, `question` are leaf packages (no internal foci deps beyond what's shown). `platform` depends on leaf packages only (config, log, secrets, session, voice, warnings).

**`internal/state` no longer exists.** The former `state` package (`system_state` crash-detection row, `state.json`/state.db key-value store, `agent/ID/default_chat`, `facet:<bot>` bot→session mapping, ask/wizard persistence) was folded into `internal/session`'s `SessionIndex` (SQLite-backed) before this doc's tracked baseline — every dependency line that used to read "state" above has been corrected to "session" (or dropped where session wasn't otherwise a dependency). If you see "state" cited anywhere else in this doc or in `shared/skills/`, it's stale.

**`provider` package:** Defines the neutral types (`Message`, `ContentBlock`, `ToolDef`, etc.) and the `Client` interface (`SendMessage`, `CountTokens`). `anthropic`, `gemini`, and `openai` all implement `provider.Client`, translating between neutral types and their wire formats.

**`platform` package:** Defines platform-agnostic messaging types (`Message`, `Attachment`), the `Connection`/`ConnectionManager` interfaces, the `MessagingProvider` interface for platform implementations, and the `Messaging` facade that manages all active providers. Providers register via `RegisterMessagingProvider()` (called from `init()`) and are activated at startup via `InitMessaging()`. An aggregating `ConnectionManager` merges connections from all providers — `AllForAgent()` returns connections across all platforms, enabling multi-platform fan-out for notifications. `cmd/foci-gw/` uses only the facade; zero platform-specific type references. Also defines the `SetupWizard` interface (optionally implemented by `MessagingProvider`) for contributing interactive setup steps to `foci first-run`. `SetupProviders()` returns all registered providers that implement `SetupWizard`. Types: `SetupFlag` (CLI flag definition), `WizardResult` (config TOML fragment + secrets), `SetupUI` (console interaction primitives).

**`chatmeta` package:** Shared per-chat metadata logic extracted from `telegram` and `discord`. Session keys are deterministic (`session.NewChatSessionKey`), so the `Resolver` derives them and registers platform ownership (a `registered` chat_metadata row backing `SessionIndex.PlatformForChat`) on first contact; it also handles `DefaultChatID`, `DefaultSessionKey`, and `RecordUsername`. Platform-specific methods (`SessionKey`, `SetSessionKey`, `ChatID`, `SetChatID`, `Username`) remain on each Bot. Imports: `platform`, `session`, `log`. All methods are nil-receiver safe.

**`route` package:** The single addressing authority. Defines the canonical `Target` grammar (`agent[/rest][?create=&policy=]`) parsed identically by every entry point (HTTP handlers, CLI, `send_to_session`, webhooks), the `Resolver` with ONE resolution ladder (exact key → existing named session → chat alias → create-named; empty rest → agent default via `SessionIndex.DefaultSessionKeyForAgent`), `Receipt` (`{target, session, resolved_via}` returned to senders in HTTP responses and tool results), `ConnFor` — the ONE outbound delivery cascade (session's own connection → policy-dependent fallback to the owning platform's primary) — and `Broadcast`, the delivery set behind `PolicyBroadcast` (`foci send --broadcast`, and the rate-limit/max-tokens warnings): one connection per platform — each platform's primary — delivering to that platform's default destination (telegram/discord: the default chat; app: the default conversation via `Hub.deliverBinding`, else the newest conversation, auto-created if none exist). Only three policies exist: `PolicyFallback` (default — session's own connection, else the agent's primary), `PolicyStrict` (session's own connection or nothing — `DeliveryNone`), and `PolicyBroadcast`. **`PolicyRootFallback` and the `DeliverySuppressed` outcome were removed (2026-07-09, `3a26a5bd`)** — branch/facet sessions with no live connection of their own now fall back to the primary like any other session, same as root sessions; the leak-prevention guard was judged not worth the complexity (see commit message for the full rationale). `route.NotifySessionChat` (`notify.go`) is a small helper for session-targeted notifications: resolves the session-or-primary connection and prefers `SessionNotifier.SendNotificationToSession` over a bare `SendNotification`. Cache-bust warnings are deliberately NOT broadcast: they concern one session's cache prefix and route to that session's chat via `SessionNotifier`. Imports: `platform`, `session`.

Most packages depend on `provider` for types; only `main.go` (`cmd/foci-gw/credentials.go`) imports `anthropic` directly in production code (for Anthropic-specific features — `tools` only references it from test-only helpers now). `periodic` still imports `session` directly (it holds a `*session.SessionIndex` to pick keepalive/reflection/background candidates) but never imports `agent` — warning dispatch is handled by the `warnings` package, wired together in `main.go`.

**`provision` package:** Shared agent creation logic used by both `cmd/foci/setup.go` (first-run wizard) and `command/agents_new.go` (`/agents new` runtime command). Stdlib-only, no imports from other foci packages. Provides `AgentSpec` + `Provision()` (workspace creation, character file copying, SOUL.md templating), validation (`IsValidAgentID`), config block generation (`GenerateAgentBlock`), and crontab templating (`GenerateCrontab`, `AppendCrontab`). Platform-specific validators (e.g. `IsValidBotToken`, `IsValidUserID`) live in their respective platform packages (e.g. `internal/telegram/validate.go`).

## Command Dispatch Architecture

Slash commands (`/ping`, `/model`, etc.) are dispatched through a three-layer architecture:

1. **Platform wrapper** (`internal/telegram/bot.go`, `internal/discord/connection.go`): Thin wrappers that extract `text`, `chatID`, and `userID` from platform-native message types (`gotgbot.Message`, `discordgo.Message`) and delegate to the shared dispatcher.

2. **Shared dispatch** (`dispatch/dispatcher.go`): Platform-agnostic routing logic. Detects dot-commands (`.model`) vs slash-commands (`/model`), resolves session keys, and builds a `command.Request`. Returns a `dispatch.Result` with `Handled`, `Response`, `SessionKey`, `UserID`.

3. **Command layer** (`command/command.go`, `type Registry`): Receives `Request` and `CommandContext` (platform-agnostic dependencies), executes the command, and returns a `Response` with `Text` and optional `DocPath`. When `DocPath` is set, it points to a temp file that the platform layer sends to the originating chat via `SendDocumentToChat(msg's chat ID, path)` and then removes. This keeps the send scoped to the exact chat that invoked the command, avoiding reliance on global "last channel" state. The HTTP `/command` endpoint handles `DocPath` by sending via `ForSessionOrPrimary(sessionKey, agentID)`.

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
  RateLimitGate         API: user probes pass; system work queues behind per-endpoint gate     Delegated: no-op
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
  RunInference          API: multi-iteration tool loop         Delegated: ImmediateInject(SourceUser interactive / SourceSystem system) (async)

Phase 4 — Post-turn:
  SaveSession           API: AppendAll to session store        Delegated: no-op (CC owns session)
  UpdateSessionMeta     API: from provider.Usage               Delegated: from backend TurnResult
  LogUsage              API: no-op (logged per-call)           Delegated: called from OnTurnComplete
  RunCompaction         API: direct maybeCompact               Delegated: sends /compact to CC
  LogConversationSent   Shared: log outbound response
  TouchActivityPost     Shared: fire OnActivity callbacks
```

**Post-turn sync/async split** (`runPostTurn`): API turns close `CompletionChan` before `RunInference` returns (synchronous), so post-turn runs inline. Delegated turns block inline waiting for `CompletionChan` with an activity-based timeout — if no stream events arrive for 2 minutes (`streamIdleTimeout`), the wait times out. Activity is tracked by the backend's `LastActivity()` method, seeded at turn start and updated on every stream event. Steered follow-ups (delegated, `IsTurnInFlight() == true`) close `CompletionChan` immediately with no post-turn work.

**Shared prompt composition** (`turn_common.go`): `composeTurnText` assembles metadata prefix, reminders, state dashboard, attachment paths, and user texts into a `turnTextParts` struct. The API transport converts these to content blocks; the delegated transport joins them into a flat string via `JoinPrompt()`.

### RunOnce Mode (`DelegatedManager.RunOnce` → `delegator.BatchRunner`, #1312)

Non-interactive backend execution for headless tasks. `RunOnce(ctx, prompt, systemPrompt)` no longer shells `claude --print` directly — it constructs a fresh (unstarted) instance of the agent's own configured backend via `m.NewBackend()` and type-asserts it to the optional `delegator.BatchRunner` interface (`RunBatch(ctx, BatchRequest) (string, error)`), so a one-shot always runs on the same vendor/auth/model family as the agent itself. There is deliberately **no cross-vendor fallback**: a backend that doesn't implement `BatchRunner` returns an error (logged at ERROR) rather than silently running the one-shot on a different CLI. `ccstream`, `codex`, and `opencode` each implement `RunBatch` in their own `batch.go` (ccstream still ends up spawning `claude --print --dangerously-skip-permissions --no-session-persistence`, but that detail now lives behind the interface, not in `DelegatedManager`). No tmux pane, no watcher, no session index in any implementation — genuinely one-shot.

Used by:
- **Nudge extraction** — `ExtractViaRunOnce` sends conversation context to the model and parses structured nudge rules from the response.
- **Consolidation** — The periodic `Runner` is wired with a `RunOnceFunc` for memory consolidation tasks that don't need an interactive session; consolidation one-shots also now receive the agent's character system prompt (previously ran with none).
- **First-run onboarding** — also routes through `RunOnce` (per the interface doc comment on `BatchRunner`).

### Session Lifecycle Operations (`agent/lifecycle.go`)

The agent exposes three lifecycle methods that encapsulate multi-step sequences previously scattered across command handlers:

- **`ResetSession(ctx, sessionKey)`** — clears session history with memory formation. The session key is a stable identity; unified across API and delegated transports: (1) `PrepareSessionEndMemory` creates the reflection branch from the still-live history — for delegated agents it also remaps the live backend and its `cc_resume_id` to the branch (`DelegatedManager.RemapSession`) so the main key gets a fresh CC on next message; (2) `Store.Reset` archives the session file in place; (3) `ClearSessionState` drops per-session overrides/metadata; (4) `RunSessionEndMemory` drives reflection on the branch in the background (up to 120s) and destroys the branch backend.
- **`CompactSession(ctx, sessionKey, dryRun)`** — triggers manual compaction. Validates message count (min 5), runs the compaction pipeline, then reloads bootstrap and resets cache baseline. When `dryRun` is true, the full pipeline runs (API call, summary generation) but the session is left unchanged — the summary is returned for inspection.

All three call `reloadAfterMutation()` internally, which reloads bootstrap, refreshes nudges, and invalidates all per-session system prompt caches.

**Delegated system prompt rebuilt from disk at session start (#828 Part A, fixes #706):** the delegated CC system prompt was previously built once at agent setup and frozen into `StartOpts.SystemPrompt` for the process lifetime, so `/reset` and idle respawn never picked up character-file or skill edits. `StartOptions.SystemPromptFunc` fixes this: when set, `DelegatedManager.getOrCreate` calls it at every session start and its non-empty result wins over the static prompt. The closure (wired in `agents_delegated.go`) reloads `Bootstrap` from disk itself and re-runs the skill load, so every respawn — reset, idle, compaction-bounce — gets a fresh prompt regardless of caller. Empty result falls back to the setup snapshot.

### Steer Mode Differences (API vs Delegated)

When `steer_mode` is enabled and a turn is active, user messages are buffered as "steers" and injected mid-turn rather than waiting for completion. **Only real-time user input steers** — platform messages (telegram/discord/app) and voice. System-initiated input never does, even with steer enabled (see "System injections never steer" below).

**Per-message override (app):** the sender can decide ad-hoc per message via `fap.ClientMessage.Steer` (`"steer"` / `"queue"` / empty = config default), mapped to `agent.Envelope.Steer` (`SteerAlways` / `SteerNever` / `SteerDefault`). `SteerAlways` steers even with `steer_mode = false`. `SteerNever` queues for a fresh turn after the in-flight turn completes AND is exempt from every conversational intercept that would otherwise consume it — plan-cancel feedback (#858), pending-`foci_ask` answer capture (both the `Enqueue` mid-turn gate and `RunTurn`'s idle capture), and the backend AskUserQuestion/elicitation typed-answer intercepts. At the transport, a `SteerNever` turn dispatches like a system turn (`ImmediateInject(SourceSystem)` — never folds, waits for backend idle); `RunTurn` threads the preference to `RunInference` via `WithSteerPreference`:

- **API transport:** Steer messages are collected via `steerBlocks(ctx)` and injected as text content blocks in the tool result message between tool execution loops. `steerBlocks` pulls from the `turnevent.Steerer` supplied by `agent.Inbox` (one per session) — the inbox accumulates mid-turn text in its per-session steer buffer when the configured backend is API-mode (no `delegator.Delegator` registered).
- **Delegated transport:** Steer messages are dispatched immediately by `agent.Inbox`. On `Enqueue` of a text-only mid-turn message, the inbox calls `Backend.ImmediateInject(ctx, Inject{Source: SourceSteer, Text: env.Text})` directly, looking up the session's backend via the agent's `DelegatedManager`. `ImmediateInject(SourceSteer)` sends the steer text as a `type: user` stream-json event at queue priority `"next"` (CC's own class for user input). CC's mid-turn drain (`claude-code/src/query.ts:1570-1589`) folds the message into the current `ask()` as an attachment to the next tool-result batch, so the model responds in the same turn and the original handler's `OnText`/`OnTurnComplete` pipeline carries the response. Steer does not abort anything: priority `"now"` — which makes CC `abort('interrupt')` the in-flight ask and answer immediately — is reserved for NYI per-message steer tagging or an NYI aggressive-steer config mode; "stop right now" semantics use `/reset hard`. Mid-turn steer for delegated agents bypasses the steer buffer entirely; the buffer only matters for API-mode agents that have no equivalent stdin protocol primitives.

**Compaction hold (#856):** `Enqueue` gates the steer decision on `Agent.IsCompacting(sessionKey)` — while a `/compact` turn is in flight, a steer would write to CC's stdin mid-compaction and CC folds the raw text into the compaction transcript unframed (no `[meta]` header). The gate routes such messages to the session channel instead. Auto-compaction runs synchronously inside the driven turn (`driveOnce` → `runPostTurn` → `RunCompaction`), so the worker is already blocked and channel-queued messages wait naturally; the session worker adds a `for a.IsCompacting(...)` poll-hold (`compactionHoldPoll`, 100ms) after the #767 in-flight gate as a backstop for the manual-`/compact` path where the worker is free. Held messages dispatch as a clean fresh turn once compaction clears.

**Declined-compaction release (#1267):** `runDelegatedCompact` arms the ccstream compaction waiter and blocks on `WaitForCompaction`, which historically returned only on a `compact_boundary` stream event or the 5-minute `delegatedCompactTimeout`. When CC *declines* to compact (a short session: `status=compacting` → assistant "Not enough messages to compact." → `result` → `session_state_changed:idle`, with **no** `compact_boundary`), that wait used to stall the full timeout with `IsCompacting` latched — so the compaction hold above held every inbound message and the session looked stuck for ~5 min. Fix: a real compaction always emits `compact_boundary` *before* idle (it nils `compactDoneCh` first), so at `session_state_changed:idle` a still-armed waiter means CC declined — `signalCompactionAbort` (ccstream `compaction.go`) fires a `compactAbortCh` and `WaitForCompaction` returns `delegator.ErrCompactionNoBoundary`. `runDelegatedCompact` treats that as a benign no-op (returns promptly so the deferred `clearCompacting` releases the hold; skips the "✅ compacted" notify and the #828 bounce). `/compact` reports "Nothing to compact — session too short."; auto-compaction logs and moves on.

### System Injections Never Steer

System-initiated input — HTTP `/send` (`foci send`, cron keepalives), `/wake` fall-through, webhooks, scheduled wakes, restart changelogs, proactive warnings, error notifications, inter-session notifies (`session_notify`), and the periodic reflection/keepalive/memory passes — must never fold into (steer) an in-flight turn. It always waits gracefully for turn completion and then runs as a fresh, fully-tracked turn. Enforced at two layers:

1. **The session inbox is the queue.** Every system entry point routes through `Agent.Enqueue` with an `Envelope.Inject` (`InjectMeta{Trigger, Run}`), so the per-session worker serialises it with platform turns, defers it behind a pending `foci_ask`, and holds it through compaction. Sync callers (HTTP `/send --sync`, delegated reflection/keepalive passes that must complete before their scheduler continues) use `Agent.EnqueueInjectWait`, which blocks until the worker has run the closure. Gateway plumbing: `runAgentQueued` / `asyncDispatch` (`cmd/foci-gw/http.go`), `deliverToSessionChat` / `newSessionNotifyFn` / `newAsyncNotifier` (`agents_notify.go`), `handleDelegatedBranch` (`agent_sessions.go`). `EnqueueInjectWait` must NOT be called from the session's own worker (deadlock) — nested same-session system turns invoked from a turn's post-phase (pre-compaction memory, session-end memory) call `HandleMessage` directly.
2. **`ImmediateInject(SourceSystem)` at the backend.** `RunInference` classifies the turn via `isInteractiveTrigger` (`internal/agent/context.go`): only registered platform triggers (telegram/discord/app) and `voice` count as interactive. Interactive turns keep the fold path (`ImmediateInject(SourceUser)` follow-up / `SourceSteer`); everything else dispatches as `ImmediateInject(SourceSystem)`, whose backend implementations (all three) atomically begin a turn iff idle — the idle check and turn begin happen under one lock, so racing begins can't clobber each other's `TurnEvents` — and return `delegator.ErrTurnInFlight` otherwise. On rejection `RunInference` waits (`WaitForTurn`, `systemInjectRetryInterval` timeout backstop) and retries. This second layer covers turns the inbox worker can't see: backend-only runs (opencode shadow turns) and the nested post-phase memory turns above.

**Autonomous runs and the pending-work gate (#1068/#1070, spec §4).** A CC *autonomous run* — CC self-resuming with no foci turn open, triggered by a backgrounded subagent or `run_in_background` Bash completing — delivers its text to the chat even though foci opened no turn for it. Delivery works because `SessionEvents` binds once (at backend acquisition) to a per-session **router** (`Agent.sessionRouter`), not to a per-turn ctx sink; a system turn can therefore never rebind the session to its silent `NopSink` (the #1068 poison). Outside any turn the router falls through to a late-delivery sink resolved at emit time (`resolvingLateSink` → `route.ConnFor`), so autonomous text reaches the chat; `autonomousStreamed` dedups the streamed-vs-result copies.

But a system inject must be held not just while a run is *visibly active* — across the whole background-work window, from the moment a turn backgrounds a subagent/Bash until the resulting autonomous run completes. The backend reports this via the optional `delegator.AutonomousRunAwaiter` interface: `AwaitingAutonomousRun()` is true while the `SubagentTracker` has pending work (`Pending() > 0`), an autonomous run is active (`autonomousActive`), or the post-run chain grace (`autonomousInjectGrace`) is open. The tracker counts both Agent-tool subagents and `run_in_background` Bash (detected at the `tool_use` block via `ExtractBashBackground`); a missed completion can't wedge the gate forever because a max-age prune drops stale entries (`[cc_backend].background_task_max_age`, default 30m). The inbox inject gate predicate is `IsInFlightDelivering(sk) || backendAwaitingAutonomousRun(sk)` (the latter a nil-safe, non-creating probe via `DelegatedManager.BackendAwaitingAutonomousRun` → `getManaged` — false for API agents and non-tracking backends). The adopted-run edge broadcasts via `InFlightWaitCh`, but a pending→run→clear transition has no channel, so the wait loop also polls at `injectGatePollInterval` (~1s). `tryBeginTurn` (the `SourceSystem` exclusive path) mirrors the same rejection: it returns `ErrTurnInFlight` while `Pending() > 0`. Scope is system input only — a platform (user) turn on the Driver path is never held on pending work (spec §3: user input adopts/folds delivering runs). The gate is one helper (`Agent.waitInjectGate`) applied at every `runInject` site — both the dequeue path and the post-batch `heldInjects` loop (injects drained alongside a platform turn), so an inject can't slip past by riding a platform turn. Residual: a µs-wide window exists between a task's `task_notification` removing it from the tracker and the chained run setting `autonomousActive`; Phase 1's router binding makes a system turn landing there non-catastrophic (it registers its `NopSink` post-accept and the run still delivers via the router fallback).

The `onAutonomousStart`/`onAutonomousEnd` adoption callbacks must fire in flip order or the adoption counter leaks: a start (reader goroutine) and an adopting end (turn goroutine) flip `autonomousActive` under `turnMu` in the true order, but firing after releasing the lock could reverse them (release-before-adopt → phantom `markInFlight(+1)` with no releaser → wedged gate). `setAutonomousActiveLocked` therefore *enqueues* each edge callback onto a per-backend FIFO under `turnMu`; callers then call `drainEdgeCallbacks`, which fires them under a dedicated `fireMu` in enqueue order. Enqueue-under-lock makes queue order == true flip order, so the drain can't reorder regardless of goroutine scheduling; `turnMu` is never held across a callback.

**Guard audit (which of the overlapping guards still earns its place).** The router binding (Phase 1) is the structural fix; the surrounding guards are re-justified against it: the **inbox adoption gate + `tryBeginTurn`** stay — the router limits blast radius but a system turn that *begins* mid-run still Registers its NopSink and captures the run's remaining output, so keeping it from beginning is load-bearing. **`autonomousStreamed`** stays — it's genuine dedup between live streaming and idle result delivery, not a race guard. **`markInFlight` adoption** stays — it's how an autonomous run counts as a turn for the keepalive/activity gates (spec §1) and how the inbox observes it. The **5s `autonomousInjectGrace`** is the one on probation: the pending-work gate now covers every *tracked* chain, so the grace only matters for untracked self-resumptions. It's instrumented (`tryBeginTurn` logs "grace blocked a system inject the pending-work gate would not have", once per window) so production frequency decides removal; kept for now because #1048 was observed in production and it's cheap (promote the hardcoded 5s to config only if it survives a release). **Accepted residual:** CC self-resuming for a reason foci can't see, in the ms before `session_state:running` arrives, remains possible — with the router binding the damage degrades from blackout to mis-attribution (output folds into the inject's turn but the router still delivers the user-facing text). Documented as a known bound, not a fifth guard.

The underlying rule for what may fold: a folded turn's own result is an empty accounting shell (the response merges into the in-flight turn's record and delivery stream), so folding is safe **iff the turn's result would have been delivered to the session's own platform stream anyway**. Platform chat and voice qualify — the user is watching that stream. Anything that routes its result elsewhere (inter-session `reply_to` replies, sync `/send`/webhook response bodies, scheduler completion waits) must never fold, or the consumer reads an empty result while the real response lands in the chat. Today "routes the result elsewhere" and "system-triggered" are the same set, which is why `isInteractiveTrigger` is the gate — judge any future change to the foldable classification against the delivery-destination test, not the trigger label.

The typed-answer intercepts in `RunInference` (pending `AskUserQuestion` / elicitation) are likewise interactive-only — a keepalive or notification text must never be consumed as a question's answer; it waits for the prompt to resolve and the turn to complete.

**Plan-cancel-by-message (#858):** a pending **ExitPlanMode** permission blocks the session — CC waits for Allow/Deny and ignores stdin until it answers, so a steered or queued message would either hit ignored stdin or wait indefinitely (the ~20-min "hung typing indicator" symptom). UNLIKE a normal tool permission — which a follow-up message keeps queuing behind via `WaitForPermission` — a typed message during *plan* approval is treated as revision feedback. Before the steer/queue routing, `Enqueue` checks (for an active turn, text-only) whether the session backend implements `delegator.PlanResponder` and has a pending plan permission (`HasPendingPlanPermission` scans `pendingPerms` for `toolName=="ExitPlanMode"`); if so it calls `CancelPlanWithFeedback(reqID, text)`, which sends a `PermissionDeny` carrying the text as the rejection `message` (CC stays in plan mode and revises using the feedback), then fires the prompt's cancel listener via `outstanding.Cancel` so the Allow/Deny buttons edit to "❌ Plan cancelled by follow-up message" and `onEmpty` clears `permPending`. The message is consumed (it became the denial feedback) — not also re-sent as a turn. Scope is ExitPlanMode-only by design; every other permission still queues unchanged.

### Backend Watcher — tmux (`internal/delegator/cctmux/watcher.go`)

The tmux backend's session watcher tails Claude Code's JSONL session file via fsnotify. It converts raw JSONL events into structured callbacks (assistant text, turn completion, usage, agent status). For the stream-json backend (ccstream), see the [ccstream Backend](#ccstream-backend-internalbackendccstream) section below — it receives these events directly on stdout rather than from a file watcher.

**Subprocess startup:** On `Backend.Start`, cctmux spawns `claude` in a tmux window named `cc-{agentID}` in the agent's workspace directory via a login shell (`sh -l -c`). The concatenated system prompt (workspace `*.md` files + skills + environment block) is written to `{workspace}/character/.full-prompt` and passed via CC's `--system-prompt-file` flag. Session ID, if known from a previous run, is passed via `--resume <uuid>` so CC reattaches to the existing session rather than starting fresh. User messages and slash commands are paste-buffered into the tmux pane via `tmux load-buffer -` (piped from stdin — no temp files) followed by `paste-buffer -p` to deliver. Sessions are discovered lazily — the JSONL watcher is created on the first message, not at process startup, so launching never depends on knowing the session ID up front.

**Pre-send offset:** Before `ImmediateInject(SourceUser)` pastes the prompt into the tmux pane (via the internal `sendToPane` primitive), the watcher records the current JSONL file size. The watcher starts reading from this offset so it doesn't replay old content from earlier turns. Falls back to `-1` (tail from end of file) if the offset discovery fails.

**Synthetic response filter:** Claude Code emits synthetic messages (model: `<synthetic>`) such as `"No response requested."` and `"[[NO_RESPONSE]]"`. The watcher filters these at the event level — they never reach the reply callback.

**Typing indicator:** Both backends use `SetTypingFunc` to register a callback. Set to `true` when a turn begins (via `ImmediateInject(SourceUser)` at idle), set to `false` when `OnTurnComplete` fires. The platform `Connection.SetTyping(bool)` is stateful — `true` starts a periodic ticker (Telegram: 4s, Discord: 9s) that keeps the indicator alive until `false` is called. The ccstream backend also restarts the typing indicator on `OnAssistant` (mid-turn text) and `OnToolProgress` (heartbeats during long tools).

**Usage extraction:** Assistant messages in the JSONL carry a `usage` payload. The watcher extracts `TurnUsage` (InputTokens, OutputTokens, CacheCreationInputTokens, CacheReadInputTokens) from the last assistant message in each turn. This is reported via `TurnState.FinalUsage` on completion. The ccstream backend extracts the same from structured `AssistantMessage` objects on stdout.

**Per-turn completion callbacks:** `ImmediateInject(SourceUser)`'s begin-turn path registers a one-shot `OnTurnComplete` handler that fires when the turn ends (`end_turn` in JSONL for tmux, `ResultMessage` on stdout for ccstream). The callback sets `TurnState.FinalText` and `TurnState.FinalUsage`, then closes `TurnState.CompletionChan` — triggering the post-turn goroutine (save, metadata, compaction, logging). Both backends carry it on `Inject.Turn` (`TurnEvents` — per-turn bookkeeping); ccstream fires it from `OnResult`, cctmux from its JSONL watcher's `fireTurnComplete` on `end_turn`.

**Agent spawn tracking:** The tmux watcher tracks pending `tool_use` calls for the Agent tool. The ccstream backend receives task lifecycle events (`task_started`, `task_notification`) as system messages. Both report status via the `onAgentStatus` callback, allowing the platform to show agent activity state.

**Subagent reactivation (#1355).** A background subagent can run more than once: the initial `Agent` spawn, then any number of `SendMessage` resumes. The STABLE identity across a resume is the `task_id` — the `tool_use_id` CHANGES per run (a resume's `task_started`/`task_notification` carry the *SendMessage* block's id), while the subagent's text keeps the ORIGINAL Agent `tool_use_id` as its `parent_tool_use_id` group key. Keying lifecycle on `tool_use_id` therefore left a resumed subagent invisible (tracker never re-`Add`ed; the app's group showed "completed" though work continued). ccstream now maps `task_id → {groupKey, runIndex}` (`subagentRuns`, bound at the first `task_started`; `handlers.go`): a subsequent `task_started` for a known `task_id` bumps `runIndex`, re-`Add`s the tracker (chip re-opens), and emits a fresh `SubagentStart(groupKey, runIndex, prompt)`; a `task_notification:completed` maps `task_id → groupKey` so `SubagentEnd` closes the right run (not the resume's fresh id). The reactivation prompt is captured from the `SendMessage` block's `input.message` (keyed by `to == task_id`); run 1's prompt from the `Agent` block's `input.prompt`. `SubagentStart`/`SubagentEnd` carry `RunIndex`+`Prompt`, and `SubagentText` carries `RunIndex`, through `turnevent` → `SubagentDeliverer` → the `subagent.start`/`subagent.text`/`subagent.end` FAP frames (additive-optional fields), so the app can draw per-run chits over one continuous, divider-split view. A text block's run is resolved by `runIndexForGroup(groupKey)` — the subagent's text keeps the ORIGINAL Agent `tool_use_id` as its group key across resumes, so it maps to the run entry whose `groupKey` matches (untracked → 1, the client's default). Non-CC backends (codex/opencode) have no reactivation and always emit run 1. **Messaging a STILL-RUNNING subagent (#1419)** is a distinct case from resuming an ended one: CC never refires `task_started` for a `SendMessage` sent before `task_notification:completed` — the message folds into the live run with no new stream event — so the stash/reactivation path never fires. Instead ccstream emits `OnSubagentPrompt`/`SubagentPrompt` (`subagent.prompt` FAP frame; `subagent_runs.go` tracks an `active` flag + `activeRunForTask` to choose the path in `handlers.go`), which attaches the follow-up to the ALREADY-OPEN run at its CURRENT `runIndex` — deliberately NOT a new `SubagentStart`, which would open a run that never receives its own `SubagentEnd` (CC sends one completion for the whole continuous execution) and spin forever. The client renders it as another `role="prompt"` block inline in the open run. telegram no-ops it (as with `SubagentStart.prompt`).

**Permission auto-approval:** When CC sends a `can_use_tool` permission request, the ccstream backend's `handleToolRequest` first checks against compiled auto-approve rules (from `[permissions]` config). Rules are assembled at startup by `buildAutoApproveRules`: built-in common readonly tools/commands (if `auto_approve_common_readonly` is true, default on), an opt-in built-in safe-write list of side-effecting commands (`curl`, `wget`, `mkdir`, `touch`; enabled by `auto_approve_common_safe_write`, default off — these rules are not path-scoped, so the operator must trust the agent not to target paths outside its workspace), workspace-scoped Edit/Write access, and user-configured patterns from global + per-agent config (union). For Bash commands, the command is split on shell operators (`&&`, `||`, `;`, `|`) and every segment must independently match at least one Bash rule — this prevents `git status && rm -rf /` from being auto-approved by a `git *` rule. Matched requests are approved directly via `SendControlResponse` with an INFO log. Unmatched requests are forwarded to the user via the platform connection with an inline keyboard of choices (Allow, Deny, Always Allow).

**AskUserQuestion handling:** When CC's `AskUserQuestion` tool triggers a `can_use_tool` request, `handleToolRequest` routes it to `handleUserQuestion` (`userquestion.go`) instead of the standard permission flow. The handler parses the questions from the tool input, stores a `pendingPermission` with question state (questions, current index, accumulated answers), and presents the first question as an interactive prompt with option buttons plus Cancel. For multi-question sequences, questions are presented one at a time; each answer advances the sequence. The user can also type a custom text answer (intercepted in `RunInference` before `WaitForPermission` blocks) or cancel via the Cancel button or `/stop`. When all questions are answered, the response is sent as `PermissionAllow` with `updatedInput` containing the original input plus an `answers` map (`{question_text: answer}`). CC receives this as the tool's input and returns the formatted answers to the model.

**Elicitation handling (`ccstream/elicitation.go`):** MCP servers can raise an `elicitation` control_request subtype when a tool call needs structured user input mid-turn. The reader dispatches these alongside `can_use_tool` and `OnElicitationRequest` builds a `pendingElicitation` (separate map from `pendingPerms` — elicitations aren't keyed to tool_use_ids). Two modes are supported: **form** walks the `requested_schema` one property at a time, presenting each field through the same `permPromptFn` platform callback used for permissions. Free-text fields accept typed answers via the same text intercept path as AskUserQuestion (`HasPendingElicitation` from `RunInference`); enum properties render as buttons; booleans render as Yes/No; once every field is satisfied, the accumulated answers are marshalled into a `content` object and sent back as a `control_response` with `action: "accept"`. **url** mode surfaces the URL with Done/Decline/Cancel buttons — Done sends `accept` with no content, while an out-of-band `system/elicitation_complete` notification from CC auto-resolves the matching (`mcp_server_name`, `elicitation_id`) entry without the user clicking Done. Unsupported or missing schemas fall back to a Decline/Cancel-only prompt (foci never synthesises field values it didn't collect). Decline and Cancel at any point short-circuit the walk and send the corresponding action with no content. The drain hook fires only when both `pendingPerms` and `pendingElicits` are empty (enforced by the unified `OutstandingRegistry` — see below) so the platform's "has pending prompt" indicator doesn't flap mid-walk. The `delegator.ElicitationResponder` optional interface exposes `RespondToElicitation` / `HasPendingElicitation` to the agent layer, mirroring `QuestionResponder`.

**Outstanding-prompt registry (`internal/delegator/outstanding.go`, package `delegator` — a shared root type, not ccstream-specific):** All user-input prompts (permissions, AskUserQuestion sequences, MCP elicitations) share one `OutstandingRegistry` per Backend. Each `pendingPerms`/`pendingElicits` insertion is paired with a `Register(requestID, kind)` call; resolutions call `Resolve(requestID)`; CC's `control_cancel_request` calls `Cancel(requestID, reason)`. The registry provides three things on top of the kind-specific stores: (1) a multi-listener cancel fanout — the platform layer registers a per-prompt cancel callback via `Backend.RegisterPromptCancelListener` at the same time it sends the interactive UI, and the registry fires those callbacks (in registration order) when CC cancels the prompt before the user responds; (2) a registry-wide `onEmpty` drain hook (`Backend.SetOnPromptsCleared`) that fires only when ALL outstanding prompts have been removed — fixing a pre-Phase-2 asymmetry where `removePendingPerm` could trigger the drain while elicitations were still outstanding; (3) idempotent semantics — cancelling/resolving an unknown requestID is a silent no-op rather than a side-effecting fall-through. `DelegatedManager.RegisterPromptCancelListener(sessionKey, requestID, fn)` exposes the per-prompt registration to the agent layer; in `cmd/foci-gw/agents_delegated.go`, the platform closure that calls `SendInteractiveMessageWithID` registers a cancel listener that invokes `platform.CancelInteractiveMessage` to disable the orphaned inline keyboard.

### Backend Session Lifecycle

**Session ID persistence:** `SetOnSessionReady` registers a callback that fires when the watcher discovers the CC session UUID from the JSONL path. The UUID is persisted in the state store. On restart, `--resume <sessionID>` is passed to the `claude` command to reconnect to the existing CC session rather than starting fresh.

**Per-instance exec bridge sockets:** Each delegated backend gets its OWN exec bridge socket, `exec-<session-key>-<gw-pid>-<n>.sock` (`NewSessionExecBridge` in `internal/tools/execbridge.go`) — unique per backend *instance*, not per session key. This is load-bearing on `/reset`: the dying session's backend is remapped onto a branch key to finish memory formation in the background while a fresh backend takes over the original key (see `Agent.BranchStrategyFor`'s session-end case). If both derived their socket from the session key alone they would share one socket, and reaping the dying branch would close the live session's bridge out from under it (#1120). The gateway pid isolates separate gateway processes sharing one `FOCI_TMPDIR` (the #804 test hijack); the `<n>` counter is the actual per-instance discriminator. Paths are never reused, so the former stable-path memo / `socketIsLive` hijack guard are gone. Sockets do NOT survive a foci restart — backends are killed+resumed, not reattached (the open #1101).

**Schema-driven shell functions:** Shell functions for `ExecExport: true` tools are emitted by `generateShellFunc` in `internal/tools/execbridge.go`. A small set of tools with custom UX (stdin reading, accumulator flags, subcommand dispatch — `web_search`, `memory_search`, `web_fetch`, `http_request`, `send_to_chat`, `todo`, `summary`, `spawn`, `tmux`) have hand-rolled cases. Every other tool falls through to `generateGenericShellFunc`, which emits a flag-parser for each schema parameter: snake_case keys become kebab-case flags, booleans are presence-only, strings consume two args, and required params trigger a usage line on missing. Both `--help` text (`generateHelpText`) and the body derive from the same JSON schema, so they cannot drift. `writeShellFuncs` calls `validateShellFuncSchemaParity` before writing — any tool whose schema gains a parameter without a matching `--<flag>` case arm in its body returns an error from `NewExecBridge`, surfacing the failure at production startup rather than at runtime.

**Branch rejection:** Delegated agents return HTTP 400 for `/branch` endpoint requests. The three task-type strategies:
- **Inject into main session** — reflection and compaction-memory prompts are sent directly into the running CC session (no branch needed).
- **New independent CC session** — consolidation, background tasks, and nudge extraction use `RunOnce` (see above), which spawns an independent headless CC process.
- **Reject** — the HTTP `/branch` endpoint is explicitly rejected since delegated agents don't support session branching.

**/reset:** Archives the session in place and returns — it does not block on memory formation. The live CC backend (and its resume ID) is handed to the reflection branch via `DelegatedManager.RemapSession`; the reflection pass then runs on that branch in the background and destroys the backend when done. The main key starts clean — a fresh CC session spawns lazily on the next message. See `agent/lifecycle.go:ResetSession`.

**/stop:** Interrupts the current turn. Tmux backend: sends Escape×2 + Ctrl-C via `send-keys`. Stream backend: sends an `interrupt` control message over stdin. Both halt the in-flight inference/tool execution inside Claude Code.

**Compaction reload-bounce + resume nudge (#828 Part B / #845):** after a delegated `/compact`, CC keeps the same frozen system prompt, so memory/skill edits made during the session never reach the post-compaction context. `runDelegatedCompact` (`agent/compaction.go`) fixes this by bouncing the CC session after a successful compaction (gated on the per-agent `ReloadOnCompact` flag, default on):

- **`DelegatedManager.BounceSession(sessionKey)`** closes the backend but *keeps* the saved resume ID (factored out of `ResetSession` via the shared `closeManaged(sessionKey, clearResume=false)` helper), so the next message respawns CC with `--resume <same session>` — resuming the now-compacted conversation. Part A's `SystemPromptFunc` then rebuilds the prompt from disk on that respawn.
- **Prompt-change gate (#828 follow-up):** every CC session start fingerprints the prompt it launched with — `log.SystemHash` of the full effective prompt from `SystemPromptFunc` (environment block incl. `## Platform`/permission allowlist + character files + skill blocks) — stored as `systemPromptHash` on the per-session `managedBackend` record in `m.backends[sessionKey]`. At compaction, `BounceSessionIfPromptChanged(sessionKey)` recomputes the hash from disk and bounces *only* when it differs (returns whether it bounced). This catches character-file edits, skill add/remove (the skill list is in the prompt), and env-visible changes (permission allowlist, shell-tool set, platform claim) but **not** skill body edits (bodies load on demand, never in the prompt). The env block's platform comes from the durable chat claim, not connection liveness, so the fingerprint is stable across startup transients — a bounce always reflects a real change. With no `SystemPromptFunc` configured it falls back to an unconditional bounce. Unchanged prompt → no restart → seamless compaction (pre-#828 behaviour).
- **Self-injected resume nudge (#845):** a mid-task flow has no next message to drive the post-bounce respawn, so it would silently stall. `maybeInjectCompactionResume(sessionKey)` synthesises one — `compactionResumePrompt` instructs the model to resume if mid-task or emit the `NoResponseSentinel` if idle — injected via `AsyncNotifier.InjectToAgent`. It is gated twice: it fires **only** when `BounceSessionIfPromptChanged` actually bounced (no restart → nothing to recover from), and is suppressed when `Agent.InboxHasPendingInput(sessionKey)` reports queued/steer input (the user's own follow-up will drive continuation with real intent) or async self-injection is unavailable.

**Automated CC re-login on 401 (`internal/relogin`, #843):** when the shared CC OAuth credential can no longer be refreshed, the subprocess returns a 401 ("Failed to authenticate") and every CC agent is dead until re-authentication. The relogin package automates recovery:

- **Detection (ccstream only).** `isAuthFailure` (`ccstream/authfail.go`) matches a 401 at two sites — the error `result` in `OnResult` (`handlers.go`) and the subprocess stderr/exit path (`lifecycle.go`) — since a dead token can surface either way. The backend fires `onAuthFailure(detail)`, wired via `Backend.SetOnAuthFailure` in `agents_delegated.go` to the agent's `triggerRelogin(reason, sessionKey)` closure.
- **Gate (`relogin/gate.go`).** `relogin.G` is a process-wide single-flight drop gate. The first 401 claims it (`G.Start()`); while active, `Agent.Enqueue` drops inbound messages for delegated agents (the `DelegatedManager != nil` check is the cheap "is delegated" test) — *except* a one-shot capture window (`ShouldCapture(agentID)`) where the triggering agent's next message is treated as the pasted-back login code (`SubmitCode`). Every driver exit path releases the gate (`defer G.Release()`), so a failed or timed-out login can never wedge message processing — the backstop, not the happy path.
- **Driver (`relogin/driver.go`).** `relogin.Run` drives an interactive `claude /login` in a dedicated tmux pane (regular TUI, not stream-json; standalone tmux helper, no cctmux dep), extracts the sign-in URL (`extract.go`), relays it to the user, awaits the code through the capture window, feeds it back, and confirms success. Aborts log at ERROR (surface in `/errors`); the just-submitted one-time code is redacted from any diagnostic screen dump.
- **URL routing (`ba3cd05c`).** The triggering session key threads through `ReloginTrigger(reason, sessionKey)` into the relogin `Config`; the URL is delivered via `conn.SendToSession(sessionKey, ...)` (reads the chat ID straight from the key). Manual `/login` passes `req.SessionKey` so the URL returns to whoever ran it; the auto-401 path passes `""` → the agent's primary/default chat. A hardening fix makes `BotForSession("")` return nil in both telegram and discord so an empty key never matches an idle facet bot and correctly falls through to the agent's primary.
- **Manual trigger (`563bac86`).** `/login` (a `RequiresBackend` command, `command/login.go`) invokes the same flow on demand for testing without waiting for a real 401. The trigger is built once in `configureDelegated` and shared between the 401 callback and the command via `Agent.ReloginTrigger` — wired only for ccstream (nil for cctmux; the command reports unavailability otherwise).
- **Startup readiness probe (#853).** A dead OAuth credential at boot would otherwise stay invisible until the first user turn 401'd — and that first turn carries the first-run onboarding, so the onboarding would be lost to the auth failure. `checkDelegatedReadiness` (`cmd/foci-gw/notifications.go`, called from `main.go` after `StartAll`, before `handleRestartAndFirstRun`) probes each delegated backend via the `Delegator.CheckReady(ctx)` interface method (`delegator/backend.go`). ccstream's impl (`ccstream/readiness.go`) shells `claude auth status` — parsing stdout before honoring the exit code, since the binary prints the same JSON whether logged in or not — and fires the existing `onAuthFailure → triggerRelogin` path when not authenticated; cctmux reports ready unconditionally (`cctmux/turn.go`); API agents (`DelegatedManager == nil`) are skipped. Probes run concurrently but the pass waits for all to settle, so a not-ready agent's relogin gate is reliably active before any startup turn is injected.

**Shared rate-limit policy + periodic suppression (#1211).** API, Claude Code, and OpenCode emit `ratelimit.Signal` values containing a limit kind plus any trustworthy absolute reset or `Retry-After` hint. The stdlib-only `internal/ratelimit` leaf package is the single interpretation policy: valid hints win; missing usage-window hints use one hour; missing request-limit hints back off 1m, 2m, 4m, … capped at 1h. The agent is the sole owner of closing endpoint gates and firing `RateLimitFunc`. Claude Code detects its synthetic no-API-call session-limit result (`model=<synthetic>`) and extracts the named-zone clock; OpenCode detects `session.status: retry` usage-limit messages and extracts only explicitly offset timestamps; direct API calls extract `Retry-After`. Periodic/system work consults the gate and queues when closed. User delegated turns bypass it by design, while user API turns are explicitly allowed through as recovery probes; any successful API response releases that endpoint gate immediately and the normal drain tick replays queued system work.

**Periodic gate split.** The rate-limit gate is consulted by two agent-level checks: `Agent.SessionRateLimited(sessionKey)` (rate limit only, keyed on the session's endpoint) and `Agent.CanFireBackgroundOperation` (`SessionRateLimited` **+** the `can_run_background` script). Every model-calling periodic scheduler consults the gate on its **specific** target session (not the agent root): keepalive per warm-window target, reflection per due session, background/consolidation/reset on their parent key. Only **`maybeBackgroundWork`** runs the full `CanFireBackgroundOperation` (so `can_run_background` gates background work alone); keepalive/reflection/consolidation/reset use `SessionRateLimited` via the runner's `checkRateLimit` helper. The memory hooks (`compaction_memory`, `session_end_memory`) still use the full `CanFireBackgroundOperation`.

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

**Mid-turn injection:** The unified entry point is `Backend.ImmediateInject(ctx, Inject{Source: ..., Text: ...})`. A mid-turn **steer** (`SourceSteer`) is written to CC's stdin at explicit priority `"next"` (`sendUserMessagePriority` → `writer.SendUserPriority`); an in-flight `SourceUser` follow-up goes in at the same priority implicitly (CC's default). `SourceSystem` never goes in mid-turn at all — it begins a turn atomically at idle (`tryBeginTurn`) or returns `ErrTurnInFlight` for the caller to wait and retry (see "System Injections Never Steer"). All `"next"` items fold into the current ask at CC's next mid-turn drain (tool boundary) and stay inside the current run, so the reply belongs to the current foci turn and no per-inject bookkeeping is needed — see **Idle-keyed turn completion** below. CC's other priority classes are deliberately unused: `"now"` (aborts the in-flight ask via `print.ts`/`REPL.tsx` `abort('interrupt')` and answers immediately) is reserved for NYI per-message steer tagging or an NYI aggressive-steer config mode; `"later"` (excluded from the mid-turn drain; CC's own background task notifications) has no foci use — background content stays in foci's inbox queue, which provides turn bookkeeping CC's queue cannot. The `interrupt` control request (`Backend.Interrupt`, mirrors the Agent SDK's `client.interrupt()`) is wired to `/stop` **only** — it is *not* used by steer.

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
1. `ImmediateInject(SourceUser)` at idle calls `sendToPane`, which calls `beginTurn(turnEvents)` (sets `b.turnEvents`, resets text/tools counters, creates result channel). Delivery is unaffected — `b.sessionEvents` was already attached.
2. `Writer.SendUser(prompt)` writes a user message to CC's stdin.
3. CC processes the turn, emitting `assistant`, `tool_progress`, and `stream_event` messages.
4. `OnAssistant` accumulates text, counts tool_use blocks, and fires `SessionEvents.OnText` / `SessionEvents.OnToolStart`. Mid-turn steer dispatch is handled at the agent's per-session inbox (see `agent.Inbox.Enqueue` routing), not at tool boundaries — this lets text-only turns be steered too.
5. `OnResult` captures this ask cycle's text/usage/model and **stashes** it (`stashedResult`; output tokens sum across cycles). It does NOT complete the turn.
6. `OnSystem("session_state_changed", state=idle)` → `onSessionIdle` → `completeTurn`: fires `TurnEvents.OnTurnComplete` with the stash, clears `b.turnEvents`, stops typing, signals `WaitForTurn`. `b.sessionEvents` is untouched.

**Idle-keyed turn completion (#813 successor, `complete.go`):** The turn boundary is CC's own `session_state_changed` running/idle SDK stream, which `lifecycle.go` enables by setting `CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS=1` in the subprocess env (opt-in in CC; a per-agent `backend_config.env` can override it for debugging). `running`/`idle` bracket CC's entire internal run loop — every ask cycle, every drained steer/follow-up/nudge, the background-agent wait, and the held-back-result flush — so **one foci turn == one CC run** and `idle` is the authoritative "no more results are coming" signal (probe-verified on the deployed CC: exactly one `idle` per run, after the last `result`, across steer-abort, mid-tool fold, background-task and `--resume`+hooks scenarios — `clutch/docs/steer-shadow-turn-design-option3.md` §Phase 3).

`result` events are per-internal-ask-cycle accounting, NOT turn boundaries: a `"now"`-priority message arriving mid-stream aborts the ask and mints an extra result (foci no longer sends `"now"`, but the machinery must tolerate it — TUI-side interrupts and future aggressive-steer would reintroduce it); a steer arriving mid-tool folds and mints none; results are withheld — and silently overwritten — while background `local_agent`/`local_workflow` tasks run (claude-code `print.ts` `heldBackResult`). The predecessor designs (per-steer counter, then init-herald + 45s activity watchdog) both tried to reconstruct the boundary from result counting and both failed in production: the watchdog was blind to long silent tool executions (79–122s builds) and force-completed live turns, re-opening the original #813 collision window. Idle-keying deletes that machinery outright (`foldPending`, `continuationExpected`, `sawFirstResult`, `heldResult`, the watchdog, `reArmForContinuation`).

- **Multi-cycle accounting.** `turnText`/`turnTools` span the whole run (reset only in `beginTurn`); output tokens accumulate across cycles (`turnOutputTokens`); input/cache/model are latest-wins (the final cycle's context fill — what compaction needs).
- **Pre-answer verification (`tryPreAnswerRedispatch`)** runs at idle on the final stashed result: a returned follow-up is sent as a fresh user message, `turnText` resets (the revision supersedes round 1), and `redispatchInFlight` holds the turn open — including across a stray idle before CC drains the follow-up — until the follow-up's own result + idle complete the turn.
- **Legacy fallback.** If CC has emitted no session-state events this session (`stateEventsSeen` false: env stripped, older binary), `OnResult` completes the turn directly (including the pre-answer gate) and warns once — the pre-idle-keyed behaviour, so nothing hangs.
- **Orphan runs.** Runs foci never opened a turn for (slash commands, task-notification runs after a background Bash finishes, proactive ticks) stash-and-drop: their text delivers via the always-live `SessionEvents`, and their `idle` no-ops.
- **Failure bounds.** A missed `idle` (unobserved in 4 probe retries after one flake in 8 runs) leaves the turn open until the next exchange folds in and its idle completes both, or the orchestrator's `streamIdleTimeout` releases it; reply text streamed live either way. Process death completes the turn via `finalizeExit` as before.

**Permission handling:** CC sends `control_request` with subtype `can_use_tool`. The backend first checks compiled auto-approve rules (`autoApprovePermission`). Unmatched requests are stored as `pendingPermission` entries and forwarded to the platform via `permPromptFn` (interactive buttons: Allow, Deny, Always Allow). The user's response is sent back as a `control_response` with either `PermissionAllow` or `PermissionDeny`. CC can also cancel a pending request via `control_cancel_request` (e.g. when a hook resolves it).

**Static permission pre-approval:** Both CC backends also pass an `--allowedTools` argv to the `claude` binary at launch. The rule list comes from merging global `[cc_backend] default_allowed_tools` with the agent's `[agents.backend_config] allowed_tools`. The merge happens in `cmd/foci-gw/agents_delegated.go` before calling `delegator.New`, so both backends read the final list from `cfg["allowed_tools"]` the same way. Factory default grants `Read/Write/Edit/MultiEdit(/tmp/**)` so agents can use the system scratch dir without a round-trip — see `internal/config/cc_backend.go`.

**`DelegatedManager.WaitForPermission`:** Before `RunInference` sends a new prompt to the backend, it calls `WaitForPermission` which blocks until all outstanding prompts are resolved. Uses `sync.Cond` with a context-cancellation goroutine (since `sync.Cond` doesn't natively support context). The drain hook installed via `Backend.SetOnPromptsCleared` (which routes through `OutstandingRegistry.SetOnEmpty`) signals the condition variable when the last outstanding prompt — permission, AskUserQuestion sequence, or MCP elicitation — is removed.

**ControlSender pattern (`delegator/control.go`, `ccstream/control.go`):** Generic runtime control for delegated backends. Three layers:

1. **Intent types** (`delegator/control.go`) — backend-agnostic request types (`SetModelRequest`, etc.) with a `ControlRequest` marker interface (unexported method prevents arbitrary types).
2. **`ControlSender` interface** (`delegator/backend.go`) — optional interface backends implement: `SendControl(ctx, ControlRequest) error`. The ccstream backend type-switches on intent types and translates to wire format.
3. **Agent routing** (`agent/delegated_control.go`) — `SendBackendControl(ctx, sk, req) (handled, err)`. Gets the backend via `DelegatedManager.Get`, type-asserts to `ControlSender`, calls `SendControl`. Returns `(false, nil)` if no backend or backend doesn't support it.

Catalogue-backed backends may additionally implement `delegator.ModelResolver`.
`Agent.SetModel` calls it before `SetModelRequest`, sends the backend-native ID,
and persists the resolver's developer-qualified canonical ID. Codex uses this
for exact or substring aliases; Claude Code keeps receiving raw aliases.

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

**Hook output path:** when CC fires the hook, it pipes an input JSON envelope (`tool_name`, `tool_use_id`, `tool_input`, `tool_response` / `error`, `agent_id`, ...) into `foci-cc-hook`'s stdin. The helper parses its own argv for `--install <id>`, reads the stdin envelope, truncates `tool_response` / `tool_input` / `error` to **`maxFieldBytes` = 4 KB** (`cmd/foci-cc-hook/main.go`) and writes a compact JSON object to stdout. Two independent constraints set that cap, the tighter winning: (1) each emitted stream line must stay under ccstream's 1 MB scanner limit — without a cap a multi-MB file read would blow the scanner and tear down the backend via `OnReaderStopped`; (2) foci's only consumer of `tool_response` is the tool-call display (a one-line result hint plus the "Show full" expansion, itself hard-capped at 4096 bytes/message in `formatToolCallWithResult`), and `tool_input` only feeds nudge matching / Agent-description extraction — so nothing foci renders or matches on needs more than ~4 KB. The cap was lowered from 64 KB (a scanner-safety margin) to 4 KB because the hook's stdout, captured verbatim by CC into `hook_success` attachments, was the single largest contributor to the on-disk CC session JSONL (~40% of the file); 4 KB satisfies both constraints and cuts that category by ~70%. CC captures that stdout and emits it as a `system/hook_response` message on its own stdout, where foci's reader picks it up.

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

**Typed ("Other") answers:** a pending ask is also keyed by session (`askState.bySession`, exposed via `AskRouter`, stored on `Agent.AskRouter`). `Agent.RunTurn` (`run_turn.go`, the platform-message path only) checks for a pending ask and routes a typed reply to `AskRouter.HandleResponse` instead of starting a turn. Gating on `RunTurn` (not the shared `HandleMessage`) ensures system injects (keepalive, reflection, `session_notify`) — whose Inject.Run closures call `HandleMessage` directly, bypassing `RunTurn` — are never mistaken for answers (they are additionally deferred behind a pending ask by the inbox worker, see "System Injections Never Steer"). A typed answer routes straight into `handleResponse`, which never touches the on-screen interactive message, so after recording each answer `handleResponse` calls the **`AskCloseFn`** hook (`tool_table.go` → `platform.CancelInteractiveMessage`) to edit that question's message shut (`✅ <answer>`) and drop its stale buttons. On the button path this is an idempotent no-op (the click already deleted the `imStore` entry and edited the message); on the typed path it is what makes the question visibly "close" — most noticeably for an **option-less** question, whose only button is Cancel and which can *only* be answered by typing. The per-question message id is `questionMsgID(requestID, idx)`, shared by present/reattach/close so all three address the same message.

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
9. maybeCompact: main threshold check → possibly compactor.Compact(sessionKey)
```

Messages are only saved to disk after the full turn completes (all tool loops resolved). Compaction runs after save; the automatic trigger is the main threshold (see below).

**Error handling by status code:**
- **429 (rate limit):** Could be burst rate limit or daily quota exhaustion. `classifyAPIError` emits a neutral request-limit signal and returns `"rate limited"`. Shared policy closes the endpoint gate using `Retry-After`, or exponential 1m→1h fallback when absent. System work queues behind the gate; user turns may probe it. A successful probe opens the gate immediately. No transport-level 429 retry.
- **529 (overloaded):** Anthropic servers are overloaded (their problem, not ours). Two-phase retry in `SendMessage`: phase 1 retries 3× with exponential backoff (2s→4s→8s, same as other retryable errors); phase 2 (529 only) enters an extended duration-based loop retrying up to ~2 hours with 5s base backoff doubling without cap. A cross-goroutine recovery signal on the `Client` wakes all sleeping retry loops when any `SendMessage` succeeds (proving the server has recovered). If still failing after phase 2, `classifyAPIError` returns `"API is overloaded (HTTP 529) — try again shortly"`.
- **500/502/503 (server error):** `SendMessage` retries 3× with backoff. If still failing, `classifyAPIError` fires `RateLimitFunc(0)` and returns a temporary unavailability message.

**Model fallback** (`[groups.fallbacks]`): `provider.Send` handles the full error recovery pipeline: (1) retry with backoff, (2) strip unsupported params (thinking/effort/speed) on 400 and retry, (3) walk the fallback chain on transient errors (529, 5xx, `context.DeadlineExceeded`). Each fallback hop resolves the model's endpoint/format via `ClientProvider.GetClient` and retries. On success, the response is used; subsequent tool-loop iterations rebuild with the primary model (fallback is per-request, not sticky). All API call sites use `provider.Send` — main agent loop, compaction, spawn one-shot, summary tool, auto-summary, and prompt-diff all have fallback support. Not triggered by 401 or 429. Configured via `[groups.fallbacks]` (global) and per-agent `[groups.fallbacks]` override. Max chain depth: 3.

### Cache Stability Invariant

Conversation history sent to the API must be a strict append-only extension of the previous request — inserting a message in the middle invalidates all cached tokens after that point. `HandleMessage` enforces this via a per-session turn lock that serializes all callers (Telegram, `AsyncNotifier`, scheduled wakes, HTTP `/send`). Different sessions run concurrently. See [CACHING.md](CACHING.md) for the full cache stability contract.

### OpenCode Backend (`internal/delegator/opencode/`)

The opencode backend drives OpenCode as a coding agent via its HTTP server API. Unlike ccstream (one subprocess per session with NDJSON over stdin/stdout), opencode runs **one `opencode serve` subprocess per foci agent**, shared across all of that agent's sessions. The Backend is its HTTP/SSE client. Registered as `"opencode"` via `delegator.Register` in `init()`.

**Architecture — Server vs Backend split:**

- **`Server`** (one per agent, package-level pool keyed by agentID): owns the `opencode serve` subprocess, the `*http.Client`, and a single SSE subscriber goroutine reading `GET /event`. Refcounted — spawned lazily on first `acquireServer`, killed when the last `releaseServer` hits zero. Batch one-shots (`RunBatch`) also spawn through `acquireServer` when nothing is pooled (background consolidation/nudge no longer hard-fails with "no running server"), and `releaseServer` on completion like any holder — so the server survives only while an interactive session still needs it, and a sole-holder batch's server is reaped when the run ends (no refcount leak, no server pinned open). Bounded-shutdown Close mirrors ccstream's kill-ladder (POST /instance/dispose → SIGTERM → SIGKILL → abandon).
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
| `message.part.updated` (tool, task, metadata.sessionId) | `trackTaskTool` (subscriber) | records `childToCallID[childSID] = callID` |
| child session `message.part.updated` (text, complete) | `route` → `handleChildEvent` | `OnSubagentText` (grouped by callID) |
| `message.updated` (assistant) | `onMessageUpdated` | Store `lastModel`/`lastUsage` (no callback) |
| `session.idle` | `onSessionIdle` | `OnTurnComplete` + flush `steerBuf`; during an abort drain, counts burst idles and flushes the buffered steer once settled (see Steer divergence) |
| `session.status` (busy) | `onSessionStatus` | `typingFunc(true)` |
| `session.status` (retry: usage/rate limit) | `handleRateLimitRetry` | Parse reset → `Agent.EngageRateLimit` callback → POST `/abort` → complete waiting turn |
| `session.compacted` | `onSessionCompacted` | `onCompactionDone(0)` + close `compactDoneCh` |
| `session.error` (ProviderAuthError) | `onSessionError` | `fanOutAuthFailure` |
| `session.error` (MessageAbortedError) | `onSessionError` → `failInFlightTurn` | completes the aborted turn (steer abort-drain turn 1) |
| `permission.updated` | `onPermissionUpdated` | `permPromptFn` (Allow/Deny/Always) |
| `permission.updated` (type:question) | `handleQuestionPermission` | `permPromptFn` (option buttons) |
| `permission.replied` | `onPermissionReplied` | cancel-listener fanout |

**Divergences from ccstream:**

- **Steer abort-drain (opencode 1.17.11):** opencode has no mid-turn fold queue (CC's `priority:"now"` has no equivalent). Empirically, a mid-turn `prompt_async` is queued behind the active turn, and `POST /abort` **discards** that queue (a turn sent before/during the abort is lost; a turn sent after survives). So a `SourceSteer` arriving mid-turn buffers in `steerBuf`, calls `Interrupt` (POST /abort) to kill the active turn, then — once the abort's event burst drains (`session.error:MessageAbortedError` + 2× `session.idle`, the empirically observed signature) OR a 500ms backstop timer fires, whichever comes first — flushes the buffered steer as a fresh follow-up turn via `flushSteerBuf`. A second steer during the drain just appends (one abort; the flush combines them). A premature-completion watchdog (`onSessionIdle` / `failInFlightTurn`) `Warnf`s if a turn ends with no text/tools outside a drain, except for `MessageAbortedError` (manual stop / steer abort — always deliberate, never anomalous). Backend fields (turnMu-guarded): `aborting`, `abortIdlesSeen`, `abortTimer`, `abortDrainTimeout`. The follow-up uses a nil TurnEvents (no `OnTurnComplete`); text arrives via `SessionEvents.OnText`.
- **Question tool:** opencode's built-in `question` tool surfaces as `permission.updated` with `type:"question"`. Metadata carries the question schema (header, text, options). `RespondToQuestion` POSTs the option label or typed text.
- **Plan delivery:** Uses the prompt body's per-request `agent:"plan"` field (no `PATCH /config`, no swap-back). Simpler than ccstream's `EnterPlanMode` turn.
- **No PostToolUse hooks:** opencode emits tool parts directly on its event bus — no external hook-helper binary needed.
- **Subagent content via child sessions:** opencode spawns a child session for each Task tool call. CC surfaces subagent text via `ParentToolUseID` on assistant messages in the same stream; opencode puts it on a separate child session. The subscriber's `route()` learns child→parent links from `session.created` (`childToParent` map) and child→callID links from the Task tool part's `state.metadata.sessionId` (`childToCallID` map, set by `trackTaskTool`). Child `message.part.updated` text events are rerouted to the parent Backend tagged with `childCallID`, where `handleChildEvent` fires `OnSubagentText(callID, text)` without touching parent turn state — mirroring ccstream's `ParentToolUseID` guard. Only completed text parts are surfaced (no streaming deltas yet).
- **No elicitation:** opencode's MCP client doesn't advertise the elicitation capability (commented out, issue #23066). `ElicitationResponder` not implemented.
- **Shared-server auth fanout:** A `ProviderAuthError` on one session fans to all Backends on the same Server (account-wide). `fireAuthFailure` is CAS-gated per-Backend (fires once per lifetime).
- **Usage-limit cancellation:** OpenCode reports rejected account limits as `session.status` retry events rather than `session.error`. The backend distinguishes usage/rate-limit messages from transient retries and emits a neutral usage signal, trusting reset timestamps only when they carry an explicit UTC offset (Z.AI currently sends a timezone-less wall clock). Shared policy supplies the one-hour fallback. The backend synchronously POSTs `/abort` and completes the waiting turn; aborting before completion prevents a replacement turn from consuming delayed abort events from the limited turn.
- **No automated relogin:** `/login` reports "unavailable" for opencode agents. Auth recovery is per-provider (`opencode auth login <provider>`).
- **System-prompt suppression via plugin (`blank_system.go`):** opencode injects its own default system prompt ("You are opencode…") on every turn. A seeded TypeScript plugin (`.opencode/plugin/blank-system.ts`) hooks `experimental.chat.system.transform` to replace the entire system array with foci's resolved character prompt, read from `{tempdir}/session-system/{sessionID}`. `acquireServer` calls `EnsureBlankSystemPlugin` just before spawning the subprocess (plugins load at boot; ensured at the single spawn chokepoint so batch and interactive spawns are wired identically), and `Backend.Start` calls `WriteSessionSystemFile`; `Close` removes the file. foci also sends the prompt in the POST `"system"` field as a fallback. Replaced a broken named-agent approach (`.opencode/agents/foci.json`) — opencode only scans for Markdown agents, so the JSON never registered.
- **Internal compaction with foci's prompt:** opencode's own `/summarize` compaction follows foci's `compaction-summary.md` format via a second hook in the same plugin (`experimental.session.compacting`). `Backend.Start` writes the resolved compaction prompt (same `CompactionSummaryPromptPath` resolution as CC) to `{tempdir}/session-compact/{sessionID}`; `Close` removes it. Unlike the system prompt, no fallback — a missing file leaves opencode's default compaction template.
- **Server-death respawn:** When the shared subprocess dies unexpectedly (SIGTERM, subscriber EOF), `finalizeExit` clears the Server's `running` flag, evicts it from the pool, and synthesizes `session.error` for each registered Backend. `Backend.IsRunning()` now checks `running && srv != nil && srv.isAlive()` — previously it returned only `b.running`, which `finalizeExit` couldn't reach, so `getOrCreate` handed back the stale Backend forever. Now the next turn detects the dead server, saves the resume ID, closes the corpse, and `acquireServer` spawns a fresh subprocess (re-ensuring both plugins itself, at the spawn chokepoint).

**Lifecycle:**
1. `Start`: acquireServer (lazy pool; **ensures the shell.env + blank-system plugins before spawn**), POST /session, **write per-session env mapping** (see below), registerSession (starts dispatcher), inject system prompt (noReply:true), PATCH /config for default_permission.
2. `ImmediateInject(SourceUser)` at idle: beginTurn + POST /prompt_async.
3. SSE events arrive → Server.route by sessionID → Backend.dispatchLoop → handleEvent → SessionEvents/TurnEvents callbacks.
4. `session.idle`: build TurnResult from accumulated state, fire OnTurnComplete, flush steerBuf.
5. `Close`: **remove per-session env mapping**, unregisterSession (stops dispatcher), DELETE /session/:id, releaseServer (refcount-- → shutdown if zero).

**Per-session exec-bridge env routing (`session_env.go`):** The shared-server model pins the subprocess env (including `FOCI_SOCK`/`BASH_ENV`) to whichever session launched it first — all subsequent sessions inherit the first session's bridge socket, misrouting session-scoped exec-bridge tools (`foci_ask`, `send_to_session`). The fix uses opencode's `shell.env` plugin hook: before every bash spawn, opencode fires `Plugin.trigger("shell.env", {sessionID}, {env:{}})`, and a foci-generated plugin (`.opencode/plugin/foci-session-env.ts`) reads a per-session JSON file from tempdir and injects the correct `FOCI_SOCK`/`BASH_ENV`. `Backend.Start` writes `{tempdir}/session-env/{sessionID}.json` with the bridge env from `opts.Env` (the `foci-session-env.ts` plugin itself is ensured inside `acquireServer` before spawn, so every spawn path — interactive or batch — loads it); `Backend.Close` removes it. The plugin is idempotent (same content every time) and self-locating (`import.meta.dir + "/../session-env"`). ccstream is unaffected — each session has its own subprocess and its own bridge baked into the process env.

## Message Metadata

**Message transforms** (`[[message_transforms]]` in config) run regex find/replace on inbound user messages. Transforms fire before command dispatch — if a message is already a recognized command, transforms are skipped. If transforms produce a command (e.g. `s` → `/status`), it is dispatched as one. Rules run in sequence; each rule's output becomes the next rule's input.

Each user message then gets a header prepended (NOT in system prompt — that would bust cache), rendered by the configurable **statusline template** (`internal/agent/statusline.go`, #831 — see also the State Dashboard note under Task List and the pause/resume note under Ask). The default template (`DefaultStatuslineTemplate`) produces:

```
[meta] time=2026-02-21T05:30:00Z gap=3h12m model=claude-haiku-4-5 via=telegram
[state] tasks: 2/5 → first active, todos: 3 open
[ask] ⏸ ask q1 paused — user replies routing to you as normal turns, not answering it (/resume to restore)
```

`[state]` and `[ask]` are conditional lines that self-omit (statusline rule 3) when every placeholder they contain renders empty, so on a fresh session with no tasks/todos/scratchpad and no paused ask, only `[meta]` appears.

- `time` — the time the user's message was received at the platform boundary, not the time the turn was composed. Stamped in `toPlatformMessage` as `QueuedMessage.ReceivedAt` (Telegram: `msg.Date`; Discord: `msg.Timestamp`) and threaded through `agent.WithReceivedAt(ctx, …)` → `TurnState.ReceivedAt` → `composeTurnText` so queued or steered messages show the user's send time rather than the drain/inject time. Falls back to wall clock for system-initiated turns with no platform receipt.
- `gap` — human-readable time since previous message ("3h12m", "2d4h", "38s", "none"). Computed from `time` minus `sessionMeta.lastMessageTime`, which is updated to `TurnState.UserMessageTime()` so gaps also measure user-send-to-user-send rather than inject-to-inject.
- `model` — current model name (e.g., "claude-haiku-4-5", "claude-opus-4-6")
- `via` — transport that delivered the message. Derived from the context trigger via `triggerToPlatform()` in `context.go`. Values: `telegram` (Telegram/voice), `discord` (Discord), `android` (Android app), `api` (HTTP /send), `cron` (system-initiated: keepalive, wake, scheduled, etc.)

**`{cost}` / `{tokens}` (prev turn's cost and token breakdown) exist as statuslineFields but are deliberately NOT in the default template** — surfacing a running cost/token figure on every turn was found to nudge the agent toward rationing its own budget, an undesired behaviour (removed 2026-07). An agent can still opt in via a custom `statusline` config (`docs/CONFIG.md`); the bare `{cost_raw}`/`{tokens_in}`/`{tokens_out}`/`{cache_read}`/`{cache_write}` fields are also available for custom templates.

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
| `internal/agent/turnevent` | `BufferSink`, `NopSink` | Leaf package: event types, Sink interface, context helpers, and pure-utility sinks. No platform or turn deps. |
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

The Steerer parameter, supplied by the agent worker, returns just the text fields of buffered steer entries — mid-turn injection on the API path (`steerBlocks`) never renders a new meta header, so it discards receipt timestamps. The post-turn orphan-drain loop (when a turn finishes and per-session worker rebuilds leftover steers as a follow-up turn) reads `SteerEntry.ReceivedAt` from the inbox so the follow-up turn's meta header reflects the original user send time rather than the drain time. Note: CC-backed agents bypass the buffer entirely via `agent.Inbox`'s `Backend.ImmediateInject(SourceSteer)` routing; the buffer only services API-mode agents and the orphan-drain fallback.

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
- **Injected turns** (`cmd/foci-gw/agents_notify.go → deliverToSessionChat`): build `turn.NewSessionSink(conn, sessionKey, trigger)`, attach, call `HandleMessage`. SessionSink owns its own delivered flag so intermediate text and final text don't double-deliver.
- **Cross-session notify** (`agents_notify.go → newSessionNotifyFn`): same as injected turns — `SessionSink` routing through `conn.SendToSession`.
- **Async notify with response routing** (`agents_notify.go → newAsyncNotifier`): `BufferSink` captures the target session's final text, then the response is routed back to the caller's session via `deliverToSessionChat`.
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

Interactive platforms supply a `Steerer` indirectly: `agent.driveAndDrainOrphans` constructs the steerer from the inbox's steer buffer and passes it to `Agent.RunTurn`, which forwards it to `turn.RunTurn`. The agent drains steers via `steerBlocks(ctx)` at tool-loop boundaries on the API path. The delegated path bypasses the steerer for mid-turn injection — `agent.Inbox.Enqueue` calls `Backend.ImmediateInject(ctx, Inject{Source: SourceSteer, Text: ...})` directly when a steer arrives during an in-flight CC turn. In the opencode backend ImmediateInject triggers the abort-drain sequence (Interrupt → drain the abort burst → flush the buffered steer as a fresh turn); in ccstream it folds via priority `"next"` at the next tool boundary.

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

**Key format:** `{agentID}/{type}{id}[/{childType}{childTS}]` — a **stable
identity**; compaction and `/reset` never change it.

**Type codes:**
- `c` — chat (Telegram/Discord/app, external stable ID; deterministic key `agent/c<chatID>`)
- `i` — independent (named `agent/i<name>` or anonymous `agent/i<ts>`)
- Child types: `b` (branch), `i` (independent spawn)

**Key → Path mapping:**
```
Root sessions:   {key}/root.jsonl
Child sessions:  {key}.jsonl

Examples:
main/c123               → sessions/main/c123/root.jsonl
main/c123/b1709596800   → sessions/main/c123/b1709596800.jsonl
main/iresearch          → sessions/main/iresearch/root.jsonl
```

**Compaction / reset:** in-place archive rotation. Compaction
(`SessionWriter.Replace`) renames `root.jsonl` → `root.{timestamp}.jsonl` and
writes the compacted messages to a fresh `root.jsonl`; `/reset` (`Store.Reset`)
archives the same way and lets the next Append recreate the file. The session
key — and everything holding it (chat metadata, reminders, tmux ownership,
in-flight maps, app conversation bindings) — is unchanged, so there is no
rotation-migration machinery anywhere. Per-session state is cleared explicitly
on reset by `Agent.ClearSessionState`.

**Branching:** Branch files start with a `{"type":"branch_meta",...}` line containing `parent_key` and `branch_point`. `LoadFull()` reads parent[:branch_point] + branch's own messages, recovering the prefix from the parent's newest archive if the parent was compacted/reset after the branch was created (P2-5). This is what makes cache sharing work — the API sees the same prefix bytes — and what lets `/reset` archive a session while its reflection branch still sees the full history.

**See also:** [SESSION_KEYS.md](SESSION_KEYS.md) for complete format specification and API reference.

## System Prompt Assembly (`workspace/bootstrap.go`, `agent/agent.go`)

System blocks are assembled in this order:

1. **Environment block** — programmatically built from config values, **per session**: API agents via `Agent.EnvironmentBlockFunc(sessionKey)` (wired in `agents.go`, cached per session in `buildSystemBlocks`), delegated agents via the env portion of `StartOptions.SystemPromptFunc(sessionKey)` (wired in `agents_delegated.go`). Contains workspace path, agent ID, platform URL, messaging platform list, config/log paths, message metadata docs, session structure, a `## Backend` section from `backend-<name>.md`, and a `## Platform` section from `platform-<name>.md` keyed to **this session's** messaging platform. The platform is resolved from the durable chat claim (`platformForSession` → `SessionIndex.PlatformForChat` — an identity lookup, deliberately not `ForSessionOrPrimary` connection routing), so the block is deterministic across startup transients, branch keys resolve identically to their parents (byte-identical prompts → cache sharing, see CACHING.md), and chat-less named sessions get no `## Platform` section. Omitted entirely when `[environment] enabled = false`.

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

`internal/modelcaps` is a leaf cache of `Caps{ContextWindow, MaxOutput, Effort, Thinking}` advertised by backend catalogues (Anthropic `/v1/models`, Codex app-server `model/list`). It is the live layer between the per-model config override and the static `modelinfo` registry: context-window and effort lookups prefer it, falling back to the static registry on a cold/empty cache so behaviour is never worse than before.

- **Per-backend registry.** Capabilities are a property of the backend *type*, not the model alone, so the cache keeps separate stores for `BackendCCStream`, `BackendAPI`, `BackendCodex`, and any other delegated backend name. `BackendKey(configBackend)` maps configured transport names to those keys. Public API: `LookupFor`, `ModelsFor`, `SetFetcher`/`Refresh` for pull catalogues, and `Publish` for catalogues discovered by a live backend instance. Background pull refresh is single-flight and serve-stale.
- **Fetcher seam.** `anthropic.FetchModelCaps` (raw `GET /v1/models`) is injected via `SetFetcher` so the package stays a DB/anthropic-free leaf. `AnthropicResolver.ModelCapsFetcher` supplies it from CC OAuth creds; nil creds → no fetcher → static fallback.
- **Codex publisher and resolver.** After each app-server initialize handshake, the Codex backend pages through visible `model/list` results. It preserves catalogue order and each model's ordered `supportedReasoningEfforts`, enriches omitted structural fields from exact `modelinfo` entries, then publishes the complete snapshot under `BackendCodex`. `backend_config.model` and `/model` accept exact IDs or case-insensitive substring aliases; exact wins, otherwise numeric version components rank matches newest-first with catalogue order as the tie-break. Fresh sessions send the resolved ID in `thread/start`; resumed sessions and runtime overrides send it in the next `turn/start`. Foci persists `codex/<id>`, while the wire receives the bare ID. `/effort` accepts the exact advertised levels.
- **DB persistence (`e301379b`).** `SetPersister` + `Restore` bridge the cold-start gap for API, Claude Code, and Codex. `session` stores the shared primitive shape in `model_caps`; saves are transactional (delete+insert) so a reader never sees a half-written catalogue. `cmd/foci-gw/modelcaps_persist.go` adapts `SessionIndex`↔`Caps`. Both fetched and published snapshots persist outside the store lock; `Restore` declines to clobber a cache a live result already populated.
- **Agent routing.** `Agent.BackendType()`, `Agent.ModelCaps(model)`, and `Agent.BackendModels()` route caps reads through the agent's own backend; consumers (session context limit, command context-limit resolver, `/effort` choices, `/model` keyboard) read via the agent. Compaction takes an injected `ModelCapsFn` bound to the agent's backend.

**Effort plumbing.** `/effort`'s level set is resolved per call: `newSessionSettingCommand`'s optional `DynamicChoices` hook reads `modelcaps.LookupFor`, building levels in catalogue order (e.g. opus-4-8: low/medium/high/xhigh/max) with matching numeric aliases; a catalogue miss falls back to the static low/medium/high. Two delivery paths make effort both instant and durable:
- **Live push (`39581989`).** `Agent.SetSessionEffort` persists, then for a delegated session fires `delegator.ApplyFlagSettingsRequest{Settings: {"effortLevel": value}}` in the background → ccstream's `SendControl` emits `{"subtype":"apply_flag_settings","settings":{...}}` so the next turn runs at the new effort with no bounce (mirrors `SetPermissionMode`'s optimistic fire-and-forget). The command layer must reject invalid settings first — CC does not validate. API-loop sessions apply effort at turn time via `output_config`, so no control is sent. `clear`/`off` skip the live push.
- **Cold-launch flag (`1aacc877`).** `apply_flag_settings` is session-local: a bounce (post-compaction reload, idle respawn) drops the override. `StartOptions.Effort` + `EffortFunc(sessionKey)` (mirrors `SystemPromptFunc` — resolved fresh per session start in `getOrCreate`, bound to `ag.SessionEffort`) make ccstream `Start` append `--effort <level>` (empty/`off` omits it). The control is the happy path; the launch flag is the backstop.

**`/thinking` backend gate (`22b3fa19`).** CC exposes no thinking control and effort subsumes it, so `/thinking` is hidden on ccstream via a backend-keyed `BackendGate` on `sessionSettingDef` (distinct from the model-keyed `Capability`), consulted in both `Visible` (hide) and `Execute` (reject). API agents keep it.

**`/model` keyboard (`275b492a`).** `/model`'s `KeyboardOptions` now offers one button per model `Agent.BackendModels()` (→ `modelcaps.ModelsFor`) advertises, marking the current model with a check. A cold catalogue falls back to typing the name.

**Keepalive:** For Anthropic endpoints, the keepalive fires on a configurable interval (default 55m, just under the 1h cache TTL). For OpenAI and DeepSeek models, keepalive is auto-detected by developer name via `config.ResolveModelKeepalive()` — these developers have a 5-minute prompt cache TTL, so keepalive fires every ~4m45s. Gemini's `CacheManager` handles its own TTL extension independently.

**Per-session warm window (`keepalive.go` `keepaliveTargets`).** Keepalive is gated per candidate session, not by a single agent-wide timer: a session is warmed only if its `session_index.last_cache_touch` (stamped at turn entry on every non-memory turn by `recordTurnActivity`, then advanced per round mid-turn by `touchTurnActivity` — see "Mid-turn activity heartbeat" below) is in the window `[interval, cacheTTL)` — due for a refresh but not yet expired. A session with **no** recorded touch (never warmed, or just reset — `ClearSessionState` nulls it via `SessionIndex.ClearCacheTouch`) is skipped: there is no live cache to keep alive, so keepalive won't fork/inject into a cold session. `cacheTTL` is the backend's static constant (`DelegatedManager.StaticCacheTTL()`, CC = 1h), resolved once at setup; `0` = unknown → interval-only gate. `setupPeriodic` warns at startup if `interval >= cacheTTL` (empty window → warming can never fire). This replaced the former in-memory agent-wide `lastCacheWarmed` field (removed) — `last_cache_touch` is the persisted, per-session source of truth, so the `onTurnComplete` lifecycle hook that fed `lastCacheWarmed` is now nil.

**Client cache-warmth indicator (#1217).** The app's `cacheExpiry` frame is now driven by the same `last_cache_touch`, not by turn-complete. `Agent.CacheExpiry` returns `last_cache_touch + TTL` (cold/zero when there's no touch), and `Agent.emitCacheExpiry` pushes it via the `onCacheExpiry` hook (wired to `app.SetCacheExpiry` → `activeHub.bindingForSession(...).setCacheExpiry`, deduped) on **every** touch write: `recordTurnActivity` (turn entry), `touchTurnActivity` (per-round mid-turn heartbeat — see below), `TouchRootCacheForBranch` (branch warms root), and `ClearCacheTouch` (reset → cold). Previously the frame was emitted only by the app sink at `TurnComplete`, so a session kept warm by keepalive **forks** (which complete off-root) or by external/cron turns (no live app sink) advanced `last_cache_touch` but never refreshed the client — the indicator went stale and showed "expired" while the cache was actually warm. The persistence-hook path fires regardless of whether a live app socket is attached (the durable binding's `cacheExpiryMs` seeds the roster snapshot on reconnect).

**Mid-turn activity heartbeat (`touchTurnActivity`, `sink_logging.go`).** `recordTurnActivity` writes the timestamps once at turn entry; to keep them fresh through a long turn, `loggingSink` (the universal per-turn sink wrapper) also calls `Agent.touchTurnActivity` on each **round** event — `TextBlock`, `ToolResult`, or `Activity` (emitted by both the API tool loop and the delegated ask-cycle path via the shared `emit*` helpers). It advances `last_cache_touch` (always) and `last_activity_at` (unless `isMemoryTrigger`), never `last_user_activity_at`; debounced to ≤1 write / 20s / turn and gated on `IsTurnInFlight` (so the wrapper reused for post-turn late delivery no-ops). It does not recapture `prevRequestTime` (a per-turn-entry concern). Separately, the `/send` activity gate consults an in-memory turn-**end** signal, `Agent.LastTurnEnd` (stamped in `markInFlight`'s decrement when `inFlight`→0), so `--wait-cold`/`--if-cold` measure continuous dead time — no turn running now AND none finished within the window — rather than releasing into the gap between back-to-back turns.

## Anthropic API Client (`anthropic/`)

Implements `provider.Client` and `provider.StreamingClient`. Uses the official `github.com/anthropics/anthropic-sdk-go` SDK.

**Transport:** `sendOnce()` sends requests via the SDK's `Messages.New()`. Same pattern for `CountTokens` and `ListModels`. The transport is wrapped by two-phase retry logic: Phase 1 (3 retries with exponential backoff on 500/502/503/529) and Phase 2 (extended overload recovery with cross-goroutine signaling on 529). The SDK client is initialized lazily (`sync.Once`) and configured with `WithMaxRetries(0)` since retry logic is handled externally.

**Translation layer** (`translate.go`): converts between provider-neutral types and SDK types at the boundary. `buildSDKParams()` translates `MessageRequest` → `MessageNewParams`. `responseFromSDK()` translates back. `classifySDKError()` maps SDK errors → `provider.APIError`. Custom tools use typed SDK fields; server tools and documents use raw JSON passthrough via `param.Override`.

**Streaming** (`stream.go`): `StreamMessage()` wraps `streamOnce()` with the same two-phase retry logic. Pre-stream errors (before any deltas) are retried; mid-stream errors are not (deltas already emitted). `streamOnce()` calls `Messages.NewStreaming()`, iterates events, fires `StreamHandler.OnTextDelta` / `OnThinkingDelta` callbacks, uses `Message.Accumulate()` for response assembly. Enabled per-agent via `streaming = true`.

Two clients (two token types — see [docs/AUTH.md](AUTH.md)):

1. **Client** (`client.go`) — messages API + token counting + streaming
   - Sends model requests with system prompt + conversation history
   - Also handles `/v1/messages/count_tokens` for `/context` command
   - Supports static token (`NewClientWithTimeout`) or dynamic token func (`NewClientWithTokenFunc`)
   - Per-request auth via `option.WithAuthToken(token)` (SDK path) or manual header (raw path)
   - Sets `anthropic-beta: oauth-2025-04-20` header for OAuth token auth

2. **CCTokenSource** (`cctoken.go`) — Claude Code credential reader
   - Reads `~/.claude/.credentials.json` lazily on each `Token()` call (no polling)
   - Never refreshes tokens itself — only reads what Claude Code writes
   - If token is expired on read, triggers background refresh (runs `claude`) and returns error
   - `CheckRefresh()` triggers proactive refresh when token is within `cc_expiry_threshold` (default 5m) of expiry
   - Provides `Token()` func used by Client via tokenFunc

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
| `send_to_chat` | telegram.go | Send proactive Telegram messages (text, documents, voice notes). With `send_as="voice"` and text (no file_path), synthesizes speech via TTS. Routes to the chat extracted from the session key (`X/c{chatID}`) so per-chat sessions get messages to the correct user. Falls back to bot's default chat when no chat ID in session key. |
| `send_to_session` | session_send.go | Inject a user-role message into another session. Tags the message with `[Message from session ...]` origin header. Appends to session store and triggers processing via `AsyncNotifier`. Used for cross-session communication (e.g. facet branches talking to main). Target accepts a full session key (`scout/c123`, `scout/iresearch`), an agent-qualified session name or chat alias (`scout/research`), or a bare agent name (`scout` → default session) — loose targets resolve through the route.Resolver ladder (create disabled). |
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

Messages starting with `/` are intercepted at the platform router level (Telegram, Discord — via the shared `dispatch` package, see Command Dispatch Architecture above) before reaching the agent. They execute immediately — never queued behind an in-flight agent turn.

**Dispatch flow:** Platform message → auth check → if `/`: `dispatch.Dispatcher.DispatchText()` → `registry.Dispatch()` → execute → reply. Never touches agent session or message history.

**Two types:**
1. **Built-in** (code-defined across `command/*.go`, one constructor per command; `builtins.go` is only a subset — `ping`, `repeat`, `facet`, `tmux`, `agents`): registered in `registerAgentCommands()` (`cmd/foci-gw/commands.go`), that function is the ground truth for the exact current set. As of this audit: `/ping`, `/status`, `/cache`, `/last`, `/cost`, `/context`, `/mana` (alias `/usage`), `/reset`, `/model`, `/effort`, `/thinking`, `/speed`, `/mode`, `/display`, `/overrides`, `/tools`, `/config`, `/prompts`, `/log`, `/errors`, `/version`, `/help`, `/compact`, `/restart`, `/secrets`, `/bitwarden`, `/sessions`, `/agents`, `/android`, `/pair` (aliases `/pair-key`, `/pairkey`), `/repeat`, `/pass`, `/todo`, `/misc`, `/stop` (+ configurable aliases like `/wait`), `/login`, `/done`, `/facet`, `/branch` (see Wake/Branch section) — plus conditionally-registered ones: `/plan` (iff the backend contributed a plan delivery), `/tmux` (iff the tmux tool is wired), `/pause`/`/resume`/`/complete` (iff `AskRouter` is set). `/uptime` and `/voice` (as a mode-toggle command) no longer exist — voice config moved to `[[tts]]`/`[[stt]]` arrays with no mode command (`7bca02b4`); `/session` is now plural `/sessions` with `list`/`default`/`info`/`index` subcommands.
   - `/login` (`RequiresBackend`, ccstream only) — manually trigger the automated CC re-login flow (see [Automated CC re-login on 401](#backend-session-lifecycle)); URL returns to the chat that ran it
   - `/pass` — forward a command directly to the delegated backend (e.g. `/pass /context`, `/pass /model opus`). Bypasses foci's command dispatch so CC slash commands that would otherwise be intercepted by foci can be sent through. For tmux backends, captures and returns pane output after stabilisation. For stream backends, output arrives normally via the stdout reader. Only available for delegated agents — returns an error for API-mode agents.
2. **Custom** (script-defined in `foci.toml` via `[[commands]]`): runs a shell script, returns stdout. Timeout default 10s.

**`/model` endpoint switching:** Accepts `endpoint:developer/model_id` syntax (e.g. `/model gemini:google/gemini-2.5-flash`, `/model openrouter:anthropic/claude-opus-4-6`). The Execute function calls `config.ResolveModel()` to parse the `developer/model_id` string and `cc.ClientProvider.ResolveEndpointClient(endpoint, format)` to lazy-init the correct client. Calls `cc.Agent.SetModel()` — the orchestrator that sends a `set_model` control request to the delegated backend (if any) and, only once that's confirmed (ccstream now waits for CC's `control_response` instead of firing-and-forgetting; a rejected model id surfaces the error and leaves metadata untouched), updates foci's session metadata. Sets `modelUserSet` flag to prevent `UpdateSessionMeta` from clobbering the user's explicit choice with the backend's reported model.

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

Steer routing moved out of `MessageQueue` and into `agent.Inbox.Enqueue`: mid-turn text-only messages are routed to the per-session steer buffer (API agents) or dispatched directly via `Backend.ImmediateInject(SourceSteer)` (CC agents) inside the agent layer, without the platform layer needing to know.

The receiver never blocks on the agent. Slash commands (including `/stop`) execute immediately on the receiver goroutine. Agent messages fan out by session key via `agentMessagePump` → `agent.Enqueue`; per-session workers in `agent.Inbox` serialize turns within a session. Different sessions on the same bot run their turns in parallel.

**Stale command filtering:** Slash commands older than 30s are silently dropped. Safety net for update replay after crashes — prevents stale `/reset` or `/stop` from firing on restart.

**Shutdown ack:** On context cancellation, each bot's poll loop fires one final `GetUpdates` with the last processed offset. This acknowledges processed updates to Telegram, preventing replay on restart. `BotManager.Wait()` blocks main after `cancel()` to ensure all bots complete this ack before process exit.

**Wizard routing (`WizardHandler`, `internal/command/wizard.go`):** Interactive wizards (e.g. `/agents new`, `/secrets set`, `/android`) take over message routing via `Registry.HandleMessage(scope, text)`. Wizards are **scoped by session key** — the interceptor passes `SessionKeyFn()`, command activations pass `req.SessionKey` — so each conversation runs its own wizard and traffic in one session can never advance another's. While a scope's wizard is active, ALL of that scope's messages (including non-`/` text) are intercepted by the receiver goroutine before reaching slash command dispatch or the agent queue. `/cancel` and `/stop` abort it; it clears automatically on completion (`done=true`). **Persistence:** wizards implementing `command.WizardSnapshotter` (all five built-ins do) are checkpointed to the session index (`agent_metadata` key `wizard_pending`, mirroring the ask tool's `ask_pending`) on every mutation and restored at startup by `Registry.RestoreWizards` (wired in `cmd/foci-gw/commands.go`; 24h TTL), so a restart no longer drops a mid-flow wizard.

**Wizards on the native app (out-of-band, `internal/app/wizard.go`):** The FAP path never routes composer text into wizards. Instead, for clients that advertised the `"wizard"` capability, `dispatchCommand` detects a wizard activation (a `Registry.WizardGen(scope)` change across `Dispatch`) and opens a hub-side `wizardSession`, sending the prompt as a structured `wizard.step` frame (suppressing the plain-text render). Answers come back as `wizard.response` frames — `qa:<i>` resolves to the option label via `internal/question`, `qa:cancel` maps to `/cancel`, anything else is passed verbatim — and are fed into `Registry.HandleMessage`; the reply becomes the next `wizard.step` or a terminal `wizard.end` (`done`/`cancelled`/`expired`). Wizards may expose structured steps (buttons) by implementing `command.WizardStepProvider` (`PendingStep() *question.Question`; `agentWizard` is the pilot); others fall back to free-text steps. A `WizardDocProvider` file (the `/android` QR) is staged as a blob and referenced inline from the next step's `media` (in-chat `media` frame fallback when the wizard just ended). Session staleness is guarded by stepId echo + the generation snapshot (a wizard replaced from chat in the same session expires the app session). App sessions persist alongside the Registry's wizards (`agent_metadata` key `wizard_app_sessions`) and are re-linked at `setupAgent`, so responses keep routing across a server restart. Wire contract: foci-android `docs/01-wire-protocol.md` §12. Uncapable clients keep the legacy plain-message behaviour.

**`/android` — native Android onboarding wizard (`command/android_onboard.go`):** `AndroidCommand()` + `androidWizard` walk the user through pairing a device. `Execute` branches on state: app provider disabled → offer to enable (appends `[[platforms]] id="app"` to foci.toml via `appendToFile` and generates `app.api_key` with `secrets.GeneratePassphrase(5)`); enabled + auto-generated key → offer to reveal the key in chat or point at `secrets.toml`; enabled + user-set key → skip to host. Auto-generated detection uses `secrets.IsGeneratedPassphrase` (all-EFF-wordlist hyphenated tokens — no stored marker). The host step emits a `foci://pair?host=…&key=…` string the Android client's `parseQr` accepts. If the wizard enabled the app provider this run (`justEnabled`), it then runs a restart-confirm step — the running server loaded its config before the `id="app"` line was appended, so the `/app` endpoints stay dark (every request 403s on the global auth middleware) until a restart; on `yes` it calls the shared `restartFunc` (same hook as `/restart`). Reads the key via `SecretsStore.Get` (added to the interface); registry handle comes from `cc.AndroidDeps.Registry`. No agent LLM involved — a pure Go wizard like the others.

**Attachment handling:** Photos (`msg.Photo`, largest size selected), image documents (`msg.Document` with image MIME type), and PDF documents (`msg.Document` with `application/pdf` MIME type) are downloaded via `GetFile()` + HTTP GET. The raw bytes are queued as `attachment` structs alongside the message text (which may come from `msg.Caption` for photos). PDFs over 32MB fall back to save-to-disk with a text annotation. The agent worker converts these to `platform.Attachment` and calls `HandleMessage`, which routes images to `ImageBlock()` and PDFs to `DocumentBlock()` content blocks.

**Turn cancellation:** Each agent turn gets its own `context.WithCancel`, owned by `agent.driveOnce` (post-TODO #746) and registered on the session's `sessionInbox.turnCancel`. `/stop` calls `Agent.CancelSession(sk)`, which fires that cancel. Cancellation propagates to in-flight API calls (HTTP client context) and tool executions (process group kill). Multi-user shared bots are precise per session — `/stop` from chat A doesn't affect chat B's in-flight turn.

**Reset guard:** `/reset` refuses when `agent.IsProcessing()` is true — prevents clearing an active conversation mid-turn.

## Streaming Output (`internal/turn/stream.go` + per-platform `StreamSink`)

When `stream_output = true` and `streaming = true`, model output is shown in the chat in real-time as tokens arrive, rather than waiting for the full response. The pump/accumulator is shared across platforms (`turn.StreamBuffer`); only the rendering/message-identity side (`StreamSink.Update`/`Close`) is platform-specific — Telegram's is `telegramStreamSink` (`internal/telegram/turn_renderer.go`), Discord's the analogous type in `internal/discord/turn_renderer.go`.

**Lifecycle (`turn.StreamBuffer`, `stream.go`):**
1. Created via `turn.NewStreamBuffer(sink, interval, live)` when the per-turn `StreamingSink` is built (see "How interactive platforms wire it" above); `live` comes from the resolved `stream_output` display setting.
2. `OnDelta` appends every text delta to an internal buffer. While `!live`, deltas accumulate but the sink is never driven (uniform interface regardless of streaming mode).
3. **Silencing-prefix gate:** the pump does not start until the accumulated buffer diverges from every entry in `platform.IsSilencingPrefix`'s sentinel set (`[[NO_RESPONSE]]`, `"No response requested."`) — this is the only mechanism that can prevent a streamed message from being created at all (downstream `IsSilent`/`StripSilencingSuffix` gates can stop *further* delivery but can't un-send an already-streamed message). On release: one immediate `sink.Update(snapshot)` fires synchronously, then a ticker goroutine (`pump()`, interval from `stream_interval` / `stream_output` config, `[display]`/`[[platforms]]`/`[[agents.platforms]]`, `hot:"turn"` reloadable) pushes the latest snapshot on every tick where the buffer is dirty.
4. `Finish()` (called when the turn's stream buffer is torn down) stops the pump, waits for the goroutine to exit, then calls `sink.Close()` and returns `(sink, surfaced)` — `surfaced` records whether the sink ever actually rendered anything, feeding the renderer's delivered-flag logic (see `StreamingSink` above).

**Telegram's `StreamSink` (`telegramStreamSink.Update`, `turn_renderer.go`):** formats the *full* accumulated text on every call — `ConvertToTelegramHTML(closePartialMarkdown(fullText), opts)` — then chops it into ≤4096-char chunks (`splitMessage`) and **rolls over to additional messages** as the reply grows past one message's worth, rather than truncating. Per chunk: an unchanged chunk is skipped (avoids "message is not modified" API churn), an existing chunk is edited in place, and a new chunk beyond the live sequence is sent as a new message. `closePartialMarkdown` detects unmatched delimiters (`**`, `` ` ``, `` ``` ``, `~~`, `__`, `*`, `_`) by parity counting and strips the trailing unmatched instance (code fences: everything from the unmatched fence onward is removed) — lightweight, no regex, runs on every tick.

**Config:** `stream_output` (bool, `hot:"turn"` — reloadable without restart) and `stream_interval` (duration string, e.g. `"250ms"`) in `[display]`/`[[platforms]]` or per-agent `[[agents.platforms]]`. Discord's default interval is longer (1200ms vs Telegram's 250ms) due to stricter platform rate limits — see Discord Bot below.

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

**Session keys:** Same format as Telegram: `agentID/c{channelID}`. Discord snowflake channel IDs are int64.

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
keyed by the stable app `chatID` so it survives restart;
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
`SetTyping`→`typing`; `SendNotification`→`notification`. `SendTextWithButtons`→
`interactive` (foci pre-encodes each button's Data as `<promptId>:<index>`, so the
app echoes it back for routing); `EditMessageText`/`EditMessageWithButtons`→
`interactive.edit` (addressed via the hub `prompts` map). The `Send{Photo,Document,
Voice,…}` media methods store the payload in the `blobStore` and emit a `media`
frame referencing the blobId. All sends go through `convBinding.send`, which
assigns seq + ack, buffers for replay, and enqueues iff a socket is attached.

**Session-blind sends (`Hub.deliverBinding`):** any send without a live
binding — the unbound conn's `SendText`/`SendNotification` (broadcast
warnings, `--broadcast` responses) or a `SendToSession` whose session has no
binding — resolves through one ladder: the pinned default conversation
(resurrected from its persisted `conv_id` row when not live — see conversation
durability below); else whatever conversation is most recently active at send
time; else a **server-minted conversation**, with an immediate roster push to
live sockets (offline devices learn it on next hello; NB the Android client
must upsert roster conversations it has never seen). `deliverBinding` returns
the rung that fired (`default`/`most-recent`/`server-created`) and the send
path logs it, so misdelivery is diagnosable from the log. The default pin is
user-owned (`conversation.setDefault`) and never set automatically. The app can
create conversations freely, so a session-blind send never has "nowhere to
deliver". There is deliberately no fan-out to every binding: one send, one
destination.

**Conversation durability (`conv_id` rows):** the app's numeric chatID is a one-way FNV-64a hash of the conversationId (`chatIDForConv`), so `ensureBinding` persists the preimage as a `conv_id` row in `chat_metadata` (agent+`app`+chatID → conversationId) at binding creation. This row is the conversation's durable identity, independent of its frames: a conversation created (and maybe pinned as default) but never used has no frames, yet survives restarts and frame-TTL expiry. It is also how `defaultChatBinding` reverses the hash to resurrect a pinned default that isn't live. Rows are never deleted (archive is a flag; the frame janitor only trims frames), so the pin can't dangle. Pins recorded before `conv_id` persistence existed are unresolvable when not live; delivery logs the fact and falls back to most-recent.

**Binding restore across restart + archive (`framestore.go`, `StartAll`, `handleConversationArchive`):** bindings (`h.convs`/`h.bySession`) are in-memory, created on client frames (or server-side by `deliverBinding`) — so a foci restart empties them. To keep unsolicited sends landing in the SAME conversations rather than freshly minted ones, `Hub.StartAll` rebuilds bindings at startup from the union of two durable sources: `frameStore.RestorableConvs()` — every conv with a **visible** frame and a known `agent_id` (a column added to `app_frames`; written by `convBinding.send`) — plus `SessionIndex.ConvRefs("app")`, the persisted `conv_id` rows covering registered-but-frameless conversations. `ensureBinding(nil, agentID, convID)` recreates each socketless binding (`attach(nil)` is a no-op; seq seeded from `MaxSeq`). **Archive is a reversible flag, not a deletion:** the `conversation.archive` frame carries an `Archived` bool; `handleConversationArchive` persists only an `is_archived` row in `chat_metadata` (keyed by agent+platform+chatID, a sibling of `is_default`) — it does NOT purge frames, drop the binding, flip session status, or fire a reflection. The binding stays live (inbound frames still flow; history retained), and the roster surfaces `ConversationInfo.Archived` (read from `SessionIndex.ArchivedChatsForAgent` by `agentRoster`). Archived convs are therefore still restored on restart. Unarchive is a real server action (`Archived=false` clears the flag); the updated roster is pushed back to the socket on every archive/unarchive so all devices reconcile. **Archiving the agent's default chat is refused** with an `archive_default` `ErrorFrame` plus a roster re-push (reverting the client's optimistic flag): an archivable default would silently degrade session-blind delivery.

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
(model/cost/tokens from the turn usage; state, plus gap via `Agent.MetaStatus`,
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

## Facet (`platform/botpool.go`, `telegram/manager.go`, `telegram/bot.go`)

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

**Bot pool** (`platform/botpool.go`): The generic `platform.Pool[B]` / `platform.BotManager[B]` — ONE implementation of LRU acquire, release on `/done`, TTL-based stale-session reclaim, and bot lifecycle, instantiated by both Telegram and Discord (`telegram/manager.go` and `discord/manager.go` are thin type aliases).

**Shared pool**: `BotManager.shared` is a fallback pool available to any agent. Shared bots are re-wired to the acquiring agent via `SetHandlerAndCommands` at fork time.

**Bot changes** (`telegram/bot.go`):
- Per-chat session routing: primary bots derive the deterministic session key from `msg.Chat.Id` → `agentID/c{chatID}`
- `SessionKey()` — returns override key (secondary bots) or default chat session (primary bots)
- `SetSessionKey()` — thread-safe override (facet fork/done)
- `Bot.SessionKeyForChat(chatID)` — derives the deterministic session key for a chat and registers platform ownership in `chat_metadata` on first contact (via `chatmeta.Resolver`). Keys are stable identities, so restart resumption needs no persisted key.
- Default chat: first message sets the default; persisted in state store as `agent/ID/default_chat`
- Username recording: persisted per chat for `/sessions list` display
- `isSecondary` flag — enables `/done` handling, idle message rejection
- `/done` handled as special case alongside `/stop` (bypasses command registry)
- Idle secondary bots respond with "This bot is idle. Use /facet..." to non-command messages

**Session persistence across restarts:** The `bot → session_key` mapping is persisted in the state store (JSON key-value file) under `facet:<bot_username>` (the bot's Telegram username). Each `SetSessionKey` call fires an `OnSessionKeyChange` callback (wired in `agent_setup.go`) that writes or deletes the mapping. On startup, `restoreFacetSessions()` iterates all pool bots via `Pool.ForEach`, looks up saved keys, validates the session file still exists via `LastActivity`, and restores via `SetSessionKeyDirect` (bypasses callback). The bot is also re-wired to the correct agent via `SetHandlerAndCommands` and gets the primary bot's chat ID for notifications.

**Per-session override persistence:** Slash command overrides (`/effort`, `/thinking`, `/model`) are stored per-session in `session_metadata`. On startup, `RestoreSessionOverrides(sessionKey)` restores them — for model overrides, it reads the endpoint and format and calls `GetClient(endpoint, format)` to restore the correct client. The `/voice` mode follows the same pattern. Session keys are stable identities, so `/reset` clears overrides explicitly via `Agent.ClearSessionState` (which drops all `session_metadata` rows for the key).

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
- `POST /branch` — branch from default session (activity-gated, supports `no_compact`/`no_reset_hook`). Returns 412 if no default session. (Renamed from `/wake` 2026-07-10, `3b32fd3b` — "it is the branch mechanism"; the CLI subcommand is `foci branch`.)
- `POST /webhook/{agent}/{hookid}` — trigger agent turn from external events. `{hookid}` must be declared in the agent's `webhooks` config map (global `[system]` merged with per-agent `[[agents]].system`). The mapped prompt path is resolved via `prompts.ResolvePrompt()` (agent workspace/prompts → shared workspace/prompts). Reads request body as payload (max 1 MB), combines prompt + payload under a `## Webhook Payload` heading, and sends to the agent's default session. Async (202) by default; `?sync=true` for synchronous response. Supports four activity gate query params — `?if_active` / `?if_inactive` (session-level, with in-flight short-circuit) and `?if_user_active` / `?if_user_inactive` (user-attention only); see [SPEC.md](SPEC.md) Activity gating. Returns 404 if hookid not in config or prompt file not found, 412 if no default session.
- `GET /voice` — WebSocket upgrade for real-time voice conversation. Enabled when `[http] ws_enabled = true`.
- `POST /-/reload-credentials` — hot-reload API credentials from `secrets.toml`. Called by `foci auth` after saving a new token. Only registered when using static token auth (setup-token or API key), not OAuth fallback.

## Ask Gateway (`internal/askgw/`, `cmd/foci-gw/askgw_setup.go`)

**Opt-in** (`[askgw] enabled = true`). A local Unix-socket NDJSON server speaking the `askgw/1` protocol that lets external Apps (e.g. `aisudo`) present multiple-choice questions to the human via foci's existing interactive-button surface (`SendInteractiveMessageWithID`) — the same path CC permission prompts use. Disabled by default; `setupAskgw` is a no-op when `enabled != true`.

**Protocol:** Line-delimited JSON frames. `ask` (question with options), `answer` (selected option), `notify`/`cancel`/`ack`/`error` (tolerated, no action). IDs are validated to not contain `:` (platform splits button data on first `:`). Composite keying `(connID, askID)` isolates answers per connection. Message IDs namespaced as `askgw-<askID>-q<idx>` so they never collide with CC permission prompts.

**Security model:** Socket owned by group `foci-askgw` (created at install), mode `0660`. `foci-gw` runs with `SupplementaryGroups=... foci-askgw` and `CAP_SETGID`; `procx` drops `foci-askgw` from every child agent subprocess (same mechanism as `foci-secrets`). A peer must both be in the group and have its UID in `allowed_uids`.

**Config (`[askgw]`):** `enabled` (default false), `socket_path` (default `<data>/askgw.sock`), `group` (default `foci-askgw`), `allowed_uids` (required non-empty; accepts usernames or numeric UIDs), `default_agent`, `default_timeout_seconds`, `max_frame_bytes` (default 1 MiB). No persistence across restarts — socket connections die, and answer isolation means answers can only reach the original connection.

## CLI Tool (`cmd/foci/`)

Separate binary (`go build ./cmd/foci`) that wraps the HTTP gateway endpoints for scripts and cron jobs. Auto-discovers the gateway Unix socket at `~/data/foci-gw.sock` (`FOCI_GW_SOCK` env var or `--socket` flag) for same-user auth with no API key. Falls back to TCP + `FOCI_API_KEY` for remote/cross-user access. See [docs/CLI.md](CLI.md) for the full command reference, flags, environment variables, and cron integration examples.

**`foci first-run`** — first-run setup wizard. Generic steps (auth, agent ID, model, character files) live in `cmd/foci/setup.go`. Platform-specific steps (e.g. bot token, user ID) are delegated to providers via the `platform.SetupWizard` interface. Each provider returns a `WizardResult` containing a TOML config fragment and secrets map. The generic wizard appends these to the generated `foci.toml` and stores secrets via `secrets.Store`. `cmd/foci/setup.go` has zero direct telegram imports — it blank-imports `internal/telegram` for provider registration and discovers wizards via `platform.SetupProviders()`. Non-interactive mode collects provider flags dynamically from `SetupFlags()`. The `consoleUI` struct implements `platform.SetupUI` for interactive prompts.

## Wake / Branch

- **HTTP Branch** (`POST /branch`, CLI `foci branch`; endpoint renamed from `/wake` 2026-07-10): Creates a branch session from the agent's default chat session, injects the text, runs the agent on the branch. Supports `--no-compact` and `--no-reset-hook` flags (`--if-warm`/`--if-cold`, aliased `--if-active`/`--if-inactive`, gate on session cache-warmth). `--oneshot` CLI flag sets both no-compact and no-reset-hook. Returns 412 if no default session. (The internal response text is still literally "wake ok" — cosmetic, not user-facing.)
- **Scheduled Wakes** (`remind` tool with `wake=true`): Agent-initiated timer that fires message injection into the default session at specified delay or timestamp. One-shot, background goroutine, auto-cleaned after firing. Skips if no default session.

## Session-End Reflection

Before a session is cleared (`/reset` or facet TTL reclaim), the agent runs the reflection pass asynchronously. Configured via `[reflection]` section (replaces `session_reset_prompt`).

**Skill-change detection (`internal/skills/snapshot.go`):** All three reflection paths (periodic interval, session-end, pre-compaction) snapshot skill directories *before* the reflection turn and diff *after*. `SkillSnapshot` is `map[skillDir]map[filePath]time.Time`; `Diff` returns `[]SkillChange` — a skill dir present only in `after` = creation (`IsNew`); a dir with new files or advanced mtimes = update. Deletes are not reported. When changes are detected and `notify_on_skill_creation` is true (default), `SkillChangeNotify(sessionKey, msg)` routes a human-readable notification to the **reflecting session's chat** via `SendNotificationToSession` (with `SendNotification` fallback). Wired in `agents_shared.go` (agent-driven paths) and `periodic_setup.go` (periodic runner) using `Agent.SkillDirs` (the same `reloadSkillsDirs` set used by `ReloadSystemFn`).

Flow (`agent.FireSessionEndMemory` in `internal/agent/session_end_memory.go`):
1. Check `reflection.session_end_enabled` (nil = true, explicit false skips)
2. **Reflect-twice guard** — `SessionIndex.ReflectionRedundant(sessionKey)`: skip if a reflection has already run AND nothing substantive happened since (`last_activity_at <= last_reflection`). Unknown / never-reflected sessions reflect. Relies on activity tracking excluding memory turns (below).
3. Resolve prompt via `prompts.ResolvePrompt(session_end_prompt, ...)` — embedded default on empty/error
4. If prompt resolves to empty, skip
5. For branch sessions, check `BranchMeta.NoResetHook` — if true, skip (unless skipMetaCheck=true for background branches)
6. Create branch from expiring session (copies conversation history)
7. Return immediately — caller proceeds to clear the main session
8. Async: `HandleMessage(ctx, branchKey, prompt)` with 120s timeout, trigger `"session_end_memory"`, NoCompact

**Activity tracking excludes memory turns.** `last_activity_at` is bumped by `RegisterSessionIndex` / `TouchActivity` (`turn_contract.go`) on every turn *except* those whose trigger is a memory-formation pass — `isMemoryTrigger` returns true for `"reflection"` and `"session_end_memory"` (`internal/agent/context.go`). Without this, a delegated agent's reflection (which injects into the *main* session, not a branch) would bump `last_activity_at` past `last_reflection` and make the reflect-twice guard always fire reflection. Keepalive / background / cron turns still count as activity by design — only the memory passes themselves are excluded. The per-round mid-turn heartbeat (`touchTurnActivity`, above) applies the same `isMemoryTrigger` exclusion, so a long reflection turn's rounds don't bump `last_activity_at` either and the guard stays intact.

Entry points:
- `/reset` command → `agent.ResetSession` (`PrepareSessionEndMemory` → `Store.Reset` → `ClearSessionState` → background `RunSessionEndMemory`)
- `Pool.Acquire` (TTL reclaim) → `ReclaimHook` → `agent.FireSessionEndMemory` (async) → clear session key
- Periodic runner (background branch completion) → `agent.FireSessionEndMemory` (async, skipMetaCheck=true)

## Reflection & Consolidation Timers

Reflection and consolidation run on the shared `periodic.Runner` tick (30s default, see the package doc comment on `internal/periodic/runner.go`). `keepalive.go` split (2026-07-16) along its timer families — one file per mechanism, all methods on the same `*Runner`: `internal/periodic/background.go` (`maybeBackgroundWork`), `cleanup.go` (`maybeReset` + idle/stale cleanup), `consolidation.go` (`maybeConsolidation`), `reflection.go` (`maybeReflection`); `keepalive.go` itself now holds only the keepalive-proper mechanism. `runner.go` owns the shared tick loop and `RunnerConfig`.

**Interval reflection** (`maybeReflection`, `internal/periodic/reflection.go`):
1. Check `interval_enabled` (nil = true)
2. Check wall-clock interval elapsed and user not idle (`sinceLastInteraction` must be ≤ interval; `lastInteraction` is fed by the platform `OnUserMessage` lifecycle callback — wired on telegram, discord, and app providers. A transport that doesn't fire it leaves `lastInteraction` frozen at boot, so this gate skips forever for that agent.)
3. Query `session_index` for active chat sessions with `last_activity_at > last_reflection` (per-session tracking)
4. Resolve prompt via `prompts.ResolvePrompt`
5. Iterate all matching sessions: `branchFn("reflection", sessionKey, promptText, true)` for each
6. On success per session: stamp `last_reflection` at branch creation time

Reflection runs before consolidation so the latest memory content is available. Consolidation is blocked while reflection is running.

**Consolidation** (`maybeConsolidation`, `internal/periodic/consolidation.go`) — config now under `[maintenance]` (`r.maintCfg`):
1. Check `consolidation_enabled` (nil = true)
2. Compute next-fire via `parseSchedule(consolidation_time).nextFire(...)` — `consolidation_time` is `"HH:MM"` daily (process tz) or a Go duration; persisted last-run in state store
3. Check recent user activity (within 1h)
4. Check reflection / reset is not running
5. Resolve prompt via `prompts.ResolvePrompt`
6. Fire branch on default session: `branchFn("consolidation", parentKey, promptText, true)`
7. On completion: persist timestamp to state store

**Scheduled reset** (`maybeReset`, `internal/periodic/cleanup.go`) — `[maintenance].reset_time` (default off):
1. Skip if `resetFn` nil or `reset_time` empty
2. Compute next-fire via `parseSchedule(reset_time).nextFire(...)` (same dual format as consolidation; `lastReset` anchored to boot, persisted as `reset_last`)
3. Skip if reflection/consolidation/reset already running
4. Inactivity guard: skip if user active within `reset_idle_guard` (default `"55m"`) — mirrors the `foci command --if-inactive` crontab it replaces
5. Skip if no default session or a turn is in flight on it
6. Fire `resetFn(ctx, parentKey)` (→ `Agent.ResetSession`: memory formation + in-place archive, the same path as a manual `/reset`) in a goroutine; persist `reset_last` on completion

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

**Compaction triggers:** `maybeCompact()` in `agent/compaction.go` has one automatic trigger:
1. **Main threshold:** standard `ShouldCompact()` check against base threshold (default 0.8).

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

- **every_n_turns / regex** — prepended to the prompt string in `InjectNudges` before the agent layer's `ImmediateInject(SourceUser)` call, same as API content blocks but flattened to text.
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
