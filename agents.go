package main

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"foci/agent"
	"foci/anthropic"
	"foci/command"
	"foci/compaction"
	"foci/config"
	"foci/keepalive"
	"foci/log"
	"foci/mana"
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
	// The response is delivered to Telegram via the primary bot's SendText.
	notifier := tools.NewAsyncNotifier(func(originSession, message string) {
		go func() {
			// Route to the originating session; fall back to default if unknown
			target := originSession
			if target == "" {
				target = defaultSessionKey()
			}

			// Resolve bot early so intermediate replies (ReplyFunc) can be delivered
			// during the turn, not just at the end.
			bot := p.botMgr.BotForSessionOrPrimary(target, acfg.ID)

			ctx := agent.WithTrigger(p.ctx, "async_notify")
			if bot != nil {
				ctx = agent.WithTurnCallbacks(ctx, &agent.TurnCallbacks{
					ReplyFunc: func(text string) {
						if err := bot.SendText(text); err != nil {
							log.Errorf("async_notify", "intermediate telegram delivery: %v", err)
						}
					},
				})
			}

			resp, err := ag.HandleMessage(ctx, target, message)
			if err != nil {
				log.Errorf("async_notify", "error: %v", err)
				return
			}
			log.Debugf("async_notify", "response length: %d", len(resp))
			if resp == "" {
				return
			}
			if bot == nil {
				log.Warnf("async_notify", "no bot for agent %s session %s, response not delivered", acfg.ID, target)
				return
			}
			if err := bot.SendText(resp); err != nil {
				log.Errorf("async_notify", "telegram delivery: %v", err)
			}
		}()
	})
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
	_, bareModelID := config.ParseModel(acfg.Model)
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
	// sessionNotifyFn handles reply_to="session": routes the target agent's
	// response to the target session's own Telegram chat.
	sessionNotifyFn := tools.SessionNotifyFn(func(targetSessionKey, message string) {
		go func() {
			// Parse agent ID from session key (agent:<id>:...)
			parts := strings.Split(targetSessionKey, ":")
			if len(parts) < 2 {
				log.Errorf("session_notify", "invalid session key: %s", targetSessionKey)
				return
			}
			targetAgentID := parts[1]

			inst := p.agentResolverFn(targetAgentID)
			if inst == nil {
				log.Errorf("session_notify", "unknown agent %q for session %s", targetAgentID, targetSessionKey)
				return
			}

			resp, err := inst.ag.HandleMessage(agent.WithTrigger(p.ctx, "session_notify"), targetSessionKey, message)
			if err != nil {
				log.Errorf("session_notify", "error: %v", err)
				return
			}
			if resp == "" {
				return
			}

			bot := p.botMgr.BotForSessionOrPrimary(targetSessionKey, targetAgentID)
			if bot == nil {
				log.Warnf("session_notify", "no bot for agent %s session %s, response not delivered", targetAgentID, targetSessionKey)
				return
			}

			// Extract chat ID from session key for targeted delivery.
			// Supports both "agent:X:chat:CHATID" and legacy "agent:X:CHATID".
			// Falls back to bot's default chat if no chat ID found.
			chatID := tools.ChatIDFromSessionKey(targetSessionKey)
			if chatID != 0 {
				if err := bot.SendTextToChat(chatID, resp); err != nil {
					log.Errorf("session_notify", "telegram delivery to chat %d: %v", chatID, err)
				}
			} else {
				if err := bot.SendText(resp); err != nil {
					log.Errorf("session_notify", "telegram delivery: %v", err)
				}
			}
		}()
	})
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
		ManaInvestInterval: func() time.Duration {
			d, err := time.ParseDuration(acfg.Background.InvestInterval)
			if err != nil {
				return 30 * time.Minute
			}
			return d
		}(),
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
	var wakesMu sync.Mutex
	wakes := make(map[int64]context.CancelFunc)
	wakeScheduleFn := func(id int64, delay time.Duration, message string) error {
		wakeCtx, wakeCancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-time.After(delay):
				log.Infof("remind", "firing wake id=%d after %v for agent %s: %q", id, delay, acfg.ID, message)
				if p.reminderStore != nil {
					_ = p.reminderStore.Dismiss(id)
				}
				sk := defaultSessionKey()
				if sk == "" {
					log.Warnf("remind", "no default session for agent %s, skipping", acfg.ID)
					return
				}
				resp, err := ag.HandleMessage(agent.WithTrigger(p.ctx, "scheduled_wake"), sk, prompts.FormatInjectedMessage("SCHEDULED WAKE", time.Now(), message))
				if err != nil {
					log.Errorf("remind", "error: %v", err)
				} else {
					log.Debugf("remind", "response: %s", resp)
				}
				wakesMu.Lock()
				delete(wakes, id)
				wakesMu.Unlock()
			case <-wakeCtx.Done():
				if p.reminderStore != nil {
					_ = p.reminderStore.Dismiss(id)
				}
				wakesMu.Lock()
				delete(wakes, id)
				wakesMu.Unlock()
			}
		}()
		wakesMu.Lock()
		wakes[id] = wakeCancel
		wakesMu.Unlock()
		return nil
	}
	if p.reminderStore != nil {
		registry.Register(tools.NewRemindTool(p.reminderStore, acfg.ID, wakeScheduleFn))

		// Restore pending wakes from DB (survives restart)
		if pending, err := p.reminderStore.PendingWakes(acfg.ID); err != nil {
			log.Errorf("remind", "failed to load pending wakes for %s: %v", acfg.ID, err)
		} else if len(pending) > 0 {
			for _, r := range pending {
				delay := time.Until(r.DueAt)
				if delay < 0 {
					delay = 0
				}
				_ = wakeScheduleFn(r.ID, delay, r.Text)
			}
			log.Infof("remind", "restored %d pending wake(s) for agent %s", len(pending), acfg.ID)
		}
	}

	// Per-agent slash commands
	lastMsgStore := command.NewLastMessageStore()
	cmds := command.NewRegistry()
	cmds.Register(command.NewPingCommand())
	cmds.Register(command.NewStatusCommand(func() command.StatusInfo {
		sk := defaultSessionKey()
		return command.StatusInfo{
			AgentID:          acfg.ID,
			SessionKey:       sk,
			MessageCount:     sessionMessageCount(p.sessions, sk),
			Model:            ag.Model,
			Uptime:           time.Since(p.startTime),
			StartTime:        p.startTime,
			AgentBusy:        ag.IsProcessing(),
			CreatedAt:        p.sessions.CreatedAt(sk),
			LastActivity:     p.sessions.LastActivity(sk),
			ContextLimit:     compaction.ContextLimit(ag.Model),
			CompactThreshold: compactionThreshold,
		}
	}, p.cfg.Logging.APIFile))
	cmds.Register(command.NewCacheCommand(p.cfg.Logging.APIFile))
	cmds.Register(command.NewLastCommand(p.cfg.Logging.APIFile))
	cmds.Register(command.NewCostCommand(p.cfg.Logging.APIFile))
	if tmuxTool != nil {
		cmds.Register(command.NewTmuxCommand(func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			return tmuxTool.Execute(ctx, params)
		}))
	}
	cmds.Register(command.NewContextCommand(p.cfg.Logging.APIFile, buildContextInfoFn(
		ag, bootstrap, registry, acfg, p.client, p.sessions, defaultSessionKey, compactionThreshold,
	)))
	cmds.Register(command.NewResetCommand(func() error {
		if ag.IsProcessing() {
			return fmt.Errorf("agent is processing — send /stop first, then /reset")
		}
		sk := defaultSessionKey()
		if sk == "" {
			return fmt.Errorf("no active session to reset")
		}
		resetOrientPath := resolveOrientPath(acfg.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt, acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt)
		fireSessionEndMemory(ag, p.sessions, sk, acfg.MemoryFormation, func(bk, pk, bt string) string {
			return buildBranchOrientation(resetOrientPath, bk, pk, bt, false, promptSearchDirs)
		}, promptSearchDirs, p.ctx)
		if err := p.sessions.Clear(sk); err != nil {
			return err
		}
		bootstrap.Reload()
		ag.InvalidateSystemCaches()
		return nil
	}))

	// Model resolution using config aliases.
	// resolveAliasFn resolves a short alias to its full endpoint:model_id form.
	// Aliases now include endpoint prefixes (e.g. "opus" → "anthropic:claude-opus-4-6").
	aliases := p.cfg.Models.Aliases
	resolveAliasFn := func(input string) string {
		key := strings.ToLower(strings.TrimSpace(input))
		if len(aliases) > 0 {
			if resolved, ok := aliases[key]; ok {
				return resolved
			}
		}
		if input == "" {
			if resolved, ok := aliases["sonnet"]; ok {
				return resolved
			}
		}
		return input
	}

	// resolveModelFn resolves user input to (endpoint, fullModelID).
	// Handles both "endpoint:alias_or_model" and bare "alias_or_model".
	// Returns endpoint="" when the caller should keep the current endpoint.
	resolveModelFn := func(input string) (string, string) {
		input = strings.TrimSpace(input)
		// First try resolving the whole input as an alias (aliases include endpoint prefix).
		resolved := resolveAliasFn(input)
		if resolved != input {
			// Alias resolved — split endpoint:model
			return config.ParseModel(resolved)
		}
		// Not an alias. If it contains ":", treat as endpoint:model_or_alias.
		if i := strings.IndexByte(input, ':'); i > 0 {
			ep := input[:i]
			rest := resolveAliasFn(input[i+1:])
			// If the alias resolved rest includes an endpoint prefix, extract just the modelID
			if strings.Contains(rest, ":") {
				_, rest = config.ParseModel(rest)
			}
			return ep, rest
		}
		// Bare model name, no alias match — infer endpoint from name
		ep := config.InferFormat(input)
		return ep, input
	}

	cmds.Register(command.NewModelCommand(
		func(ctx context.Context) string { return ag.SessionModel(sessionKeyFromCtx(ctx)) },
		func(ctx context.Context, endpoint string, m string) {
			var client provider.Client
			if endpoint != "" {
				client = p.resolveEndpointClient(endpoint, m)
			}
			ag.SetSessionModel(sessionKeyFromCtx(ctx), m, endpoint, client)
		},
		resolveModelFn,
		p.cfg.Models.Aliases,
	))

	cmds.Register(command.NewEffortCommand(
		func(ctx context.Context) string { return ag.SessionEffort(sessionKeyFromCtx(ctx)) },
		func(ctx context.Context, e string) { ag.SetSessionEffort(sessionKeyFromCtx(ctx), e) },
	))
	cmds.Register(command.NewThinkingCommand(
		func(ctx context.Context) string { return ag.SessionThinking(sessionKeyFromCtx(ctx)) },
		func(ctx context.Context, t string) { ag.SetSessionThinking(sessionKeyFromCtx(ctx), t) },
	))
	cmds.Register(command.NewToolsCommand(func() []command.ToolInfo {
		var infos []command.ToolInfo
		for _, t := range registry.All() {
			infos = append(infos, command.ToolInfo{Name: t.Name, Description: t.Description})
		}
		return infos
	}))
	cmds.Register(command.NewConfigCommand(func(ctx context.Context, args string) (string, error) {
		dw, _ := ctx.Value(command.DisplayWidthKey{}).(int)
		switch strings.TrimSpace(strings.ToLower(args)) {
		case "toml":
			return config.FormatConfigTOML(p.cfg, acfg), nil
		case "table":
			return strings.Join(config.FormatConfigGrouped(p.cfg, acfg, dw), "\x00"), nil
		case "available":
			return "```\n" + config.FormatAvailable(p.cfg, acfg, dw) + "\n```", nil
		default:
			return "/config toml — raw TOML of running config (secrets redacted)\n/config table — formatted table of current config values\n/config available — unset options with defaults", nil
		}
	}))
	cmds.Register(command.NewPromptsCommand(command.PromptsCmdDeps{
		DataFn: func() command.PromptsData {
			dirs := promptSearchDirs

			// All file-based prompts
			allPrompts := []command.PromptInfo{
				resolvePromptInfo("compaction_summary",
					resolveString(acfg.CompactionSummaryPrompt, p.cfg.Sessions.CompactionSummaryPrompt),
					"compaction-summary.md", prompts.CompactionSummary(), dirs),
				resolvePromptInfo("branch_orient_multiball",
					resolveOrientPath(acfg.BranchOrientationMultiballPrompt, p.cfg.Sessions.BranchOrientationMultiballPrompt, acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt),
					"branch-orientation-multiball.md", prompts.BranchOrientationMultiball(), dirs),
				resolvePromptInfo("branch_orient_headless",
					resolveOrientPath(acfg.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt, acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt),
					"branch-orientation-headless.md", prompts.BranchOrientationHeadless(), dirs),
				resolvePromptInfo("keepalive",
					acfg.Keepalive.Prompt,
					"keepalive.md", prompts.Keepalive(), dirs),
				resolvePromptInfo("background",
					acfg.Background.Prompt,
					"background.md", prompts.Background(), dirs),
				resolvePromptInfo("memory_formation",
					acfg.MemoryFormation.IntervalPrompt,
					"memory-formation.md", prompts.MemoryFormation(), dirs),
				resolvePromptInfo("memory_consolidation",
					acfg.MemoryFormation.ConsolidationPrompt,
					"memory-consolidation.md", prompts.MemoryConsolidation(), dirs),
				resolvePromptInfo("memory_session_end",
					acfg.MemoryFormation.SessionEndPrompt,
					"memory-formation.md", prompts.MemoryFormation(), dirs),
			}

			// Inline prompts (not file-based)
			allPrompts = append(allPrompts,
				inlinePromptInfo("compaction_handoff",
					resolveString(acfg.CompactionHandoffMsg, p.cfg.Sessions.CompactionHandoffMsg),
					prompts.CompactionHandoff()),
				inlinePromptInfo("braindead_warning",
					acfg.BraindeadPrompt, ""),
			)

			// Embedded defaults (for reinstall)
			embedded := map[string]string{
				"compaction-summary.md":           prompts.CompactionSummary(),
				"compaction-handoff.md":           prompts.CompactionHandoff(),
				"branch-orientation-multiball.md": prompts.BranchOrientationMultiball(),
				"branch-orientation-headless.md":  prompts.BranchOrientationHeadless(),
				"keepalive.md":                    prompts.Keepalive(),
				"background.md":                   prompts.Background(),
				"memory-formation.md":             prompts.MemoryFormation(),
				"memory-consolidation.md":         prompts.MemoryConsolidation(),
			}

			// Resolved and default texts per label (for diff)
			type promptDef struct {
				label, configPath, filename string
				embeddedDefault             string
			}
			fileDefs := []promptDef{
				{"compaction_summary", resolveString(acfg.CompactionSummaryPrompt, p.cfg.Sessions.CompactionSummaryPrompt), "compaction-summary.md", prompts.CompactionSummary()},
				{"branch_orient_multiball", resolveOrientPath(acfg.BranchOrientationMultiballPrompt, p.cfg.Sessions.BranchOrientationMultiballPrompt, acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt), "branch-orientation-multiball.md", prompts.BranchOrientationMultiball()},
				{"branch_orient_headless", resolveOrientPath(acfg.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt, acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt), "branch-orientation-headless.md", prompts.BranchOrientationHeadless()},
				{"keepalive", acfg.Keepalive.Prompt, "keepalive.md", prompts.Keepalive()},
				{"background", acfg.Background.Prompt, "background.md", prompts.Background()},
				{"memory_formation", acfg.MemoryFormation.IntervalPrompt, "memory-formation.md", prompts.MemoryFormation()},
				{"memory_consolidation", acfg.MemoryFormation.ConsolidationPrompt, "memory-consolidation.md", prompts.MemoryConsolidation()},
				{"memory_session_end", acfg.MemoryFormation.SessionEndPrompt, "memory-formation.md", prompts.MemoryFormation()},
			}
			resolvedTexts := make(map[string]string, len(fileDefs)+2)
			defaultTexts := make(map[string]string, len(fileDefs)+2)
			for _, d := range fileDefs {
				resolvedTexts[d.label] = prompts.ResolvePrompt(d.configPath, d.filename, d.embeddedDefault, dirs...)
				defaultTexts[d.label] = d.embeddedDefault
			}
			// Inline prompts
			handoffVal := resolveString(acfg.CompactionHandoffMsg, p.cfg.Sessions.CompactionHandoffMsg)
			if handoffVal == "" {
				resolvedTexts["compaction_handoff"] = prompts.CompactionHandoff()
			} else if handoffVal != "none" {
				resolvedTexts["compaction_handoff"] = handoffVal
			}
			defaultTexts["compaction_handoff"] = prompts.CompactionHandoff()
			if acfg.BraindeadPrompt != "" && acfg.BraindeadPrompt != "none" {
				resolvedTexts["braindead_warning"] = acfg.BraindeadPrompt
			}
			defaultTexts["braindead_warning"] = ""

			// Build set of configured paths for tagging files
			configuredPaths := make(map[string]bool)
			for _, pi := range allPrompts {
				if pi.Path != "" {
					configuredPaths[pi.Path] = true
				}
			}

			// Scan prompt directories
			var promptDirs []string
			var files []command.PromptFile
			sharedDir := filepath.Join(filepath.Dir(acfg.Workspace), "shared", "prompts")
			wsDir := filepath.Join(acfg.Workspace, "prompts")
			for _, dir := range []string{sharedDir, wsDir} {
				entries, err := os.ReadDir(dir)
				if err != nil {
					continue
				}
				promptDirs = append(promptDirs, dir)
				for _, e := range entries {
					if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
						continue
					}
					fullPath := filepath.Join(dir, e.Name())
					files = append(files, command.PromptFile{
						Dir:        dir,
						Name:       e.Name(),
						Configured: configuredPaths[fullPath],
					})
				}
			}

			knownFilenames := make(map[string]bool, len(embedded)+1)
			for name := range embedded {
				knownFilenames[name] = true
			}
			knownFilenames["first-run.md"] = true

			return command.PromptsData{
				AgentID:             acfg.ID,
				Prompts:             allPrompts,
				PromptDirs:          promptDirs,
				Files:               files,
				KnownFilenames:      knownFilenames,
				WorkspacePromptsDir: filepath.Join(acfg.Workspace, "prompts"),
				EmbeddedPrompts:     embedded,
				ResolvedTexts:       resolvedTexts,
				DefaultTexts:        defaultTexts,
			}
		},
		SendDocFn: func(path string) error {
			bot := p.botMgr.PrimaryBot(acfg.ID)
			if bot == nil {
				return fmt.Errorf("no bot available")
			}
			return bot.SendDocument(path)
		},
		DiffSummaryFn: func(ctx context.Context, customText, defaultText, name string) (string, error) {
			callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			// Pick cheap model based on agent provider
			cheapAlias := "haiku"
			if strings.HasPrefix(acfg.Model, "gemini-") {
				cheapAlias = "flash"
			}
			cheapModel := cheapAlias
			if full, ok := p.cfg.Models.Aliases[cheapAlias]; ok {
				cheapModel = full
			}
			// Resolve the right client for the cheap model
			// cheapModel may now have endpoint prefix from aliases
			var diffClient provider.Client
			if ep, mid := config.ParseModel(cheapModel); ep != "" {
				diffClient = p.resolveEndpointClient(ep, mid)
			}
			if diffClient == nil {
				diffClient = p.client
			}
			prompt := fmt.Sprintf("Below are two versions of the %q prompt. These prompts are injected into AI agent sessions to guide agent behaviour during specific operations (compaction, keepalive, memory formation, etc).\n\n--- DEFAULT (embedded) ---\n%s\n\n--- CURRENT (resolved from config) ---\n%s\n\nConcisely summarise: 1) what the default version instructs the agent to do, 2) what the current version instructs, 3) key differences.", name, defaultText, customText)
			resp, err := provider.Send(callCtx, diffClient, &provider.MessageRequest{
				Model:     cheapModel,
				MaxTokens: 1024,
				Messages:  []provider.Message{{Role: "user", Content: provider.TextContent(prompt)}},
			}, nil)
			if err != nil {
				return "", err
			}
			return provider.TextOf(resp.Content), nil
		},
	}))
	cmds.Register(command.NewLogCommand(p.cfg.Logging.EventFile))
	cmds.Register(command.NewErrorsCommand(p.cfg.Logging.EventFile))
	cmds.Register(command.NewVersionCommand(command.BuildInfo{
		Version:   version,
		GoVersion: goVersion,
		GitCommit: gitCommit,
		BuildTime: buildTime,
	}))
	cmds.Register(command.NewHelpCommand(cmds))

	// Dynamic mana command (configurable name: /mana, /juice, /credits, etc.)
	manaName := p.cfg.ManaWarnings.Name
	if manaName == "" {
		manaName = "mana"
	}
	manaEmoji := []string{"🔮", "✨", "🌙", "⚡", "🪄", "💎", "🌟", "🔥", "🧿", "🪬", "💫", "🌀", "🎇"}
	displayName := strings.ToUpper(manaName[:1]) + manaName[1:]
	manaFn := func(ctx context.Context) (string, error) {
		emoji := manaEmoji[rand.IntN(len(manaEmoji))]
		usage, err := p.usageClient.GetUsage(ctx)
		if err != nil {
			return fmt.Sprintf("%s Error fetching %s: %v", emoji, displayName, err), nil
		}
		percent := mana.FormatPercent(usage)
		if percent == "" {
			return fmt.Sprintf("%s %s: unknown", emoji, displayName), nil
		}
		result := fmt.Sprintf("%s %s: %s remaining", emoji, displayName, percent)
		if reset := mana.FormatReset(usage); reset != "" {
			result += fmt.Sprintf(" (resets %s)", reset)
		}
		return result, nil
	}
	cmds.Register(command.NewManaCommand(manaName, manaFn))

	// /usage — hidden alias for the mana command
	cmds.Register(&command.Command{
		Name:   "usage",
		Hidden: true,
		Execute: func(ctx context.Context, args string) (string, error) {
			return manaFn(ctx)
		},
	})

	// /m — short alias for the mana command
	if manaName != "m" {
		cmds.Register(&command.Command{
			Name:   "m",
			Hidden: true,
			Execute: func(ctx context.Context, args string) (string, error) {
				return manaFn(ctx)
			},
		})
	}

	// /reload command
	cmds.Register(command.NewReloadCommand(func() (string, error) {
		bootstrap.Reload()
		ag.InvalidateSystemCaches()
		checkSystemPromptSizes(bootstrap, p.cfg.Sessions, acfg.ID)
		newSkillRegistry := skills.Load(skillsDirs)
		var newExtraSystemBlocks []anthropic.SystemBlock
		if newSkillRegistry.Len() > 0 {
			newExtraSystemBlocks = []anthropic.SystemBlock{
				{Type: "text", Text: newSkillRegistry.SystemBlock(acfg.Workspace)},
			}
		}
		ag.ExtraSystemBlocks = newExtraSystemBlocks
		msg := fmt.Sprintf("Reloaded:\n- workspace files (system prompt)\n- %d skills\n\nNote: foci.toml config changes require a service restart to take effect. Prompt file changes take effect immediately.", newSkillRegistry.Len())
		return msg, nil
	}))

	// Custom script commands from config
	for _, cc := range p.cfg.Commands {
		cmds.Register(command.NewScriptCommand(cc.Name, cc.Description, cc.Script, cc.Timeout))
	}

	// Skill slash commands (command + script in frontmatter)
	for _, s := range skillRegistry.All() {
		if s.Command != "" && s.Script != "" {
			name := strings.TrimPrefix(s.Command, "/")
			cmds.Register(command.NewScriptCommand(name, s.Description, s.Script, 30))
		}
	}

	// /voice command
	cmds.Register(command.NewVoiceCommand(
		func(ctx context.Context) bool { return ag.VoiceMode(sessionKeyFromCtx(ctx)) },
		func(ctx context.Context, on bool) { ag.SetVoiceMode(sessionKeyFromCtx(ctx), on) },
	))

	// /multiball and /mb — per-agent pool first, shared pool fallback.
	// Forks from the requesting chat's session (per-chat routing).
	forkFn := func(ctx context.Context) (string, error) {
		if !p.botMgr.HasMultiball(acfg.ID) {
			return "", fmt.Errorf("no multiball bots configured")
		}
		secBot, ok := p.botMgr.AcquireMultiball(acfg.ID)
		if !ok {
			return "", fmt.Errorf("all multiball bots are busy")
		}

		// Re-wire the bot to this agent (needed when acquired from shared pool)
		secBot.SetAgentAndCommands(ag, cmds)
		applyAgentDisplaySettings(secBot, acfg, p.cfg)

		// Determine parent session: use the requesting chat's session
		parentKey := defaultSessionKey()
		if chatID, ok := ctx.Value(command.ChatIDKey{}).(int64); ok && chatID != 0 {
			parentKey = telegram.SessionKeyForChat(acfg.ID, chatID)
		}
		if parentKey == "" {
			secBot.SetSessionKey("") // release back to pool
			return "", fmt.Errorf("no active session to fork from")
		}

		branchID := fmt.Sprintf("mb-%d", time.Now().Unix())
		branchKey := fmt.Sprintf("agent:%s:multiball:%s", acfg.ID, branchID)

		orientPath := resolveOrientPath(acfg.BranchOrientationMultiballPrompt, p.cfg.Sessions.BranchOrientationMultiballPrompt, acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt)
		orientText := buildBranchOrientation(orientPath, branchKey, parentKey, "multiball", true, promptSearchDirs)
		if err := p.sessions.CreateBranchWithOptions(parentKey, branchKey, session.BranchOptions{
			OrientationMessage: orientText,
		}); err != nil {
			secBot.SetSessionKey("") // release back to pool
			return "", fmt.Errorf("create branch: %w", err)
		}

		secBot.SetSessionKey(branchKey)
		if primaryBot := p.botMgr.PrimaryBot(acfg.ID); primaryBot != nil {
			secBot.SetChatID(primaryBot.ChatID())
		}
		secBot.SendNotification("🎱 Forked from main. What do you need?")

		return fmt.Sprintf("Forked to @%s (session: %s)", secBot.Username(), branchKey), nil
	}
	cmds.Register(command.NewMultiballCommand(forkFn))
	cmds.Register(&command.Command{
		Name:        "mb",
		Description: "Fork session to a secondary bot (alias for /multiball)",
		Category:    "session",
		Hidden:      true,
		Execute: func(ctx context.Context, args string) (string, error) {
			return forkFn(ctx)
		},
	})
	agentNewDeps := &command.AgentNewDeps{
		ConfigPath:  p.configPath,
		DefaultsDir: filepath.Join(filepath.Dir(acfg.Workspace), "shared", "defaults"),
		HomeDir:     filepath.Dir(acfg.Workspace),
		ListFn:      p.agentListFn,
		SecretNames: func() []string { return agentStore.Names() },
		ResolveModel: resolveAliasFn,
	}
	cmds.Register(command.NewAgentsCommand(p.agentListFn, cmds, agentNewDeps))
	cmds.Register(command.NewCompactCommand(func(ctx context.Context, dryRun bool) (int, error) {
		if ag.Compactor == nil {
			return 0, fmt.Errorf("compaction is not configured")
		}
		sk := defaultSessionKey()
		if sk == "" {
			return 0, fmt.Errorf("no active session to compact")
		}
		mc, _ := p.sessions.MessageCount(sk)
		if mc < 5 {
			return 0, fmt.Errorf("too few messages to compact (%d)", mc)
		}
		if ag.CompactionNotifyFunc != nil {
			if dryRun {
				ag.CompactionNotifyFunc(sk, "⏳ Running compaction dry-run...")
			} else {
				ag.CompactionNotifyFunc(sk, "⏳ Compacting context...")
			}
		}
		system := bootstrap.SystemBlocks()
		summaryPrompt := prompts.ResolvePrompt(ag.CompactionSummaryPromptPath, "compaction-summary.md", prompts.CompactionSummary(), promptSearchDirs...)
		handoffMsg := ag.CompactionHandoffMsg
		if handoffMsg == "" {
			handoffMsg = prompts.ResolvePrompt("", "compaction-handoff.md", prompts.CompactionHandoff(), promptSearchDirs...)
		}
		summary, err := ag.Compactor.Compact(ctx, ag.SessionClient(sk), sk, system, summaryPrompt, handoffMsg, dryRun)
		if err != nil {
			return 0, fmt.Errorf("compaction failed: %w", err)
		}
		if dryRun {
			// Dry-run: always send summary as document, skip reload/cache reset
			if ag.CompactionDebugFunc != nil && summary != "" {
				ag.CompactionDebugFunc(sk, summary)
			} else if summary != "" {
				// No debug func configured — send directly via primary bot
				if bot := p.botMgr.PrimaryBot(acfg.ID); bot != nil {
					f, tmpErr := os.CreateTemp("", "compaction-dryrun-*.md")
					if tmpErr == nil {
						if _, writeErr := f.WriteString(summary); writeErr == nil {
							_ = f.Close()
							if sendErr := bot.SendDocument(f.Name()); sendErr != nil {
								log.Warnf("agent", "dry-run: send document: %v", sendErr)
							}
						} else {
							_ = f.Close()
						}
						_ = os.Remove(f.Name())
					}
				}
			}
			if ag.CompactionNotifyFunc != nil {
				ag.CompactionNotifyFunc(sk, "✅ Dry-run complete — summary sent.")
			}
		} else {
			if ag.CompactionNotifyFunc != nil {
				ag.CompactionNotifyFunc(sk, fmt.Sprintf("✅ Context compacted — %d messages summarised.", mc))
			}
			if ag.CompactionDebugFunc != nil && summary != "" {
				ag.CompactionDebugFunc(sk, summary)
			}
			bootstrap.Reload()
			ag.InvalidateSystemCaches()
			// Reset cache baseline — compaction changed the prefix
			ag.ResetCacheBaseline(sk)
		}
		return mc, nil
	}))
	cmds.Register(command.NewRepeatCommand(lastMsgStore))
	cmds.Register(command.NewSessionsCommand(command.SessionsDeps{
		AgentID: acfg.ID,
		ListFn: func() ([]command.SessionChatInfo, error) {
			chatSessions, err := p.sessions.ListChatSessions(acfg.ID)
			if err != nil {
				return nil, err
			}
			var result []command.SessionChatInfo
			for _, cs := range chatSessions {
				info := command.SessionChatInfo{
					ChatID:       cs.ChatID,
					MessageCount: cs.MessageCount,
					LastActivity: cs.LastActivity,
				}
				// Look up username from state store
				if p.stateStore != nil {
					var username string
					key := fmt.Sprintf("agent:%s:chat:%d:username", acfg.ID, cs.ChatID)
					if p.stateStore.Get(key, &username) {
						info.Username = username
					}
				}
				result = append(result, info)
			}
			return result, nil
		},
		SetDefaultFn: func(chatID int64) error {
			if p.stateStore == nil {
				return fmt.Errorf("no state store configured")
			}
			return p.stateStore.Set("agent:"+acfg.ID+":default_chat", chatID)
		},
		DefaultChatFn: func() int64 {
			if p.stateStore == nil {
				return 0
			}
			var chatID int64
			p.stateStore.Get("agent:"+acfg.ID+":default_chat", &chatID)
			return chatID
		},
		IndexFn: func(opts command.SessionIndexOpts) ([]command.SessionIndexInfo, error) {
			if p.sessionIndex == nil {
				return nil, fmt.Errorf("session index not available")
			}
			qopts := session.QueryOptions{
				SessionType: session.SessionType(opts.TypeFilter),
				Status:      session.SessionStatus(opts.StatusFilter),
				MaxAge:      opts.MaxAge,
				Limit:       50,
			}
			entries, err := p.sessionIndex.Query(qopts)
			if err != nil {
				return nil, err
			}
			var result []command.SessionIndexInfo
			for _, e := range entries {
				result = append(result, command.SessionIndexInfo{
					SessionKey:       e.SessionKey,
					CreatedAt:        e.CreatedAt,
					LastActivityAt:   e.LastActivityAt,
					ParentSessionKey: e.ParentSessionKey,
					SessionType:      string(e.SessionType),
					Status:           string(e.Status),
				})
			}
			return result, nil
		},
	}))
	cmds.Register(command.NewSecretsCommand(p.store))
	cmds.Register(command.NewBitwardenCommand(p.bwStore, p.cfg.Bitwarden.Enabled))
	cmds.Register(command.NewRestartCommand(nil))

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

// gracefulShutdown waits for all in-flight agent turns to complete, up to the
// configured timeout. This allows exec subprocesses and API calls to finish
// naturally before the context is cancelled.
func gracefulShutdown(agents map[string]*agentInstance, timeout time.Duration) {
	const tickInterval = 100 * time.Millisecond
	deadline := time.After(timeout)

	for {
		var anyBusy bool
		for _, inst := range agents {
			if inst.ag.IsProcessing() {
				anyBusy = true
				break
			}
		}
		if !anyBusy {
			return
		}
		select {
		case <-deadline:
			var parts []string
			now := time.Now()
			for id, inst := range agents {
				for _, d := range inst.ag.ProcessingDetails() {
					s := fmt.Sprintf("%s(session=%s", id, d.SessionKey)
					if d.ToolName != "" {
						s += fmt.Sprintf(", tool=%s", d.ToolName)
					}
					if d.Trigger != "" {
						s += fmt.Sprintf(", trigger=%s", d.Trigger)
					}
					s += fmt.Sprintf(", elapsed=%s)", now.Sub(d.StartTime).Truncate(time.Second))
					parts = append(parts, s)
				}
			}
			if len(parts) == 0 {
				// Shouldn't happen, but be safe
				log.Warnf("main", "graceful shutdown timed out after %s — agents still processing (no detail available)", timeout)
			} else {
				log.Warnf("main", "graceful shutdown timed out after %s — blocking: %s", timeout, strings.Join(parts, ", "))
			}
			return
		default:
			time.Sleep(tickInterval)
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
func injectWelcomeFile(path string, agents map[string]*agentInstance, agentOrder []string, sessions *session.Store) string {
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

		branchID := fmt.Sprintf("%s-%d", branchType, time.Now().Unix())
		branchKey := fmt.Sprintf("agent:%s:cron:%s", agentID, branchID)

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
	isDefault := md5.Sum([]byte(resolved)) == md5.Sum([]byte(embeddedDefault))

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
	isDefault := md5.Sum([]byte(value)) == md5.Sum([]byte(embeddedDefault))
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
	branchID := fmt.Sprintf("session-end-%d", time.Now().Unix())
	branchKey := sessionKey + ":branch:" + branchID
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
