package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"foci/agent"
	"foci/anthropic"
	"foci/command"
	"foci/config"
	"foci/gemini"
	"foci/keepalive"
	"foci/log"
	"foci/mana"
	"foci/memory"
	oai "foci/openai"
	"foci/prompts"
	"foci/provider"
	"foci/resources"
	"foci/secrets"
	"foci/secrets/bitwarden"
	"foci/session"
	"foci/startup"
	"foci/state"
	"foci/telegram"
	"foci/tools"
	"foci/warnings"
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

	// Initialize logging
	if err := log.Init(log.Config{
		Level:       cfg.Logging.Level,
		EventFile:   cfg.Logging.EventFile,
		APIFile:     cfg.Logging.APIFile,
		PayloadFile: cfg.Logging.PayloadFile,
	}); err != nil {
		log.Fatalf("main", "init logging: %v", err)
	}
	defer log.Close()

	// Log rotation
	if cfg.Logging.LogRotation {
		rotPeriod, _ := time.ParseDuration(cfg.Logging.RotationPeriod)
		retPeriod, _ := time.ParseDuration(cfg.Logging.RetentionPeriod)
		maxLineSize, _ := config.ParseByteSize(cfg.Logging.RotationMaxLineSize)
		archiveDir := cfg.Logging.ArchiveDir
		if archiveDir == "" {
			archiveDir = filepath.Join(filepath.Dir(cfg.Logging.EventFile), "archive")
		}
		var files []string
		for _, p := range []string{cfg.Logging.EventFile, cfg.Logging.APIFile, cfg.Logging.PayloadFile} {
			if p != "" {
				files = append(files, p)
			}
		}
		stopRotation := log.StartRotation(log.RotationConfig{
			Period:      rotPeriod,
			Retention:   retPeriod,
			MaxLineSize: maxLineSize,
			ArchiveDir:  archiveDir,
			Files:       files,
		})
		defer stopRotation()
	}

	// API call log (SQLite)
	if cfg.Logging.APIDB != "" {
		if err := log.InitAPIDB(cfg.Logging.APIDB); err != nil {
			log.Fatalf("main", "init API db: %v", err)
		}
		defer log.CloseAPIDB()
	}

	// Conversation log (SQLite)
	if cfg.Logging.ConversationFile != "" {
		if err := log.InitConversation(cfg.Logging.ConversationFile); err != nil {
			log.Fatalf("main", "init conversation log: %v", err)
		}
		defer log.CloseConversation()
	}

	// Seed default prompts to ~/shared/prompts/ for user customisation
	if home, err := os.UserHomeDir(); err == nil {
		seedDefaultPrompts(filepath.Join(home, "shared", "prompts"))
	}

	// Load secrets (from secrets.toml alongside config file)
	secretsPath := filepath.Join(filepath.Dir(configPath), "secrets.toml")
	store, err := secrets.Load(secretsPath)
	if err != nil {
		log.Fatalf("main", "load secrets: %v", err)
	}
	if names := store.Names(); len(names) > 0 {
		log.Infof("main", "loaded %d secrets: %v", len(names), names)
	}

	// Startup security checks for secrets.toml
	if !cfg.SkipSecurityChecks {
		if warnings := store.CheckSecurity(); len(warnings) > 0 {
			for _, w := range warnings {
				log.Warnf("security", "%s", w)
			}
		}
	}
	if len(cfg.Agents) > 1 && !store.HasAgentRestrictions() {
		log.Warnf("security", "multiple agents but no allowed_agents/denied_agents in secrets.toml — all agents can access all secrets")
	}

	// Auto-generate HTTP API key if not present
	httpAPIKey, _ := store.Get("http.api_key")
	if httpAPIKey == "" {
		generated, err := secrets.GeneratePassphrase(5)
		if err != nil {
			log.Fatalf("main", "generate HTTP API key: %v", err)
		}
		store.Set("http.api_key", generated)
		if err := store.Save(); err != nil {
			log.Fatalf("main", "save HTTP API key: %v", err)
		}
		httpAPIKey = generated
		log.Infof("main", "generated HTTP API key — add FOCI_API_KEY to crontab: %s", httpAPIKey)
	}

	// Wire child process group-dropping into the command package
	// (so script commands also drop supplementary groups).
	command.ChildSysProcAttr = tools.ChildSysProcAttr

	// Shared: Bitwarden store (optional)
	var bwStore *bitwarden.Store
	if cfg.Bitwarden.Enabled {
		secretTTL, _ := time.ParseDuration(cfg.Bitwarden.SecretTTL)
		bwExec := &bitwarden.DefaultExecutor{SessionFile: cfg.Bitwarden.SessionFile}
		bwStore = bitwarden.New(bwExec, secretTTL)

		if err := bwStore.Refresh(); err != nil {
			log.Errorf("main", "bitwarden initial refresh: %v", err)
		} else {
			log.Infof("main", "bitwarden: loaded %d vault items", bwStore.ItemCount())
		}

		// Background refresh ticker
		refreshInterval, _ := time.ParseDuration(cfg.Bitwarden.RefreshInterval)
		go func() {
			ticker := time.NewTicker(refreshInterval)
			defer ticker.Stop()
			for range ticker.C {
				if err := bwStore.Refresh(); err != nil {
					log.Warnf("bitwarden", "background refresh: %v", err)
				}
			}
		}()

		// Background cleanup of expired values
		cleanupInterval, _ := time.ParseDuration(cfg.Bitwarden.CleanupInterval)
		bwStore.StartCleanup(cleanupInterval)
		defer bwStore.Close()
	}

	// Shared: Anthropic client
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	anthropicClient, usageClient, credHolder := resolveCredentials(cfg, store, ctx)
	anthropicClient.SetUseSDK(cfg.Anthropic.UseSDK)
	log.Debugf("main", "anthropic client ready (use_sdk=%v, streaming=%v)", cfg.Anthropic.UseSDK, cfg.Anthropic.Streaming)

	// Lazy client registry — creates provider clients on first use per endpoint:format pair.
	type clientEntry struct {
		client provider.Client
		once   sync.Once
	}
	clientRegistry := map[string]*clientEntry{}
	var clientRegistryMu sync.Mutex

	getClient := func(endpointName, format string) provider.Client {
		key := endpointName + ":" + format
		clientRegistryMu.Lock()
		entry, ok := clientRegistry[key]
		if !ok {
			entry = &clientEntry{}
			clientRegistry[key] = entry
		}
		clientRegistryMu.Unlock()

		entry.once.Do(func() {
			epCfg, exists := cfg.Endpoints[endpointName]
			if !exists {
				log.Errorf("main", "endpoint %q not found in config", endpointName)
				return
			}

			// Resolve API key from secrets store
			apiKeyName := epCfg.APIKey
			if apiKeyName == "" {
				apiKeyName = endpointName + ".api_key"
			}

			switch format {
			case "anthropic":
				// Built-in anthropic endpoint uses resolveCredentials (setup-token, API key, CC creds).
				// Other endpoints using anthropic format use simple API key auth.
				if endpointName == "anthropic" {
					entry.client = anthropicClient
					return
				}
				apiKey, _ := store.Get(apiKeyName)
				if apiKey == "" {
					log.Errorf("main", "%s not found in secrets — endpoint %q (anthropic format) unavailable", apiKeyName, endpointName)
					return
				}
				httpTimeout := parseDurationDefault(epCfg.HTTPTimeout, parseDurationDefault(cfg.Anthropic.HTTPTimeout, 600*time.Second))
				holder := &tokenHolder{token: apiKey}
				c := anthropic.NewClientWithTokenFunc(holder.Get, httpTimeout)
				url := epCfg.URLForFormat("anthropic")
				if url != "" {
					c.SetBaseURL(url)
				}
				c.SetUseSDK(cfg.Anthropic.UseSDK)
				entry.client = c
				log.Infof("main", "anthropic client ready for endpoint %q (url=%s)", endpointName, url)

			case "gemini":
				if endpointName == "gemini" {
					// Built-in gemini endpoint
					apiKey, _ := store.Get("gemini.api_key")
					if apiKey == "" {
						log.Errorf("main", "gemini.api_key not found in secrets — gemini endpoint unavailable")
						return
					}
					httpTimeout, err := time.ParseDuration(cfg.Gemini.HTTPTimeout)
					if err != nil {
						httpTimeout = 120 * time.Second
					}
					opts := []gemini.Option{gemini.WithHTTPTimeout(httpTimeout)}
					if cfg.Gemini.CacheTTL != "0" {
						if cacheTTL, err := time.ParseDuration(cfg.Gemini.CacheTTL); err == nil && cacheTTL > 0 {
							opts = append(opts, gemini.WithCacheTTL(cacheTTL))
						}
					}
					gc, err := gemini.NewClient(ctx, apiKey, opts...)
					if err != nil {
						log.Errorf("main", "create gemini client: %v", err)
						return
					}
					entry.client = gc
					log.Infof("main", "gemini client ready (cache_ttl=%s)", cfg.Gemini.CacheTTL)
				} else {
					log.Errorf("main", "gemini format on non-gemini endpoint %q not supported", endpointName)
				}

			case "openai":
				apiKey, _ := store.Get(apiKeyName)
				if apiKey == "" {
					log.Errorf("main", "%s not found in secrets — endpoint %q (openai format) unavailable", apiKeyName, endpointName)
					return
				}
				httpTimeout := parseDurationDefault(epCfg.HTTPTimeout, parseDurationDefault(cfg.OpenAI.HTTPTimeout, 120*time.Second))
				opts := []oai.Option{oai.WithHTTPTimeout(httpTimeout)}
				url := epCfg.URLForFormat("openai")
				if url == "" && endpointName == "openai" {
					url = cfg.OpenAI.BaseURL
				}
				if url != "" {
					opts = append(opts, oai.WithBaseURL(url))
				}
				entry.client = oai.NewClient(apiKey, opts...)
				log.Infof("main", "openai client ready for endpoint %q (url=%s)", endpointName, url)
			}
		})

		return entry.client
	}

	// peekClient returns the client for an endpoint:format pair without initializing it.
	peekClient := func(endpointName, format string) provider.Client {
		key := endpointName + ":" + format
		clientRegistryMu.Lock()
		entry, ok := clientRegistry[key]
		clientRegistryMu.Unlock()
		if !ok || entry == nil {
			return nil
		}
		return entry.client
	}

	// resolveEndpointClient resolves the client for an endpoint+modelID pair.
	// Infers wire format from model name, falls back to openai if endpoint doesn't support it.
	resolveEndpointClient := func(endpointName, modelID string) provider.Client {
		format := config.InferFormat(modelID)
		epCfg, ok := cfg.Endpoints[endpointName]
		if ok && !epCfg.SupportsFormat(format) {
			format = "openai" // universal fallback
		}
		return getClient(endpointName, format)
	}

	// Shared: Session store
	sessions := session.NewStore(cfg.Sessions.Dir)
	log.Debugf("main", "session store dir=%s", cfg.Sessions.Dir)

	// Repair sessions with orphaned tool_use blocks (from mid-tool-call restarts)
	if n, err := sessions.RepairOrphans(); err != nil {
		log.Warnf("main", "session repair: %v", err)
	} else if n > 0 {
		log.Infof("main", "repaired %d orphaned session(s) with interrupted tool calls", n)
	}

	// Inject restart markers into recently active sessions
	if n, err := sessions.InjectRestartMarkers(session.RestartMarkerMaxAge); err != nil {
		log.Warnf("main", "restart markers: %v", err)
	} else if n > 0 {
		log.Infof("main", "injected restart markers into %d active session(s)", n)
	}

	// Shared: Session index (SQLite-backed metadata index of all session files)
	sessionIndexPath := cfg.DataPath("session_index.db")
	sessionIndex, err := session.NewSessionIndex(sessionIndexPath)
	if err != nil {
		log.Errorf("main", "create session index: %v (session index disabled)", err)
	} else {
		defer func() { _ = sessionIndex.Close() }()
		// Rebuild index from disk on startup
		if n, err := sessionIndex.Rebuild(sessions); err != nil {
			log.Warnf("main", "rebuild session index: %v", err)
		} else {
			log.Infof("main", "session index: %d sessions indexed", n)
		}
		// Wire lifecycle events from session store to index
		sessions.OnSessionEvent(func(e session.SessionEvent) {
			switch e.Status {
			case session.SessionStatusActive:
				sessionIndex.Upsert(session.SessionIndexEntry{
					SessionKey:       e.Key,
					FilePath:         e.FilePath,
					CreatedAt:        e.CreatedAt,
					ParentSessionKey: e.ParentKey,
					SessionType:      e.Type,
					Status:           session.SessionStatusActive,
				})
			case session.SessionStatusCompacted:
				// Session was compacted (Replace): the current file is still active,
				// and the archive file is a new compacted entry.
				if e.ArchivePath != "" {
					// Derive archive key from file path
					rel, err := filepath.Rel(cfg.Sessions.Dir, e.ArchivePath)
					if err == nil {
						archiveKey := strings.TrimSuffix(rel, ".jsonl")
						archiveKey = strings.ReplaceAll(archiveKey, string(filepath.Separator), ":")
						sessionIndex.Upsert(session.SessionIndexEntry{
							SessionKey:       archiveKey,
							FilePath:         e.ArchivePath,
							CreatedAt:        time.Now().UTC(),
							ParentSessionKey: e.Key,
							SessionType:      e.Type,
							Status:           session.SessionStatusCompacted,
						})
					}
				}
				// Keep the current session as active (don't change its status)
			case session.SessionStatusCleared:
				sessionIndex.SetStatus(e.Key, session.SessionStatusCleared)
			}
		})

		// Start archive sweep goroutine
		archiveAfter, err := time.ParseDuration(cfg.Sessions.ArchiveAfter)
		if err != nil {
			log.Warnf("main", "invalid sessions.archive_after %q: %v (archive sweep disabled)", cfg.Sessions.ArchiveAfter, err)
		} else {
			archiveStop := make(chan struct{})
			archiveTicker := time.NewTicker(6 * time.Hour)
			go func() {
				// Run immediately on startup
				if n, err := session.ArchiveSweep(sessions, sessionIndex, archiveAfter); err != nil {
					log.Warnf("main", "archive sweep: %v", err)
				} else if n > 0 {
					log.Infof("main", "archive sweep: archived %d idle session(s)", n)
				}
				for {
					select {
					case <-archiveTicker.C:
						if n, err := session.ArchiveSweep(sessions, sessionIndex, archiveAfter); err != nil {
							log.Warnf("main", "archive sweep: %v", err)
						} else if n > 0 {
							log.Infof("main", "archive sweep: archived %d idle session(s)", n)
						}
					case <-archiveStop:
						return
					}
				}
			}()
			defer func() {
				archiveTicker.Stop()
				close(archiveStop)
			}()
		}
	}

	// Shared: State persistence (JSON file in data dir)
	statePath := cfg.DataPath("state.json")
	stateStore := state.New(statePath)
	if err := stateStore.Load(); err != nil {
		log.Errorf("main", "load state: %v", err)
	}

	// ========== Memory system ==========
	mem := initMemorySystem(cfg)
	defer mem.cleanup()

	// Shared: Tool call detail persistence (for show_tool_calls=full inline keyboard expansion)
	toolDetailDbPath := cfg.DataPath("tool_details.db")
	toolDetailStore, err := telegram.NewToolDetailStore(toolDetailDbPath)
	if err != nil {
		log.Errorf("main", "create tool detail store: %v (inline keyboard expansion will not persist)", err)
	} else {
		// Expire old entries and vacuum on startup (cleanup from previous run)
		toolDetailStore.ExpireAndVacuum()
		defer func() {
			toolDetailStore.ExpireAndVacuum()
			_ = toolDetailStore.Close()
		}()
	}

	// Shared: Voice providers
	groqKey, _ := store.Get("groq.api_key")
	openrouterKey, _ := store.Get("openrouter.api_key")
	braveKey, _ := store.Get("brave.api_key")
	sttProvider, ttsProvider := initVoice(cfg, groqKey, openrouterKey)

	startTime := time.Now()

	// Bot manager — owns all Telegram bots
	botMgr := telegram.NewBotManager()

	// ========== Per-agent setup ==========
	agents := make(map[string]*agentInstance, len(cfg.Agents))
	var agentOrder []string // preserve config order

	// Closure for resolving agent instances by ID — resolved at call time.
	agentResolverFn := func(agentID string) *agentInstance {
		return agents[agentID]
	}

	// Closure for /agents command — captures agents/agentOrder, resolved at call time.
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
		// Resolve memory backends: per-agent (if configured) or shared
		var agentBackends map[string]memory.Searcher
		if ab, ok := mem.agentBackends[acfg.ID]; ok {
			agentBackends = ab
		} else {
			agentBackends = mem.sharedBackends
		}

		// Resolve per-agent provider client from endpoint:model
		endpoint, modelID := config.ParseModel(acfg.Model)
		agentClient := resolveEndpointClient(endpoint, modelID)
		if agentClient == nil {
			log.Errorf("main", "agent %q: endpoint %q unavailable for model %q", acfg.ID, endpoint, modelID)
			continue
		}

		inst := setupAgent(setupParams{
			acfg:            acfg,
			cfg:             cfg,
			configPath:      configPath,
			client:          agentClient,
			getClient:       getClient,
			peekClient:      peekClient,
			resolveEndpointClient: resolveEndpointClient,
			sessions:        sessions,
			store:           store,
			bwStore:         bwStore,
			stateStore:      stateStore,
			memBackends:     agentBackends,
			reminderStore:   mem.reminderStore,
			scratchpadStore: mem.scratchpadStore,
			todoStore:       mem.todoStore,
			toolDetailStore: toolDetailStore,
			sessionIndex:    sessionIndex,
			sttProvider:     sttProvider,
			ttsProvider:     ttsProvider,
			braveKey:        braveKey,
			usageClient:     usageClient,
			botMgr:          botMgr,
			startTime:       startTime,
			ctx:             ctx,
			agentListFn:     agentListFn,
			agentResolverFn: agentResolverFn,
		})
		agents[acfg.ID] = inst
		agentOrder = append(agentOrder, acfg.ID)

		// Keepalive & background work runner (per-agent config, falls back to global).
		// Non-Anthropic agents skip keepalive (Anthropic ephemeral cache warming) — Gemini's
		// CacheManager handles its own TTL extension, OpenAI has no cache. Background/memory/warnings still run.
		kaEnabled := acfg.Keepalive.Enabled && endpoint == "anthropic"
		if kaEnabled || acfg.Background.Enabled || hasMemoryFormation(acfg.MemoryFormation) || acfg.InjectAgentWarnings {
			kaOrientPrompt := resolveOrientPath(acfg.BranchOrientationHeadlessPrompt, cfg.Sessions.BranchOrientationHeadlessPrompt, acfg.BranchOrientationPrompt, cfg.Sessions.BranchOrientationPrompt)
			branchFn := buildBranchFunc(
				acfg.ID, inst.ag, sessions, inst.defaultSessionKey,
				func(branchKey, parentKey, branchType string) string {
					return buildBranchOrientation(kaOrientPrompt, branchKey, parentKey, branchType, false, inst.promptSearchDirs)
				},
				ctx,
			)

			// Mana monitor for background work throttling
			manaStaleness, err := time.ParseDuration(acfg.Background.ManaStalenessTimeout)
			if err != nil || manaStaleness <= 0 {
				manaStaleness = 10 * time.Minute
			}
			manaMonitor := mana.NewMonitor(usageClient, manaStaleness)

			// Proactive warning dispatcher
			var warningDispatcher *warnings.Dispatcher
			if acfg.InjectAgentWarnings {
				warningActiveInterval, _ := time.ParseDuration(cfg.Logging.WarningProactiveActiveInterval)
				warningInactiveInterval, _ := time.ParseDuration(cfg.Logging.WarningProactiveInactiveInterval)
				warningActivityThreshold, _ := time.ParseDuration(cfg.Logging.WarningProactiveActivityThreshold)

				agentID := acfg.ID
				agentInst := inst
				warningDispatcher = warnings.NewDispatcher(warnings.DispatcherConfig{
					Queue: inst.ag.Warnings,
					FormatFn: func(body string) string {
						return prompts.FormatInjectedMessage("PROACTIVE WARNINGS", time.Now(), body)
					},
					DispatchFn: func(warningText string) {
						sk := agentInst.defaultSessionKey()
						if sk == "" {
							log.Warnf("keepalive", "no default session for proactive warning dispatch on agent %q", agentID)
							return
						}
						resp, err := agentInst.ag.HandleMessage(agent.WithTrigger(ctx, "proactive_warning"), sk, warningText)
						if err != nil {
							log.Errorf("keepalive", "proactive warning turn error: %v", err)
							return
						}
						if resp == "" {
							return
						}
						if bot := botMgr.BotForSessionOrPrimary(sk, agentID); bot != nil {
							if err := bot.SendText(resp); err != nil {
								log.Errorf("keepalive", "proactive warning telegram delivery: %v", err)
							}
						}
					},
					ActiveInterval:    warningActiveInterval,
					InactiveInterval:  warningInactiveInterval,
					ActivityThreshold: warningActivityThreshold,
					LastUserMessageTimeFn: func() time.Time {
						sk := agentInst.defaultSessionKey()
						if sk == "" {
							return time.Time{}
						}
						return agentInst.ag.LastUserMessageTime(sk)
					},
				})
			}

			kaCfg := acfg.Keepalive
			kaCfg.Enabled = kaEnabled
			inst.kaRunner = keepalive.New(keepalive.RunnerConfig{
				AgentID:           acfg.ID,
				Keepalive:         kaCfg,
				Background:        acfg.Background,
				MemoryFormation:   acfg.MemoryFormation,
				PromptSearchDirs:  inst.promptSearchDirs,
				TodoStore:         mem.todoStore,
				StateStore:        stateStore,
				BranchFunc:        branchFn,
				ManaMonitor:       manaMonitor,
				WarningDispatcher: warningDispatcher,
			})
			inst.kaRunner.Start(ctx)

			// Wire Telegram bot callbacks to keepalive runner
			if bot := botMgr.PrimaryBot(acfg.ID); bot != nil {
				runner := inst.kaRunner
				bot.OnUserMessage = func() {
					runner.NotifyInteraction()
				}
				bot.OnTurnComplete = func() {
					runner.NotifyCacheWarmed()
				}
			}

			log.Infof("main", "agent %q keepalive runner started (ka=%v bg=%v)", acfg.ID, acfg.Keepalive.Enabled, acfg.Background.Enabled)
		}

		log.Infof("main", "agent %q ready (model=%s, workspace=%s)", acfg.ID, acfg.Model, acfg.Workspace)
	}

	// Shared multiball pool — fallback bots available to any agent.
	// Created after all agents so we can use the first agent's instance for initial binding.
	// Bots are re-wired to the acquiring agent at fork time via SetAgentAndCommands.
	if len(cfg.Telegram.MultiballBots) > 0 && len(agentOrder) > 0 {
		firstInst := agents[agentOrder[0]]
		for _, botName := range cfg.Telegram.MultiballBots {
			mbToken := config.ResolveBotToken(botName, "", store)
			if mbToken == "" {
				log.Errorf("main", "shared multiball bot %q: token not found", botName)
				continue
			}
			mbBot, err := telegram.NewBot(mbToken, cfg.Telegram.AllowedUsers,
				firstInst.ag, firstInst.cmds, command.NewLastMessageStore(), "")
			if err != nil {
				log.Errorf("main", "shared multiball bot %q: create: %v", botName, err)
				continue
			}
			configureMultiballBot(mbBot, multiballBotConfig{
				sttProvider:     sttProvider,
				ttsProvider:     ttsProvider,
				stopAliases:     cfg.Telegram.StopAliases,
				enableStopAlias: cfg.Telegram.EnableStopAliases,
				acfg:            firstInst.agentCfg,
				cfg:             cfg,
				toolDetailStore: toolDetailStore,
				stateStore:      stateStore,
			})
			botMgr.AddSharedMultiball(mbBot)
		}
		if pool := botMgr.SharedPool(); pool != nil && pool.Size() > 0 {
			ttl, _ := time.ParseDuration(cfg.Telegram.MultiballSessionTTL)
			if ttl > 0 {
				pool.SetSessionTTL(ttl, sessions)
			}
			pool.ReclaimHook = func(sessionKey string) {
				// Determine agent from session key for session-end memory
				for _, id := range agentOrder {
					inst := agents[id]
					prefix := "agent:" + id + ":"
					if strings.HasPrefix(sessionKey, prefix) {
						orientPath := resolveOrientPath(inst.agentCfg.BranchOrientationHeadlessPrompt, cfg.Sessions.BranchOrientationHeadlessPrompt, inst.agentCfg.BranchOrientationPrompt, cfg.Sessions.BranchOrientationPrompt)
						fireSessionEndMemory(inst.ag, sessions, sessionKey, inst.agentCfg.MemoryFormation, func(bk, pk, bt string) string {
							return buildBranchOrientation(orientPath, bk, pk, bt, false, inst.promptSearchDirs)
						}, inst.promptSearchDirs, ctx)
						return
					}
				}
			}
			log.Infof("main", "%d shared multiball bots ready", pool.Size())
		}
	}

	// Wire log warnings into agent sessions (any agent with inject_agent_warnings)
	{
		anyInjection := false
		for _, acfg := range cfg.Agents {
			if acfg.InjectAgentWarnings {
				anyInjection = true
				break
			}
		}
		if anyInjection {
			log.SetWarnHook(func(level log.Level, component string, msg string) {
				for _, inst := range agents {
					if inst.ag.Warnings != nil {
						inst.ag.Warnings.Push(level.String(), component, msg)
					}
				}
			})
			log.Infof("main", "warning injection into agent sessions enabled")
		}
	}

	// Tmux memory monitor — checks RSS of tmux server, notifies/kills at thresholds
	hasTmux := false
	if _, err := exec.LookPath("tmux"); err == nil {
		hasTmux = true
	}
	if hasTmux && cfg.Tools.TmuxMemoryCheckInterval != "0" {
		checkInterval, _ := time.ParseDuration(cfg.Tools.TmuxMemoryCheckInterval)
		if checkInterval > 0 {
			tmuxMemMon := tools.NewTmuxMemoryMonitor(
				tools.TmuxMemoryConfig{
					CheckInterval: checkInterval,
					WarnStr:       cfg.Tools.TmuxMemoryWarn,
					CriticalStr:   cfg.Tools.TmuxMemoryCritical,
					KillStr:       cfg.Tools.TmuxMemoryKill,
				},
				// Notification callback: send to agents without inject_agent_warnings
				func(msg string) {
					for _, id := range agentOrder {
						inst := agents[id]
						if inst.agentCfg.InjectAgentWarnings {
							continue // agent sees warnings via injection
						}
						if bot := botMgr.PrimaryBot(id); bot != nil {
							bot.SendNotification(msg)
						}
					}
				},
				// Cleanup callback: clear all tmux tool instances
				func() {
					for _, id := range agentOrder {
						if fn := agents[id].tmuxClearAll; fn != nil {
							fn()
						}
					}
				},
			)
			tmuxMemMon.Start(ctx)
			defer tmuxMemMon.Stop()
		}
	}

	// System memory guard — monitors total RSS for foci user, warns/kills under pressure
	if cfg.Resources.MemoryGuardEnabled {
		guardInterval, _ := time.ParseDuration(cfg.Resources.MemoryGuardInterval)
		if guardInterval > 0 {
			memGuard := resources.NewMemoryGuard(
				resources.MemoryGuardConfig{
					Interval:          guardInterval,
					WarnPercent:       cfg.Resources.MemoryWarnPercent,
					KillPercent:       cfg.Resources.MemoryKillPercent,
					PressureThreshold: cfg.Resources.MemoryPressureThreshold,
				},
				// Warning callback: push to all agents with warning injection enabled
				func(msg string) {
					for _, inst := range agents {
						if inst.ag.Warnings != nil {
							inst.ag.Warnings.Push("WARN", "memory_guard", msg)
						}
					}
				},
			)
			memGuard.Start(ctx)
			defer memGuard.Stop()
		}
	}

	// Periodic tool detail cleanup — expire old entries when all users are idle
	if toolDetailStore != nil {
		go func() {
			ticker := time.NewTicker(10 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					// Only run cleanup when all users are idle (>10min since last message)
					allIdle := true
					for _, id := range agentOrder {
						inst := agents[id]
						sk := inst.defaultSessionKey()
						if sk == "" {
							continue
						}
						if t := inst.ag.LastUserMessageTime(sk); !t.IsZero() && time.Since(t) < 10*time.Minute {
							allIdle = false
							break
						}
					}
					if allIdle {
						toolDetailStore.ExpireAndVacuum()
					}
				}
			}
		}()
	}

	// Intercept SIGINT/SIGTERM before starting bots.
	// Must be registered before any goroutine that could trigger a signal
	// (e.g. /restart via Telegram), otherwise Go's default handler
	// terminates the process with no graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Restore multiball sessions from persisted state.
	// For each secondary bot, check if a saved session key exists and the session
	// file is still active. If so, restore the session key and re-wire the agent.
	if stateStore != nil {
		restoreMultiballSessions(botMgr, stateStore, sessions, agents, agentOrder, cfg)
	}

	// Start all bots
	botMgr.StartAll(ctx)

	// Diagnose restart type (clean/crash/reboot) and gather diagnostics
	var diagnosis *startup.DiagnosisResult
	logsDir := filepath.Dir(cfg.Logging.EventFile)
	if logsDir == "" || logsDir == "." {
		if home, err := os.UserHomeDir(); err == nil {
			logsDir = filepath.Join(home, "logs")
		}
	}
	diagnosis = startup.DiagnoseRestart(stateStore, startTime, logsDir)
	if diagnosis.Class != startup.ClassClean && diagnosis.Class != startup.ClassUnknown {
		log.Infof("startup", "restart classified as %s: %s", diagnosis.Class, diagnosis.Summary)
	}

	// Send startup notifications to users via Telegram
	// Per-agent startup_notification overrides global enable_startup_notify
	for _, id := range agentOrder {
		inst := agents[id]
		enabled := cfg.Telegram.EnableStartupNotify // global default
		if inst.agentCfg.StartupNotification != nil {
			enabled = *inst.agentCfg.StartupNotification // per-agent override
		}
		if enabled {
			if bot := botMgr.PrimaryBot(id); bot != nil {
				bot.SendStartupNotificationWithDiagnosis(id, diagnosis)
			}
		}
	}

	// ========== HTTP server ==========
	// Build credential reload function (only for static token auth, not OAuth fallback).
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
		stateStore:        stateStore,
		sessions:          sessions,
		botMgr:            botMgr,
		cfg:               cfg,
		ctx:               ctx,
		sttProvider:       sttProvider,
		ttsProvider:       ttsProvider,
		reloadCredentials: reloadCreds,
	})

	addr := fmt.Sprintf("%s:%d", cfg.HTTP.Bind, cfg.HTTP.Port)
	var httpServer *http.Server
	var httpMu sync.Mutex
	go func() {
		for ctx.Err() == nil {
			srv := &http.Server{Addr: addr, Handler: authMiddleware(httpAPIKey, mux)}
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

	// Check for welcome file (written by setup.sh on update)
	// Returns the changelog content if injected, empty string otherwise.
	if content := injectWelcomeFile(cfg.WelcomeFile, agents, agentOrder, sessions); content != "" {
		// Fire an agent turn so the changelog is processed immediately,
		// rather than waiting for the next user message.
		inst := agents[agentOrder[0]]
		go func() {
			sk := inst.defaultSessionKey()
			if sk == "" {
				log.Warnf("main", "no default session for welcome file injection, skipping")
				return
			}
			restartCtx := agent.WithTrigger(ctx, "restart")
			inst.ag.SetSessionNoCompact(sk, true)
			msg := prompts.FormatInjectedMessage("SYSTEM UPDATE", time.Now(), content)
			if _, err := inst.ag.HandleMessage(restartCtx, sk, msg); err != nil {
				log.Errorf("main", "restart turn failed: %v", err)
			}
		}()
	}

	// Check for first-run onboarding — inject prompt for new agents
	for _, agentID := range agentOrder {
		inst := agents[agentID]
		if msg := checkFirstRun(stateStore, inst.agentCfg); msg != "" {
			agentID := agentID // capture for goroutine
			go func() {
				sk := inst.defaultSessionKey()
				if sk == "" {
					// On first run, no Telegram message has arrived yet so
					// there's no default session. Construct one from the
					// first allowed user ID in config.
					users := inst.agentCfg.AllowedUsers
					if len(users) == 0 {
						users = cfg.Telegram.AllowedUsers
					}
					if len(users) > 0 {
						if chatID, err := strconv.ParseInt(users[0], 10, 64); err == nil {
							sk = telegram.SessionKeyForChat(agentID, chatID)
						}
					}
				}
				if sk == "" {
					log.Warnf("main", "no default session for first-run injection on %s, skipping", agentID)
					return
				}
				firstRunCtx := agent.WithTrigger(ctx, "first_run")
				if _, err := inst.ag.HandleMessage(firstRunCtx, sk, msg); err != nil {
					log.Errorf("main", "first-run turn for %s failed: %v", agentID, err)
					return
				}
				// Mark first-run as completed after one successful turn
				if err := stateStore.Set("agent:"+agentID+":first_run_completed", true); err != nil {
					log.Errorf("main", "set first_run_completed for %s: %v", agentID, err)
				}
				log.Infof("main", "first-run onboarding completed for %s", agentID)
			}()
		}
	}

	// Wait for signal
	<-sigCh

	// Record clean shutdown immediately — before graceful shutdown wait.
	// If a second SIGTERM arrives during graceful shutdown (e.g. systemd
	// TimeoutStopSec), Go's default handler kills the process. Recording
	// now ensures the next startup sees this as a clean shutdown.
	if err := startup.RecordCleanShutdown(stateStore); err != nil {
		log.Warnf("main", "record clean shutdown: %v", err)
	}

	log.Infof("main", "shutting down...")

	// Stop keepalive runners — prevents new timer-triggered branches
	for _, inst := range agents {
		if inst.kaRunner != nil {
			inst.kaRunner.Stop()
		}
	}

	// Close HTTP server — prevents new HTTP-triggered turns
	httpMu.Lock()
	if httpServer != nil {
		_ = httpServer.Close()
	}
	httpMu.Unlock()

	// Wait for in-flight agent turns to complete naturally.
	// The turn lock prevents new turns from starting on sessions that are
	// already processing. With HTTP closed, no new
	// turns will be initiated. We defer cancel() until AFTER this loop so
	// that in-flight turns (including exec subprocesses) aren't killed.
	shutdownTimeout, _ := time.ParseDuration(cfg.HTTP.GracefulShutdownTimeout)
	if shutdownTimeout == 0 {
		shutdownTimeout = 30 * time.Second
	}
	gracefulShutdown(agents, shutdownTimeout)

	// Close MCP managers — disconnect from MCP servers
	for _, inst := range agents {
		if inst.mcpManager != nil {
			_ = inst.mcpManager.Close()
		}
	}

	// Clean up Gemini cache (delete server-side cached content)
	if gc := peekClient("gemini", "gemini"); gc != nil {
		if gcTyped, ok := gc.(*gemini.Client); ok {
			gcTyped.Close(ctx)
		}
	}

	// Now cancel the context — stops Telegram bots and cleans up goroutines
	cancel()

	// Wait for Telegram bots to finish cleanup (ack processed updates)
	botMgr.Wait()
}
