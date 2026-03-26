package main

import (
	"context"
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
func registerCoreTools(registry *tools.Registry, p setupParams, client provider.Client, agentStore *secrets.Store, notifier *tools.AsyncNotifier, groupResolver *config.GroupResolver, fallbackFn provider.FallbackFunc) coreToolsResult {
	acfg := p.acfg

	tc := p.resolved.Tools
	sc := p.resolved.Summary
	execAutoBg := tc.ExecAutoBackground
	maxUploadSize := tc.MaxUploadFileSize
	spillThreshold := sc.MaxResultChars
	fileMode, _ := config.ParseFileMode(p.cfg.FileMode)

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
		tmuxAutopilot := tc.TmuxAutopilot
		tmuxWatchThreshold := tc.TmuxWatchThreshold
		tmuxWatchThresholdSec := 30
		if d, err := time.ParseDuration(tmuxWatchThreshold); err == nil {
			tmuxWatchThresholdSec = int(d.Seconds())
		}
		tmuxSessionTTLStr := tc.TmuxSessionTTL
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
	bc := p.resolved.Browser
	if bc.Enabled {
		browserMgr := tools.NewBrowserManager(&bc, fileMode)
		registry.Register(tools.NewBrowserTool(browserMgr))
	}

	blockedPaths := resolveBlockedPaths(acfg, p.cfg)
	if len(blockedPaths) > 0 {
		log.Infof("setup", "agent %s: %d blocked write/edit path(s) configured", acfg.ID, len(blockedPaths))
	}
	registry.Register(tools.NewReadTool(agentStore, acfg.Workspace))
	registry.Register(tools.NewWriteTool(agentStore, acfg.Workspace, blockedPaths, fileMode))
	registry.Register(tools.NewEditTool(agentStore, acfg.Workspace, blockedPaths, fileMode))
	registry.Register(tools.NewSummaryTool(client, p.clientProvider, groupResolver, acfg.Workspace, fallbackFn))
	registry.Register(tools.NewHTTPRequestTool(agentStore, p.bwStore, p.cfg.Tools.TempDir, execAutoBg, maxUploadSize, notifier, fileMode))

	return result
}

// registerWebTools registers web search and fetch tools.
// Returns server-side tool definitions for provider-hosted tools.
func registerWebTools(registry *tools.Registry, p setupParams) []provider.ToolDef {
	var serverTools []provider.ToolDef

	tc := p.resolved.Tools
	searchProvider := tc.SearchProvider
	if searchProvider == "anthropic" {
		serverTools = append(serverTools, buildServerTool("web_search_20250305", "web_search",
			p.cfg.Tools.WebSearchMaxUses, p.cfg.Tools.WebSearchAllowedDomains, p.cfg.Tools.WebSearchBlockedDomains))
	} else if searchProvider == "brave" && p.braveKey != "" {
		registry.Register(tools.NewWebSearchTool(p.braveKey))
	}

	fetchProvider := tc.FetchProvider
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
		registry.Register(tools.NewMemorySearchTool(p.memBackends, p.resolved.MemorySearch.SearchBackend, p.convReader))
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

	bootstrap := workspace.NewBootstrap(acfg.Workspace, acfg.System.SystemFiles)
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
	maxRC := p.resolved.Summary.MaxResultChars
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
	cc := p.resolved.Compaction
	compactionThreshold := cc.CompactionThreshold
	preserveMessages := cc.CompactionPreserveMessages
	compactor := compaction.NewCompactor(p.sessions, compactionThreshold)
	compactor.WithConfig(
		p.cfg.Sessions.CompactionMaxTokens,
		p.cfg.Sessions.CompactionMinMessages,
		preserveMessages,
	)
	compactor.ModelDefaultsFn = modelDefaultsFn(p.cfg.Models)
	compactor.ModelMetaFn = modelMetaFn(p.cfg.Models)
	compactor.Scratchpad = p.scratchpadStore
	compactor.TaskListStore = p.taskListStore
	compactor.AgentID = p.acfg.ID
	compactor.FallbackFunc = fallbackFn
	compactor.ClientProvider = p.clientProvider

	return compactor, compactionThreshold
}

// registerSessionTools registers send_to_chat and send_to_session tools.
// Returns the resolved agent TTS and TTS replacements for reuse in platform setup.
func registerSessionTools(registry *tools.Registry, p setupParams, connMgr platform.ConnectionManager, notifier *tools.AsyncNotifier) (voice.TTS, map[string]string) {
	acfg := p.acfg

	vc := p.resolved.Voice
	ttsRepls := voice.MergeReplacements(p.cfg.Voice.TTSReplacements, acfg.Voice.TTSReplacements)
	agentTTS := resolveTTS(p.ttsMap, p.cfg.TTS, vc.TTS, vc.TTSRate, ttsRepls)
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
func setupNudgeSystem(ag *agent.Agent, acfg config.AgentConfig, nc config.ResolvedNudge, connMgr platform.ConnectionManager, sessions *session.Store, toolRegistry *tools.Registry, skillRegistry *skills.Registry, fileMode os.FileMode) {
	nudgeEnabled := nc.NudgeEnable
	nudgeDefaultEnabled := nc.NudgeDefaultEnable
	braindeadThreshold := nc.NudgeDefaultBraindeadThreshold
	hasBraindead := braindeadThreshold > 0
	if !nudgeEnabled && !nudgeDefaultEnabled && !hasBraindead {
		return
	}

	// Braindead warning rule (fires every N tool calls).
	braindeadRules := nudge.BraindeadRule(braindeadThreshold, nc.NudgeDefaultBraindeadPrompt)

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
		if toolRegistry != nil {
			for _, t := range toolRegistry.All() {
				toolNames = append(toolNames, t.Name)
			}
		}
		var skillSummaries []nudge.SkillSummary
		if skillRegistry != nil {
			for _, s := range skillRegistry.All() {
				skillSummaries = append(skillSummaries, nudge.SkillSummary{Name: s.Name, Description: s.Description})
			}
		}
		defaultRules = nudge.DefaultRules(toolNames, skillSummaries, nc.NudgeDefaultFrequency)
	}

	// Scratchpad staleness reminder: fires every N turns, but only when entries exist.
	scratchpadFreq := nc.NudgeDefaultScratchpadFrequency
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
	cooldown := nc.NudgeCooldown
	maxPerBatch := nc.NudgeMaxPerBatch
	allRules := append(braindeadRules, append(charRules, append(defaultRules, scratchpadRules...)...)...)
	if len(allRules) > 0 {
		rs := &nudge.RuleSet{Rules: allRules}
		ag.Nudger = nudge.NewScheduler(rs, cooldown, maxPerBatch)
		log.Infof("main", "agent %s: loaded %d nudge rules (%d braindead, %d character, %d default, %d scratchpad)", acfg.ID, len(allRules), len(braindeadRules), len(charRules), len(defaultRules), len(scratchpadRules))
	}

	ag.NudgePreAnswerGate = nc.NudgePreAnswerGate
	ag.NudgePreAnswerMinTools = nc.NudgePreAnswerMinTools

	if !nudgeEnabled {
		return // no character rules → no reload/extraction logic needed
	}

	// NudgeReloadFunc: on bootstrap reload, optionally extract new rules
	// from character files (if nudge_auto_extract), then refresh from disk.
	fileOrder := acfg.System.SystemFiles
	if len(fileOrder) == 0 {
		fileOrder = workspace.DefaultFileOrder
	}
	autoExtract := nc.NudgeAutoExtract
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
		extractor := nudge.NewExtractor(acfg.Workspace, fileOrder, fileMode)
		_, needed := extractor.NeedsExtraction()
		if needed {
			go func() {
				ctx := context.Background()
				parentKey := mostRecentSessionKey(ag, connMgr, acfg.ID)
				if parentKey == "" {
					log.Warnf("nudge", "agent %s: no default session for extraction branch", acfg.ID)
					return
				}
				branchKey, err := sessions.CreateBranchWithOptions(parentKey, session.BranchOptions{
					NoResetHook: true,
					BranchType:  "nudge-extraction",
				})
				if err != nil {
					log.Warnf("nudge", "agent %s: create branch: %v", acfg.ID, err)
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
func setupWarningQueue(ag *agent.Agent, rc *config.ResolvedAgentConfig, cfg *config.Config) {
	warningWindow, err := time.ParseDuration(cfg.Logging.WarningWindowDuration)
	if err != nil {
		warningWindow = 5 * time.Minute
	}

	agentLevel := maxInjectionLevel(rc, cfg, func(n config.ResolvedNotify) config.InjectionLevel { return n.InjectAgentWarnings })
	if agentLevel.Enabled() {
		ag.WarningQueue = warnings.NewQueue(rc.Notify.WarningMaxPerWindow, warningWindow)
		if !agentLevel.IncludeWarnings() {
			ag.WarningQueue.SetErrorsOnly(true)
		}
	}

	chatLevel := maxInjectionLevel(rc, cfg, func(n config.ResolvedNotify) config.InjectionLevel { return n.InjectChatWarnings })
	if chatLevel.Enabled() {
		ag.ChatWarningQueue = warnings.NewQueue(rc.Notify.WarningMaxPerWindow, warningWindow)
		if !chatLevel.IncludeWarnings() {
			ag.ChatWarningQueue.SetErrorsOnly(true)
		}
	}
}

// setupManaWatcher configures mana threshold warnings on the agent.
func setupManaWatcher(ag *agent.Agent, p setupParams) {
	mc := p.resolved.Mana
	if len(mc.Thresholds) == 0 {
		return
	}

	ag.ManaWatcher = agent.NewManaWatcher(mc.Name, mc.Thresholds)
	ag.ManaWatcher.SetSessionIndex(p.sessionIndex, p.acfg.ID)
	ag.ManaWatcher.Restore()
	ag.ManaWatcher.SetRestoreThreshold(mc.RestoreThreshold)
}

// registerSpawnTool registers the spawn tool for forking sub-agents.
func registerSpawnTool(registry *tools.Registry, p setupParams, client provider.Client, bootstrap *workspace.Bootstrap, agLazy func() tools.SpawnAgent, notifier *tools.AsyncNotifier, promptSearchDirs []string, setNoCompact func(string, bool), groupResolver *config.GroupResolver, resolvedModel, defaultFormat string, fallbackFn provider.FallbackFunc) {
	acfg := p.acfg
	fileMode, _ := config.ParseFileMode(p.cfg.FileMode)

	spawnOrientPath := config.DerefStr(config.First(acfg.Sessions.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt))
	al := p.resolved.Loop
	tc := p.resolved.Tools
	spawnDeps := tools.SpawnDeps{
		Client:          client,
		ClientProvider:  p.clientProvider,
		Bootstrap:       bootstrap,
		Registry:        registry,
		Sessions:        &sessionBranchAdapter{store: p.sessions},
		AgentID:         acfg.ID,
		GroupResolver:   groupResolver,
		FallbackFunc:    fallbackFn,
		FallbackModel:   resolvedModel,
		FallbackFormat:  defaultFormat,
		MaxInherit:      tc.MaxConcurrentSpawns,
		MaxToolLoops:    al.MaxToolLoops,
		ExploreMaxDepth: tc.ExploreMaxDepth,
		Notifier:        notifier,
		OrientationTemplate: prompts.ResolveOrientationTemplate(spawnOrientPath, false, promptSearchDirs...),
		SetNoCompact: setNoCompact,
		FileMode:     fileMode,
	}
	registry.Register(tools.NewSpawnTool(spawnDeps, agLazy))
}

// platformConnectionResult holds the callbacks wired by platform connection setup.
type platformConnectionResult struct {
	configureFacetFn  func(platform.Connection)
	displayDefaultsFn func() platform.DisplaySettings
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
	reclaimOrientTemplate := prompts.ResolveOrientationTemplate(reclaimOrientPath, false, promptSearchDirs...)
	reclaimMfCfg := p.resolved.MemoryFormation
	reclaimSearchDirs := promptSearchDirs

	vc := p.resolved.Voice
	results := p.plat.SetupAgentConnection(platform.AgentConnectionParams{
		AgentID:        acfg.ID,
		Handler:        ag,
		Commands:       cmds,
		CommandContext: cc,
		LastMsgStore:   lastMsgStore,
		AgentConfig:    acfg,
		STT:            resolveSTT(p.sttMap, p.cfg.STT, vc.STT, voice.MergeReplacements(p.cfg.Voice.STTReplacements, acfg.Voice.STTReplacements)),
		TTS:            resolveTTS(p.ttsMap, p.cfg.TTS, vc.TTS, vc.TTSRate, ttsRepls),
		ReclaimHook: func(sessionKey string) {
			agent.FireSessionEndMemory(ag, p.sessions, sessionKey, reclaimMfCfg,
				reclaimOrientTemplate, reclaimSearchDirs, p.ctx, false)
		},
		DisplayOverrideFn: func(sessionKey string) platform.DisplaySettings {
			return platform.DisplaySettings{
				ShowToolCalls: ag.SessionShowToolCalls(sessionKey),
				ShowThinking:  ag.SessionDisplayShowThinking(sessionKey),
				StreamOutput:  ag.SessionStreamOutput(sessionKey),
				DisplayWidth:  ag.SessionDisplayWidth(sessionKey),
			}
		},
		Resolved: p.resolved,
	})
	for _, r := range results {
		if r.ConfigureFacetConn != nil {
			result.configureFacetFn = r.ConfigureFacetConn
		}
		if r.DisplayDefaultsFn != nil {
			result.displayDefaultsFn = r.DisplayDefaultsFn
		}
	}

	wireAgentPlatformCallbacks(ag, acfg, p.resolved, p.plat, p.connMgr, p.sessionIndex, tmuxMigrateKey)

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

