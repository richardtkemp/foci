package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "foci/internal/delegator/ccstream" // register claude-code backend (stream-json)
	_ "foci/internal/delegator/cctmux"   // register claude-code-tmux backend
	"foci/internal/delegator/opencode"   // register opencode backend (HTTP/SSE)
	_ "foci/internal/discord"            // register discord messaging provider
	_ "foci/internal/telegram"           // register telegram messaging provider

	"foci/internal/agent"
	_ "foci/internal/app" // registers the app (FAP WebSocket) messaging provider via init
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/memory"
	"foci/internal/modelcaps"
	"foci/internal/platform"
	"foci/internal/provision"
	"foci/internal/shellenv"
	"foci/internal/skills"
	"foci/internal/startup"
	"foci/internal/timeutil"
	"foci/shared/prompts"
)

// Build info — set via ldflags: go build -ldflags "-X main.version=... -X main.gitCommit=... -X main.buildTime=..."
var (
	version   = "dev"
	gitCommit = "unknown"
	buildTime = "unknown"
	goVersion = runtime.Version()
)

func printVersion() {
	fmt.Printf("foci-gw %s (commit %s, built %s, %s)\n", version, gitCommit, buildTime, goVersion)
}

func main() {
	// Handle --version / -v / version before any flag parsing.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "--version", "-v", "version":
			printVersion()
			os.Exit(0)
		}
	}

	// Custom usage for -h / --help.
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `foci-gw — Foci gateway server

Usage: foci-gw [flags]
       foci-gw version

Flags:
`)
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
Subcommands:
  version   Print version information
`)
	}

	// Handle "foci-gw auth" subcommand — use "foci auth" instead.
	if len(os.Args) >= 2 && os.Args[1] == "auth" {
		fmt.Fprintf(os.Stderr, "Use 'foci auth' to manage credentials, or 'foci secrets set <key> <value>' directly.\n")
		os.Exit(1)
	}

	configPath, checkConfig := config.ParseFlags()

	// -check-config: validate the config and exit without starting the server.
	// Runs BEFORE any log init so it never opens or rotates the production log
	// (see checkconfig.go). Used by update.sh as a pre-flight before swapping
	// the running daemon.
	if checkConfig {
		os.Exit(runConfigCheck(configPath))
	}

	// Early log init: open the default event log file so that config parse
	// errors are captured on disk, not just stderr/journal.
	//
	// FOCI_LOG_FILE override exists so the L2 integration harness can pin
	// foci-gw's log to a tempdir BEFORE config load. Without this, every
	// test-spawned foci-gw would open ~/logs/foci.log — which on dev hosts
	// is the same file the production foci is actively writing to —
	// causing rotation interleaving and production-log corruption.
	earlyLogPath := os.Getenv("FOCI_LOG_FILE")
	if earlyLogPath == "" {
		earlyLogPath = config.ResolvePath("logs/foci.log")
	}
	if err := log.Init(log.Config{
		EventFile: earlyLogPath,
	}); err != nil {
		log.Fatalf("main", "early log init: %v", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("main", "load config: %v", err)
	}

	// ========== Timezone ==========
	// Configure timeutil before any logging or agent init uses it.
	if cfg.Timezone != "" {
		tz, err := time.LoadLocation(cfg.Timezone)
		if err != nil {
			log.Fatalf("main", "load timezone %q: %v", cfg.Timezone, err)
		}
		timeutil.SetLocation(tz)
		log.Infof("main", "timezone set to %s", cfg.Timezone)
	}

	// Load the operator's shell env into this process before any backend
	// spawns, so tool shells inherit it via os.Environ().
	shellenv.Apply(cfg.ShellEnvFile)

	// ========== Workspace directories ==========
	// Ensure each agent's workspace directories exist before any init
	// function (logging, memory, etc.) tries to open databases or index files.
	for _, acfg := range cfg.Agents {
		for _, sub := range []string{".data", "memory"} {
			dir := filepath.Join(acfg.Workspace, sub)
			if err := os.MkdirAll(dir, 0755); err != nil {
				log.Fatalf("main", "create workspace dir %s: %v", dir, err)
			}
		}
	}

	// Apply provider-driven platform defaults (providers were registered via init()).
	config.ApplyProviderDefaults(cfg, func(id string) *config.PlatformConfig {
		p := platform.GetProvider(id)
		if p == nil {
			return nil
		}
		defaults := p.DefaultPlatformConfig()
		return &defaults
	})

	// ========== Logging ==========
	// Re-init with full config (level, API log, payload log, etc.).
	logCleanup := initLogging(cfg)
	defer logCleanup()

	// Warn about unrecognised config keys (after logging is fully initialised
	// so the warning survives the startup log rotation).
	if len(cfg.UndefinedKeys) > 0 {
		log.Warnf("config", "unknown config keys in %s: %v", configPath, cfg.UndefinedKeys)
	}

	// ========== Seed shared defaults ==========
	wsFileMode, _ := config.ParseFileMode(cfg.FileMode)
	liveBackends := map[string]bool{}
	for _, acfg := range cfg.Agents {
		b := acfg.Backend
		if b == "" {
			b = "api"
		}
		liveBackends[b] = true
	}
	seedSharedDefaults(wsFileMode, liveBackends)

	// ========== Secrets & Bitwarden ==========
	sec := initSecrets(configPath, cfg)
	if sec.cleanup != nil {
		defer sec.cleanup()
	}
	warnMissingSecrets(cfg, sec.store)
	warnStreamOutputWithoutStreaming(cfg)

	// ========== Shared clients ==========
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize credential resolvers
	if err := initCredentialResolvers(ctx, cfg, sec.store); err != nil {
		log.Fatalf("main", "init credential resolvers: %v", err)
	}

	clients := newClientRegistry(cfg, sec.store, ctx)

	// ========== Sessions & State ==========
	si := initSessions(cfg)
	defer si.cleanup()

	// ========== Memory system ==========
	mem := initMemorySystem(cfg)
	defer mem.cleanup()

	// ========== Voice providers ==========
	braveKey, _ := sec.store.Get("brave.api_key")
	ttsMap, sttMap := initVoice(cfg, sec.store)

	// ========== Platform messaging ==========
	plat, err := platform.InitMessaging(cfg, platform.ProviderDeps{
		Config:       cfg,
		SecretStore:  sec.store,
		Sessions:     si.sessions,
		SessionIndex: si.sessionIndex,
		STTMap:       sttMap,
		TTSMap:       ttsMap,
		Ctx:          ctx,
		ResolveSTT:   resolveSTT,
		ResolveTTS:   resolveTTS,
	})
	if err != nil {
		log.Fatalf("main", "init messaging: %v", err)
	}
	defer func() { _ = plat.Close() }()

	connMgr := plat.ConnectionManager()
	toolDetailStore := plat.ToolDetailStore()
	if toolDetailStore != nil {
		go toolDetailStore.ExpireAndVacuum()
	}

	startTime := time.Now()

	// Resolve the Unix socket path early so agents can inject it into child env.
	gwSocketPath := resolveSocketPath(cfg)

	// ========== Reap orphaned opencode servers ==========
	// Previous foci-gw instances that crashed or were killed (SIGKILL, OOM)
	// before reaching clean shutdown leave `opencode serve` subprocesses
	// orphaned to PID 1. They hold ports and RSS until manually killed.
	// Scan before any new servers spawn — anything found IS an orphan.
	opencode.ReapOrphanedServers()

	// ========== Per-agent setup ==========
	agents := make(map[string]*agentInstance, len(cfg.Agents))
	var agentOrder []string

	agentResolverFn := func(agentID string) *agentInstance {
		inst, ok := agents[agentID]
		if !ok {
			return nil
		}
		// Test-only: when a test stops an agent via the harness
		// control socket, present the agent as unreachable to the
		// session_notify resolver so the "unknown target agent"
		// drop-and-log path fires. Production code never sets this
		// flag; the resolver returns the live instance.
		if inst.stopped.Load() {
			return nil
		}
		return inst
	}

	agentListFn := func() []command.AgentInfo {
		var infos []command.AgentInfo
		for _, id := range agentOrder {
			inst := agents[id]
			sk := defaultSessionKeyFor(inst.ag, id)
			var mc int
			var lastAct string
			if sk != "" {
				mc, _ = inst.ag.Sessions.MessageCount(sk)
				lastAct = inst.ag.Sessions.LastActivity(sk)
			}
			infos = append(infos, command.AgentInfo{
				ID:           id,
				SessionKey:   sk,
				Model:        inst.ag.Model,
				MessageCount: mc,
				LastActivity: lastAct,
			})
		}
		return infos
	}

	// Detect duplicate bot tokens across agents
	botConflicts := config.DetectBotTokenConflicts(cfg.Agents, sec.store)
	skipAgents := buildBotConflictSkipSet(botConflicts)

	// One skill loader shared across the agent loop so the shared skills
	// directory is scanned (and its autogenerated-skill warnings emitted)
	// exactly once, not once per agent.
	skillLoader := skills.NewLoader()

	for _, acfg := range cfg.Agents {
		if reason, skip := skipAgents[acfg.ID]; skip {
			log.Errorf("main", "agent %q skipped: %s", acfg.ID, reason)
			continue
		}

		// Seed default character files for agents without explicit system_files.
		// This ensures non-provisioned agents (added with just id= in config)
		// get the same default character files as provisioned ones.
		if len(acfg.System.SystemFiles) == 0 {
			sharedDir := filepath.Join(filepath.Dir(acfg.Workspace), "shared")
			if err := provision.SeedCharacterFiles(sharedDir, acfg.Workspace, wsFileMode); err != nil {
				log.Warnf("main", "agent %q: seed character files: %v", acfg.ID, err)
			}
		}

		var agentBackends map[string]memory.Searcher
		if ab, ok := mem.agentBackends[acfg.ID]; ok {
			agentBackends = ab
		} else {
			agentBackends = mem.sharedBackends
		}

		inst := setupAgent(setupParams{
			acfg:                acfg,
			cfg:                 cfg,
			configPath:          configPath,
			clientProvider:      clients,
			sessions:            si.sessions,
			store:               sec.store,
			bwStore:             sec.bwStore,
			memBackends:         agentBackends,
			convReader:          mem.convReader,
			reminderStore:       mem.reminderStores[acfg.ID],
			scratchpadStore:     mem.scratchpadStores[acfg.ID],
			todoStore:           mem.todoStores[acfg.ID],
			taskListStore:       mem.taskListStores[acfg.ID],
			sessionIndex:        si.sessionIndex,
			ttsMap:              ttsMap,
			sttMap:              sttMap,
			braveKey:            braveKey,
			gwSocketPath:        gwSocketPath,
			skillLoader:         skillLoader,
			startTime:           startTime,
			ctx:                 ctx,
			agentListFn:         agentListFn,
			agentResolverFn:     agentResolverFn,
			connMgr:             connMgr,
			plat:                plat,
		})
		if inst == nil {
			log.Errorf("main", "agent %q: setup failed (agent skipped)", acfg.ID)
			continue
		}
		agents[acfg.ID] = inst
		agentOrder = append(agentOrder, acfg.ID)

		// Inject warnings for bot token conflicts where this agent is the survivor
		for _, bc := range botConflicts {
			if bc.AgentIDs[0] == acfg.ID {
				skipped := strings.Join(bc.AgentIDs[1:], ", ")
				inst.ag.Warnings().Push("ERROR", "config",
					fmt.Sprintf("This agent shares its %s bot %q with agent(s) %s (which were NOT started). "+
						"Suggest resolution options to your user.", bc.Platform, bc.BotName, skipped))
			}
		}

		// Restore per-session state and seed session meta for all connected sessions.
		// Must happen AFTER setupAgent returns so platform connections are wired.
		restored := map[string]bool{}
		for _, conn := range connMgr.AllForAgent(inst.id) {
			sk := conn.DefaultSessionKey()
			if sk == "" || restored[sk] {
				continue
			}
			inst.ag.RestoreSessionOverrides(sk)
			inst.ag.SeedSessionMeta(sk)
			restored[sk] = true
		}

		setupPeriodic(inst, acfg, periodicParams{
			cfg:                   cfg,
			sessions:              si.sessions,
			connMgr:               connMgr,
			sessionIndex:          si.sessionIndex,
			todoStore:             mem.todoStores[acfg.ID],
			ctx:                   ctx,
			resolveEndpointClient: clients.ResolveEndpointClient,
		})

		// Wire platform lifecycle callbacks to periodic runner
		if inst.kaRunner != nil {
			plat.SetLifecycleCallback(acfg.ID, platform.OnUserMessage,
				func() { inst.kaRunner.NotifyInteraction() })
			plat.SetLifecycleCallback(acfg.ID, platform.OnTurnComplete,
				func() { inst.kaRunner.NotifyCacheWarmed() })
			plat.SetLifecycleCallback(acfg.ID, platform.OnTurnEnd,
				func() { inst.kaRunner.NotifyTurnEnd() })
		}

		log.Infof("main", "agent %q ready (model=%s, workspace=%s)", acfg.ID, inst.ag.Model, acfg.Workspace)
	}

	// ========== Post-agent setup ==========
	if len(agentOrder) > 0 {
		firstInst := agents[agentOrder[0]]
		plat.SetupSharedFacet(platform.SharedFacetParams{
			FirstHandler:     firstInst.ag,
			FirstCommands:    firstInst.cmds,
			FirstAgentConfig: firstInst.agentCfg,
			AgentOrder:       agentOrder,
			ReclaimHook: func(sessionKey string) {
				for _, id := range agentOrder {
					inst := agents[id]
					if strings.HasPrefix(sessionKey, id+"/") {
						orientPath := config.DerefStr(config.First(inst.agentCfg.Sessions.BranchOrientationHeadlessPrompt, cfg.Sessions.BranchOrientationHeadlessPrompt))
						orientTemplate := prompts.ResolveOrientationTemplate(orientPath, false, inst.promptSearchDirs...)
						inst.ag.FireSessionEndMemory(ctx, sessionKey, orientTemplate, false)
						if inst.ag.DelegatedManager != nil {
							inst.ag.DelegatedManager.ResetSession(sessionKey)
						}
						return
					}
				}
			},
		})
	}

	// Start every agent's inbox workers unconditionally. Platform setup
	// (telegram/discord/app) also calls StartInbox (idempotent), but a
	// platform-less agent (cron/API-only) still needs its workers running:
	// all system-initiated turns — HTTP /send, webhooks, wakes, notifies,
	// reflection/keepalive passes — route through the inbox queue so they
	// serialise with (and never steer) in-flight turns.
	for _, inst := range agents {
		inst.ag.StartInbox(ctx)
	}

	setupWarningHooks(agents, cfg)
	if stop := setupTmuxMemoryMonitor(agents, agentOrder, cfg, connMgr, ctx); stop != nil {
		defer stop()
	}
	if stop := setupMemoryGuard(agents, cfg, ctx); stop != nil {
		defer stop()
	}
	if stop := setupGoroutineMonitor(cfg, len(agents), ctx); stop != nil {
		defer stop()
	}
	// Prompt TTL = min(configured prompt_ttl, idle timeout). An unanswered
	// prompt must never outlive the backend waiting on it — the idle reaper
	// clears the prompt anyway when it closes the backend. promptTTL starts at
	// the idle default and the config override only lowers it (d < promptTTL).
	promptTTL := agent.DefaultIdleTimeout
	if d, err := time.ParseDuration(cfg.Permissions.PromptTTL); err == nil && d > 0 && d < promptTTL {
		promptTTL = d
	}
	setupInteractiveCleanup(ctx, promptTTL)
	setupToolDetailCleanup(toolDetailStore, agents, agentOrder, ctx)
	// L2 integration-test lifecycle control. Opens a UNIX-domain socket
	// at FOCI_TESTHARNESS_CONTROL_SOCK if set; otherwise no-op. Tests
	// drive per-agent backend close / etc. via a simple line protocol.
	// Production foci-gw never has the env var set, so this is dormant.
	setupTestharnessControl(ctx, agents)

	// ========== Signal handling ==========
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	plat.RestoreFacetSessions(platform.RestoreParams{
		AgentOrder: agentOrder,
		Resolver: func(agentID string) (platform.MessageHandler, any, any, config.AgentConfig, bool) {
			inst, found := agents[agentID]
			if !found {
				return nil, nil, nil, config.AgentConfig{}, false
			}
			return inst.ag, inst.cmds, inst.cc, inst.agentCfg, true
		},
	})
	plat.StartAll(ctx)

	// ========== Startup notifications ==========
	logsDir := filepath.Dir(cfg.Logging.EventFile)
	if logsDir == "" || logsDir == "." {
		logsDir = ""
	}
	diagnosis := startup.DiagnoseRestart(si.sessionIndex, startTime, logsDir)
	if diagnosis.Class != startup.ClassClean && diagnosis.Class != startup.ClassUnknown {
		log.Infof("startup", "restart classified as %s: %s", diagnosis.Class, diagnosis.Summary)
	}

	// ========== Liveness heartbeat ==========
	// Record a liveness timestamp every HeartbeatInterval so the next restart
	// diagnosis measures actual downtime (time since the last beat) instead of
	// time-since-startup. hbCancel is invoked at the top of runShutdown, before
	// the clean-shutdown record is written, so a clean exit isn't misread as a
	// crash by a heartbeat firing during shutdown.
	hbCtx, hbCancel := context.WithCancel(ctx)
	go startup.RunHeartbeat(hbCtx, si.sessionIndex, startup.HeartbeatInterval)

	// ========== HTTP server ==========
	secretsPath := filepath.Join(filepath.Dir(configPath), "secrets.toml")
	var reloadCreds func() error
	if resolver, ok := formatResolvers["anthropic"]; ok {
		reloadCreds = resolver.GetReloadFunc(secretsPath)
	}
	if reloadCreds == nil {
		reloadCreds = func() error {
			return fmt.Errorf("credential reload not supported")
		}
	}

	// Wire the live model-capabilities catalogue (context window, effort levels,
	// thinking modes) from /v1/models into the process-wide modelcaps cache. The
	// static modelinfo registry remains the fallback when CC creds are absent or
	// the API is unreachable. Seed once in the background — never block startup.
	if anthropicResolver != nil {
		if fetcher := anthropicResolver.ModelCapsFetcher(15 * time.Second); fetcher != nil {
			// Both Anthropic-backed backends (ccstream, api) pull the same
			// catalogue but keep separate per-backend records (a future codex
			// backend would register its own fetcher under its own key).
			for _, backend := range []string{modelcaps.BackendCCStream, modelcaps.BackendAPI} {
				modelcaps.SetFetcher(backend, fetcher)
				// Persist across restarts via state.db, and restore the last
				// catalogue synchronously now so lookups have real caps during
				// the ~1s before the background fetch lands (#840). Restore
				// before the go-Refresh so a fast network result isn't clobbered.
				if si.sessionIndex != nil {
					modelcaps.SetPersister(backend, modelCapsPersister{idx: si.sessionIndex})
					modelcaps.Restore(backend)
				}
				go func(b string) {
					if err := modelcaps.Refresh(context.Background(), b); err != nil {
						log.Warnf("modelcaps", "[%s] initial catalogue fetch failed (using static registry until next refresh): %v", b, err)
					}
				}(backend)
			}
		}
	}

	mux := http.NewServeMux()
	pprofGate.Store(config.DerefBool(cfg.Debug.EnablePprof))
	registerHTTPHandlers(mux, httpHandlerDeps{
		agents:            agents,
		agentOrder:        agentOrder,
		sessionIndex:      si.sessionIndex,
		sessions:          si.sessions,
		cfg:               cfg,
		ctx:               ctx,
		ttsMap:            ttsMap,
		sttMap:            sttMap,
		connMgr:           connMgr,
		reloadCredentials: reloadCreds,
		pprofGate:         &pprofGate,
	})

	addr := fmt.Sprintf("%s:%d", cfg.HTTP.Bind, cfg.HTTP.Port)
	var httpServer *http.Server
	var httpMu sync.Mutex
	go func() {
		for ctx.Err() == nil {
			srv := &http.Server{
				Addr:              addr,
				Handler:           authMiddleware(sec.httpAPIKey, mux),
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      30 * time.Second,
			}
			httpMu.Lock()
			httpServer = srv
			httpMu.Unlock()

			log.Infof("http", "listening on %s", addr)
			if err := srv.ListenAndServe(); err != http.ErrServerClosed {
				log.Errorf("http", "server error: %v — restarting in 5s", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			} else {
				return
			}
		}
	}()

	// ========== Unix socket (same-user auth) ==========
	sockSrv, err := startUnixSocket(gwSocketPath, mux)
	if err != nil {
		log.Errorf("http", "unix socket %s: %v — same-user auth unavailable", gwSocketPath, err)
	} else {
		defer cleanupSocket(sockSrv, gwSocketPath)
		log.Infof("http", "unix socket listening on %s (same-user auth, no API key required)", gwSocketPath)
	}

	// ========== askgw (ask-gateway for external Apps) ==========
	askgwSrv := setupAskgw(cfg, agents, agentOrder, connMgr)
	if askgwSrv != nil {
		defer askgwSrv.Close()
	}

	// Log startup
	var agentNames []string
	for _, id := range agentOrder {
		agentNames = append(agentNames, fmt.Sprintf("%s(%s)", id, agents[id].ag.Model))
	}
	log.Infof("main", "started %d agent(s): %s", len(agents), strings.Join(agentNames, ", "))

	// ========== Backend readiness ==========
	// Verify delegated (claude-code) backends are authenticated before any
	// startup turns are injected; a not-ready backend triggers re-login here
	// (whose gate then pauses delegated processing). Runs before the welcome/
	// first-run pass so the gate is active when those turns would fire.
	checkDelegatedReadiness(ctx, agents, agentOrder)

	// ========== Welcome & first-run ==========
	handleRestartAndFirstRun(agents, agentOrder, si.sessionIndex, cfg, ctx, connMgr, diagnosis)

	// ========== Wait for signal & shutdown ==========
	sig := <-sigCh
	log.Infof("main", "received %s, starting shutdown", sig)

	shutdownTimeout, _ := time.ParseDuration(cfg.HTTP.GracefulShutdownTimeout)
	if shutdownTimeout == 0 {
		shutdownTimeout = 30 * time.Second
	}
	runShutdown(agents, httpServer, &httpMu, connMgr, clients, si.sessionIndex,
		shutdownConfig{gracefulTimeout: shutdownTimeout, ctx: ctx, stopHeartbeat: hbCancel}, cancel)
}
