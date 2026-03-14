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

	_ "foci/internal/telegram" // register telegram messaging provider

	"foci/internal/agent"
	"foci/internal/anthropic"
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/memory"
	"foci/internal/platform"
	"foci/internal/secrets"
	"foci/internal/startup"
	"foci/prompts"
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
       foci-gw auth [-config <path>]
       foci-gw version

Flags:
`)
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
Subcommands:
  auth      Authenticate with Anthropic (setup token from Claude Code)
  version   Print version information
`)
	}

	// Handle "foci auth" subcommand before normal flag parsing
	if len(os.Args) >= 2 && os.Args[1] == "auth" {
		authFlags := flag.NewFlagSet("auth", flag.ExitOnError)
		configFile := authFlags.String("config", "", "path to foci.toml (to find secrets.toml)")
		_ = authFlags.Parse(os.Args[2:])

		cfgPath := *configFile
		if cfgPath == "" {
			cfgPath = config.ParseFlags()
		}
		secretsPath := filepath.Join(filepath.Dir(cfgPath), "secrets.toml")
		authStore, err := secrets.Load(secretsPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load secrets: %v\n", err)
			os.Exit(1)
		}
		if err := anthropic.RunSetupTokenFlow(authStore); err != nil {
			fmt.Fprintf(os.Stderr, "auth failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Setup token saved to %s\n", secretsPath)
		os.Exit(0)
	}

	configPath := config.ParseFlags()

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("main", "load config: %v", err)
	}

	// ========== Logging ==========
	logCleanup := initLogging(cfg)
	defer logCleanup()

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

	// ========== Dynamic model alias resolution ==========
	resolveAllAliases(ctx, clients, sec.store, cfg, cfg.Models.Aliases)

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
		StateStore:   si.stateStore,
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
		toolDetailStore.ExpireAndVacuum()
	}

	startTime := time.Now()

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
			sk := inst.defaultSessionKey()
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

	for _, acfg := range cfg.Agents {
		var agentBackends map[string]memory.Searcher
		if ab, ok := mem.agentBackends[acfg.ID]; ok {
			agentBackends = ab
		} else {
			agentBackends = mem.sharedBackends
		}

		// Resolve model using new ResolveModel function
		resolved, err := config.ResolveModel(acfg.Model, acfg.Endpoint, cfg.Models.Aliases)
		if err != nil {
			log.Fatalf("agent %q: %v", acfg.ID, err)
		}

		// Update acfg.Model to the resolved developer/model_id format so all
		// downstream code (SplitDeveloperModel, agent.Model) uses the full ID.
		acfg.Model = resolved.Developer + "/" + resolved.ModelID
		config.ApplyProviderDefaults(&acfg, resolved.Format, cfg)

		agentClient := clients.GetClient(resolved.Endpoint, resolved.Format)
		if agentClient == nil {
			log.Errorf("main", "agent %q: endpoint %q unavailable for model %q (format: %s)", acfg.ID, resolved.Endpoint, resolved.ModelID, resolved.Format)
			continue
		}

		inst := setupAgent(setupParams{
			acfg:                acfg,
			cfg:                 cfg,
			configPath:          configPath,
			client:              agentClient,
			clientProvider:      clients,
			usageClientProvider: usageClients,
			sessions:            si.sessions,
			store:               sec.store,
			bwStore:             sec.bwStore,
			stateStore:          si.stateStore,
			memBackends:         agentBackends,
			reminderStore:       mem.reminderStores[acfg.ID],
			scratchpadStore:     mem.scratchpadStores[acfg.ID],
			todoStore:           mem.todoStores[acfg.ID],
			taskListStore:       mem.taskListStores[acfg.ID],
			sessionIndex:        si.sessionIndex,
			ttsMap:              ttsMap,
			sttMap:              sttMap,
			braveKey:            braveKey,
			startTime:           startTime,
			ctx:                 ctx,
			agentListFn:         agentListFn,
			agentResolverFn:     agentResolverFn,
			connMgr:             connMgr,
			plat:                plat,
		})
		agents[acfg.ID] = inst
		agentOrder = append(agentOrder, acfg.ID)

		// Restore per-session state and seed session meta for default session (if any).
		// Must happen AFTER setupAgent returns so the deferred defaultSessionKeyFn wiring has executed.
		if sk := inst.defaultSessionKey(); sk != "" {
			inst.ag.RestoreSessionOverrides(sk)
			inst.ag.SeedSessionMeta(sk)
		}

		setupPeriodic(inst, acfg, periodicParams{
			cfg:                   cfg,
			sessions:              si.sessions,
			usageClientReg:        usageClients,
			connMgr:               connMgr,
			stateStore:            si.stateStore,
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

		log.Infof("main", "agent %q ready (model=%s, workspace=%s)", acfg.ID, acfg.Model, acfg.Workspace)
	}

	// ========== Post-agent setup ==========
	if len(agentOrder) > 0 {
		firstInst := agents[agentOrder[0]]
		plat.SetupSharedMultiball(platform.SharedMultiballParams{
			FirstHandler:     firstInst.ag,
			FirstCommands:    firstInst.cmds,
			FirstAgentConfig: firstInst.agentCfg,
			AgentOrder:       agentOrder,
			ReclaimHook: func(sessionKey string) {
				for _, id := range agentOrder {
					inst := agents[id]
					if strings.HasPrefix(sessionKey, id+"/") {
						orientPath := prompts.ResolveOrientPath(inst.agentCfg.BranchOrientationHeadlessPrompt, cfg.Sessions.BranchOrientationHeadlessPrompt)
						agent.FireSessionEndMemory(inst.ag, si.sessions, sessionKey, inst.agentCfg.MemoryFormation, func(bk, pk, bt string) string {
							return prompts.BuildBranchOrientation(orientPath, bk, pk, bt, false, inst.promptSearchDirs)
						}, inst.promptSearchDirs, ctx, false)
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
	if stop := setupGoroutineMonitor(cfg, ctx); stop != nil {
		defer stop()
	}
	setupToolDetailCleanup(toolDetailStore, agents, agentOrder, ctx)

	// ========== Signal handling ==========
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	plat.RestoreMultiballSessions(platform.RestoreParams{
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
	diagnosis := startup.DiagnoseRestart(si.stateStore, startTime, logsDir)
	if diagnosis.Class != startup.ClassClean && diagnosis.Class != startup.ClassUnknown {
		log.Infof("startup", "restart classified as %s: %s", diagnosis.Class, diagnosis.Summary)
	}
	for _, id := range agentOrder {
		inst := agents[id]
		enabled := cfg.Telegram.StartupNotify
		if inst.agentCfg.StartupNotify != nil {
			enabled = *inst.agentCfg.StartupNotify
		}
		if enabled {
			for _, conn := range connMgr.AllForAgent(id) {
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
		stateStore:        si.stateStore,
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

	// Log startup
	var agentNames []string
	for _, id := range agentOrder {
		agentNames = append(agentNames, fmt.Sprintf("%s(%s)", id, agents[id].agentCfg.Model))
	}
	log.Infof("main", "started %d agent(s): %s", len(agents), strings.Join(agentNames, ", "))

	// ========== Welcome & first-run ==========
	handleWelcomeAndFirstRun(agents, agentOrder, si.sessions, si.stateStore, cfg, ctx)

	// ========== Wait for signal & shutdown ==========
	<-sigCh

	shutdownTimeout, _ := time.ParseDuration(cfg.HTTP.GracefulShutdownTimeout)
	if shutdownTimeout == 0 {
		shutdownTimeout = 30 * time.Second
	}
	runShutdown(agents, httpServer, &httpMu, connMgr, clients, si.stateStore,
		shutdownConfig{gracefulTimeout: shutdownTimeout, ctx: ctx}, cancel)
}
