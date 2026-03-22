package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/log"
	mcpkg "foci/internal/mcp"
	"foci/internal/memory"
	"foci/internal/periodic"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/secrets"
	"foci/internal/secrets/bitwarden"
	"foci/internal/session"

	"foci/internal/tools"
	"foci/internal/voice"
	"foci/internal/workspace"
	"foci/shared/prompts"
)

// agentInstance holds all per-agent state.
type agentInstance struct {
	id                string
	ag                *agent.Agent
	cmds              *command.Registry
	cc                command.CommandContext
	registry          *tools.Registry
	bootstrap         *workspace.Bootstrap
	defaultSessionKey func() string // resolves current default session key
	agentCfg          config.AgentConfig
	resolved          *config.ResolvedAgentConfig // pre-merged agent+global config
	promptSearchDirs  []string         // directories to search for prompt files
	tmuxClearAll      func()               // clears tmux tool state (watches, owned sessions)
	tmuxWatchCount    func() int           // returns number of active tmux watches
	tmuxMigrateKey    func(string, string) // updates tmux owned/watched maps on session key rotation
	webhooks          map[string]string // hook ID → prompt path (merged from global + per-agent)
	kaRunner          *periodic.Runner  // keepalive & background work timer (nil if disabled)
	mcpManager        *mcpkg.Manager    // nil if no MCP servers configured
}

// setupParams holds the shared resources needed by each agent.
type setupParams struct {
	acfg                config.AgentConfig
	cfg                 *config.Config
	resolved            *config.ResolvedAgentConfig
	configPath          string
	client              provider.Client
	clientProvider      provider.ClientProvider
	usageClientProvider provider.UsageClientProvider
	sessions            *session.Store
	store               *secrets.Store
	bwStore             *bitwarden.Store
	memBackends         map[string]memory.Searcher
	convReader          *memory.ConversationReader
	reminderStore       *memory.ReminderStore
	scratchpadStore     *memory.Scratchpad
	todoStore           *memory.TodoStore
	taskListStore       *memory.TaskListStore
	sessionIndex        *session.SessionIndex
	ttsMap              map[string]voice.TTS
	sttMap              map[string]voice.STT
	braveKey     string
	gwSocketPath string // Unix socket path for same-user CLI auth (injected into child env as FOCI_GW_SOCK)

	startTime       time.Time
	ctx             context.Context
	agentListFn     func() []command.AgentInfo
	agentResolverFn func(agentID string) *agentInstance
	connMgr         platform.ConnectionManager
	plat            *platform.Messaging
}

// setupAgent wires up a single agent with its own tools, commands, bootstrap, and bot.
func setupAgent(p setupParams) *agentInstance {
	acfg := p.acfg

	// Pre-resolve the 2-layer agent→global config cascade once.
	p.resolved = config.Resolve(p.cfg, acfg)

	gc := p.resolved.Groups

	// Create group resolver for multi-model routing (powerful model is the agent's primary)
	groupResolver := config.NewGroupResolver(gc, p.cfg.Models)

	// Resolve agent's default endpoint and format from powerful group
	powerfulResolved := groupResolver.ResolveGroup(config.GroupPowerful)
	var defaultEndpoint, defaultFormat, resolvedModel string
	if powerfulResolved != nil {
		defaultEndpoint = powerfulResolved.Endpoint
		defaultFormat = powerfulResolved.Format
		resolvedModel = powerfulResolved.Developer + "/" + powerfulResolved.ModelID
	}

	// Create fallback resolver for automatic model failover
	fallbackResolver := config.NewFallbackResolver(gc.Fallbacks, nil, p.cfg.Models)

	// Build provider-level fallback function from config resolver.
	// This bridges config (which doesn't import provider) to the provider package.
	var fallbackFn provider.FallbackFunc
	if fallbackResolver != nil {
		fallbackFn = func(model string) (string, string, string, bool) {
			rm := fallbackResolver.Resolve(model)
			if rm == nil {
				return "", "", "", false
			}
			return rm.Developer + "/" + rm.ModelID, rm.Endpoint, rm.Format, true
		}
	}

	// Prompt search directories: agent workspace first, then shared.
	promptSearchDirs := []string{
		filepath.Join(acfg.Workspace, "prompts"),
		filepath.Join(filepath.Dir(acfg.Workspace), "shared", "prompts"),
	}

	// Default session key resolver — returns the session key for the agent's default chat.
	// Before any platform message arrives, this returns "" (no default set).
	// After the first message, it returns {id}/c{chatID}/{versionTS}.
	// The resolver is set to use the primary connection's DefaultSessionKey once wired.
	var defaultSessionKeyFn func() string
	defaultSessionKey := func() string {
		if defaultSessionKeyFn != nil {
			return defaultSessionKeyFn()
		}
		return ""
	}

	connMgr := p.connMgr

	// Declare ag early so closures (tmux wake, etc.) can capture it.
	// Assigned later in this function.
	var ag *agent.Agent
	agLazy := func() *agent.Agent { return ag }

	// Per-agent tool registry and supporting services
	registry := tools.NewRegistry()
	notifier := newAsyncNotifier(agLazy, defaultSessionKey, acfg.ID, p.ctx, connMgr)
	agentStore := p.store.ForAgent(acfg.ID)

	// Register tools by category
	coreResult := registerCoreTools(registry, p, agentStore, notifier, groupResolver, fallbackFn)
	serverTools := registerWebTools(registry, p)
	mcpMgr := registerMemoryAndExtTools(registry, p, agLazy)

	// Bootstrap and skills
	bs := setupBootstrapAndSkills(p, agentStore)

	// Compaction
	compactor, compactionThreshold := buildCompactor(p, fallbackFn)

	// Session messaging tools (send_to_chat, send_to_session)
	_, ttsRepls := registerSessionTools(registry, p, connMgr, notifier)

	// Per-agent environment block
	var envBlock string
	if config.DerefBool(p.cfg.Environment.Enabled) {
		crontabCount := countCrontabJobs()
		envBlock = buildEnvironmentBlock(acfg, p.configPath, p.cfg, p.resolved, crontabCount, p.plat.ActivePlatformNames())
	}

	// Per-agent agent struct — read from pre-resolved config.
	al := p.resolved.Loop
	sc := p.resolved.Summary
	cpc := p.resolved.Compaction
	bc := p.resolved.Behavior

	ag = &agent.Agent{
		Log:                            log.NewComponentLogger("agent/" + acfg.ID),
		Client:                         p.client,
		ClientProvider:                 p.clientProvider,
		Sessions:                       p.sessions,
		Tools:                          registry,
		ServerTools:                    serverTools,
		EnvironmentBlock:               envBlock,
		Bootstrap:                      bs.bootstrap,
		Compactor:                      compactor,
		AsyncNotifier:                  notifier,
		Reminders:                      p.reminderStore,
		TaskListStore:                  p.taskListStore,
		TodoStore:                      p.todoStore,
		ScratchpadStore:                p.scratchpadStore,
		DefaultSessionKey:              defaultSessionKey,
		AgentID:                        acfg.ID,
		Model:                          resolvedModel,
		Format:                         defaultFormat,
		Endpoint:                       defaultEndpoint,
		ExtraSystemBlocks:              bs.extraSystemBlocks,
		CacheStrategy:                  p.cfg.Cache.Strategy,
		CacheBustDetect:                p.cfg.Logging.CacheBustDetect,
		CacheBustIdleThreshold:         time.Duration(config.DerefInt(p.cfg.Logging.CacheBustIdleMinutes)) * time.Minute,
		DuplicateMessages:              al.DuplicateMessages,
		BatchPartialAssistantMessages:  al.BatchPartialAssistantMessages,
		BatchPartialJoiner:             al.BatchPartialJoiner,
		MaxResultChars:                 sc.MaxResultChars,
		ToolResultTempDir:              p.cfg.Tools.TempDir,
		GroupResolver:                  groupResolver,
		FallbackFunc:                   fallbackFn,
		SummaryContextTurns:            sc.SummaryContextTurns,
		SummaryContextChars:            sc.SummaryContextChars,
		MaxSummaryChars:                sc.MaxSummaryChars,
		MaxSummaryInputChars:           sc.MaxSummaryInputChars,
		MaxImagePixels:                 sc.MaxImagePixels,
		AutoSummarise:                  sc.AutoSummarise,
		SessionIndex:                   p.sessionIndex,
		UsageClient:                    p.usageClientProvider.GetUsageClient(defaultEndpoint),
		UsageClientProvider:            p.usageClientProvider,
		MessageTransforms:              agent.CompileTransforms(resolveMessageTransforms(acfg, p.cfg)),
		CompactionSummaryPromptPath:    cpc.CompactionSummaryPrompt,
		CompactionHandoffMsg:           cpc.CompactionHandoffMsg,
		AutocompactBeforeManaRefresh:          cpc.AutocompactBeforeManaRefresh,
		AutocompactBeforeManaRefreshThreshold: cpc.AutocompactBeforeManaRefreshThreshold,
		AutocompactBeforeManaRefreshFactor:    cpc.AutocompactBeforeManaRefreshFactor,
		AutocompactBeforeManaRefreshPreserve:    cpc.AutocompactBeforeManaRefreshPreserve,
		AutocompactBeforeManaRefreshPreservePct: cpc.AutocompactBeforeManaRefreshPreservePct,
		PromptSearchDirs:               promptSearchDirs,
		MaxToolLoops:                   al.MaxToolLoops,
		MaxOutputTokens:                al.MaxOutputTokens,
		TurnLockWarnThreshold:          parseDurationDefault(bc.TurnLockWarnThreshold, 3*time.Minute),
		ShowToolCalls:                  resolveShowToolCalls(p.resolved),
		CacheTTL:                       al.CacheTTL,
		Streaming:                      p.resolved.Display.Streaming,
		ModelDefaultsFn:                modelDefaultsFn(p.cfg.Models),
		ManaInvestInterval:             parseDurationDefault(config.DerefStr(p.cfg.Mana.InvestInterval), 30*time.Minute),
	}

	// Pre-compaction memory formation hook
	compactMemOrientPath := config.DerefStr(config.First(acfg.Sessions.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt))
	compactMemMfCfg := acfg.MemoryFormation
	compactMemSearchDirs := promptSearchDirs
	ag.CompactionMemoryFunc.Add(func(sessionKey string) {
		agent.FireCompactionMemory(ag, p.sessions, sessionKey, compactMemMfCfg, func(bk, pk, bt string) string {
			return prompts.BuildBranchOrientation(compactMemOrientPath, bk, pk, bt, false, compactMemSearchDirs)
		}, compactMemSearchDirs, p.ctx)
	})

	// Post-creation agent configuration
	setupNudgeSystem(ag, acfg, p.resolved.Nudge, defaultSessionKey, registry, bs.skillRegistry)
	setupRedaction(ag, p, agentStore)
	setupWarningQueue(ag, p.resolved, p.cfg)
	setupManaWatcher(ag, p)

	// Spawn and wake tools (registered after agent creation for lazy capture)
	registerSpawnTool(registry, p, bs.bootstrap, func() tools.SpawnAgent { return ag }, notifier, promptSearchDirs, func(sk string, v bool) { ag.SetSessionNoCompact(sk, v) }, groupResolver, defaultFormat, fallbackFn)
	setupWakeScheduler(agLazy, defaultSessionKey, registry, p.reminderStore, acfg.ID, p.ctx, p.connMgr)

	// Per-agent slash commands
	// configureFacet is set later by setupPlatform but captured
	// by the closure below, which is only called at runtime (forkFacet).
	var configureFacet func(platform.Connection)

	// displayDefaultsFn is set after platform setup — provides resolved
	// display defaults from the platform (lazy-forward pattern).
	var displayDefaultsFn func() platform.DisplaySettings

	lastMsgStore := command.NewLastMessageStore()
	cmds, cc := registerAgentCommands(cmdRegParams{
		ag:                  ag,
		acfg:                acfg,
		defaultSessionKey:   defaultSessionKey,
		bootstrap:           bs.bootstrap,
		promptSearchDirs:    promptSearchDirs,
		compactionThreshold: compactionThreshold,
		cfg:                 p.cfg,
		configPath:          p.configPath,
		sessions:            p.sessions,
		sessionIndex:        p.sessionIndex,
		client:              p.client,
		clientProvider:      p.clientProvider,
		usageClientProvider: p.usageClientProvider,
		store:               p.store,
		bwStore:             p.bwStore,
		startTime:           p.startTime,
		todoStore:           p.todoStore,
		registry:            registry,
		tmuxTool:            coreResult.tmuxTool,
		skillsDirs:          bs.skillsDirs,
		skillRegistry:       bs.skillRegistry,
		agentListFn:         p.agentListFn,
		plat:                p.plat,
		connMgr:             connMgr,
		groupResolver:       groupResolver,
		fallbackFn:          fallbackFn,
		resolved:            p.resolved,
		configureFacet: func(conn platform.Connection) {
			if configureFacet != nil {
				configureFacet(conn)
			}
		},
		displayDefaultsFn: func() platform.DisplaySettings {
			if displayDefaultsFn != nil {
				return displayDefaultsFn()
			}
			return platform.DisplaySettings{}
		},
	}, lastMsgStore)

	// Finalize tools and log
	registry.FinalizeShellDescription()
	logRegisteredTools(registry, serverTools, acfg.ID)

	// Platform connections
	if p.plat != nil {
		platResult := setupPlatformConnections(ag, p, cmds, cc, lastMsgStore, ttsRepls, promptSearchDirs, coreResult.tmuxMigrateKey)
		defaultSessionKeyFn = platResult.defaultSessionKeyFn
		configureFacet = platResult.configureFacetFn
		displayDefaultsFn = platResult.displayDefaultsFn
	}

	// Nudge: trigger initial extraction on first message.
	if ag.NudgeReloadFunc != nil {
		var nudgeInitOnce sync.Once
		ag.OnActivity.Add(func(sessionKey string) {
			nudgeInitOnce.Do(func() {
				ag.NudgeReloadFunc()
			})
		})
	}

	return &agentInstance{
		id:                acfg.ID,
		ag:                ag,
		cmds:              cmds,
		cc:                cc,
		registry:          registry,
		bootstrap:         bs.bootstrap,
		defaultSessionKey: defaultSessionKey,
		agentCfg:          acfg,
		resolved:          p.resolved,
		promptSearchDirs:  promptSearchDirs,
		webhooks:          p.resolved.Webhooks,
		tmuxClearAll:      coreResult.tmuxClearAll,
		tmuxWatchCount:    coreResult.tmuxWatchCount,
		tmuxMigrateKey:    coreResult.tmuxMigrateKey,
		mcpManager:        mcpMgr,
	}
}

// checkFirstRun determines whether a first-run onboarding prompt should be
// injected for an agent. Returns the prompt message if injection is needed,
// empty string otherwise. Uses session index agent_metadata to track completion.
func checkFirstRun(idx *session.SessionIndex, acfg config.AgentConfig) string {
	if idx == nil {
		return ""
	}

	// Already completed — nothing to do
	val, err := idx.GetAgentMetadata(acfg.ID, "first_run_completed")
	if err == nil && val == "true" {
		return ""
	}

	// First run — inject the onboarding prompt
	prompt := prompts.FirstRun()
	if prompt == "" {
		return ""
	}

	log.Infof("main", "agent %s: first run detected, injecting onboarding prompt", acfg.ID)
	return prompt
}

// readAndConsumeWelcomeFile checks for a welcome/changelog file written by
// setup.sh on update. If found, returns the file contents and deletes the file.
// Returns empty string if no file exists or file is empty.
func readAndConsumeWelcomeFile(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "" // file doesn't exist — normal for non-update starts
	}
	content := strings.TrimSpace(string(data))
	if err := os.Remove(path); err != nil {
		log.Warnf("main", "remove welcome file: %v", err)
	}
	if content == "" {
		return ""
	}

	log.Infof("main", "found welcome file (%d bytes)", len(content))
	return content
}

// buildServerTool constructs an anthropic server tool config map with optional
// max_uses, allowed_domains, and blocked_domains fields.
func buildServerTool(toolType, toolName string, maxUses int, allowed, blocked []string) provider.ToolDef {
	cfg := map[string]interface{}{
		"type": toolType,
		"name": toolName,
	}
	if maxUses > 0 {
		cfg["max_uses"] = maxUses
	}
	if len(allowed) > 0 {
		cfg["allowed_domains"] = allowed
	}
	if len(blocked) > 0 {
		cfg["blocked_domains"] = blocked
	}
	return provider.NewServerTool(cfg)
}

// resolveMessageTransforms merges per-agent and global message transforms.
// Agent rules override matching global rules (by Find pattern); non-matching global rules fall through.
func resolveMessageTransforms(acfg config.AgentConfig, cfg *config.Config) []config.MessageTransform {
	return config.SuperveneSlice(acfg.MessageTransforms, cfg.MessageTransforms,
		func(t config.MessageTransform) string { return t.Find })
}

// resolveBlockedPaths merges per-agent and global blocked paths.
// Agent paths override matching global paths (by Path); non-matching global paths fall through.
func resolveBlockedPaths(acfg config.AgentConfig, cfg *config.Config) []config.BlockedPath {
	return config.SuperveneSlice(acfg.BlockedPaths, cfg.BlockedPaths,
		func(b config.BlockedPath) string { return b.Path })
}

// hasMemoryFormation returns true if any memory formation feature is enabled.
// All four default to true, so returns false only when all are explicitly disabled.
func hasMemoryFormation(mf config.ResolvedMemoryFormation) bool {
	return mf.IntervalEnabled || mf.ConsolidationEnabled || mf.SessionEndEnabled || mf.CompactionEnabled
}
