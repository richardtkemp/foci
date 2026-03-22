package main

import (
	"context"
	"fmt"
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
	"foci/internal/nudge"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/secrets"
	"foci/internal/session"
	"foci/internal/skills"
	"foci/internal/tools"
	"foci/internal/voice"
	"foci/internal/warnings"
	"foci/internal/workspace"
	"foci/shared/prompts"
)

// coreToolsResult holds the side-products of core tool registration.
type coreToolsResult struct {
	tmuxTool       *tools.Tool
	tmuxClearAll   func()
	tmuxWatchCount func() int
	tmuxMigrateKey func(string, string)
}

// registerCoreTools registers exec, tmux, browser, file I/O, summary, and HTTP tools.
func registerCoreTools(registry *tools.Registry, p setupParams, agentStore *secrets.Store, notifier *tools.AsyncNotifier, groupResolver *config.GroupResolver, fallbackFn provider.FallbackFunc) coreToolsResult {
	acfg := p.acfg

	tc := config.Merge(acfg.Tools.ToolConfig, p.cfg.Tools.ToolConfig)
	sc := config.Merge(acfg.Tools.SummaryConfig, p.cfg.Tools.SummaryConfig)
	execAutoBg := config.DerefInt(tc.ExecAutoBackground)
	maxUploadSize := config.DerefInt64(tc.MaxUploadFileSize)
	spillThreshold := config.DerefInt(sc.MaxResultChars)

	// Inject FOCI_ADDR and FOCI_GW_SOCK so agents can run foci CLI commands
	// (send, branch, ping, etc.) without sourcing vars manually.
	// FOCI_GW_SOCK is a Unix socket path (not a secret) — the CLI uses it
	// for same-user authentication via kernel peer credentials, so no API
	// key needs to appear in the child environment.
	var execExtraEnv []string
	if p.cfg.HTTP.Port > 0 {
		bind := p.cfg.HTTP.Bind
		if bind == "" || bind == "0.0.0.0" {
			bind = "127.0.0.1"
		}
		execExtraEnv = append(execExtraEnv, fmt.Sprintf("FOCI_ADDR=%s:%d", bind, p.cfg.HTTP.Port))
	}
	if p.gwSocketPath != "" {
		execExtraEnv = append(execExtraEnv, "FOCI_GW_SOCK="+p.gwSocketPath)
	}

	registry.Register(tools.NewExecTool(agentStore, p.bwStore, execAutoBg, notifier, acfg.Workspace, registry, spillThreshold, p.cfg.Tools.TempDir, execExtraEnv))

	var result coreToolsResult

	// Only register tmux tool if tmux is available in PATH
	if _, err := exec.LookPath("tmux"); err == nil {
		tmuxAutopilot := config.DerefBool(tc.TmuxAutopilot)
		tmuxWatchThreshold := config.DerefStr(tc.TmuxWatchThreshold)
		tmuxWatchThresholdSec := 30
		if d, err := time.ParseDuration(tmuxWatchThreshold); err == nil {
			tmuxWatchThresholdSec = int(d.Seconds())
		}
		tmuxSessionTTLStr := config.DerefStr(tc.TmuxSessionTTL)
		var tmuxSessionTTL time.Duration
		if tmuxSessionTTLStr != "0" {
			if d, err := time.ParseDuration(tmuxSessionTTLStr); err == nil {
				tmuxSessionTTL = d
			}
		}
		result.tmuxWatchCount, result.tmuxTool, result.tmuxClearAll, result.tmuxMigrateKey = tools.NewTmuxTool(p.cfg.Tools.TmuxCols, p.cfg.Tools.TmuxRows, notifier, p.sessionIndex, acfg.ID, tmuxAutopilot, tmuxWatchThresholdSec, tmuxSessionTTL, "")
		registry.Register(result.tmuxTool)
	}

	// Only register browser tool if enabled
	bc := config.Merge(acfg.Browser, p.cfg.Browser)
	if config.DerefBool(bc.Enabled) {
		browserMgr := tools.NewBrowserManager(&bc)
		registry.Register(tools.NewBrowserTool(browserMgr))
	}

	blockedPaths := resolveBlockedPaths(acfg, p.cfg)
	if len(blockedPaths) > 0 {
		log.Infof("setup", "agent %s: %d blocked write/edit path(s) configured", acfg.ID, len(blockedPaths))
	}
	registry.Register(tools.NewReadTool(agentStore, acfg.Workspace))
	registry.Register(tools.NewWriteTool(agentStore, acfg.Workspace, blockedPaths))
	registry.Register(tools.NewEditTool(agentStore, acfg.Workspace, blockedPaths))
	registry.Register(tools.NewSummaryTool(p.client, p.clientProvider, groupResolver, acfg.Workspace, fallbackFn))
	registry.Register(tools.NewHTTPRequestTool(agentStore, p.bwStore, p.cfg.Tools.TempDir, execAutoBg, maxUploadSize, notifier))

	return result
}

// registerWebTools registers web search and fetch tools.
// Returns server-side tool definitions for provider-hosted tools.
func registerWebTools(registry *tools.Registry, p setupParams) []provider.ToolDef {
	acfg := p.acfg
	var serverTools []provider.ToolDef

	tc := config.Merge(acfg.Tools.ToolConfig, p.cfg.Tools.ToolConfig)
	searchProvider := config.DerefStr(tc.SearchProvider)
	if searchProvider == "anthropic" {
		serverTools = append(serverTools, buildServerTool("web_search_20250305", "web_search",
			p.cfg.Tools.WebSearchMaxUses, p.cfg.Tools.WebSearchAllowedDomains, p.cfg.Tools.WebSearchBlockedDomains))
	} else if searchProvider == "brave" && p.braveKey != "" {
		registry.Register(tools.NewWebSearchTool(p.braveKey))
	}

	fetchProvider := config.DerefStr(tc.FetchProvider)
	if fetchProvider == "anthropic" {
		serverTools = append(serverTools, buildServerTool("web_fetch_20250910", "web_fetch",
			p.cfg.Tools.WebFetchMaxUses, p.cfg.Tools.WebFetchAllowedDomains, p.cfg.Tools.WebFetchBlockedDomains))
	} else {
		registry.Register(tools.NewWebFetchTool())
	}

	return serverTools
}

// registerMemoryAndExtTools registers memory, scratchpad, todo, task list,
// bitwarden, and MCP tools. Returns the MCP manager.
func registerMemoryAndExtTools(registry *tools.Registry, p setupParams, agLazy func() *agent.Agent) *mcpkg.Manager {
	acfg := p.acfg

	if len(p.memBackends) > 0 {
		registry.Register(tools.NewMemorySearchTool(p.memBackends, p.cfg.Memory.SearchBackends, p.convReader))
	}
	if p.scratchpadStore != nil {
		registry.Register(tools.NewScratchpadTool(p.scratchpadStore, acfg.ID))
	}
	if p.todoStore != nil {
		registry.Register(tools.NewTodoTool(p.todoStore, acfg.ID))
	}
	if p.taskListStore != nil {
		registry.Register(tools.NewTaskListTool(p.taskListStore, acfg.ID, func(sk, msg string) {
			ag := agLazy()
			for _, fn := range ag.TaskListNotifyFunc {
				fn(sk, msg)
			}
		}))
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

	return mcpMgr
}

// bootstrapResult holds the output of setupBootstrapAndSkills.
type bootstrapResult struct {
	bootstrap         *workspace.Bootstrap
	skillRegistry     *skills.Registry
	extraSystemBlocks []provider.SystemBlock
	skillsDirs        []string
}

// setupBootstrapAndSkills loads the workspace bootstrap and skill registry.
func setupBootstrapAndSkills(p setupParams, agentStore *secrets.Store) bootstrapResult {
	acfg := p.acfg

	bootstrap := workspace.NewBootstrap(acfg.Workspace, acfg.Defaults.SystemFiles)
	bootstrap.SetSecretNames(agentStore.Names(), p.bwStore != nil)
	checkSystemPromptSizes(bootstrap, p.cfg.Sessions, acfg.ID)

	home := filepath.Dir(acfg.Workspace)
	skillsDirs := skills.ResolveDirs(home, acfg.Workspace, p.cfg.Skills.Dir, acfg.SkillsDir)
	skillRegistry := skills.Load(skillsDirs)
	var extraSystemBlocks []provider.SystemBlock
	if skillRegistry.Len() > 0 {
		extraSystemBlocks = []provider.SystemBlock{
			{Type: "text", Text: skillRegistry.SystemBlock(acfg.Workspace)},
		}
		log.Infof("main", "agent %q: loaded %d skills", acfg.ID, skillRegistry.Len())
	}
	bsc := config.Merge(acfg.Tools.SummaryConfig, p.cfg.Tools.SummaryConfig)
	maxRC := config.DerefInt(bsc.MaxResultChars)
	checkSkillSizes(skillRegistry, maxRC, acfg.ID)

	return bootstrapResult{
		bootstrap:         bootstrap,
		skillRegistry:     skillRegistry,
		extraSystemBlocks: extraSystemBlocks,
		skillsDirs:        skillsDirs,
	}
}

// buildCompactor creates a Compactor configured for this agent.
// Returns the compactor and the resolved compaction threshold.
func buildCompactor(p setupParams, fallbackFn provider.FallbackFunc) (*compaction.Compactor, float64) {
	acfg := p.acfg
	cc := config.Merge(acfg.Sessions.CompactionConfig, p.cfg.Sessions.CompactionConfig)
	compactionThreshold := config.DerefFloat(cc.CompactionThreshold)
	if compactionThreshold == 0 {
		compactionThreshold = 0.8 // code default
	}
	preserveMessages := config.DerefInt(cc.CompactionPreserveMessages)
	compactor := compaction.NewCompactor(p.sessions, compactionThreshold)
	compactor.WithConfig(
		p.cfg.Sessions.CompactionMaxTokens,
		p.cfg.Sessions.CompactionMinMessages,
		preserveMessages,
	)
	compactor.ModelParamsFn = modelParamsFn(p.cfg.Models)
	compactor.ModelMetaFn = modelMetaFn(p.cfg.Models)
	compactor.Scratchpad = p.scratchpadStore
	compactor.TaskListStore = p.taskListStore
	compactor.AgentID = acfg.ID
	compactor.FallbackFunc = fallbackFn
	compactor.ClientProvider = p.clientProvider

	return compactor, compactionThreshold
}

// registerSessionTools registers send_to_chat and send_to_session tools.
// Returns the resolved agent TTS and TTS replacements for reuse in platform setup.
func registerSessionTools(registry *tools.Registry, p setupParams, connMgr platform.ConnectionManager, notifier *tools.AsyncNotifier) (voice.TTS, map[string]string) {
	acfg := p.acfg

	vc := config.Merge(acfg.Defaults.VoiceConfig, p.cfg.Defaults.VoiceConfig)
	ttsRepls := voice.MergeReplacements(p.cfg.Defaults.TTSReplacements, acfg.Defaults.TTSReplacements)
	agentTTS := resolveTTS(p.ttsMap, p.cfg.TTS, config.DerefStr(vc.TTS), config.DerefFloat(vc.TTSRate), ttsRepls)
	registry.Register(tools.NewSendToChatTool(func(sessionKey string) platform.Sender {
		conn := connMgr.ForSessionOrPrimary(sessionKey, acfg.ID)
		if conn == nil {
			return nil
		}
		return conn
	}, agentTTS))

	sessionNotifyFn := newSessionNotifyFn(p.agentResolverFn, p.ctx, connMgr)
	var resolveKeyFn tools.SessionKeyResolverFn
	if p.sessionIndex != nil {
		resolveKeyFn = p.sessionIndex.ResolvePartialKey
	}
	registry.Register(tools.NewSendToSessionTool(p.sessions, notifier, sessionNotifyFn, resolveKeyFn))

	return agentTTS, ttsRepls
}

// setupNudgeSystem configures the nudge scheduler and reload logic on the agent.
func setupNudgeSystem(ag *agent.Agent, acfg config.AgentConfig, cfg *config.Config, defaultSessionKey func() string, toolRegistry *tools.Registry, skillRegistry *skills.Registry) {
	nc := config.Merge(acfg.Defaults.NudgeConfig, cfg.Defaults.NudgeConfig)
	nudgeEnabled := nc.NudgeEnable == nil || *nc.NudgeEnable                       // default true
	nudgeDefaultEnabled := nc.NudgeDefaultEnable == nil || *nc.NudgeDefaultEnable  // default true
	braindeadThreshold := config.DerefInt(nc.NudgeDefaultBraindeadThreshold)
	hasBraindead := braindeadThreshold > 0
	if !nudgeEnabled && !nudgeDefaultEnabled && !hasBraindead {
		return
	}

	// Braindead warning rule (fires every N tool calls).
	braindeadRules := nudge.BraindeadRule(braindeadThreshold, config.DerefStr(nc.NudgeDefaultBraindeadPrompt))

	// Load character-derived rules.
	var charRules []nudge.Rule
	rulesPath := nudge.RulesPath(acfg.Workspace)
	if nudgeEnabled {
		rs, err := nudge.LoadRules(rulesPath)
		if err != nil {
			log.Warnf("main", "agent %s: load nudge rules: %v", acfg.ID, err)
		}
		if rs != nil {
			charRules = rs.Rules
		}
	}

	// Generate default tool/skill reminder rules.
	var defaultRules []nudge.Rule
	if nudgeDefaultEnabled {
		var toolNames []string
		for _, t := range toolRegistry.All() {
			toolNames = append(toolNames, t.Name)
		}
		var skillSummaries []nudge.SkillSummary
		if skillRegistry != nil {
			for _, s := range skillRegistry.All() {
				skillSummaries = append(skillSummaries, nudge.SkillSummary{Name: s.Name, Description: s.Description})
			}
		}
		freq := config.DerefInt(nc.NudgeDefaultFrequency)
		if freq <= 0 {
			freq = 50
		}
		defaultRules = nudge.DefaultRules(toolNames, skillSummaries, freq)
	}

	// Scratchpad staleness reminder: fires every N turns, but only when entries exist.
	scratchpadFreq := config.DerefInt(nc.NudgeDefaultScratchpadFrequency)
	var scratchpadRules []nudge.Rule
	if nudgeDefaultEnabled && ag.ScratchpadStore != nil && scratchpadFreq > 0 {
		agentID := ag.AgentID
		store := ag.ScratchpadStore
		scratchpadRules = nudge.ScratchpadRule(scratchpadFreq, func() bool {
			entries, err := store.List(agentID)
			return err == nil && len(entries) > 0
		})
	}

	// Braindead first (highest effective priority), then character, then defaults, then scratchpad.
	cooldown := config.DerefInt(nc.NudgeCooldown)
	maxPerBatch := config.DerefInt(nc.NudgeMaxPerBatch)
	allRules := append(braindeadRules, append(charRules, append(defaultRules, scratchpadRules...)...)...)
	if len(allRules) > 0 {
		rs := &nudge.RuleSet{Rules: allRules}
		ag.Nudger = nudge.NewScheduler(rs, cooldown, maxPerBatch)
		log.Infof("main", "agent %s: loaded %d nudge rules (%d braindead, %d character, %d default, %d scratchpad)", acfg.ID, len(allRules), len(braindeadRules), len(charRules), len(defaultRules), len(scratchpadRules))
	}

	ag.NudgePreAnswerGate = config.DerefBool(nc.NudgePreAnswerGate)
	preAnswerMinTools := config.DerefInt(nc.NudgePreAnswerMinTools)
	if preAnswerMinTools <= 0 {
		preAnswerMinTools = 2
	}
	ag.NudgePreAnswerMinTools = preAnswerMinTools

	if !nudgeEnabled {
		return // no character rules → no reload/extraction logic needed
	}

	// NudgeReloadFunc: on bootstrap reload, optionally extract new rules
	// from character files (if nudge_auto_extract), then refresh from disk.
	fileOrder := acfg.Defaults.SystemFiles
	if len(fileOrder) == 0 {
		fileOrder = workspace.DefaultFileOrder
	}
	autoExtract := nc.NudgeAutoExtract == nil || *nc.NudgeAutoExtract // default true
	nudgeReloadFromDisk := func() {
		rs, err := nudge.LoadRules(rulesPath)
		if err != nil {
			log.Warnf("nudge", "agent %s: reload rules: %v", acfg.ID, err)
			return
		}
		var reloaded []nudge.Rule
		if rs != nil {
			reloaded = rs.Rules
		}
		merged := append(reloaded, append(defaultRules, scratchpadRules...)...)
		if len(merged) > 0 {
			ag.Nudger = nudge.NewScheduler(&nudge.RuleSet{Rules: merged}, cooldown, maxPerBatch)
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
				ag.SetSessionNoCompact(branchKey, true)
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

// setupRedaction configures secret redaction on the agent.
func setupRedaction(ag *agent.Agent, p setupParams, agentStore *secrets.Store) {
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
}

// setupWarningQueue configures warning injection queues on the agent.
// Creates separate queues for agent session injection and chat notifications,
// each with independent severity filtering based on their InjectionLevel.
func setupWarningQueue(ag *agent.Agent, acfg config.AgentConfig, cfg *config.Config) {
	warningWindow, err := time.ParseDuration(cfg.Logging.WarningWindowDuration)
	if err != nil {
		warningWindow = 5 * time.Minute
	}

	agentLevel := maxInjectionLevel(acfg, cfg, config.DebugConfig.InjectAgentWarningsLevel)
	if agentLevel.Enabled() {
		ag.WarningQueue = warnings.NewQueue(cfg.Logging.WarningMaxPerWindow, warningWindow)
		if !agentLevel.IncludeWarnings() {
			ag.WarningQueue.SetErrorsOnly(true)
		}
	}

	chatLevel := maxInjectionLevel(acfg, cfg, config.DebugConfig.InjectChatWarningsLevel)
	if chatLevel.Enabled() {
		ag.ChatWarningQueue = warnings.NewQueue(cfg.Logging.WarningMaxPerWindow, warningWindow)
		if !chatLevel.IncludeWarnings() {
			ag.ChatWarningQueue.SetErrorsOnly(true)
		}
	}
}

// setupManaWatcher configures mana threshold warnings on the agent.
func setupManaWatcher(ag *agent.Agent, p setupParams) {
	mc := config.Merge(p.acfg.Mana, p.cfg.Mana)
	if len(mc.Thresholds) == 0 {
		return
	}

	ag.ManaWatcher = agent.NewManaWatcher(config.DerefStr(mc.Name), mc.Thresholds)
	ag.ManaWatcher.SetSessionIndex(p.sessionIndex, p.acfg.ID)
	ag.ManaWatcher.Restore()
	ag.ManaWatcher.SetRestoreThreshold(config.DerefInt(mc.RestoreThreshold))
}

// registerSpawnTool registers the spawn tool for forking sub-agents.
func registerSpawnTool(registry *tools.Registry, p setupParams, bootstrap *workspace.Bootstrap, agLazy func() tools.SpawnAgent, notifier *tools.AsyncNotifier, promptSearchDirs []string, setNoCompact func(string, bool), groupResolver *config.GroupResolver, defaultFormat string, fallbackFn provider.FallbackFunc) {
	acfg := p.acfg

	spawnOrientPath := config.DerefStr(config.First(acfg.Sessions.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt))
	al := config.Merge(acfg.Defaults.AgentLoopConfig, p.cfg.Defaults.AgentLoopConfig)
	tc := config.Merge(acfg.Tools.ToolConfig, p.cfg.Tools.ToolConfig)
	spawnDeps := tools.SpawnDeps{
		Client:          p.client,
		ClientProvider:  p.clientProvider,
		Bootstrap:       bootstrap,
		Registry:        registry,
		Sessions:        &sessionBranchAdapter{store: p.sessions},
		AgentID:         acfg.ID,
		GroupResolver:   groupResolver,
		FallbackFunc:    fallbackFn,
		FallbackModel:   groupResolver.PowerfulModel(),
		FallbackFormat:  defaultFormat,
		MaxInherit:      config.DerefInt(tc.MaxConcurrentSpawns),
		MaxToolLoops:    config.DerefInt(al.MaxToolLoops),
		ExploreMaxDepth: config.DerefInt(tc.ExploreMaxDepth),
		Notifier:        notifier,
		OrientationBuilder: func(branchKey, parentKey string) string {
			return prompts.BuildBranchOrientation(spawnOrientPath, branchKey, parentKey, "spawn", false, promptSearchDirs)
		},
		SetNoCompact: setNoCompact,
	}
	registry.Register(tools.NewSpawnTool(spawnDeps, agLazy))
}

// platformConnectionResult holds the callbacks wired by platform connection setup.
type platformConnectionResult struct {
	defaultSessionKeyFn func() string
	configureFacetFn    func(platform.Connection)
	displayDefaultsFn   func() platform.DisplaySettings
}

// setupPlatformConnections creates and registers platform connections for the agent.
func setupPlatformConnections(
	ag *agent.Agent,
	p setupParams,
	cmds *command.Registry,
	cc command.CommandContext,
	lastMsgStore *command.LastMessageStore,
	ttsRepls map[string]string,
	promptSearchDirs []string,
	tmuxMigrateKey func(string, string),
) platformConnectionResult {
	acfg := p.acfg
	var result platformConnectionResult

	reclaimOrientPath := config.DerefStr(config.First(acfg.Sessions.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt))
	reclaimMfCfg := acfg.MemoryFormation
	reclaimSearchDirs := promptSearchDirs

	vc := config.Merge(acfg.Defaults.VoiceConfig, p.cfg.Defaults.VoiceConfig)
	results := p.plat.SetupAgentConnection(platform.AgentConnectionParams{
		AgentID:        acfg.ID,
		Handler:        ag,
		Commands:       cmds,
		CommandContext: cc,
		LastMsgStore:   lastMsgStore,
		AgentConfig:    acfg,
		STT:            resolveSTT(p.sttMap, p.cfg.STT, config.DerefStr(vc.STT), voice.MergeReplacements(p.cfg.Defaults.STTReplacements, acfg.Defaults.STTReplacements)),
		TTS:            resolveTTS(p.ttsMap, p.cfg.TTS, config.DerefStr(vc.TTS), config.DerefFloat(vc.TTSRate), ttsRepls),
		ReclaimHook: func(sessionKey string) {
			agent.FireSessionEndMemory(ag, p.sessions, sessionKey, reclaimMfCfg, func(bk, pk, bt string) string {
				return prompts.BuildBranchOrientation(reclaimOrientPath, bk, pk, bt, false, reclaimSearchDirs)
			}, reclaimSearchDirs, p.ctx, false)
		},
		DisplayOverrideFn: func(sessionKey string) platform.DisplaySettings {
			return platform.DisplaySettings{
				ShowToolCalls: ag.SessionShowToolCalls(sessionKey),
				ShowThinking:  ag.SessionDisplayShowThinking(sessionKey),
				StreamOutput:  ag.SessionStreamOutput(sessionKey),
				DisplayWidth:  ag.SessionDisplayWidth(sessionKey),
			}
		},
	})
	var sessionKeyFns []func() string
	for _, r := range results {
		if r.DefaultSessionKeyFn != nil {
			sessionKeyFns = append(sessionKeyFns, r.DefaultSessionKeyFn)
		}
		if r.ConfigureFacetConn != nil {
			result.configureFacetFn = r.ConfigureFacetConn
		}
		if r.DisplayDefaultsFn != nil {
			result.displayDefaultsFn = r.DisplayDefaultsFn
		}
	}
	// Each platform's DefaultSessionKeyFn is scoped to its own PlatformName,
	// so we try all of them and return the first match.
	switch len(sessionKeyFns) {
	case 1:
		result.defaultSessionKeyFn = sessionKeyFns[0]
	default:
		result.defaultSessionKeyFn = func() string {
			for _, fn := range sessionKeyFns {
				if sk := fn(); sk != "" {
					return sk
				}
			}
			return ""
		}
	}

	wireAgentPlatformCallbacks(ag, acfg, p.cfg, p.plat, p.connMgr, p.sessionIndex, tmuxMigrateKey)

	return result
}

// logRegisteredTools logs the names of all registered client and server tools.
func logRegisteredTools(registry *tools.Registry, serverTools []provider.ToolDef, agentID string) {
	allTools := registry.All()
	toolNames := make([]string, len(allTools))
	for i, t := range allTools {
		toolNames[i] = t.Name
	}
	log.Infof("main", "agent %q: registered %d tools: [%s]", agentID, len(toolNames), strings.Join(toolNames, ", "))
	if len(serverTools) > 0 {
		stNames := make([]string, len(serverTools))
		for i, st := range serverTools {
			stNames[i] = st.Name()
		}
		log.Infof("main", "agent %q: server tools: [%s]", agentID, strings.Join(stNames, ", "))
	}
}

