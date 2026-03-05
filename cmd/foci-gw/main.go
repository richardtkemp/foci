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

	"foci/internal/anthropic"
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/memory"
	oai "foci/internal/openai"
	"foci/internal/secrets"
	"foci/internal/telegram"
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

	// ========== Shared clients ==========
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	anthropicClient, usageClient, credHolder := resolveCredentials(cfg, sec.store, ctx)
	anthropicClient.SetUseSDK(cfg.Anthropic.UseSDK)
	log.Debugf("main", "anthropic client ready (use_sdk=%v, streaming=%v)", cfg.Anthropic.UseSDK, cfg.Anthropic.Streaming)

	clients := newClientRegistry(cfg, sec.store, anthropicClient, ctx)

	// ========== Dynamic model alias resolution ==========
	resolveAnthropicAliases(anthropicClient, cfg.Models.Aliases)
	if openaiKey, _ := sec.store.Get("openai.api_key"); openaiKey != "" {
		var openaiOpts []oai.Option
		if cfg.OpenAI.BaseURL != "" {
			openaiOpts = append(openaiOpts, oai.WithBaseURL(cfg.OpenAI.BaseURL))
		}
		resolveOpenAIAliases(ctx, oai.NewClient(openaiKey, openaiOpts...), cfg.Models.Aliases)
	}

	// ========== Sessions & State ==========
	si := initSessions(cfg)
	defer si.cleanup()

	// ========== Memory system ==========
	mem := initMemorySystem(cfg)
	defer mem.cleanup()

	// ========== Tool detail store ==========
	toolDetailDbPath := cfg.DataPath("tool_details.db")
	toolDetailStore, err := telegram.NewToolDetailStore(toolDetailDbPath)
	if err != nil {
		log.Errorf("main", "create tool detail store: %v (inline keyboard expansion will not persist)", err)
	} else {
		toolDetailStore.ExpireAndVacuum()
		defer func() {
			toolDetailStore.ExpireAndVacuum()
			_ = toolDetailStore.Close()
		}()
	}

	// ========== Voice providers ==========
	braveKey, _ := sec.store.Get("brave.api_key")
	ttsMap, sttMap := initVoice(cfg, sec.store)

	startTime := time.Now()
	botMgr := telegram.NewBotManager()

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
			acfg:                  acfg,
			cfg:                   cfg,
			configPath:            configPath,
			client:                agentClient,
			getClient:             clients.GetClient,
			peekClient:            clients.PeekClient,
			resolveEndpointClient: clients.ResolveEndpointClient,
			sessions:              si.sessions,
			store:                 sec.store,
			bwStore:               sec.bwStore,
			stateStore:            si.stateStore,
			memBackends:           agentBackends,
			reminderStore:         mem.reminderStore,
			scratchpadStore:       mem.scratchpadStore,
			todoStore:             mem.todoStore,
			toolDetailStore:       toolDetailStore,
			sessionIndex:          si.sessionIndex,
			ttsMap:                ttsMap,
			sttMap:                sttMap,
			braveKey:              braveKey,
			usageClient:           usageClient,
			botMgr:                botMgr,
			startTime:             startTime,
			ctx:                   ctx,
			agentListFn:           agentListFn,
			agentResolverFn:       agentResolverFn,
		})
		agents[acfg.ID] = inst
		agentOrder = append(agentOrder, acfg.ID)

		setupKeepalive(inst, acfg, keepaliveParams{
			cfg:         cfg,
			sessions:    si.sessions,
			usageClient: usageClient,
			botMgr:      botMgr,
			stateStore:  si.stateStore,
			todoStore:   mem.todoStore,
			ctx:         ctx,
		})

		log.Infof("main", "agent %q ready (model=%s, workspace=%s)", acfg.ID, acfg.Model, acfg.Workspace)
	}

	// ========== Post-agent setup ==========
	setupSharedMultiball(botMgr, agents, agentOrder, cfg, sec.store, si.sessions,
		ttsMap, sttMap, toolDetailStore, si.stateStore, ctx)
	setupWarningHooks(agents, cfg)
	if stop := setupTmuxMemoryMonitor(agents, agentOrder, cfg, botMgr, ctx); stop != nil {
		defer stop()
	}
	if stop := setupMemoryGuard(agents, cfg, ctx); stop != nil {
		defer stop()
	}
	setupToolDetailCleanup(toolDetailStore, agents, agentOrder, ctx)

	// ========== Signal handling ==========
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	if si.stateStore != nil {
		restoreMultiballSessions(botMgr, si.stateStore, si.sessions, agents, agentOrder, cfg)
	}
	botMgr.StartAll(ctx)

	// ========== Startup notifications ==========
	sendStartupNotifications(agents, agentOrder, botMgr, si.stateStore, cfg, startTime)

	// ========== HTTP server ==========
	secretsPath := filepath.Join(filepath.Dir(configPath), "secrets.toml")
	var reloadCreds func() error
	if credHolder != nil {
		reloadCreds = func() error {
			st, err := secrets.Load(secretsPath)
			if err != nil {
				return fmt.Errorf("reload secrets.toml: %w", err)
			}
			token, _ := st.Get("anthropic.setup_token")
			if token == "" {
				token, _ = st.Get("anthropic.api_key")
			}
			if token == "" {
				return fmt.Errorf("no setup_token or api_key found in secrets.toml after reload")
			}
			credHolder.Set(token)
			log.Infof("main", "credentials hot-reloaded from secrets.toml")
			return nil
		}
	}

	mux := http.NewServeMux()
	registerHTTPHandlers(mux, httpHandlerDeps{
		agents:            agents,
		agentOrder:        agentOrder,
		stateStore:        si.stateStore,
		sessions:          si.sessions,
		botMgr:            botMgr,
		cfg:               cfg,
		ctx:               ctx,
		ttsMap:            ttsMap,
		sttMap:            sttMap,
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
	handleWelcomeAndFirstRun(agents, agentOrder, si.sessions, si.stateStore, botMgr, cfg, ctx)

	// ========== Wait for signal & shutdown ==========
	<-sigCh

	shutdownTimeout, _ := time.ParseDuration(cfg.HTTP.GracefulShutdownTimeout)
	if shutdownTimeout == 0 {
		shutdownTimeout = 30 * time.Second
	}
	runShutdown(agents, httpServer, &httpMu, botMgr, clients, si.stateStore,
		shutdownConfig{gracefulTimeout: shutdownTimeout, ctx: ctx}, cancel)
}
