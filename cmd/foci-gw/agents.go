package main

import (
	"context"
	"os"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/backend"
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

	// Backend agents take a completely separate setup path.
	if acfg.Backend != "" && acfg.Backend != "api" {
		if !backend.IsRegistered(acfg.Backend) {
			log.Errorf("agent/"+acfg.ID, "backend %q not registered (missing blank import?)", acfg.Backend)
			return nil
		}
		return setupBackendAgent(p, acfg.Backend, acfg.BackendConfig)
	}

	// --- Shared preamble ---
	shared := resolveSharedSetup(p)
	p = shared.p // p.resolved is now set
	groupResolver := shared.groupResolver
	promptSearchDirs := shared.promptSearchDirs

	gc := p.resolved.Groups

	// Resolve agent's primary model via the chat call site
	primaryResolved := groupResolver.ResolveCall(config.CallChat)
	if primaryResolved == nil {
		log.Errorf("agent/"+acfg.ID, "cannot resolve chat model (agent skipped)")
		return nil
	}
	defaultEndpoint := primaryResolved.Endpoint
	defaultFormat := primaryResolved.Format
	resolvedModel := primaryResolved.Developer + "/" + primaryResolved.ModelID
	primaryCacheStrategy := primaryResolved.CacheStrategy
	if primaryCacheStrategy == "" {
		primaryCacheStrategy = "auto"
	}

	// Resolve the API client for this agent's endpoint+format
	client := p.clientProvider.GetClient(defaultEndpoint, defaultFormat)
	if client == nil {
		log.Errorf("agent/"+acfg.ID, "endpoint %q unavailable for model %q (format: %s)", defaultEndpoint, primaryResolved.ModelID, defaultFormat)
		return nil
	}

	// Create fallback resolver for automatic model failover
	fallbackResolver := config.NewFallbackResolver(gc.Fallbacks, nil, p.cfg.Models)

	// Build provider-level fallback function from config resolver.
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

	connMgr := p.connMgr

	// Declare ag early so closures (tmux wake, etc.) can capture it.
	var ag *agent.Agent
	agLazy := func() *agent.Agent { return ag }

	// Per-agent tool registry and supporting services
	registry := tools.NewRegistry()
	notifier := newAsyncNotifier(agLazy, acfg.ID, p.ctx, connMgr)
	agentStore := p.store.ForAgent(acfg.ID)

	// Register tools by category
	coreResult := registerCoreTools(registry, p, client, agentStore, notifier, groupResolver, fallbackFn)
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
	if p.resolved.Environment.Enabled {
		crontabCount := countCrontabJobs()
		envBlock = buildEnvironmentBlock(acfg, p.configPath, p.cfg, p.resolved, crontabCount, p.plat.ActivePlatformNames())
	}

	// Per-agent agent struct — start with shared fields, add API-specific ones.
	al := p.resolved.Loop
	sc := p.resolved.Summary
	cpc := p.resolved.Compaction
	bc := p.resolved.Behavior
	mdFn := modelDefaultsFn(p.cfg.Models)

	ag = shared.newAgent()
	ag.Client = client
	ag.ClientProvider = p.clientProvider
	ag.Tools = registry
	ag.ServerTools = serverTools
	ag.EnvironmentBlock = envBlock
	ag.Bootstrap = bs.bootstrap
	ag.Compactor = compactor
	ag.AsyncNotifier = notifier
	ag.Model = resolvedModel
	ag.Format = defaultFormat
	ag.Endpoint = defaultEndpoint
	ag.ExtraSystemBlocks = bs.extraSystemBlocks
	ag.CacheStrategy = primaryCacheStrategy
	ag.CacheBustDetect = p.resolved.Debug.CacheBustDetect
	ag.CacheBustIdleThreshold = time.Duration(p.resolved.Debug.CacheBustIdleMinutes) * time.Minute
	ag.DuplicateMessages = al.DuplicateMessages
	ag.BatchPartialAssistantMessages = al.BatchPartialAssistantMessages
	ag.BatchPartialJoiner = al.BatchPartialJoiner
	ag.MaxResultChars = sc.MaxResultChars
	ag.ToolResultTempDir = p.cfg.Tools.TempDir
	ag.GroupResolver = groupResolver
	ag.FallbackFunc = fallbackFn
	ag.SummaryContextTurns = sc.SummaryContextTurns
	ag.SummaryContextChars = sc.SummaryContextChars
	ag.MaxSummaryChars = sc.MaxSummaryChars
	ag.MaxSummaryInputChars = sc.MaxSummaryInputChars
	ag.MaxImagePixels = sc.MaxImagePixels
	ag.AutoSummarise = sc.AutoSummarise
	ag.UsageClient = p.usageClientProvider.GetUsageClient(defaultEndpoint)
	ag.UsageClientProvider = p.usageClientProvider
	ag.CompactionSummaryPromptPath = cpc.CompactionSummaryPrompt
	ag.CompactionHandoffMsg = cpc.CompactionHandoffMsg
	ag.AutocompactBeforeManaRefresh = cpc.AutocompactBeforeManaRefresh
	ag.AutocompactBeforeManaRefreshThreshold = cpc.AutocompactBeforeManaRefreshThreshold
	ag.AutocompactBeforeManaRefreshFactor = cpc.AutocompactBeforeManaRefreshFactor
	ag.AutocompactBeforeManaRefreshPreserve = cpc.AutocompactBeforeManaRefreshPreserve
	ag.AutocompactBeforeManaRefreshPreservePct = cpc.AutocompactBeforeManaRefreshPreservePct
	ag.MaxToolLoops = al.MaxToolLoops
	ag.MaxOutputTokens = al.MaxOutputTokens
	ag.TurnLockWarnThreshold = parseDurationDefault(bc.TurnLockWarnThreshold, 0)
	ag.Streaming = p.resolved.Display.Streaming
	ag.ModelDefaultsFn = mdFn
	ag.ManaInvestInterval = parseDurationDefault(p.resolved.Mana.InvestInterval, 0)

	// Pre-compaction memory formation hook
	compactMemOrientPath := config.DerefStr(config.First(acfg.Sessions.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt))
	compactMemMfCfg := acfg.MemoryFormation
	compactMemSearchDirs := promptSearchDirs
	compactMemOrientTemplate := prompts.ResolveOrientationTemplate(compactMemOrientPath, false, compactMemSearchDirs...)
	ag.CompactionMemoryFunc.Add(func(sessionKey string) {
		agent.FireCompactionMemory(ag, p.sessions, sessionKey, compactMemMfCfg, compactMemOrientTemplate, compactMemSearchDirs, p.ctx)
	})

	// Post-creation agent configuration (API-specific)
	setupRedaction(ag, p, agentStore)
	setupWarningQueue(ag, p.resolved, p.cfg)
	setupManaWatcher(ag, p)

	// Spawn and wake tools (registered after agent creation for lazy capture)
	registerSpawnTool(registry, p, client, bs.bootstrap, func() tools.SpawnAgent { return ag }, notifier, promptSearchDirs, func(sk string, v bool) { ag.SetSessionNoCompact(sk, v) }, groupResolver, resolvedModel, defaultFormat, fallbackFn)
	setupWakeScheduler(agLazy, registry, p.reminderStore, acfg.ID, p.ctx, p.connMgr)

	// --- Shared postamble ---
	return shared.finalize(ag, finalizeParams{
		bootstrap:           bs.bootstrap,
		registry:            registry,
		skillRegistry:       bs.skillRegistry,
		serverTools:         serverTools,
		client:              client,
		clientProvider:      p.clientProvider,
		usageClientProvider: p.usageClientProvider,
		fallbackFn:          fallbackFn,
		compactionThreshold: compactionThreshold,
		tmuxTool:            coreResult.tmuxTool,
		tmuxClearAll:        coreResult.tmuxClearAll,
		tmuxWatchCount:      coreResult.tmuxWatchCount,
		tmuxMigrateKey:      coreResult.tmuxMigrateKey,
		ttsRepls:            ttsRepls,
		mcpManager:          mcpMgr,
		skillsDirs:          bs.skillsDirs,
	})
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
