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
	"foci/prompts"
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
	configPath          string
	client              provider.Client
	clientProvider      provider.ClientProvider
	usageClientProvider provider.UsageClientProvider
	sessions            *session.Store
	store               *secrets.Store
	bwStore             *bitwarden.Store
	memBackends         map[string]memory.Searcher
	reminderStore       *memory.ReminderStore
	scratchpadStore     *memory.Scratchpad
	todoStore           *memory.TodoStore
	taskListStore       *memory.TaskListStore
	sessionIndex        *session.SessionIndex
	ttsMap              map[string]voice.TTS
	sttMap              map[string]voice.STT
	braveKey            string

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

	// Resolve agent's default endpoint and format
	resolved, err := config.ResolveModel(acfg.Model, acfg.Endpoint, p.cfg.Models.Aliases)
	var defaultEndpoint, defaultFormat string
	if err == nil {
		defaultEndpoint = resolved.Endpoint
		defaultFormat = resolved.Format
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
	notifier := newAsyncNotifier(agLazy, defaultSessionKey, acfg.ID, p.ctx, p.sessions, connMgr)
	agentStore := p.store.ForAgent(acfg.ID)

	// Register tools by category
	coreResult := registerCoreTools(registry, p, agentStore, notifier)
	serverTools := registerWebTools(registry, p)
	mcpMgr := registerMemoryAndExtTools(registry, p, agLazy)

	// Bootstrap and skills
	bs := setupBootstrapAndSkills(p, agentStore)

	// Compaction
	compactor, compactionThreshold := buildCompactor(p, defaultFormat)

	// Session messaging tools (send_message_to_user, send_to_session)
	_, ttsRepls := registerSessionTools(registry, p, connMgr, notifier)

	// Per-agent environment block
	var envBlock string
	if p.cfg.Environment.Enabled {
		crontabCount := countCrontabJobs()
		envBlock = buildEnvironmentBlock(acfg, p.configPath, p.cfg, crontabCount, p.plat.ActivePlatformNames())
	}

	// Per-agent agent struct
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
		Model:                          acfg.Model,
		Format:                         defaultFormat,
		Endpoint:                       defaultEndpoint,
		ExtraSystemBlocks:              bs.extraSystemBlocks,
		CacheStrategy:                  p.cfg.Cache.Strategy,
		CacheBustDetect:                p.cfg.Logging.CacheBustDetect,
		CacheBustIdleThreshold:         time.Duration(p.cfg.Logging.CacheBustIdleMinutes) * time.Minute,
		DuplicateMessages:              acfg.DuplicateMessages,
		BatchPartialAssistantMessages:  acfg.BatchPartialAssistantMessages,
		BatchPartialJoiner:             acfg.BatchPartialJoiner,
		MaxResultChars:                 resolveInt(acfg.MaxResultChars, p.cfg.Tools.MaxResultChars),
		ToolResultTempDir:              p.cfg.Tools.TempDir,
		ModelAliases:                   p.cfg.Models.Aliases,
		SummaryContextTurns:            resolveInt(acfg.SummaryContextTurns, p.cfg.Tools.SummaryContextTurns),
		SummaryContextChars:            resolveInt(acfg.SummaryContextChars, p.cfg.Tools.SummaryContextChars),
		MaxSummaryChars:                resolveInt(acfg.MaxSummaryChars, p.cfg.Tools.MaxSummaryChars),
		MaxSummaryInputChars:           resolveInt(acfg.MaxSummaryInputChars, p.cfg.Tools.MaxSummaryInputChars),
		SummaryModel:                   resolveString(acfg.SummaryModel, p.cfg.Tools.SummaryModel),
		SummaryEndpoint:                resolveString(acfg.SummaryEndpoint, p.cfg.Tools.SummaryEndpoint),
		MaxImagePixels:                 resolveInt(acfg.MaxImagePixels, p.cfg.Tools.MaxImagePixels),
		AutoSummarise:                  resolveBoolPtr(acfg.AutoSummarise, p.cfg.Tools.AutoSummarise),
		SessionIndex:                   p.sessionIndex,
		UsageClient:                    p.usageClientProvider.GetUsageClient(defaultEndpoint),
		UsageClientProvider:            p.usageClientProvider,
		MessageTransforms:              agent.CompileTransforms(resolveMessageTransforms(acfg, p.cfg)),
		CompactionSummaryPromptPath:    resolveString(acfg.CompactionSummaryPrompt, p.cfg.Sessions.CompactionSummaryPrompt),
		CompactionHandoffMsg:           resolveString(acfg.CompactionHandoffMsg, p.cfg.Sessions.CompactionHandoffMsg),
		CompactionIdleThreshold:        resolveString(acfg.CompactionIdleThreshold, p.cfg.Sessions.CompactionIdleThreshold),
		CompactionIdlePressureStart:    resolveString(acfg.CompactionIdlePressureStart, p.cfg.Sessions.CompactionIdlePressureStart),
		CompactionIdlePressureMax:      resolveFloat64Ptr(acfg.CompactionIdlePressureMax, p.cfg.Sessions.CompactionIdlePressureMax),
		CompactionManaRefreshThreshold: resolveString(acfg.CompactionManaRefreshThreshold, p.cfg.Sessions.CompactionManaRefreshThreshold),
		CompactionManaRefreshPreserve:    resolveIdlePreserve(acfg.CompactionManaRefreshPreserve, p.cfg.Sessions.CompactionManaRefreshPreserve),
		CompactionManaRefreshPreservePct: resolveFloat64PtrDefault(acfg.CompactionManaRefreshPreservePct, p.cfg.Sessions.CompactionManaRefreshPreservePct, 0.5),
		PromptSearchDirs:               promptSearchDirs,
		MaxToolLoops:                   acfg.MaxToolLoops,
		MaxOutputTokens:                acfg.MaxOutputTokens,
		BraindeadWarningThreshold:      acfg.BraindeadThreshold,
		BraindeadWarningPrompt:         acfg.BraindeadPrompt,
		TurnLockWarnThreshold:          parseDurationDefault(acfg.TurnLockWarnThreshold, 3*time.Minute),
		Effort:                         acfg.Effort,
		Thinking:                       acfg.Thinking,
		Speed:                          acfg.Speed,
		ShowToolCalls:                  resolveShowToolCalls(acfg, p.cfg),
		CacheTTL:                       resolveString(acfg.CacheTTL, resolveString(p.cfg.Defaults.CacheTTL, p.cfg.Cache.TTL)),
		Streaming:                      resolveStreamingConfig(acfg, p.cfg),
		ManaInvestInterval:             parseDurationDefault(p.cfg.Mana.InvestInterval, 30*time.Minute),
	}

	// Post-creation agent configuration
	setupNudgeSystem(ag, acfg, defaultSessionKey)
	setupRedaction(ag, p, agentStore)
	setupWarningQueue(ag, acfg, p.cfg)
	setupManaWatcher(ag, p)

	// Spawn and wake tools (registered after agent creation for lazy capture)
	registerSpawnTool(registry, p, bs.bootstrap, func() tools.SpawnAgent { return ag }, notifier, promptSearchDirs, func(sk string, v bool) { ag.SetSessionNoCompact(sk, v) })
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
		promptSearchDirs:  promptSearchDirs,
		webhooks:          mergeWebhooks(p.cfg.Defaults.Webhooks, acfg.Webhooks),
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

// injectWelcomeFile checks for a welcome/changelog file written by setup.sh
// on update. If found, returns the file contents and deletes the file.
// Returns empty string if no file exists or file is empty.
func injectWelcomeFile(path string, agents map[string]*agentInstance, agentOrder []string, sessions *session.Store) string { // nolint:unparam
	if path == "" || len(agentOrder) == 0 {
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

	log.Infof("main", "found welcome file for agent %s (%d bytes)", agentOrder[0], len(content))
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

// resolveMessageTransforms returns per-agent message transforms if set, otherwise global.
func resolveMessageTransforms(acfg config.AgentConfig, cfg *config.Config) []config.MessageTransform {
	if len(acfg.MessageTransforms) > 0 {
		return acfg.MessageTransforms
	}
	return cfg.MessageTransforms
}

// resolveBlockedPaths returns per-agent blocked paths if set, otherwise global.
func resolveBlockedPaths(acfg config.AgentConfig, cfg *config.Config) []config.BlockedPath {
	if len(acfg.BlockedPaths) > 0 {
		return acfg.BlockedPaths
	}
	return cfg.BlockedPaths
}

// hasMemoryFormation returns true if any memory formation feature is enabled.
// All three default to true (nil *bool = true), so returns false only when
// all are explicitly disabled.
func hasMemoryFormation(mf config.MemoryFormationConfig) bool {
	intervalEnabled := mf.IntervalEnabled == nil || *mf.IntervalEnabled
	consolidationEnabled := mf.ConsolidationEnabled == nil || *mf.ConsolidationEnabled
	sessionEndEnabled := mf.SessionEndEnabled == nil || *mf.SessionEndEnabled
	return intervalEnabled || consolidationEnabled || sessionEndEnabled
}
