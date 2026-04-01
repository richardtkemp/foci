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

	_ "foci/internal/backend/claudecode" // register claude-code-tmux backend
	_ "foci/internal/discord"            // register discord messaging provider
	_ "foci/internal/telegram"           // register telegram messaging provider

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/memory"
	"foci/internal/platform"
	"foci/internal/provision"
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

	configPath := config.ParseFlags()

	// Early log init: open the default event log file so that config parse
	// errors are captured on disk, not just stderr/journal.
	if err := log.Init(log.Config{
		EventFile: config.ResolvePath("logs/foci.log"),
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
	seedSharedDefaults(wsFileMode)

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
	usageClients := newUsageClientRegistry(cfg)

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

	// ========== Per-agent setup ==========
	agents := make(map[string]*agentInstance, len(cfg.Agents))
	var agentOrder []string

	agentResolverFn := func(agentID string) *agentInstance {
		return agents[agentID]
	}

	agentListFn := func() []command.AgentInfo {
		var infos []command.AgentInfo
		for _, id := range agentOrder {
			inst := agents[id]
			sk := mostRecentSessionKey(inst.ag, connMgr, id)
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
				Busy:         inst.ag.IsProcessing(),
				MessageCount: mc,
				LastActivity: lastAct,
			})
		}
		return infos
	}

	// Detect duplicate bot tokens across agents
	botConflicts := config.DetectBotTokenConflicts(cfg.Agents, sec.store)
	skipAgents := buildBotConflictSkipSet(botConflicts)

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
			usageClientProvider: usageClients,
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
			braveKey:     braveKey,
			gwSocketPath: gwSocketPath,
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
				inst.ag.Warnings().Push("error", "config",
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
			usageClientReg:        usageClients,
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
						agent.FireSessionEndMemory(inst.ag, si.sessions, sessionKey, inst.resolved.MemoryFormation,
							orientTemplate, inst.promptSearchDirs, ctx, false)
						return
					}
				}
			},
		})
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
	setupToolDetailCleanup(toolDetailStore, agents, agentOrder, connMgr, ctx)

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
	for _, id := range agentOrder {
		inst := agents[id]
		for _, conn := range connMgr.AllForAgent(id) {
			if !inst.resolved.PlatformNotify(conn.PlatformName()).StartupNotify {
				continue
			}
			name := conn.Username()
			if name == "" {
				name = "foci"
			}
			text := fmt.Sprintf("%s restarted at %s", name, time.Now().Format("15:04:05"))
			if extra := diagnosis.FormatNotification(); extra != "" {
				text += "\n\n" + extra
			}
			conn.SendNotification(text)
		}
	}

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

	mux := http.NewServeMux()
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

	// Log startup
	var agentNames []string
	for _, id := range agentOrder {
		agentNames = append(agentNames, fmt.Sprintf("%s(%s)", id, agents[id].ag.Model))
	}
	log.Infof("main", "started %d agent(s): %s", len(agents), strings.Join(agentNames, ", "))

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
		shutdownConfig{gracefulTimeout: shutdownTimeout, ctx: ctx}, cancel)
}
