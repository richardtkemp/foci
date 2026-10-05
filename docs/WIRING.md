# Foci — Wiring Diagram

How the pieces connect. Read this before touching the code.

## Startup Flow (`main.go`)

Each phase is extracted into its own file. `main()` has grown to ~670 lines (still one function, but each phase below is a one/two-line call into its own file).

```
config.Load(path)                                        ← validates values; logs to stderr + buffer
                                                         ← merges [[modelinfo]] entries into modelinfo.registry

→ timeutil.SetLocation(tz)                               ← [timezone], before anything logs/init's
→ shellenv.Load(cfg.ShellEnvFile)                        ← internal/shellenv; captures the operator's shell
  procx.SetOperatorEnv(operatorEnv)                         rc/env file ONCE, AS A VALUE, and records it as the
                                                            OPERATOR population. It is NEVER applied to
                                                            foci-gw's own process (#1914) — the daemon keeps the
                                                            environment systemd gave it, so foci's own machinery
                                                            cannot be made to exec an agent-writable bash/git/
                                                            tmux. Tool shells get the operator env because their
                                                            spawn sites declare procx.Operator, not because the
                                                            global was mutated.
→ cfg.DropSubstitutableAutoApproveRules(execguard.Live()) ← must sit after SetOperatorEnv, never in config.Load:
                                                            it resolves auto_approve commands against the
                                                            OPERATOR PATH, which is only populated here
→ reportPopulations(...)                                 ← cmd/foci-gw/spawn_env.go; logs both PATHs, their
                                                            difference, and whether any TRUSTED PATH directory
                                                            is writable by foci. The only alarm for a
                                                            misclassified spawn site — both ways of getting one
                                                            wrong are otherwise silent.
→ preload.Apply()                                        ← internal/preload; sets LD_PRELOAD to ~/.lib/nosgid.so
                                                            (same inherit-via-os.Environ() mechanism) so shell
                                                            tools + backends silently drop setgid chmod bits
                                                            instead of hitting EPERM under RestrictSUIDSGID=yes.
                                                            No-op if the shim isn't installed.

→ initLogging(cfg)                                       ← logging_init.go
  → log.Init, accounting.Open (the cost ledger in api.db: migrates a pre-ledger
    api.db once, then accounting.SetLive), log.InitConversation, log rotation
  → returns cleanup func

→ app.MigrateLegacyBlobDir(cfg.DataDir)                  ← internal/app/blob.go; one-time move of blobs from the
                                                            pre-#1556 <temp root>/app-blobs into <data_dir>/app-blobs.
                                                            MUST precede CleanStale, which no longer spares app-blobs/.
                                                            Leftovers (non-blob files, failed moves) are wiped by it.

→ tempdir.CleanStale()                                   ← internal/tempdir/cleanup.go; best-effort wipe of orphaned
                                                            top-level temp state (exec bridge sockets, spill/pair/
                                                            browser scratch, escaped test dirs, spawn/ sandbox
                                                            contents, …) under the RESOLVED root (tempdir.Dir(),
                                                            never a hardcoded path). Note tempdir.resolve() PANICS
                                                            under `go test` if it lands on the shared /tmp/foci
                                                            (#1510) — tests must set FOCI_TMPDIR; the Makefile test
                                                            targets do. Runs once, right after logging
                                                            init, before anything below (bridges, spawn sandboxes)
                                                            creates its own state for this process lifetime — the
                                                            one moment everything already on disk there is provably
                                                            orphaned, not just old. tool-results/ is excluded (paths
                                                            quoted in history; aged out by CleanOldFiles instead).
                                                            Never fatal; failures (e.g. a spawn/ sandbox dir owned by
                                                            a different rich-readers-group uid) are counted and logged,
                                                            never block startup.

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

→ initVoice(cfg, sec.store)                              ← voice_init.go; builds the STT/TTS provider maps shared by voice WS + send_to_chat TTS + app voice-mode replies, plus the built-in `edge-tts` fallback provider every chain ends at (#1808) (#1439: a trigger=voice app turn's reply is TTS'd into a fap.Media{VoiceMode:true} attachment, bundled with the text. #1444: synthesized PER user-facing text block — one clip per app bubble, not one per turn — since a turn's FinalText is the ccstream backend's whole-turn accumulation; text passes through voice.NormalizeForSpeech first to strip markdown/symbols that mangle TTS. #1809: a failed synthesis stages a short reason on appBackend.voiceUnavailable, which Deliver carries as `voiceUnavailable` on that bubble's text.end/message frame so the client can mark it — rate-limit wait or generic "TTS provider error", never the raw error)

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
       → mcp.NewManagerForAgent(configDir, agentID)         ← dynamic MCP; re-reads mcp.toml on each tool call; a server that failed to connect is re-dialled when a later call names it (#2042)
       → workspace.NewBootstrap(agent.Workspace, agent.SystemFiles)
       → buildEnvironmentBlock(acfg, configPath, cfg)       ← if [environment] enabled
       → skills.ResolveDirs(home, workspace, cfg.Skills.Dir, acfg.SkillsDir)
       → skills.Load(resolvedDirs)                          ← shared first, then per-agent (overrides on collision)
         A skill directory may itself be a SYMLINK (e.g. a skill vendored in another repo and
         linked into shared/skills). os.ReadDir's DirEntry describes the entry, not its target,
         so IsDir() is false for one, and filepath.WalkDir will not follow such a root either.
         `resolveSkillDir` is the single gate: it os.Stat's THROUGH the link but returns the
         LINK path, because that path is the skill's identity — map key, log lines, recorded
         Dir — and must not move when someone repoints the link. Reading through it follows the
         link, so most callers need nothing more; walking it does not, so `scanSkillFiles`
         EvalSymlinks its OWN root rather than trusting a caller to hand it a resolved one.
         Resolution sits with the walk that needs it, so no call site can get it wrong.
         Frontmatter is read by a lenient line parser; `parseSkillFile` also strict-YAML-parses
         it (`checkFrontmatterYAML`, #2077) and `scanDir` logs a WARN (file + yaml.v3 error,
         file line numbers, dedup'd daily) when invalid. Advisory only — the skill still loads.
       → compaction.NewCompactor(sessions, model, threshold)
       → config.NewFallbackResolver(global, perAgent, aliases) ← nil if no fallbacks configured
       → agent.Agent{shared fields + Client, Tools, Bootstrap, EnvironmentBlock, FallbackResolver, ...}
       → shared.finalize(ag, params)                        ← commands, platform, nudge (shared postamble)
         → registerAgentCommands(cmdRegParams)              ← commands.go — all slash command registration
         → plat.SetupAgentConnection(AgentConnectionParams) ← creates platform connections (bots) for all active providers — NO network:
             telegram/discord build + wire each bot and register it PENDING via BotManager.ConnectBeforeRun (#2043). The network connect
             (telegram Bot.connect → connectBot → getMe; discord connectGateway → gateway Open) runs later, in the bot's own goroutine
             under BotManager.StartAll, so a Telegram/Discord outage at boot never holds up this loop, later agents, or the app provider
           → returns []*platform.SetupResult with Platform + DefaultSessionKeyFn + ConfigureFacetConn (a result means "configured",
             not "connected"); the Platform names are kept on agentInstance.platforms for startup decisions
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
  → plat.RestoreFacetSessions(...)                     ← restore bot→session mappings from state store (telegram: for facets that
                                                            connect later, the params are kept and the facet is restored on connect —
                                                            restoreConnectedFacet — since its key is the getMe username)
  → plat.StartAll(ctx)                                     ← starts all provider connections; non-blocking. Per pending bot (#2043):
                                                            goroutine → connect (netretry.Do(ctx): unbounded backoff on transient
                                                            errors, fail fast on auth / discord close 4004,4010-4014 → ERROR
                                                            "(running without it)"; ctx cancel at shutdown ends it) → markLive →
                                                            WhenPrimaryLive callbacks → Bot.Run
  → DelegatedManager.RestoreUndelivered() per agent        ← #2050: re-send inputs no backend confirmed consuming before the last
                                                            shutdown/crash (session_metadata cc_undelivered), after a transcript
                                                            check; after StartAll so the redelivered turns have chats to land in
  → startup notifications (handleRestartAndFirstRun)       ← "restarted at" notice per primary via plat.WhenPrimaryConnected (sent
                                                            when a still-connecting platform attaches); restart-turn gate reads
                                                            agentInstance.platforms, not live connections
  → deferStore = defersend.NewStore(deferred-sends.db)     ← wait_defer.go; SQLite-backed queue for `foci send --wait-*` sends whose activity gate isn't yet satisfied, swept by a background goroutine (10s tick, 2h default send-anyway timeout) — see internal/defersend below. Also the queue for ANY `/send` whose target endpoint is currently rate-limited (#1417): a hard capacity constraint, not a scheduling preference, so it defers even under `wait_none`/`--no-gate`, and the sweep withholds it unconditionally (no send-anyway-on-deadline) until `Agent.SessionRateLimited` clears — one record delivered per sweep tick.
  → http.Server{...}                                       ← http.go (registerHTTPHandlers)
  → startUnixSocket(...)                                   ← unix_socket.go (same-user auth, no API key)
  → setupAskgw(cfg, agents, connMgr)                       ← askgw_setup.go (opt-in [askgw] enabled=true; NDJSON Unix socket for external Apps to ask humans questions via foci's interactive button surface)
  → setupAskgwHTTP(ctx, cfg, mux, askgwSrv)                ← askgw_http.go (opt-in [askgw] http_enabled=true, on top of enabled=true; HTTP transport onto the SAME askgw.Server/Registry, for remote callers that can't reach the local socket — registers /askgw/ask*, /askgw/notify on the shared mux, so they inherit http.api_key auth for free)
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
    → Agent.BeginShutdown() (per-agent)             ← drain mode (#2059), see below
    → stop keepalive timers (per-agent)
    → close HTTP server
    → gracefulShutdown(agents, timeout)             ← wait for in-flight agent turns
    → startup.RecordCleanShutdown()                 ← record timestamp for crash detection
    → close MCP managers                            ← disconnect from MCP servers
    → cancel context                                ← stops platform poll loops, triggers update ack
    → connMgr.Wait()                                ← block until all platform connections finish
  → deferred closes run (SQLite DBs, log files)
```

**Drain mode (#2059, `internal/agent/shutdown.go`).** `BeginShutdown` latches the agent: turns already running finish (graceful shutdown waits for them), but nothing new begins. The gate is at the backend-agnostic layer, so it holds for the API transport and every delegated backend alike:

- **Inbox** — `runInject` runs no injection (it calls `InjectMeta.Refused`, so `EnqueueInjectWait` returns `ErrShuttingDown`), and `waitInjectGate` opens on shutdown so a held injection is refused rather than pinning the worker.
- **Turn dispatch** — `OrchestrateFullTurn` refuses a non-interactive (system) turn with `ErrShuttingDown` after `AcquireTurnLock`; `DelegatedTransport.RunInference`'s `SourceSystem` retry loop re-checks on every attempt and its bounded wait is cancelled by shutdown, so a system turn queued behind an in-flight turn is never dispatched into the draining session. Interactive (real-time user) turns are not refused — they have no durable record to fall back on.
- **Post-turn** — `runPostTurn` skips `RunCompaction`; the threshold is re-checked after the next turn.
- **Backend creation** — `DelegatedManager.refuseNewBackends` (set ONLY by `BeginShutdown`) makes `getOrCreate` fail with `ErrShuttingDown` for a new or respawned backend, rechecked at map insertion so a spawn racing the shutdown `Close` is torn down; running backends are still returned. `Close` alone does not latch it: after a plain `Close` (gateway closed its backends) the next message respawns one.
- **Warnings** — the agent-queue `warnings.Dispatcher` is wired with `HoldFn: ag.ShuttingDown`, so neither `MaybeFire` nor the turn-end `FlushPending` dispatches (warnings stay queued).
- **Scheduled wakes** — the row is dismissed by `wakeTurnDone` only after the wake's turn ran (`deliverToSessionChatThen`'s completion hook), never on a refused/unrun one, so a wake caught by the drain stays pending and is re-fired by the restore at next startup.

The drain-timeout warning (`describeBusyTurn`) reports `elapsed` from `TurnDetail.DispatchedAt` (stamped by `markTurnDispatched` when the turn actually began on its backend), and the time spent waiting before that separately — not from registration, which counted minutes of queueing as run time.

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

**Platform notification:** Startup notifications go to each of the agent's primary connections via `plat.WhenPrimaryConnected` — at once for a connected one, on attach for one still connecting in the background (#2043), stamped with the restart time. The diagnosis text is appended to the restart message. Clean restarts get no extra text. Crashes show "⚠️ Unexpected restart" with error lines. Reboots show "🔄 System reboot detected".

**State key:** `system:last_clean_shutdown` holds Unix timestamp of last graceful shutdown.

## Package Dependency Graph

```
main
 ├── config        → delegator/pretool, delegator/stoprule, display, execguard, log, modelinfo, provider
 ├── sqlite        → modernc.org/sqlite (shared Open, AgentPath, MigrateFile utilities)
 ├── log           → timeutil (the event, API-call JSONL and payload files; api.db belongs to delegator/accounting, conversation storage to convo)
 ├── convo         → log, session, sqlite, timeutil (per-agent conversation SQLite store + memory-index Hook; extracted from log so log stays lean)
 ├── display       (no deps — table rendering with Unicode display-width handling)
 ├── secrets       → BurntSushi/toml
 │   └── secrets/bitwarden → log, procx
 ├── provider      → clock, log, modelinfo (provider-neutral types and Client interface)
 ├── turnevent     → provider (leaf — the agent's per-turn event stream: event types, Sink interface, context helpers, and pure-utility sinks (BufferSink, NopSink); no platform or turn deps; moved out of agent/ per #1983 since tools and telemetry both import it too)
 ├── platform      → clock, config, log, secrets, session, voice, warnings
 │                  (messaging types, interfaces, provider registry, Messaging facade,
 │                   MessageQueue thin filter+throttle helper + GroupThrottle for group chat batching)
 ├── anthropic     → config, log, modelcaps, modelinfo, provider, secrets, github.com/anthropics/anthropic-sdk-go
 ├── gemini        → config, log, messages, provider, google.golang.org/genai
 ├── openai        → config, log, messages, provider, github.com/openai/openai-go/v3
 ├── session       → provider, log, messages, sqlite, timeutil
 ├── memory        → convo, log, session, sqlite, timeutil, fsnotify, blevesearch/bleve/v2 (FTS5 + bleve backends)
 ├── voice         → config, log, procx, ratelimit, session, tempdir, gorilla/websocket
 ├── skills        → log, procx
 ├── startup       → log, session (leaf package for crash detection)
 ├── resources     → log (goroutine monitor, memory guard)
 ├── procx         → log (process-spawn helper: strips foci-secrets/foci-askgw supplementary groups from child processes, own process group; used by every subprocess spawn site)
 ├── peercred      (stdlib syscall only — SO_PEERCRED extraction for Unix-socket auth; see HTTP Gateway)
 ├── question      (no internal deps — backend-agnostic AskUserQuestion core: parsing, formatting, choice buttons, answer resolution/merge; shared by ccstream and tools so the two surfaces can't drift)
 ├── defersend     → sqlite, timeutil (leaf — SQLite-backed queue for `foci send --wait-*` deferred sends; a pending send that isn't yet warm/cold/user-active/-inactive, OR whose target endpoint is currently rate-limited (#1417), is persisted and delivered by a background sweep, surviving a restart. Wired in `cmd/foci-gw/wait_defer.go`.)
 ├── mcp           → log, procx, provider, tools, BurntSushi/toml, go-sdk/mcp
 ├── fap           (no internal deps — Foci App Protocol (FAP v1) wire types/codec: Envelope, ServerFrame/ClientFrame, ToolResult, ToolStatus* constants, NewULID; pure protocol leaf shared by app and tools so neither sits above the other)
 ├── tools         → turnevent, config, convo, delegator, delegator/accounting, display, fap, log, memory, modelinfo, peercred, platform, procx, prompts, provider, question, ratelimit, secrets, secrets/bitwarden, session, telemetry, tempdir, tools/spill, voice (Registry, Tool, shared helpers, the exec-bridge generator, web, http, and most tool impls)
 │     ├── tools/spill    → (stdlib only) shared spill-to-disk writer: bounded in-RAM head + overflow to temp file, optional total cap; used by tools/shell and the http tool
 │     ├── tools/shell    → tools, tools/spill, log, procx, secrets, secrets/bitwarden (the exec/shell tool; execbridge generator stays at root)
 │     ├── tools/tmux     → tools, log, display, session, procx, prompts, tempdir (tmux session tool — 8 files)
 │     ├── tools/browser  → tools, config, log, tempdir, tools/browserjs (browser automation — imports root for Tool/ToolResult)
 │     └── tools/browserjs (no foci deps — vendored go-rod JS snippets)
 ├── workspace     → log, provider
 ├── nudge         → delegator, log, platform, turnevent, workspace (rule extraction, scheduling, file I/O)
 ├── prompts       (top-level package, not internal — lives at `shared/prompts/`) → log, timeutil (embedded .md files incl. nudge framing + ResolveOrientationTemplate helpers)
 ├── modelinfo     (no deps — stdlib-only leaf package for model attributes: context window, capabilities, pricing)
 ├── ratelimit     (no deps — neutral limit signals + shared reset/fallback policy)
 ├── modelcaps     → modelinfo, log (leaf — per-backend live capability cache; Fetcher + Persister seams injected at startup so it imports no anthropic/session/DB)
 ├── compaction    → config, delegator/accounting, log, memory, messages, modelcaps, modelinfo, prompts, provider, session, tools
 ├── tempdir       (no deps — stdlib-only leaf package for canonical temp dir)
 ├── linkwalk      (no deps — stdlib-only leaf: Up/Down walks over parent/child links that stop at the first repeated key, so a cycle in session_index rows or subagent ancestry ends the walk; used by command `sessionFamily` (/cost) and ccstream `topLevelAncestor`, #1581)
 ├── provision     → modelinfo, procx (agent creation; modelinfo so a bare model alias — "opus", "fable" — resolves to the newest member of that family instead of a literal that goes stale)
 ├── command       → agent, config, delegator, delegator/accounting, display, linkwalk, log, memory, modelcaps, modelinfo, platform, procx, prompts, provider, provision, question, session, tempdir, timeutil, tools, workspace
 ├── warnings      → log (leaf — warning queue and proactive dispatch)
 ├── messages      → provider (shared message-inspection utilities: HasToolUse, ToolUseIDs)
 ├── timeutil      (no deps — centralised timestamp formatting with configurable timezone)
 ├── relogin       → log, procx (automated CC re-login on 401 — see Backend Session Lifecycle)
 ├── delegator     → clock, log (Delegator interface, registry, StartOptions, SessionEvents/TurnEvents)
  │   ├── delegator/accounting → log, modelinfo, sqlite (the per-call cost ledger in api.db, #2111 — schema, SQL cost views, rate render, Book/Report/RecordTurn and their observers (api.jsonl, BookedHook), the direct-API adapter, the legacy-row writer the readers' tests seed history with, the readers, the startup migration; see "Cost ledger")
  │   ├── delegator/autoapprove → execguard, secrets (shared by ccstream/codex/opencode — auto-approve rule compilation/matching)
  │   ├── delegator/cctmux     → delegator, delegator/accounting, delegator/ccstream, log, modelinfo, procx, fsnotify (tmux-based Claude Code; registers "claude-code-tmux" via init(); books each completed main-thread call in the ledger)
  │   ├── delegator/ccstream   → delegator, delegator/accounting, delegator/autoapprove, delegator/hookbin, delegator/pretool, delegator/stoprule, linkwalk, log, modelinfo, procx, question, ratelimit, tempdir, timeutil, toolformat (stream-json Claude Code; registers "claude-code" via init())
  │   ├── delegator/pretool    (no foci deps; stdlib + mvdan.cc/sh parser — PreToolUse deny-rule engine, #2028/#2033/#2034; spawns `when` checks with raw os/exec, not procx, as it runs only in foci-cc-hook and the foci CLI; shared by config, ccstream, cmd/foci-cc-hook and cmd/foci `pretool`)
  │   ├── delegator/stoprule   → delegator/pretool (only its Patterns type; stdlib otherwise — Stop-hook rule engine, #2089: matches the turn's final text, scans the CC transcript for the turn's background launches; shared by config, ccstream and cmd/foci-cc-hook)
  │   ├── delegator/sessionenv → tempdir (shared by codex/opencode + cmd/foci-codex-hook — per-session exec-bridge env file format, lifecycle, and the codex command wrap/unwrap)
  │   ├── delegator/codex      → delegator, delegator/accounting, delegator/autoapprove, delegator/hookbin, delegator/keyedmutex, delegator/sessionenv, log, modelcaps, modelinfo, procx (Codex app-server JSON-RPC; books its own calls in the cost ledger; registers "codex" via init())
  │   └── delegator/opencode   → delegator, delegator/accounting, delegator/autoapprove, delegator/keyedmutex, delegator/sessionenv, log, modelinfo, procx, ratelimit, tempdir (HTTP/SSE OpenCode; books its own calls in the cost ledger; registers "opencode" via init())
 ├── agent         → turnevent, compaction, config, convo, delegator, delegator/accounting, display, log, memory, messages, modelcaps, modelinfo, nudge, platform, procx, prompts, provider, ratelimit, relogin, session, skills, telemetry, timeutil, tools, turn, warnings, workspace
 ├── periodic      → config, delegator, log, memory, prompts, provider, session, skills, timeutil, warnings (NO agent)
 ├── dispatch      → command, platform, session, tools (shared command dispatch logic; platform wrappers delegate here)
 ├── turn          → turnevent, display, log, platform, tooldetail (shared turn rendering, tool call tracking, and tool-result display store for all platforms)
 ├── telegram      → agent, turnevent, chatmeta, command, config, dispatch, display, log, netretry, platform, secrets, session, timeutil, tooldetail, toolformat, turn, voice
 │                  (registers via init() → platform.RegisterMessagingProvider; blank-imported in main.go)
 ├── discord       → agent, turnevent, chatmeta, command, config, dispatch, display, log, netretry, platform, secrets, session, timeutil, tooldetail, toolformat, turn, voice
 │                  (registers via init() → platform.RegisterMessagingProvider; blank-imported in main.go)
 ├── app           → agent, turnevent, command, config, delegator, dispatch, fap, log, platform, question, ratelimit, secrets, session, sqlite, tempdir, toolformat, tools, turn, voice (FAP WebSocket native-app provider — see App Provider section; registers via init() like telegram/discord)
 ├── netretry      → log (startup-connect retry shared by telegram + discord: Backoff schedule, Do loop, PermanentMarkers auth/transient split)
 ├── askgw         → clock, log, peercred, question (opt-in ask-gateway for external Apps — see Ask Gateway section)
 ├── telemetry     → turnevent, delegator/accounting, log, modelinfo, provider, session, go.opentelemetry.io/otel (+ sdk, otlptracehttp) — OpenTelemetry export of every turn to an OTLP/HTTP collector (Langfuse) plus scores/score configs over its REST API; wired from cmd/foci-gw (init), agent (turn spans), tools + cmd/foci-gw (cross-agent links). See "Tracing".
 └── evals         → log, fsnotify, yaml.v3 — rubric registry (scoring axes as files, watched); consumed by cmd/foci-gw (/score validation, score-config mirroring). See "Tracing" → "Scores and rubrics".
```

No circular dependencies. `provider`, `display`, `log`, `secrets`, `memory`, `skills`, `prompts`, `startup`, `resources`, `tempdir`, `warnings`, `modelinfo`, `modelcaps`, `messages`, `ratelimit`, `timeutil`, `turn`, `dispatch`, `procx`, `peercred`, `question`, `netretry`, `linkwalk` are leaf packages (no internal foci deps beyond what's shown). `platform` depends on leaf packages only (clock, config, log, secrets, session, voice, warnings). `provision` depends only on the leaves `modelinfo` and `procx`. The tree above is checked against `go list` by `make lint` (`scripts/find-wiring-drift`), so a change that moves imports must update its line.

**`internal/state` no longer exists.** The former `state` package (`system_state` crash-detection row, `state.json`/state.db key-value store, `agent/ID/default_chat`, `facet:<bot>` bot→session mapping, ask/wizard persistence) was folded into `internal/session`'s `SessionIndex` (SQLite-backed) before this doc's tracked baseline — every dependency line that used to read "state" above has been corrected to "session" (or dropped where session wasn't otherwise a dependency). If you see "state" cited anywhere else in this doc or in `shared/skills/`, it's stale.

**`provider` package:** Defines the neutral types (`Message`, `ContentBlock`, `ToolDef`, etc.) and the `Client` interface (`SendMessage`, `CountTokens`). `anthropic`, `gemini`, and `openai` all implement `provider.Client`, translating between neutral types and their wire formats.

**`platform` package:** Defines platform-agnostic messaging types (`Message`, `Attachment`), the `Connection`/`ConnectionManager` interfaces, the `MessagingProvider` interface for platform implementations, and the `Messaging` facade that manages all active providers. Providers register via `RegisterMessagingProvider()` (called from `init()`) and are activated at startup via `InitMessaging()`. An aggregating `ConnectionManager` merges connections from all providers — `AllForAgent()` returns connections across all platforms, enabling multi-platform fan-out for notifications. `cmd/foci-gw/` uses only the facade; zero platform-specific type references. Also defines the `SetupWizard` interface (optionally implemented by `MessagingProvider`) for contributing interactive setup steps to `foci first-run`. `SetupProviders()` returns all registered providers that implement `SetupWizard`. Types: `SetupFlag` (CLI flag definition), `WizardResult` (config TOML fragment + secrets), `SetupUI` (console interaction primitives).

**`chatmeta` package:** Shared per-chat metadata logic extracted from `telegram` and `discord`. Session keys are deterministic (`session.NewChatSessionKey`), so the `Resolver` derives them and registers platform ownership (a `registered` chat_metadata row backing `SessionIndex.PlatformForChat`) on first contact; it also handles `DefaultChatID`, `DefaultSessionKey`, and `RecordUsername`. Platform-specific methods (`SessionKey`, `SetSessionKey`, `ChatID`, `SetChatID`, `Username`) remain on each Bot. Imports: `platform`, `session`, `log`. All methods are nil-receiver safe.

**`route` package:** The single addressing authority. Defines the canonical `Target` grammar (`agent[/rest][?create=&policy=]`) parsed identically by every entry point (HTTP handlers, CLI, `send_to_session`, webhooks), the `Resolver` with ONE resolution ladder (exact key → existing named session → chat alias → create-named; empty rest → agent default via `SessionIndex.DefaultSessionKeyForAgent`, and when that finds nothing non-archived, the `CreateDefault` hook mints one visible app conversation — injected at exactly two sites, both delivery paths: the `send_to_session` tool (`tool_table.go`) and the HTTP `resolveTargetSession` (`/send`, `/branch`, `/webhook`; `httpHandlerDeps.createDefault`, #1859); warm/keepalive resolvers and `defaultSessionKey` (command dispatch) leave it nil), `Receipt` (`{target, session, resolved_via}` returned to senders in HTTP responses and tool results), `ConnFor` — the ONE outbound delivery cascade (session's own connection → policy-dependent fallback to the owning platform's primary) — and `Broadcast`, the delivery set behind `PolicyBroadcast` (`foci send --broadcast`, and the rate-limit *gate* notice + max-tokens warnings): one connection per platform — each platform's primary — delivering to that platform's default destination (telegram/discord: the default chat; app: the default conversation via `Hub.deliverBinding`, else the newest conversation, auto-created if none exist). Only three policies exist: `PolicyFallback` (default — session's own connection, else the agent's primary), `PolicyStrict` (session's own connection or nothing — `DeliveryNone`), and `PolicyBroadcast`. **`PolicyRootFallback` and the `DeliverySuppressed` outcome were removed (2026-07-09, `34419b44`)** — branch/facet sessions with no live connection of their own now fall back to the primary like any other session, same as root sessions; the leak-prevention guard was judged not worth the complexity (see commit message for the full rationale). `route.NotifySessionChat` (`notify.go`) is a small helper for session-targeted notifications: resolves the session-or-primary connection and prefers `SessionNotifier.SendNotificationToSession` over a bare `SendNotification`. Cache-bust warnings are deliberately NOT broadcast: they concern one session's cache prefix and route to that session's chat via `SessionNotifier`. Claude Code's **usage-limit warning** (ccstream `rate_limit_event`, #1211/#1238) is likewise not broadcast: `cmd/foci-gw/agents_delegated.go`'s `deliverRateLimitNotice` picks ONE target per `[notify] rate_limit_notify_to` — `"session"` (default: `ConnFor`/`PolicyFallback` on the backend's own `StartOptions.SessionKey`, which the `SetOnRateLimited(sessionKey, notice)` hook now carries), `"default"` (`Primary`, the pre-#1857 behaviour), or `"both"` — read live off `LiveConfig()` so the key is hot (#1857). Imports: `platform`, `session`.

Most packages depend on `provider` for types; only `main.go` (`cmd/foci-gw/credentials.go`) imports `anthropic` directly in production code (for Anthropic-specific features — `tools` only references it from test-only helpers now). `periodic` still imports `session` directly (it holds a `*session.SessionIndex` to pick keepalive/reflection/background candidates) but never imports `agent` — warning dispatch is handled by the `warnings` package, wired together in `main.go`.

**`provision` package:** Shared agent creation logic used by both `cmd/foci/setup.go` (first-run wizard) and `command/agents_new.go` (`/agents new` runtime command). Imports only `modelinfo` and `procx`. Provides `AgentSpec` + `Provision()` (workspace creation, character file copying, SOUL.md templating), validation (`IsValidAgentID`), config block generation (`GenerateAgentBlock`), and crontab templating (`GenerateCrontab`, `AppendCrontab`). `ResolveModelAlias` turns a family word (`opus`, `sonnet`, `haiku`, `fable`; empty ⇒ sonnet) into `anthropic/<newest id in that family>` via `modelinfo.NewestInFamily`, and passes anything else through untouched — an alias names a family, not a version, so pinning it to a literal guarantees drift (the old fixed map still returned `claude-opus-4-6` long after every real turn ran opus-5). `NewestInFamily` ranks only the numeric segments FOLLOWING the family token, which excludes `-latest` pointers, `-fast`/`[1m]` variants, and `claude-3-haiku` (version before the token). This affects agent CREATION only; a delegated backend still resolves its own `backend_config.model` string itself. Platform-specific validators (e.g. `IsValidBotToken`, `IsValidUserID`) live in their respective platform packages (e.g. `internal/telegram/validate.go`).

## Command Dispatch Architecture

Slash commands (`/ping`, `/model`, etc.) are dispatched through a three-layer architecture:

1. **Platform wrapper** (`internal/telegram/bot.go`, `internal/discord/connection.go`): Thin wrappers that extract `text`, `chatID`, and `userID` from platform-native message types (`gotgbot.Message`, `discordgo.Message`) and delegate to the shared dispatcher.

2. **Shared dispatch** (`dispatch/dispatcher.go`): Platform-agnostic routing logic. Detects dot-commands (`.model`) vs slash-commands (`/model`), resolves session keys, and builds a `command.Request`. Returns a `dispatch.Result` with `Handled`, `Response`, `SessionKey`, `UserID`.

3. **Command layer** (`command/command.go`, `type Registry`): Receives `Request` and `CommandContext` (platform-agnostic dependencies), executes the command, and returns a `Response` with `Text` and optional `DocPath`. When `DocPath` is set, it points to a temp file that the platform layer sends to the originating chat and then removes, via the shared `platform.SendDocAndRemove(sender, chatID, path, caption)` helper (`internal/platform/senddoc.go`) — the ONE place that owns "send this temp file, then delete it regardless of outcome" (#1511; previously every call site hand-rolled the pair, and the one that dropped the removal leaked hundreds of files over two weeks). `chatID == 0` sends via the sender's default chat (`SendDocument`) instead of a specific one (`SendDocumentToChat`); a nil sender or empty path is a no-op-but-still-removes / no-op respectively. Callers: `telegram.Bot.renderCommandOutcome`/`tryIntercept` (`Response.DocPath` / `WizardDocPath`), `discord.Bot.renderCommandOutcome`/`tryIntercept` (same), `app.Hub.dispatchCommand` (`Response.DocPath`), and the HTTP `/command` handler (`cmd/foci-gw/http_handlers.go`, resolving the sender via `ForSessionOrPrimary(sessionKey, agentID)`, chatID 0). The app wizard's inline-media path (`internal/app/wizard.go`) is a genuinely different lifecycle — it stages the file as a blob for in-step rendering when possible, falling back to `SendDocumentToChat` only when staging fails or the wizard already ended — so it removes the file itself rather than going through the helper.

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

Both transport paths (API and delegated) are unified under the `TurnContract` interface — 19 methods grouped into four phases. Adding a method to the interface produces a compile error in both transports until implemented.

**Transports:**
- `APITransport` (`turn_api.go`) — traditional API code path: direct provider calls with client-side tool execution loop.
- `DelegatedTransport` (`turn_delegated.go`) — delegated path: the backend (Claude Code) owns inference and tool execution.
- Both embed `sharedTurnOps` (`turn_contract.go`) for shared implementations (6 methods).

**Transport selection:** In `HandleMessage`, if `Agent.DelegatedManager != nil` → `DelegatedTransport`; otherwise → `APITransport`.

**Orchestrator:** `OrchestrateFullTurn` (`turn_orchestrator.go`) calls all 19 methods in canonical order:

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
  LogConversationSent   API: log final text (recordTurnEntry)  Delegated: no-op (loggingSink logs per TextBlock)
  TouchActivityPost     Shared: fire OnActivity callbacks
```

**Post-turn sync/async split** (`runPostTurn`): API turns close `CompletionChan` before `RunInference` returns (synchronous), so post-turn runs inline. Delegated turns block inline waiting for `CompletionChan` with an activity-based timeout — if no stream events arrive for 24h (`streamIdleTimeout`), the wait times out. That ceiling is deliberately huge: it is a backstop against orphaned goroutines, not a liveness check (normal backend death is caught by process exit / stream EOF), and CC emits nothing while a permission prompt awaits approval, so a short value would false-alarm on every long approval wait. Activity is tracked by the backend's `LastActivity()` method, seeded at turn start and updated on every stream event. Steered follow-ups (delegated, `IsTurnInFlight() == true`) close `CompletionChan` immediately with no post-turn work.

**Shared prompt composition** (`turn_common.go`): `composeTurnText` assembles metadata prefix, reminders, state dashboard, attachment paths, and user texts into a `turnTextParts` struct. The API transport converts these to content blocks; the delegated transport joins them into a flat string via `JoinPrompt()`, **separated by a BLANK LINE** — flat text has to carry the structure that ContentBlocks make explicit on the API side. A single `\n` made the injected header and the user's first line indistinguishable: with no pending ask the statusline ends on `[state] …`, and coach read Dick's `Yesterday: …` habit line as a state field and reported the message empty (#1627). `\n\n` is already the separator `turn_delegated.go` uses for the first-run message, branch orientation and the nudge/user boundary.

**Shared attachment normalisation** (`turn_message.go` `prepareAttachments`, #2095): both transports run a turn's attachments through the same step — convertible documents (`platform.IsConvertibleDocMIME`: docx/xlsx/pptx/html/csv/txt) become text via `convertAttachmentToText`, PDFs over 32 MB are dropped (the saved-to-disk annotation covers them), images are downscaled, and any other MIME is logged and dropped — only image/jpeg|png|gif|webp may become an image block (`isImageBlockMIME`, #2099; the API rejects the whole request otherwise). The API transport makes each result a content block; the delegated transport prepends the document texts to the flat prompt and passes only the binary images/PDFs on `Inject.Attachments` (held on `TurnState.delegatedAttachments` between `ComposePrompt` and `RunInference`). ccstream's `attachmentBlockType` is an allowlist backstop (image/jpeg|png|gif|webp → image, application/pdf → document, anything else logged and skipped): the API 400s a document block of any other type, and CC < 2.1.284 then fails every later turn of the session.

### Batch runs (`DelegatedManager.RunBatch`, #1962)

A **batch** is a one-shot prompt whose answer goes back to an in-process caller and never to a chat: memory consolidation (`periodic/consolidation.go`), nudge extraction (`nudge.Extractor.ExtractViaBatch`), the delegated `foci_summary` tool (`tools.BatchSummariser`), and the delegated `/prompts diff` summary (`command.buildDiffSummary`). Every caller passes a `delegator.BatchRequest` with a **`Purpose`** (`delegator.BatchPurpose*`: `consolidation`, `nudge_extraction`, `summary`, `prompt_diff`) and the owning session (`OwnerSessionKey`).

There is **no per-backend batch mechanism** — no `claude --print`, no `codex exec`, no opencode side session. `RunBatch` mints a fresh, ephemeral child session of the owner's ROOT chat (`<agent>/<c|i><id>/b<ns>`; with no parseable owner, `<agent>/ibatch/b<ns>`), indexes it as `background-task` with the owner as parent, and runs ONE ordinary turn on it through `RunBatchTurn` → `Agent.RunBatchTurn` → `Agent.HandleMessage` — the same `getOrCreate` → backend `Start` → `DelegatedTransport` path a branch uses. The api.db row (`LogUsage`, `call_type='delegated_turn'`, `purpose=<Purpose>`), the Langfuse trace (tagged `purpose:<Purpose>`) and the cost come from that turn code, so batch spend is in each agent's normal totals (the daily briefing sums every row, no `call_type` filter). Before #1962 batches shelled a separate one-shot per backend and left no row and no trace at all.

What makes a batch session differ from a branch is confined to four places:
- **Launch** (`getOrCreate`, from the `batchSpec` RunBatch registers for the key): `SystemPrompt` = the caller's (replacing the agent's composed prompt; `SystemPromptFunc`/`ModelFunc`/`EffortFunc` are cleared so no backend re-resolves the agent's — opencode's Start calls `SystemPromptFunc` itself), model = the request's, else — when the request sets `Cheap` (the summary and prompt-diff callers) — the backend's `delegator.BatchCheapModeler` (ccstream/cctmux: `haiku`), else the backend's `delegator.BatchModelDefaulter` (ccstream: `sonnet`), else the agent's. Model names are backend-specific, so callers never hardcode one: the summary callers pass `[tools] summary_model` (empty by default) plus `Cheap`, so an opencode/codex agent runs them on its own model rather than an alias it can't resolve (#2032); no launch effort; `StartOptions.SkipPermissions` (CC launches with `--dangerously-skip-permissions`: a batch has no user to answer a prompt, and a prompt would land in a chat; the few requests CC still sends in that mode, e.g. from 2.1.281 `rm -rf "$(pwd)"`, ccstream's `handleToolRequest` denies at once without calling the prompt func, #2096; opencode has no launch flag and a shared server, so `surfacePermission` answers each ask itself via `answerUnattended` — permissions allowed once, questions declined, never prompted, #2153). The system prompt reaches CC through the stream-json `initialize` request, not argv, so there is no `MAX_ARG_STRLEN` cliff (#1963).
- **Callbacks** (`setBackendCallbacks`): no typing indicator, subagent status or autonomous-run adoption for a batch key — all three would surface in the owner's chat.
- **Turn** (`Agent.RunBatchTurn`, `ComposePrompt`): trigger = purpose; the prompt is sent verbatim (no `[meta]` header, nudges, first-run onboarding or branch orientation); a `BufferSink` is registered on the session router BEFORE dispatch (the orchestrator's Phase-3.5 registration comes only after the begin-turn inject, and on a batch key anything emitted in that gap would late-deliver to the owner's chat — a fresh key has no concurrent autonomous run to clobber), and a `NopSink` stays registered afterwards.
- **Teardown** (`RunBatch`): `ResetSession` closes the backend as soon as the turn returns. The backend transcript is left for the daily ephemeral-session GC (`CleanupEphemeralSessions`, via `backend_resume_history`) like any branch's.

Why this was once separate: the original `RunOnce` (12036174) fled an interactive session that "leaked output to platform and returned ''". The sink/callback handling above is what makes the turn path safe for it now.

`ArchiveSweep` has a third skip alongside current-chat and active-branch: an entry whose `file_path` stats as a **non-regular** file (a directory) is skipped and counted, never handed to `gzipFile` (#1555). Without it a failed gzip `continue`s past `UpdateStatus`, so the row stays `active`, stays a candidate, and re-warns every sweep forever — 544 WARNs in the archived logs from one row (`codex/c7508215090037080023`, `file_path` = `/home/foci/.codex/sessions/`). The skip stats only; an **empty** `file_path` deliberately still falls through to `gzipFile`, which swallows ENOENT and marks it archived, matching `PruneOrphans`' "backend session, not an orphan" rule.

Codex follows the shared-connection/per-session-facade model: one pooled app-server subprocess and JSON-RPC transport is shared per agent, while each foci session (interactive or batch) receives its own `Backend` facade bound to one Codex thread. Turn, approval, model, compaction, callbacks, and metadata remain facade-local. `Start` is structured to keep that true by construction: the two paths differ ONLY in how they obtain a connection (attach to the pooled owner, or `launchAppServer` — spawn, pipes, reader goroutine, `initialize`, `model/list`), and everything session-scoped after that — resolve the model, record the reasoning effort, start or resume the thread — is a single shared tail with `b.Close()` as the teardown for both. Two hand-maintained copies of that tail is what made facade sessions silently run an unresolved model at codex's default reasoning effort (#1573); a facade holds no catalogue of its own and deliberately does NOT re-run `model/list` — `b.catalogue()` reads the owner's through `process()` under its lock, so a facade always sees the CURRENT catalogue. Copying it at attach time was both a race (the attach path holds only `sharedPool`, never `owner.mu`) and permanently stale, since `refreshModelCaps` rebinds the owner's field to a freshly allocated slice a copy never observes (#1577). The shared reader routes notifications by Codex thread ID; closing one facade releases only that session and does not stop the app-server while sibling facades remain.

**Codex warnings are logged, never routed (#1575).** `configWarning` and `warning` are logged at **WARN** by the backend and that is their entire delivery path — there is deliberately NO codex-specific hook (`SetOnWarning`/`fireWarning`/`onWarning` existed and were removed). Delivery is the generic `log.SetWarnHook` → `setupWarningHooks` mechanism, per-agent-configurable via `notify.inject_chat_warnings` (chat) and `notify.inject_agent_warnings` (agent context); a backend-specific hook alongside it produces duplicates for any operator who has those set. The WARN level is therefore load-bearing, not cosmetic: `log.SetWarnHook` fires only at WARN/ERROR, so logging these at Info (the pre-#1575 behaviour) made them invisible to both switches — including `"Invalid configuration; using defaults."`, where one typo in `config.toml` silently swaps the agent's model and sandbox for defaults. Routing them per-session is not merely unnecessary but unanswerable: both observed shapes are emitted at `initialize`, BEFORE any thread exists, so they carry no `threadId` and there is no session that owns them.

`ForkRequest` carries the owning foci session key so Codex resolves fork targets without a current-thread default.

### Session Lifecycle Operations (`agent/lifecycle.go`)

The agent exposes three lifecycle methods that encapsulate multi-step sequences previously scattered across command handlers:

- **`ResetSession(ctx, sessionKey)`** — clears session history with memory formation. The session key is a stable identity; unified across API and delegated transports: (1) `PrepareSessionEndMemory` creates the reflection branch from the still-live history — for delegated agents it also remaps the live backend and its `cc_resume_id` to the branch (`DelegatedManager.RemapSession`) so the main key gets a fresh CC on next message; (2) `Store.Reset` archives the session file in place; (3) `ClearSessionState` drops per-session overrides/metadata; (4) `RunSessionEndMemory` drives reflection on the branch in the background (up to 120s) and destroys the branch backend.
- **`CompactSession(ctx, sessionKey, dryRun)`** — triggers manual compaction. Validates message count (min 5), runs the compaction pipeline, then reloads bootstrap and resets cache baseline. When `dryRun` is true, the full pipeline runs (API call, summary generation) but the session is left unchanged — the summary is returned for inspection.

All three call `reloadAfterMutation()` internally, which reloads bootstrap, refreshes nudges, and invalidates all per-session system prompt caches.

**Delegated system prompt rebuilt from disk at session start (#828 Part A, fixes #706):** the delegated CC system prompt was previously built once at agent setup and frozen into `StartOpts.SystemPrompt` for the process lifetime, so `/reset` and idle respawn never picked up character-file or skill edits. `StartOptions.SystemPromptFunc` fixes this: when set, `DelegatedManager.getOrCreate` calls it at every session start and its non-empty result wins over the static prompt. The closure (wired in `agents_delegated.go`) reloads `Bootstrap` from disk itself and re-runs the skill load, so every respawn — reset, idle, compaction-bounce — gets a fresh prompt regardless of caller. Empty result falls back to the setup snapshot. **Exception — backend forks (#2051):** a branch created by `ForkBackendBranch` launches with its parent process's launch prompt (`managedBackend.systemPrompt`, handed over by `InheritParentPrompt`, consumed once by `getOrCreate` via `takeForkPrompt`), because a rebuilt prompt breaks the fork's cache sharing with its parent — see CACHING.md "Session branching".

### Steer Mode Differences (API vs Delegated)

When `steer_mode` is enabled and a turn is active, user messages are buffered as "steers" and injected mid-turn rather than waiting for completion. **Only real-time user input steers** — platform messages (telegram/discord/app) and voice. System-initiated input never does, even with steer enabled (see "System injections never steer" below).

**Per-message override (app):** the sender can decide ad-hoc per message via `fap.ClientMessage.Steer` (`"steer"` / `"queue"` / empty = config default), mapped to `agent.Envelope.Steer` (`SteerAlways` / `SteerNever` / `SteerDefault`). `SteerAlways` steers even with `steer_mode = false`. `SteerNever` queues for a fresh turn after the in-flight turn completes AND is exempt from every conversational intercept that would otherwise consume it — plan-cancel feedback (#858), pending-`foci_ask` answer capture (both the `Enqueue` mid-turn gate and `RunTurn`'s idle capture), and the backend AskUserQuestion/elicitation typed-answer intercepts. At the transport, a `SteerNever` turn dispatches like a system turn (`ImmediateInject(SourceSystem)` — never folds, waits for backend idle); `RunTurn` threads the preference to `RunInference` via `WithSteerPreference`:

- **API transport:** Steer messages are collected via `steerBlocks(ctx)` and injected as text content blocks in the tool result message between tool execution loops. `steerBlocks` pulls from the `turnevent.Steerer` supplied by `agent.Inbox` (one per session) — the inbox accumulates mid-turn text in its per-session steer buffer when the configured backend is API-mode (no `delegator.Delegator` registered).
- **Delegated transport:** Steer messages are dispatched immediately by `agent.Inbox`. On `Enqueue` of a mid-turn message, the inbox calls `Backend.ImmediateInject(ctx, Inject{Source: SourceSteer, Text: env.Text})` directly, looking up the session's backend via the agent's `DelegatedManager`. `ImmediateInject(SourceSteer)` sends the steer text as a `type: user` stream-json event at queue priority `"next"` (CC's own class for user input). CC's mid-turn drain (`claude-code/src/query.ts:1570-1589`) folds the message into the current `ask()` as an attachment to the next tool-result batch, so the model responds in the same turn and the original handler's `OnText`/`OnTurnComplete` pipeline carries the response. Steer does not abort anything: priority `"now"` — which makes CC `abort('interrupt')` the in-flight ask and answer immediately — is reserved for NYI per-message steer tagging or an NYI aggressive-steer config mode; "stop right now" semantics use `/reset hard`. Mid-turn steer for delegated agents bypasses the steer buffer entirely; the buffer only matters for API-mode agents that have no equivalent stdin protocol primitives.

  **Attachments (#2099):** a mid-turn message with attachments steers only when the session backend implements the optional `delegator.FoldAttachmentCarrier` and `FoldsAttachment` accepts every binary attachment left after `prepareAttachments` (converted documents are text and always fold). `Agent.steerInject` builds the inject: converted-document text, then the saved-to-disk notes (`attachmentPathNotes`, shared with `composeTurnText`), then the caption, with the images in `Inject.Attachments`; ccstream's `sendFold` writes them as one content-block message at the same priority. Only ccstream is a carrier, and it accepts images only: probed on CC 2.1.280, a PDF document block written mid-turn is stored as a `queued_command` but never reaches the model (the same block in a turn-starting message does). Anything else — a PDF, a non-carrier backend, API mode (the steer buffer is text-only), the compaction hold — queues the whole message for a fresh turn. Plan-cancel (#858) and mid-turn ask capture (#884) stay text-only.

**Compaction hold (#856):** `Enqueue` gates the steer decision on `Agent.IsCompacting(sessionKey)` — while a `/compact` turn is in flight, a steer would write to CC's stdin mid-compaction and CC folds the raw text into the compaction transcript unframed (no `[meta]` header). The gate routes such messages to the session channel instead. Auto-compaction runs synchronously inside the driven turn (`driveOnce` → `runPostTurn` → `RunCompaction`), so the worker is already blocked and channel-queued messages wait naturally; the session worker adds a `for a.IsCompacting(...)` poll-hold (`compactionHoldPoll`, 100ms) after the #767 in-flight gate as a backstop for the manual-`/compact` path where the worker is free. Held messages dispatch as a clean fresh turn once compaction clears.

**Declined-compaction release (#1267):** `runDelegatedCompact` arms the ccstream compaction waiter and blocks on `WaitForCompaction`, which historically returned only on a `compact_boundary` stream event or the 5-minute `delegatedCompactTimeout`. When CC *declines* to compact (a short session: `status=compacting` → assistant "Not enough messages to compact." → `result` → `session_state_changed:idle`, with **no** `compact_boundary`), that wait used to stall the full timeout with `IsCompacting` latched — so the compaction hold above held every inbound message and the session looked stuck for ~5 min. Fix: a real compaction always emits `compact_boundary` *before* idle (it resolves the waiter first), so at `session_state_changed:idle` a still-armed waiter means CC declined — `signalCompactionAbort` (ccstream `compaction.go`) resolves the wait with `delegator.ErrCompactionNoBoundary` and `WaitForCompaction` returns it. Both outcomes travel through a single buffered(1) `compactCh chan error` via `resolveCompactionWait`, whose identity is reset only by `WaitForCompaction` after it has actually received a value; the earlier design of two `chan struct{}` fields nil'd at fire time let a not-yet-captured reader misread "resolved via abort" as "never armed" and report a declined compaction as success (#1526). `runDelegatedCompact` treats that as a benign no-op (returns promptly so the deferred `clearCompacting` releases the hold; skips the "✅ compacted" notify and the #828 bounce). `/compact` reports "Nothing to compact — session too short."; auto-compaction logs and moves on.

**Delegated compaction summary chit (#1390):** after a successful wait, `runDelegatedCompact` asks the backend for its own summary via the optional `delegator.CompactionSummarizer` (`CompactionSummary(ctx)`) and passes it as the 3rd arg of `CompactionNotifyFunc`, so the gateway's `DetailAttacher.AttachDetail` path renders the same tappable summary chit as self-managed compaction (#1358). A missing summary or fetch error only drops the chit, never the notice. **ccstream:** CC never puts the summary on the stream; it writes it to the transcript as a user record flagged `isCompactSummary` right after the `compact_boundary` record. `ArmCompactionWait` records the transcript's size, and `CompactionSummary` reads only records past that offset (so an earlier compaction's summary is never returned), polls up to 3s for the write to land, and strips CC's model-facing preamble ("This session is being continued…Summary:") and trailer. **opencode:** `GET /session/{id}/message?limit=10` (newest 10, ascending), newest assistant message with `summary:true`/mode `compaction` created since `ArmCompactionWait`, its text parts joined. `summary` is a `SummaryFlag` (protocol.go) everywhere opencode messages are decoded: a boolean on an assistant message, a session-diff object on a user message, read as false so it never fails the decode (#2173). **Codex** does not implement it: its compaction output is server-side encrypted (`encrypted_content`, empty `message`), so no plaintext exists.

### System Injections Never Steer

System-initiated input — HTTP `/send` (`foci send`, cron keepalives), `/wake` fall-through, webhooks, scheduled wakes, restart changelogs, proactive warnings, error notifications, inter-session notifies (`session_notify`), and the periodic reflection/keepalive/memory passes — must never fold into (steer) an in-flight turn. It always waits gracefully for turn completion and then runs as a fresh, fully-tracked turn. Enforced at two layers:

1. **The session inbox is the queue.** Every system entry point routes through `Agent.Enqueue` with an `Envelope.Inject` (`InjectMeta{Trigger, Run, AskReqID}`), so the per-session worker serialises it with platform turns, defers it behind a pending `foci_ask` (**keyed to that ask's requestID** — see "Ask-deferral is keyed to the pending ask" below), and holds it through compaction. Sync callers (HTTP `/send --sync`, delegated reflection/keepalive passes that must complete before their scheduler continues) use `Agent.EnqueueInjectWait`, which blocks until the worker has run the closure. Gateway plumbing: `runAgentQueued` / `asyncDispatch` (`cmd/foci-gw/http.go`), `deliverToSessionChat` / `newSessionNotifyFn` / `newAsyncNotifier` (`agents_notify.go`), `handleDelegatedBranch` (`agent_sessions.go`). `EnqueueInjectWait` must NOT be called from the session's own worker (deadlock) — nested same-session system turns invoked from a turn's post-phase (pre-compaction memory, session-end memory) call `HandleMessage` directly.
**Ask-deferral is keyed to the pending ask (#1712).** `runInject` defers a non-control injection only when the session has a live (unpaused, on-screen) ask AND the injection either carries no `AskReqID` (a proactive interruption) or carries the requestID of *that same* ask. An injection tagged with a DIFFERENT ask's requestID is the **result** of an ask that has already resolved — `foci_ask`'s answer batch or grader verdict, delivered via `newAskDeliverFn` (`agents_notify.go`) which threads `AskDeliverFn(sessionKey, requestID, message)` into `InjectMeta.AskReqID` — and is released immediately. Blanket deferral was the whole of #1712: a quiz agent answers-then-immediately-asks, so the verdict for ask N is routinely ready seconds *after* ask N+1 opens, and holding it behind N+1 left the agent permanently one verdict behind (3h30m observed, unbounded if the user stops answering, since the held verdict is what would have told the agent the previous group was graded). The complementary half is `DrainDeferredInjects(sk, reqID)`, called **per ask** from `WithOnResolve` — it re-enqueues only the entries whose `deferredInject.heldBy` matches, so resolving one of two coexisting asks cannot release what the other is still holding. The same-ask race the gate was written for stays closed (an injection tagged with the *still-pending* ask is still deferred); exempting `ask_grader` wholesale was considered and rejected for exactly that reason.

2. **`ImmediateInject(SourceSystem)` at the backend.** `RunInference` classifies the turn via `isInteractiveTrigger` (`internal/agent/context.go`): only registered platform triggers (telegram/discord/app) and `voice` count as interactive. Interactive turns keep the fold path (`ImmediateInject(SourceUser)` follow-up / `SourceSteer`); everything else dispatches as `ImmediateInject(SourceSystem)`, whose backend implementations (all three) atomically begin a turn iff idle — the idle check and turn begin happen under one lock, so racing begins can't clobber each other's `TurnEvents` — and return `delegator.ErrTurnInFlight` otherwise. On rejection `RunInference` waits (`WaitForTurn`, `systemInjectRetryInterval` timeout backstop) and retries. This second layer covers turns the inbox worker can't see: backend-only runs (opencode shadow turns) and the nested post-phase memory turns above.

**Autonomous runs and the pending-work gate (#1068/#1070, spec §4).** A CC *autonomous run* — CC self-resuming with no foci turn open, triggered by a backgrounded subagent or `run_in_background` Bash completing — delivers its text to the chat even though foci opened no turn for it. Delivery works because `SessionEvents` binds once (at backend acquisition) to a per-session **router** (`Agent.sessionRouter`), not to a per-turn ctx sink; a system turn can therefore never rebind the session to its silent `NopSink` (the #1068 poison). Outside any turn the router falls through to a late-delivery sink resolved at emit time (`resolvingLateSink` → `route.ConnFor`), so autonomous text reaches the chat; `autonomousStreamed` dedups the streamed-vs-result copies.

But a system inject must be held not just while a run is *visibly active* — across the whole background-work window, from the moment a turn backgrounds a subagent/Bash until the resulting autonomous run completes. The backend reports this via the optional `delegator.AutonomousRunAwaiter` interface: `AwaitingAutonomousRun()` is true while the `SubagentTracker` has pending work (`Pending() > 0`), an autonomous run is active (`autonomousActive`), or the post-run chain grace (`autonomousInjectGrace`) is open. The tracker counts both Agent-tool subagents and `run_in_background` Bash (detected at the `tool_use` block via `ExtractBashBackground`); a missed completion can't wedge the gate forever because a max-age prune drops stale entries (`[cc_backend].background_task_max_age`, default 2h). A `task_notification` with any terminal status — `completed`, `failed` (a background Bash exiting non-zero) or `stopped` (TaskStop, or killed); `isTerminalTaskStatus`, #2022 — retires the entry it NAMES (`SubagentTracker.Remove(groupKey)`, matching whichever key the Add site used — Agent tool_use id, background-Bash tool_use id, or the stable groupKey on a reactivation); an identified notification whose entry is already gone removes nothing. Count-based `RemoveOne` survives in exactly one place: a notification carrying no identity at all. A `TaskStop` tool_use removes nothing itself — CC follows it with a `stopped` notification naming the task's tool_use_id (verified live, CC 2.1.280), and decrementing at the tool_use as well retired a second, unrelated entry. The distinction matters because the prune breaks Add/complete symmetry BY DESIGN — before #1770 a pruned entry's late completion retired an unrelated, still-running one and released this gate underneath it (observed 2026-08-21: two ~45-minute background commands against the 30m backstop). The inbox inject gate predicate is `IsInFlightDelivering(sk) || backendAwaitingAutonomousRun(sk)` (the latter a nil-safe, non-creating probe via `DelegatedManager.BackendAwaitingAutonomousRun` → `getManaged` — false for API agents and non-tracking backends). The adopted-run edge broadcasts via `InFlightWaitCh`, but a pending→run→clear transition has no channel, so the wait loop also polls at `injectGatePollInterval` (~1s). `tryBeginTurn` (the `SourceSystem` exclusive path) mirrors the same rejection: it returns `ErrTurnInFlight` while `Pending() > 0`. Scope is system input only — a platform (user) turn on the Driver path is never held on pending work (spec §3: user input adopts/folds delivering runs). The gate is one helper (`Agent.waitInjectGate`) applied at every `runInject` site — both the dequeue path and the post-batch `heldInjects` loop (injects drained alongside a platform turn), so an inject can't slip past by riding a platform turn. Residual: a µs-wide window exists between a task's `task_notification` removing it from the tracker and the chained run setting `autonomousActive`; Phase 1's router binding makes a system turn landing there non-catastrophic (it registers its `NopSink` post-accept and the run still delivers via the router fallback).

**Running-subagent list and stopping subagents alone (#2138).** Besides the one-line detail (`OnStatus`), `SubagentTracker` reports a structured list through `OnRunning` (fired just before `OnStatus`): each entry's key (the subagent's groupKey, or a background Bash's tool_use id), `Kind` (`agent`/`command`), `Model` (the Agent input's `model` alias, replaced via `SetModel` by the real model the subagent transcript tail names in `noteSubagentTranscriptUsage`), `SubagentType` and start time. Path: `SetOnSubagentRunning` (ccstream, opencode) → `DelegatedManager.SubagentRunningFunc` → `app.SetSubagents` → `convBinding.setSubagents`, which stores the list AND the detail derived from it (`delegator.FormatSubagentDetail`), so the detail-only `SetSubagentDetail` that follows dedups. The Activity frame keeps its flattened `kind`/`detail` and adds `agentKind`/`agentDetail`/`agentCommand` (the agent's OWN activity, never hidden by subagents; `agentCommand` is `toolformat.FullCommand` of the running tool, set via `setTurnTool`), `agentShell` (that tool is a shell call, `toolformat.IsShellTool`, #2140) and `subagents`; the roster snapshot mirrors them. `applyActivity` compares the whole frame (`activitySent`), so a tool change under running subagents now emits. `/stop subagents` → `DelegatedManager.StopSubagents` → the backend's optional `StopSubagents` (ccstream only): one `stop_task` control request per tracked `agent`-kind key, naming the CC `task_id` recorded at `task_started` (`recordTaskID`/`trackedTaskIDs`, dropped at the entry's `task_notification`); the task then ends through the ordinary `stopped` notification. An entry whose `task_started` has not arrived is skipped. `/stop commands` (#2140) → `DelegatedManager.StopCommands` → ccstream `StopCommands`: the same `stop_task` for the `command`-kind keys plus `mainShellCalls`, the main thread's running FOREGROUND Bash calls (noted in `OnAssistant`; CC registers a foreground call as a `local_bash` task once it runs past a couple of seconds, so it leaves at its `task_notification` via `forgetTaskID`, and an unregistered one is dropped at `OnResult`). A subagent's own Bash is not among them: stopping the subagent stops it. `/stop all` stops the subagents and the commands first, then the turn (without the cancel-a-pending-question shortcut). Verified live on CC 2.1.285 (#2140): `stop_task` stops a foreground or background subagent (with its commands) and a foreground or background command, and the turn carries on (a stopped foreground call returns "Exit code 137 [killed]" or "[Request interrupted by user for tool use]"); `interrupt` stops the turn, its foreground command and every subagent, but NOT a background command, and no `reason`/`scope` field keeps the subagents alive.

The `onAutonomousStart`/`onAutonomousEnd` adoption callbacks must fire in flip order or the adoption counter leaks: a start (reader goroutine) and an adopting end (turn goroutine) flip `autonomousActive` under `turnMu` in the true order, but firing after releasing the lock could reverse them (release-before-adopt → phantom `markInFlight(+1)` with no releaser → wedged gate). `setAutonomousActiveLocked` therefore *enqueues* each edge callback onto a per-backend FIFO under `turnMu`; callers then call `drainEdgeCallbacks`, which fires them under a dedicated `fireMu` in enqueue order. Enqueue-under-lock makes queue order == true flip order, so the drain can't reorder regardless of goroutine scheduling; `turnMu` is never held across a callback.

**Guard audit (which of the overlapping guards still earns its place).** The router binding (Phase 1) is the structural fix; the surrounding guards are re-justified against it: the **inbox adoption gate + `tryBeginTurn`** stay — the router limits blast radius but a system turn that *begins* mid-run still Registers its NopSink and captures the run's remaining output, so keeping it from beginning is load-bearing. **`autonomousStreamed`** stays — it's genuine dedup between live streaming and idle result delivery, not a race guard. **`markInFlight` adoption** stays — it's how an autonomous run counts as a turn for the keepalive/activity gates (spec §1) and how the inbox observes it. The **5s `autonomousInjectGrace`** is the one on probation: the pending-work gate now covers every *tracked* chain, so the grace only matters for untracked self-resumptions. It's instrumented (`tryBeginTurn` logs "grace blocked a system inject the pending-work gate would not have", once per window) so production frequency decides removal; kept for now because #1048 was observed in production and it's cheap (promote the hardcoded 5s to config only if it survives a release). **Accepted residual:** CC self-resuming for a reason foci can't see, in the ms before `session_state:running` arrives, remains possible — with the router binding the damage degrades from blackout to mis-attribution (output folds into the inject's turn but the router still delivers the user-facing text). Documented as a known bound, not a fifth guard.

The underlying rule for what may fold: a folded turn's own result is an empty accounting shell (the response merges into the in-flight turn's record and delivery stream), so folding is safe **iff the turn's result would have been delivered to the session's own platform stream anyway**. Platform chat and voice qualify — the user is watching that stream. Anything that routes its result elsewhere (inter-session `reply_to` replies, sync `/send`/webhook response bodies, scheduler completion waits) must never fold, or the consumer reads an empty result while the real response lands in the chat. Today "routes the result elsewhere" and "system-triggered" are the same set, which is why `isInteractiveTrigger` is the gate — judge any future change to the foldable classification against the delivery-destination test, not the trigger label.

The typed-answer intercepts in `RunInference` (pending `AskUserQuestion` / elicitation) are likewise interactive-only — a keepalive or notification text must never be consumed as a question's answer; it waits for the prompt to resolve and the turn to complete.

**Plan-cancel-by-message (#858):** a pending **ExitPlanMode** permission blocks the session — CC waits for Allow/Deny and ignores stdin until it answers, so a steered or queued message would either hit ignored stdin or wait indefinitely (the ~20-min "hung typing indicator" symptom). UNLIKE a normal tool permission — which a follow-up message keeps queuing behind via `WaitForPermission` — a typed message during *plan* approval is treated as revision feedback. Before the steer/queue routing, `Enqueue` checks (for an active turn, text-only) whether the session backend implements `delegator.PlanResponder` and has a pending plan permission (`HasPendingPlanPermission` scans `pendingPerms` for `toolName=="ExitPlanMode"`); if so it calls `CancelPlanWithFeedback(reqID, text)`, which sends a `PermissionDeny` carrying the text as the rejection `message` (CC stays in plan mode and revises using the feedback), then fires the prompt's cancel listener via `outstanding.Cancel` so the Allow/Deny buttons edit to "❌ Plan cancelled by follow-up message" and `onEmpty` clears `permPending`. The message is consumed (it became the denial feedback) — not also re-sent as a turn. Scope is ExitPlanMode-only by design; every other permission still queues unchanged.

### Input Delivery Tracking & Redelivery (#2050)

Every user-role line foci writes to a backend that implements the optional **`delegator.DeliveryTracker`** (claude-code / ccstream only; opencode, codex, cctmux and the API loop do not) is tracked until the backend proves it consumed it. Scope is ALL conversation input: turn-starting messages, in-flight `SourceUser` follow-ups, `SourceSteer` steers, post-tool nudges (`hooks.go`) and the pre-answer re-dispatch (`complete.go`). Slash commands (`SourceCompact`/`SourcePass`) are commands to CC, not input, and are untracked.

- **Write (`ccstream/delivery.go`).** Each write carries a uuid in `UserMessage.UUID` (fresh, or `Inject.ID` on a redelivery). `registerInput` appends it to `Backend.pendingInputs` under `turnMu` — refused (`errInputsClosed`, wraps `ErrBackendClosed`) once `finalizeExit` has set `inputsClosed`, so every write either lands in the set finalize hands back or is refused and re-routed (the inbox's failed-steer fallback; `RunInference`'s refused-follow-up path re-`Get`s and begins a fresh turn). `DeliveryHooks.OnPending` fires before the write (write-ahead persistence). A write failing because the transport is gone stays pending and returns nil (finalize owns it); any other failure is dropped (`OnDropped`) and returned to the caller.
- **Ack.** `Reader.dispatchInputAck` decodes top-level `user_message_uuids` / `user_message_uuid` on any stdout record (CC stamps them on the `message_start` stream_event, `assistant` and `result` of the API response whose request carried the message; verified for primaries and folds on CC 2.1.280) → `Backend.OnInputAck` → `OnConsumed`. `command_lifecycle` records are NOT an ack (lost on kill at `queued` and `started`).
- **Idle (`onSessionIdle` → `sweepPendingAtIdle`).** Never hands back — CC can legitimately go idle holding a line written as the run ended. Inputs already in the transcript are marked consumed; the rest stay pending.
- **Death/close (`finalizeExit` → `handBackPending`).** After the process is reaped (or on the waiter goroutine if the 2s `exitCh` wait times out), pending inputs are checked against the dead process's transcript (`transcriptInputs`: `{"type":"user","uuid":U}` or `{"type":"attachment","attachment":{"type":"queued_command","source_uuid":U}}`; `queue-operation` records prove nothing). Found → `OnConsumed`. Not found (or transcript unreadable — at-least-once) → `Redeliveries++` and `OnUndelivered`, in write order, BEFORE `OnTurnComplete` so redeliveries queue ahead of later input. `delegator.MaxRedeliveries` (3) caps loops (`OnDropped` + ERROR).
- **Persistence (`agent/delivery_store.go`).** `DelegatedManager.installDeliveryHooks` (from `setBackendCallbacks`, not for batch sessions) keeps session_metadata `cc_undelivered` = JSON `[]PendingInput`: upsert on pending (keyed by id), delete on consumed/dropped. `OnConsumed` with refs → `DelegatedManager.InputConsumed` → gateway → `app.MarkMessageConsumed(conv, msg)` → durable `fap.MessageConsumed` frame (the app's ✓✓). `OnUndelivered` → `DelegatedManager.Redeliver` → gateway `deliverToSessionChat(ag, agent.WithRedelivery(ctx, r), agent.TriggerRedelivery, …)`. `RemapSession` moves the row.
- **Redelivery turn.** Trigger `redelivery` is a control trigger (not held behind a pending ask) and non-interactive (dispatches `SourceSystem`: never folds, waits for idle). `ComposePrompt` writes the input verbatim under a `[meta] redelivered=<cc-exit|restart>` line; `RunInference` skips it if the id is no longer in `cc_undelivered` (a `/reset` since), and otherwise injects with the original `ID`/`Refs`/`Attachments`/`Redeliveries`.
- **`/reset` drops, shutdown preserves.** `ResetSession` sets `managedBackend.dropUndelivered` before `Close` (the hook then drops instead of redelivering) and clears `cc_undelivered`. A shutdown's handback redeliveries are refused by the draining inbox, so the rows survive and `RestoreUndelivered` (startup, after `plat.StartAll`) re-sends them after a `TranscriptChecker` (`ccstream.InputInTranscript`) check.
- **Stub fidelity.** `cc-stub` echoes `user_message_uuids` on its assistant/result envelopes; `crash_on_unconsumed` / `crash_after_consuming` script fields model a death before/after the fold (L2 `steer_replay_test.go`, `delivery_ack_test.go`).

### Backend Watcher — tmux (`internal/delegator/cctmux/watcher.go`)

The tmux backend's session watcher tails Claude Code's JSONL session file via fsnotify. It converts raw JSONL events into structured callbacks (assistant text, turn completion, usage, agent status). For the stream-json backend (ccstream), see the [ccstream Backend](#ccstream-backend-internalbackendccstream) section below — it receives these events directly on stdout rather than from a file watcher.

**Subprocess startup:** On `Backend.Start`, cctmux spawns `claude` in a tmux window named `cc-{agentID}` in the agent's workspace directory via a login shell (`sh -l -c`). The concatenated system prompt (workspace `*.md` files + skills + environment block) is written to `{workspace}/character/.full-prompt` and passed via CC's `--system-prompt-file` flag. Session ID, if known from a previous run, is passed via `--resume <uuid>` so CC reattaches to the existing session rather than starting fresh. User messages and slash commands are paste-buffered into the tmux pane via `tmux load-buffer -` (piped from stdin — no temp files) followed by `paste-buffer -p` to deliver. Sessions are discovered lazily — the JSONL watcher is created on the first message, not at process startup, so launching never depends on knowing the session ID up front.

**Pre-send offset:** Before `ImmediateInject(SourceUser)` pastes the prompt into the tmux pane (via the internal `sendToPane` primitive), the watcher records the current JSONL file size. The watcher starts reading from this offset so it doesn't replay old content from earlier turns. Falls back to `-1` (tail from end of file) if the offset discovery fails.

**Synthetic response filter:** Claude Code emits synthetic messages (model: `<synthetic>`) such as `"No response requested."` and `"[[NO_RESPONSE]]"`. The watcher filters these at the event level — they never reach the reply callback.

**Typing indicator:** Both backends use `SetTypingFunc` to register a callback. Set to `true` when a turn begins (via `ImmediateInject(SourceUser)` at idle), set to `false` when `OnTurnComplete` fires. The platform `Connection.SetTyping(bool)` is stateful — `true` starts a periodic ticker (Telegram: 4s, Discord: 9s) that keeps the indicator alive until `false` is called. The ccstream backend also restarts the typing indicator on `OnAssistant` (mid-turn text) and `OnToolProgress` (heartbeats during long tools).

**Usage extraction:** Assistant messages in the JSONL carry a `usage` payload. The watcher extracts `TurnUsage` (InputTokens, OutputTokens, CacheCreationInputTokens, CacheReadInputTokens) from the last assistant message in each turn. This is reported via `TurnState.FinalUsage` on completion. The ccstream backend extracts the same from structured `AssistantMessage` objects on stdout.

**Per-turn completion callbacks:** `ImmediateInject(SourceUser)`'s begin-turn path registers a one-shot `OnTurnComplete` handler that fires when the turn ends (`end_turn` in JSONL for tmux, `ResultMessage` on stdout for ccstream). The callback sets `TurnState.FinalText` and `TurnState.FinalUsage`, then closes `TurnState.CompletionChan` — triggering the post-turn goroutine (save, metadata, compaction, logging). Both backends carry it on `Inject.Turn` (`TurnEvents` — per-turn bookkeeping); ccstream fires it from `OnResult`, cctmux from its JSONL watcher's `fireTurnComplete` on `end_turn`.

**Agent spawn tracking:** The tmux watcher tracks pending `tool_use` calls for the Agent tool. The ccstream backend receives task lifecycle events (`task_started`, `task_notification`) as system messages. Both report status via the `onAgentStatus` callback, allowing the platform to show agent activity state.

**A subagent tail ends on a marker in the FILE, not on the stream event (#1938).** `task_notification:completed` tells the tail to *finish*, not to *stop*: it arrives on CC's stdout stream while the transcript is appended to a FILE, with no ordering between the two channels, so the last record can still be in flight. The old single post-stop `drain()` therefore lost exactly one record — the run's final completed message, which carries the accumulated `cache_read`. Measured live twice, both off by one (12 records read as 11; 15 read as 14; file mtime postdating the record's own timestamp by ~110ms). On stop the tail now drains until the transcript **ends at rest** — its last conversational record (assistant or user; attachments don't count) is an assistant message whose `stop_reason` is **terminal**, non-nil and not `tool_use`, because `tool_use` means the assistant will be called again, or a user record CC marks `toolEndsTurn` (`transcriptLine.endsRun`, shared with the Workflow tail) — or until `subagentTailSettle` (3s) expires, whichever is first. "Ends at", not "has seen": a run can write an end_turn and carry on (a Stop hook sends it back for SubagentHandback; a SendMessage reactivates it, and a reactivation tail re-reads run 1 from byte 0), and the first version's sticky "saw a terminal record" flag let those tails return on the first post-stop drain, losing the final record again (agent-aed9b9fd…: 49 records, `closed … lines=48`, file mtime 220ms after the task_notification). **A hand-back ends the run (#2124).** From CC 2.1.285 a subagent's run stops at its `SubagentHandback` call, and a Workflow agent's at its `StructuredOutput`: the tool_use is written with a null `stop_reason`, its tool_result carries `"toolEndsTurn":true`, and no assistant record follows. Without the marker every such tail waited out the full settle and logged `no terminal record within`. Before 2.1.285 a hand-back's result was unmarked and a final end_turn followed it, so the marker cannot close a tail early. A user record's `content` can be a plain string, so it is kept raw and decoded only for text delivery. The happy path ends early and costs nothing; a subagent that never writes a terminal record (killed, errored, rate-limited) still terminates. **The terminator and the data now arrive on the same channel, so they cannot race.** Note `TestSubagentTail_FinalizeDrainsRemainder` looks like it covers this and cannot — its append is a synchronous in-process write completing before `finalize()` is called, so it passes whether or not the race exists; the real regression test writes the terminal record from a separate PROCESS after `finalize` has been entered. **Backend teardown is the exception (#1202).** `stopAll` (from `closeInner`) marks each tail `teardown` before stopping it, so the tail reads what the transcript already holds and exits without waiting for a terminal record, and `stopAll` then waits for every tail to exit. It used to return at once, leaving each tail to run its settle drain, up to 3s, calling `OnSubagentText` and the ledger feed after `Close` had moved on.

**One stop site for a subagent tail: `task_notification:completed` (#1934).** The Agent `PostToolUse` hook does NOT stop a tail — it only clears the pending foreground expectation (`clearPendingForeground`). Measured on CC 2.1.261 (`timing.sh`, 4 scenarios): a BACKGROUND Agent `PostToolUse` fires at **+0.03s**, the instant the task is launched, with the entire run still ahead of it; a FOREGROUND one fires **~30ms AFTER** the subagent has genuinely ended, by which time `task_notification:completed` is already arriving. So stopping there is fatal for one kind and redundant for the other. **The foreground/background label is no longer consulted when stopping**, only when deciding whether to forward text — which is what it was ever able to answer. Two earlier attempts stopped the tail at `PostToolUse`: the first called `finalize()` unconditionally (harmless until `85f5ffb8` began tailing every subagent for usage); the second (#1924) made it foreground-only, which looked right and was inert, because `ExtractAgentBackground` read an ABSENT `run_in_background` as foreground and the Agent tool **backgrounds by default** — so every ordinary subagent was labelled foreground and killed anyway. `ExtractAgentBackground` is now tri-state (absent ⇒ background) and can no longer be aliased to `ExtractBashBackground`, whose default is genuinely foreground. **One case where the Agent Post hook IS the end (#2104):** an Agent call that errors before any `task_started` bound a run to its group (`taskBoundForGroup`), e.g. CC's "Concurrent subagent limit reached" refusal. Its `PreToolUse` already sent `SubagentStart` and `OnAssistant` already `Add`ed it to the tracker, but no task exists, so no `task_notification` will ever come; `endUnlaunchedAgent` removes the tracker entry and sends `SubagentEnd(groupKey, 1)`. An errored result for a task that DID start still ends only on its notification. There is no tail to stop: tails start at `task_started`.

**Why a subagent produced no usage: the tail decision now narrates (#1934).** Every path by which `onTaskStarted` declines to tail was previously silent, so "this subagent contributed no tokens" was indistinguishable from "no subagent ran" — and `waitForFile` reported ONLY on its 60s deadline, so any tail stopped earlier (i.e. every subagent finishing inside a minute) vanished without trace. `handlers.go` now logs which of the three outcomes occurred (missing ids / no transcript path / starting, with the path), and `subagent_tail.go` logs the open, the stop-before-open, an already-running no-op, a `finalize` that found no tail, and at close `lines=` (read, each handed to the ledger adapter) `terminal=`. So a subagent with no calls in the ledger is one `grep <group>` away. `subagent_harness_test.go` drives the real handlers with the CC event sequence (Agent + background-Bash `task_started`, a transcript on disk, `task_notification`, `result`) and asserts which tails start, which subagent calls reach the ledger, and these log lines — the millisecond stand-in for a 100-second live probe. Read those before theorising: a day of production probing in 2026-09 went to questions any one of these lines would have answered. **CC emits `system/task_started` for `run_in_background` BASH tasks as well as for Agent subagents** (and for other non-Agent task kinds). The event's `task_type` says which: `local_agent` for an Agent spawn, `local_bash` for a background Bash (captured live, CC 2.1.280). A Bash task has no agent transcript, so `handlers.go` skips the tail for `local_bash` only and logs the skip at DEBUG (#1935). Every other kind, including an unknown one or an EMPTY `task_type` (a CC predating the field), still tails: a wasted 60s tail is cheap, and a skipped real subagent silently loses its usage. **`task_notification` sends `SubagentEnd` only for a group that got a `SubagentStart` (#2010).** CC notifies for every task kind, including a subagent's own Bash that CC auto-backgrounded, and the notification carries no `task_type`; a Bash never gets a start, so its end closed a group the app never had (837 of 844 orphan `subagent.end` frames in the week to 2026-09-25). `subagentStartEmitted(groupKey)` reads the `markSubagentStarted` set, which the hook start, the `task_started` fallback and a reactivation start all mark. The tracker `Remove` still runs for every terminal notification, so the inject gate is released as before.

**A subagent tail is keyed by the subagent, not by the run (#2056).** A `SendMessage` resume's `task_started` carries the SendMessage `tool_use_id`, and the tail used to be started under it. That key is the actor the tail hands the ledger, so every run after the first was booked under a SendMessage id (live, pre-ledger: 8 of 113 delegations split over 2-3 rows), breaking #1946's "the actor names THIS subagent"; and the resume's `task_notification` finalizes the RESOLVED group key, so it never stopped that tail at all. `task_started` now resolves the run first (`nestedTask` / `onTaskStarted`) and starts the tail under the subagent's own group key (the original Agent `tool_use_id`, or a nested subagent's own Agent id), the same key its `task_notification` finalizes; only a task neither identifies keeps the raw id. Consequence: within one CC process a resumed run's calls also book on the SPAWNING turn (the adapter's agent state is first-sight per agent), like any background run that outlives its turn.

**A reactivation tail books only its own run (#2057).** A run-2+ tail reads the transcript from byte 0 and hands every line to the ledger adapter, which books each call once per process (by message id) and skips any line billed before the process launched. So within one CC process run 1 is not booked again, and after a foci restart or a CC relaunch inside a running foci (the post-compaction `BounceSession` close + `--resume`, an idle close, a crash respawn: a new Backend and a new process) run 1's lines predate the launch. The pre-ledger path needed a file-order gate (`runStartGate`) for the relaunch case, after six reactivated subagents re-booked 15.2M cache tokens of earlier runs on 2026-09-27 (#2087); the adapter's launch boundary replaced it (`TestSubagentRereadIsHarmless`, `TestSubagentLineBeforeLaunchIsSkipped`).

**Subagent reactivation (#1355).** A background subagent can run more than once: the initial `Agent` spawn, then any number of `SendMessage` resumes. The STABLE identity across a resume is the `task_id` — the `tool_use_id` CHANGES per run (a resume's `task_started`/`task_notification` carry the *SendMessage* block's id), while the subagent's text keeps the ORIGINAL Agent `tool_use_id` as its `parent_tool_use_id` group key. Keying lifecycle on `tool_use_id` therefore left a resumed subagent invisible (tracker never re-`Add`ed; the app's group showed "completed" though work continued). ccstream now maps `task_id → {groupKey, runIndex}` (`subagentRuns`, bound at the first `task_started`; `handlers.go`): a subsequent `task_started` for a known `task_id` bumps `runIndex`, re-`Add`s the tracker (chip re-opens), and emits a fresh `SubagentStart(groupKey, runIndex, prompt)`; a `task_notification:completed` maps `task_id → groupKey` so `SubagentEnd` closes the right run (not the resume's fresh id). The reactivation prompt is captured from the `SendMessage` block's `input.message` (keyed by `to == task_id`); run 1's prompt from the `Agent` block's `input.prompt`. **Run 1's `SubagentStart` fallback (#1425).** Run 1's start is normally emitted by the `Agent` `PreToolUse` hook (fires at the tool_use itself), but that hook drops for ~7% of background subagents under bursty/concurrent dispatch (#1423), which uniquely orphaned the run (text+end are native-stream, so they still arrive, but no start → the app had no chit to attach them to). So the first `task_started` ALSO emits a fallback run-1 `SubagentStart` (label/prompt from the `agentLabels`/`agentPrompts` stashes primed at the `Agent` block), deduped against the hook by `markSubagentStarted(groupKey)` — a check-and-set both emit sites call, so exactly one start fires whichever wins the race. Both emit sites log (`signal=agent_pre_tool_use` / `signal=task_started_fallback`). The hook path ALSO sources its prompt from the `agentPrompts` stash (`getAgentPrompt`, falling back to its own hook-payload `input.prompt` only when the stash is unset): the `PreToolUse` payload's prompt can arrive BLANK for a large prompt that outraces the tool_use stream, and because the blank hook still WINS the race it suppresses the stash-backed fallback and would orphan the prompt (the client drops an empty `SubagentStart.prompt` and no later frame recovers it). Sourcing both emit sites from the same authoritative stash (primed from the COMPLETE `Agent` block in `OnAssistant`) closes that gap. **Post-restart identity rehydration (#1433).** The run maps (`subagentRuns`/`agentLabels`/`agentPrompts`) are in-memory only, and `claude --resume` keeps the same session uuid but does NOT re-stream the historical `Agent` tool_use block — so after a foci restart a `SendMessage` follow-up to a pre-restart subagent reaches `onTaskStarted` as an untracked `task_started` whose id has no live stash, which would bind `groupKey =` the SendMessage id and emit a BLANK fallback start (the visible symptom of a pre-existing gap #1425 merely surfaced). `onTaskStarted` now treats an untracked `task_started` as a genuine fresh spawn ONLY when its `tool_use_id` has a live `agentLabels` stash; otherwise it rehydrates the original identity from CC's on-disk `agent-<task_id>.meta.json` sidecar (`{toolUseId (=groupKey), description (=label)}`, reachable post-restart via the stable session uuid; `loadSubagentMeta`/`rehydrateRunLocked`) and treats the follow-up as a REACTIVATION under the ORIGINAL group key; if no sidecar identifies it, NO start is emitted (a blank start is worse than none). `stashResumePrompt` rehydrates likewise so the follow-up prompt survives. The client also self-heals a provisional chit off the first orphaned `SubagentText`/`SubagentPrompt` and adopts a late/fallback start (#1422/#1424), so the two fixes are defence-in-depth. `SubagentStart`/`SubagentEnd` carry `RunIndex`+`Prompt`, and `SubagentText` carries `RunIndex`, through `turnevent` → `SubagentDeliverer` → the `subagent.start`/`subagent.text`/`subagent.end` FAP frames (additive-optional fields), so the app can draw per-run chits over one continuous, divider-split view. A text block's run is resolved by `runIndexForGroup(groupKey)` — the subagent's text keeps the ORIGINAL Agent `tool_use_id` as its group key across resumes, so it maps to the run entry whose `groupKey` matches (untracked → 1, the client's default). OpenCode has no reactivation and always emits run 1. **The subagent lifecycle arrives on `item/completed`, not `item/started` (#1589).** Codex 0.145.0 delivers every `subAgentActivity` kind — `started`, `interacted`, `interrupted` — on `item/completed`; `item/started` carries only `agentMessage`, `reasoning` and `userMessage` (verified against a live app-server across spawn → follow-up message → close). foci watched `item/started` for `kind=started`, so `subagentTracker.start()` never ran: no run opened, no 500 ms poll, no `OnSubagentStart`, and the later `stop()` had nothing to end — the whole codex subagent display was inert, along with the #1571 cursor and #1576 queue layered on it. Both notifications now feed one `openSubagentRun` helper (`handlers.go`) rather than the handler moving between them: `start()` is idempotent for an already-active child, so a release emitting on either or both behaves identically. **Those kinds track what the PARENT does to the child** (spawn / message / close), not what the child does — a child the parent never touches again emits nothing further, which is why `finishAll` is still the only terminator. **`collabAgentToolCall` is logged, never interpreted (#1590).** foci used to drive the whole subagent UI from these (`spawnAgent` opened a run, `sendInput`/`resumeAgent` emitted prompts, `closeAgent` ended it, plus an older-protocol branch synthesising text+end from `agentsStates`). That was written from codex's type definitions, not observed traffic — and codex does not send them: across **0.144.5 and 0.145.0**, with collab mode genuinely on (**`collab = true`**, NOT `collaboration_mode`, which codex silently ignores along with any unknown key) and the model demonstrably calling `collaboration.spawn_agent`/`send_message`/`interrupt_agent`, a spawn → follow-up → close sequence produced **zero** `collabAgentToolCall` items in the notification stream and zero in either thread's history. openai/codex#31300 canonicalised these for the **v1** collaboration tools; current codex uses MultiAgentV2, which reports through `subAgentActivity`. `logUnhandledCollabItem` now dumps the raw item at **WARN** — deliberately the level that reaches an operator via `log.SetWarnHook`/`notify.inject_chat_warnings` — so the first real specimen surfaces with its payload instead of being consumed by handling built on assumptions; it still counts toward `TurnResult.ToolCalls`. Removing the interpretation orphaned the #1576 prompt queue (`prompt`/`pendingPrompts`/`startPrompts`) and `identityFor`, whose only caller it was; all are gone. **`OnSubagentStart` therefore carries an empty prompt** — `subAgentActivity` has no prompt field (`{type, id, kind, agentThreadId, agentPath}` is the whole payload), so `agentPath` is the only label available.

**`subagents` and `collab` are two different codex features** that foci sees through one code path. Lifecycle-wise that is safe — both emit byte-identical `subAgentActivity` items — but their child threads differ: a **collab** child is a FORK of the parent carrying its entire prior conversation, while a **subagents** child has only its own history. That fork is why foci no longer reads a child's thread at all: flattening every turn and emitting each `agentMessage` from cursor 0 replayed the parent's own transcript into the subagent panel (#1592). Consuming the child's event stream instead (#1588, above) delivers only what the child itself produced, so the fork is harmless. Older Codex responses without `receiverThreadIds` retain the legacy self-contained run-1 handling. **A codex subagent is driven by its child thread's own event stream, not by polling (#1588).** A subagent runs in its own codex thread, and the app-server streams that thread's ENTIRE lifecycle down the SAME connection as the parent's, tagged with the child's `threadId`: `thread/status/changed` idle→active, `turn/started`, `item/agentMessage/delta`×N, `item/completed`, `thread/status/changed`→idle, `turn/completed` (verified live on codex 0.144.5 AND 0.145.0, for both the subagents feature and collab mode). Because foci registers only its OWN sessions' threads, a child `threadId` resolves to no facade, so `dispatch()` used to fall through to the process **owner** — handing the parent its child's `turn/completed` (**completing the parent's live turn, delivering the child's answer as the parent's**), the child's message deltas (streamed into the user's chat as the parent's text) and the child's token usage. Same class as the batch-thread leak fixed in `f3381eb6`. `dispatch()` now routes a recognised child (`subagentTracker.isChild`) into `handleSubagentNotification`, which consumes by default: `item/completed`+`agentMessage` → `OnSubagentText`, `turn/completed` → end the run, everything else swallowed with a debug log. **The child's own `turn/completed` is the completion signal.** `e7a519a6` ended runs at the parent-turn boundary because `SubAgentActivityKind` is `{started, interacted, interrupted}` with no terminal variant — true of that enum (codex source has exactly three emission sites, all parent-side tool handlers: `multi_agents_v2/{spawn,message_tool,interrupt_agent}.rs`) but it was never the only channel. A live probe showed a parent's turn completing with its child still working, so `finishAll` (and `onTurnStarted`'s `stopAll`) reported children as finished while they were going; both are gone, because a child outlives its parent's turn in BOTH directions. **The 500 ms `thread/read` poll is deleted, and #1571/#1592 with it**: it solved a problem that did not exist (the output was already streamed) while creating two — a full re-read from index 0 needs a de-dup cursor (#1571), and a **collab** child's thread is a FORK of its parent, so re-reading replays the parent's own transcript into the subagent panel (#1592). Consuming the stream has neither, since each item arrives exactly once and only the child's own items do. `readSubagentThread`, `deliverSubagentItems`, `advanceCursor`, `pollLoop`, `finishAll`, `seenItems` and `subagentItem` are all removed; `subagentPoll` became `subagentRun` — identity, not machinery.

**Codex token usage is per-API-CYCLE, and foci sums it (#1855).** `thread/tokenUsage/updated` fires once per model cycle, not once per turn: a turn running three shell commands and then answering emits FOUR (probe-verified on codex 0.145.0, `clutch/skills/codex-backend-debugging`). Each carries `tokenUsage.last` — that cycle's own figures — and `tokenUsage.total`, a running sum of every `last` for the **THREAD's whole lifetime**, which does NOT reset between turns (62,768 at the end of turn 1, continuing to 99,008 through turn 2 on the same thread). So neither field is a per-turn total: `last` is one cycle and `total` is the whole thread. `onTokenUsage` (`codex/handlers.go`) therefore keeps both scopes, exactly as ccstream does — `stashedUsage` is the LATEST cycle's fill (what `GetContextWindow` and compaction read; never summed) and `turnCalc` is a `modelinfo.TokenCounts` accumulating every cycle's `last` (what `TurnUsage.Turn` carries and `CalculatedCostUSD` is priced from, so the persisted row re-prices to its cost). `OutputTokens` is the summed one, as everywhere else. Both are cleared by the single `resetTurnUsageLocked` at every turn boundary — `beginTurn` (`inject.go`), `completeTurn`, and a change of the notification's own `turnId` (which catches a turn foci never opened) — so a field added to that group is reset everywhere for free. Before #1855 only `stashedUsage` existed and each notification overwrote it, so a multi-cycle turn was logged as its FINAL cycle alone: the captured 4-cycle probe turn spent 151 output tokens and would have recorded 5.

**Nested subagents (#1554, `ccstream/nested_subagents.go`).** A subagent can itself call `Agent`, spawning a grandchild. Only depth-1 subagents get a chit. CC's `task_started`/`task_progress`/`task_notification` carry NO parentage (re-verified live on CC 2.1.280), so depth is recorded at SPAWN into `nestedAgents` (nested Agent tool_use id → spawner's groupKey) from two signals: the nested `Agent` tool_use block, which reaches the parent stream tagged `parent_tool_use_id` = the spawner's groupKey and precedes the nested `task_started` (`OnAssistant`'s sub-agent branch); and the nested `Agent` `PreToolUse`, whose `agent_id` is the spawner's task_id (`handleHookResponse`, before the sidechain filter). `nestedTask(task_id, tool_use_id)` then classifies each `task_*` event, remembering the task_id in `nestedTasks` so a `SendMessage` resume (new tool_use id) is still recognised; after a foci restart it falls back to the meta sidecar's `spawnDepth`/`parentAgentId`. For a nested task: `task_started` still starts its transcript tail (its USAGE is real), but skips `onTaskStarted` (which would otherwise rehydrate it from the sidecar as a spurious reactivation chit); `task_notification` finalizes only the grandchild's own tail and sends no `SubagentEnd` and no tracker removal, and never touches the spawner's group. Text tagged with a nested group (a background grandchild's text carries its OWN Agent id) is re-attributed to the depth-1 ancestor via `topLevelAncestor`, or dropped if none is known. **Delivery (probed live, CC 2.1.280, 2026-09-24):** CC delivers a grandchild's completion notification TWICE: to its spawner, but only while the spawner is mid-turn, and ALWAYS to the main thread. So the main-thread `task_notification` for a nested task is expected (and is what the suppression above handles). A spawner that ends its turn to "wait" for background children never sees their results. That was the whole cause of the 2026-09-22 report of results "reaching the wrong parent": the spawner had gone idle before any child finished.

**Messaging a STILL-RUNNING subagent (#1419)** is a distinct case from resuming an ended one: CC never refires `task_started` for a `SendMessage` sent before `task_notification:completed` — the message folds into the live run with no new stream event — so the stash/reactivation path never fires. Instead ccstream emits `OnSubagentPrompt`/`SubagentPrompt` (`subagent.prompt` FAP frame; `subagent_runs.go` tracks an `active` flag + `activeRunForTask` to choose the path in `handlers.go`), which attaches the follow-up to the ALREADY-OPEN run at its CURRENT `runIndex` — deliberately NOT a new `SubagentStart`, which would open a run that never receives its own `SubagentEnd` (CC sends one completion for the whole continuous execution) and spin forever. The client renders it as another `role="prompt"` block inline in the open run. telegram no-ops it (as with `SubagentStart.prompt`). The production `SessionEvents` (`Agent.AttachDelivery`, `inbox.go`) must set `OnSubagentPrompt` beside the other subagent closures: it was missing from #1419 until #2092, so ccstream's nil-check made every follow-up a silent no-op (tests passed because they build their own `SessionEvents`).

**Permission auto-approval:** When CC sends a `can_use_tool` permission request, the ccstream backend's `handleToolRequest` first checks against compiled auto-approve rules (from `[permissions]` config). Rules are assembled at startup by `buildAutoApproveRules`: built-in common readonly tools/commands (if `auto_approve_common_readonly` is true, default on), an opt-in built-in safe-write list of side-effecting commands (`curl`, `wget`, `mkdir`, `touch`; enabled by `auto_approve_common_safe_write`, default off — these rules are not path-scoped, so the operator must trust the agent not to target paths outside its workspace), workspace-scoped Edit/Write access, and user-configured patterns from global + per-agent config (union). **Two checks guard against foci substituting the binary behind an approved command** (`internal/execguard`, shared so they cannot drift). *At match time* — the control — `matchBashSegment` vetoes any Bash segment whose command token is substitutable, regardless of which rule matched (including the built-in read-only group): the executable file is writable, or the directory holding it is writable (replaceable), or — for a symlink — the directory holding the LINK is writable (re-pointable), or for a bare name the PATH winner fails any of those tests. Every path is resolved through `filepath.EvalSymlinks` first, so the file/directory tests judge the bytes that actually execute; before #1893 `access(2)` followed the link while `filepath.Dir` did not, so a read-only target in a writable directory passed. This runs against the live filesystem on every approval, so a shadow planted after boot is caught with no restart. *At startup* — hygiene only — `cmd/foci-gw/main.go` additionally DROPS user `auto_approve` entries that already fail the same test (`config.DropSubstitutableAutoApproveRules`), warning once each so a permanently-untrustworthy entry is reported loudly rather than silently never matching, and warning separately for any entry naming a bare command **no PATH directory provides** — the guard cannot judge a file it cannot find, and its silence about such an entry is indistinguishable from approval. A writable PATH directory not containing the command is deliberately not a finding (a shadow that could be created, not one that exists). **The PATH both checks search is the OPERATOR population's** (`execguard.Live()` → `procx.PathDirs(procx.Operator)`), read at CHECK time and never captured once. Asking for the population BY NAME is the correctness condition: the guard's job is to predict what a tool shell resolves a bare name to, so it must search the same PATH that shell searches, and naming it means there is no list to keep in sync. Historically `foci-gw`'s own PATH was written twice — the unit's `Environment=PATH` (rendered from the Makefile's `SERVICE_PATH`), then `shellenv.Apply()` inside `main()`, which replaced it with the operator's dotfile PATH. A package-level `var guardEnv = execguard.Live()` pinned the FIRST of those, because Go runs package initialisers before `main()`, and so judged `/usr/bin/git` while an agent-writable `~/scripts/git` ran (#1900). #1914 removed the second write entirely: the daemon's own environment is no longer mutated, so `SERVICE_PATH` is now the TRUSTED population — the PATH foci's own `sh`, `git`, `tmux`, `sudo` and capture shell resolve on — and every directory on it must be one foci cannot write. `reportPopulations` asserts exactly that at startup and names the one allowed exception (`~/.local/bin`, which Claude Code's updater must be able to rewrite). Keeping `SERVICE_PATH` a superset of what `~/.shellcommon` builds is defence-in-depth (#1898), not the guard's correctness condition. Note that `/proc/<pid>/environ` CANNOT audit any of this: it records the exec-time environment and is never updated by `os.Setenv`, so it always shows the unit's PATH. Bash builtins and glob tokens are exempt — the shell resolves those ahead of any PATH search, or they name no single file. Read/Edit/Write entries are exempt (data, not code). For Bash commands, the command is split on shell operators (`&&`, `||`, `;`, `|`) and every segment must independently match at least one Bash rule — this prevents `git status && rm -rf /` from being auto-approved by a `git *` rule. Matched requests are approved directly via `SendControlResponse` with an INFO log. Unmatched requests are forwarded to the user via the platform connection with an inline keyboard of choices (Allow, Deny, Always Allow). **A veto explains itself (#1906).** A denial by the substitutability check is not an ordinary no-match: a rule DID match and the guard overrode it, so the user is being asked to approve a command sitting on their own allowlist. `MatchWithEnv` therefore returns a REASON alongside the bool, non-empty only for that case; ccstream logs it at WARN and appends it to the prompt body, so the explanation arrives where the user already is rather than in a log they would have to go and find. The reason is carried through the walk on a `vetoNote` held behind a POINTER that `varCtx.clone()` shares rather than copies — clones are made for subshells and compound statements, and a by-value copy silently drops a veto raised inside one. This covers built-in, operator-written and workspace rules alike, because the veto is already independent of which rule matched; reporting at rule-assembly time (`buildAutoApproveRules`) was considered and rejected, since a startup list of permanently-writable commands is a standing condition that trains its reader to ignore it.

**AskUserQuestion handling:** When CC's `AskUserQuestion` tool triggers a `can_use_tool` request, `handleToolRequest` routes it to `handleUserQuestion` (`userquestion.go`) instead of the standard permission flow. The handler parses the questions from the tool input, stores a `pendingPermission` with question state (questions, current index, accumulated answers), and presents the first question as an interactive prompt with option buttons plus Cancel. For multi-question sequences, questions are presented one at a time; each answer advances the sequence. The user can also type a custom text answer (intercepted in `RunInference` before `WaitForPermission` blocks) or cancel via the Cancel button or `/stop`. When all questions are answered, the response is sent as `PermissionAllow` with `updatedInput` containing the original input plus an `answers` map (`{question_text: answer}`). CC receives this as the tool's input and returns the formatted answers to the model.

**Elicitation handling (`ccstream/elicitation.go`):** MCP servers can raise an `elicitation` control_request subtype when a tool call needs structured user input mid-turn. The reader dispatches these alongside `can_use_tool` and `OnElicitationRequest` builds a `pendingElicitation` (separate map from `pendingPerms` — elicitations aren't keyed to tool_use_ids). Two modes are supported: **form** walks the `requested_schema` one property at a time, presenting each field through the same `permPromptFn` platform callback used for permissions. Free-text fields accept typed answers via the same text intercept path as AskUserQuestion (`HasPendingElicitation` from `RunInference`); enum properties render as buttons; booleans render as Yes/No; once every field is satisfied, the accumulated answers are marshalled into a `content` object and sent back as a `control_response` with `action: "accept"`. **url** mode surfaces the URL with Done/Decline/Cancel buttons — Done sends `accept` with no content, while an out-of-band `system/elicitation_complete` notification from CC auto-resolves the matching (`mcp_server_name`, `elicitation_id`) entry without the user clicking Done. Unsupported or missing schemas fall back to a Decline/Cancel-only prompt (foci never synthesises field values it didn't collect). Decline and Cancel at any point short-circuit the walk and send the corresponding action with no content. Every resolver (a button, a typed field answer, `elicitation_complete`) claims the entry — lookup and removal under `elicMu`, with the field walk's writes in the same critical section — before responding, so racing resolvers yield exactly one `control_response` (#2070; `RespondToQuestion` claims its `pendingPerms` entry the same way). The drain hook fires only when both `pendingPerms` and `pendingElicits` are empty (enforced by the unified `OutstandingRegistry` — see below) so the platform's "has pending prompt" indicator doesn't flap mid-walk. The `delegator.ElicitationResponder` optional interface exposes `RespondToElicitation` / `HasPendingElicitation` to the agent layer, mirroring `QuestionResponder`.

**Outstanding-prompt registry (`internal/delegator/outstanding.go`, package `delegator` — a shared root type, not ccstream-specific):** All user-input prompts (permissions, AskUserQuestion sequences, MCP elicitations) share one `OutstandingRegistry` per Backend. Each `pendingPerms`/`pendingElicits` insertion is paired with a `Register(requestID, kind)` call; resolutions call `Resolve(requestID)`; CC's `control_cancel_request` calls `Cancel(requestID, reason)`. The registry provides three things on top of the kind-specific stores: (1) a multi-listener cancel fanout — the platform layer registers a per-prompt cancel callback via `Backend.RegisterPromptCancelListener` at the same time it sends the interactive UI, and the registry fires those callbacks (in registration order) when CC cancels the prompt before the user responds; (2) a registry-wide `onEmpty` drain hook (`Backend.SetOnPromptsCleared`) that fires only when ALL outstanding prompts have been removed — fixing a pre-Phase-2 asymmetry where `removePendingPerm` could trigger the drain while elicitations were still outstanding; (3) idempotent semantics — cancelling/resolving an unknown requestID is a silent no-op rather than a side-effecting fall-through. `DelegatedManager.RegisterPromptCancelListener(sessionKey, requestID, fn)` exposes the per-prompt registration to the agent layer; in `cmd/foci-gw/agents_delegated.go`, the platform closure that calls `SendInteractiveMessageWithID` registers a cancel listener that invokes `platform.CancelInteractiveMessage` to disable the orphaned inline keyboard.

### Backend Session Lifecycle

**Session ID persistence:** `SetOnSessionReady` registers a callback that fires when the watcher discovers the CC session UUID from the JSONL path. The UUID is persisted in the state store. On restart, `--resume <sessionID>` is passed to the `claude` command to reconnect to the existing CC session rather than starting fresh.

**Per-agent acquire serialisation (`internal/delegator/keyedmutex`, #1718).** Both pooled backends must satisfy one invariant — *use the existing server for this agent, or start one if none exists* — and neither did it atomically. `opencode.acquireServer` checked the pool under `serverPoolMu`, RELEASED it, spawned, then re-took it to insert; a second caller arriving in that gap read an empty slot and launched a second subprocess, and both opened the same SQLite store ("database is locked", 10 occurrences in 16 days, all on restarts). `codex.Start` already claims its `sharedPool` slot before launching, but its predicate `owner != nil && owner.IsRunning()` reads false between the claim and `b.running = true`, so a second caller OVERWROTE the entry and launched again — same defect, window of an exec rather than a 13s health probe, hence one occurrence ever. Both now take `acquireLocks.Lock(agentID)` around the whole check-then-create. **Serialisation, not `singleflight` dedup** (which `DelegatedManager.createGroup` uses, but keyed per *sessionKey*): a deduplicated second caller would never run its own `refCount++`. **Keyed, not one global lock held across the spawn**: `Start` gets `context.Background()` and `healthProbe` returns only on healthy-or-death, so one wedged subprocess under a shared lock would block every agent's acquire forever. ccstream is untouched — it has no per-agent server pool, which is also why its `OpenCleanupScope` is a no-op. opencode's post-spawn "someone raced ahead" branch is now unreachable and WARNs if entered, since reaching it means the serialisation failed.

**Per-instance exec bridge sockets:** Each delegated backend gets its OWN exec bridge socket, `exec-<session-key>-<gw-pid>-<n>.sock` (`NewSessionExecBridge` in `internal/tools/execbridge.go`) — unique per backend *instance*, not per session key. This is load-bearing on `/reset`: the dying session's backend is remapped onto a branch key to finish memory formation in the background while a fresh backend takes over the original key (see `Agent.BranchStrategyFor`'s session-end case). If both derived their socket from the session key alone they would share one socket, and reaping the dying branch would close the live session's bridge out from under it (#1120). The gateway pid isolates separate gateway processes sharing one `FOCI_TMPDIR` (the #804 test hijack); the `<n>` counter is the actual per-instance discriminator. Paths are never reused, so the former stable-path memo / `socketIsLive` hijack guard are gone. Sockets do NOT survive a foci restart — backends are killed+resumed, not reattached (the open #1101).

**Schema-driven shell functions:** Shell functions for `ExecExport: true` tools are emitted by `generateShellFunc` in `internal/tools/execbridge.go`. A small set of tools with custom UX (stdin reading, accumulator flags, subcommand dispatch — `web_search`, `memory_search`, `web_fetch`, `http_request`, `send_to_chat`, `todo`, `summary`, `spawn`, `tmux`) have hand-rolled cases. Every other tool falls through to `generateGenericShellFunc`, which emits a flag-parser for each schema parameter: snake_case keys become kebab-case flags, booleans are presence-only, strings consume two args, and required params trigger a usage line on missing. **A POSITIONAL param also gets a `--flag` arm** (#1778) — `foci_web_fetch --url X` and `foci_web_fetch X` are equivalent — and supplying BOTH forms in one command is an error (`__foci_<param>_via` records which form set the value; aliases of a positional carry the same guard). Before this the generic generator did it and the hand-rolled `http_request`/`summary` cases did not, so `--url` worked on one tool and was "unrecognized flag" on its sibling; where both forms were accepted they silently concatenated. The unrecognized-flag branch also prints the usage line naming the positional, since a "valid flags" list can never contain one. **A JSON-typed flag value is validated where the flag is given** (#1811): the shared `foci__json_arg` helper in the preamble checks parseability and the schema type before `jq --argjson` sees the value, so the error names the flag, the expected type, the actual type and the value. Without it jq printed `invalid JSON text passed to --argjson` twice — once for the failing assignment, once for the empty `$params` the failure left behind — naming neither flag nor value. The generic generator emits the check for every `object`/`array`/`integer`/`number` param, and the hand-rolled cases carry it on their own JSON flags; `todo --ids` is checked AFTER its comma-to-array normalisation, since the advertised `1,2,3` form is not JSON until then. `todo` is exempt: `action` must be read from `$1` before flag parsing because it selects the per-action flag allowlist. Both `--help` text (`generateHelpText`) and the body derive from the same JSON schema, so they cannot drift. **For sub-actioned tools the schema is not the whole story (#1786):** which flags a given action accepts is owned by `todoActions` (`Flags` per action), which drives the bash gate, the per-action usage lines, the rejection message, AND — since #1786 — the `[actions: …]` annotation on each flag in the help table. That annotation used to be hand-written prose inside the schema `description`, so it drifted: the table advertised `search --status` and `search --query`, the subcommand line omitted both, and the shell rejected both. `TestTodoHelpFlagActionsDerivedFromAllowlist` now fails if any description reintroduces action prose. Note the bash gate is skipped entirely when an action's `Flags` is EMPTY, so an empty allowlist means "accepts anything known", not "accepts nothing" — `TestTodoActionsAllowlistMatchesRealBehaviour` forbids it. `writeShellFuncs` calls `validateShellFuncSchemaParity` before writing — any tool whose schema gains a parameter without a matching `--<flag>` case arm in its body returns an error from `NewExecBridge`, surfacing the failure at production startup rather than at runtime. The write itself uses `O_EXCL` (#1501): the funcs path embeds the gateway PID and a per-process counter so it is never reused, and an existing file or symlink at it is therefore a hard error from `NewExecBridge` rather than a followed write.

**`StdinParam` and its two bounded reads (#1552).** A tool with `StdinParam` (today: `send_to_chat`) emits two mutually exclusive stdin branches, and `[ ! -t 0 ]` gates both. That test is isatty — "am I non-interactive" — and is equally true of a pipe with data, a pipe whose writer has not written yet, `/dev/null`, a regular file, and **an fd a supervisor merely left open**; only the first is what either branch means. The last one never reaches EOF, so both branches were once able to block forever. Both are now bounded, differently, because they want different things:
- **value supplied** → stdin is unwanted and is peeked at only to warn that piped content will be discarded. Bounded at a fixed `timeout 5`. Deliberately still a *wait* and not a non-blocking `read -t 0` poll: in `cmd | foci_send_to_chat --text x` the function reaches the guard before upstream writes byte one, so a poll skips the warning on the most ordinary pipeline there is.
- **value absent** → stdin IS the body and reading it is the job. Bounded at `timeout ${FOCI_STDIN_WAIT:-30} cat`, and on expiry it **warns on stderr and proceeds** with whatever arrived. The warning is the load-bearing half: a bare bound would let a `tail -f`/`curl -N`-style unending stream be truncated and sent looking like a clean success. `FOCI_STDIN_WAIT` is a test seam (the regression cases run it at 1s), not a documented user knob.

**Branching a delegated agent.** (Superseded the old "delegated agents return HTTP 400 for `/branch`" rule — they branch for real now. `Agent.BranchStrategyFor` picks per branch type; the legacy inject-in-place / independent-session strategies below are what a *non-branch-capable* backend still falls back to.)

`ForkSession` is the single routing point: an API agent gets a history-reading session branch, a delegated agent whose backend implements `delegator.BackendBrancher` gets a **real transcript fork** that starts with the parent's full context and leaves the parent running. Some backends can branch and some can't; both cases are supported, and the answer is a runtime capability check (`BackendCanBranch`) — deliberately not a list of backend names here, since such a list has already gone stale and mislabelled a backend that had gained the capability.

**What a ccstream fork appends (`forkTranscript`, `internal/delegator/ccstream/branch.go`; #1431, #2051, #2086, #2097).** The fork copies the parent's transcript, then appends ONE synthetic user message (`origin.kind=task-notification`) holding a `[fork boundary]` `<task-notification>` block with `<status>completed</status>` for every background task launched in the copied prefix with no resolution in it: async subagents (`toolUseResult.status=="async_launched"` + `agentId`) and background shell commands (`toolUseResult.backgroundTaskId` — `run_in_background`, or a foreground command its timeout moved to the background). A task counts as resolved when any task-notification naming its id sits in a user message or a `queued_command` attachment. Without the closures, the fork's CC resume-time orphan scan tells the branch the parent's live work "didn't finish before the previous session ended" (agents `failed`, shells `stopped`), which reads as a kill. CC needs a `<status>` element to count a notification as a resolution; the summary text claims no outcome. It is one message, not one per task, so the fork stays inside the API's cache look-back (CACHING.md "Session branching"). **Before that message, a fork taken mid foreground tool call gets a tool closure (#2097).** The copy can end on an assistant `tool_use` the parent is still running, whose `tool_result` only ever lands in the parent. `appendToolClosures` answers each call of the transcript's final assistant turn still lacking a result with ONE synthetic user message of `[fork boundary]` `tool_result` blocks (`is_error:false`, string `toolUseResult`, parented on the call's assistant line like CC's own results) saying the call belongs to the parent and must not be re-run; the task-notification message then chains off it, so it is never the record directly after a dangling `tool_use`. Without it CC answers the call itself on resume, as interrupted (2.1.280) or outcome unknown (2.1.281+), and in live probes the fork re-ran the parent's command on both. An orphan the conversation has already moved past (a later assistant turn or user prompt) is left alone.

`ForkSession` returns `ok=false` for **two unrelated reasons**, and conflating them was #1634:
- the backend cannot branch at all → no branch is possible;
- the backend *can* branch but the parent has **no backend session to clone** — never started, or `/reset`. `ForkParentSession` resolves a live backend's session id then the persisted `cc_resume_id`; with neither it returns `("", nil)`.

The second is not a failure: a branch of an empty parent is well-defined — a fresh session, exactly what the API path already produces. **`Agent.ForkOrFreshBranch`** encodes that policy (`branchKey == ""` with `err == nil` now means only the first case, plus an `inherited` bool so callers log honestly), and `POST /branch` uses it. Callers that genuinely *require* inherited context keep using `ForkSession`/`ForkBackendBranch` and treat `ok=false` as failure — **spawn**'s clone mode (whose promise is the parent's history) and **compaction-memory** (which summarises a transcript that must exist). `/facet` deliberately uses neither: it wants a branch even when the backend can't fork, so it keeps its own unconditional `CreateBranchWithOptions` fallback.

**Reclaiming those forks.** `Agent.CleanupEphemeralSessions` (daily, from the periodic runner's `maybeEphemeralCleanup`) deletes the backend transcripts of ephemeral sessions past `ephemeral_retention_days`, leaving the `session_index` rows as a historical record. **Each reclaimed transcript is stamped `swept_at` on its `backend_resume_history` row, and the sweep selects only sessions with at least one unswept row (#1801)** — without that, keeping the index rows forever meant re-selecting the same expired sessions every night, and since every backend reports an already-absent transcript as success, the re-attempts were counted and the nightly total meant "rows attempted" rather than "transcripts reclaimed". The same `EXISTS` clause drops sessions that never recorded a transcript at all, which is most index rows; those kept the candidate list non-empty and so kept an idle agent opening a cleanup scope nightly. A FAILED delete is deliberately left unstamped, so it stays eligible next run. It deletes via the same `BackendBrancher.CleanupSession` that reclaims a fork — but for backends where that is an RPC to a live server rather than a file delete, the sweep must first acquire one. That is the optional `delegator.RunningBackendCleaner` capability (the cleanup analogue of `RunningBackendForker`): the sweep opens ONE scope via `DelegatedManager.OpenBackendCleanupScope`, deletes every expired session inside it, and releases once. Backends deleting from disk (ccstream) don't implement it and get a no-op release. **Codex is
NOT one of them** — this line said it was, and that error is why codex was left out when
8590df7e fixed this for opencode: codex deletes a thread by `thread/delete` RPC to a live
app-server, so an idle agent's expired sessions could never be collected and the daily sweep
logged one WARN per session, permanently (24 in one sweep, 2026-08-26). It now implements
`OpenCleanupScope` via `Start(BatchOnly: true)` — the same acquisition an interactive session
uses, stopping once the connection is up and before a thread would be started. The sweep's
per-session `CleanupSession` runs on a FRESH backend that never ran Start, so it falls back to
the agent's pooled owner; without that fallback the scope would acquire a server nothing could
see.

opencode implements it by acquiring — and if the agent is idle, **spawning** — its shared pooled server, via `acquireServerFn` (the same `acquireServer` path `Start` uses). Without that, cleanup was permanently impossible for exactly the agents whose sessions were expiring: an agent idle long enough to have expired sessions has no server *because* it is idle, so every pass logged a per-session WARN and deleted nothing (#1707). The scope is opened only when the sweep has something to delete, so a quiet day spawns nothing. Two sites share the acquire-spawn-release shape — `Backend.Start` and `OpenCleanupScope`; `pooledServer` (never spawns) remains correct only for `ForkSession`, which runs mid-turn when the server is up by construction.

Legacy strategies a non-branch-capable backend still uses:
- **Inject into main session** — reflection and compaction-memory prompts go directly into the running CC session.
- **New independent CC session** — background tasks run on a fresh independent session; delegated consolidation and nudge extraction are batch runs (see "Batch runs" above).
- **Fall through to `/send`** — the `/branch` endpoint runs the turn on the parent instead, warning that the branch options were ignored. Reachable only for a backend that cannot branch; before #1634 it also caught every reset/never-started parent, silently polluting the session the branch existed to keep clean.

**/reset:** Archives the session in place and returns — it does not block on memory formation. The live CC backend (and its resume ID) is handed to the reflection branch via `DelegatedManager.RemapSession`; the reflection pass then runs on that branch in the background and destroys the backend when done. The main key starts clean — a fresh CC session spawns lazily on the next message. See `agent/lifecycle.go:ResetSession`.

**/stop:** Interrupts the current turn. Tmux backend: sends Escape×2 + Ctrl-C via `send-keys`. Stream backend: sends an `interrupt` control message over stdin. Both halt the in-flight inference/tool execution inside Claude Code.

**Compaction reload-bounce + resume nudge (#828 Part B / #845):** after a delegated `/compact`, CC keeps the same frozen system prompt, so memory/skill edits made during the session never reach the post-compaction context. `runDelegatedCompact` (`agent/compaction.go`) fixes this by bouncing the CC session after a successful compaction (gated on the per-agent `ReloadOnCompact` flag, default on):

- **`DelegatedManager.BounceSession(sessionKey)`** closes the backend but *keeps* the saved resume ID (factored out of `ResetSession` via the shared `closeManaged(sessionKey, clearResume=false)` helper), so the next message respawns CC with `--resume <same session>` — resuming the now-compacted conversation. Part A's `SystemPromptFunc` then rebuilds the prompt from disk on that respawn.
- **Prompt-change gate (#828 follow-up):** every CC session start fingerprints the prompt it launched with — `log.SystemHash` of the full effective prompt from `SystemPromptFunc` (environment block incl. `## Platform`/permission allowlist + character files + skill blocks) — stored as `systemPromptHash` on the per-session `managedBackend` record in `m.backends[sessionKey]`. At compaction, `BounceSessionIfPromptChanged(sessionKey)` recomputes the hash from disk and bounces *only* when it differs (returns whether it bounced). This catches character-file edits, skill add/remove (the skill list is in the prompt), and env-visible changes (permission allowlist, shell-tool set, platform claim) but **not** skill body edits (bodies load on demand, never in the prompt). The env block's platform comes from the durable chat claim, not connection liveness, so the fingerprint is stable across startup transients — a bounce always reflects a real change. With no `SystemPromptFunc` configured it falls back to an unconditional bounce. Unchanged prompt → no restart → seamless compaction (pre-#828 behaviour).
- **Self-injected resume nudge (#845):** a mid-task flow has no next message to drive the post-bounce respawn, so it would silently stall. `maybeInjectCompactionResume(sessionKey)` synthesises one — `compactionResumePrompt` instructs the model to resume if mid-task or emit the `NoResponseSentinel` if idle — injected via `AsyncNotifier.InjectToAgent`. It is gated twice: it fires **only** when `BounceSessionIfPromptChanged` actually bounced (no restart → nothing to recover from), and is suppressed when `Agent.InboxHasPendingInput(sessionKey)` reports queued/steer input (the user's own follow-up will drive continuation with real intent) or async self-injection is unavailable.

**Automated CC re-login on 401 (`internal/relogin`, #843):** when the shared CC OAuth credential can no longer be refreshed, the subprocess returns a 401 ("Failed to authenticate") and every CC agent is dead until re-authentication. The relogin package automates recovery:

- **Detection (ccstream only).** `isAuthFailure` (`ccstream/authfail.go`) matches a 401 at two sites — the error `result` in `OnResult` (`handlers.go`) and the subprocess stderr/exit path (`lifecycle.go`) — since a dead token can surface either way. The backend fires `onAuthFailure(detail)`, wired via `Backend.SetOnAuthFailure` in `agents_delegated.go` to the agent's `triggerRelogin(reason, sessionKey)` closure.
- **Gate (`relogin/gate.go`).** `relogin.G` is a process-wide single-flight hold gate. The first 401 claims it (`G.Start()`); while active, `Agent.Enqueue` queues inbound messages and injections for delegated agents straight onto the session inbox channel, skipping mid-turn routing (the `DelegatedManager != nil` check is the cheap "is delegated" test) — *except* a one-shot capture window (`ShouldCapture(agentID)`) where the triggering agent's next message is treated as the pasted-back login code (`SubmitCode`). The session worker holds dispatch (`waitReloginGate`, at dequeue and before each post-turn held inject) until `G.Released()` closes, then runs the backlog batched — the same shape as the #767 and compaction holds. Before #1932 this was a drop gate: the front end had already rendered the message as sent. Every driver exit path releases the gate (`defer G.Release()`), so a failed or timed-out login can never wedge message processing — the backstop, not the happy path.
- **Driver (`relogin/driver.go`).** `relogin.Run` drives an interactive `claude /login` in a dedicated tmux pane (regular TUI, not stream-json; standalone tmux helper, no cctmux dep), extracts the sign-in URL (`extract.go`), relays it to the user, awaits the code through the capture window, feeds it back, and confirms success. Aborts log at ERROR (surface in `/errors`); the just-submitted one-time code is redacted from any diagnostic screen dump.
- **URL routing (`ec3ce87d`).** The triggering session key threads through `ReloginTrigger(reason, sessionKey)` into the relogin `Config`; the URL is delivered via `conn.SendToSession(sessionKey, ...)` (reads the chat ID straight from the key). Manual `/login` passes `req.SessionKey` so the URL returns to whoever ran it; the auto-401 path passes `""` → the agent's primary/default chat. A hardening fix makes `BotForSession("")` return nil in both telegram and discord so an empty key never matches an idle facet bot and correctly falls through to the agent's primary.
- **Manual trigger (`8dcd45d0`).** `/login` (a `RequiresBackend` command, `command/login.go`) invokes the same flow on demand for testing without waiting for a real 401. The trigger is built once in `configureDelegated` and shared between the 401 callback and the command via `Agent.ReloginTrigger` — wired only for ccstream (nil for cctmux; the command reports unavailability otherwise).
- **Startup readiness probe (#853).** A dead OAuth credential at boot would otherwise stay invisible until the first user turn 401'd — and that first turn carries the first-run onboarding, so the onboarding would be lost to the auth failure. `checkDelegatedReadiness` (`cmd/foci-gw/notifications.go`, called from `main.go` after `StartAll`, before `handleRestartAndFirstRun`) probes each delegated backend via the `Delegator.CheckReady(ctx)` interface method (`delegator/backend.go`). ccstream's impl (`ccstream/readiness.go`) shells `claude auth status` — parsing stdout before honoring the exit code, since the binary prints the same JSON whether logged in or not — and fires the existing `onAuthFailure → triggerRelogin` path when not authenticated; cctmux reports ready unconditionally (`cctmux/turn.go`); API agents (`DelegatedManager == nil`) are skipped. Probes run concurrently but the pass waits for all to settle, so a not-ready agent's relogin gate is reliably active before any startup turn is injected.

**Shared rate-limit policy + periodic suppression (#1211).** API, Claude Code, and OpenCode emit `ratelimit.Signal` values containing a limit kind plus any trustworthy absolute reset or `Retry-After` hint. The stdlib-only `internal/ratelimit` leaf package is the single interpretation policy: valid hints win; missing usage-window hints use one hour; missing request-limit hints back off 1m, 2m, 4m, … capped at 1h. The agent is the sole owner of closing endpoint gates and firing `RateLimitFunc`. Claude Code detects its synthetic no-API-call session-limit result (`model=<synthetic>`) and extracts the named-zone clock; OpenCode detects `session.status: retry` usage-limit messages and extracts only explicitly offset timestamps; direct API calls extract `Retry-After`. Periodic/system work consults the gate and queues when closed. User delegated turns bypass it by design, while user API turns are explicitly allowed through as recovery probes; any successful API response releases that endpoint gate immediately and the normal drain tick replays queued system work.

The **intelligence/turn path** above owns the endpoint gate. **Auxiliary direct-HTTP clients** apply the same `internal/ratelimit` policy locally via `ratelimit.Transport` (an `http.RoundTripper` wrapping the client's transport) — no gate involvement. It resolves a 429/503's `Retry-After` header or `try again in …` body hint through `Resolve`, then per `Mode`: `ModeDegrade` returns a typed `*ratelimit.Error` (clean, body-free — no leaked URL) so the caller degrades — used by **voice TTS/STT** (`voice.HTTPOpts.client`), where the app sink logs it at info and delivers text-only with a `voiceUnavailable` reason on the bubble (#1809) after the TTS fallback chain (#1808) has exhausted every provider; `ModePassthrough` retries within a bounded inline budget and otherwise returns the real 429 response unchanged — used by the agent's **`newSafeClient`** (`http_request`/`web_fetch`), so the model still sees the status/body. openai/Gemini are NOT wrapped: their turn calls already run through `provider/retry.go` (and gemini-cache shares that SDK client).

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

#### Subprocess environment (`ccstream/env.go`)

`buildEnv(extra)` assembles the `claude` subprocess's environment in three layers, later-wins (execve semantics): the gateway's own `os.Environ()`, then foci's **CC-only** defaults, then `StartOptions.Env` — the per-session extras `DelegatedManager` builds (`FOCI_SESSION_KEY`, and `BASH_ENV`/`FOCI_SOCK` from the exec bridge) merged with the agent's `[agents.backend_config] env`. Because the CC-only defaults sit in the middle, a per-agent `backend_config.env` can always override one without new config surface; and because they live in `ccstream` rather than in `DelegatedManager`'s shared per-session env, they reach **only** the Claude Code subprocess — codex (`codex/lifecycle.go buildEnv`) and opencode (`opencode/lifecycle.go buildCmdEnv`) build their own environments and never see them, and an API agent has no subprocess at all. Negative tests in each of those packages fail if a CC-only var migrates up to the shared layer.

The CC-only defaults are:

| Var | Value | Why |
|-----|-------|-----|
| `CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS` | `1` | Opts into the `session_state_changed` running/idle stream that foci's turn boundary is keyed to — see [Idle-keyed turn completion](#ccstream-backend-internaldelegatorccstream) below. |
| `BASH_MAX_TIMEOUT_MS` | `1200000` (20 min) | Raises the **ceiling** a CC model may request for one foreground Bash call, from CC's built-in 600000 ms. `BASH_DEFAULT_TIMEOUT_MS` is deliberately *not* set, so the 120000 ms default (and therefore every call that doesn't ask for longer) is unchanged. Needed because a CC **subagent** is never woken by a background-task completion notification (open upstream bug, anthropics/claude-code #78782 / #77578 / #76594) — it must finish long work inside a single foreground Bash call, and the 10-minute ceiling made that impossible for slow builds/tests, forcing a parent agent to poll and poke it. Kept a constant rather than a `[cc_backend]` field: raising a ceiling has no cost when unused (nothing to tune), it is a workaround pinned to a specific upstream bug (when that is fixed the move is to delete it, not retune it), and the per-agent `backend_config.env` override above already provides the escape hatch. |

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
| `system` | Lifecycle events — subtypes: `init`, `status`, `compact_boundary`, `session_state_changed`, `task_*`, `api_retry`, `hook_started` / `hook_progress` / `hook_response` (from `--include-hook-events`), `elicitation_complete` (URL-mode MCP elicitation finished externally), `model_refusal_fallback` (a safeguard refused the primary model's turn and CC silently retried on a fallback model — logged at WARN, see below) |
| `control_request` | CC requesting user interaction — subtypes: `can_use_tool` (tool permission), `elicitation` (MCP structured-input request). Any other subtype is logged at WARN and refused with an error `control_response` (`OnUnknownControlRequest`, #2168), so CC's pending request fails fast instead of blocking the turn |
| `control_cancel_request` | CC cancelling a pending permission request |
| `tool_progress` | Heartbeat during long-running tool execution |
| `stream_event` | Token-level streaming (with `--include-partial-messages`) — `text_delta` and `thinking_delta` subtypes are extracted |

**Mid-turn injection:** The unified entry point is `Backend.ImmediateInject(ctx, Inject{Source: ..., Text: ...})`. A mid-turn **steer** (`SourceSteer`) is written to CC's stdin at explicit priority `"next"` (`sendUserMessagePriority` → `writer.SendUserPriority`); an in-flight `SourceUser` follow-up goes in at the same priority implicitly (CC's default). `SourceSystem` never goes in mid-turn at all — it begins a turn atomically at idle (`tryBeginTurn`) or returns `ErrTurnInFlight` for the caller to wait and retry (see "System Injections Never Steer"). All `"next"` items fold into the current ask at CC's next mid-turn drain (tool boundary) and stay inside the current run, so the reply belongs to the current foci turn and no per-inject bookkeeping is needed — see **Idle-keyed turn completion** below. CC's other priority classes are deliberately unused: `"now"` (aborts the in-flight ask via `print.ts`/`REPL.tsx` `abort('interrupt')` and answers immediately) is reserved for NYI per-message steer tagging or an NYI aggressive-steer config mode; `"later"` (excluded from the mid-turn drain; CC's own background task notifications) has no foci use — background content stays in foci's inbox queue, which provides turn bookkeeping CC's queue cannot. The `interrupt` control request (`Backend.Interrupt`, mirrors the Agent SDK's `client.interrupt()`) is wired to `/stop` **only** — it is *not* used by steer.

**Lifecycle:**
1. `Start` spawns `claude` with stream-json flags, creates stdin/stdout/stderr pipes.
2. Sends an `initialize` control request with the system prompt. If that write fails because the transport is already gone (`transportGone`: `errors.Is` on `os.ErrClosed`/`EPIPE`/`io.ErrClosedPipe`/`EBADF`), the subprocess died before the handshake and `Start` does **not** report a distinct error — it logs and falls through so the death surfaces via the single dead-backend path (`WaitReady` → `subprocess exited before init`, `IsRunning()==false`, manager respawns next turn). Otherwise a pre-handshake death's observable outcome would depend on whether the write or the waiter goroutine's `cmd.Wait` (which closes the parent's pipe end) won the race. For the same reason `running` is only claimed when `finalized` is unset, so a finalize that already declared the process dead is never overwritten.
3. Reader goroutine dispatches stdout lines to typed handler methods.
4. `OnSystem("init", ...)` fires `readyOnce` (unblocks `WaitReady`) and persists session ID.
5. Keep-alive goroutine sends heartbeats every 30s.
6. `Close` sends interrupt + EOF, waits up to 5s, escalates SIGTERM → SIGKILL.
7. There is no `Restart`: a `Backend` is single-use. `DelegatedManager.Get` sees `IsRunning()==false` and constructs a fresh Backend, resuming via `--resume <saved session id>`. (Stale `Reset in Restart()` comments on `finalizeOnce`/`closeOnce` refer to a method that does not exist.)

**`QueryUsage` (`usage_oneshot.go`, behind `/mana`; registered for both CC backends via `delegator.RegisterUsage`, result types in `delegator/usage.go`)** spawns its own throwaway `claude` and runs the same handshake outside any `Backend`, so it inherits the same pre-handshake race and the same `transportGone` treatment — every control request goes through `oneshotRoundTrip`, which swallows a transport-gone write and lets the death be reported once, by the reader hitting stdout EOF (`claude exited before responding`). The reachable *sentinel* differs: `Start` sees `os.ErrClosed` because a concurrent waiter goroutine's `cmd.Wait` closes the parent's pipe end, while the one-shot defers `cmd.Wait` to teardown so nothing closes our end and only the kernel's `EPIPE` can fire (measured: same-instant write succeeds, +5ms write gets EPIPE). `transportGone` matches both. There is no manager and no `WaitReady` here — the canonical outcome is simply the one error string instead of a coin flip between two.

**Two-lifetime callback split (TODO #747):** ccstream divides backend callbacks across two distinct lifetimes that match the actual semantics — delivery is session-scoped, bookkeeping is per-turn:

- **`SessionEvents`** (delivery: `OnText`, `OnTextDelta`, `OnThinkingDelta`, `OnToolStart`, `OnToolEnd`, and the subagent lifecycle `OnSubagentStart`/`OnSubagentText`/`OnSubagentPrompt`/`OnSubagentEnd`) — installed once per session via `Backend.AttachSessionEvents`, stored on the backend in an `atomic.Pointer[SessionEvents]` that's never nil after first attach. Text/tool emission paths (`OnAssistant`, `OnStreamEvent`, hook dispatch) read through this pointer without taking `turnMu` and never drop on a nil handler. The agent layer's `RunInference` re-attaches per turn (idempotent — replaces); closures capture the session router (lazy-built once per session in `inbox.sessionRouterFor`), so they remain safe to call any time the backend is alive.
- **`TurnEvents`** (bookkeeping: `OnTurnComplete`, `PostToolNudgeFunc`, `PreAnswerNudgeFunc`) — installed via `Inject.Turn` for begin-turn paths, captured-then-nilled under `turnMu` in `OnResult` for the fire-once invariant. May legitimately be nil between turns; backend tolerates that. The pre-answer second round explicitly preserves `turnEvents` across rounds — only the round-2 `OnResult` clears it.

This divorces "where does this text go?" (session lifetime — always somewhere) from "did this turn finish yet?" (turn lifetime — might be nil). The pre-TODO #747 design bundled both into one per-turn handler pointer that nilled on `OnResult`, which made post-OnResult text drops a structural inevitability rather than a bug. Both backends now implement the split: ccstream routes delivery through the `atomic.Pointer`-stored `SessionEvents` and bookkeeping through `Inject.Turn`; cctmux's JSONL watcher dispatches into the same `SessionEvents` (delivery) and `TurnEvents` (completion) on its `Backend`. The legacy combined `EventHandler` has been removed.

**Turn flow:**
1. `ImmediateInject(SourceUser)` at idle calls `sendToPane`, which calls `beginTurn(turnEvents)` (sets `b.turnEvents`, resets text/tools counters, creates result channel). Delivery is unaffected — `b.sessionEvents` was already attached.
2. `Writer.SendUser(prompt)` writes a user message to CC's stdin.
3. CC processes the turn, emitting `assistant`, `tool_progress`, and `stream_event` messages.
4. `OnAssistant` accumulates text, counts tool_use blocks, and fires `SessionEvents.OnText` / `SessionEvents.OnToolStart`. Mid-turn steer dispatch is handled at the agent's per-session inbox (see `agent.Inbox.Enqueue` routing), not at tool boundaries — this lets text-only turns be steered too.
5. `OnResult` captures this ask cycle's text/usage/model and **stashes** it (`stashedResult`; output tokens sum across cycles). It does NOT complete the turn.
6. `OnSystem("session_state_changed", state=idle)` → `onSessionIdle` → `completeTurn`: fires `TurnEvents.OnTurnComplete` with the stash, clears `b.turnEvents`, stops typing, signals `WaitForTurn`. `b.sessionEvents` is untouched.

**Idle-keyed turn completion (#813 successor, `complete.go`):** The turn boundary is CC's own `session_state_changed` running/idle SDK stream, which `env.go`'s `buildEnv` enables by setting `CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS=1` in the subprocess env (opt-in in CC; a per-agent `backend_config.env` can override it for debugging — see [Subprocess environment](#subprocess-environment-ccstreamenvgo)). `running`/`idle` bracket CC's entire internal run loop — every ask cycle, every drained steer/follow-up/nudge, the background-agent wait, and the held-back-result flush — so **one foci turn == one CC run** and `idle` is the authoritative "no more results are coming" signal (probe-verified on the deployed CC: exactly one `idle` per run, after the last `result`, across steer-abort, mid-tool fold, background-task and `--resume`+hooks scenarios — `clutch/docs/steer-shadow-turn-design-option3.md` §Phase 3).

`result` events are per-internal-ask-cycle accounting, NOT turn boundaries: a `"now"`-priority message arriving mid-stream aborts the ask and mints an extra result (foci no longer sends `"now"`, but the machinery must tolerate it — TUI-side interrupts and future aggressive-steer would reintroduce it); a steer arriving mid-tool folds and mints none; results are withheld — and silently overwritten — while background `local_agent`/`local_workflow` tasks run (claude-code `print.ts` `heldBackResult`). The predecessor designs (per-steer counter, then init-herald + 45s activity watchdog) both tried to reconstruct the boundary from result counting and both failed in production: the watchdog was blind to long silent tool executions (79–122s builds) and force-completed live turns, re-opening the original #813 collision window. Idle-keying deletes that machinery outright (`foldPending`, `continuationExpected`, `sawFirstResult`, `heldResult`, the watchdog, `reArmForContinuation`).

- **Multi-cycle accounting.** `turnText`/`turnTools` span the whole run (reset only in `beginTurn`); output tokens accumulate across cycles (`turnOutputTokens`); input/cache/model are latest-wins (the final cycle's context fill — what compaction needs).
- **Pre-answer verification (`tryPreAnswerRedispatch`)** runs at idle on the final stashed result: a returned follow-up is sent as a fresh user message, `turnText` resets (the revision supersedes round 1) while the token/cost accumulators do NOT — the re-dispatch is more ask cycles inside the same turn, exactly like a steer, so the turn writes ONE api.db row (output/`turn_*` summed across both rounds, input/cache = final fill; #1856) — and `redispatchInFlight` holds the turn open — including across a stray idle before CC drains the follow-up — until the follow-up's own result + idle complete the turn.
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

**VoiceModer pattern (`delegator.VoiceModer`, #1445):** a turn tagged `trigger == "voice"` (any envelope in the batch was produced by transcribing spoken audio, #1436) runs at low effort on backends that opt in via `EnterVoiceMode(ctx)`/`ExitVoiceMode(ctx)` — an optional interface, same pattern as `ControlSender`/`CompactionWaiter`. Effort is a request-side parameter CC/Codex never echo back on the wire, so unlike model/permission mode there's nothing for the Agent layer to read generically — each implementing backend tracks its own "current effort" and does the save/restore itself:
- **ccstream** (`ccstream/voicemode.go`): tracks `effortLevel` (set at cold launch from `opts.Effort`, updated on every live `apply_flag_settings effortLevel` push — including voice mode's own). `EnterVoiceMode` saves it and pushes `"low"`; `ExitVoiceMode` pushes the saved value back (skipped if it was empty/`"off"` — mirrors `Agent.SetSessionEffort`'s own "clear takes effect on next launch, not live" limitation).
- **codex** (`codex/voicemode.go`): reuses `pendingEffort` (the queued override `applyPendingControls` reads at the next `beginTurn` — Codex has no mid-session control channel) the same way.
- **opencode**: no effort knob at all, so it doesn't implement the interface.

Wired at `agent/turn_delegated.go`'s `RunInference`: `EnterVoiceMode` runs right before the begin-turn `ImmediateInject` (scoped to that dispatch only — the fold/answer-capture paths above it return early, so a voice follow-up folding into an already in-flight turn never re-enters/exits). `ExitVoiceMode` is wired into the same `TurnEvents.OnTurnComplete` callback `buildTurnEvents` installs (the delegated path is async — that callback, not `RunInference` returning, is the turn's true end), with a fallback call if the begin-turn dispatch itself errors (so a failed dispatch — CC never firing `OnTurnComplete` — doesn't strand the session at low effort).

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
4. Build a JSON settings object: `{"hooks": {"PostToolUse": [{"matcher":"*", "hooks":[{"type":"command","command":<cmd>,"timeout":10}]}], "PostToolUseFailure": [...], "PreToolUse": [{"matcher":"Agent|<ruled tools>", "hooks":[{..."command":<cmd> --rules <b64>}]}]}}` (`buildHookSettingsJSON`). The PreToolUse entry carries the pretool rules — see "PreToolUse rules" below. When the agent has stop rules a fourth entry, `"Stop": [{"hooks":[{..."command":<cmd> --stop-rules <b64>}]}]` (no matcher), is added — see "Stop rules" below; with none there is no Stop hook.
5. Append `--settings <json>` to the claude argv before spawning.
6. Record `hookCmd` / `hookInstallID` on the Backend struct so `handleHookResponse` can filter events by matching install ID.

**Why `--settings` over file mutation:** CC loads the JSON as an additional settings source called `flagSettings` (`constants.ts:159`). `flagSettings` is always enabled regardless of `--setting-sources` filters, and hooks from multiple sources merge rather than replace, so foci's hook coexists automatically with any user hooks in `settings.json` / `settings.local.json`. The JSON lives in a content-hashed temp file CC creates internally (`loadSettingsFromFlag` at `main.tsx:454`) — identical settings produce the same path across process boundaries, so prompt-cache stability is preserved. Foci has **no filesystem footprint** for hook installation.

**No uninstall step:** `Backend.Close` has nothing to clean up. The CC subprocess exits, its temp settings file is CC's concern, and foci's own state (`hookCmd`, `hookInstallID`) disappears with the Backend struct. There's no shared settings.local.json file to unwind, no mutex, no crash-orphan accumulation, no multi-backend race — each Backend passes its own `--settings` argv and each CC subprocess has independent hook state.

**Multi-backend safety:** two foci backends running CC in the same workdir each generate a unique install ID and each passes its own `--settings <json>` argv. CC's subprocesses have no shared state — each reads its own flagSettings from its own temp file. The install ID is still bound into the hook command and echoed back by `foci-cc-hook` so `handleHookResponse` can filter events by origin — not for race protection (there's no race) but to distinguish foci's hook_response events from any user-installed PostToolUse hooks that fire alongside.

**Hook output path:** when CC fires the hook, it pipes an input JSON envelope (`tool_name`, `tool_use_id`, `tool_input`, `tool_response` / `error`, `agent_id`, ...) into `foci-cc-hook`'s stdin. The helper parses its own argv for `--install <id>`, reads the stdin envelope, truncates `tool_response` / `tool_input` / `error` to **`maxFieldBytes` = 4 KB** (`cmd/foci-cc-hook/main.go`) and writes a compact JSON object to stdout. Two independent constraints set that cap, the tighter winning: (1) each emitted stream line must stay under ccstream's 1 MB scanner limit — without a cap a multi-MB file read would blow the scanner and tear down the backend via `OnReaderStopped`; (2) foci's only consumer of `tool_response` is the tool-call display (a one-line result hint plus the "Show full" expansion, itself hard-capped at 4096 bytes/message in `formatToolCallWithResult`), and `tool_input` only feeds nudge matching / Agent-description extraction — so nothing foci renders or matches on needs more than ~4 KB. The cap was lowered from 64 KB (a scanner-safety margin) to 4 KB because the hook's stdout, captured verbatim by CC into `hook_success` attachments, was the single largest contributor to the on-disk CC session JSONL (~40% of the file); 4 KB satisfies both constraints and cuts that category by ~70%. CC captures that stdout and emits it as a `system/hook_response` message on its own stdout, where foci's reader picks it up. **Slow-tool runtime note (#2125):** on `PostToolUse`/`PostToolUseFailure` the helper also reads CC's `duration_ms`; a call of `slowToolThreshold` (10 s, Dick's ruling 2026-10-02) or more gets `hookSpecificOutput{hookEventName, additionalContext:"Tool runtime: this <Tool> call took <d>. ..."}`, which CC attaches to the model's context right after that tool's result (verified live on CC 2.1.286, extra foci fields tolerated) — no foci-side injection, so nothing races the next API call, and subagents' calls are covered too. The helper echoes `duration_ms` and `runtime_shown`, and `handleHookResponse` logs each shown note as INFO `tool_runtime_shown` (before the sidechain filter) so the effect can be measured.

**Dispatch path:** `OnSystem("hook_response", ...)` calls `handleHookResponse`, which applies three filters before firing `handler.OnToolEnd`:

1. **Hook event type:** only `PreToolUse` (foci's own: Agent spawns and pretool-rule denies), `PostToolUse` and `PostToolUseFailure` are processed. (`Stop` is diverted before these filters to `logStopVerdict`, which applies its own install-ID check and only logs — see "Stop rules".) Other hook events (lifecycle events) are silently ignored; a user's own PreToolUse hook is dropped by the install-ID filter.
2. **Install ID match:** parses `install_id` from the helper's stdout JSON; events whose ID doesn't match the current backend's `hookInstallID` are dropped. This is what keeps user-authored hook responses out of foci's tracker.
3. **Sidechain filter:** events with non-empty `agent_id` are dropped — sub-agent tool calls belong to the sub-agent's own transcript rather than the parent turn, consistent with the `isSidechain` filter in the cctmux backend. One is read first: an `Agent` `PreToolUse` with a non-empty `agent_id` is a NESTED spawn, whose `agent_id` is the spawner's task_id, so it is recorded via `registerNestedAgent` before being dropped (#1554, see "Nested subagents").

**PreToolUse rules (#2028, `internal/delegator/pretool`).** The same helper enforces configurable deny rules. Layers, merged by rule `name` with field-level override (`pretool.Resolve`): the preinstalled `pretool.Defaults` (`ask_user_question` → redirect to `foci_ask`; `cron_create` → crontab for repeating events, `foci_remind` for one-shots) ← `[[cc_backend.pretool_rules]]` ← `[[agents.backend_config.pretool_rules]]`; `enabled = false` switches a rule off. Each rule is `{name, tool (exact name), input (field → regex(es)), command (Bash only: regex(es) matched per simple command, anchored at its first word — `pretool.Parse` splits the script with the mvdan.cc/sh bash parser, drops quotes, and shows in-word whitespace as `␣`), background/subshell/output (#2040: Bash-only per-command fact filters from `pretool.Parse`, which walks the syntax tree carrying a frame of {shell dir, stdout sink, subshell, background} so heredoc bodies and quoted text never become commands or operators; `shell.go`), cwd (regex(es) on the hook payload's top-level `cwd`), when (#2034: a bash script `foci-cc-hook` runs only after the patterns hold, with the matched command's words as `$@`, `TOOL_INPUT[_<FIELD>]`/`TOOL_NAME`/`TOOL_CWD` env, the parsed script's `TOOL_COMMANDS`/`TOOL_END_DIR` and the matched command's `CMD_DIR`/`CMD_OP`/`CMD_PIPE` (#2040), the input JSON on stdin, cwd as its directory; deny iff exit 0; 2 s per run, 5 s per call, own process group killed on timeout; any other outcome fails open and rides back as `when_errors`, logged by `handleHookResponse` as WARN `pretool_when_error`; `pretool/when.go`), action ("deny" only), reason}`; constraints are ANDed and each takes a string or a list (any-of, `pretool.Patterns`, #2033); `config.validatePreToolRules` rejects any other action at load, so no rule can ever emit `allow` (a PreToolUse allow would skip foci's permission flow). `configureDelegated` hands each claude-code Backend a rule SOURCE via `SetPreToolRules` (`livePreToolRules`, #2033): every `prepareHooks` (each CC launch) calls it, and it re-reads the config FILE (`config.Load` + `Config.PreToolRules`), keeping the last good rules if the file fails to load. There is no live-apply registry row for the rules and hand edits never go through `/config set`, so reading the file at launch is what makes an edit reach the next session. `foci pretool list|test` (cmd/foci/cmd_pretool.go) resolves through the same `Config.PreToolRules` offline. `prepareHooks` bakes them into the PreToolUse command as base64url JSON (`--rules`) and widens its matcher to `Agent|<each ruled tool>` (CC treats a `[A-Za-z0-9_|]` matcher as an exact name list). On a match `foci-cc-hook` adds `denied_rule` and `hookSpecificOutput{permissionDecision:"deny", permissionDecisionReason}` to its usual stdout object. **Verified live on CC 2.1.280:** the deny preempts CC's permission check (no `can_use_tool` control_request reaches foci), the tool does not run, the reason reaches the model as the tool_result error (`PreToolUse:<Tool> hook error: <reason>`), the extra foci fields in the same object are tolerated, and CC fires **no** PostToolUse/PostToolUseFailure for the denied call. So `handleHookResponse` treats a `denied_rule` hook_response (checked before the sidechain filter, logged as `pretool_rule_deny`) as the call's end: for a main-thread call `endDeniedCall` fires `OnToolEnd(reason, is_error)`, drops any pending-work tracker entry and clears a pending foreground-tail expectation; no subagent start fires.

**Stop rules (#2089, `internal/delegator/stoprule`).** Per-agent only: `[[agents.backend_config.stop_rules]]`, each `{name, text (regex(es) on the turn's final assistant text), reason (optional; `{match}` is replaced by the matched text; empty → `stoprule.DefaultReason`), enabled}`. No preinstalled rules and no `[cc_backend]` layer — `config.validateStopRules` rejects a `cc_backend.stop_rules`, rejects unknown rule keys the same way as pretool_rules (via the shared `undefinedKeysUnder`), and runs `stoprule.ValidateLayer` (name required and unique, at least one non-empty compiling pattern). `configureDelegated` hands each claude-code Backend a source via `SetStopRules` (`liveStopRules`, which shares `liveRules` with `livePreToolRules`: re-read the config file at each launch, keep the last good set). `prepareHooks` installs the Stop entry only when that source yields rules. On `Stop`, `foci-cc-hook` (`stopFor`) runs `stoprule.Evaluate` on the payload: no rule matches `last_assistant_message` → `pass`; matched and `stop_hook_active` → `pass_active` (so a turn is blocked at most once); otherwise it reads `transcript_path` and `stoprule.TurnLaunches` collects the background launches from the turn's first user entry whose `promptId` equals the payload's `prompt_id` to the end (a tool_use with `run_in_background`, or a tool_result whose `toolUseResult` has `backgroundTaskId` or `status:"async_launched"` — the Agent tool backgrounds by default with no `run_in_background` in its input; sidechain entries ignored). Any launch → `pass_launched`; none → `fire`, and the helper's stdout carries `{"decision":"block","reason":…}`; a transcript it can't read or scope → `pass_error` (fail open). Every verdict also rides in the same stdout object (`stop_result`, `stop_rule`, `stop_excerpt`, `stop_launches`, `stop_error`), and `handleHookResponse` → `logStopVerdict` logs it as `stop_rule_eval` (INFO; WARN for `pass_error`). **Verified live on CC 2.1.280:** the Stop payload carries `transcript_path`, `prompt_id`, `stop_hook_active`, `last_assistant_message` and `background_tasks` (running tasks, not turn-scoped, so unused); a block continues the turn with the reason as a meta user message `Stop hook feedback:\n<reason>`; the next Stop has `stop_hook_active: true`; CC tolerates the extra foci fields in the block object; stream-json input mode carries the same `prompt_id` as the transcript's `promptId`.

For events that pass all three, `handler.OnToolEnd(tool_use_id, tool_name, tool_response_or_error, is_error)` fires. The id plumbs through `turn_delegated.go` → `turnevent.ToolResult{ID, Name, Output, IsError}` → `StreamingSink.Emit` → `tracker.ObserveToolResult(id, name, result, isError)` which looks up the entry by id (see Tool Call Visibility below) and updates the correct message.

**Required CC flags:** `--include-hook-events` + `--verbose` in `ccstream.go:Start` (both already set) enable the `hook_response` system message subtype on CC's stream-json output. Without them, hooks would run but their output would never reach foci.

**Silent model swap (`system/model_refusal_fallback`, #1968):** CC can fall back a session's model mid-process without asking — a safeguard flags the primary model's turn (`stop_reason: "refusal"`) and CC silently retries once on a fallback model. Before this, foci logged nothing about it at all: the only observable trace was the model name changing in the per-turn cost INFO line, indistinguishable from normal output. `OnSystem("model_refusal_fallback", ...)` (`handlers.go`) now unmarshals `ModelRefusalFallbackMessage` (`protocol.go`) and logs a WARN naming the session, the refusing (`original_model`) and fallback (`fallback_model`) models, and CC's own `content` prose VERBATIM — `content` is the only place that states in human language which model refused and what it switched to (Dick's ruling, 2026-09-23). **Field-name gotcha:** the wire record's fields are snake_case (`original_model`, `fallback_model`, `session_id`, ...) and are NOT the same shape as the camelCase record CC writes to its own on-disk transcript for the same event (`originalModel`, `fallbackModel`, plus transcript-only envelope fields like `level`/`isMeta`/`timestamp` that the wire record doesn't carry) — confirmed by decompiling the shipped CLI binary rather than assumed from the transcript. `scope` (`"session"` vs `"local"`) is absent on older CLIs; treat absence as `"session"`.

### Interactive Messages (`platform/interactive.go`)

Platform-agnostic interactive messages with button callbacks. `SendInteractiveMessage(conn, text, buttons, callback)` sends a message with inline buttons via `ButtonSender`. When a button is pressed, the callback fires and the message is edited with the return value. Falls back to numbered text choices when the connection doesn't support buttons.

Callback data format: `im:<promptID>:<buttonIndex>`. Prompt IDs are atomic uint64 counters. Callbacks are stored in a global `sync.Mutex`-protected map and auto-expire after the effective prompt TTL (`min(prompt_ttl, idle timeout)`, swept hourly by `setupInteractiveCleanup` → `CleanupExpiredInteractive`). The same tick also calls `app.ExpireBatchPrompts`, which expires the app hub's batched ask forms (`batchPrompts`, outside imStore) on the same TTL (#1895). **Expiry is not a cancel (#2091):** both sweeps feed an unanswered foci `ask` `question.ExpiredData` (`qa:expired`, server-internal, never a button) — `newAskPresentFn`/`newAskRestoreFn`'s `onExpire` and `expireBatchPrompts` — and `askState.expireLocked` tells the agent the ask EXPIRED (`expiredAskNotice`, under the same #1302 cold-cache gate as the cancel notice), not that the user cancelled it. Callbacks are one-shot — removed after handling. **Expiry removes, never marks (#2080):** on a connection implementing `platform.InteractiveRemover` (the app's `appConn.RemoveInteractive` → `interactive.remove`) the sweep deletes the expired prompt instead of editing it to "⌛ This request expired."; Telegram/Discord keep the edit.

Used by permission prompts (delegated backends), config selection menus, and other platform interactions that need structured user choices.

**Permission-prompt attachments (`PermissionPromptFunc` `attachmentPath`):** the `delegator.PermissionPromptFunc` carries an optional `attachmentPath` — a file the platform closure (`agents_delegated.go`) sends via `conn.SendDocument` *before* drawing the keyboard. Populated only by ccstream's `handleToolRequest` for **ExitPlanMode**: the generic formatter would truncate `input.plan` to a 200-char JSON blob, so instead foci attaches the full plan markdown that CC already wrote to `input.planFilePath` (under `~/.claude/plans/`) and replaces the prompt body with a short caption. Allow/Deny choices are unchanged — over the `--permission-prompt-tool stdio` protocol ExitPlanMode is a plain binary gate (the auto-accept/manual/keep-planning menu is CC-TUI-only and not exposed as `permission_suggestions`). Falls back to the generic rendering when the file is absent (`planAttachmentPath` returns `""`).

### Ask Tool (`ask` / `foci_ask`) and shared `internal/question`

`ask` is a foci-native, **backend-agnostic** equivalent of Claude Code's `AskUserQuestion`, with **no 4-item cap** and an **async** delivery model. It works for delegated (CC) and API agents alike.

**Shared core — `internal/question`:** the pure question machinery (types `Question`/`Option`, `Parse`, `FormatText`, `Choices`, `ResolveAnswer` — including the `qa:<index>`/`qa:cancel` button-data convention — `MergeAnswers`, and a sequential `Accumulator`). Zero backend deps. Consumed by **both** `internal/tools/ask.go` (async) and `internal/delegator/ccstream/userquestion.go` (CC's blocking `AskUserQuestion`, which keeps its control-response wiring but parses/formats/accumulates via the shared package). The 4-item cap only ever lived in CC's tool *schema*, never the parser.

**Async flow (`internal/tools/ask.go`):**
1. `Execute` reads the calling session key via `SessionKeyFromContext(ctx)` (set on the exec bridge ctx by `delegated_manager.go` for CC and `turn_api_tools.go` for API agents), parses questions, registers a `pendingAsk` in the in-memory `askState`, presents the first question, and **returns immediately** with `{"status":"asked",...}` — it does not block (sidesteps CC's 600s Bash ceiling).
2. Each question is one one-shot interactive message (via the presenter, wired in `cmd/foci-gw/ask_setup.go` → `platform.SendInteractiveMessageWithID`). A button click runs the generic `imStore` callback → `askState.handleResponse` → `question.ResolveAnswer` → `Accumulator.Record`; the next question is presented or, when done, the batch is delivered.
3. **Answer delivery** uses `newAskDeliverFn` — the same resolve-and-deliver core (`deliverToResolvedSession`) as `send_to_session`'s `newSessionNotifyFn`, plus the ask's requestID so the injection is tagged (see "Ask-deferral is keyed to the pending ask") — it calls `HandleMessage` on the asking session, waking the agent in a fresh turn with the `{questions, answers}` batch as a normal inbound user message. This single path works for both backends.

**App-only batched presentation (`AskPresentBatchFn`):** the native app can render a multi-question ask as ONE on-screen form and return all answers at once; chat transports stay one-question-at-a-time, unchanged. This is an **additive, gated** path: `start()` (and `reattach()` for an un-advanced ask) first tries `tryPresentBatch` → the optional `AskPresentBatchFn` (wired via `tools.WithBatchPresent(newAskPresentBatchFn(...))` in `tool_table.go`). `newAskPresentBatchFn` (`ask_setup.go`) resolves the session's connection and type-asserts `platform.BatchButtonSender`; only the app's `*appConn` implements it. `appConn.SendInteractiveBatch` (`internal/app/conn.go`) returns `batched=false` — falling back to the sequential `AskPresentFn` — unless the client advertised the `"interactiveBatch"` capability in its `ClientHello.Features` (stored per-socket on `wsClient.features`, checked via `convBinding.clientHasFeature`). When it batches, it sends one `fap.Interactive{Questions:[...]}` frame and registers a callback in the hub's separate `batchPrompts` registry (keyed by promptID, distinct from the single-prompt `imStore` path). The app replies with one `fap.InteractiveResponse{Answers:[...]}`; `handleInteractiveResponse` (`dispatch.go`) routes any reply carrying `Answers` to that callback → `askState.handleBatchResponse`, which `ResolveAnswer`+`Record`s every answer positionally then shares the sequential path's `deliverBatch`. A `qa:cancel` in any slot cancels the whole ask. Chat platforms and uncapable/legacy app clients never take this path, so their behaviour is byte-for-byte unchanged. An unanswered batched form is expired by the hourly `app.ExpireBatchPrompts` sweep on the sequential prompt TTL: `qa:expired` (`question.ExpiredData`) to the callback (#1895, #2091) and an `interactive.remove` to every client — the form disappears (#2080). The `batchPrompts` registry is **in-memory only**, so it is re-registered on restart and a batched reply that outlives its registration is still delivered — see **Batched-ask restart survival** below (#1473).

**Restart persistence (`askState` ↔ `agent_metadata`):** in-flight asks are persisted to the session index under key `ask_pending` (24h TTL) on every change and rehydrated on construction (`restorePending`). For each survivor `reattach` rebinds the callback to the buttons still on screen via `AskRestoreFn` → `platform.RestoreInteractiveCallback` (no new message sent). Two wiring details make a restored ask first-class: (a) the interactive store holds a **`platform.ConnResolver` (`func() Connection`)**, not a captured connection — built by the single `connResolver(connMgr, sessionKey, agentID)` helper in `agents_shared.go` and used by `newAskPresentFn`, `newAskRestoreFn`, **and** the permission prompt in `agents_delegated.go` — so it re-resolves at edit time and survives the startup race where the platform connection isn't up yet when restore runs (no more spurious `ambiguous routing` WARN — `ForSessionOrPrimary` now logs that claimed-but-not-live case at DEBUG); (b) the platform-side message id returned by `SendInteractiveMessageWithID` is captured by `AskPresentFn`, persisted (`persistedAsk.PlatformMsgID`), and handed back through `AskRestoreFn` so proactive cancel/expiry edits reach the restored message too. **Expired-at-restore notice (#1894):** an entry older than the TTL is dropped with an agent notice (`expiredAskNotice`, shared with the queue's presentation-time TTL), never silently — but restore runs inside `setupAgent`, before `main.go` has put the instance in `agents` (so `agentResolverFn` returns nil and `deliverToResolvedSession` drops the injection as `unknown agent`) and before inboxes/platforms start. So `askState.restoring` makes `deliverMsg` park every restore-time message in `askState.held`, and `main.go` releases them via `AskRouter.DeliverRestoreNotices` right after `plat.StartAll`. A dropped entry that was on screen (not queued) also carries `heldNotice.removeMsgID`; the release calls the optional **`AskRemoveFn`** (`tools.WithRemove(newAskRemoveFn(...))`, `ask_setup.go` → `platform.InteractiveRemover`) so the stale question disappears from the app at the same moment (#2080).

**Batched-ask restart survival (#1473):** the batched callback lives only in the hub's in-memory `batchPrompts` map, so a naive restart lost it while the sequential `AskRestoreFn` path restored only single-prompt callbacks — a batched ask opened before a restart and answered after fell through `handleInteractiveResponse`'s single-prompt branch (`HandleInteractiveCallback("")` → `ok=false` → `deletePrompt`), emitting no resolution frame and dropping the answer (sibling clients never synced; the agent re-asked). Two coupled fixes close it: **(C, root)** batched asks persist a `batched` flag; on restart `reattach` dispatches to `restoreBatch` (vs sequential `restore`), re-registering the hub's `batchPrompts` entry via the new `AskRestoreBatchFn` hook → `appConn.RegisterInteractiveBatch` (register-only, no frame — the batched analogue of `AskRestoreFn`, wired in `tool_table.go`); **(B, defense-in-depth)** a reply carrying `Answers` with no live `batchPrompt` routes to the ask layer via `appConn.routeBatchAnswer` → `AskRouter.HandleBatchByPrompt` — the same entry a registered answer reaches — instead of the single-prompt dead-end. The registered path resolves through `Hub.resolveBatchedAsk` (fan out the terminal `InteractiveProgressEdit{Done:true}` to every attached client — closing the form / clearing the banner / rendering the chit — then deliver the answers); the B fallback sends the same Done edit only once `HandleBatchByPrompt` returns true (a pending ask matched). The registered path claims its `batchPrompts` entry (lookup and delete under `h.mu`, `claimBatchPrompt`) before resolving, so a second device's submit or the expiry sweep racing it cannot resolve the ask again (#2070). The claim (and the sweep) also record how the entry ended in `Hub.closedBatch` (in-memory), so the losing submit of a two-device race reads as a duplicate rather than a late answer — it can arrive before the winner's Done frame exists. The record is bounded by an event, not a timer: the closing frame is sent with `convBinding.sendThen`, whose `onRecorded` (run by the frame store's writer after the insert — `frameStore.AppendThen`/`frameWrite.onStored` — or at once with no store) deletes it; from then on the frame history classifies (`lastResolution`/`promptHistory`).

**Late answers and dead prompts (#2080/#1868, Dick's 2026-09-28 ruling) — `internal/app/lateask.go`:** an answer for a prompt nobody holds any more is never dropped, and the prompt never lingers. Two entry points in `handleInteractiveResponse` reach `Hub.resolveDeadPrompt`: the B fallback when `HandleBatchByPrompt` reports no pending ask (the old `WARN … (dropped)` return), and the single-prompt branch when `platform.HandleInteractiveCallback` misses (the per-question twin of that return). `Hub.promptHistory` rebuilds what was asked from the frames the app transport already keeps — the durable frame store (`frameStore.PromptFrames`, `instr` scan of one conversation, 30-day retention) plus the binding's in-memory buffer (the store write is async) — reading the original `interactive` frame (question text, options, send time) and the last resolution frame. No new persistence: the frame is the record of what the user saw. If that resolution was a real answer (`✅` edit / non-cancel Done) the new submit is a duplicate and is left alone. Otherwise the prompt is removed from every client (`interactive.remove`, skipped if the last resolution already was one) and the answer goes to the ask layer via `appConn.routeLateAnswer` → `AskRouter.DeliverLateAnswer` → `askState.deliverLateAnswer`, which delivers `formatLateAnswer` into the session tagged with the dead ask's requestID (so ask-deferral never holds it behind a live ask): a `[SYSTEM: LATE ANSWER …]` line naming the req and when it was asked, one `• <question text> → <chosen label>` line per question, and a JSON payload with `"late":true`. It declines a prompt that is not one of this agent's asks (permission prompts: removed only) and a late cancel (nothing to answer). A question aged out of the store still delivers its raw answer, flagged as having no text on record.

**Concurrent asks in one session — primary + queue (#1711).** A session maps to an **ordered queue** of request ids (`askState.bySession map[string][]string`, oldest first), not to a single "latest wins" id. Its **PRIMARY** ask is the oldest one actually on screen, and it alone owns every session-keyed path: typed-answer routing, `/pause`, `/resume`, `/complete`, the `{ask}` statusline field, and the inbox's ask-deferral gate (all of which read the one hook `AskRouter.PendingForSession` → `primaryLocked`). A second `foci_ask` is allowed and never displaces the first. Whether it *also* goes live is **app-only**: `tryPresentBatch` is the coexistence test as well as the presenter — the native app renders each ask as its own self-contained form, which is what makes it unambiguous which ask a typed answer belongs to, and it presents nothing and returns false for any transport that cannot, so a chat client simply leaves the ask **queued**. `promoteQueued` presents the head of the queue when the primary resolves; every resolution path (answered / cancelled / batched / `/complete`) funnels through the single `resolveLocked` helper so promotion cannot be forgotten at one of them. `removeLocked` is symmetric — removing any member of the ordered list leaves the rest untouched and fires `onResolve(sessionKey, requestID)` **per ask**, which is the signature the keyed deferral above needs (it previously fired only when the session's set emptied, and not at all when the older of two asks resolved first). A queued ask deliberately reads as "nothing pending": it is on nobody's screen and can be answered by nothing, so it defers no injection. The queue is FIFO — a new ask never jumps one already waiting. **Persistence:** `persistedAsk.Queued` carries the flag (a queued ask is worse to lose than a live one — the user never saw it), and `restorePending` **sorts by created_at** before rebuilding the queue. Without that sort the primary was re-elected at random on every restart: `persistLocked` iterated a Go map, so the stored array order was arbitrary (a real artifact holds three asks for one session stored 19:58 / 08:00 / 02:14). `persistLocked` now writes in the same order for a stable artifact. The 24h TTL applies to a queued ask at **presentation** time, aged from `created_at` — a queue draining hours later drops an expired entry with a notice to the agent rather than surfacing a stale question; restore promotes the queue head for a session whose live ask did not survive, and never re-attaches a queued ask (it has no on-screen buttons to bind to).

**Typed ("Other") answers:** a pending ask is also keyed by session (`askState.bySession`, exposed via `AskRouter`, stored on `Agent.AskRouter`). `Agent.RunTurn` (`run_turn.go`, the platform-message path only) checks for a pending ask and routes a typed reply to `AskRouter.HandleResponse` instead of starting a turn. Gating on `RunTurn` (not the shared `HandleMessage`) ensures system injects (keepalive, reflection, `session_notify`) — whose Inject.Run closures call `HandleMessage` directly, bypassing `RunTurn` — are never mistaken for answers (they are additionally deferred behind a pending ask by the inbox worker, see "System Injections Never Steer"). A typed answer routes straight into `handleResponse`, which never touches the on-screen interactive message, so after recording each answer `handleResponse` calls the **`AskCloseFn`** hook (`tool_table.go` → `platform.CancelInteractiveMessage`) to edit that question's message shut (`✅ <answer>`) and drop its stale buttons. On the button path this is an idempotent no-op (the click already deleted the `imStore` entry and edited the message); on the typed path it is what makes the question visibly "close" — most noticeably for an **option-less** question, whose only button is Cancel and which can *only* be answered by typing. The per-question message id is `questionMsgID(requestID, idx)`, shared by present/reattach/close so all three address the same message.

**Pause / resume answer-capture (`/pause`, `/resume`):** typed-answer capture can be suspended per session. `askState` carries a `paused` flag on each `pendingAsk` (persisted via `persistedAsk.Paused`, so a pause survives a restart mid-ask); `AskRouter` exposes `PauseSession`/`ResumeSession`/`IsPaused` (all no-ops returning false when nothing is pending). All three act on the session's **primary** ask only (#1711) — the platforms carrying these commands only ever have one live ask, since coexistence is app-only. The `RunTurn` guard (above) additionally skips answer-capture when `IsPaused(sk)` is true, so the user's typed replies run as **normal turns** while the ask stays pending (buttons still resolve it). The commands live in `internal/command/ask_pause.go` (`PauseCommand`/`ResumeCommand`, registered in `commands.go` only when `Agent.AskRouter != nil`); `Visible` hides them unless an ask is pending, and the in-`Execute` "No active question." guard is the real enforcement (Visible is cosmetic — a hidden command can still be typed). The agent is reminded a session is paused by the **`{ask}` statusline field** (`internal/agent/statusline.go`): it renders an `[ask] ⏸ …` line only while paused (rule 3 drops it otherwise) and names the ask id. Only agents on the default statusline template get the line automatically; a custom template must add `{ask}` itself.

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
- **429 (rate limit):** Could be burst rate limit or daily quota exhaustion. `provider/retry.go` first retries the 429 **inline** while the limit is short-lived (`inlineRateLimitWait`): it honors `Retry-After` when present, capped at `maxInlineRateLimitWait` (30s) over the 3-attempt backoff loop, so a brief upstream throttle (e.g. an OpenRouter/Moonshot burst limit) is ridden out transparently instead of failing the turn. A `Retry-After` beyond the cap (a long quota window) skips inline retry and returns immediately. If inline retry is exhausted or skipped, `classifyAPIError` emits a neutral request-limit signal and returns `"rate limited"`: shared policy closes the endpoint gate using `Retry-After`, or exponential 1m→1h fallback when absent; system work queues behind the gate; user turns may probe it; a successful probe opens the gate immediately. (The `Retry-After` hint reaches both layers because the openai transport now surfaces it — from the HTTP header, else OpenRouter's `error.metadata.retry_after_seconds` body field — where it previously dropped it.)
- **529 (overloaded):** Anthropic servers are overloaded (their problem, not ours). Two-phase retry in `SendMessage`: phase 1 retries 3× with exponential backoff (2s→4s→8s, same as other retryable errors); phase 2 (529 only) enters an extended duration-based loop retrying up to ~2 hours with 5s base backoff doubling without cap. A cross-goroutine recovery signal on the `Client` wakes all sleeping retry loops when any `SendMessage` succeeds (proving the server has recovered). If still failing after phase 2, `classifyAPIError` returns `"API is overloaded (HTTP 529) — try again shortly"`.
- **500/502/503 (server error):** `SendMessage` retries 3× with backoff. If still failing, `classifyAPIError` fires `RateLimitFunc(0)` and returns a temporary unavailability message.

**Model fallback** (`[groups.fallbacks]`): `provider.Send` handles the full error recovery pipeline: (1) retry with backoff, (2) strip unsupported params (thinking/effort/speed) on 400 and retry, (3) walk the fallback chain on transient errors (529, 5xx, `context.DeadlineExceeded`). Each fallback hop resolves the model's endpoint/format via `ClientProvider.GetClient` and retries. On success, the response is used; subsequent tool-loop iterations rebuild with the primary model (fallback is per-request, not sticky). All API call sites use `provider.Send` — main agent loop, compaction, spawn one-shot, summary tool, auto-summary, and prompt-diff all have fallback support. Not triggered by 401 or 429. Configured via `[groups.fallbacks]` (global) and per-agent `[groups.fallbacks]` override. Max chain depth: 3.

### Cache Stability Invariant

Conversation history sent to the API must be a strict append-only extension of the previous request — inserting a message in the middle invalidates all cached tokens after that point. `HandleMessage` enforces this via a per-session turn lock that serializes all callers (Telegram, `AsyncNotifier`, scheduled wakes, HTTP `/send`). Different sessions run concurrently. See [CACHING.md](CACHING.md) for the full cache stability contract.

### OpenCode Backend (`internal/delegator/opencode/`)

The opencode backend drives OpenCode as a coding agent via its HTTP server API. Unlike ccstream (one subprocess per session with NDJSON over stdin/stdout), opencode runs **one `opencode serve` subprocess per foci agent**, shared across all of that agent's sessions. The Backend is its HTTP/SSE client. Registered as `"opencode"` via `delegator.Register` in `init()`.

**Architecture — Server vs Backend split:**

- **`Server`** (one per agent, package-level pool keyed by agentID): owns the `opencode serve` subprocess, the `*http.Client`, and a single SSE subscriber goroutine reading `GET /event`. Refcounted — spawned lazily on first `acquireServer`, killed when the last `releaseServer` hits zero. A batch run is an ordinary session (`DelegatedManager.RunBatch`), so it acquires and releases through `Start`/`Close` like any holder — the server survives only while something still needs it. Bounded-shutdown Close mirrors ccstream's kill-ladder (POST /instance/dispose → SIGTERM → SIGKILL → abandon).
- **`Backend`** (one per foci session): holds the opencode sessionID (created via `POST /session`), a buffered events channel (256), and the per-session/per-turn state (SessionEvents, TurnEvents, turnText, steerBuf). The Server's SSE goroutine routes events to the right Backend by sessionID; the Backend's own dispatcher goroutine drains its channel and calls handlers serially.

- **Two readiness signals, not one (#1722).** `GET /global/health` and `GET /event` become ready at *different* times — health answered 200 a full **8 seconds** before the SSE stream would establish on 2026-08-16, with the connect loop retrying every 100ms throughout. `Start` therefore waits for BOTH: the health probe, then `waitForSubscriber` (bounded by `subscriberAttachWait`, 30s) on a `subscribed` channel the subscriber closes when its `GET /event` returns 200. Waiting in `Start` is what makes it total — no prompt of any kind is sent before `Start` returns. Without it a prompt POSTed into that window runs to completion with nobody listening, foci never sees the turn end, and the session worker wedges **permanently**: every later message to that agent queues behind a turn that already finished, with no error anywhere. The timeout is deliberately non-fatal (WARN, then proceed) — a subscriber that never attaches already breaks turn completion, and failing `Start` would escalate that to "agent unusable" for a window opencode may simply be slow through.

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
| `message.updated` (assistant) | `onMessageUpdated` | Store `lastModel`/`lastUsage` (context fill); book the message in the cost ledger once completed (`bookMessage`, see "Cost ledger") |
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
- **Internal compaction with foci's prompt:** opencode's own `/summarize` compaction follows foci's `compaction-summary.md` format via a second hook in the same plugin (`experimental.session.compacting`). `Backend.Start` writes the resolved compaction prompt (same file-only `prompts.ResolvePrompt("", "compaction-summary.md", …, PromptSearchDirs)` resolution as CC — no config path field) to `{tempdir}/session-compact/{sessionID}`; `Close` removes it. Unlike the system prompt, no fallback — a missing file leaves opencode's default compaction template.
- **Codex internal compaction with foci's prompt (#1327):** codex's own compaction uses the `compact_prompt` config value. `Backend.Start` resolves foci's `compaction-summary.md` (via `CompactionPromptFunc`, same file-only resolution as CC/opencode) ONCE at launch and passes it as a per-process `-c compact_prompt=<TOML>` app-server override (`appServerArgs`); `triggerCompaction` then just sends `thread/compact/start`, which consumes the process-local value. This deliberately does NOT write `compact_prompt` to the shared `~/.codex/config.toml` (the pre-#1327 mechanism, via `config/value/write`) — that leaked foci's prompt into the user's own codex CLI and every other codex agent, and raced concurrent compactions last-writer-wins. `ThreadCompactStartParams` carries no per-request prompt at codex 0.144.5, so the launch override is the isolation boundary.
- **Codex per-thread exec-bridge env via a PreToolUse hook (`codex/hooks.go`, `cmd/foci-codex-hook`):** codex offers no per-thread environment lever — an app-server process has ONE env — so where one app-server is shared across an agent's threads every thread but the first would run its bash tool calls on the first thread's exec bridge (wrong chat, silently). foci installs a `PreToolUse` hook matched to `^Bash$` that rewrites the command. The hook payload's `session_id` IS the codex thread id and varies per thread on a shared process, so it is the lookup key: `bindThreadEnv` writes `{tempdir}/session-env/{threadID}.json` (shared `sessionenv` format) and is called from **`registerThread` only** — the one point at which a thread becomes bound to a foci session — with `unbindThreadEnv` in `unregisterThread` as its exact inverse. They are deliberately not hand-placed at call sites: while they sat next to `startThread`/`resumeThread`, the other `registerThread` callers (batch threads in the since-deleted `batch.go`, #1962; threads first seen via a `thread/started` notification in `handlers.go`) registered with no env of their own and ran their bash calls on the app-server owner's bridge, and facade/batch teardown left the file behind. The sole remaining standalone `unbindThreadEnv` is on the owner's own close path, where the process and its maps are torn down without an `unregisterThread`. Both are no-ops when the session has no exec-bridge env, so a `BatchOnly` or test backend never touches the temp root. `foci-codex-hook` reads that file and returns `updatedInput.command` = `env FOCI_SESSION_KEY=… FOCI_SOCK=… BASH_ENV=… bash -c <original>`; the **nested** shell is required because codex runs `/bin/bash -lc '<command>'` and that outer bash has already sourced the stale `BASH_ENV` before our string reaches it. `permissionDecision:"allow"` is mandatory alongside `updatedInput` (codex rejects the rewrite otherwise) but does NOT bypass approvals — `item/commandExecution/requestApproval` still fires, so `sessionenv.UnwrapDisplayCommand` undoes the wrap in `onItemStarted` and `onCommandApproval` to keep the chat display and the auto-approve rules matching the command the agent actually asked for. The hook always exits 0: a non-zero exit or malformed output BLOCKS the tool call, so every failure path emits nothing.
- **Codex hook config + trust — session flags, never the user's config:** both the hook entry and its trust state are `-c` overrides on the app-server's own argv (`hooks.PreToolUse=[…]`, `hooks.state={"<key>"={enabled=true,trusted_hash="sha256:…"}}`), so foci writes nothing to `~/.codex/config.toml` — same isolation rule as `compact_prompt` (#1327). An **untrusted hook on `codex app-server` is silently skipped** (no error, no log; `--dangerously-bypass-hook-trust` is exec/TUI-only), and the trust hash is computed by codex over the config ENTRY, so foci cannot derive it. `probeHookTrust` starts a short-lived app-server with the same hook flags, calls `hooks/list` (available after `initialize`, needs no credentials), and reads back `key` + `currentHash`; the result is memoised per hook-config for the process, so it costs one extra codex spawn per foci-gw. The hash covers the config entry, not the script body — shipping a new `foci-codex-hook` needs no re-probe, but changing the command string or timeout does (and is handled automatically).
- **Server-death respawn:** When the shared subprocess dies unexpectedly (SIGTERM, subscriber EOF), `finalizeExit` clears the Server's `running` flag, evicts it from the pool, and synthesizes `session.error` for each registered Backend. `Backend.IsRunning()` now checks `running && srv != nil && srv.isAlive()` — previously it returned only `b.running`, which `finalizeExit` couldn't reach, so `getOrCreate` handed back the stale Backend forever. Now the next turn detects the dead server, saves the resume ID, closes the corpse, and `acquireServer` spawns a fresh subprocess (re-ensuring both plugins itself, at the spawn chokepoint).

**Lifecycle:**
1. `Start`: acquireServer (lazy pool; **ensures the shell.env + blank-system plugins before spawn**), POST /session, **write per-session env mapping** (see below), registerSession (starts dispatcher), inject system prompt (noReply:true), PATCH /config for default_permission.
2. `ImmediateInject(SourceUser)` at idle: beginTurn + POST /prompt_async.
3. SSE events arrive → Server.route by sessionID → Backend.dispatchLoop → handleEvent → SessionEvents/TurnEvents callbacks.
4. `session.idle`: build TurnResult from accumulated state, fire OnTurnComplete, flush steerBuf.
5. `Close`: **remove per-session env mapping**, unregisterSession (stops dispatcher), DELETE /session/:id, releaseServer (refcount-- → shutdown if zero).

**Per-session exec-bridge env routing (`internal/delegator/sessionenv` + `opencode/session_env.go`):** Any shared-subprocess backend pins the subprocess env (including `FOCI_SOCK`/`BASH_ENV`) to whichever session launched it first — all subsequent sessions inherit the first session's bridge socket, misrouting session-scoped exec-bridge tools (`foci_ask`, `send_to_session`). The **file format, its location, its lifecycle and the idempotent stale-aware artifact writer are shared** and live in `internal/delegator/sessionenv` (`Entry` / `Dir` / `Path` / `Write` / `Remove` / `Load` / `EnsureFile`); only the injector is per-backend. opencode's injector is its `shell.env` plugin hook: before every bash spawn opencode fires `Plugin.trigger("shell.env", {sessionID}, {env:{}})`, and a foci-generated plugin (`.opencode/plugin/foci-session-env.ts`) reads `{tempdir}/session-env/{sessionID}.json` and `Object.assign`s the correct `FOCI_SOCK`/`BASH_ENV`/`FOCI_SESSION_KEY` (the `agent/c<chatID>` session key, #1401) over the spawn env. `Backend.Start` writes the mapping from `opts.Env` (the plugin itself is ensured inside `acquireServer` before spawn, so every spawn path — interactive or batch — loads it); `Backend.Close` removes it. The plugin write is idempotent by content, so an unchanged plugin isn't rewritten and no file-watcher fires. codex uses the same files with a different injector (see the Codex section). ccstream is unaffected — each session has its own subprocess and its own bridge baked into the process env.

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
- `via` — transport that delivered the message. Derived from the context trigger via `triggerToPlatform()` in `context.go`. Values: `telegram` (Telegram), `discord` (Discord), `app` (native app), `voice` (content produced by transcribing a voice attachment — live voice-mode WS, or a voice note/attachment transcribed inbound on Telegram or the app; overrides the originating platform's own trigger, #1436), `api` (HTTP /send), `cron` (system-initiated: keepalive, wake, scheduled, etc.)

**`{cost}` / `{tokens}` (prev turn's cost and token breakdown) exist as statuslineFields but are deliberately NOT in the default template** — surfacing a running cost/token figure on every turn was found to nudge the agent toward rationing its own budget, an undesired behaviour (removed 2026-07). An agent can still opt in via a custom `statusline` config (`docs/CONFIG.md`); the bare `{cost_raw}`/`{tokens_in}`/`{tokens_out}`/`{cache_read}`/`{cache_write}` fields are also available for custom templates.

Per-session state is tracked in `sessionMeta` (in-memory map on Agent). The metadata goes past the cache breakpoint, so it doesn't affect prompt caching.

## Turn Event Stream (Sink Architecture)

All per-turn output — text, thinking, tool calls, retries, typing-indicator lifecycle — flows through a single ordered event stream defined in `internal/turnevent`. The agent is the sole producer; consumers attach a `Sink` to the turn context and receive events as they happen.

### Contract

```go
// internal/turnevent/event.go
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
| `internal/turnevent` | `BufferSink`, `NopSink` | Leaf package: event types, Sink interface, context helpers, and pure-utility sinks. No platform or turn deps. |
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
- **`asyncDispatch` / `deliverBufferedQueued`** (`cmd/foci-gw/http.go`) — as of #1385 the async **chat-delivering** paths (async `/send`, `/branch`, `/webhook`, deferred-send sweep) no longer buffer-then-forward: `deliverBufferedQueued` attaches `turnSinkForConn`'s sink and streams the turn to the resolved connection as it runs (app → `StreamingSink`; Telegram/Discord → `SessionSink`), exactly like `deliverToSessionChat`, so those paths now own their gate at the sink chokepoint (`SessionSink.Emit` / the renderer) like every other injected turn. The **`BufferSink.FinalText` → `StripSilencingSuffix` → `conn.SendToSession`** out-of-band forwarding survives only in the two carve-outs `deliverBufferedQueued` still runs buffered: `PolicyBroadcast` (final text fans out to every connection via `broadcastResponse`, no single chat to stream into — this branch keeps the `StripSilencingSuffix` gate) and `--silent` (no delivery at all). Synchronous HTTP handlers that return `buf.FinalText()` as the JSON response body deliberately do *not* apply this gate — API clients receive the raw response, sentinels and all.

What used to be at `StreamingSink.Emit` on `TurnComplete` (a single sink-level `IsSilent` check) was removed when the renderer's gates were added — it was redundant once `Finalize` had its own gate, and the sink-level check missed the case where `FinalText` was a *concatenation* of normal text + sentinel (text-then-tool-then-`[[NO_RESPONSE]]`), which exact-match `IsSilent` rejected but the streaming path had already partially delivered. The renderer's `OnReply` gate catches that case at the segment boundary, and `StripSilencingSuffix` now removes the trailing marker from the committed text rather than leaking it verbatim.

### How headless callers wire it

- **Sync HTTP `/send`/`/branch`, `/wake`, voice** (`cmd/foci-gw/http_handlers.go`, `http.go`): build a `turnevent.NewBufferSink()`, attach via `WithSink`, call `HandleMessage`, return `buf.FinalText()` as the JSON response — a blocking API caller wants the value back, not a chat delivery. (Async `/send`/`/branch`/`/webhook` and the deferred sweep instead **stream** to the chat via `deliverBufferedQueued` + `turnSinkForConn` as of #1385 — see the Injected-turns entry below and the async-gate note above.)
- **Injected turns** (`cmd/foci-gw/agents_notify.go → deliverToSessionChat`): build `turn.NewSessionSink(conn, sessionKey, trigger)`, attach, call `HandleMessage`. SessionSink owns its own delivered flag so intermediate text and final text don't double-deliver.
- **Cross-session notify** (`agents_notify.go → newSessionNotifyFn`): same as injected turns — `SessionSink` routing through `conn.SendToSession`.
- **Async notify with response routing** (`agents_notify.go → newAsyncNotifier`): `BufferSink` captures the target session's final text, then the response is routed back to the caller's session via `deliverToSessionChat`.
- **Internal hooks** (compaction memory, session-end memory, lifecycle, ratelimit replay): call `HandleMessage` without attaching any sink — the `NopSink` fallback absorbs events silently.
- **Spawn tool** (`internal/tools/spawn.go`): `BufferSink` captures the branch session's response so the tool can return it as a `ToolResult` to the parent agent.
- **Nudge extraction** (`internal/nudge/extract.go`): `BufferSink` captures the rule-extraction response for JSON parsing.

### Steering (pull-direction)

Steering is deliberately separate from the event stream because it flows the other way — the agent needs to ask the platform for pending user input at safe points inside the turn and receive a return value.

```go
// internal/turnevent/steerer.go
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

The agent can defer thoughts for later via the `remind` tool. Reminders are stored in SQLite (`reminders.db`) and surfaced as injected context when due. With `wake=true`, the session is actively woken at the specified time. `list=true` enumerates pending wakes and `cancel=<id>` cancels one (#1648).

**Tool registration:** `remind` is `ExecExport: true`, so it is exposed both as a native API tool (in API-mode agents) and as a `foci_remind` shell function via the exec bridge (in delegated/Claude Code agents). The wake-scheduling machinery (`buildWakeScheduler` in `cmd/foci-gw/agents_notify.go`) is built once per agent in `setupAgent` — transport-independent — and returns **two** callbacks: `tools.ScheduleWakeFn` and `tools.CancelWakeFn`. They are held on `sharedAgentSetup.wakeScheduleFn` / `.wakeCancelFn` and passed into `toolDeps.wakeFn` / `.wakeCancelFn`. The `remind` row in the unified tool table (`cmd/foci-gw/tool_table.go`) is `pathBoth` and gated on `reminderStore != nil && wakeFn != nil`, so the single `registerTools` driver adds it to whichever registry (API or exec) is being built.

**Cancelling a wake (#1648):** a scheduled wake is an **in-process timer**, not a DB row that gets polled — `ReminderStore.Due` explicitly excludes `wake = 1` rows. So deleting the row does **not** stop the wake: the goroutine fires anyway and delivers. Cancellation must go through `wakeCancelFn`, which cancels the wake's context; the goroutine's `Done` branch then dismisses the row, keeping row cleanup in one place. The `wakes` map entry is the wake's **claim** (#2036): it is registered before the goroutine starts, and whichever of cancel and fire removes it under `wakesMu` owns the wake. `wakeCancelFn` removes it synchronously, so it reports `false` for any wake that has already fired or been cancelled, and a `true` means the wake really will not fire (a timer that fires after losing the claim just dismisses the row). The tool's `cancel` path looks the id up via the **agent-scoped** `PendingWakes(agentID)` first, so one agent cannot cancel another's wake.

**Firing a wake (#2059):** the fire branch does NOT dismiss the row up front. It delivers via `deliverToSessionChatThen` with `wakeTurnDone` as the completion hook, which dismisses the row once the wake's turn has run — unless the turn returned `agent.ErrShuttingDown`. An injection the inbox never runs (shutdown drain, full inbox) never calls the hook at all. Either way the row stays pending and the startup restore re-fires it (a past-due wake fires immediately). The one early dismissal is a wake with no resolvable session, which could never run.

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
   - **Backend/Platform section resolution — the on-disk copy WINS over the embedded default.** `writeBackend`/`writePlatform` (`environment.go`) both go through `prompts.ResolvePrompt("", "<name>.md", embedded, searchDirs...)`, where `searchDirs` = `{workspace}/prompts` → `{workspace}/../shared/prompts` (built in `resolveSharedSetup`, `agents_shared.go`). `seedDefaultPrompts` (`agent_prompts.go`) writes an editable copy of `backend-<name>.md` (live backends only, #950) and `platform-<name>.md` into `shared/prompts` **if missing** — and never rewrites it afterwards. Consequence for maintainers: **editing the embedded `shared/prompts/backend-*.md` in this repo does NOT reach an install that already seeded that file.** Shipping a change to backend/platform guidance needs the deployed copy refreshed too (overwrite the file, or delete it and restart to re-seed; `/prompts reinstall` rewrites *all* prompt files and will clobber genuine local customisations). An unknown backend with no file and no embedded default (e.g. `codex`, `claude-code-tmux`) emits no `## Backend` section at all.

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

**Per-model defaults:** `[models.*]` config sections define named models with per-model settings (thinking, effort, speed, provider routing, etc.). These serve as both aliases (usable in `[groups]`, fallbacks, and `/model` command) and default API parameters. At request time, the hierarchy is: session override (via `/effort` etc.) → model config default → empty (API decides). The `ModelDefaultsFn` closure on both `Agent` and `Compactor` performs the reverse lookup from `developer/model_id` to `ModelConfig`.

**OpenRouter provider routing (#1478):** `[models.*.provider]` carries OpenRouter's upstream-provider-selection preferences (`order`, `allow_fallbacks`, `sort`, `ignore`/`only`, `quantizations`, `max_price`, `data_collection`, ...) as `provider.ProviderRouting` (`internal/provider/types.go`) — field names/JSON tags mirror OpenRouter's `provider` request-body object exactly. `modelDefaultsFn` (`cmd/foci-gw/helpers.go`) copies it into `ModelDefaults.ProviderRouting` alongside thinking/effort/speed; `turn_api.go` and `compact.go` copy it onto `MessageRequest.ProviderRouting` per request. `openai/translate.go`'s `buildParams()` forwards it as the top-level `"provider"` extra field — accumulated into the SAME map as the `"reasoning"` extra field before a single `SetExtraFields` call, since `SetExtraFields` *overwrites* rather than merges (a second call would silently drop whichever field was set first; this bites on nearly every request since the reference config combines `:floor`/`:nitro` with `thinking = "adaptive"`). Only meaningful when the model resolves to the `openrouter` endpoint — enforced by config discipline (same precedent as the `reasoning` injection), not a runtime endpoint check. Separately, OpenRouter's `:nitro`/`:floor` model-string shortcuts need no parsing at all: the suffix rides through unchanged as part of the model leaf via the existing `ResolveModel` → `StripDeveloperPrefix` → wire-`model` chain. Not wired into the admin/observability/guard/summariser one-off request sites, or recomputed across a fallback-chain hop (pre-existing gap, shared with `CacheTTL`/`CacheStrategy`).

**OpenRouter session grouping (#1546):** `MessageRequest.SessionKey` carries the foci session key; the direct-API request sites `turn_api.go`, `compact.go`, `agent/guard.go` (tool-result summary), `tools/spawn.go` (one-shot) and `tools/summariser_api.go` (key from ctx via `tools.SessionKeyFromContext`) set it. `openai/translate.go`'s `buildParams(req, openRouter)` sends it as the top-level `"session_id"` extra field (same single-`SetExtraFields` map as `reasoning`/`provider`), truncated to OpenRouter's 256-char cap. Unlike `ProviderRouting`, `SessionKey` is set for every endpoint, so this one IS runtime-gated: the `openRouter` flag is `Client.isOpenRouter()` (base URL contains `openrouter.ai`), and other OpenAI-compatible endpoints never see the field. Only the openai-format client sends it — an openrouter model resolved to anthropic format goes through the anthropic client and carries no `session_id`.

**Compaction:** `Compactor.Compact()` receives the client, model, and format as parameters (not stored on the struct). The caller resolves these via `agent.ResolveCallSite(config.CallCompaction, sessionKey)`, so compaction uses the group-appropriate model in multi-model mode or the session's active client in single-model mode.

### Live Model Capabilities (`modelcaps`, #840)

`internal/modelcaps` is a leaf cache of `Caps{ContextWindow, MaxOutput, Effort, Thinking}` advertised by backend catalogues (Anthropic `/v1/models`, Codex app-server `model/list`). It is the live layer between the per-model config override and the static `modelinfo` registry: context-window and effort lookups prefer it, falling back to the static registry on a cold/empty cache so behaviour is never worse than before.

- **Per-backend registry.** Capabilities are a property of the backend *type*, not the model alone, so the cache keeps separate stores for `BackendCCStream`, `BackendAPI`, `BackendCodex`, and any other delegated backend name. `BackendKey(configBackend)` maps configured transport names to those keys. Public API: `LookupFor`, `ModelsFor`, `SetFetcher`/`Refresh` for pull catalogues, and `Publish` for catalogues discovered by a live backend instance. Background pull refresh is single-flight and serve-stale.
- **Fetcher seam.** `anthropic.FetchModelCaps` (raw `GET /v1/models`) is injected via `SetFetcher` so the package stays a DB/anthropic-free leaf. `AnthropicResolver.ModelCapsFetcher` supplies it from CC OAuth creds; nil creds → no fetcher → static fallback.
- **Codex publisher and resolver.** After each app-server initialize handshake, the Codex backend pages through visible `model/list` results. It preserves catalogue order and each model's ordered `supportedReasoningEfforts`, enriches omitted structural fields from exact `modelinfo` entries, then publishes the complete snapshot under `BackendCodex`. `backend_config.model` and `/model` accept exact IDs or case-insensitive substring aliases; exact wins, otherwise numeric version components rank matches newest-first with catalogue order as the tie-break. Fresh sessions send the resolved ID in `thread/start`; resumed sessions and runtime overrides send it in the next `turn/start`. Foci persists `codex/<id>`, while the wire receives the bare ID. `/effort` accepts the exact advertised levels.
- **DB persistence (`54c6a8e2`).** `SetPersister` + `Restore` bridge the cold-start gap for API, Claude Code, and Codex. `session` stores the shared primitive shape in `model_caps`; saves are transactional (delete+insert) so a reader never sees a half-written catalogue. `cmd/foci-gw/modelcaps_persist.go` adapts `SessionIndex`↔`Caps`. Both fetched and published snapshots persist outside the store lock; `Restore` declines to clobber a cache a live result already populated.
- **Agent routing.** `Agent.BackendType()`, `Agent.ModelCaps(model)`, and `Agent.BackendModels()` route caps reads through the agent's own backend; consumers (session context limit, command context-limit resolver, `/effort` choices, `/model` keyboard) read via the agent. Compaction takes an injected `ModelCapsFn` bound to the agent's backend.

**Effort plumbing.** `/effort`'s level set is resolved per call: `newSessionSettingCommand`'s optional `DynamicChoices` hook reads `modelcaps.LookupFor`, building levels in catalogue order (e.g. opus-4-8: low/medium/high/xhigh/max) with matching numeric aliases; a catalogue miss falls back to the static low/medium/high. Two delivery paths make effort both instant and durable:
- **Live push (`55e6fb0e`).** `Agent.SetSessionEffort` persists, then for a delegated session fires `delegator.ApplyFlagSettingsRequest{Settings: {"effortLevel": value}}` in the background → ccstream's `SendControl` emits `{"subtype":"apply_flag_settings","settings":{...}}` so the next turn runs at the new effort with no bounce (mirrors `SetPermissionMode`'s optimistic fire-and-forget). The command layer must reject invalid settings first — CC does not validate. API-loop sessions apply effort at turn time via `output_config`, so no control is sent. `clear`/`off` skip the live push.
- **Cold-launch flag (`c30154f2`).** `apply_flag_settings` is session-local: a bounce (post-compaction reload, idle respawn) drops the override. `StartOptions.Effort` + `EffortFunc(sessionKey)` (mirrors `SystemPromptFunc` — resolved fresh per session start in `getOrCreate`, bound to `ag.SessionEffort`) make ccstream `Start` append `--effort <level>` (empty/`off` omits it). The control is the happy path; the launch flag is the backstop.

**`/thinking` backend gate (`5967d901`).** CC exposes no thinking control and effort subsumes it, so `/thinking` is hidden on ccstream via a backend-keyed `BackendGate` on `sessionSettingDef` (distinct from the model-keyed `Capability`), consulted in both `Visible` (hide) and `Execute` (reject). API agents keep it.

**`/model` keyboard (`ca928e22`).** `/model`'s `KeyboardOptions` now offers one button per model `Agent.BackendModels()` (→ `modelcaps.ModelsFor`) advertises, marking the current model with a check. A cold catalogue falls back to typing the name.

**Keepalive:** For Anthropic endpoints, the keepalive fires on a configurable interval (default 55m, just under the 1h cache TTL). For OpenAI and DeepSeek models, keepalive is auto-detected by developer name via `config.ResolveModelKeepalive()` — these developers have a 5-minute prompt cache TTL, so keepalive fires every ~4m45s. Gemini's `CacheManager` handles its own TTL extension independently.

**Per-session warm window (`keepalive.go` `keepaliveTargets`).** Keepalive is gated per candidate session, not by a single agent-wide timer: a session is warmed only if its `session_index.last_cache_touch` (stamped at turn entry on every non-memory turn by `recordTurnActivity`, then advanced per round mid-turn by `touchTurnActivity` — see "Mid-turn activity heartbeat" below) is in the window `[interval, cacheTTL)` — due for a refresh but not yet expired. A session with **no** recorded touch (never warmed, or just reset — `ClearSessionState` nulls it via `SessionIndex.ClearCacheTouch`) is skipped: there is no live cache to keep alive, so keepalive won't fork/inject into a cold session. `cacheTTL` is the backend's static constant (`DelegatedManager.StaticCacheTTL()`, CC = 1h), resolved once at setup; `0` = unknown → interval-only gate. `setupPeriodic` warns at startup if `interval >= cacheTTL` (empty window → warming can never fire). This replaced the former in-memory agent-wide `lastCacheWarmed` field (removed) — `last_cache_touch` is the persisted, per-session source of truth, so the `onTurnComplete` lifecycle hook that fed `lastCacheWarmed` is now nil. The in-flight check is the LAST filter per candidate, so a skip logged (DEBUG) as `due but turn in flight on <key>` means exactly "this session would have been warmed now but for the in-flight guard" — the gate decision `TestL2_Cron_KeepaliveSkippedWhenTurnInFlight` syncs on (#2084). That test cannot use cc-stub's recorder: the fork fails under the stub, and the in-place fallback is serialised behind the in-flight turn by the inbox, so even a keepalive the gate wrongly let through only reaches the stub after the turn ends.

**Client cache-warmth indicator (#1217).** The app's `cacheExpiry` frame is now driven by the same `last_cache_touch`, not by turn-complete. `Agent.CacheExpiry` returns `last_cache_touch + TTL`; with no touch it returns zero (no cache — the client renders WARM) unless the session has a saved backend resume id (`cc_resume_id`), in which case the touch was lost, not absent, and it returns a positive long-past instant so the client renders COLD (#2061), and `Agent.emitCacheExpiry` pushes it via the `onCacheExpiry` hook (wired to `app.SetCacheExpiry` → `activeHub.bindingForSession(...).setCacheExpiry`, deduped) on **every** touch write: `recordTurnActivity` (turn entry), `touchTurnActivity` (per-round mid-turn heartbeat — see below), `TouchRootCacheForBranch` (branch warms root), and `ClearCacheTouch` (reset → cold). Previously the frame was emitted only by the app sink at `TurnComplete`, so a session kept warm by keepalive **forks** (which complete off-root) or by external/cron turns (no live app sink) advanced `last_cache_touch` but never refreshed the client — the indicator went stale and showed "expired" while the cache was actually warm. The persistence-hook path fires regardless of whether a live app socket is attached (the durable binding's `cacheExpiryMs` seeds the roster snapshot on reconnect). `TTL` resolution (`Agent.cacheTTLFor`, #1446): config override (`ModelDefaultsFn`) first, then the session's *live* backend TTL (`DelegatedManager.CacheTTL`, requires a running backend), then the backend type's *static* TTL (`DelegatedManager.StaticCacheTTL` — no live session needed, same throwaway-backend trick the keepalive setup above uses), else 5m. The static fallback exists because right after a gateway restart no session has a running backend yet, so the live lookup reports 0 for every session; without the static fallback a CC session's real 1h TTL silently understated to the 5m default until its next touch, showing a still-warm session as cold (avatar grayscaled) post-restart.

**Mid-turn activity heartbeat (`touchTurnActivity`, `sink_logging.go`).** `recordTurnActivity` writes the timestamps once at turn entry; to keep them fresh through a long turn, `loggingSink` (the universal per-turn sink wrapper — attached at these sites, and every delivery path must use one of them or its replies go unlogged: `run_turn.go` for platform turns, `in_flight.go` for adopted autonomous runs, `inbox.go` for late delivery, and `turnSinkForConn` (cmd/foci-gw) for the async `/send` path and all system-injected deliveries; plus, for NON-delivered turns of a recorded kind (#2060: reflection, session_end_memory, background, consolidation, branch — `nonDeliveredTurnKinds` in `context.go`, the single which-turns-are-recorded rule; keepalive, compaction memory and the batch utility purposes are not recorded at all — #2094 routes every TurnState-based record site (`LogConversationRecv`, `LogConversationSent`, the API per-round `logConversationSent`, `logConversationThinking`) through `recordTurnEntry`, which applies it on both transports), `turn_orchestrator.go` Phase 3.5, which registers the system turn's NopSink/BufferSink wrapped by `recordingSystemSink`, and `batch_turn.go`, which registers a consolidation batch's wrapped sink before dispatch. Those rows carry `turn_kind`. The broadcast fan-out is the one exception — it runs behind a `BufferSink` that emits no `TextBlock`, so `broadcastResponse` calls `agent.RecordConversationSent` directly. Besides main-thread `TextBlock`s, `loggingSink` records subagent output (#2060): `SubagentStart` (label + prompt) and `SubagentPrompt` as `content_type='subagent_prompt'`, `SubagentText` as `'subagent'`, each with `subagent_group` (the Agent tool_use id) and `subagent_run`, under the parent session key; subagent tool calls/results are not recorded) also calls `Agent.touchTurnActivity` on each **round** event (except the #2060 recording wrapper of a non-delivered turn, which only records) — `TextBlock`, `ToolResult`, or `Activity` (emitted by both the API tool loop and the delegated ask-cycle path via the shared `emit*` helpers). It advances `last_cache_touch` (always) and `last_activity_at` (unless `isMemoryTrigger`), never `last_user_activity_at`; debounced to ≤1 write / 20s / turn and gated on `IsTurnInFlight` (so the wrapper reused for post-turn late delivery no-ops). It does not recapture `prevRequestTime` (a per-turn-entry concern). Separately, the `/send` activity gate consults an in-memory turn-**end** signal, `Agent.LastTurnEnd` (stamped in `markInFlight`'s decrement when `inFlight`→0), so `--wait-cold`/`--if-cold` measure continuous dead time — no turn running now AND none finished within the window — rather than releasing into the gap between back-to-back turns.

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
- **Subprocess group boundary:** `procx.Setup` (`internal/procx`) drops the `foci-secrets` group from spawned children and, after the startup probe, clears the ambient capability set on every OS thread (`syscall.AllThreadsSyscall`; needs the `CGO_ENABLED=0` build, else it fails closed) so children can't inherit `CAP_SETGID` and re-add the group. The foci user is granted the group per-process (systemd `SupplementaryGroups` / `setpriv --groups` / `runuser --supp-group`), never as an `/etc/group` member. See [SECRETS.md](SECRETS.md).
- **Outbound SSRF:** `web_fetch` and `http_request` share one SSRF-safe `*http.Client` (`internal/tools/safehttp.go`): its dialer validates each resolved IP at connect time (blocking loopback/private/link-local/metadata/unspecified, defeating DNS rebinding) and re-checks every redirect hop.

**Bitwarden integration** (`secrets/bitwarden/`): Optional dynamic secret store. Depends only on `log` (leaf package). Two-tier aisudo model:
- Metadata refresh: `sudo -u bitwarden bw list items` (allowlisted, auto-approved)
- Password fetch: `sudo -u bitwarden bw get password <id>` (requires Telegram approval)
- Template syntax: `{{secret:bw.UUID}}` — resolved in both `http_request` and `shell` (approval-gated, safe for both)
- Host validation: vault item URI fields → allowed hosts (same pattern as `allowed_hosts` in secrets.toml)
- TTL-based caching with background cleanup goroutine

## Logging (`log/`)

**Two-phase init:** Before `log.Init()`, events go to stderr and are buffered in memory. When `Init()` opens the event file, buffered events are replayed to it. This ensures config-load warnings (e.g. unknown keys) appear in the log file despite being emitted before the file path is known. **Startup rotation cuts at process start, not "now" (#1869):** main's early `log.Init` (default path, or `FOCI_LOG_FILE`) is writing the event file from before `config.Load`, but the startup `RotateOnce` needs cfg and so runs later in `initLogging`. It therefore archives only lines stamped before `processStart` (taken first thing in `main`, truncated to the second because foci.log stamps are whole seconds) via `RotationConfig.Before` — a "now" cutoff swept this lifetime's config/shellenv/preload lines into the archive and left the live log starting at the `[rotate]` line.

**Rotation and stale-inode self-heal (`rotate.go`, `log.go`):** `StartRotation` runs `rotateAll` on a timer (`rotation_period`, default 24h): it moves lines older than `retention_period` into a gzip archive via `os.Rename`, then always calls `log.Reopen()` so the writer's fd follows the renamed-in file. `os.Rename` never disturbs an already-open fd on its own — that's why the paired `Reopen()` exists. **The final rename runs inside `log.SwapUnderWriterLock`**, which holds the writer mutex across the rename AND the reopen: pairing them in sequence was not enough, because the very next write — rotation's own `rotated %s: archived %d old lines` line — took the lock first, found its fd on the replaced inode and emitted the stale-inode WARN, once per rotation, every rotation. Holding the lock makes the swap invisible to writers, so the detector below now fires only for genuinely external replacement. (The callback must not log: it runs under that mutex.) If something OUTSIDE this pair replaces one of the three files directly (external truncation, a stray script, anything that doesn't call `Reopen()` afterward), the open fd is silently orphaned: it keeps appending into an unlinked inode that no rotation pass — and no rotation cadence, however short — can ever see again, while the visible file at the path stays empty. This actually happened in production (foci_todo #1479: `api-payload.jsonl` archiving stopped silently for 4 months). Fix: every write through `event()`/`api()`/`payload()` now compares the open fd's `(dev, ino)` (fstat) against a fresh `stat` of the configured path before writing (`staleFile`/`reopen{Event,API,Payload}IfStaleLocked` in `log.go`); on a mismatch it reopens the current path immediately (no 24h wait) and logs a WARN (debounced to once per minute per file via `Logger.staleWarns`, a `WarnLimiter` keyed by log path with base == max == `staleWarnCooldown`, so a persistently-failing reopen — e.g. the replacement dir is gone — can't flood the log or recurse into itself through `event()`; the interval is deliberately FIXED rather than escalating here because it bounds that recursion, unlike the app's outage warning which escalates).

Four outputs:

1. **Event log** (`foci.log` + stderr): `2026-02-21T03:52:39Z INFO  [telegram:mybot] message from rich: hello`
   - Package-level: `log.Infof("component", "format", args...)`
   - Per-component: `log.NewComponentLogger("telegram:" + agentID)` → `logger.Infof("format", args...)`
   - Major components (Agent, Bot, Keepalive, Compactor) carry a `*log.ComponentLogger` field
     initialized at construction with a prefix like `"agent/mybot"`. This avoids repeating
     the component string at every call site and encodes the agent ID for multi-agent setups.
   - Levels: DEBUG < INFO < WARN < ERROR
   - Newlines in messages are replaced with literal `\n` to guarantee one log line per event

2. **API log — JSONL** (`api.jsonl`): one line per call the cost ledger books — the
   call's ledger fields (`ts` = billed_at, backend, kind, tokens by class, `cost_usd` as
   Book priced it, context_fill, turn_id, actor, detail). Written by `accounting` after each booking commits, through
   `log.AppendAPILine` (this package owns only the file: rotation, stale-inode reopen).
   It is the readers' fallback when there is no ledger (`accounting.ReadJSONL`, which
   re-prices counts at read time as the views do). Archived to empty on every start.

3. **API log — SQLite** (`api.db`): the per-call cost ledger. Owned by
   `internal/delegator/accounting` — see "Cost ledger" for its schema, writers, readers and
   the startup cutover. `log` no longer touches it.

   **The pre-ledger rows (history).** Before the #2111 cutover api.db held one `api_calls`
   row per turn (v1). The cutover migrated every v1 row into the ledger as one
   `kind='legacy'` call (see "Cost ledger" → "Migration"), and since the Claude Code switch
   (#2115) no backend books that way: every call since is booked per call by its backend's
   adapter. Reading migrated history against the v1 names: `calculated_cost_usd` (foci's
   own figure, the authoritative one) is `legacy_calculated_cost_usd`, which the views take
   verbatim (`cost_basis='recorded'`); the `turn_*` group (the turn's summed counts) is the
   call's `call_tokens`; the un-suffixed four (the final cycle's context fill) are the turn's
   `legacy_*`; `cost_usd` (the backend's own figure — CUMULATIVE per CC process, never to be
   summed, #1674) is a `backend_reports` row; `subagent_id` is `actor`; `call_type` is
   `detail.v1_call_type`. Rows before #1674 have no calculated figure and are priced from
   their counts.

   **Turn identity.** `turn_id` is `"<session>@<StartedAt UnixNano>"`, from
   `TurnState.RowID()` — not `TurnState.TurnID`, an in-process counter that restarts at 1
   on every foci restart. Session key plus start nanosecond is unique because turns are
   serialised per session. Every call of a turn carries it, its subagents' included (on the
   turn that SPAWNED the subagent, however late its calls land, #1880).

   **`agent_id` and `actor` (#1946).** `agent_id` is the AGENT that owns the call,
   populated on every call — including a subagent's, where it is the owning agent, not the
   subagent. `actor` is the subagent (the Agent tool's `tool_use` id, which also names its
   transcript, `.../subagents/agent-<id>.jsonl`), `''` for the session's own thread.
   `session.AgentIDFromKey`/`AgentIDFromAnyKey` (`internal/session/key.go`) is the ONE
   parser for deriving an agent from a session key; `scripts/langfuse-etl/etl.py`'s
   `agent_of` mirrors it by hand.

   **`purpose` (#1962)** is on the TURN, set only for a batch turn — consolidation, nudge
   extraction, the delegated `foci_summary` tool, the delegated `/prompts diff` summary
   (`delegator.BatchPurpose*`). A batch is an ordinary delegated turn in every other
   respect (see "Batch runs"), so its cost is in the agent's totals like any turn's.
   Absent is **NULL, not `''`**.

   **Reading migrated subagent shares (#1863).** A v1 turn with subagents was SPLIT across
   rows: the parent row's cost had each subagent's share subtracted, so cost consumers sum
   every call; anything COUNTING calls skips the shares — `accounting.CallRow.Counted()` is
   the predicate (a legacy call with an actor is a share, not a call), and `sumCosts` does
   both halves in one function. `/last` prints the context fill (`ctx=`) from the turn's
   `legacy_*` for a legacy parent call.

   **Read cost from the ledger's views** (`call_costs`, `turn_costs`, `daily_costs`,
   `session_costs`), which switch on `cost_basis`. Never `SUM` a stored figure yourself.

4. **Conversation log** (`conversation-{agentID}.db`): Per-agent SQLite databases logging exact Telegram messages sent and received. Entries are routed to the correct agent's database by parsing the session key. Table `messages` with columns: `id`, `ts`, `direction` (recv/sent), `user_id`, `username`, `chat_id`, `text`, `parse_mode`, `session`, `error`.
   - Use: `log.Conversation(log.ConversationEntry{...})`
   - Queryable with `sqlite3 conversation-clutch.db "SELECT * FROM messages"`
   - Useful for debugging formatting (see exact markdown sent vs plain text fallback)

## Cost ledger (`internal/delegator/accounting`, #2111)

The per-call ledger that replaced the per-turn `api_calls` rows and the window-subtraction
pricing of each backend's turn (see "Logging" → the pre-ledger rows). Design: clutch `notes/2111.md` (revision 3), rulings R1-R10 on todo #2111.
**Built in phases.** P1 (#2113) was the schema, the accounting core, the class rates in
modelinfo and the migration. P2 (#2115) switches one backend per change, deleting its old
path in the same change, so no backend ever has two booking paths:
- **Direct API: switched** (the first, with the cutover). One response is one call.
- **opencode: switched** (second). One assistant message is one call (see "opencode
  adapter" below).
- **codex: switched** (third). One `thread/tokenUsage/updated` is one call (see "codex
  adapter" below).
- **Claude Code: switched** (fourth and last, after a 1-2 day shadow run compared by
  `foci-gw ledger-shadow`). Each API call is one call (see "Claude Code adapter" below).

Every delegated backend implements `delegator.LedgerBooker`. The agent layer records a
delegated turn at its start (`buildTurnEvents`, before any call can name it) and at its end
(`closeLedgerTurn`: `ended_at`, `activity_closed_at`, stop reason, model), books no turn-level
row, and takes `FinalCost` (the sink header's figure) from `Ledger.TurnCost`; a backend
that is not a `LedgerBooker` books nothing and `LogUsage` logs the gap. An adapter names
the open turn with a stub (id, session, backend) whose upsert never overwrites what the
agent recorded. `activity_closed_at` is set at the turn's end, except for a backend that is
a `delegator.TurnActivityCloser` (Claude Code): its spend can outlive the turn, so it closes
the activity itself (see "Claude Code adapter"). A codex child that outlives its turn still
reads `still_running` false early (not yet handled for codex). At startup
`Ledger.CloseOrphanedTurns` closes any turn an earlier gateway left running (at its last
call, else its end, else its start): no backend process survives a restart.

**The cutover** runs at startup: `initLogging` calls `accounting.Open(cfg.Logging.APIDB)`.
On a pre-ledger api.db (`api_calls.calculated_cost_usd` exists) that takes a `VACUUM INTO`
backup (`<api.db>.pre-ledger-<stamp>`) and migrates in one transaction (see "Migration");
the report is logged under `ledger`. Every later open finds the ledger and only re-renders
the rates. The ledger then becomes the process's live one (`accounting.SetLive`), which is
how every writer and reader reaches it; `accounting.Live()` is nil with no `api_db`
configured (and in unit tests), and writers then only observe (api.jsonl, BookedHook).
Alarms go to `OnAlarm`, which reports each through `delegator.Expectations.Violated`
(invariant `ledger <inv>`): an ERROR, so operator chat, rate-limited per backend and
invariant (#2013). An adapter alarm carries the backend's version (`Alarm.Version`; CC's
ccBook takes it from `system/init` via `setVersion`), so it names the release (#2149); an
alarm found inside a booking (`Tx.alarm`) carries none and reads "version unknown".
A live-applied `[[modelinfo]]` change re-renders `token_rates` (`Ledger.RenderRates`, from
`liveapply.go`), so SQL keeps pricing like Go. `foci-gw ledger-migrate` still dry-runs the
migration on a COPY of an api.db.

**Token classes are data (R1)** — `modelinfo/classes.go`. A `Class` is a string from one
vocabulary (`input`, `output`, `cache_read`, `cache_write_5m`, `cache_write_1h`,
`cache_write` = TTL not reported, `reasoning`, `web_search`, `web_fetch`), each with a unit
(`mtok` or `each`) and a flag for whether it fills context. Classes are disjoint: an adapter
normalises overlapping provider fields before booking. `Model.Rates map[Class]float64`
replaced the named rate fields; `Prices.Rates` is the ONE place the sources' named figures
(models.jsonl, `[[modelinfo]]`) become class rates — a zero figure means "not published"
and leaves the class out (input and output excepted), the TTL-unknown and 1h classes take
the higher write figure, `reasoning` defaults to the output rate, `web_fetch` is always $0.
`config.toModel` merges an override through `Model.Prices()` and back.
`modelinfo.CostAsOf(model, at, Tokens) (usd, priced)` is the ONLY pricing function (`Cost`,
`CostAsOfSplit`, `CacheWrites`, `TTLSurchargeAsOf`, `WebSearchCostAsOf`, `cacheWriteRate`
and `ttlPremium` are gone). `priced` is false when a billed class has no rate or the model
resolves to no rate group — there is no guessed fallback rate any more (the old haiku /
OpenAI-$5/$15 guesses), and `UnpricedModelHook` says so. Family pricing on the as-of path
now uses the newest family member (#1967), as the latest-price path always did.

**Rate groups.** `ResolveRateModel(model, at)` names the history group a call is priced
from — `"<leaf>|<provider>|<dev>"`, the call's `rate_model`, resolved once at booking
(exact leaf → variant-stripped → punctuation-folded → family canonical; CC's `<synthetic>`
resolves to a zero-rate group). `RatesAsOf(rateModel, at)` is that group's row in effect on
`at`'s UTC date. `RateTable()` renders every group into dated per-class rows with exactly
that rule (a group's first row is in effect from `""`, same-date rows resolve to the last,
a class a later row drops is rendered NULL), which is what lets SQL price like Go.

**Schema** (`accounting/schema.go`; FKs on via the DSN):
- `api_calls` — one row per API call on every backend (R3). `call_key` (provider message id
  or an adapter-built stable id) with a unique index on `(backend, call_key)` IS the dedup;
  NULL only on `kind='legacy'`. `billed_at` is UTC, fixed-width nanoseconds (sorts as text;
  its first ten characters are the rate date). `rate_model` NULL = unresolved. `turn_id` NULL
  only on `kind='overhead'` (R6), and must name a `turns` row. `kind` (call, compaction,
  overhead, summary, spawn, legacy), `finality` (completed, stopless, interrupted, derived,
  legacy), `class_method` (observed, backend_rule, solved, kind, unknown) are CHECKed.
  `legacy_calculated_cost_usd` is the pre-ledger recorded figure on legacy calls only.
  `cost_basis` says per call where the views take its cost from (Dick, 2026-09-29 12:08):
  `counts` (price `call_tokens` at the dated rates — every live call) or `recorded` (take
  `legacy_calculated_cost_usd` verbatim — a legacy row whose stored counts are not what was
  billed). The view switches on it with a CASE, never a COALESCE. No cost column.
- `call_tokens(call_id, class, count>0)` — the counts.
- `turns` — per-turn facts, never cost; `activity_closed_at` NULL = still spending (R8);
  `legacy_*` = context fill COPIED from a pre-ledger row (R4).
- `backend_reports` — each backend's own totals at its native grain (R5): CC cumulative,
  opencode per call, codex tokens only.
- `token_classes`, `token_rates` — rendered from modelinfo in full on every `Open`.
- Views: `call_class_costs` (per call per class, `count / per_units * rate` — CostAsOf's
  term order), `call_costs` (by `cost_basis`; on counts, NULL when any class is unpriced,
  never a silent 0, and 0 for a call with no counts; every finality priced, `interrupted`
  included — #2111 §13.8 ruling), `turn_costs` (priced sum + `unpriced_calls`, parent/subagent split,
  `still_running`, `context_fill` from the last parent call or the legacy copy),
  `turn_class_costs` (counts-basis calls only: a recorded figure has no class split),
  `daily_costs` and `session_costs` (UTC days, by billing time).

**Writing** (`accounting/ledger.go`): `Ledger.Update(func(*Tx) error)` is one transaction;
`Tx.Book(Call)` validates the Call contract, resolves `rate_model`, `INSERT OR IGNORE`s the
call and its non-zero counts, and on a duplicate key compares counts
(`invSameIDDifferentUsage`; the first booking stands). `invModelNotInTable` /
`invClassNoRate` fire for a call the views will show as NULL. Alarms go to
`Ledger.OnAlarm` only after the transaction commits. `Tx.Report` stores a backend report
(the record each backend's checks compare against: they run in the adapters, on the same
figures, and `ledger-shadow` re-runs CC's offline); `Tx.RecordTurn` upserts a turn, a zero field leaving the
stored value alone. A call whose model is not known yet (`""`, #1290) is unresolved
without asking modelinfo, so it alarms invModelNotInTable but never trips the
unpriced-model warning.

**Observers** (`live.go`, `jsonl.go`). Every call Book inserts becomes a `Booking` — the
call, its id, its cost priced exactly as `call_costs` prices it, its turn's purpose, and
the context fill it leaves — handed, once the transaction COMMITS, to the api.jsonl
writer and to `accounting.BookedHook` (the trace exporter's generations, see "Tracing").
A rolled-back booking is never observed; a duplicate key is not re-observed; the
migration books quietly (history is neither re-logged nor re-sent).

**Direct API adapter** (`apicall.go`, the backend switched in P2). `APIResponse.Call()`
makes one response one call: `backend='api'`, key = the provider's response id (else
`<kind>:<session>@<start nanos>`), `finality='completed'`, cache writes `cache_write_1h`
by the backend rule (`provider.Usage.Tokens`, `class_method='backend_rule'`), billed at
the request's start, `duration_ms` (and a compaction's `pre_messages`) in detail, provider
inferred from the model when the caller named none. `accounting.Record(turn, call)`
upserts the turn and books the call in one transaction. Writers:
- `agent.logAPIResponse` — each call of a conversation turn, on `TurnState.RowID()`, with
  the turn's source (user / keepalive / batch / autonomous) and purpose;
  `APITransport.RunInference` closes the turn at its end (`ended_at`,
  `activity_closed_at`, stop reason, model), since the API's spend is final with its last
  response (R8).
- `compaction.Compactor` — `kind='compaction'`;
- `tools` summary (`APISummariser`) and one-shot spawn — `kind='summary'` / `'spawn'`.

**A helper call is booked on the turn that invoked it** (Dick, 2026-09-29: "subagent turns
should be tied to their parent"): its own call row, the invoking turn's `turn_id`.
`OrchestrateFullTurn` attaches the direct-API turn to `ts.Ctx` (`accounting.WithTurn`),
which is the context the turn's tools run under (`executeToolCalls`) and its post-turn
compaction runs under (`APITransport.RunCompaction` → `maybeCompact`); each helper books on
`accounting.TurnFor(ctx, own)`. A post-turn compaction spends after the turn ended, so it
moves the turn's `activity_closed_at` on (`recordAPITurnActivity`). Only a helper call no
turn invoked keeps a turn of its own, minted `<session>@<start nanos>:<kind>`
(`MintTurnID`, `OwnTurn`: source `compaction` or `system`, never counted as the session's
turns): an operator's `/compact` (`Agent.CompactSession` from the command layer), and a
summary or spawn reached with no turn on its context. Delegated turns attach none: their
helper calls never take these paths (a delegated agent summarises through a batch turn,
compacts inside its backend, and has no spawn tool).

**opencode adapter** (`opencode/ledger.go`, P2 second switch). Every assistant
`message.updated` with `time.completed` set is one call: key = the message id, on the foci
turn open on the backend (subagent messages and an in-turn automatic compaction included),
`finality='completed'`. Classes: `input`, `output`, `reasoning` (billed ON TOP of output at
the output rate, #2112 P0-d — foci dropped it before, so reasoning models were
under-priced), `cache_read`, and `cache_write` (TTL not reported, `class_method='unknown'`);
they are disjoint — `tokens.total` is their sum. opencode's own `cost` is a per-call
`backend_reports` row, booked with the call in one transaction (`Ledger.RecordCall`), and
still feeds the shared `CostDivergenceChecker` against the ledger's price. A compaction's
summary message (`mode:"compaction"`, `summary:true`) is `kind='compaction'`: on the open
turn when opencode compacts inside one, else (an operator's `/compact`, between turns) on a
`source='compaction'` turn of its own, closed at once. A subagent's messages: the subscriber
now reroutes a child session's `message.updated` to its parent Backend (with the task tool's
callID, or `(unnamed)` when no tool part named the child), booked with that actor — before
the switch they were dropped, so subagent spend was missing. Before the switch opencode's turn
row also priced only the turn's LAST message (`lastUsage` was overwritten per message);
multi-message turns were under-counted. `lastUsage` is now context fill only.

**codex adapter** (`codex/ledger.go`, P2 third switch). codex is token-only: no message id
and no cost anywhere (#2112 P0-c). Each `thread/tokenUsage/updated` is one API cycle and one
call, keyed `<threadId>:<total.totalTokens>` — unique and monotonic per thread, and the same
key when codex re-delivers a notification (the ledger books it once). Classes from `last`:
`input` = inputTokens − cachedInputTokens (cached is a SUBSET of input,
`invCachedIsSubsetOfInput`), `cache_read` = cachedInputTokens, `cache_write` =
cacheWriteInputTokens (TTL not reported), `output`; `reasoningOutputTokens` is inside output
and goes to detail only. Model `codex/<model>`, provider `openai`. The thread's running
`total` is a `grain='cumulative'` backend report with no cost. Conservation — total grows by
exactly last (`invTotalIsSumOfLast`, `checkTokenUsage`) — runs on every notification, a
child thread's included. The call is booked on the open foci turn; a cycle with no foci turn
open gets an autonomous turn of its own. A child thread's cycles (subAgentActivity, routed to
`handleSubagentNotification`) are its subagent's calls: actor = the subagent's group key,
booked on the turn that SPAWNED it (`subagentTracker` records it at first sight, #1880) —
before the switch they were consumed unread, so codex subagent spend was missing. The
`turnCalc` accumulator and its per-turn `CostAsOf` are gone; `stashedUsage` is context fill
only. Tested against `codex/testdata/tokenusage_synthetic.json`: the #1855 live cycles plus
SYNTHETIC child-thread and re-delivery notifications; replace them with a real capture when
one is available.

**Claude Code adapter** (`ccstream/ccbook.go` state machine, `ccstream/ccledger.go`
wiring; #2111 §3.1, §4, §5; P2 fourth switch, #2115). Each CC process runs one adapter
(`newCCLedger` at `Start`, closed — drained and flushed — when the process exits, at
`finalizeExit`) booking into `accounting.Live()`; with no live ledger a call is still
observed (api.jsonl, BookedHook), as every backend's is. The hooks only enqueue (non-blocking;
a full queue drops, logs at ERROR and counts — that process is then under-booked), and one
goroutine per process runs the book. Inputs: the stream's top-level assistant ids with the
foci turn open when each was named (`OnAssistant`); results' cumulative `modelUsage` with the
running-subagent count (`OnResult`); `compact_boundary`; every subagent transcript line,
ungated, and each tail's open/close (`subagentTailManager.ledgerLine/ledgerTail`); every
Workflow run's agent transcripts (`workflow_tail.go`, #2130: a run's agents write under
`subagents/workflows/<run id>/agent-*.jsonl`, never on the stream, and the stream's one
`local_workflow` `task_started` names only the run; the Workflow tool's PostToolUse
`tool_response` carries `transcriptDir`, and one goroutine per run polls it until every agent
is at rest after the run's `task_notification`); and a MAIN-transcript tail from the file's
size at launch (a resumed or forked session's history is an earlier process's), started at
Start for a resume or at `init` for a fresh session, which also sees the `cost-state` record
a graceful close appends. Rules (ccbook.go header, one test each): a main call books from its
transcript line on the turn that named it, only if the stream named it — a line never named
is a history copy (F1b: re-appended after every 2nd compaction, batch writes) and is dropped,
as is a re-append of a call already booked; a named call seen only at a stopless line is
`interrupted` — booked and priced from it at the result's settle, excluded from the
remainder; a subagent call books on its spawning turn, `completed` on its stop_reason line or
`stopless` at the next id in its file or its tail's close, billed at its own line's time (so a
subagent running past midnight books each call to its own day), and a subagent line billed
before the process launched is skipped — which is also why a reactivation tail, re-reading
the transcript from byte 0, books no earlier run again (#2057, #2087); a Workflow run is an
open agent from its PostToolUse, on the turn open then (or, invoked by a subagent, that
subagent's spawning turn), and each of its agents books on the run's turn and window however
late its transcript appears, so no remainder is booked mid-run; each result settles once its
named calls are in (bound 250ms) and, if no subagent was running, once every tail is at rest
(bound `subagentTailSettle`); a bound passing is final only after a main-tail read that began
past it (each drain reports its start after its lines, #2134), so a stalled tail delays the
settle instead of dropping a call — then the remainder `modelUsage − baseline − Σ counted
calls` per model and class (CC's utility calls, stopless subagent output, compactions) is
booked as overhead (no turn) or, if the window held a `compact_boundary`, as a compaction (on
the turn open at the boundary, else a compaction turn of its own); modelUsage has no
cache-write TTL, so the remainder's writes are split 5m/1h by solving CC's own cost of the
interval less the counted calls' (`solveRemainderTTL`, `class_method=solved`; exact, to
`accounting.SolveTolerance`, like the migration's solve), and one that will not solve stays
TTL-unknown at the 1h rate and alarms `invRemainderTTLUnsolved`; a negative class alarms
`invNegativeRemainder` and books nothing; a named call whose line never came alarms
`invStreamIdBooked`. At exit the last remainder comes from the `cost-state` record, else the
last result. A call on a run no foci turn opened books on a minted `run` turn, recorded as
`autonomous`. Every process writes its baseline (the totals CC restored on --resume,
`resumeBaselineFor`, #2012) and each result as cumulative `backend_reports` for the scope
`<session>@<launch nanos>`, so CC's own cost per process is a difference of two reports.
**Live checks (#2111 P3, §8).** At each quiet point's remainder (and at exit) the windows
just settled join the process total, and `checkDivergence` compares, per model, CC's cost
since launch (`modelUsage.costUSD` less the launch baseline's) with the ledger's price of
everything booked in the process that CC counts: beyond `delegator.CostDivergenceTolerance`
(3%, both sides under $0.01 skipped, a model with an unpriced call skipped) it alarms
`invCostDivergence`, the interrupted calls' price shown beside (CC counts them nowhere). A
remainder's TTL solve makes its own window match CC's cost exactly, so the check bites on
the counted calls' rates and on an unsolved remainder. `checkOverhead` alarms
`invOverheadBounded` when one remainder's overhead (not a compaction) exceeds both $0.50 and
2% of CC's cost for its window. **Turn activity (R8):** `completeTurn` and `finalizeExit`
send `ccLedger.turnEnded`; the book closes a turn's `activity_closed_at` once it has ended,
no subagent tail of it is open and no call the stream named on it is unbooked, else when
the last of those clears (`closeIfIdle`, from `tailClosed`, `mainLine`, `closeWindows`); a
tail reopening on a closed turn (a SendMessage resume) sets it running again
(`Ledger.SetTurnActivity` with zero), and a compaction booked on a closed turn moves its
close forward. A run turn ends at its result, or at exit. A turn end arriving after the
adapter's exit flush is applied directly (`markExited`).
**A turn's completion waits for its calls** (`ccLedger.flush`, from `completeTurn` before
`OnTurnComplete`): until every main-thread call the stream named has its line read (the
main tail is poked to read at once), bounded by `ccBarrierBound`, and at once with no main
tail — so `closeLedgerTurn`'s `FinalCost` holds the turn's own calls. A remainder or a
background subagent's later calls book after it, on the same turns. Replayed against the
#2112 P0 captures (`ccstream/testdata/ledger/*.json.gz`: two idle compactions with the
history re-append, an interrupted resume, graceful/killed exits, a subagent run) —
conservation holds exactly with no negative remainder on every one.

The adapter replaced CC's turn-level path, and everything that path needed is gone: the
`ModelUsage` window subtraction and its per-turn accumulators, the per-message usage
accumulator and its baselines (`ttlsplit.go`), the subagent shares and their #1918 late-spend
corrections (`CostCorrection`, `ApplyLegacyCorrections`, `CorrectionHook`), the TTL
surcharge, the clamp, the per-turn cost breakdown and divergence warning, the #721 output
floor, the reactivation tail's `runStartGate`, the legacy subagent accumulation, and the
#2013 fresh-process guard (`checkFreshProcessUsage`, which misfired on background
subagents). The live per-process divergence alarm replaces the per-turn warning;
`foci-gw ledger-shadow` runs the same check offline.

**Shadow verification** (#2111 §12), kept for the next adapter. With
`logging.api_shadow_db` set, startup opens a SHADOW ledger (`accounting.Options.Shadow`:
its bookings reach no observer — no api.jsonl line, no generation — and its alarms log at
INFO as `shadow <inv>`) and hands it to `accounting.SetShadow`; no backend books there at
present (the next adapter verified this way adds the `Shadow()` getter it books through).
`foci-gw ledger-shadow -live api.db -shadow api-shadow.db [-since 36h]` compares the two
ledgers per turn, per CC process (CC's own cost against the adapter's, interrupted excluded)
and per day. The per-process check (#2122) ends at the latest report whose tokens less the
baseline equal the booked calls of a window prefix (`detail.window`, or a remainder's
`through_window`) — never a `billed_at` cut, which is racy; a process no report matches (a
subagent still running, a remainder not settled, a booking error) is listed as unmatched.
Tokens alone cannot catch a mispriced call, so the report also checks dollars
(`ledger_shadow_checks.go`, #2131) and ends with FLAGS: a compared process whose CC cost
lies beyond the known residual (the median over processes with no unknown-TTL write) from
the adapter's price, or from the range its unknown-TTL `cache_write` tokens span (all 5m ..
all 1h as booked), with the 5m share CC's cost implies; a turn-less remainder row costing
more than its process's largest booked call (not overhead by design: a call source never
read, #2130); and unpriced calls. It also prints the remainder's share per day and process
with its models and classes, and the calls per `class_method` with the unknown-TTL writes
priced both ways.

**tmux Claude Code** (`cctmux/ledger.go`) books each main-thread call its session watcher
reads once the line carries its stop_reason, keyed by the message id, on the open foci turn
(else an autonomous turn of its own). It reads only the main transcript, so its subagents'
and CC's utility calls are not booked, nor is a call interrupted before its stop_reason.

**Legacy rows.** Nothing books a legacy call any more; the only ones are the migrated
history every live ledger holds. accounting's own tests put such rows into a test ledger
with `BookLegacy` (`legacyrow_test.go`, test code since #2145), which writes a row in the
pre-ledger shape (`LegacyRow`) through the migration's own converter (`v1Row` →
class-by-kind → cost basis → `legacyCall`/`legacyTurn`/`legacyReport`). Tests outside the
package seed ordinary new-format calls.

**Readers** (`read.go`). `Ledger.Calls(since)` gives `CallRow`s from `call_costs` plus
each call's per-class counts and costs (`call_class_costs`; a recorded-basis call has no
class costs) and its fill (a legacy parent's is its turn's copy) — what `/cost`, `/cache`
and `/last` render (`command.readCalls`, falling back to `ReadJSONL` with no ledger).
`/cost`'s category table is the views' own split, so it adds up to Total; recorded-basis
cost is its own "Pre-ledger (no split)" line and unpriced calls are named, never summed as
$0. `Ledger.SessionStats` (turns from `turns` without compaction/system ones, calls,
cost, unpriced count, span, context = the latest turn's `turn_costs.context_fill`) serves
`/status`, `/context` and the admin session list; `Ledger.LastTurnID` serves
`POST /score`. Scripts read the views: `scripts/langfuse-etl/etl.py`, and outside the repo
the morning routine and clutch's keepalive-roi / rule-effects tools.

**Migration** (`accounting/migrate.go`, #2111 §6): `Open` detects a pre-ledger api.db
(`api_calls.calculated_cost_usd` exists), takes a `VACUUM INTO` backup, and in ONE
transaction renames the table to `api_calls_v1`, creates the schema, books each v1 row as
a legacy call keeping its id (tokens = the `turn_*` group, else the un-suffixed four),
makes one turn per v1 `turn_id` (else `legacy:<id>`) with its context fill copied and its
source inferred (`/b<n>` session → keepalive, purpose → batch, compaction, summary or
spawn → system), gives an unnamed subagent share actor `(unnamed)`, records in each call's
detail its v1 `call_type` and whether its counts were turn totals, stores CC's
cumulative and opencode's per-call reported costs as reports, verifies by id SET and
per-class token totals, and drops v1 — or rolls back whole. The backend is inferred from
call type and model (v1 never recorded it). Cache-write TTL: solved from CC's reported
cost delta between consecutive same-model rows of a session when that yields an in-range
integer (`solved`), else by record kind — CC main thread 1h, CC subagent 5m, direct API
always 1h (`kind`) — else `cache_write` (`unknown`, codex/opencode). Cost basis (after the
solve): direct-API rows (one call each), `solved` rows (their counts reproduce CC's own
cost delta) and rows with no recorded figure are re-priced from their counts (`counts`);
every other delegated row keeps its recorded figure (`recorded`) — its counts are not
provably the bill (context-fill snapshot before #1854; output from `output_tokens` where
`turn_output_tokens` was wiped before 2026-09-26; cross-model turn totals with an
unobserved TTL split). The
`MigrationReport` gives counts per method, the solve's outcomes, invariants found in
history (counted, not alarmed), and old (`calculated_cost_usd`, and the old
`EffectiveCost`) against new (`call_costs`) totals, overall and per UTC day.

## Tracing (`telemetry/`)

Every turn becomes an OpenTelemetry trace exported over OTLP/HTTP (`[tracing]` in
foci.toml; secrets `langfuse.public_key` / `langfuse.secret_key`). The target is a
self-hosted Langfuse, whose OTel endpoint reads the `langfuse.*` span attributes the
package sets; any OTLP collector accepts the same spans. Off by default; when off,
every entry point is one atomic load and no span is ever allocated.

**Two producers, both already shared by all four backends — no per-backend hooks:**

| Producer | Hooked at | Yields |
|---|---|---|
| `turnevent.Sink` stream | `Agent.HandleMessage` and `Agent.OpenAutonomousTurn` wrap the ctx sink with `telemetry.NewTurnSink` (a `turnSink` that forwards every event and mirrors the ones it cares about) | the trace's **shape**: root `turn` span (type `agent`), a `tool` child per `ToolCall`/`ToolResult`, an `agent` child per `SubagentStart`/`Text`/`End` run, intermediate texts, thinking, retries, error status |
| `accounting.BookedHook` (every booked call) | the ledger's `Update`, after each commit (`Tx.Book`) | exactly one `generation` observation per call — model, `usage_details`, `cost_details.total` = the call's cost as the views price it — parented onto the call's turn (or its subagent span when it has an actor). A compaction, summary or spawn a turn made is booked on that turn and hangs under its root; one no turn made is on a turn of its own (source `compaction`/`system`, `Booking.TurnSource`) that the tracer never opened, so it becomes its own one-observation trace named after its call type. **Cost lives only here**; root/tool/subagent spans carry cost as read-only metadata. So `SUM(observation cost)` per day equals the ledger's `daily_costs` by construction — `scripts/langfuse-etl/etl.py reconcile` is the check. |

**Invariant — the wrapper must never be registered into the router it wraps.** On a
platform turn the ctx sink at `HandleMessage` *is* the session router (RunTurn
registered the real streaming sink and put the router in ctx), so the tracing wrapper
there is `turnSink{inner: router}`. Anything that asks "is this ctx sink the router?"
(the orchestrator's Phase-3.5 register) must compare through `turnevent.Unwrap`, and
`sessionRouter.Register` refuses a sink that `routesTo` itself. Both were added after
the 2026-09-19 stack overflow (#1944): an identity compare registered the wrapper as
the router's current sink and the first `ToolCall` ping-ponged `turnSink.Emit ↔
sessionRouter.Emit` until the goroutine stack was gone. Every decorating sink
(`telemetry.turnSink`, `agent.loggingSink`) implements `turnevent.Unwrapper` for this.

The orchestrator supplies what the sink cannot see: `traceBegin` opens the root the
instant `ts.StartedAt` is set (before any gate can fail, so a rate-limited turn is
still a short errored trace), and `traceInput` — after `InjectNudges` — attaches the
prompt *as sent* (`ts.Prompt` for delegated, the user message for API) and the
resolved model, and registers the API path's system blocks. The delegated path
registers its system prompt where it is actually sent, `DelegatedManager.Get` at
backend launch. Each turn's `Complete` records the prompt's sha256 + length; the
full text goes out once per session per distinct hash as a `system_prompt` event
child (so a character-file edit shows up on the first turn after it).

**Identity is derived, never random** (`ids.go`): trace = sha256 of the api.db
`turn_id` (`<session>@<StartedAt UnixNano>`), root = sha256(role + turn_id), tool =
(turn_id, tool_use id), subagent run = (turn_id, Agent tool_use id[, run]),
generation = (turn_id, call_type, subagent_id, model, ts, …) — subagent_id, NOT
agent_id: #1946 made agent_id the same value for every row of a turn, so keying
on it there would collapse every subagent's generation onto one span id.
Consequences the design
leans on: a background subagent that books its spend 30 min after its parent
closed still lands in the *spawning* turn's trace (the row carries that turn_id);
its generation parents onto the subagent span by the same tool_use id without any
in-memory lookup; and a peer agent can compute the caller's trace id from the turn
id alone. Rows with no turn id (compaction, summariser, spawn) become their own
one-observation traces named after the call type.

**Subagent spans outlive turns** (`subagent.go`): a run's text/end events reach
whichever sink the session router holds *at that moment*, which for a background
Agent is a later turn's. Open runs therefore live in a package registry keyed by
(session, group key, run), and whichever turn sees the end closes it; runs with no
end signal are closed as `end_unobserved` after 2 h.

**Cross-agent links** (`links.go`): Langfuse has no cross-trace edge, so the two
turns of a `send_to_session` exchange are joined by metadata. The tool
(`tools/session_send.go`) stages `LinkPending(target, origin)` as it enqueues the
inject; the target's next injected turn (trigger `session_notify` / `async_notify` /
`ask_grader`) consumes it in `Begin` and records `parent_trace_id`, `parent_turn_id`,
`caller_session`, `caller_agent` plus a `from:<agent>` tag; the caller's tool span
records `target_session`. `relayResponseToCaller` stages the reverse edge so the
caller's `[SESSION RESPONSE]` turn descends from the target turn that answered.
Injects queue in order on the session's inbox, so the pairing holds unless another
injection was already queued ahead; a link older than 10 min expires rather than
mis-attaching.

**Content and redaction.** With `content = true` (default) the prompt, reply,
thinking, tool args/output, subagent prompt/output and system prompt are exported,
each capped at `max_field_bytes`. The gateway holds the secrets store, so
`cmd/foci-gw/tracing_init.go` hands the exporter every secret *value* and
`Redactor` replaces them outright (longest first, ≥ 8 chars) before the generic
credential patterns run — no hashing dance, unlike the backfill ETL which runs
outside the process. `content = false` keeps shape, timing, usage and cost only.

**Export path**: OTel SDK `BatchSpanProcessor` (2 s / 256 spans / 4096 queue) →
`otlptracehttp` with basic auth → `<endpoint>/v1/traces`. Exporter errors go through
`otel.SetErrorHandler` to the `telemetry` component log, debounced to one per minute.
`Shutdown` (deferred in `main.go` right after `initTracing`) flushes for
`flush_timeout`. A turn cut off by shutdown still completes its root via
`HandleMessage`'s deferred `TurnComplete`.

**Attribute vocabulary** (Langfuse's): `langfuse.observation.type|input|output|level|
status_message|model.name|usage_details|cost_details|metadata.<k>`,
`langfuse.trace.name|input|output|tags`, `langfuse.environment`, `user.id` (= the
foci agent), `session.id` (= the foci session key). Trace-level ones are set on the
root; `user.id`/`session.id`/environment on every span.

**Scores and rubrics (`telemetry/score.go`, `internal/evals/`).** A score is
Langfuse's evaluation primitive — a named value on a trace or one observation,
posted over its REST API (`<api base>/scores`; the base is derived from the OTLP
endpoint by stripping `/otel`, so scoring is available exactly when tracing points
at Langfuse). Every score carries a **deterministic id**, sha256 of (trace,
observation, name, source, user): scores are upsertable, so a re-grade overwrites.
The axes are **rubrics** — `<name>.md` files with YAML front matter in `[evals]
rubrics_dir` (default `<home>/shared/evals`), loaded by `evals.Load`, followed by
fsnotify (`Registry.Watch`, 500 ms debounce) and mirrored to Langfuse score configs
on every load that adds/changes one (`cmd/foci-gw/evals_init.go mirrorScoreConfigs`
→ `telemetry.EnsureScoreConfig`, which creates but never modifies — drift is a
logged warning). Three kinds: `human` (graded by a person), `derive` (from trace
metadata), `judge` (an LLM against the body prompt); only `human` is *acted on* in
phase 1–2 — the other two are validated at load so a runner never meets a bad file.
Entry points: `POST /score` (`cmd/foci-gw/http_score.go scoreTurn` — resolves the
turn: explicit → `telemetry.LastTurnID` for this process → `Ledger.LastTurnID`
from api.db; validates against the rubric when one exists; posts with source
`human`), `GET /evals/rubrics`, and the `foci score` / `foci evals list` CLI. The
path is deliberately agent-free: the value is written as validated, nothing
interprets it. Format and phases: `docs/EVALS.md`.

**History**: the shape+cost split exists because the backfill (`scripts/langfuse-etl`,
one generation per api.db row, content joined from conversation.db) came first and
its reconcile is the contract; the Go exporter had to stay row-exact with it. The
ETL's `tail` is retired once this is deployed (it would double-emit); `backfill`
(historical) and the weekly `reconcile` stay.

## Tool System (`tools/`)

Each tool is a `Tool` struct with `Execute func(ctx, params) (ToolResult, error)`. `ToolResult` contains `Text` (the tool's text output) and optional `ExtraBlocks` (additional content blocks like document blocks for PDFs). Registry maps name → tool. See [TOOLS.md](TOOLS.md) for the canonical tool reference. Data-flow summary:

| Tool | File | What it does |
|------|------|-------------|
| `shell` | shell.go | Shell commands via `sh -c`, process group kill on timeout, output redaction. Regular `{{secret:}}` templates are blocked (returns error — use http_request). Bitwarden `{{secret:bw.*}}` templates are allowed (approval-gated via aisudo). |
| `http_request` | http.go | Domain-locked HTTP requests over the shared SSRF-safe client (resolved-IP validation + redirect re-check; see safehttp.go). Secrets in headers/body validated against per-section `allowed_hosts` before sending. `basic_auth` rides the header path as the `Authorization` value and is base64-encoded after resolution; when it held a secret, the encoded token is also redacted from the response, since the stores only know the raw value (#2141). Cross-domain redirects blocked when secrets present. `body_file`/`files[]`/`save_to` paths run through `fileScope` (blocklist + isolated containment). Response redacted. Binary responses (image/*, audio/*, etc.) auto-saved to temp file. `save_to` saves any response to a specific path. `save_from_json_path` extracts a value from JSON response and decodes data: URIs (base64 images from generation APIs). |
| `tmux` | tmux.go | Manage tmux sessions — start (auto-watches by default), send keys, read pane output, list, kill, watch for inactivity, unwatch. Owned sessions persist across app restarts via state store. Autopilot mode (default on): auto-unwatches after inactivity notification, auto-watches on send. |
| `read` | files.go | File contents with line numbers, truncates at 2000 lines |
| `write` | files.go | Create/overwrite files |
| `edit` | files.go | Find-and-replace (old_string must be unique). Syntax validation for .json, .toml, .go, .yaml/.yml, .xml, .py, .sh/.bash: rejects edits that would break a valid file, warns if file was already invalid. |
| `web_fetch` | web.go / server | Fetch web content (server-side default, client-side fallback). Client-side fetches use the shared SSRF-safe client (safehttp.go) — resolved-IP validation blocks metadata/private/loopback in all modes. A bot-shield interstitial (vendor header, or a known marker on a page with almost no visible text — `web_challenge.go`) or an empty result is returned as an ERROR, never as success (#1889); a non-2xx markdown result leads with an HTTP-status note. The thin-extraction (#1960) and list-drop (#2011) warnings measure the extraction against `contentRegions` (`web_baseline.go`): the page minus the regions readability rejects (hidden, unlikely-candidate class/ids such as comment threads, page chrome) unless the extraction draws on them (#2069); the stub fallback (#2065) still compares against the whole page. |
| `web_search` | web.go / server | Web search (server-side default, Brave fallback) |
| `summary` | summary.go | Summarize/extract from large files via a cheap model (API: `cheap` group; delegated: `[tools] summary_model`, else the backend's cheap tier) |
| `memory_search` | memory.go | Full-text search over memory files (+ conversation history for FTS5). Pluggable backends: FTS5 (default) and bleve. Porter stemming, weighted ranking, sort by relevance or recency. Optional `backend` parameter when multiple backends are active. |
| `remind` | remind.go | Defer a thought for later; stored in SQLite, surfaced as injected context when due. `wake=true` actively wakes the session. |
| `scratchpad` | scratchpad.go | Working notes that survive compaction (write/read/clear/list via `action` parameter) |
| `spawn` | spawn.go | Unified sub-call: four context modes. All modes have tool access with a tool-call loop. `raw`: one-shot, no system prompt (`send_to_chat` and `send_to_session` blacklisted — no character context means no communication awareness). `character`: one-shot with character files (all tools except `send_to_session` — an ephemeral one-shot spawn has no session of its own to receive replies, so it must not inject into other sessions; it returns its result to the caller instead). `clone` (default): branch session — a headless self-fork. `explore`: one-shot safe exploration with `ls`, `find`, `grep`, `read`, `memory_search`, `web_search`, `web_fetch` only — no file mutation, no shell exec, no messaging, always haiku. clone creates branch `{parentKey}/b{TIMESTAMP}`, always runs async via `AsyncNotifier` (returns immediate ack, delivers `[SPAWN RESULT]` on completion). Recursive clone blocked via context key. Concurrent clone limited by `max_concurrent_spawns` (default 3). `spawn` itself is excluded from one-shot tool sets to prevent recursion. |
| `ls` | explore.go | List directory contents. Internal to `explore` spawn mode — not registered in the main tool registry. |
| `find` | explore.go | Search for files in a directory hierarchy. Dangerous predicates (`-exec`, `-delete`, etc.) blocked. Internal to `explore` spawn mode. |
| `grep` | explore.go | Search file contents using the best available binary (rg > ack > ag > grep). Flags are validated and translated to the active binary's dialect. Internal to `explore` spawn mode. |
| `send_to_chat` | telegram.go | Send proactive Telegram messages (text, documents, voice notes). With `send_as="voice"` and text (no file_path), synthesizes speech via TTS. Routes to the chat extracted from the session key (`X/c{chatID}`) so per-chat sessions get messages to the correct user. Falls back to bot's default chat when no chat ID in session key. |
| `send_to_session` | session_send.go | Inject a user-role message into another session. Tags the message with `[Message from session ...]` origin header. Appends to session store and triggers processing via `AsyncNotifier`. Used for cross-session communication (e.g. facet branches talking to main). Target accepts a full session key (`scout/c123`, `scout/iresearch`), an agent-qualified session name or chat alias (`scout/research`), or a bare agent name (`scout` → default session) — loose targets resolve through the route.Resolver ladder (create disabled). **A reflection/keepalive caller is barred outright (#1409):** a reflection/consolidation/compaction pass or a keepalive cache-warm turn (`SessionType.IsBarredFromSessionSend`) runs headless with no human waiting and no context its parent/peers don't already have, so `Execute` refuses the call before resolving or delivering anything — a reflection branch once injected an operational status handoff into the main clutch session this way, confusing it; state worth preserving belongs in a handoff file on disk instead. The refusal says so, and the headless branch orientation's `{report_rule}` (`session.reportRule`, rendered from the same `IsBarredFromSessionSend` predicate) tells a barred branch up front that it has no session channel rather than instructing it to use `send_to_session` (#1951). **`reply_to=caller` from a oneshot caller that ISN'T barred is downgraded to fire-and-forget (#1430):** relaying a reply back to a background-task/spawn session (`SessionType.IsOneshot`; reflection/keepalive are excluded here since #1409 already stops them earlier) would wake a fresh, unusable turn whose output leaks into the wrong chat (the #1429 misdelivery root cause), so when the CALLING session is oneshot-but-not-barred the target's reply is delivered to the target's OWN chat (`replyToSession=""`, same as `reply_to=session`) instead of routing back, and no caller-waiting state is set. The caller's type is looked up via `callerSessionTypeFn` (wired in `tool_table.go` to `sessionIndex.Get`); `unknown`/legacy is deliberately NOT oneshot (nor barred). |
| `todo` | todo.go | Per-agent task list (add, list, complete, remove). SQLite backend with priority ordering (high/medium/low). Scoped by `agent_id`. |
| `bitwarden_search` | bitwarden.go | Search Bitwarden vault items by name, URI, folder, username. Returns metadata only (never passwords). Max 5 results. Only registered when `[bitwarden] enabled = true`. |
| `bitwarden_unlock` | bitwarden.go | Unlock a vault item by ID. Calls `sudo -u bitwarden bw get password` via aisudo — blocks until Telegram approval or denial. Caches value for `secret_ttl`. Never returns the actual password. |
| `browser` | browser.go, browser_actions.go, browser_snapshot.go, procexit_linux.go | Browser automation via accessibility tree snapshots. Uses go-rod to control Chrome, captures ARIA snapshot as YAML with numeric refs (`[ref=s1e5]`). Actions: navigate, click, fill, select, press, screenshot, pdf, evaluate, etc. Each mutation auto-captures a fresh snapshot. JS engine vendored from go-rod/rod-mcp (browserjs/). Registered by default; disable with `[tools.browser] enabled = false`. `pathBoth` + `ExecExport` (#1600): delegated backends get it as `foci_browser`. **One browser per session, not per agent:** the table row builds a `SessionPool` (session_pool.go) and `NewSessionBrowserTool` picks the manager by `SessionKeyFromContext` — set by the agent loop on the API path and on the exec bridge's ctx for delegated backends — so the main chat, forks, branches and other chats never share page state. Each call holds an in-flight count; when it drops to zero a `SessionIdleTTL` (30 min) timer arms, and on firing stops and forgets that session's browser. The managers share one `profileLock`, so only one session at a time can open the configured persistent `user_data_dir` (chromium cannot open it twice); a second gets a clear error instead. **`Stop` waits for chromium's process to exit** (#1518) before it removes the owned temp profile dir or releases the `profileLock`: `Start` opens a pidfd on the launched pid (`watchExit`, procexit_linux.go), and `Stop` sends CDP `Browser.close` and waits on it; an unacknowledged close, or no exit within the browser `timeout_sec`, kills chromium and waits again. Off Linux there is no pidfd and `Stop` does not wait. A `Start` that fails after the launch attempt (Launch or CDP `Connect` error) cleans up itself via `abortStart` (#2090) — same wait/kill, then remove the owned dir and release the lock — since `Stop` returns early when no browser connected; it returns an error, never panics. |

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
5. Bridge accepts connections and routes requests to tools with `ExecExport: true`. The accept loop (`acceptLoop`, which gets its listener as an argument) returns only on `net.ErrClosed` (Close), or when the bridge ctx ends during a retry wait. Any other Accept error (EMFILE, ENFILE, ENOBUFS, ENOMEM) logs one WARN per error run and retries on the same listener after a backoff of 5ms doubling to a 1s cap, reset by a good Accept — the `acceptBackoffMin`/`acceptBackoffMax` values from `net/http.Server.Serve`. Before #1122 the loop returned on any error, which left the socket bound with nobody accepting: every `foci-call` then hung with no log line.
6. Bridge is closed after the subprocess exits (cleanup: socket + funcs files removed)

**Skipped for:** explicit `background: true` mode (daemons don't need piping).

**For auto-background:** bridge context uses `context.Background()` + session key so it survives agent turn end.

**Tools with `ExecExport: true`:** `http_request`, `web_fetch`, `web_search`, `memory_search`, `todo`, `send_to_chat`, `spawn`, `tmux`, `browser`.

**`foci-call` binary** (`cmd/foci-call/`): Reads `FOCI_SOCK`, connects to unix socket, sends JSON request (newline-terminated), prints result to stdout or error to stderr, exits 0/1. 1MB scanner buffer for the response envelope.

**Large-result spill handoff:** A tool may return its full result on disk via `ToolResult.ResultFile` (set by the http tool when the response body exceeds the inline preview, and by the shell tool when command output overflows — both via `tools/spill`). In that case the bridge response carries a small `{result, result_file, result_size}` envelope rather than inlining megabytes, and `foci-call` streams the file straight to stdout (same-UID, 0600 file). So `foci_http_request url | jq` pipes the complete body without it passing through the socket as one giant JSON line; the calling agent (CC for delegated, the tool-result guard for API) then applies its own output truncation. Falls back to the inline preview if the file can't be opened.

**Output hints — "is my stdout piped?" (#2048):** every generated function opens with `shellStdoutPipedDetect` (via `shellFuncPrologue`, before the `foci__json` guard): it declares `FOCI_STDOUT_PIPED`/`FOCI_OUTPUT_FORMAT` as fresh `local -x` (so a caller's exported value never leaks in) and sets `FOCI_STDOUT_PIPED=1` iff `/proc/$BASHPID/fd/1` is not `-ef` `/proc/$$/fd/1`. isatty is useless here (an agent's Bash stdout is never a TTY); comparing against the calling shell's fd 1 gives piped for `| head`, `2>&1 | tee` and `$(...)`, and not-piped for a plain call, `( … )` and a main-shell `> file`. No `/proc` → never piped. The `todo` wrapper additionally sets `FOCI_OUTPUT_FORMAT` from `--format jsonl|md` (a shell-only flag, not a schema param). `foci-call` forwards whichever is set as a `"hints"` object on the request envelope (unchanged bytes when neither is); `handleConn` puts it on the tool's context (`WithOutputHints`/`OutputHintsFromContext`). Tools opt in — today only `todo` (list/search/get → JSONL via `todoWantsJSONL`). The API tool path never sets hints, so its output is unchanged.

**`--json` output flag (#1215):** the prologue also carries `shellJSONFlagStrip` (every function except `ask`, whose `--json` is its questions INPUT — `shellJSONInputTools`): it removes each exact `--json` argument, in any position and before the `foci__json` guard, and sets `FOCI_OUTPUT_FORMAT=json`, so the hand-written parsers never see it. A tool that renders its own JSON checks `WantsJSON(ctx)` and returns `JSONResult(v)` (sets `ToolResult.JSON`), documenting the shape in `Tool.JSONOutput` — today `web_search`, `web_fetch`, `memory_search`, `http_request`, `remind`, and `todo` (treats `json` as `jsonl`, `todoMarkJSONL`). For any other result on a `json` call, `handleConn` wraps it via `wrapResultAsJSON` as `{"result","result_file"?,"result_size"?}` — a spilled result's file is named, not streamed, and the envelope's `ResultFile` is cleared (http_request's JSON likewise names `body_file` instead of setting `ResultFile`), since foci-call streaming it would put non-JSON on stdout. `generateHelpText` appends the "Output:" section from `Tool.JSONOutput` (or `genericJSONOutput`). Errors stay stderr + exit 1.

### Tmux Memory Monitor (`tools/tmux_memory.go`)

Background goroutine that checks the RSS of the tmux server process at configurable intervals. Three thresholds (warn, critical, kill) fire Telegram notifications and, at the kill threshold, run `tmux kill-server` and call `ClearAll()` on all tmux tool instances. Notifications use dedup — same threshold level won't re-fire until memory drops below it or tmux is killed.

Wired in `main.go` after agent setup. Notification callback sends to agents whose `inject_agent_warnings` is disabled (agents with injection see warnings via their `warnings.Queue` — proactively dispatched as independent agent turns via `warnings.Dispatcher`). Cleanup callback calls `tmuxClearAll` on each agent instance (stored on `agentInstance` struct).

### System Memory Guard (`resources/memory_guard.go`)

Background goroutine monitoring total RSS of all processes owned by the foci user. Reads `/proc/[pid]/status` directly — no external commands. Two thresholds (warn at 25%, kill at 40% of RAM), both gated by memory pressure (PSI `avg10` from `/proc/pressure/memory` > configurable threshold). Warn pushes to all agents' `WarningQueue` (surfaces via proactive warning dispatch). Kill finds the largest non-foci process by RSS (excludes `os.Getpid()`), sends SIGTERM, waits 5s, SIGKILL if still alive.

Wired in `main.go` after tmux memory monitor. Warning callback iterates `agents` map and pushes to any `inst.ag.Warnings` that's non-nil (agents with `inject_agent_warnings` enabled).

### Backend Expectation Guards (`internal/delegator/expectations.go`, #2013)

Live checks that the backend behaviours foci's accounting assumes still hold, so an auto-update that changes one is reported rather than silently corrupting data (#2012 overstated costs up to 128x for ~36h with nothing firing). Each check compares data the backend already hands foci; a failure calls `ExpectationGuard.Violated`, which logs `BACKEND EXPECTATION VIOLATED: <backend> <version> broke "<invariant>" — <figures>` at **ERROR**. ERROR is the level `log.SetWarnHook` forwards to both `notify.inject_chat_warnings` and `inject_agent_warnings` even when either is set to `"errors"`, so no extra delivery path exists. Rate limit is process-wide (`delegator.Expectations`, since CC gets a new Backend per process) and keyed per backend+invariant: first violation reports at once, repeats are counted into the next report at most hourly, and a violation under a version the last report did not name reports at once. Backends take an optional `expect` field so tests use a private guard.

Versions: `NoteVersion` logs INFO for the first version of a backend a foci process sees and WARN for any version not seen before (`backend version CHANGED: claude-code 2.1.261 -> 2.1.280`). Every violation names the version. CC's comes from `system/init`'s `claude_code_version` (`OnSystem`); codex's from the `initialize` response's `userAgent` (`<client>/<version> (...)`, `noteInitializeVersion`). In memory only, so a change across a foci restart shows up as a first-seen INFO line.

The checks:
- **ccstream, ModelUsage monotonic within a process** (`checkModelUsageMonotonic`, from `OnResult`). A counter going down between two of one process's results is reported: the ledger adapter's remainder is `modelUsage` less the process's baseline and its booked calls, so it is then unreliable. The first result of a process has nothing to compare with (`prevModelUsage` is cleared at `Start`). The #2013 fresh-process check (first result vs the seeded resume baseline) went with the CC ledger switch (#2115): it misfired on background subagents, and a wrong baseline now shows as a negative remainder (`invNegativeRemainder`) or an oversized one (P3's `invOverheadBounded`).
- **ccstream, per-message cache-write TTL split present** (`checkCacheWriteSplit`, from `OnAssistant` for every message, a subagent's included). `cache_creation_input_tokens > 0` with no `cache_creation` means the call's writes are TTL-unknown, priced at 1h.
- **codex, total grows by exactly last** (`totalGrowthMismatch`, via `checkTokenUsage` at the top of `onTokenUsage`). Per-thread history in `threadTotal`. The first notification on a thread, a total that goes down, and an exact repeat are not judged.
- **codex, cached is a subset of input**: `last.totalTokens == input + output` and `cached <= input`. Skipped when `last` is all zero (codex emits zero-`last` context-estimate updates).

### Warning Injection Architecture

Each agent can have two independent warning queues, controlled by `inject_agent_warnings` and `inject_chat_warnings` (both accept `"all"`, `"errors"`, or `"off"`):

- **Agent session queue** (`WarningQueue`): feeds the existing proactive dispatcher which injects warnings as system-initiated turns in the agent's session.
- **Chat notification queue** (`ChatWarningQueue`): feeds a second dispatcher that sends warnings as platform notifications (Telegram messages) directly to the user.

Both queues are independently rate-limited and severity-filtered at push time (`errorsOnly` drops WARN-level entries when the level is `"errors"`). The log hook pushes to all non-nil queues on every agent.

### Tool Result Guard

If a tool result exceeds `agent.MaxResultChars` (from config, default 15,000), the result is written to `agent.ToolResultTempDir` instead of injected directly. Before returning a guard message, the agent makes a side-call to a cheap model to auto-summarise the oversized content, including recent conversation context (configurable via `summary_context_turns` and `summary_context_chars`). The summary model is resolved via `agent.ResolveCallSite(config.CallSummarizeTool, sessionKey)`, which delegates to the `GroupResolver` (see Model Group Resolution below). In multi-model mode this routes to the `cheap` group; in single-model mode it uses the session model. The agent receives the summary plus a reference to the saved file for deeper inspection. If the cheap-model call fails (API error, context cancelled, resolution error), falls back to the original guard message with file path and contextual tool hints (e.g. `jq` for JSON, `mdq` for markdown). This prevents large results from bloating session history while giving the agent useful visibility into the content.

## Slash Commands (`command/`)

Messages starting with `/` are intercepted at the platform router level (Telegram, Discord — via the shared `dispatch` package, see Command Dispatch Architecture above) before reaching the agent. They execute immediately — never queued behind an in-flight agent turn.

**Dispatch flow:** Platform message → auth check → if `/`: `dispatch.Dispatcher.DispatchText()` → `registry.Dispatch()` → execute → reply. Never touches agent session or message history.

**Two types:**
1. **Built-in** (code-defined across `command/*.go`, one constructor per command; `builtins.go` is only a subset — `ping`, `repeat`, `facet`, `tmux`, `agents`): registered in `registerAgentCommands()` (`cmd/foci-gw/commands.go`), that function is the ground truth for the exact current set. As of this audit: `/ping`, `/status`, `/cache`, `/last`, `/cost`, `/context`, `/reset`, `/model`, `/effort`, `/thinking`, `/speed`, `/mode`, `/display`, `/overrides`, `/tools`, `/config`, `/prompts`, `/log`, `/errors`, `/version`, `/help`, `/compact`, `/restart`, `/secrets`, `/bitwarden`, `/sessions`, `/agents`, `/android`, `/pair` (aliases `/pair-key`, `/pairkey`), `/repeat`, `/pass`, `/todo`, `/misc`, `/stop` (+ configurable aliases like `/wait`), `/login`, `/done`, `/facet`, `/branch` (see Wake/Branch section) — plus conditionally-registered ones: `/plan` (iff the backend contributed a plan delivery), `/mana` (alias `/usage`; iff the backend contributed a usage query via `delegator.RegisterUsage` — only `claude-code`/`claude-code-tmux`, both backed by `ccstream.QueryUsage`, #1543), `/tmux` (iff the tmux tool is wired), `/pause`/`/resume`/`/complete` (iff `AskRouter` is set). `/uptime` and `/voice` (as a mode-toggle command) no longer exist — voice config moved to `[[tts]]`/`[[stt]]` arrays with no mode command (`ad8feb13`); `/session` is now plural `/sessions` with `list`/`default`/`info`/`index` subcommands.
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

**Wizards on the native app (out-of-band, `internal/app/wizard.go`):** The FAP path never routes composer text into wizards. Instead, for clients that advertised the `"wizard"` capability, `dispatchCommand` detects a wizard activation (a `Registry.WizardGen(scope)` change across `Dispatch`) and opens a hub-side `wizardSession`, sending the prompt as a structured `wizard.step` frame (suppressing the plain-text render). Answers come back as `wizard.response` frames — `qa:<i>` resolves to the option label via `internal/question`, `qa:cancel` maps to `/cancel`, anything else is passed verbatim — and are fed into `Registry.HandleMessage`; the reply becomes the next `wizard.step` or a terminal `wizard.end` (`done`/`cancelled`/`expired`). Wizards may expose structured steps (buttons) by implementing `command.WizardStepProvider` (`PendingStep() *question.Question`; `agentWizard` is the pilot); others fall back to free-text steps. A `WizardDocProvider` file (the `/android` QR) is staged as a blob and referenced inline from the next step's `media` (in-chat `media` frame fallback when the wizard just ended). Session staleness is guarded by stepId echo (claimed once under `wizardMu`, so a duplicate response for the same step is dropped rather than fed to the next step, #2070) + the generation snapshot (a wizard replaced from chat in the same session expires the app session). App sessions persist alongside the Registry's wizards (`agent_metadata` key `wizard_app_sessions`) and are re-linked at `setupAgent`, so responses keep routing across a server restart. Wire contract: foci-android `docs/01-wire-protocol.md` §12. Uncapable clients keep the legacy plain-message behaviour.

**Device tools on the native app (`app_android`, `internal/app/tool_invoke.go`):** the `app_android` tool (`internal/tools/android.go`, registered in `cmd/foci-gw/tool_table.go`) type-asserts the agent's primary connection to `tools.AppInvoker` → `appConn.InvokeTool` → `Hub.InvokeTool`, which sends a `tool.invoke` frame and waits on the `toolCalls` registry for the matching `tool.result` (a `pending` result is a keepalive, not a terminal). The target socket is chosen by `Hub.clientForTool` from EVERY live socket (`snapshotClients`), not from the agent's bindings: a device tool belongs to the device. A socket qualifies via `wsClient.canRunTool` — it advertised `"tool:<name>"` in its hello features, or (pre-capability clients) it is the `"android"` tool and the hello's `ClientInfo.OS` starts with `Android`. Among qualifying sockets the newest hello (`wsClient.helloAt`) wins. None qualifying → `ErrNoLiveDevice`. Before #1079 the pick was map order over the agent's bindings, so a connected desktop answered about half the Tasker calls with "not supported on desktop".

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

**Wire layer (`internal/fap/`):** pure Go mirror of the client's Kotlin
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

**Handshake observability (#1713).** A socket is only useful once its `hello`
arrives; everything the app receives follows from it. Three signals record that,
because until 2026-08-15 nothing did: a `hello: device=… app=… os=… version=… resume=N
features=… push_token=<bool>` DEBUG line on receipt (push token presence ONLY —
the value is a credential; app/os/version come straight off `fap.ClientInfo`,
which the wire has always carried, and are how the server can tell WHICH app
build is talking to it — #1782), `wsClient.helloSeen`, checked by
`noteSocketClosed` when the socket closes, and the **`device connected:
device=… ip=…`** INFO line in `ServeWS`. The ip is resolved once by
`clientIPForLog` and cached on `wsClient.ip`, so the disconnect line and the
outbound-stall WARN can name it too without recomputing (#1782).
That last one exists because the other two cannot identify a socket that dies
BEFORE its hello: `deviceID` normally arrives in the hello frame, which such a
socket never sends, so a burst of hello-less connects was unattributable from
the server and the diagnosis had to move to client-side logging. Device-token
auth resolves the device before the upgrade, so `ServeWS` can name it
unconditionally (`authenticate` returns ok only with a device — there is no
master key since #862). A run of `helloLessWarnAt` (5) consecutive hello-less sockets logs
a WARN — **but only if no handshake has succeeded within `helloQuietPeriod`**
(5 min). Both conditions are needed: a hello-less socket is usually the client
abandoning a superseded connect (`FapClient.closeSocket` cancels the in-flight
handshake, and an incomplete upgrade cannot send a close frame, so it reaches us
as a sub-second 1006), which is ordinary churn when several reconnect triggers
fire at once. Count alone would cry wolf; count *plus* silence is what actually
means the user is getting nothing.

All of that state is **per device** (`Hub.helloByDevice`, keyed by the
AUTHENTICATED `deviceID` `ServeWS` set at connect — `dispatch.go` captures it
before the hello's advisory value overwrites it, since a socket dying before its
hello only ever has the former). A fleet-wide counter lied in both directions on
2026-08-17: a healthy phone's handshakes kept resetting the global run while the
Mac was dead throughout, and the line named no device, so it read as a fleet
outage when half the fleet was fine.

`helloLessWarnAt` is the **entry** condition, not the repeat interval. Repeats
come from `Hub.helloWarns`, a `log.WarnLimiter` keyed by device: immediate, then
`helloWarnBase` (1 min) doubling to `helloWarnMax` (30 min), with the count of
suppressed lines carried on the next one that does emit. Warning every 5th
failed connect made the volume scale with the client's retry rate — the
2026-08-17 outage produced 31 WARN lines in 3 hours, 82% of every warning in the
log. **Nothing about reconnection is throttled:** `noteSocketClosed` runs after
the socket is already dead, so only the announcing is rationed. `noteHelloSeen`
then logs a **`app handshakes RECOVERED for device=…`** WARN (same severity as
the alarm, so a WARN-level reader never sees an outage without its all-clear)
and calls `Reset`, which re-arms the immediate warning — an intermittent fault
therefore stays loud instead of inheriting the silence its previous episode
earned.

**The connect/disconnect lines also carry the client IP** (`clientIPForLog`,
`devices.go`), which is deliberately NOT `remoteIP`. `remoteIP` answers a
security question — which bucket to rate-limit against — and correctly trusts
only the RIGHTMOST `X-Forwarded-For` hop, which behind the Cloudflare tunnel is
the proxy, not the user. `clientIPForLog` prefers `CF-Connecting-IP` (set by
Cloudflare, unforgeable while the origin is reachable only through the tunnel)
and falls back to `remoteIP`, so a tailnet client shows its `100.x` address.
It must never feed a security decision: logging a spoofed IP is harmless,
rate-limiting on one is not. The field exists because deviceID answers "who"
but not "from where", and a client that BROKE versus one that merely MOVED
NETWORK is a different diagnosis with a different owner — on 2026-08-17/18 a
phone moved Wi-Fi→5G unnoticed and the behaviour change was blamed on a
Cloudflare config edit, which was reverted on that false premise (#1728).

**Every authenticated app endpoint names its device.** `Hub.authenticate`
resolves the device from the bearer token, and the HTTP handlers used to
discard it (`if _, ok := h.authenticate(w, r); !ok`). They now log it:
`replay GET: device=…`, `history GET: device=…`, `blob GET: device=…`,
`devices list: device=…`, and `revoked device "X" (by device=Y)` — the revoke
line previously recorded only its target, never the actor. `authBlob` returns
`(*device, bool)` to match. This is the HTTP half of the same lesson as the
`device connected:` line: on 2026-08-17, 1370 anonymous `GET /app/replay` calls
proved SOME client's HTTPS was healthy while its WebSockets died, but not
which one — so "the network path is broken" and "only WebSockets are broken"
could not be told apart from the server. `deviceID` is an identifier; the
`Token` is the credential and is never logged (asserted in
`request_identity_test.go`, whose leak arm is verified by injecting it).

This exists because a ~3-hour near-total app outage produced **zero log
lines above DEBUG**: the 1006 disconnects are DEBUG by design (#888, mobile 1006s
are normal) and `replayTo` is DEBUG, so hundreds of connects that delivered
nothing were invisible. `resume=N` is also the sole discriminator between "socket
died before `hello`" and "`hello` arrived asking to resume nothing" — both
produce no `replayTo`, and conflating them is what stalled the diagnosis.

**Inbound (`dispatch.go`):** decode → **reliability gate** (`inboundConvID` →
`convForReliability`: dedup by `(conversationId, envelope id)`, drop resent
outbox entries, fold piggybacked `ack` to trim the replay buffer) → switch.
`hello`→server hello (roster) + `resumeConversations` (attach EVERY live binding —
not just the resumed ones, see below — but replay `seq > ack` only for the
**non-archived** conversations the hello actually named. **The hello's resume list is
BOUNDED (#1737): the app sends its open tabs plus the most recent, 6 in total, not one entry
per conversation.** At ~65 bytes an entry a 163-conversation device sent an ~11KB
hello, and on a 1492-MTU path with broken PMTUD (PPPoE, VPNs, mobile carriers)
anything past ~1452 bytes of TCP payload is silently black-holed: the WS upgrade
SUCCEEDS, `device connected` logs, then the socket dies before the hello with no
close frame and no error (275 consecutive failures; `resume=167` failing while
`resume=0` on the same server in the same hour connected fine). The asymmetry is
real — large DOWNSTREAM is fine, `GET /app/replay` returns pages of up to 10000
frames over the same path — so the budget is client→server only. Because the list is
bounded it can no longer decide who gets live frames, so **attach and replay are now
fully separated**: `attachUnresumed` attaches the socket to every binding it isn't
already on, seeded at that binding's current high-water, and replay is the only half
that consumes a resume point. Attaching all is not a widening — `agentRoster`
advertises the same `h.convs` to the same socket in the same hello. A conversation
omitted from the hello therefore still gets live fan-out (so it still badges as
unread), still gets its `lastSeq`/`lastPreview`/`lastActivityTs` from that roster and
its watermark from `pushReads`, and backfills its missed tail over `GET /app/replay`
when `applyRoster` sees the roster's `lastSeq` ahead of the local row (#1834). The
per-conv `features` persistence in `dispatch.go` is driven off the ATTACH set
(`wsClient.attachedBindings`) for the same reason — keyed off the capped resume list
it would leave most conversations resolving caps-less after a restart. The resume
point also carried an `open` flag until #1737; nothing read it (#1742) and it is gone
from the wire (an older client still sending it decodes fine — `encoding/json` ignores
unknown keys).

The archived split predates the cap and still applies to whatever the hello does
name: a hello named every conversation the device had ever seen, and one device sent
169 points against 11 live conversations. Replaying an archived
conversation pushes frames the roster hides, and each `replayTo` can enqueue up to
`maxResumeStoreReplay` 2000 frames into a `sendBuffer` 256-slot queue, so the burst
overflows, `enqueue` closes the socket "to force resume", and the reconnect replays
the identical backlog — #1779. Attach is NOT skipped, so a live frame on an archived
conversation still arrives; only history is withheld, and `GET /app/replay` can still
pull it. The archived set is memoised per agent for the resume, as in `pushCommandsTo`.
Replay uses `enqueueReplay`, NOT `enqueue`, and the difference is the point: `enqueue`
must never drop a LIVE frame (the client acks past it and never re-asks, so a drop is an
unrecoverable hole) and pays for that by blocking then closing the socket. A replay frame
is durable and the client already fetches it — `FociRepository` backfills the active
conversation on every reconnect and the rest on open — so `enqueueReplay` is non-blocking
and simply STOPS on a full queue. Stopping must be clean: a contiguous short prefix is what
the pull path finishes, a mid-stream hole is what the no-drop rule exists to prevent. A
truncated conversation does not abort the resume, since each conversation has its own seq
stream. The bound is therefore the socket queue itself, self-tuning per client, rather than
`maxResumeStoreReplay` — which bounds nothing once a hello carries many conversations,
because each one spends the cap again into the SAME queue); `conversation.open`→bind socket to agent;
`message`→`routeUserTurn`→`ensureBinding`→`agent.Enqueue(Envelope{Driver:
appConn})`; `conversation.open`→`handleConversationOpen` (adopts a client-assigned
`conversationId` when the frame carries one — so the app creates + opens the
conversation locally and instantly — else mints one; binds it, optionally adopts
a named `sessionKey`, **broadcasts** an updated roster the app upserts; idempotent
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
binding's seq; a miss → `resolveDeadPrompt`: late answer + `interactive.remove`, #2080); `conversation.openSet`→`handleConversationOpenSet` (records the
socket's open-set for keepalive, persists + mirrors it, AND attaches the socket to
each open conversation it has a binding for but isn't yet attached to — a 4th
attach trigger alongside `resumeConversations`/`conversation.open`/an active send —
so a conversation learned AFTER hello, mirrored via `conversation.openSync` and
backfilled over HTTP, still gets live fan-out without waiting for the next resume;
ack seeded to the current high-water, idempotent for an already-attached reader);
`ping`→`pong`; unknown→ignored, logged once per (socket, type) (#1884). No agent → `error` frame.

**Platform lifecycle callbacks:** `SetLifecycleCallback` stores the gateway's
`OnUserMessage`/`OnTurnComplete`/`OnTurnEnd` hooks on the per-agent `appConn`
(`PrimaryBot`), mirroring telegram's `Bot` fields. `OnUserMessage` fires from
`routeUserTurn` (right before `agent.Enqueue`) and `routeCommand` — it feeds
the periodic runner's in-process receipt stamp, which `Runner.LastUserActivity`
folds into the persisted `last_user_activity_at` (see Reflection & Consolidation
Timers); for `routeCommand` it is the only activity signal, since a command
starts no turn. `OnTurnComplete`/`OnTurnEnd` fire from `appConn.WrapTurn` (complete after
the turn body returns, end deferred last) — same shape as `telegram.Bot.WrapTurn`.

**Outbound — `appConn` (`conn.go`)** implements `platform.Connection`,
`platform.ButtonSender`, and `agent.Driver`. `SendToSession`/`SendText`→`message`;
`SetTyping`→`typing`; `SendNotification`→`notification`. `SendTextWithButtons`→
`interactive` (foci pre-encodes each button's Data as `<promptId>:<index>`, so the
app echoes it back for routing); `EditMessageText`/`EditMessageWithButtons`→
`interactive.edit` (addressed via the hub `prompts` map); `RemoveInteractive`
(`platform.InteractiveRemover`)→`interactive.remove`, used wherever a prompt dies
unanswered (#2080). The `Send{Photo,Document,
Voice,…}` media methods store the payload in the `blobStore` and emit a `media`
frame referencing the blobId. All sends go through `convBinding.send`, which
assigns seq + ack, buffers for replay, and enqueues iff a socket is attached.

**Session-blind sends (`Hub.deliverBinding`):** any send without a live
binding — the unbound conn's `SendText`/`SendNotification` (broadcast
warnings, `--broadcast` responses) or a `SendToSession` whose session has no
binding — resolves through one ladder: the pinned default conversation
(resurrected from its persisted `conv_id` row when not live — see conversation
durability below); else whatever conversation is most recently active at send
time; else a **server-minted conversation**, with an immediate roster broadcast to
live sockets (offline devices learn it on next hello; NB the Android client
must upsert roster conversations it has never seen — it does). `deliverBinding` returns
the rung that fired (`default`/`most-recent`/`server-created`) and the send
path logs it, so misdelivery is diagnosable from the log. The default pin is
user-owned (`conversation.setDefault`) and never set automatically. The app can
create conversations freely, so a session-blind send never has "nowhere to
deliver". There is deliberately no fan-out to every binding: one send, one
destination.

**Roster push: per-socket vs broadcast (`pushRoster` / `pushRosterAll`, #1558).** Two functions, and the split is load-bearing. `pushRoster(client)` sends the `hello` roster to ONE socket and is for read-only or *rejected* round-trips: the `hello` handshake, `conversation.list`, and the refused-archive revert (nothing changed server-side, and only the requesting device applied the optimistic flag). `pushRosterAll()` broadcasts to every socket in `h.clients` and is the ack half of every roster-**mutating** round-trip, whichever side originated it: server-side `mintFacetConversation` / `deliverBinding` / `NotifyAliasChanged` (an alias set by the agent's `set_session_alias` tool or a backend thread name via `Agent.SetOnAliasChanged`, #2157: the roster is the only frame carrying a title, so before this the app kept the old name until it reconnected), and client-side `conversation.open` / `rename` / `setDefault` / `archive`. **Any new roster mutation must use `pushRosterAll`** — a change only the originating socket hears is invisible on the user's other devices until they reconnect, which is exactly the bug #1558 fixed (a chat created on desktop never appeared on Android). `conversation.openSync` is not a substitute: it carries bare IDs with no title/agent/sessionKey, so a peer that mirrors the open-set still cannot render a conversation it has no roster entry for. The originating socket needs no separate ack — `serveWS` calls `addClient` **before** `readPump`, and `readPump` is `dispatchInbound`'s only caller, so any socket able to send a frame is already in `h.clients`. Palette pushes are NOT symmetric with this: `pushRoster(client)` calls `pushCommandsTo(client)`, which visits only the conversations that socket is attached to, while `pushRosterAll` keeps the global `pushCommandsAll`. Serving one socket is not a reason to rebuild every other conversation's palette — the global call used to sit on both paths, so a single device's wifi reconnect fanned a ~3.4KB `commands` frame across every live conversation. The **second entry point** is `routeUserTurn`: a `message` frame can name a conversationId the server has never seen (the app's `ConversationOpen` is fire-and-forget and is dropped outright if the socket was down when the chat was created), and `ensureBinding` pushes no roster of its own — so `routeUserTurn` broadcasts when its `!existed` branch fires. The broadcast sits at that call site rather than inside `ensureBinding`, because `StartAll` calls `ensureBinding` in a loop to rebuild every binding at startup and would otherwise emit one `hello` per conversation on every restart. No client change was needed: `InboundFrameDispatcher.applyRoster` has no handshake-only guard, applies every `hello`, and upserts conversations it has never seen into a reactive Room flow.

**Cross-device mirrors: one broadcast shape, one replay shape (`hub.go`).** Five user-level values sync between a user's devices — the read watermark, the composer draft, the shared open-set, (since #1882) a conversation's pinned-message set, and (since #2144) where the user left each conversation's scroll view. Every one of them is a pair: a live fan-out to the OTHER sockets when one device changes it, plus a post-`hello` replay so a device that was offline for the change catches up. **Both halves are required.** A mirror with only the fan-out looks correct in a two-device live test and still fails the case that usually gets reported — the device that was not connected at the time.

Each half has exactly one implementation, and a new mirror must use them rather than copy them:
- **Fan-out — `broadcastExcept(sender, frame)`**, over `snapshotClients()` (which copies `h.clients` under the read lock so the sends happen OUTSIDE it). The frame is built once by the caller, not per recipient. `sender` is skipped because the originating device already applied the change locally; pass `nil` for a server-originated change (`mintFacetConversation`) so every device hears it. There is deliberately no per-recipient filter: all five frames are idempotent/last-write-wins, so sending one to a client that does not currently hold the conversation is harmless and adding a filter would be a behaviour change. (Before #1882 this was three byte-identical `broadcast*Except` functions; `broadcastSettings` still differs, because it filters on the `settingsSync` capability rather than on the sender.)
- **Replay — `pushChatScalar(client, metaKey, replayEmpty, frame)`**, called from the `hello` branch of `dispatchInbound` alongside `pushRoster`/`pushSettings`/`pushOpenSet`. It walks every live binding and re-sends that chat's stored `chat_metadata` value. `replayEmpty` decides whether an empty STORED STRING is replayed: `draft` sets it true (a cleared draft must reach a device that missed the clear), `last_read` false (an empty watermark carries nothing), `scroll` false (a never-written position has nothing to say; the frame builder may also return nil to skip an unreadable stored value), `pins` false — an empty pins string means "never written", and replaying that as "nothing is pinned" would wipe the local-only message pins (#893) every device already holds. A pin set that is genuinely empty stores as `"[]"`, which is a non-empty string and replays either way.

**Message pin (`pin.put`/`pin.sync`, #1882).** Stored as a JSON array under the `pins` `chat_metadata` key (sorted + de-duplicated by `pins.go`, so an unchanged set round-trips to an identical string). `pin.put` is a per-message delta from the app; `pin.sync` carries the conversation's WHOLE resulting set, so the same frame serves both the fan-out and the replay and the receiver never needs a base it might have missed. `handlePin` and `handleRead` are the read-modify-writes among the four mirrors. `handlePin` is serialised on `h.pinsMu` — two devices pinning different messages in one chat at the same instant would otherwise both compute from the same stale set and lose one. This is MESSAGE pin only; the roster's CONVERSATION pin remains a deliberate per-device preference with no wire representation.

**Scroll position (`scroll.put`/`scroll.sync`, #2144).** Where the user LEFT a conversation, sent by the app on the same leave event as `draft.put`. Device-portable by construction: `messageId` names the message at the top of the viewport and `following` says they left tailing the newest (then `messageId` is dropped) — no list index or pixel offset, which mean nothing on a device with a different width or collapse state. `handleScroll` (`scroll_sync.go`) is the draft handler's shape: normalise, store JSON under the `scroll` `chat_metadata` key, `broadcastExcept`; a not-following put without a message is refused so it cannot wipe a stored position. Server-side it is last-write-wins by arrival, like drafts; the conflict rule (never move a chat the user is looking at; on leaving it, adopt a remote position only if they did not scroll themselves) lives in the client's `ScrollSyncGate`.

**Read watermark (`read`/`read.sync`, #1204).** `handleRead` is serialised on `h.readMu` because the stored `last_read` only moves forward: `readWatermarkAdvances` compares the incoming id with the stored one (message ids are ULIDs, so a string compare orders them by mint time; a non-ULID id can't be ordered and is accepted, the old behaviour). A stale device's older Read is neither stored nor fanned out — the stored value is what `pushReads` replays at every `hello`.

**Conversation durability (`conv_id` rows):** the app's numeric chatID is a one-way FNV-64a hash of the conversationId (`chatIDForConv`), so `ensureBinding` persists the preimage as a `conv_id` row in `chat_metadata` (agent+`app`+chatID → conversationId) at binding creation. This row is the conversation's durable identity, independent of its frames: a conversation created (and maybe pinned as default) but never used has no frames, yet survives restarts and frame-TTL expiry. It is also how `defaultChatBinding` reverses the hash to resurrect a pinned default that isn't live. Rows are never deleted (archive is a flag; the frame janitor only trims frames), so the pin can't dangle. Pins recorded before `conv_id` persistence existed are unresolvable when not live; delivery logs the fact and falls back to most-recent.

**Binding restore across restart + archive (`framestore.go`, `StartAll`, `handleConversationArchive`):** bindings (`h.convs`/`h.bySession`) are in-memory, created on client frames (or server-side by `deliverBinding`) — so a foci restart empties them. To keep unsolicited sends landing in the SAME conversations rather than freshly minted ones, `Hub.StartAll` rebuilds bindings at startup from the union of two durable sources: `frameStore.RestorableConvs()` — every conv with a **visible** frame and a known `agent_id` (a column added to `app_frames`; written by `convBinding.send`) — plus `SessionIndex.ConvRefs("app")`, the persisted `conv_id` rows covering registered-but-frameless conversations. `ensureBinding(nil, agentID, convID)` recreates each socketless binding (`attach(nil)` is a no-op; seq seeded from `MaxSeq`). **Archive is a reversible flag, not a deletion:** the `conversation.archive` frame carries an `Archived` bool; `handleConversationArchive` persists only an `is_archived` row in `chat_metadata` (keyed by agent+platform+chatID, a sibling of `is_default`) — it does NOT purge frames, drop the binding, flip session status, or fire a reflection. The binding stays live (inbound frames still flow; history retained), and the roster surfaces `ConversationInfo.Archived` (read from `SessionIndex.ArchivedChatsForAgent` by `agentRoster`). Archived convs are therefore still restored on restart. Unarchive is a real server action (`Archived=false` clears the flag); the updated roster is broadcast to every live socket on each archive/unarchive so all devices reconcile at once (#1558). **Archiving the agent's default chat is refused** with an `archive_default` `ErrorFrame` plus a per-socket roster re-push (reverting only the requesting client's optimistic flag): an archivable default would silently degrade session-blind delivery.

**Media / blobs (`blob.go`, slice 4):** binary payloads never cross the
WebSocket. `blobStore` keeps blobs on disk under `<data_dir>/app-blobs`
(`blobDir`; metadata in memory; size-capped + TTL-reaped). It lived under the
temp root until #1556 — a host reboot wiped /tmp and with it every blob, while
the blob's path (an inbound attachment's `SavedPath`) stays quoted in agent
history; `MigrateLegacyBlobDir` moves the old dir's blobs across once at
gateway startup, before `tempdir.CleanStale`. With no data dir (tests only)
`blobDir` falls back to the temp root. `newBlobStore` rehydrates that
in-memory metadata from the directory itself at construction (`rehydrate`,
#1500): each filename is decoded as a ULID for its `created` time (`fap.ULIDTime`,
mirroring `fap.NewULID`'s encoding), mime is sniffed from a 512-byte content
prefix (never persisted, so unrecoverable any other way), and anything already
past TTL is reaped immediately instead of resurrected — this is what makes
app-blobs/ self-managing across a restart. Outbound: a `Send*` media call
→ `putFile`/`putBytes` → `media {blobId,mime,…}` (no `kind` — app clients derive
presentation from `mime`; kind is an internal blob/Telegram-method label only); the app fetches bytes via
`GET /app/blob/<id>` (`ServeBlobGet`, range-capable `http.ServeContent`).
Inbound: the app uploads via `POST /app/blob` (`ServeBlobPost`, returns
`{blobId,size,mime}`), then references the blobId in `message.attachments`;
`resolveAttachments` reads each blob back into a `platform.Attachment`
(small ones into `Data`, `SavedPath` always set). The user-role echo
(`routeUserTurn`) carries the same refs back as `message.attachments`
(`echoAttachmentRefs`: every ref in send order, blank mime/name filled from the
blob) and is sent for a file with no caption, so the user's other devices show
each file as a placeholder and fetch it on tap (#2161). Blobs are not
device-scoped: any paired device can GET one until its TTL. Both endpoints share the
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
`commandInfos(conn)` via `command.Registry.VisibleList` with no session in
context: the commands the agent as a whole can run (backend, default model).
Every command listing (/help, this roster, the per-conversation `Commands` frame,
the Telegram menu) filters through one predicate, `command.Available` (not
`Hidden`, `Requires` met, `Visible` gate true), so none offers a command that
would only answer "not available" (#898). Per-conversation palettes (`pushCommands`) are **sent only on
change** — `convBinding.lastCmdHash` fingerprints the last one sent, the same
send-on-change discipline `activitySent` applies to Activity
frames — and are **skipped entirely for archived conversations**, which are
hidden from the roster and so could never render one. Both matter because the
frame is durably stored as well as sent: before this, palettes were 256,699
frames totalling 847MB, **87% of every byte foci had ever sent to the app**,
~91% of them byte-identical, driven by ~361 device reconnects a day across ~152
conversations. `lastCmdHash` is deliberately NOT rehydrated on restart (see
`TestConvBindingFieldCensus`): a client reconnecting to a fresh process should
be told the palette, not have it suppressed by a surviving hash. The Telegram `setMyCommands` menu
(`bot_poll.go:RegisterCommands`) is the same agent-level list
(`AvailableList`, which keeps `ExcludeApp` commands), except on a pooled facet
bot not yet wired to an agent, which keeps the full non-hidden menu. The server
is authoritative for the descriptions, so the app renders what it receives
rather than hardcoding its own copies. Session-dependent gating (the session's
model, a pending ask) is evaluated only in the per-conversation `Commands`
frame; the roster list is the app's fallback until that frame arrives. The list rides the existing `hello`/`conversation.open` roster
frames, so it refreshes whenever the roster does; no dedicated frame type.

**Auth hardening (`devices.go`, slice 7):** there is **no shared master key** —
`app.api_key` was removed in #862, so no long-lived shared secret exists to leak.
The bootstrap is instead a single-use, short-TTL **pairing key** held only in
memory (`pairKeyStore`, never written to disk; a restart clears an unused one and
you re-mint). A device exchanges it once at `POST /app/pair` for a revocable
per-device token (`deviceStore`, 256-bit random, persisted to
`<DataDir>/app-devices.json` so pairings survive deploys). Thereafter `/app/ws`
and `/app/blob` accept **only** a device token (`Hub.authenticate` → `authToken`),
which is why `authenticate` returning ok implies a non-nil device and `ServeWS`
can seed `client.deviceID` and name the device unconditionally. `POST
/app/pair/revoke` drops the token and force-closes the device's live socket(s)
with `4403`; `GET /app/devices` lists pairings (tokens omitted). An `authLimiter` locks
out a remote IP (`remoteIP`, X-Forwarded-For-aware) after repeated auth failures
— the endpoint is internet-facing.

**Push (`push.go`, slice 5):** offline wake via FCM v1 data-messages. `fcmPusher`
authenticates with a service-account token source (`golang.org/x/oauth2/google`,
auto-refreshing). `newFCMPusherForApp` resolves the credential in priority
order: (1) decomposed `app.fcm_project_id`/`app.fcm_client_email`/
`app.fcm_private_key` secret fields (plus optional `app.fcm_private_key_id`/
`app.fcm_token_uri`) — the preferred path (#967), reconstructing the minimal
service-account JSON in memory so the credential content never touches
`foci.toml` or disk as a file; (2) `[platforms.app].fcm_credentials`, a
service-account JSON file path in `foci.toml`; (3) the legacy
`app.fcm_credentials` secret holding a file path (pre-#967 compat). Absent
everywhere, or `push=false` → push disabled gracefully. `private_key` values are
newline-normalized on read (`normalizeFCMPrivateKey`) since a PEM key pasted
into a single-line `secrets.toml` string commonly carries literal `\n`
two-char escapes instead of real newlines. The client
registers its FCM token in `ClientHello` (or out-of-band via
`POST /app/push/register` after an OS token rotation)
→ `pushTokens` (in-memory deviceId→token, repopulated each connect). When
`convBinding.send` runs with no attached socket, it buffers the frame and — for
user-visible frames only (`pushPreview` classifies; control/streaming frames are
skipped) — fires `notifyOffline` → `pusher.notify`, which coalesces (≤1 push per
conversation PER DEVICE per `push_coalesce` window, default 15s; keyed
`pushKey{convID, deviceID}` so one device's wake never swallows another's, and
the `hello` branch calls `pusher.deviceConnected` to clear that device's windows
because its replay consumed the wake, #1204) and sends a hint
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

**Fallback chain (`voice.FallbackTTS`, #1808).** `resolveTTS` does not return one provider — it walks `[[tts]].fallback` from the requested id and returns a `voice.FallbackTTS` over every link it reaches (bare provider when the chain is one long, so a lone entry is unchanged). Each link is decorated with its OWN entry rate and replacements, combined with the caller's agent-level ones. `initVoice` auto-registers a key-less `edge-tts` provider under the reserved id `edge-tts` (`builtinFallbackTTSID`) whenever at least one `[[tts]]` entry loaded, and that id is the default `fallback` for every entry that is not itself edge-tts — so a cloud provider hitting its quota degrades to a local voice instead of to silent text. Declaring a `[[tts]]` entry with that id replaces the built-in; `fallback = ""` opts an entry out (hence the `*string`). Cycles are broken by a visited set, depth by `maxTTSChain`.

`FallbackTTS` deliberately contains **no retry, backoff or wait**: a spoken reply is live, so a provider that says "try again in 4h" is skipped, never waited out — waiting spends quota on audio nobody is still listening for. The only response to a failure is the next provider, or text-only. Its error joins every link's failure with `errors.Join`, keeping a `*ratelimit.Error` reachable via `errors.As` so `appSink.ttsFailed` still classifies quota exhaustion at info rather than warn (and reports it to the client as "rate limited", #1809).

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

**Background connect (#2043)** — also ONE mechanism for both platforms, in `BotManager`: `ConnectBeforeRun(bot, label, connect)` marks a registered bot PENDING. A pending bot is wired but invisible to every lookup that hands out a connection (`PrimaryBot`, `BotForSession`, pool `Acquire`/`Available`, `HasFacet`, `AgentIDs`); setup-time wiring that must reach it anyway uses `RegisteredPrimary` (lifecycle callbacks, display defaults, restore's persisted chat ID). `StartAll` runs `connect` in the bot's goroutine, then `markLive`, then `Run`. Shutdown (ctx cancel) stops a pending retry loop at the next attempt boundary (`netretry.Do` re-checks ctx before every attempt) and `Wait` does not wait for an attempt already in flight (a getMe or gateway dial that only ends on its own network timeout) — it is abandoned and its bot never runs. `WhenPrimaryLive` (surfaced as `ConnectionManagerAdapter.WhenPrimaryConnected` / `Messaging.WhenPrimaryConnected`) runs a callback exactly once when the primary is live. **Delivery to a not-yet-connected platform** is decided as "offline", not queued: `route.ConnFor` sees no connection for it, exactly as for a platform whose connection is down (#990/#1493) — a chat owned by that platform gets `DeliveryNone` (the turn still runs and lands in the JSONL); an unclaimed notice falls to another live platform. Queueing was rejected: it is a second delivery path only for the startup window, and a backlog flushed after a long outage would be stale.

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
- `POST /branch` — branch from default session (activity-gated, supports `no_compact`/`no_reset_hook`). Returns 412 if no default session. (Renamed from `/wake` 2026-07-10, `fc44c981` — "it is the branch mechanism"; the CLI subcommand is `foci branch`.)
- `POST /webhook/{agent}/{hookid}` — trigger agent turn from external events. `{hookid}` must be declared in the agent's `webhooks` config map (global `[system]` merged with per-agent `[[agents]].system`). The mapped prompt path is resolved via `prompts.ResolvePrompt()` (agent workspace/prompts → shared workspace/prompts). Reads request body as payload (max 1 MB), combines prompt + payload under a `## Webhook Payload` heading, and sends to the agent's default session. Async (202) by default; `?sync=true` for synchronous response. Supports four activity gate query params — `?if_active` / `?if_inactive` (session-level, with in-flight short-circuit) and `?if_user_active` / `?if_user_inactive` (user-attention only); see [SPEC.md](SPEC.md) Activity gating. Returns 404 if hookid not in config or prompt file not found, 412 if no default session.
- `GET /voice` — WebSocket upgrade for real-time voice conversation. Enabled when `[http] ws_enabled = true`.
- `POST /-/reload-credentials` — hot-reload API credentials from `secrets.toml`. Called by `foci auth` after saving a new token. Only registered when using static token auth (setup-token or API key), not OAuth fallback.
- `POST /askgw/ask`, `GET /askgw/ask/{id}`, `POST /askgw/ask/{id}/cancel`, `POST /askgw/notify` — HTTP transport onto the ask-gateway (see Ask Gateway section below). Registered only when `[askgw] enabled = true` **and** `[askgw] http_enabled = true` (separate opt-in — this endpoint is gated by `http.api_key` alone, a materially weaker gate than the Unix socket's UID allow-list).

## Ask Gateway (`internal/askgw/`, `cmd/foci-gw/askgw_setup.go`)

**Opt-in** (`[askgw] enabled = true`). A local Unix-socket NDJSON server speaking the `askgw/1` protocol that lets external Apps (e.g. `aisudo`) present multiple-choice questions to the human via foci's existing interactive-button surface (`SendInteractiveMessageWithID`) — the same path CC permission prompts use. Disabled by default; `setupAskgw` is a no-op when `enabled != true`.

**Protocol:** Line-delimited JSON frames. `ask` (question with options), `answer` (selected option), `cancel` (withdraw), `notify` (completion echo for a previously-answered ask — rendered by editing that ask's chat message in place, standalone message as fallback; see `docs/ASKGW-PROTOCOL.md`), `ack`/`error`. IDs are validated to not contain `:` (platform splits button data on first `:`). Composite keying `(connID, askID)` isolates answers per connection. Message IDs namespaced as `askgw-<askID>-q<idx>` so they never collide with CC permission prompts. A `cancel` frame claims the entry (`Registry.cancel`: removal under `r.mu`) before editing the prompt to "Cancelled by App", so a cancel that loses to an answer or timeout leaves that ask's final text alone (#2073).

**Notify rendering (`Registry.recordAnswered`/`getAnswered`, `Server.handleNotify`):** when an ask is answered, the registry stashes (agentID, sessionKey, platform-native message ID) keyed by askID — separately from the per-connection entry `sendAnswer` just removed — for up to 15 minutes (`answeredTTL`, swept lazily on each write, mirroring `HTTPTransport`'s own abandoned-answer TTL). A later `notify` frame referencing that askID (over *either* transport) looks this up and renders via the `EditMessage`/`NotifyFallback` closures `askgw_setup.go` wires: preferentially `EditMessage` (edits the answered message in place via `platform.SessionNotifier.EditNotificationInSession` or `platform.ButtonSender.EditMessageText` — same session-aware-edit-then-ButtonSender-fallback pattern `agent_platforms.go`'s compaction-notify path uses), falling back to `NotifyFallback` (`route.NotifySessionChat`, a new standalone message) when no platform message ID was captured or the edit fails. The platform message ID itself comes from `PresentFn`'s return value, captured via `Registry.SetPresentedMsgID` each time a question is (re-)presented — `PresentFn` returns `(platformMsgID string, ok bool)` rather than a bare `bool` for exactly this reason.

**Lifecycle logging (#2021):** every ask is traceable in `foci.log` at INFO or above, over both transports (`client` is `socket uid=N` or `http`): `ask arrived` (id, conn, via, source, requested agent, question count, timeout, one-line summary from `askSummary` — Title, else first question's `Header: Question`, whitespace-collapsed, truncated to 120 runes), `ask posted` (resolved agent/session, question i/n, platform message id), then exactly one terminal line — `ask answered` (answers + elapsed), `ask dismissed`, `ask cancelled by client`, `ask abandoned` (socket closed with it pending), or **WARN** `ask timed out` (waited + summary; WARN so it reaches the warn hook — an unanswered approval must not be silent). Also WARN `ask unavailable` (no chat session) / `ask present failed`, INFO `conn=N opened/closed uid=…`, and INFO `notify for unknown or expired ask`. Elapsed times use the registry clock (`entry.arrivedAt`).

**Security model:** Socket owned by group `foci-askgw` (created at install), mode `0660`. `foci-gw` runs with `SupplementaryGroups=... foci-askgw` and `CAP_SETGID`; `procx` drops `foci-askgw` from every child agent subprocess (same mechanism as `foci-secrets`). A peer must both be in the group and have its UID in `allowed_uids`.

**Config (`[askgw]`):** `enabled` (default false), `socket_path` (default `<data>/askgw.sock`), `group` (default `foci-askgw`), `allowed_uids` (required non-empty; accepts usernames or numeric UIDs), `default_agent`, `default_timeout_seconds`, `max_frame_bytes` (default 1 MiB). No persistence across restarts — socket connections die, and answer isolation means answers can only reach the original connection.

### HTTP transport (`internal/askgw/http.go`, `cmd/foci-gw/askgw_http.go`)

**Opt-in, on top of opt-in** (`[askgw] enabled = true` AND `[askgw] http_enabled = true`). A second front door onto the *same* `*askgw.Server`/`Registry`/present-cancel-resolve closures the Unix socket uses — for a REMOTE host (e.g. a Mac running `aisudo`) that can reach foci's HTTP server but not its local Unix socket, without ssh-forwarding the socket. `http_enabled` is a deliberately separate flag from `enabled`: the HTTP endpoint is gated only by `http.api_key` (one bearer token), a materially weaker gate than the socket's `SO_PEERCRED` UID allow-list + Unix group, so upgrading foci must not silently expose a network-reachable ask endpoint for every existing askgw-enabled install.

**Endpoints** (registered on the same `mux` as `/send` etc. — inherits `authMiddleware`/`http.api_key` for free, no new auth scheme):
- `POST /askgw/ask` — body is the same `ask` frame JSON the socket accepts (`protocol`/`type`/`id`/`questions`/...). Presents the question to chat and returns immediately: 202 `{"id","status":"pending"}` on success (mirrors the socket's `ack`), or 400/409 with `{"id","code","error"}` on validation failure/duplicate id.
- `GET /askgw/ask/{id}?wait=<seconds>` — poll for the result. Blocks server-side up to `wait` (default 20s, clamped to 25s) for a human answer; returns the terminal `AnswerFrame` (`answered`/`timeout`/`dismissed`/`unavailable`, plus HTTP-only `cancelled`) once resolved, or `{"status":"pending"}` if `wait` elapses first — the caller re-issues the same GET to keep waiting. 404 if `id` is unknown (never submitted, already collected by a prior terminal poll, or evicted after `HTTPTransport.RunSweeper`'s 15-minute TTL for an answered-but-never-polled ask).
- `POST /askgw/ask/{id}/cancel` — withdraw a pending ask (mirrors the socket's `cancel` frame); unblocks any in-flight poll immediately with `cancelled` instead of leaving it to wait out the full timeout.
- `POST /askgw/notify` — fire-and-forget HTTP counterpart to the socket's `notify` frame. Body is the same `notify` frame JSON; no id/poll bookkeeping of its own (unlike `/askgw/ask`) — goes straight to `askgw.Server.HandleNotifyFrame`, which is transport-agnostic (an ask submitted over the socket can be notified over HTTP, and vice versa, since both share the same `Registry`). Returns 202 once the frame itself validates, regardless of whether a matching answered ask was found to render it against (see `docs/ASKGW-PROTOCOL.md`).

**DESIGN FORK — read before changing this shape.** The socket protocol is async/persistent-duplex (one ask, human answers later — seconds to minutes — correlated by id on the same live connection); HTTP is fundamentally request/response. Three shapes were considered: (1) hold a single POST open until the human answers, (2) submit-returns-id then GET-poll for the result, (3) submit-returns-id then foci calls a webhook back. **Chosen: (2)**, with the poll GET itself long-polling (bounded wait, resumable by re-issuing the same GET) — full rationale in the doc comment on `askgw.HTTPTransport` (`internal/askgw/http.go`). In short: (1) cannot hold open for minutes without weakening the 30s `ReadTimeout`/`WriteTimeout` foci's shared HTTP server applies to every endpoint (`cmd/foci-gw/main.go`), and a held-open connection can't recover from a mid-wait drop on a remote/flaky link anyway (the whole reason this feature exists); (3) requires the remote caller to run its own inbound listener, defeating the point for an unattended client like `aisudo` behind NAT. (2) decouples the ask's lifetime from any one HTTP connection, so a dropped poll simply resumes on the next GET with the same id — nothing is resubmitted or lost.

**Net-new limitation vs the socket transport:** an ask answered while nobody is polling sits in memory (in the HTTP transport's own map, separate from the socket registry's per-connection bookkeeping) until the next poll collects it, or is evicted by the 15-minute sweep if never collected — there is no persistent listener to push the answer to.

## CLI Tool (`cmd/foci/`)

Separate binary (`go build ./cmd/foci`) that wraps the HTTP gateway endpoints for scripts and cron jobs. Auto-discovers the gateway Unix socket at `~/data/foci-gw.sock` (`FOCI_GW_SOCK` env var or `--socket` flag) for same-user auth with no API key. Falls back to TCP + `FOCI_API_KEY` for remote/cross-user access. See [docs/CLI.md](CLI.md) for the full command reference, flags, environment variables, and cron integration examples.

**`foci first-run`** — first-run setup wizard. Generic steps (auth, agent ID, model, character files) live in `cmd/foci/setup.go`. Platform-specific steps (e.g. bot token, user ID) are delegated to providers via the `platform.SetupWizard` interface. Each provider returns a `WizardResult` containing a TOML config fragment and secrets map. The generic wizard appends these to the generated `foci.toml` and stores secrets via `secrets.Store`. `cmd/foci/setup.go` has zero direct telegram imports — it blank-imports `internal/telegram` for provider registration and discovers wizards via `platform.SetupProviders()`. Non-interactive mode collects provider flags dynamically from `SetupFlags()`. The `consoleUI` struct implements `platform.SetupUI` for interactive prompts.

**`foci pretool list|test`** (`cmd/foci/cmd_pretool.go`, #2033) — offline, no gateway: loads the config file via `config.Load` and resolves an agent's PreToolUse rules through `Config.PreToolRules` (the same call the gateway's `livePreToolRules` makes at each CC launch), then runs a sample call through `pretool.Match`.

## Wake / Branch

- **HTTP Branch** (`POST /branch`, CLI `foci branch`; endpoint renamed from `/wake` 2026-07-10): Creates a branch session from the agent's default chat session, injects the text, runs the agent on the branch. Supports `--no-compact` and `--no-reset-hook` flags (`--if-warm`/`--if-cold`, aliased `--if-active`/`--if-inactive`, gate on session cache-warmth). `--oneshot` CLI flag sets both no-compact and no-reset-hook. Returns 412 if no default session. (The internal response text is still literally "wake ok" — cosmetic, not user-facing.)
- **Scheduled Wakes** (`remind` tool with `wake=true`): Agent-initiated timer that fires message injection into the default session at specified delay or timestamp. One-shot, background goroutine, auto-cleaned after firing. Skips if no default session.

## Session-End Reflection

Before a session is cleared (`/reset` or facet TTL reclaim), the agent runs the reflection pass asynchronously. Configured via `[reflection]` section (replaces `session_reset_prompt`).

**Skill-change detection (`internal/skills/snapshot.go`, `internal/skills/gitattrib.go`):** All three reflection paths (periodic interval, session-end, pre-compaction) snapshot skill directories *before* the reflection turn and diff *after*. `SkillSnapshot` is `map[skillDir]map[filePath]time.Time`; `Diff` returns `[]SkillChange` — a skill dir present only in `after` = creation (`IsNew`); a dir with new files or advanced mtimes = update. Deletes are not reported. **The mtime diff alone is only a candidate list, not an attribution** (#1404): `SkillDirs` always includes the process-wide shared `home/shared/skills` tree, so a bare "mtime moved during my wall-clock window" check over-reports whenever anything else — another session's own reflection/session-end/compaction pass, a different agent process, a human/subagent editing skills directly — touches the same shared directory while the window is open. Each caller therefore also records `[winStart, winEnd]` bracketing its own branch/turn and passes `Diff`'s output through `skills.AttributeToGit(ctx, changes, winStart, winEnd)`: it gates each changed skill on a real git commit that (a) lands inside the (git-timestamp-resolution-padded) window and (b) touches the skill's changed files — the convention being that a reflection pass which edits a skill also commits it (see `shared/skills/*`, tracked from `/home/foci`). A skill dir that isn't inside a git working tree, or for which no commit in the window touches its files, produces **nothing** — no false report. Each match yields a `GitReport{Name, Markdown}` — the commit message(s) + `git show`-restricted diff of the touched files. When `notify_on_skill_creation` is true (default) and a `GitReport` is produced, `SkillChangeNotify(sessionKey, skillName, markdown)` fires; wired in `agents_shared.go` (agent-driven paths) and `periodic_setup.go` (periodic runner) via `sendSkillGitReport` (`cmd/foci-gw/skill_git_report.go`), which delivers the markdown as a **document attachment on a system-styled notice** to the reflecting session's chat — for a branch key (session-end, branch-strategy compaction) that is the branch's ROOT chat, since no chat is bound to a branch key and the app would drop the notice (#1652); `AttachDetail`'s blob-backed tappable detail (app; mirrors the compaction-summary flow) falling back to `SendInjectedMessage` + `SendDocument` (Telegram/Discord, or on an attach failure). `Agent.SkillDirs` is the same `reloadSkillsDirs` set used by `ReloadSystemFn`.

Flow (`agent.FireSessionEndMemory` in `internal/agent/session_end_memory.go`):
1. Check `reflection.session_end_enabled` (nil = true, explicit false skips)
2. **Reflect-twice guard** — `SessionIndex.ReflectionRedundant(sessionKey)`: skip if a reflection has already run AND nothing substantive happened since (`last_activity_at <= last_reflection`). Unknown / never-reflected sessions reflect. Relies on activity tracking excluding memory turns (below).
3. Resolve prompt via `prompts.ResolvePrompt(session_end_prompt, ...)` — embedded default on empty/error
4. If prompt resolves to empty, skip
5. For branch sessions, check `BranchMeta.NoResetHook` — if true, skip (unless skipMetaCheck=true for background branches)
6. Create branch from expiring session (copies conversation history)
7. Return immediately — caller proceeds to clear the main session
8. Async: `HandleMessage(ctx, branchKey, prompt)` with 120s timeout, trigger `"session_end_memory"`, NoCompact
9. On success: `SessionIndex.StampReflection(parentKey, dispatchedAt)` — stamps the ORIGINAL (parent) session key, not the branch, with the pre-turn timestamp. (#1465, fixed 2026-07-22: this step was missing — `RunSessionEndMemory` never stamped, so the reflect-twice guard in step 2 could only ever be satisfied by an unrelated periodic interval-reflection pass (below) happening to run more recently than the last real activity. Symptom: "Memories from the previous session are being saved in the background" fired again on the very next reset/reclaim even with zero activity since a session-end reflection had already completed.)

**Activity tracking excludes memory turns.** `last_activity_at` is bumped by `RegisterSessionIndex` / `TouchActivity` (`turn_contract.go`) on every turn *except* those whose trigger is a memory-formation pass — `isMemoryTrigger` returns true for `"reflection"` and `"session_end_memory"` (`internal/agent/context.go`). Without this, a delegated agent's reflection (which injects into the *main* session, not a branch) would bump `last_activity_at` past `last_reflection` and make the reflect-twice guard always fire reflection. Keepalive / background / cron turns still count as activity by design — only the memory passes themselves are excluded. The per-round mid-turn heartbeat (`touchTurnActivity`, above) applies the same `isMemoryTrigger` exclusion, so a long reflection turn's rounds don't bump `last_activity_at` either and the guard stays intact.

Entry points:
- `/reset` command → `agent.ResetSession` (`PrepareSessionEndMemory` → `Store.Reset` → `ClearSessionState` → background `RunSessionEndMemory`)
- `Pool.Acquire` (TTL reclaim) → `ReclaimHook` → `agent.FireSessionEndMemory` (async) → clear session key
- Periodic runner (background branch completion) → `agent.FireSessionEndMemory` (async, skipMetaCheck=true)

## Reflection & Consolidation Timers

Reflection and consolidation run on the shared `periodic.Runner` tick (30s default, see the package doc comment on `internal/periodic/runner.go`). `keepalive.go` split (2026-07-16) along its timer families — one file per mechanism, all methods on the same `*Runner`: `internal/periodic/background.go` (`maybeBackgroundWork`), `cleanup.go` (`maybeReset` + idle/stale cleanup), `consolidation.go` (`maybeConsolidation`), `reflection.go` (`maybeReflection`); `keepalive.go` itself now holds only the keepalive-proper mechanism. `runner.go` owns the shared tick loop and `RunnerConfig`.

**User-activity lookup** (`internal/periodic/user_activity.go`, #2023). Every agent-scoped "has the human been idle?" check goes through `Runner.LastUserActivity()` / `sinceUserActivity()`: the reflection interval + backend quiet-period gates, `consolidation_max_idle`, the background-work idle interval, the `reset_idle_guard`, and the warning dispatchers' active/inactive cadence (`lastUserMsgFn` in `periodic_setup.go`). It returns the max of the **persisted** `session_index.last_user_activity_at` (`SessionIndex.LastUserActivityForAgent`, read live each call — written by the turn path on interactive human turns only) and the runner's **in-process receipt stamp** (`NotifyInteraction`, fed by the platforms' `OnUserMessage`; zero at boot, covers slash commands and messages still queued behind an in-flight turn). Nothing is seeded from boot time, so a restart cannot make an idle agent look active. The one boot fallback: an agent with **no** recorded interaction at all uses `bootedAt` in the scheduler gates (pre-#2023 behaviour, kept on purpose); the dispatchers see zero there (inactive cadence), also as before. Session-scoped checks read the same column per session instead: keepalive `max_user_idle` (`SessionIndex.LastUserActivity`) and the `/send --if-user-active` gate.

**Restart-surviving timers** (`session.PersistedTime`, `internal/session/persisted_time.go`; runner side `internal/periodic/persist.go`, #2026). Every scheduler/gate timestamp that must not reset on a restart is an `agent_metadata` row read and written through this one helper (RFC3339Nano; a zero Save deletes the row; a nil index is a no-op). `New()` boots each runner timer to its fresh-agent default and then `restoreTimers()` overwrites it with the persisted value; each run saves on COMPLETION (so a run cut short by the restart is retried, not skipped), persisting the same value the in-memory schedule uses. Members — add a row here when you add one:
- `reflection_last` → `lastReflection` (fresh default: boot)
- `consolidation_last` → `lastConsolidation` (fire time; fresh default: boot)
- `reset_last` → `lastReset` (fire time; fresh default: boot)
- `background_last_ended` → `lastBackgroundEnded` (background cooldown; fresh default: zero = no cooldown)
- `ephemeral_cleanup_last` → `lastEphemeralCleanup` (daily GC; fresh default: zero = run at boot)
- `warnings_dispatch_last:agent` / `:chat` → `warnings.Dispatcher.lastDispatch` (via `DispatcherConfig.LastDispatchStore`, saved at dispatch)
- `ratelimit_until:<endpoint>` → `RateLimitGate.until` (restored in `getOrCreateRateLimitGate`, saved in `engageRateLimit`, cleared in `releaseRateLimit`; the replay queue and missing-hint streak stay process-scoped)

**Interval reflection** (`maybeReflection`, `internal/periodic/reflection.go`):
1. Check `interval_enabled` (nil = true)
2. Check wall-clock interval elapsed and user not idle (`sinceUserActivity()` must be ≤ interval — see **User-activity lookup** below)
3. Query `session_index` for active chat sessions with `last_activity_at > last_reflection` (per-session tracking)
4. Resolve prompt via `prompts.ResolvePrompt`
5. Iterate all matching sessions: `branchFn("reflection", sessionKey, promptText, true)` for each
6. On success per session: stamp `last_reflection` at branch creation time

Reflection runs before consolidation so the latest memory content is available. Consolidation is blocked while reflection is running.

**Tick order is load-bearing** (`runner.go`, `run()`): `maybeReflection` → `maybeConsolidation` → `maybeReset` → `maybeKeepalive` → `maybeBackgroundWork` → `maybeEphemeralCleanup`. Every `maybeX` sets its `xRunning` flag *synchronously* and then dispatches its work in a goroutine, so a scheduler can only observe a sibling's flag if it runs **after** it in the same tick. Two consumers of that: consolidation and reset skip while reflection is running (the memory-mutating passes are mutually exclusive), and **keepalive skips while any of reflection, consolidation or reset is running** (#1694), logging `skip keepalive: memory task running`. Reflection and consolidation would otherwise branch the same parent inside the same second and collide on the one-second branch key (`session.withChild`'s `ChildTS`), costing a `branch key collision … retrying` retry (logged at INFO; only the give-up after 3 retries is a WARN, #2024) and a second of latency; reset branches nothing but rotates the session key, so a keepalive racing it warms a cache `ClearSessionState` is about to discard. Keepalive yields rather than the reverse: it loses one tick of cache warmth and never a cache expiry, since the warm window's upper bound is the cache TTL.

**Consolidation** (`maybeConsolidation`, `internal/periodic/consolidation.go`) — config now under `[maintenance]` (`r.maintCfg`):
1. Check `consolidation_enabled` (nil = true)
2. Compute next-fire via `parseSchedule(consolidation_time).nextFire(...)` — `consolidation_time` is `"HH:MM"` daily (process tz) or a Go duration; last run persisted as `consolidation_last` (see **Restart-surviving timers**)
3. Check recent user activity (`sinceUserActivity()` ≤ `consolidation_max_idle`)
4. Check reflection / reset is not running
5. Resolve prompt via `prompts.ResolvePrompt`
6. Fire branch on default session: `branchFn("consolidation", parentKey, promptText, true)`
7. On completion: persist the fire time as `consolidation_last`

**Scheduled reset** (`maybeReset`, `internal/periodic/cleanup.go`) — `[maintenance].reset_time` (default off):
1. Skip if `resetFn` nil or `reset_time` empty
2. Compute next-fire via `parseSchedule(reset_time).nextFire(...)` (same dual format as consolidation; `lastReset` anchored to boot, persisted as `reset_last`)
3. Skip if reflection/consolidation/reset already running
4. Inactivity guard: skip if user active within `reset_idle_guard` (default `"55m"`) — mirrors the `foci command --if-inactive` crontab it replaces
5. Skip if no default session or a turn is in flight on it
6. Fire `resetFn(ctx, parentKey)` (→ `Agent.ResetSession`: memory formation + in-place archive, the same path as a manual `/reset`) in a goroutine; persist the fire time as `reset_last` on completion

The shared schedule parser lives in `internal/periodic/schedule.go` (`parseSchedule` / `schedule.nextFire`): a daemon asleep past a clock time fires once on wake (catch-up), never once per missed day; clock times are rebuilt via `time.Date` so they stay stable across DST.

**Proactive warning dispatch** (`warnings.Dispatcher.MaybeFire`):
1. Check `queue != nil` and `dispatchFn != nil` — skip if no injection configured
2. Check `queue.Pending()` — skip if no warnings
3. Check `dispatching` guard — skip if dispatch in flight
4. Determine rate limit interval: call `lastUserMessageTimeFn()`, if within `activityThreshold` → use active interval, else → inactive interval
5. Check `sinceLastDispatch < interval` — skip if too soon (`lastDispatch` is persisted, so this holds across a restart)
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
- **pre_answer** — wired through `delegator.TurnEvents.PreAnswerNudgeFunc`. On `OnResult`, ccstream gives the bookkeeping callback a chance to return a verification follow-up. When non-empty, ccstream re-runs `beginTurn` with the same `TurnEvents`, sends the follow-up via `writer.SendUser`, and skips `OnTurnComplete` until the second round's `OnResult`. `turn_delegated.go` tracks `preAnswerFired` in a closure local so the gate fires at most once per user turn, stashes the round-1 text (not usage — ccstream keeps accumulating across the re-dispatch, so the round-2 result already carries the whole turn) and restores the original answer when round 2 echoes `NoResponseSentinel`. Unlike the API path, the round-1 answer has already streamed to the user as intermediate text via `SessionEvents.OnText` — round 2's text becomes the authoritative final reply.

### Framing text (per-agent overridable)

Every injected nudge is wrapped in three pieces of framing, all methods on `*Agent` in `agent.go`: `wrapStandaloneNudge` (mid-loop after-tools / pre-answer paths) = `nudgePreamble()` + reminder + `nudgeReplyInstruction()`; `wrapBundledNudge` (start-of-turn interval/regex path) = `nudgePreamble()` + reminder, with `nudgeUserBoundary()` appended once at the join point to separate the nudge region from the user's text. The preamble/reply-instruction/user-boundary are each resolved per-agent via `prompts.ResolvePrompt("", "nudge-*.md", embeddedDefault, a.PromptSearchDirs...)` — the SAME override mechanism as the compaction-summary prompt (agent `workspace/prompts` → `shared/prompts` → embedded default from `shared/prompts/nudge-{preamble,reply-instruction,user-boundary}.md`). There is no config path field; the override is purely a same-named file in the agent's prompts dir. Resolved values are stored trimmed and the wrappers reintroduce the exact `\n` / `\n\n` separators. Seeded as editable copies by `seedDefaultPrompts`.

### Trigger Gate (`nudgesAllowed`, #815)

All four nudge paths are gated on `nudgesAllowed(ts) == isUserTrigger(ts.Trigger)` (`context.go`). Nudges shape the agent's user-facing reply, so they fire **only on user-triggered turns**. System-internal turns — `reflection`, `keepalive`, `consolidation`, `session_end_memory` (trigger set in `cmd/foci-gw/agent_sessions.go` via `WithTrigger(ctx, branchType)`) — are exempt at every site:

- `InjectNudges` (both transports) returns *before* `Scheduler.StartTurn`, so system turns also do not advance the `every_n_turns` lifetime counter — the cadence tracks user turns only.
- `PostToolNudgeFunc` / API after-tools loop, and `PreAnswerNudgeFunc` / API pre-answer gate, each short-circuit on a non-user trigger.

Without this gate, the pre-answer gate fired "verify before answering" on reflection turns that wrote memory files (observed 2026-06-05; `ts.Trigger` is populated at `turn_orchestrator.go` before any nudge site runs).

### Configuration

Cooldown (min tool calls between repeating the same rule, default 5 — also the shared cross-rule window for `tool_pattern`, #1309) and max-per-batch (max reminders per tool batch, default 1 — also caps simultaneous regex matches) prevent spam; `nudge_max_per_turn` (default 0 = unlimited) additionally bounds the total injected across a whole turn. All config is per-agent via `nudge_enable`, `nudge_cooldown`, `nudge_max_per_batch`, `nudge_max_per_turn`, `nudge_extraction_model`, `nudge_pre_answer_gate`, `nudge_pre_answer_min_tools`, `nudge_default_enable`, `nudge_default_frequency`.

## Deployment

### `sudo make update`

`sudo make update` (from the foci repo root) — builds Go binaries, installs to `/usr/local/bin`, validates configs, restarts service.

## Testing

```
go test ./...           # all tests (~66, runs in ~1s)
go test ./... -v        # verbose
go test ./session/...   # single package
```

The cache_test.go in `anthropic/` requires `ANTHROPIC_API_KEY` env var and hits the real API. All other tests are self-contained.
