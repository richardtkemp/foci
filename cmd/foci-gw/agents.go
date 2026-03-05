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

	"foci/agent"
	"foci/anthropic"
	"foci/command"
	"foci/compaction"
	"foci/config"
	"foci/keepalive"
	"foci/log"
	mcpkg "foci/mcp"
	"foci/memory"
	"foci/prompts"
	"foci/provider"
	"foci/secrets"
	"foci/secrets/bitwarden"
	"foci/session"
	"foci/skills"
	"foci/state"
	"foci/telegram"
	"foci/tools"
	"foci/voice"
	"foci/warnings"
	"foci/workspace"
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
	promptSearchDirs  []string           // directories to search for prompt files
	tmuxClearAll      func()             // clears tmux tool state (watches, owned sessions)
	kaRunner          *keepalive.Runner  // keepalive & background work timer (nil if disabled)
	mcpManager        *mcpkg.Manager     // nil if no MCP servers configured
}

// applyAgentDisplaySettings sets per-agent display settings on a bot,
// falling back to global config when the agent field is nil/empty.
// Used for primary bots, per-agent multiball bots, and shared pool bots
// acquired or restored for a specific agent.
func applyAgentDisplaySettings(bot *telegram.Bot, acfg config.AgentConfig, cfg *config.Config) {
	switch {
	case acfg.ShowToolCalls != nil:
		bot.SetShowToolCalls(string(*acfg.ShowToolCalls))
	case cfg.Defaults.ShowToolCalls != nil:
		bot.SetShowToolCalls(string(*cfg.Defaults.ShowToolCalls))
	}
	switch {
	case acfg.ShowThinking != nil:
		bot.SetShowThinking(string(*acfg.ShowThinking))
	case cfg.Defaults.ShowThinking != nil:
		bot.SetShowThinking(string(*cfg.Defaults.ShowThinking))
	}
	switch {
	case acfg.DisplayWidth != nil:
		bot.SetDisplayWidth(*acfg.DisplayWidth)
	case cfg.Defaults.DisplayWidth != nil:
		bot.SetDisplayWidth(*cfg.Defaults.DisplayWidth)
	}
	if acfg.MessagesInLog != nil {
		bot.SetMessagesInLog(*acfg.MessagesInLog)
	} else {
		bot.SetMessagesInLog(cfg.Logging.MessagesInLog)
	}
	if acfg.ReceivedFilesDir != "" {
		bot.SetReceivedFilesDir(acfg.ReceivedFilesDir)
	} else if cfg.Telegram.ReceivedFilesDir != "" {
		bot.SetReceivedFilesDir(cfg.Telegram.ReceivedFilesDir)
	}
}

// checkActivityGate parses if_active/if_inactive durations, checks them against
// isActive, and writes a skip JSON response if the gate blocks the request.
// Returns true if the request should continue, false if it was skipped or errored.

// multiballBotConfig holds common settings applied to every multiball bot.
type multiballBotConfig struct {
	sttProvider     voice.STT
	ttsProvider     voice.TTS
	stopAliases     []string
	enableStopAlias bool
	acfg            config.AgentConfig
	cfg             *config.Config
	toolDetailStore *telegram.ToolDetailStore
	stateStore      *state.Store
}

// configureMultiballBot applies the standard multiball bot settings shared by
// both per-agent and shared-pool multiball bots.
func configureMultiballBot(bot *telegram.Bot, mc multiballBotConfig) {
	if mc.sttProvider != nil {
		bot.SetTranscriber(mc.sttProvider)
	}
	if mc.ttsProvider != nil {
		bot.SetTTS(mc.ttsProvider)
	}
	bot.SetStopAliases(mc.stopAliases, mc.enableStopAlias)
	applyAgentDisplaySettings(bot, mc.acfg, mc.cfg)
	if mc.toolDetailStore != nil {
		bot.SetToolDetailStore(mc.toolDetailStore)
	}
	if mc.stateStore != nil {
		ss := mc.stateStore
		bot.OnSessionKeyChange = func(username, sessionKey string) {
			key := "multiball:" + username
			if sessionKey == "" {
				_ = ss.Delete(key)
			} else {
				_ = ss.Set(key, sessionKey)
			}
		}
	}
}

// setupParams holds the shared resources needed by each agent.
type setupParams struct {
	acfg            config.AgentConfig
	cfg             *config.Config
	configPath      string
	client          provider.Client
	getClient       func(endpoint, format string) provider.Client
	peekClient      func(endpoint, format string) provider.Client
	resolveEndpointClient func(endpoint, modelID string) provider.Client
	sessions        *session.Store
	store           *secrets.Store
	bwStore         *bitwarden.Store
	stateStore      *state.Store
	memBackends     map[string]memory.Searcher
	reminderStore   *memory.ReminderStore
	scratchpadStore *memory.Scratchpad
	todoStore       *memory.TodoStore
	toolDetailStore *telegram.ToolDetailStore
	sessionIndex    *session.SessionIndex
	sttProvider     voice.STT
	ttsProvider     voice.TTS
	braveKey        string
	usageClient     *anthropic.UsageClient
	botMgr          *telegram.BotManager
	startTime       time.Time
	ctx             context.Context
	agentListFn     func() []command.AgentInfo
	agentResolverFn func(agentID string) *agentInstance
}

// setupAgent wires up a single agent with its own tools, commands, bootstrap, and bot.
func setupAgent(p setupParams) *agentInstance {
	acfg := p.acfg

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
	// (2) command.ChatIDKey (set by Telegram command dispatch),
	// (3) defaultSessionKey fallback.
	sessionKeyFromCtx := func(ctx context.Context) string {
		if sk := tools.SessionKeyFromContext(ctx); sk != "" {
			return sk
		}
		if chatID, ok := ctx.Value(command.ChatIDKey{}).(int64); ok && chatID != 0 {
			return telegram.SessionKeyForChat(acfg.ID, chatID)
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
	notifier := newAsyncNotifier(func() *agent.Agent { return ag }, defaultSessionKey, p.botMgr, acfg.ID, p.ctx)
	// Per-agent secrets view: agent-specific values overlay globals
	agentStore := p.store.ForAgent(acfg.ID)

	execAutoBg := resolveInt(acfg.ExecAutoBackground, p.cfg.Tools.ExecAutoBackground)
	maxUploadSize := resolveInt64(acfg.MaxUploadFileSize, p.cfg.Tools.MaxUploadFileSize)
	registry.Register(tools.NewExecTool(agentStore, p.bwStore, execAutoBg, notifier, acfg.Workspace, registry))

	// Only register tmux tool if tmux is available in PATH
	var tmuxTool *tools.Tool
	var tmuxClearAll func()
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
		tmuxTool, tmuxClearAll = tools.NewTmuxTool(p.cfg.Tools.TmuxCols, p.cfg.Tools.TmuxRows, notifier, p.stateStore, "tmux:"+acfg.ID, tmuxAutopilot, tmuxWatchThresholdSec, tmuxSessionTTL)
		registry.Register(tmuxTool)
	}
	blockedPaths := resolveBlockedPaths(acfg, p.cfg)
	if len(blockedPaths) > 0 {
		log.Infof("setup", "agent %s: %d blocked write/edit path(s) configured", acfg.ID, len(blockedPaths))
	}
	registry.Register(tools.NewReadTool(agentStore))
	registry.Register(tools.NewWriteTool(agentStore, blockedPaths))
	registry.Register(tools.NewEditTool(agentStore, blockedPaths))
	registry.Register(tools.NewSummaryTool(p.client, p.getClient, p.peekClient, acfg.Model, p.cfg.Models.Aliases))
	registry.Register(tools.NewHTTPRequestTool(agentStore, p.bwStore, p.cfg.Tools.TempDir, execAutoBg, maxUploadSize, notifier))

	// Web search/fetch: server-side (Anthropic) or client-side (Brave/builtin) based on config.
	var serverTools []anthropic.ToolDef

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
	var extraSystemBlocks []anthropic.SystemBlock
	if skillRegistry.Len() > 0 {
		extraSystemBlocks = []anthropic.SystemBlock{
			{Type: "text", Text: skillRegistry.SystemBlock(acfg.Workspace)},
		}
		log.Infof("main", "agent %q: loaded %d skills", acfg.ID, skillRegistry.Len())
	}

	compactionThreshold := resolveFloat64Ptr(acfg.CompactionThreshold, p.cfg.Sessions.CompactionThreshold)
	preserveMessages := resolveIntPtr(acfg.CompactionPreserveMessages, p.cfg.Sessions.CompactionPreserveMessages)
	_, bareModelID := config.SplitDeveloperModel(acfg.Model)
	compactor := compaction.NewCompactor(p.sessions, bareModelID, compactionThreshold)
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

	// Per-agent send_telegram tool (closure captures this agent's bot)
	registry.Register(tools.NewSendTelegramTool(func(sessionKey string) tools.TelegramSender {
		bot := p.botMgr.BotForSessionOrPrimary(sessionKey, acfg.ID)
		if bot == nil {
			return nil
		}
		return bot
	}, p.ttsProvider))

	// send_to_session tool — inject messages into other sessions.
	sessionNotifyFn := newSessionNotifyFn(p.agentResolverFn, p.botMgr, p.ctx)
	registry.Register(tools.NewSendToSessionTool(p.sessions, notifier, sessionNotifyFn))

	// Per-agent environment block
	var envBlock string
	if p.cfg.Environment.Enabled {
		crontabCount := countCrontabJobs()
		envBlock = buildEnvironmentBlock(acfg, p.configPath, p.cfg, crontabCount)
	}

	// Per-agent agent struct
	ag = &agent.Agent{
		Log:                         log.NewComponentLogger("agent:" + acfg.ID),
		Client:                      p.client,
		GetClient:                   p.getClient,
		PeekClient:                  p.peekClient,
		Sessions:                    p.sessions,
		Tools:                       registry,
		ServerTools:                 serverTools,
		EnvironmentBlock:            envBlock,
		Bootstrap:                   bootstrap,
		Compactor:                   compactor,
		AsyncNotifier:               notifier,
		Reminders:                   p.reminderStore,
		DefaultSessionKey:           defaultSessionKey,
		AgentID:                     acfg.ID,
		Model:                       bareModelID,
		ExtraSystemBlocks:           extraSystemBlocks,
		CacheStrategy:               p.cfg.Cache.Strategy,
		CacheBustDetect:             p.cfg.Logging.CacheBustDetect,
		CacheBustIdleThreshold:      time.Duration(p.cfg.Logging.CacheBustIdleMinutes) * time.Minute,
		DuplicateMessages:              acfg.DuplicateMessages,
		BatchPartialAssistantMessages:  acfg.BatchPartialAssistantMessages,
		BatchPartialJoiner:             acfg.BatchPartialJoiner,
		MaxResultChars:              resolveInt(acfg.MaxResultChars, p.cfg.Tools.MaxResultChars),
		ToolResultTempDir:           p.cfg.Tools.TempDir,
		ModelAliases:                p.cfg.Models.Aliases,
		SummaryContextTurns:         resolveInt(acfg.SummaryContextTurns, p.cfg.Tools.SummaryContextTurns),
		SummaryContextChars:         resolveInt(acfg.SummaryContextChars, p.cfg.Tools.SummaryContextChars),
		MaxSummaryChars:             resolveInt(acfg.MaxSummaryChars, p.cfg.Tools.MaxSummaryChars),
		AutoSummarise:               resolveBoolPtr(acfg.AutoSummarise, p.cfg.Tools.AutoSummarise),
		StateStore:                  p.stateStore,
		UsageClient:                 p.usageClient,
		MessageTransforms:           agent.CompileTransforms(resolveMessageTransforms(acfg, p.cfg)),
		CompactionSummaryPromptPath: resolveString(acfg.CompactionSummaryPrompt, p.cfg.Sessions.CompactionSummaryPrompt),
		CompactionHandoffMsg:        resolveString(acfg.CompactionHandoffMsg, p.cfg.Sessions.CompactionHandoffMsg),
		PromptSearchDirs:            promptSearchDirs,
		MaxToolLoops:                acfg.MaxToolLoops,
		MaxOutputTokens:             acfg.MaxOutputTokens,
		BraindeadWarningThreshold:   acfg.BraindeadThreshold,
		BraindeadWarningPrompt:      acfg.BraindeadPrompt,
		TurnLockWarnThreshold:       parseDurationDefault(acfg.TurnLockWarnThreshold, 3*time.Minute),
		Effort:                      acfg.Effort,
		Thinking:                    acfg.Thinking,
		Streaming:                   resolveStreamingConfig(acfg, p.cfg),
		ManaInvestInterval: parseDurationDefault(acfg.Background.InvestInterval, 30*time.Minute),
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
	// Restore per-session state and seed session meta for default session (if any).
	// These are no-ops if no default session exists yet (first startup).
	if sk := defaultSessionKey(); sk != "" {
		ag.RestoreVoiceMode(sk)
		ag.RestoreSessionOverrides(sk)
		ag.SeedSessionMeta(sk)
	}

	// Warning injection queue (if enabled per-agent)
	if acfg.InjectAgentWarnings {
		warningWindow, err := time.ParseDuration(p.cfg.Logging.WarningWindowDuration)
		if err != nil {
			warningWindow = 5 * time.Minute
		}
		ag.Warnings = warnings.NewQueue(p.cfg.Logging.WarningMaxPerWindow, warningWindow)
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
		GetClient:       p.getClient,
		PeekClient:      p.peekClient,
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
	lastMsgStore := command.NewLastMessageStore()
	cmds := registerAgentCommands(cmdRegParams{
		ag:                    ag,
		acfg:                  acfg,
		defaultSessionKey:     defaultSessionKey,
		sessionKeyFromCtx:     sessionKeyFromCtx,
		bootstrap:             bootstrap,
		promptSearchDirs:      promptSearchDirs,
		compactionThreshold:   compactionThreshold,
		cfg:                   p.cfg,
		configPath:            p.configPath,
		sessions:              p.sessions,
		stateStore:            p.stateStore,
		sessionIndex:          p.sessionIndex,
		client:                p.client,
		resolveEndpointClient: p.resolveEndpointClient,
		usageClient:           p.usageClient,
		botMgr:                p.botMgr,
		store:                 p.store,
		bwStore:               p.bwStore,
		startTime:             p.startTime,
		ctx:                   p.ctx,
		registry:              registry,
		tmuxTool:              tmuxTool,
		skillsDirs:            skillsDirs,
		skillRegistry:         skillRegistry,
		agentListFn:           p.agentListFn,
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
	allowedUsers := acfg.AllowedUsers
	if len(allowedUsers) == 0 {
		allowedUsers = p.cfg.Telegram.AllowedUsers
	}

	// Create and register Telegram bots via BotManager
	setupTelegram(p, acfg, ag, cmds, allowedUsers, lastMsgStore)

	// Wire the default session key function after bot creation.
	// Must be deferred because primaryBot may not exist yet.
	defer func() {
		bot := p.botMgr.PrimaryBot(acfg.ID)
		if bot != nil {
			defaultSessionKeyFn = bot.DefaultSessionKey
		}
	}()

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
		mcpManager:        mcpMgr,
	}
}

// setupTelegram creates and registers Telegram bots for an agent.
// If the primary bot fails to initialize, the agent continues without Telegram.
func setupTelegram(p setupParams, acfg config.AgentConfig, ag *agent.Agent, cmds *command.Registry, allowedUsers []string, lastMsgStore *command.LastMessageStore) {
	telegramToken := config.ResolveBotToken(acfg.TelegramBot, acfg.BotSecret, p.store)
	if telegramToken == "" {
		return
	}

	primaryBot, err := telegram.NewBot(telegramToken, allowedUsers, ag, cmds, lastMsgStore, acfg.ID)
	if err != nil {
		log.Errorf("main", "agent %q: create telegram bot: %v (agent will run without Telegram)", acfg.ID, err)
		return
	}

	if p.stateStore != nil {
		botKey := "bot:" + acfg.TelegramBot
		if botKey == "bot:" {
			botKey = "bot:" + acfg.ID
		}
		primaryBot.SetStateStore(p.stateStore, botKey)
	}
	if p.toolDetailStore != nil {
		primaryBot.SetToolDetailStore(p.toolDetailStore)
	}

	if p.sttProvider != nil {
		primaryBot.SetTranscriber(p.sttProvider)
	}
	if p.ttsProvider != nil {
		primaryBot.SetTTS(voice.WithRate(p.ttsProvider, acfg.TTSRate))
	}
	primaryBot.SetStopAliases(p.cfg.Telegram.StopAliases, p.cfg.Telegram.EnableStopAliases)
	primaryBot.SetToolCallPreviewChars(p.cfg.Tools.ToolCallPreviewChars)
	applyAgentDisplaySettings(primaryBot, acfg, p.cfg)

	// Wire cache bust alerts to this agent's bot
	if ag.CacheBustDetect {
		ag.CacheBustAlert = func(session string, prevRead, curRead int) {
			msg := fmt.Sprintf("⚠️ Cache bust: read dropped %d → %d on %s", prevRead, curRead, session)
			log.Warnf("agent", "%s", msg)
			primaryBot.SendNotification(msg)
		}
	}

	// Wire mana threshold warnings to Telegram
	if ag.ManaWatcher != nil {
		ag.ManaWarnFunc = func(warn string) {
			log.Infof("mana", "%s", warn)
			primaryBot.SendNotification("⚠️ " + warn)
		}
	}

	// Wire rate limit notifications to Telegram
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
		primaryBot.SendNotification("⚡ " + msg)
	}

	// Wire max_tokens warnings to Telegram
	ag.MaxTokensWarnFunc = func(warn string) {
		primaryBot.SendNotification("⚠️ " + warn)
	}

	// Wire compaction notifications to Telegram (default on)
	// Per-agent compaction_notify overrides global
	compactNotify := p.cfg.Sessions.CompactionNotify
	if acfg.CompactionNotify != nil {
		compactNotify = acfg.CompactionNotify
	}
	if compactNotify == nil || *compactNotify {
		ag.CompactionNotifyFunc = func(session string, msg string) {
			primaryBot.SendNotification(msg)
		}
	}

	// Wire session activity tracking for the session index.
	if p.sessionIndex != nil {
		sidx := p.sessionIndex // capture for closure
		ag.OnActivity = func(sessionKey string) {
			sidx.TouchActivity(sessionKey)
		}
	}

	// Wire compaction debug (send summary as file attachment)
	compactDebug := p.cfg.Sessions.CompactionDebug
	if acfg.CompactionDebug != nil {
		compactDebug = *acfg.CompactionDebug
	}
	if compactDebug {
		bot := primaryBot // capture for closure
		ag.CompactionDebugFunc = func(sessionKey, summary string) {
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
			if err := bot.SendDocument(f.Name()); err != nil {
				log.Warnf("agent", "compaction debug: send document: %v", err)
			}
			_ = os.Remove(f.Name())
		}
	}

	p.botMgr.AddPrimary(acfg.ID, primaryBot)

	// Per-agent multiball bots (if configured)
	for _, botName := range acfg.MultiballBots {
		mbToken := config.ResolveBotToken(botName, "", p.store)
		if mbToken == "" {
			log.Errorf("main", "agent %q: multiball bot %q: token not found", acfg.ID, botName)
			continue
		}
		mbBot, err := telegram.NewBot(mbToken, allowedUsers, ag, cmds, lastMsgStore, "") // secondary: no agentID
		if err != nil {
			log.Errorf("main", "agent %q: create multiball bot %q: %v", acfg.ID, botName, err)
			continue
		}
		configureMultiballBot(mbBot, multiballBotConfig{
			sttProvider:     p.sttProvider,
			ttsProvider:     p.ttsProvider,
			stopAliases:     p.cfg.Telegram.StopAliases,
			enableStopAlias: p.cfg.Telegram.EnableStopAliases,
			acfg:            acfg,
			cfg:             p.cfg,
			toolDetailStore: p.toolDetailStore,
			stateStore:      p.stateStore,
		})
		p.botMgr.AddMultiball(acfg.ID, mbBot)
	}
	if pool := p.botMgr.Pool(acfg.ID); pool != nil && pool.Size() > 0 {
		log.Infof("main", "agent %q: %d per-agent multiball bots ready", acfg.ID, pool.Size())
	}

	// Configure session TTL for per-agent multiball pool
	if pool := p.botMgr.Pool(acfg.ID); pool != nil {
		ttl, _ := time.ParseDuration(p.cfg.Telegram.MultiballSessionTTL) // validated earlier
		if ttl > 0 {
			pool.SetSessionTTL(ttl, p.sessions)
			log.Infof("main", "agent %q: multiball session TTL = %v", acfg.ID, ttl)
		}
		reclaimOrientPath := resolveOrientPath(acfg.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt, acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt)
		reclaimMfCfg := acfg.MemoryFormation
		reclaimSearchDirs := []string{
			filepath.Join(acfg.Workspace, "prompts"),
			filepath.Join(filepath.Dir(acfg.Workspace), "shared", "prompts"),
		}
		pool.ReclaimHook = func(sessionKey string) {
			fireSessionEndMemory(ag, p.sessions, sessionKey, reclaimMfCfg, func(bk, pk, bt string) string {
				return buildBranchOrientation(reclaimOrientPath, bk, pk, bt, false, reclaimSearchDirs)
			}, reclaimSearchDirs, p.ctx)
		}
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
func buildServerTool(toolType, toolName string, maxUses int, allowed, blocked []string) anthropic.ToolDef {
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
	return anthropic.NewServerTool(cfg)
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

// buildBranchFunc creates a keepalive.BranchFunc that dispatches branch sessions
// using the provided agent infrastructure. This is the bridge between the keepalive
// package and the main package's agent/session handling.
func buildBranchFunc(
	agentID string,
	ag *agent.Agent,
	sessions *session.Store,
	defaultSessionKey func() string,
	buildOrientation func(branchKey, parentKey, branchType string) string,
	ctx context.Context,
) keepalive.BranchFunc {
	return func(branchType, promptText string, noCompact bool) {
		parentKey := defaultSessionKey()
		if parentKey == "" {
			log.Warnf("keepalive", "no default session for agent %q, skipping %s", agentID, branchType)
			return
		}

		// Cron tasks are branches from the default session
		branchKey, branchErr := session.BranchFromSession(parentKey)
		if branchErr != nil {
			log.Errorf("keepalive", "%s branch key error: %v", branchType, branchErr)
			return
		}

		orientText := buildOrientation(branchKey, parentKey, branchType)
		err := sessions.CreateBranchWithOptions(parentKey, branchKey, session.BranchOptions{
			NoResetHook:        true,
			OrientationMessage: orientText,
		})
		if err != nil {
			log.Errorf("keepalive", "%s branch error: %v", branchType, err)
			return
		}

		turnCtx := agent.WithTrigger(ctx, branchType)
		if noCompact {
			ag.SetSessionNoCompact(branchKey, true)
		}

		resp, err := ag.HandleMessage(turnCtx, branchKey, promptText)
		if err != nil {
			log.Errorf("keepalive", "%s turn error: %v", branchType, err)
			return
		}
		_ = resp // keepalive/background responses are not delivered to user
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
		log.Errorf("session-end-memory", "create branch key: %v", err)
		return
	}
	orientText := buildOrientation(branchKey, sessionKey, "session-end-memory")
	if err := sessions.CreateBranchWithOptions(sessionKey, branchKey, session.BranchOptions{
		NoResetHook:        true,
		OrientationMessage: orientText,
	}); err != nil {
		log.Errorf("session-end-memory", "branch error: %v", err)
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

// extractAgentID extracts the agent ID from a session key.
// Session keys have the format "agent:<id>:..." — returns the second segment.
func extractAgentID(sessionKey string) string {
	parts := strings.SplitN(sessionKey, ":", 3)
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

// restoreMultiballSessions restores persisted multiball session mappings after restart.
// For each secondary bot in all pools, it looks up "multiball:<username>" in stateStore.
// If a saved session key exists and the session file is still active, the bot is restored.
func restoreMultiballSessions(
	botMgr *telegram.BotManager,
	stateStore *state.Store,
	sessions *session.Store,
	agents map[string]*agentInstance,
	agentOrder []string,
	cfg *config.Config,
) {
	// Collect all pools to iterate
	type poolInfo struct {
		pool *telegram.Pool
		name string
	}
	var pools []poolInfo
	for _, id := range agentOrder {
		if pool := botMgr.Pool(id); pool != nil {
			pools = append(pools, poolInfo{pool: pool, name: "agent:" + id})
		}
	}
	if sp := botMgr.SharedPool(); sp != nil {
		pools = append(pools, poolInfo{pool: sp, name: "shared"})
	}

	restored := 0
	for _, pi := range pools {
		pi.pool.ForEach(func(bot *telegram.Bot) {
			username := bot.Username()
			if username == "" {
				return
			}
			var savedKey string
			if !stateStore.Get("multiball:"+username, &savedKey) || savedKey == "" {
				return
			}

			// Validate session still exists on disk
			if sessions.LastActivity(savedKey) == "n/a" {
				log.Infof("main", "multiball restore: @%s session %s no longer exists, cleaning up", username, savedKey)
				_ = stateStore.Delete("multiball:" + username)
				return
			}

			// Restore session key (bypass callback — already persisted)
			bot.SetSessionKeyDirect(savedKey)

			// Re-wire agent if we can identify it from the session key
			agentID := extractAgentID(savedKey)
			if inst, ok := agents[agentID]; ok {
				bot.SetAgentAndCommands(inst.ag, inst.cmds)
				applyAgentDisplaySettings(bot, inst.agentCfg, cfg)
			}

			// Copy chatID from primary bot so notifications work
			if agentID != "" {
				if primary := botMgr.PrimaryBot(agentID); primary != nil {
					if chatID := primary.ChatID(); chatID != 0 {
						bot.SetChatID(chatID)
					}
				}
			}

			restored++
			log.Infof("main", "multiball restore: @%s → %s", username, savedKey)
		})
	}
	if restored > 0 {
		log.Infof("main", "restored %d multiball session(s) from state", restored)
	}
}
