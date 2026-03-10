package main

import (
	"context"
	"crypto/md5" // #nosec G501 - used for agent ID hashing, not security
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/log"
	mcpkg "foci/internal/mcp"
	"foci/internal/memory"
	"foci/internal/periodic"
	"foci/internal/provider"
	"foci/internal/secrets"
	"foci/internal/secrets/bitwarden"
	"foci/internal/session"
	"foci/internal/skills"
	"foci/internal/state"
	"foci/internal/platform"
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
	// Before any Telegram message arrives, this returns "" (no default set).
	// After the first message, it returns agent:<id>:chat:<chatID>.
	// The resolver is set to use the primary bot's DefaultSessionKey once wired.
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
	compactor.Scratchpad = p.scratchpadStore
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
		envBlock = buildEnvironmentBlock(acfg, p.configPath, p.cfg, crontabCount)
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
		CompactionSummaryPromptPath:   resolveString(acfg.CompactionSummaryPrompt, p.cfg.Sessions.CompactionSummaryPrompt),
		CompactionHandoffMsg:          resolveString(acfg.CompactionHandoffMsg, p.cfg.Sessions.CompactionHandoffMsg),
		PromptSearchDirs:              promptSearchDirs,
		MaxToolLoops:                  acfg.MaxToolLoops,
		MaxOutputTokens:               acfg.MaxOutputTokens,
		BraindeadWarningThreshold:     acfg.BraindeadThreshold,
		BraindeadWarningPrompt:        acfg.BraindeadPrompt,
		TurnLockWarnThreshold:         parseDurationDefault(acfg.TurnLockWarnThreshold, 3*time.Minute),
		Effort:                        acfg.Effort,
		Thinking:                      acfg.Thinking,
		Streaming:                     resolveStreamingConfig(acfg, p.cfg),
		ManaInvestInterval:            parseDurationDefault(p.cfg.Mana.InvestInterval, 30*time.Minute),
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

	// Resolve per-agent allowed users (falls back to global)
	// Prefer new platform config, fall back to deprecated fields
	tg := acfg.GetTelegramPlatform()
	var allowedUsers []string
	switch {
	case tg != nil && len(tg.AllowedUsers) > 0:
		allowedUsers = tg.AllowedUsers
	case len(acfg.AllowedUsers) > 0:
		allowedUsers = acfg.AllowedUsers
	default:
		allowedUsers = p.cfg.Telegram.AllowedUsers
	}

	// Create and register platform connections
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
			AllowedUsers: allowedUsers,
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

// seedDefaultPrompts writes embedded prompt files to dir if they don't already
// exist. This gives users editable copies they can customise.
func seedDefaultPrompts(dir string) {
	promptFiles := map[string]func() string{
		"keepalive.md":                    prompts.Keepalive,
		"background.md":                   prompts.Background,
		"memory-formation.md":             prompts.MemoryFormation,
		"memory-consolidation.md":         prompts.MemoryConsolidation,
		"compaction-summary.md":           prompts.CompactionSummary,
		"compaction-handoff.md":           prompts.CompactionHandoff,
		"branch-orientation-headless.md":  prompts.BranchOrientationHeadless,
		"branch-orientation-multiball.md": prompts.BranchOrientationMultiball,
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Warnf("main", "seed prompts: mkdir %s: %v", dir, err)
		return
	}

	for name, fn := range promptFiles {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			continue // already exists
		}
		if err := os.WriteFile(path, []byte(fn()+"\n"), 0644); err != nil {
			log.Warnf("main", "seed prompts: write %s: %v", path, err)
			continue
		}
		log.Infof("main", "seeded default prompt: %s", path)
	}
}

// buildBranchFunc returns a function that creates a branch session from the
// agent's default session and runs a single turn with the given prompt.
func buildBranchFunc(
	agentID string,
	ag *agent.Agent,
	sessions *session.Store,
	defaultSessionKey func() string,
	buildOrientation func(branchKey, parentKey, branchType string) string,
	ctx context.Context,
) periodic.BranchFunc {
	return func(branchType, promptText string, noCompact bool) {
		parentKey := defaultSessionKey()
		if parentKey == "" {
			log.Warnf(branchType, "[%s] no default session, skipping", agentID)
			return
		}

		branchKey, branchErr := session.BranchFromSession(parentKey)
		if branchErr != nil {
			log.Errorf(branchType, "[%s] branch key error (parent=%s): %v", agentID, parentKey, branchErr)
			return
		}

		orientText := buildOrientation(branchKey, parentKey, branchType)
		err := sessions.CreateBranchWithOptions(parentKey, branchKey, session.BranchOptions{
			NoResetHook:        true,
			OrientationMessage: orientText,
		})
		if err != nil {
			log.Errorf(branchType, "[%s] branch error: %v", agentID, err)
			return
		}

		turnCtx := agent.WithTrigger(ctx, branchType)
		if noCompact {
			ag.SetSessionNoCompact(branchKey, true)
		}

		resp, err := ag.HandleMessage(turnCtx, branchKey, promptText)
		if err != nil {
			log.Warnf(branchType, "[%s] session=%s turn error: %v", agentID, branchKey, err)
			return
		}
		_ = resp
	}
}

// buildBranchOrientation constructs orientation text for a branch session.
// Resolves the prompt through ResolvePrompt: explicit path → search dirs → embedded default.
// Template variables: {branch_key}, {parent_key}, {branch_type}, {direct_chat}.
func buildBranchOrientation(promptPath, branchKey, parentKey, branchType string, directChat bool, searchDirs []string) string {
	var filename, embedded string
	if directChat {
		filename = "branch-orientation-multiball.md"
		embedded = prompts.BranchOrientationMultiball()
	} else {
		filename = "branch-orientation-headless.md"
		embedded = prompts.BranchOrientationHeadless()
	}
	text := prompts.ResolvePrompt(promptPath, filename, embedded, searchDirs...)
	return prompts.ReplaceVars(text, map[string]string{
		"branch_key":  branchKey,
		"parent_key":  parentKey,
		"branch_type": branchType,
		"direct_chat": fmt.Sprintf("%v", directChat),
	})
}

// resolvePromptInfo builds a PromptInfo for a file-path-based prompt, comparing
// the resolved text against the embedded default via md5 to detect customisation.
func resolvePromptInfo(label, configPath, filename, embeddedDefault string, searchDirs []string) command.PromptInfo {
	if configPath == "none" {
		return command.PromptInfo{Label: label, Filename: filename, Disabled: true}
	}

	resolved := prompts.ResolvePrompt(configPath, filename, embeddedDefault, searchDirs...)
	isDefault := md5.Sum([]byte(resolved)) == md5.Sum([]byte(embeddedDefault)) // #nosec G401 - content comparison, not security

	// Find the actual file path being used
	path := configPath
	if path == "" || path == "default" {
		// Search dirs — find which file was used
		for _, dir := range searchDirs {
			fp := filepath.Join(dir, filename)
			if _, err := os.Stat(fp); err == nil {
				path = fp
				break
			}
		}
	}

	if path == "" || path == "default" {
		// Using embedded default, no file on disk
		return command.PromptInfo{Label: label, Filename: filename, Default: isDefault}
	}

	_, err := os.Stat(path)
	return command.PromptInfo{Label: label, Path: path, Filename: filename, Exists: err == nil, Default: isDefault}
}

// inlinePromptInfo builds a PromptInfo for an inline prompt value,
// comparing against the embedded default via md5.
func inlinePromptInfo(label, value, embeddedDefault string) command.PromptInfo {
	if value == "" {
		return command.PromptInfo{Label: label, Inline: embeddedDefault, Default: true}
	}
	if value == "none" {
		return command.PromptInfo{Label: label, Disabled: true}
	}
	isDefault := md5.Sum([]byte(value)) == md5.Sum([]byte(embeddedDefault)) // #nosec G401 - content comparison, not security
	return command.PromptInfo{Label: label, Inline: value, Default: isDefault}
}

// fireSessionEndMemory runs memory formation on the expiring session before it is cleared.
// Creates an async branch from the session so the caller can proceed immediately.
// Checks BranchMeta.NoResetHook and memory_formation.session_end_enabled.
func fireSessionEndMemory(ag *agent.Agent, sessions *session.Store, sessionKey string, mfCfg config.MemoryFormationConfig, buildOrientation func(branchKey, parentKey, branchType string) string, searchDirs []string, parentCtx context.Context) {
	if mfCfg.SessionEndEnabled != nil && !*mfCfg.SessionEndEnabled {
		return
	}

	// Check availability before doing any work
	canFire, reason := ag.CanFireBackgroundOperation(parentCtx, sessionKey)
	if !canFire {
		log.Debugf("session-end-memory", "skipping for %s: %s", sessionKey, reason)
		return
	}

	prompt := prompts.ResolvePrompt(mfCfg.SessionEndPrompt, "memory-formation.md", prompts.MemoryFormation(), searchDirs...)
	if prompt == "" {
		return
	}

	// Check branch metadata for NoResetHook
	meta, err := sessions.GetBranchMeta(sessionKey)
	if err != nil {
		log.Warnf("session-end-memory", "check branch meta for %s: %v", sessionKey, err)
	}
	if meta != nil && meta.NoResetHook {
		log.Debugf("session-end-memory", "skipping for %s (no_reset_hook set)", sessionKey)
		return
	}

	// Branch from expiring session so the memory formation job has conversation history.
	// The caller proceeds immediately to clear the main session.
	// Create session-end memory branch
	branchKey, err := session.BranchFromSession(sessionKey)
	if err != nil {
		log.Errorf("session-end-memory", "create branch key for session %s: %v", sessionKey, err)
		return
	}
	orientText := buildOrientation(branchKey, sessionKey, "session-end-memory")
	if err := sessions.CreateBranchWithOptions(sessionKey, branchKey, session.BranchOptions{
		NoResetHook:        true,
		OrientationMessage: orientText,
	}); err != nil {
		log.Errorf("session-end-memory", "branch error for session %s → %s: %v", sessionKey, branchKey, err)
		return
	}

	log.Infof("session-end-memory", "firing for %s → %s", sessionKey, branchKey)

	go func() {
		hookCtx, cancel := context.WithTimeout(parentCtx, 120*time.Second)
		defer cancel()
		hookCtx = agent.WithTrigger(hookCtx, "session_end_memory")
		ag.SetSessionNoCompact(branchKey, true)
		if _, err := ag.HandleMessage(hookCtx, branchKey, prompt); err != nil {
			log.Warnf("session-end-memory", "failed for %s: %v", branchKey, err)
		}
	}()
}

// wireAgentPlatformCallbacks wires notification callbacks from the agent to all
// platform connections. Uses the Messaging facade for fan-out — zero platform-specific types.
func wireAgentPlatformCallbacks(
	ag *agent.Agent,
	acfg config.AgentConfig,
	cfg *config.Config,
	plat *platform.Messaging,
	connMgr platform.ConnectionManager,
	sessionIndex *session.SessionIndex,
) {
	// Register ALL platform connections with agent
	for i, conn := range connMgr.AllForAgent(acfg.ID) {
		ag.AddPlatform(fmt.Sprintf("platform-%d", i), conn)
	}

	// Cache bust — notify all connections
	if ag.CacheBustDetect {
		ag.CacheBustAlert = func(sess string, prev, cur int) {
			msg := fmt.Sprintf("⚠️ Cache bust: cache_read dropped %d → %d on %s", prev, cur, sess)
			log.Warnf("agent", "%s", msg)
			plat.NotifyAgent(acfg.ID, msg)
		}
	}

	// Mana warnings — notify all
	if ag.ManaWatcher != nil {
		ag.ManaWarnFunc = func(warn string) {
			log.Infof("mana", "%s", warn)
			plat.NotifyAgent(acfg.ID, "⚠️ "+warn)
		}
	}

	// Rate limit — notify all
	ag.RateLimitFunc = func(retryAfter int) {
		msg := "I've run out of mana, it will reset in ~5 hours."
		if retryAfter > 0 {
			mins := (retryAfter + 59) / 60
			if mins >= 60 {
				msg = fmt.Sprintf("I've run out of mana, it will reset in ~%dh %dm.", mins/60, mins%60)
			} else {
				msg = fmt.Sprintf("I've run out of mana, it will reset in ~%d minutes.", mins)
			}
		}
		plat.NotifyAgent(acfg.ID, "⚡ "+msg)
	}

	// Max tokens — notify all
	ag.MaxTokensWarnFunc = func(warn string) {
		plat.NotifyAgent(acfg.ID, "⚠️ "+warn)
	}

	// Compaction notify — session-specific connection, falls back to all
	compactNotify := cfg.Sessions.CompactionNotify
	if acfg.CompactionNotify != nil {
		compactNotify = acfg.CompactionNotify
	}
	if compactNotify == nil || *compactNotify {
		ag.CompactionNotifyFunc = func(sk, msg string) {
			if c := connMgr.ForSession(sk); c != nil {
				c.SendNotification(msg)
			} else {
				plat.NotifyAgent(acfg.ID, msg)
			}
		}
	}

	// Compaction debug — session-specific connection for document
	compactDebug := cfg.Sessions.CompactionDebug
	if acfg.CompactionDebug != nil {
		compactDebug = *acfg.CompactionDebug
	}
	if compactDebug {
		ag.CompactionDebugFunc = func(sk, summary string) {
			c := connMgr.ForSession(sk)
			if c == nil {
				c = connMgr.Primary(acfg.ID)
			}
			if c == nil {
				return
			}
			f, err := os.CreateTemp("", "compaction-summary-*.md")
			if err != nil {
				log.Warnf("agent", "compaction debug: create temp file: %v", err)
				return
			}
			if _, err := f.WriteString(summary); err != nil {
				_ = f.Close()
				_ = os.Remove(f.Name())
				log.Warnf("agent", "compaction debug: write temp file: %v", err)
				return
			}
			_ = f.Close()
			if err := c.SendDocument(f.Name()); err != nil {
				log.Warnf("agent", "compaction debug: send document: %v", err)
			}
			_ = os.Remove(f.Name())
		}
	}

	// Session activity tracking
	if sessionIndex != nil {
		ag.OnActivity = func(sk string) { sessionIndex.TouchActivity(sk) }
	}
}

// sessionBranchAdapter wraps session.Store to implement tools.SessionBrancher.
type sessionBranchAdapter struct {
	store *session.Store
}

func (a *sessionBranchAdapter) CreateBranch(parentKey, branchKey string, opts tools.BranchOptions) error {
	return a.store.CreateBranchWithOptions(parentKey, branchKey, session.BranchOptions{
		NoResetHook:        opts.NoResetHook,
		OrientationMessage: opts.OrientationMessage,
	})
}

func (a *sessionBranchAdapter) SessionPath(key string) (string, error) {
	return a.store.SessionPath(key)
}

