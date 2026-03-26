package main

import (
	"path/filepath"
	"sync"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/log"
	mcpkg "foci/internal/mcp"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/skills"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// sharedAgentSetup holds resolved config and helpers shared by both the
// traditional API agent path and the backend agent path.
type sharedAgentSetup struct {
	p                setupParams
	promptSearchDirs []string
	groupResolver    *config.GroupResolver
}

// resolveSharedSetup performs the common preamble for all agent types:
// config resolution, prompt search dirs, and group resolver creation.
func resolveSharedSetup(p setupParams) *sharedAgentSetup {
	p.resolved = config.Resolve(p.cfg, p.acfg)

	promptSearchDirs := []string{
		filepath.Join(p.acfg.Workspace, "prompts"),
		filepath.Join(filepath.Dir(p.acfg.Workspace), "shared", "prompts"),
	}

	groupResolver := config.NewGroupResolver(p.resolved.Groups, p.cfg.Models)

	return &sharedAgentSetup{
		p:                p,
		promptSearchDirs: promptSearchDirs,
		groupResolver:    groupResolver,
	}
}

// newAgent creates an Agent struct with fields common to all agent types.
// Caller sets path-specific fields (Client, Tools, Backend, etc.) before
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
		ShowToolCalls:     resolveShowToolCalls(s.p.resolved),
	}
}

// finalizeParams holds optional values for finalize. Nil/zero fields are
// safe — finalize skips the corresponding setup. API agents populate all
// fields; backend agents leave most nil.
type finalizeParams struct {
	bootstrap     *workspace.Bootstrap
	registry      *tools.Registry        // nil for backend agents
	skillRegistry *skills.Registry       // nil for backend agents
	serverTools   []provider.ToolDef     // nil for backend agents
	client        provider.Client        // nil for backend agents
	clientProvider provider.ClientProvider // nil for backend agents
	usageClientProvider provider.UsageClientProvider // nil for backend agents
	fallbackFn    provider.FallbackFunc  // nil for backend agents
	compactionThreshold float64          // 0 for backend agents
	tmuxTool      *tools.Tool            // nil for backend agents
	tmuxClearAll  func()                 // nil for backend agents
	tmuxWatchCount func() int            // nil for backend agents
	tmuxMigrateKey func(string, string)  // nil for backend agents
	ttsRepls      map[string]string      // nil for backend agents
	mcpManager    *mcpkg.Manager         // nil for backend agents
	skillsDirs    []string               // nil for backend agents
}

// finalize performs the common postamble: nudge system, slash commands,
// platform connections, nudge init, and instance assembly.
func (s *sharedAgentSetup) finalize(ag *agent.Agent, fp finalizeParams) *agentInstance {
	acfg := s.p.acfg
	p := s.p

	// Nudge system.
	wsFileMode, _ := config.ParseFileMode(p.cfg.FileMode)
	setupNudgeSystem(ag, acfg, p.resolved.Nudge, p.connMgr, p.sessions, fp.registry, fp.skillRegistry, wsFileMode)

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
		usageClientProvider: fp.usageClientProvider,
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
		platResult := setupPlatformConnections(ag, p, cmds, cc, lastMsgStore, fp.ttsRepls, s.promptSearchDirs, fp.tmuxMigrateKey)
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

	return &agentInstance{
		id:               acfg.ID,
		ag:               ag,
		cmds:             cmds,
		cc:               cc,
		registry:         fp.registry,
		bootstrap:        fp.bootstrap,
		agentCfg:         acfg,
		resolved:         p.resolved,
		promptSearchDirs: s.promptSearchDirs,
		webhooks:         p.resolved.Webhooks,
		tmuxClearAll:     fp.tmuxClearAll,
		tmuxWatchCount:   fp.tmuxWatchCount,
		tmuxMigrateKey:   fp.tmuxMigrateKey,
		mcpManager:       fp.mcpManager,
	}
}
