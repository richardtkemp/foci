package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/nudge"
	mcpkg "foci/internal/mcp"
	"foci/internal/memory"
	"foci/internal/periodic"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/secrets"
	"foci/internal/secrets/bitwarden"
	"foci/internal/session"
	"foci/internal/skills"
	"foci/internal/state"
	"foci/internal/tools"
	"foci/internal/voice"
	"foci/internal/warnings"
	"foci/internal/workspace"
	"foci/prompts"
)

// agentInstance holds all per-agent state.
type agentInstance struct {
	id                string
	ag                *agent.Agent
	cmds              *command.Registry
	registry          *tools.Registry
	bootstrap         *workspace.Bootstrap
	defaultSessionKey func() string // resolves current default session key
	agentCfg          config.AgentConfig
	promptSearchDirs  []string         // directories to search for prompt files
	tmuxClearAll      func()           // clears tmux tool state (watches, owned sessions)
	tmuxWatchCount    func() int       // returns number of active tmux watches
	kaRunner          *periodic.Runner // keepalive & background work timer (nil if disabled)
	mcpManager        *mcpkg.Manager   // nil if no MCP servers configured
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
	stateStore          *state.Store
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
	// Used by ResolvePrompt when no explicit path is configured.
	promptSearchDirs := []string{
		filepath.Join(acfg.Workspace, "prompts"),
		filepath.Join(filepath.Dir(acfg.Workspace), "shared", "prompts"),
	}

	// Default session key resolver — returns the session key for the agent's default chat.
	// Before any platform message arrives, this returns "" (no default set).
	// After the first message, it returns agent:<id>:chat:<chatID>.
	// The resolver is set to use the primary connection's DefaultSessionKey once wired.
	var defaultSessionKeyFn func() string

	defaultSessionKey := func() string {
		if defaultSessionKeyFn != nil {
			return defaultSessionKeyFn()
		}
		return ""
	}

	// sessionKeyFromCtx resolves the session key from a command/tool context.
	// Priority: (1) tools.SessionKeyFromContext (set by agent tool execution),
	// (2) command.ChatIDKey via primary bot cache (stable across calls),
	// (3) defaultSessionKey fallback.
	connMgr := p.connMgr
	sessionKeyFromCtx := func(ctx context.Context) string {
		if sk := tools.SessionKeyFromContext(ctx); sk != "" {
			return sk
		}
		if chatID, ok := ctx.Value(command.ChatIDKey{}).(int64); ok && chatID != 0 {
			if conn := connMgr.Primary(acfg.ID); conn != nil {
				return conn.SessionKeyForChat(chatID)
			}
			return session.NewChatSessionKey(acfg.ID, chatID)
		}
		return defaultSessionKey()
	}

	// Declare ag early so closures (tmux wake, etc.) can capture it.
	// Assigned later in this function.
	var ag *agent.Agent

	// Per-agent tool registry
	registry := tools.NewRegistry()

	// Async notifier: delivers results from auto-backgrounded exec commands
	// and tmux watch inactivity alerts to the agent session.
	notifier := newAsyncNotifier(func() *agent.Agent { return ag }, defaultSessionKey, acfg.ID, p.ctx, p.sessions, connMgr)
	// Per-agent secrets view: agent-specific values overlay globals
	agentStore := p.store.ForAgent(acfg.ID)

	execAutoBg := resolveInt(acfg.ExecAutoBackground, p.cfg.Tools.ExecAutoBackground)
	maxUploadSize := resolveInt64(acfg.MaxUploadFileSize, p.cfg.Tools.MaxUploadFileSize)
	spillThreshold := resolveInt(acfg.MaxResultChars, p.cfg.Tools.MaxResultChars)
	registry.Register(tools.NewExecTool(agentStore, p.bwStore, execAutoBg, notifier, acfg.Workspace, registry, spillThreshold, p.cfg.Tools.TempDir))

	// Only register tmux tool if tmux is available in PATH
	var tmuxTool *tools.Tool
	var tmuxClearAll func()
	var tmuxWatchCount func() int
	if _, err := exec.LookPath("tmux"); err == nil {
		tmuxAutopilot := resolveBoolPtr(acfg.TmuxAutopilot, p.cfg.Tools.TmuxAutopilot)
		tmuxWatchThreshold := resolveString(acfg.TmuxWatchThreshold, p.cfg.Tools.TmuxWatchThreshold)
		tmuxWatchThresholdSec := 30
		if d, err := time.ParseDuration(tmuxWatchThreshold); err == nil {
			tmuxWatchThresholdSec = int(d.Seconds())
		}
		tmuxSessionTTLStr := resolveString(acfg.TmuxSessionTTL, p.cfg.Tools.TmuxSessionTTL)
		var tmuxSessionTTL time.Duration
		if tmuxSessionTTLStr != "0" {
			if d, err := time.ParseDuration(tmuxSessionTTLStr); err == nil {
				tmuxSessionTTL = d
			}
		}
		tmuxWatchCount, tmuxTool, tmuxClearAll = tools.NewTmuxTool(p.cfg.Tools.TmuxCols, p.cfg.Tools.TmuxRows, notifier, p.stateStore, "tmux:"+acfg.ID, tmuxAutopilot, tmuxWatchThresholdSec, tmuxSessionTTL)
		registry.Register(tmuxTool)
	}
	// Only register browser tool if enabled
	browserEnabled := resolveBoolPtr(acfg.BrowserEnabled, p.cfg.Tools.Browser.Enabled)
	if browserEnabled {
		browserMgr := tools.NewBrowserManager(&p.cfg.Tools.Browser)
		registry.Register(tools.NewBrowserTool(browserMgr))
	}

	blockedPaths := resolveBlockedPaths(acfg, p.cfg)
	if len(blockedPaths) > 0 {
		log.Infof("setup", "agent %s: %d blocked write/edit path(s) configured", acfg.ID, len(blockedPaths))
	}
	registry.Register(tools.NewReadTool(agentStore))
	registry.Register(tools.NewWriteTool(agentStore, blockedPaths))
	registry.Register(tools.NewEditTool(agentStore, blockedPaths))
	registry.Register(tools.NewSummaryTool(p.client, p.clientProvider, acfg.Model, p.cfg.Models.Aliases))
	registry.Register(tools.NewHTTPRequestTool(agentStore, p.bwStore, p.cfg.Tools.TempDir, execAutoBg, maxUploadSize, notifier))

	// Web search/fetch: server-side (Anthropic) or client-side (Brave/builtin) based on config.
	var serverTools []provider.ToolDef

	searchProvider := resolveString(acfg.SearchProvider, p.cfg.Tools.SearchProvider)
	if searchProvider == "anthropic" {
		serverTools = append(serverTools, buildServerTool("web_search_20250305", "web_search",
			p.cfg.Tools.WebSearchMaxUses, p.cfg.Tools.WebSearchAllowedDomains, p.cfg.Tools.WebSearchBlockedDomains))
	} else if searchProvider == "brave" && p.braveKey != "" {
		registry.Register(tools.NewWebSearchTool(p.braveKey))
	}

	fetchProvider := resolveString(acfg.FetchProvider, p.cfg.Tools.FetchProvider)
	if fetchProvider == "anthropic" {
		serverTools = append(serverTools, buildServerTool("web_fetch_20250910", "web_fetch",
			p.cfg.Tools.WebFetchMaxUses, p.cfg.Tools.WebFetchAllowedDomains, p.cfg.Tools.WebFetchBlockedDomains))
	} else {
		registry.Register(tools.NewWebFetchTool())
	}

	// Memory tools (shared stores, registered per-agent)
	if len(p.memBackends) > 0 {
		registry.Register(tools.NewMemorySearchTool(p.memBackends, p.cfg.Memory.SearchBackends))
	}
	if p.scratchpadStore != nil {
		registry.Register(tools.NewScratchpadTool(p.scratchpadStore, acfg.ID))
	}
	if p.todoStore != nil {
		registry.Register(tools.NewTodoTool(p.todoStore, acfg.ID))
	}
	if p.taskListStore != nil {
		registry.Register(tools.NewTaskListTool(p.taskListStore, acfg.ID))
	}

	// Bitwarden tools (if enabled)
	if p.bwStore != nil {
		registry.Register(tools.NewBitwardenSearchTool(p.bwStore))
		registry.Register(tools.NewBitwardenUnlockTool(p.bwStore))
	}

	// MCP servers (dynamic — re-reads mcp.toml on each tool call)
	mcpMgr := mcpkg.NewManagerForAgent(filepath.Dir(p.configPath), acfg.ID)
	if tool := mcpMgr.Tool(); tool != nil {
		registry.Register(tool)
	}

	// Per-agent workspace bootstrap
	bootstrap := workspace.NewBootstrap(acfg.Workspace, acfg.SystemFiles)
	bootstrap.SetSecretNames(agentStore.Names(), p.bwStore != nil)
	checkSystemPromptSizes(bootstrap, p.cfg.Sessions, acfg.ID)

	// Per-agent skills (per-agent dirs override global)
	skillsDirs := p.cfg.Skills.Dirs
	if len(acfg.SkillsDirs) > 0 {
		skillsDirs = acfg.SkillsDirs
	}
	skillRegistry := skills.Load(skillsDirs)
	var extraSystemBlocks []provider.SystemBlock
	if skillRegistry.Len() > 0 {
		extraSystemBlocks = []provider.SystemBlock{
			{Type: "text", Text: skillRegistry.SystemBlock(acfg.Workspace)},
		}
		log.Infof("main", "agent %q: loaded %d skills", acfg.ID, skillRegistry.Len())
	}
	maxRC := p.cfg.Tools.MaxResultChars
	if len(acfg.SkillsDirs) > 0 {
		maxRC = resolveInt(acfg.MaxResultChars, p.cfg.Tools.MaxResultChars)
	}
	checkSkillSizes(skillRegistry, maxRC, acfg.ID)

	compactionThreshold := resolveFloat64Ptr(acfg.CompactionThreshold, p.cfg.Sessions.CompactionThreshold)
	preserveMessages := resolveIntPtr(acfg.CompactionPreserveMessages, p.cfg.Sessions.CompactionPreserveMessages)
	compactor := compaction.NewCompactor(p.sessions, acfg.Model, compactionThreshold)
	compactor.WithConfig(
		p.cfg.Sessions.CompactionMaxTokens,
		p.cfg.Sessions.CompactionMinMessages,
		preserveMessages,
	)
	if acfg.CompactionEffort != "" {
		compactor.WithEffort(acfg.CompactionEffort)
	}
	compactor.WithFormat(defaultFormat)
	compactor.Scratchpad = p.scratchpadStore
	compactor.TaskListStore = p.taskListStore
	compactor.AgentID = acfg.ID

	// Per-agent send_message_to_user tool (closure captures this agent's bot)
	agentTTS := resolveTTS(p.ttsMap, p.cfg.TTS, acfg.TTS, acfg.TTSRate)
	registry.Register(tools.NewSendMessageToUserTool(func(sessionKey string) tools.MessageSender {
		conn := connMgr.ForSessionOrPrimary(sessionKey, acfg.ID)
		if conn == nil {
			return nil
		}
		return conn
	}, agentTTS))

	// send_to_session tool — inject messages into other sessions.
	sessionNotifyFn := newSessionNotifyFn(p.agentResolverFn, p.ctx, connMgr)
	registry.Register(tools.NewSendToSessionTool(p.sessions, notifier, sessionNotifyFn))

	// Per-agent environment block
	var envBlock string
	if p.cfg.Environment.Enabled {
		crontabCount := countCrontabJobs()
		envBlock = buildEnvironmentBlock(acfg, p.configPath, p.cfg, crontabCount, p.plat.ActivePlatformNames())
	}

	// Per-agent agent struct
	ag = &agent.Agent{
		Log:                           log.NewComponentLogger("agent:" + acfg.ID),
		Client:                        p.client,
		ClientProvider:                p.clientProvider,
		Sessions:                      p.sessions,
		Tools:                         registry,
		ServerTools:                   serverTools,
		EnvironmentBlock:              envBlock,
		Bootstrap:                     bootstrap,
		Compactor:                     compactor,
		AsyncNotifier:                 notifier,
		Reminders:                     p.reminderStore,
		TaskListStore:                 p.taskListStore,
		TodoStore:                     p.todoStore,
		ScratchpadStore:               p.scratchpadStore,
		DefaultSessionKey:             defaultSessionKey,
		AgentID:                       acfg.ID,
		Model:                         acfg.Model,
		Format:                        defaultFormat,
		Endpoint:                      defaultEndpoint,
		ExtraSystemBlocks:             extraSystemBlocks,
		CacheStrategy:                 p.cfg.Cache.Strategy,
		CacheBustDetect:               p.cfg.Logging.CacheBustDetect,
		CacheBustIdleThreshold:        time.Duration(p.cfg.Logging.CacheBustIdleMinutes) * time.Minute,
		DuplicateMessages:             acfg.DuplicateMessages,
		BatchPartialAssistantMessages: acfg.BatchPartialAssistantMessages,
		BatchPartialJoiner:            acfg.BatchPartialJoiner,
		MaxResultChars:                resolveInt(acfg.MaxResultChars, p.cfg.Tools.MaxResultChars),
		ToolResultTempDir:             p.cfg.Tools.TempDir,
		ModelAliases:                  p.cfg.Models.Aliases,
		SummaryContextTurns:           resolveInt(acfg.SummaryContextTurns, p.cfg.Tools.SummaryContextTurns),
		SummaryContextChars:           resolveInt(acfg.SummaryContextChars, p.cfg.Tools.SummaryContextChars),
		MaxSummaryChars:               resolveInt(acfg.MaxSummaryChars, p.cfg.Tools.MaxSummaryChars),
		MaxSummaryInputChars:          resolveInt(acfg.MaxSummaryInputChars, p.cfg.Tools.MaxSummaryInputChars),
		SummaryModel:                  resolveString(acfg.SummaryModel, p.cfg.Tools.SummaryModel),
		SummaryEndpoint:               resolveString(acfg.SummaryEndpoint, p.cfg.Tools.SummaryEndpoint),
		MaxImagePixels:                resolveInt(acfg.MaxImagePixels, p.cfg.Tools.MaxImagePixels),
		AutoSummarise:                 resolveBoolPtr(acfg.AutoSummarise, p.cfg.Tools.AutoSummarise),
		StateStore:                    p.stateStore,
		UsageClient:                   p.usageClientProvider.GetUsageClient(defaultEndpoint),
		UsageClientProvider:           p.usageClientProvider,
		MessageTransforms:             agent.CompileTransforms(resolveMessageTransforms(acfg, p.cfg)),
		CompactionSummaryPromptPath:    resolveString(acfg.CompactionSummaryPrompt, p.cfg.Sessions.CompactionSummaryPrompt),
		CompactionHandoffMsg:           resolveString(acfg.CompactionHandoffMsg, p.cfg.Sessions.CompactionHandoffMsg),
		CompactionIdleThreshold:        resolveString(acfg.CompactionIdleThreshold, p.cfg.Sessions.CompactionIdleThreshold),
		CompactionIdlePressureStart:    resolveString(acfg.CompactionIdlePressureStart, p.cfg.Sessions.CompactionIdlePressureStart),
		CompactionIdlePressureMax:      resolveFloat64Ptr(acfg.CompactionIdlePressureMax, p.cfg.Sessions.CompactionIdlePressureMax),
		CompactionManaRefreshThreshold: resolveString(acfg.CompactionManaRefreshThreshold, p.cfg.Sessions.CompactionManaRefreshThreshold),
		CompactionManaRefreshPreserve:  resolveIdlePreserve(acfg.CompactionManaRefreshPreserve, p.cfg.Sessions.CompactionManaRefreshPreserve),
		PromptSearchDirs:              promptSearchDirs,
		MaxToolLoops:                  acfg.MaxToolLoops,
		MaxOutputTokens:               acfg.MaxOutputTokens,
		BraindeadWarningThreshold:     acfg.BraindeadThreshold,
		BraindeadWarningPrompt:        acfg.BraindeadPrompt,
		TurnLockWarnThreshold:         parseDurationDefault(acfg.TurnLockWarnThreshold, 3*time.Minute),
		Effort:                        acfg.Effort,
		Thinking:                      acfg.Thinking,
		CacheTTL:                      resolveString(acfg.CacheTTL, resolveString(p.cfg.Defaults.CacheTTL, p.cfg.Cache.TTL)),
		Streaming:                     resolveStreamingConfig(acfg, p.cfg),
		ManaInvestInterval:            parseDurationDefault(p.cfg.Mana.InvestInterval, 30*time.Minute),
	}
	// Nudge system: load rules and create scheduler
	if acfg.NudgeEnable {
		rulesPath := nudge.RulesPath(acfg.Workspace)
		rs, err := nudge.LoadRules(rulesPath)
		if err != nil {
			log.Warnf("main", "agent %s: load nudge rules: %v", acfg.ID, err)
		}
		if rs != nil && len(rs.Rules) > 0 {
			ag.Nudger = nudge.NewScheduler(rs, acfg.NudgeCooldown, acfg.NudgeMaxPerBatch)
			log.Infof("main", "agent %s: loaded %d nudge rules", acfg.ID, len(rs.Rules))
		}
		ag.NudgePreAnswerGate = acfg.NudgePreAnswerGate
		ag.NudgePreAnswerMinTools = acfg.NudgePreAnswerMinTools
		if ag.NudgePreAnswerMinTools <= 0 {
			ag.NudgePreAnswerMinTools = 2
		}

		// NudgeReloadFunc: on bootstrap reload, optionally extract new rules
		// from character files (if nudge_auto_extract), then refresh from disk.
		fileOrder := acfg.SystemFiles
		if len(fileOrder) == 0 {
			fileOrder = workspace.DefaultFileOrder
		}
		nudgeCooldown := acfg.NudgeCooldown
		nudgeMaxPerBatch := acfg.NudgeMaxPerBatch
		autoExtract := acfg.NudgeAutoExtract
		nudgeReloadFromDisk := func() {
			rs, err := nudge.LoadRules(rulesPath)
			if err != nil {
				log.Warnf("nudge", "agent %s: reload rules: %v", acfg.ID, err)
				return
			}
			if rs != nil && len(rs.Rules) > 0 {
				ag.Nudger = nudge.NewScheduler(rs, nudgeCooldown, nudgeMaxPerBatch)
			}
		}

		ag.NudgeReloadFunc = func() {
			if !autoExtract {
				nudgeReloadFromDisk()
				return
			}
			extractor := nudge.NewExtractor(acfg.Workspace, fileOrder)
			_, needed := extractor.NeedsExtraction()
			if needed {
				go func() {
					ctx := context.Background()
					parentKey := defaultSessionKey()
					if parentKey == "" {
						log.Warnf("nudge", "agent %s: no default session for extraction branch", acfg.ID)
						return
					}
					branchKey, err := session.BranchFromSession(parentKey)
					if err != nil {
						log.Warnf("nudge", "agent %s: create branch key: %v", acfg.ID, err)
						return
					}
					if err := extractor.Extract(ctx, ag, branchKey); err != nil {
						log.Warnf("nudge", "agent %s: extraction failed: %v", acfg.ID, err)
						return
					}
					nudgeReloadFromDisk()
					log.Infof("nudge", "agent %s: refreshed rules after extraction", acfg.ID)
				}()
			} else {
				nudgeReloadFromDisk()
			}
		}
	}

	if p.store != nil && p.bwStore != nil {
		ag.Redact = func(text string) string {
			text = agentStore.Redact(text)
			return p.bwStore.Redact(text)
		}
	} else if p.store != nil {
		ag.Redact = agentStore.Redact
	} else if p.bwStore != nil {
		ag.Redact = p.bwStore.Redact
	}

	// Warning injection queue (if enabled per-agent)
	if acfg.InjectAgentWarnings {
		warningWindow, err := time.ParseDuration(p.cfg.Logging.WarningWindowDuration)
		if err != nil {
			warningWindow = 5 * time.Minute
		}
		ag.WarningQueue = warnings.NewQueue(p.cfg.Logging.WarningMaxPerWindow, warningWindow)
	}

	// Mana threshold warnings (per-agent thresholds override global)
	manaThresholds := p.cfg.ManaWarnings.Thresholds
	if len(acfg.UsageWarnings.Thresholds) > 0 {
		manaThresholds = acfg.UsageWarnings.Thresholds
	}
	if len(manaThresholds) > 0 {
		ag.ManaWatcher = agent.NewManaWatcher(p.cfg.ManaWarnings.Name, manaThresholds)
		ag.ManaWatcher.SetStore(p.stateStore)
		ag.ManaWatcher.Restore()
		// Mana restore notification: per-agent overrides global
		restoreThreshold := p.cfg.ManaWarnings.RestoreThreshold
		if acfg.UsageWarnings.RestoreThreshold != nil {
			restoreThreshold = *acfg.UsageWarnings.RestoreThreshold
		}
		ag.ManaWatcher.SetRestoreThreshold(restoreThreshold)
	}

	// Spawn tool — replaces request_model, adds inherit (self-fork) mode.
	// Uses lazy getter for agent since ag is assigned later in this function.
	spawnOrientPath := resolveOrientPath(acfg.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt, acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt)
	spawnDeps := tools.SpawnDeps{
		Client:          p.client,
		ClientProvider:  p.clientProvider,
		Bootstrap:       bootstrap,
		Registry:        registry,
		Sessions:        &sessionBranchAdapter{store: p.sessions},
		AgentID:         acfg.ID,
		Model:           acfg.Model,
		ModelAliases:    p.cfg.Models.Aliases,
		MaxInherit:      resolveInt(acfg.MaxConcurrentSpawns, p.cfg.Tools.MaxConcurrentSpawns),
		MaxToolLoops:    acfg.MaxToolLoops,
		ExploreMaxDepth: resolveInt(acfg.ExploreMaxDepth, p.cfg.Tools.ExploreMaxDepth),
		Notifier:        notifier,
		OrientationBuilder: func(branchKey, parentKey string) string {
			return buildBranchOrientation(spawnOrientPath, branchKey, parentKey, "spawn", false, promptSearchDirs)
		},
	}
	registry.Register(tools.NewSpawnTool(spawnDeps, func() tools.SpawnAgent { return ag }))

	// Per-agent scheduled wakes
	setupWakeScheduler(func() *agent.Agent { return ag }, defaultSessionKey, registry, p.reminderStore, acfg.ID, p.ctx)

	// Per-agent slash commands
	// configureMultiball is set later by setupPlatform but captured
	// by the closure below, which is only called at runtime (forkMultiball).
	var configureMultiball func(platform.Connection)

	lastMsgStore := command.NewLastMessageStore()
	cmds := registerAgentCommands(cmdRegParams{
		ag:                  ag,
		acfg:                acfg,
		defaultSessionKey:   defaultSessionKey,
		sessionKeyFromCtx:   sessionKeyFromCtx,
		bootstrap:           bootstrap,
		promptSearchDirs:    promptSearchDirs,
		compactionThreshold: compactionThreshold,
		cfg:                 p.cfg,
		configPath:          p.configPath,
		sessions:            p.sessions,
		stateStore:          p.stateStore,
		sessionIndex:        p.sessionIndex,
		client:              p.client,
		clientProvider:      p.clientProvider,
		usageClientProvider: p.usageClientProvider,
		store:               p.store,
		bwStore:             p.bwStore,
		startTime:           p.startTime,
		ctx:                 p.ctx,
		registry:            registry,
		tmuxTool:            tmuxTool,
		skillsDirs:          skillsDirs,
		skillRegistry:       skillRegistry,
		agentListFn:         p.agentListFn,
		plat:                p.plat,
		connMgr:             connMgr,
		configureMultiball: func(conn platform.Connection) {
			if configureMultiball != nil {
				configureMultiball(conn)
			}
		},
	}, lastMsgStore)

	// Finalize exec tool description with dynamically-generated shell function list.
	registry.FinalizeExecDescription()

	// Log registered tools
	allTools := registry.All()
	toolNames := make([]string, len(allTools))
	for i, t := range allTools {
		toolNames[i] = t.Name
	}
	log.Infof("main", "agent %q: registered %d tools: [%s]", acfg.ID, len(toolNames), strings.Join(toolNames, ", "))
	if len(serverTools) > 0 {
		stNames := make([]string, len(serverTools))
		for i, st := range serverTools {
			stNames[i] = st.Name()
		}
		log.Infof("main", "agent %q: server tools: [%s]", acfg.ID, strings.Join(stNames, ", "))
	}

	// Create and register platform connections (allowed users resolved by each provider)
	if p.plat != nil {
		reclaimOrientPath := resolveOrientPath(acfg.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt, acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt)
		reclaimMfCfg := acfg.MemoryFormation
		reclaimSearchDirs := promptSearchDirs

		results := p.plat.SetupAgentConnection(platform.AgentConnectionParams{
			AgentID:      acfg.ID,
			Handler:      ag,
			Commands:     cmds,
			LastMsgStore: lastMsgStore,
			AgentConfig:  acfg,
			STT:          resolveSTT(p.sttMap, acfg.STT),
			TTS:          resolveTTS(p.ttsMap, p.cfg.TTS, acfg.TTS, acfg.TTSRate),
			ReclaimHook: func(sessionKey string) {
				fireSessionEndMemory(ag, p.sessions, sessionKey, reclaimMfCfg, func(bk, pk, bt string) string {
					return buildBranchOrientation(reclaimOrientPath, bk, pk, bt, false, reclaimSearchDirs)
				}, reclaimSearchDirs, p.ctx)
			},
		})
		for _, result := range results {
			if result.DefaultSessionKeyFn != nil {
				defaultSessionKeyFn = result.DefaultSessionKeyFn
			}
			if result.ConfigureMultiballConn != nil {
				configureMultiball = result.ConfigureMultiballConn
			}
		}

		wireAgentPlatformCallbacks(ag, acfg, p.cfg, p.plat, connMgr, p.sessionIndex)
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
		registry:          registry,
		bootstrap:         bootstrap,
		defaultSessionKey: defaultSessionKey,
		agentCfg:          acfg,
		promptSearchDirs:  promptSearchDirs,
		tmuxClearAll:      tmuxClearAll,
		tmuxWatchCount:    tmuxWatchCount,
		mcpManager:        mcpMgr,
	}
}

// checkFirstRun determines whether a first-run onboarding prompt should be
// injected for an agent. Returns the prompt message if injection is needed,
// empty string otherwise. Uses state.json to track completion.
func checkFirstRun(stateStore *state.Store, acfg config.AgentConfig) string {
	if stateStore == nil {
		return ""
	}

	key := "agent:" + acfg.ID + ":first_run_completed"

	// Already completed — nothing to do
	var completed bool
	if stateStore.Get(key, &completed) && completed {
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
