package main

import (
	"path/filepath"
	"sync"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/log"
	mcpkg "foci/internal/mcp"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/route"
	"foci/internal/skills"
	"foci/internal/tools"
	"foci/internal/workspace"
	"foci/shared/prompts"
)

// connResolver returns a platform.ConnResolver — a thunk that re-resolves the
// live connection for (sessionKey, agentID) every time it is called. Interactive
// prompts (ask, permission/AskUserQuestion) store this rather than a connection
// captured up front, so a callback registered now stays correct across the
// connection coming and going: a platform reconnect, or a restart where a
// persisted prompt is re-registered before the platform connection is back up.
// Resolving lazily at fire time is the same pattern the notify path already uses
// (see newSessionNotifyFn); this is the one canonical builder for it.
func connResolver(connMgr platform.ConnectionManager, sessionKey, agentID string) platform.ConnResolver {
	return func() platform.Connection {
		return connMgr.ForSessionOrPrimary(sessionKey, agentID)
	}
}

// sharedAgentSetup holds resolved config and helpers shared by both the
// traditional API agent path and the delegated transport path.
type sharedAgentSetup struct {
	p                setupParams
	promptSearchDirs []string
	groupResolver    *config.GroupResolver
	// wakeScheduleFn is the agent's scheduled-wake callback, built once
	// transport-independently in setupAgent. Nil when reminderStore is nil
	// (reminder support disabled). Used by both transports to register the
	// remind tool into their respective registries.
	wakeScheduleFn tools.ScheduleWakeFn
}

// resolveSharedSetup performs the common preamble for all agent types:
// config resolution, prompt search dirs, and group resolver creation.
func resolveSharedSetup(p setupParams) *sharedAgentSetup {
	p.resolved = config.Resolve(p.cfg, p.acfg)

	promptSearchDirs := []string{
		filepath.Join(p.acfg.Workspace, "prompts"),
		filepath.Join(filepath.Dir(p.acfg.Workspace), "shared", "prompts"),
	}

	// Delegated agents (claude-code, etc.) route all LLM work through the
	// backend and never resolve a model group — so they get no resolver at all.
	// A nil groupResolver cascades cleanly: configureAPI never runs for them,
	// command/tool deps see nil (their call sites are nil-guarded), and the
	// prompt-diff command falls back to a one-shot `claude --print`. Building one
	// here would also trip the resolver's no-API-agent guard on every use.
	var groupResolver *config.GroupResolver
	if !p.acfg.IsDelegated() {
		groupResolver = config.NewGroupResolver(p.resolved.Groups, p.cfg.Models, p.cfg.HasAPIAgent())
	}

	return &sharedAgentSetup{
		p:                p,
		promptSearchDirs: promptSearchDirs,
		groupResolver:    groupResolver,
	}
}

// newAgent creates an Agent struct with fields common to all agent types.
// Caller sets path-specific fields (Client, Tools, DelegatedManager, etc.) before
// calling finalize.
func (s *sharedAgentSetup) newAgent() *agent.Agent {
	acfg := s.p.acfg
	return &agent.Agent{
		Log:               log.NewComponentLogger("agent/" + acfg.ID),
		Sessions:          s.p.sessions,
		Reminders:         s.p.reminderStore,
		TaskListStore:     s.p.taskListStore,
		TodoStore:         s.p.todoStore,
		ScratchpadStore:   s.p.scratchpadStore,
		AgentID:           acfg.ID,
		SessionIndex:      s.p.sessionIndex,
		MessageTransforms: agent.CompileTransforms(resolveMessageTransforms(acfg, s.p.cfg)),
		PromptSearchDirs:  s.promptSearchDirs,
		Reflection:        s.p.resolved.Reflection,
		DefaultPlatform:   s.p.cfg.DefaultPlatformFor(s.p.acfg.ID),
		ShowToolCalls:     resolveShowToolCalls(s.p.resolved),
		Statusline:        s.p.resolved.Display.Statusline,
	}
}

// configureUniversal sets agent fields that apply to both API and delegated
// agents: compaction, warning queue, model defaults, and
// turn-lock warning threshold. Call this after newAgent() and before the
// transport-specific configuration (configureAPI / configureDelegated).
func configureUniversal(ag *agent.Agent, p setupParams, compactor *compaction.Compactor) {
	cpc := p.resolved.Compaction
	bc := p.resolved.Behavior

	ag.Compactor = compactor
	ag.CompactionSummaryPromptPath = cpc.CompactionSummaryPrompt
	ag.CompactionHandoffMsg = cpc.CompactionHandoffMsg
	ag.ReloadOnCompact = cpc.ReloadOnCompact
	ag.TurnLockWarnThreshold = parseDurationDefault(bc.TurnLockWarnThreshold, 0)
	ag.ModelDefaultsFn = modelDefaultsFn(p.cfg.Models)

	// Reset lifecycle: orientation template resolver (closed over config for lazy resolution).
	resetOrientPath := config.DerefStr(config.First(p.acfg.Sessions.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt))
	resetSearchDirs := ag.PromptSearchDirs
	ag.ResetOrientTemplateFn = func() string {
		return prompts.ResolveOrientationTemplate(resetOrientPath, false, resetSearchDirs...)
	}
	ag.CanRunBackground = p.resolved.Background.CanRunBackground

	setupWarningQueue(ag, p.resolved, p.cfg)
}

// finalizeParams holds optional values for finalize. Nil/zero fields are
// safe — finalize skips the corresponding setup. API agents populate all
// fields; delegated agents populate bootstrap + skillRegistry + skillsDirs
// and leave the API-only tool/client fields nil.
type finalizeParams struct {
	bootstrap           *workspace.Bootstrap
	registry            *tools.Registry // nil for delegated agents
	skillRegistry       *skills.Registry
	serverTools         []provider.ToolDef       // nil for delegated agents
	client              provider.Client          // nil for delegated agents
	clientProvider      provider.ClientProvider  // nil for delegated agents
	fallbackFn          provider.FallbackFunc    // nil for delegated agents
	compactionThreshold float64                  // 0 for delegated agents
	tmuxTool            *tools.Tool              // nil for delegated agents
	tmuxClearAll        func()                   // nil for delegated agents
	tmuxWatchCount      func() int               // nil for delegated agents
	ttsRepls            map[string]string        // nil for delegated agents
	mcpManager          *mcpkg.Manager           // nil for delegated agents
	skillsDirs          []string
}

// finalize performs the common postamble: nudge system, slash commands,
// platform connections, nudge init, and instance assembly.
func (s *sharedAgentSetup) finalize(ag *agent.Agent, fp finalizeParams) *agentInstance {
	acfg := s.p.acfg
	p := s.p

	// Bootstrap — needed by both API (system prompt) and delegated (slash commands, /reset).
	ag.Bootstrap = fp.bootstrap

	// Reload lifecycle: skills/extra-blocks reload callback.
	if fp.skillsDirs != nil {
		reloadSkillsDirs := fp.skillsDirs
		reloadWorkspace := acfg.Workspace
		ag.ReloadSystemFn = func() ([]provider.SystemBlock, int) {
			reg := skills.Load(reloadSkillsDirs)
			if reg.Len() == 0 {
				return nil, 0
			}
			return []provider.SystemBlock{{Type: "text", Text: reg.SystemBlock(reloadWorkspace)}}, reg.Len()
		}
		ag.SkillDirs = reloadSkillsDirs
		notifyAgentID := acfg.ID
		ag.SkillChangeNotify = func(sessionKey, text string) {
			route.NotifySessionChat(p.connMgr, notifyAgentID, sessionKey, text)
		}
	}

	// Nudge system.
	wsFileMode, _ := config.ParseFileMode(p.cfg.FileMode)
	setupNudgeSystem(ag, acfg, p.resolved.Nudge, p.sessions, fp.registry, fp.skillRegistry, wsFileMode)

	// Slash commands.
	var configureFacet func(platform.Connection)
	var displayDefaultsFn func() platform.DisplaySettings

	lastMsgStore := command.NewLastMessageStore()
	cmds, cc := registerAgentCommands(cmdRegParams{
		ag:                  ag,
		acfg:                acfg,
		bootstrap:           fp.bootstrap,
		promptSearchDirs:    s.promptSearchDirs,
		compactionThreshold: fp.compactionThreshold,
		cfg:                 p.cfg,
		configPath:          p.configPath,
		sessions:            p.sessions,
		sessionIndex:        p.sessionIndex,
		client:              fp.client,
		clientProvider:      fp.clientProvider,
		store:               p.store,
		bwStore:             p.bwStore,
		startTime:           p.startTime,
		todoStore:           p.todoStore,
		registry:            fp.registry,
		tmuxTool:            fp.tmuxTool,
		skillsDirs:          fp.skillsDirs,
		skillRegistry:       fp.skillRegistry,
		agentListFn:         p.agentListFn,
		plat:                p.plat,
		connMgr:             p.connMgr,
		groupResolver:       s.groupResolver,
		fallbackFn:          fp.fallbackFn,
		resolved:            p.resolved,
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

	// Finalize tools (API agents only).
	if fp.registry != nil {
		fp.registry.FinalizeShellDescription()
		logRegisteredTools(fp.registry, fp.serverTools, acfg.ID)
	}

	// Platform connections.
	if p.plat != nil {
		platResult := setupPlatformConnections(ag, p, cmds, cc, lastMsgStore, fp.ttsRepls, s.promptSearchDirs)
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

	inst := &agentInstance{
		id:               acfg.ID,
		ag:               ag,
		cmds:             cmds,
		cc:               cc,
		registry:         fp.registry,
		bootstrap:        fp.bootstrap,
		agentCfg:         acfg,
		resolved:         p.resolved,
		promptSearchDirs: s.promptSearchDirs,
		skillsDirs:       fp.skillsDirs,
		webhooks:         p.resolved.Webhooks,
		tmuxClearAll:     fp.tmuxClearAll,
		tmuxWatchCount:   fp.tmuxWatchCount,
		mcpManager:       fp.mcpManager,
	}
	// testActiveWorkOverride uses -1 as the "unset" sentinel so the
	// periodic HasActiveWorkFn closure can distinguish "test asked for
	// zero" from "test set no override". Production code never reads or
	// writes the override; tests drive it via the control socket.
	inst.testActiveWorkOverride.Store(-1)
	return inst
}
