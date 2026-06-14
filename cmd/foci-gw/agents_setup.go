package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/log"
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
		// braindeadRules first, mirroring the initial-construction order — the
		// safety "braindead" rule must survive reloads, not just the first build.
		merged := append(braindeadRules, append(reloaded, append(defaultRules, scratchpadRules...)...)...)
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
				if ag.DelegatedManager != nil {
					// Delegated: use claude --print for one-shot extraction.
					// No interactive session, no platform delivery, no session index.
					log.Infof("nudge", "agent %s: delegated extraction via RunOnce", acfg.ID)
					if err := extractor.ExtractViaRunOnce(ctx, ag.DelegatedManager); err != nil {
						log.Warnf("nudge", "agent %s: extraction failed: %v", acfg.ID, err)
						return
					}
				} else {
					// API: branch from the most recent session.
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
			ag.FireSessionEndMemory(p.ctx, sessionKey, reclaimOrientTemplate, false)
			if ag.DelegatedManager != nil {
				ag.DelegatedManager.ResetSession(sessionKey)
			}
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
