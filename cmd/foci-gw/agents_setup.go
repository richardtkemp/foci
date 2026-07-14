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
	"foci/internal/modelcaps"
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
	maxFileChars := acfg.Sessions.EffectiveMaxSystemPromptFile(p.cfg.Sessions.MaxSystemPromptFile)
	maxTotalChars := acfg.Sessions.EffectiveMaxSystemPromptTotal(p.cfg.Sessions.MaxSystemPromptTotal)
	checkSystemPromptSizes(bootstrap, maxFileChars, maxTotalChars, acfg.ID)

	home := filepath.Dir(acfg.Workspace)
	skillsDirs := skills.ResolveDirs(home, acfg.Workspace, p.cfg.Skills.Dir, acfg.SkillsDir)
	// Use the process-shared loader so the shared skills dir (skillsDirs[0]) is
	// scanned and warned about once, not once per agent. ResolveDirs always
	// returns [sharedDir, agentDir]. Fall back to a plain Load if no loader was
	// injected (e.g. a direct test caller).
	var skillRegistry *skills.Registry
	if p.skillLoader != nil && len(skillsDirs) == 2 {
		skillRegistry = p.skillLoader.LoadForAgent(skillsDirs[0], skillsDirs[1])
	} else {
		skillRegistry = skills.Load(skillsDirs)
	}
	var extraSystemBlocks []provider.SystemBlock
	if skillRegistry.Len() > 0 {
		extraSystemBlocks = []provider.SystemBlock{
			{Type: "text", Text: skillRegistry.SystemBlock(acfg.Workspace)},
		}
		mainLog.Infof("agent %q: loaded %d skills", acfg.ID, skillRegistry.Len())
	}
	maxRC := p.resolved.Summary.MaxResultChars // static-cfg:ignore: one-time startup diagnostic (warns if a skill file is oversized), not a hot path
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
	cc := p.resolved.Compaction // static-cfg:ignore: initial construction value; live edits flow through the OnChange registered below (bucket D, a30414b8)
	compactionThreshold := cc.CompactionThreshold
	preserveMessages := cc.CompactionPreserveMessages
	compactor := compaction.NewCompactor(p.sessions, compactionThreshold)
	compactor.SetNonlinear(!cc.CompactionThresholdSet)
	compactor.WithConfig(
		p.cfg.Sessions.CompactionMaxTokens,
		p.cfg.Sessions.CompactionMinMessages,
		preserveMessages,
	)
	compactor.ModelDefaultsFn = modelDefaultsFn(p.cfg.Models)
	compactor.ModelMetaFn = modelMetaFn(p.cfg.Models)
	compactorBackend := modelcaps.BackendKey(p.acfg.Backend)
	compactor.ModelCapsFn = func(model string) (modelcaps.Caps, bool) {
		return modelcaps.LookupFor(compactorBackend, model)
	}
	compactor.Scratchpad = p.scratchpadStore
	compactor.TaskListStore = p.taskListStore
	compactor.AgentID = p.acfg.ID
	compactor.FallbackFunc = fallbackFn
	compactor.ClientProvider = p.clientProvider

	p.resolvedLive.OnChange(func(old, fresh *config.ResolvedAgentConfig) {
		if fresh.Compaction.CompactionThreshold != old.Compaction.CompactionThreshold {
			compactor.SetThreshold(fresh.Compaction.CompactionThreshold)
		}
		if fresh.Compaction.CompactionThresholdSet != old.Compaction.CompactionThresholdSet {
			compactor.SetNonlinear(!fresh.Compaction.CompactionThresholdSet)
		}
		if fresh.Compaction.CompactionPreserveMessages != old.Compaction.CompactionPreserveMessages {
			compactor.SetPreserveMessages(fresh.Compaction.CompactionPreserveMessages)
		}
	})

	return compactor, compactionThreshold
}

// nudgeExtractMaxAttempts bounds how many times nudge rule extraction is re-run
// when it fails (e.g. the model returns malformed JSON). See #830.
const nudgeExtractMaxAttempts = 3

// nudgeSettings maps the resolved nudge config to the Scheduler's config-free
// live-tunable settings.
func nudgeSettings(nc config.ResolvedNudge) nudge.Settings {
	return nudge.Settings{
		Cooldown:           nc.NudgeCooldown,
		MaxPerBatch:        nc.NudgeMaxPerBatch,
		Enable:             nc.NudgeEnable,
		DefaultEnable:      nc.NudgeDefaultEnable,
		DefaultFreq:        nc.NudgeDefaultFrequency,
		ScratchpadFreq:     nc.NudgeDefaultScratchpadFrequency,
		BraindeadThreshold: nc.NudgeDefaultBraindeadThreshold,
		BraindeadPrompt:    nc.NudgeDefaultBraindeadPrompt,
		PreAnswerGate:      nc.NudgePreAnswerGate,
		PreAnswerMinTools:  nc.NudgePreAnswerMinTools,
	}
}

// setupNudgeSystem configures the nudge scheduler and reload logic on the agent.
func setupNudgeSystem(ag *agent.Agent, acfg config.AgentConfig, nc config.ResolvedNudge, sessions *session.Store, toolRegistry *tools.Registry, skillRegistry *skills.Registry, fileMode os.FileMode) {
	// Backend capabilities for rule filtering. Turn-start triggers (every_n_turns,
	// regex) work on all backends; mid-turn triggers (every_n_tools, after_error,
	// tool_pattern, pre_answer) need stdin-pipe injection (ccstream), which the
	// HTTP-based opencode backend lacks.
	isOpencode := acfg.Backend == "opencode"
	schedOpts := nudge.SchedulerOpts{
		Cooldown:     nc.NudgeCooldown,
		MaxPerBatch:  nc.NudgeMaxPerBatch,
		CanPostTool:  !isOpencode,
		CanPreAnswer: !isOpencode,
		AgentID:      acfg.ID,
	}
	rulesPath := nudge.RulesPath(acfg.Workspace)

	// Built-in rules are built unconditionally; the Scheduler gates each on live
	// [defaults.nudge] settings (build-all + live-gate, so an enable/interval edit
	// applies with no rebuild). Braindead first — highest effective priority.
	braindeadRules := nudge.BraindeadRule()

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
	defaultRules := nudge.DefaultRules(toolNames, skillSummaries)

	var scratchpadRules []nudge.Rule
	if ag.ScratchpadStore != nil {
		agentID := ag.AgentID
		store := ag.ScratchpadStore
		scratchpadRules = nudge.ScratchpadRule(func() bool {
			entries, err := store.List(agentID)
			return err == nil && len(entries) > 0
		})
	}

	// liveNudge reads the current resolved nudge config, falling back to the
	// startup snapshot in agents without LiveConfigFn (tests).
	liveNudge := func() config.ResolvedNudge {
		if ag.LiveConfigFn != nil {
			return ag.LiveConfigFn().Nudge
		}
		return nc
	}

	// build assembles the latest character rules from disk with the built-ins into
	// a fresh Scheduler and applies the latest live settings. Called at setup and
	// on every NudgeReloadFunc (session reset / compaction) — the sanctioned
	// rebuild point where losing per-session counters is acceptable.
	build := func() {
		var charRules []nudge.Rule
		if rs, err := nudge.LoadRules(rulesPath); err != nil {
			nudgeLog.Warnf("agent %s: load nudge rules: %v", acfg.ID, err)
		} else if rs != nil {
			charRules = rs.Rules
		}
		for i := range charRules {
			charRules[i].Category = nudge.CategoryChar
		}
		merged := append(braindeadRules, append(charRules, append(defaultRules, scratchpadRules...)...)...)
		var sched *nudge.Scheduler
		if isOpencode {
			sched = nudge.NewSchedulerOpts(&nudge.RuleSet{Rules: merged}, schedOpts)
		} else {
			sched = nudge.NewScheduler(&nudge.RuleSet{Rules: merged}, schedOpts.Cooldown, schedOpts.MaxPerBatch)
		}
		sched.Configure(nudgeSettings(liveNudge()))
		ag.Nudger = sched
	}
	build()

	fileOrder := acfg.System.SystemFiles
	if len(fileOrder) == 0 {
		fileOrder = workspace.DefaultFileOrder
	}

	ag.NudgeReloadFunc = func() {
		if !liveNudge().NudgeAutoExtract {
			build()
			return
		}
		extractor := nudge.NewExtractor(acfg.ID, acfg.Workspace, fileOrder, fileMode, schedOpts.CanPostTool, schedOpts.CanPreAnswer)
		_, needed := extractor.NeedsExtraction()
		if needed {
			go func() {
				ctx := context.Background()

				// Select the extraction attempt; branch setup (API path) happens
				// once, outside the retry loop, so only the extraction itself is
				// re-run.
				var attempt func() error
				if ag.DelegatedManager != nil {
					// Delegated: use claude --print for one-shot extraction.
					// No interactive session, no platform delivery, no session index.
					nudgeLog.Infof("agent %s: delegated extraction via RunOnce", acfg.ID)
					attempt = func() error { return extractor.ExtractViaRunOnce(ctx, ag.DelegatedManager) }
				} else {
					// API: branch from the most recent session.
					parentKey := defaultSessionKeyFor(ag, acfg.ID)
					if parentKey == "" {
						nudgeLog.Warnf("agent %s: no default session for extraction branch", acfg.ID)
						return
					}
					branchKey, err := sessions.CreateBranchWithOptions(parentKey, session.BranchOptions{
						NoResetHook: true,
						BranchType:  "nudge-extraction",
					})
					if err != nil {
						nudgeLog.Warnf("agent %s: create branch: %v", acfg.ID, err)
						return
					}
					ag.SetSessionNoCompact(branchKey, true)
					attempt = func() error { return extractor.Extract(ctx, ag, branchKey) }
				}

				// Re-run the extractor up to nudgeExtractMaxAttempts times: the
				// model occasionally returns malformed JSON, and a fresh sample
				// usually parses. Early failures log at info; the final one at
				// error (#830).
				var err error
				for i := 1; i <= nudgeExtractMaxAttempts; i++ {
					if err = attempt(); err == nil {
						break
					}
					if i < nudgeExtractMaxAttempts {
						nudgeLog.Infof("agent %s: extraction attempt %d/%d failed: %v; retrying", acfg.ID, i, nudgeExtractMaxAttempts, err)
					} else {
						nudgeLog.Errorf("agent %s: extraction failed after %d attempts: %v", acfg.ID, nudgeExtractMaxAttempts, err)
					}
				}
				if err != nil {
					return
				}
				build()
				nudgeLog.Infof("agent %s: refreshed rules after extraction", acfg.ID)
			}()
		} else {
			build()
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
// The queues and their dispatchers are always constructed (a disabled queue
// drops pushes and its dispatcher no-ops), so an off→on injection-level change
// can be applied live via applyWarningQueueLevels without spinning up new
// goroutines (#1225).
func setupWarningQueue(ag *agent.Agent, rc *config.ResolvedAgentConfig, cfg *config.Config) {
	ag.WarningQueue = warnings.NewQueue(rc.Notify.WarningMaxPerWindow, 0)
	ag.ChatWarningQueue = warnings.NewQueue(rc.Notify.WarningMaxPerWindow, 0)
	applyWarningQueueLevels(ag, rc, cfg)
}

// applyWarningQueueLevels (re)applies the injection levels and rate-limit window
// to the agent's warning queues from resolved config. Called at setup and from
// the live-config applier.
func applyWarningQueueLevels(ag *agent.Agent, rc *config.ResolvedAgentConfig, cfg *config.Config) {
	warningWindow, err := time.ParseDuration(cfg.Logging.WarningWindowDuration)
	if err != nil {
		warningWindow = 5 * time.Minute
	}

	agentLevel := maxInjectionLevel(rc, cfg, func(n config.ResolvedNotify) config.InjectionLevel { return n.InjectAgentWarnings })
	ag.WarningQueue.Configure(agentLevel.Enabled(), !agentLevel.IncludeWarnings(), rc.Notify.WarningMaxPerWindow, warningWindow)

	chatLevel := maxInjectionLevel(rc, cfg, func(n config.ResolvedNotify) config.InjectionLevel { return n.InjectChatWarnings })
	ag.ChatWarningQueue.Configure(chatLevel.Enabled(), !chatLevel.IncludeWarnings(), rc.Notify.WarningMaxPerWindow, warningWindow)
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
) platformConnectionResult {
	acfg := p.acfg
	var result platformConnectionResult

	reclaimOrientPath := config.DerefStr(config.First(acfg.Sessions.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt))
	reclaimOrientTemplate := prompts.ResolveOrientationTemplate(reclaimOrientPath, false, promptSearchDirs...)

	// STT/TTS re-resolve from p.resolvedLive on every call (lazySTT/lazyTTS),
	// so voice.stt/voice.tts/voice.tts_rate changes reach this bot-connection
	// path live instead of being baked in once here (#1224). Replacements maps
	// (tts_replacements/stt_replacements) stay static — map-field addressability
	// for them is a separate follow-up.
	//
	// sttMap/ttsMap are built once at startup from [[stt]]/[[tts]] entries
	// (provider definitions, not hot-reloadable) — only wrap with the lazy
	// adapter when at least one provider actually exists. A voice.STT/voice.TTS
	// interface holding a non-nil *lazySTT/*lazyTTS is itself non-nil even
	// when resolve() would return nil, which would break every `transcriber
	// == nil` / `tts == nil` "no provider configured" check downstream.
	var sttParam voice.STT
	if len(p.sttMap) > 0 {
		sttParam = &lazySTT{resolve: func() voice.STT {
			vc := p.resolvedLive.Load().Voice
			return resolveSTT(p.sttMap, p.cfg.STT, vc.STT, voice.MergeReplacements(p.cfg.Voice.STTReplacements, acfg.Voice.STTReplacements))
		}}
	}
	var ttsParam voice.TTS
	if len(p.ttsMap) > 0 {
		ttsParam = &lazyTTS{resolve: func() voice.TTS {
			vc := p.resolvedLive.Load().Voice
			return resolveTTS(p.ttsMap, p.cfg.TTS, vc.TTS, vc.TTSRate, ttsRepls)
		}}
	}
	results := p.plat.SetupAgentConnection(platform.AgentConnectionParams{
		AgentID:        acfg.ID,
		Handler:        ag,
		Commands:       cmds,
		CommandContext: cc,
		LastMsgStore:   lastMsgStore,
		AgentConfig:    acfg,
		STT:            sttParam,
		TTS:            ttsParam,
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
		Resolved:     p.resolved, // static-cfg:ignore: plumbing — still consumed by the deliberately-baked display/voice setup above, see ResolvedLive for the live counterpart
		ResolvedLive: p.resolvedLive,
	})
	for _, r := range results {
		if r.ConfigureFacetConn != nil {
			result.configureFacetFn = r.ConfigureFacetConn
		}
		if r.DisplayDefaultsFn != nil {
			result.displayDefaultsFn = r.DisplayDefaultsFn
		}
	}

	wireAgentPlatformCallbacks(ag, acfg, p.resolvedLive, p.connMgr, p.sessionIndex)

	return result
}

// logRegisteredTools logs the names of all registered client and server tools.
func logRegisteredTools(registry *tools.Registry, serverTools []provider.ToolDef, agentID string) {
	allTools := registry.All()
	toolNames := make([]string, len(allTools))
	for i, t := range allTools {
		toolNames[i] = t.Name
	}
	mainLog.Infof("agent %q: registered %d tools: [%s]", agentID, len(toolNames), strings.Join(toolNames, ", "))
	if len(serverTools) > 0 {
		stNames := make([]string, len(serverTools))
		for i, st := range serverTools {
			stNames[i] = st.Name()
		}
		mainLog.Infof("agent %q: server tools: [%s]", agentID, strings.Join(stNames, ", "))
	}
}
