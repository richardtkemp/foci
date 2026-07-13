package main

// commands.go — slash command registration, extracted from setupAgent().
//
// registerAgentCommands creates the command registry for a single agent
// and builds the CommandContext that all commands share.

import (
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"foci/internal/agent"
	"foci/internal/app"
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/memory"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/secrets"
	"foci/internal/secrets/bitwarden"
	"foci/internal/session"
	"foci/internal/skills"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// cmdRegParams holds all dependencies needed for slash command registration.
type cmdRegParams struct {
	// Per-agent state
	ag                  *agent.Agent
	acfg                config.AgentConfig
	bootstrap           *workspace.Bootstrap
	promptSearchDirs    []string
	compactionThreshold float64

	// Shared infrastructure
	cfg            *config.Config
	configPath     string
	sessions       *session.Store
	sessionIndex   *session.SessionIndex
	client         provider.Client
	clientProvider provider.ClientProvider
	store          *secrets.Store
	bwStore        *bitwarden.Store
	startTime      time.Time

	// Stores
	todoStore *memory.TodoStore

	// Tools & skills
	registry      *tools.Registry
	tmuxTool      *tools.Tool
	skillsDirs    []string
	skillRegistry *skills.Registry

	// Cross-agent
	agentListFn func() []command.AgentInfo

	// Platform
	plat              *platform.Messaging
	connMgr           platform.ConnectionManager
	configureFacet    func(platform.Connection)
	displayDefaultsFn func() platform.DisplaySettings

	// Model groups
	groupResolver *config.GroupResolver
	fallbackFn    provider.FallbackFunc

	// Pre-resolved agent+global config
	resolved *config.ResolvedAgentConfig
}

// pprofGate is the process-global live-toggle gate for /debug/pprof/*.
// Seeded from [debug] enable_pprof at startup; toggled at runtime via
// /misc pprof or the /-/pprof admin endpoint.
var pprofGate atomic.Bool

// registerAgentCommands creates and populates the command registry for an agent.
func registerAgentCommands(p cmdRegParams, lastMsgStore *command.LastMessageStore) (*command.Registry, command.CommandContext) {
	cmds := command.NewRegistry()

	// Build ConfigSetDeps (needs registry reference for wizard activation)
	configSetDeps := &command.ConfigSetDeps{
		Registry:        cmds,
		ConfigPath:      p.configPath,
		AgentID:         p.acfg.ID,
		SectionsFn:      config.FieldSections,
		FieldsInSection: config.FieldsInSection,
		LookupFn:        config.LookupField,
		SetInFileFn: func(path string, target config.SetTarget, value string) (string, error) {
			fm, _ := config.ParseFileMode(p.cfg.FileMode)
			old, err := config.SetInFile(path, target, value, fm)
			if err == nil && gwLiveApply != nil {
				section := target.Section
				if section == "agents" { // file target → registry section
					section = "agent"
				}
				if _, applyErr := gwLiveApply.Apply(section, target.Key); applyErr != nil {
					log.Warnf("config", "%s.%s written but live apply failed (takes effect on restart): %v", section, target.Key, applyErr)
				}
			}
			return old, err
		},
		EffectiveValueFn: func(section, key string) string {
			return config.LookupValue(p.cfg, p.acfg, section, key)
		},
	}

	// Build SecretsDeps (needs registry reference for wizard activation)
	secretsDeps := &command.SecretsDeps{
		Registry: cmds,
		Store:    p.store,
	}

	// Build AndroidDeps (registry reference for the /android onboarding wizard).
	// MintPairKey reaches the live app hub at call time (#862): a single-use,
	// in-memory pairing key replaces the old persisted master key.
	androidDeps := &command.AndroidDeps{Registry: cmds, MintPairKey: app.MintActivePairKey}

	// Build AgentNewDeps
	cmdFileMode, _ := config.ParseFileMode(p.cfg.FileMode)
	agentNewDeps := &command.AgentNewDeps{
		Registry:    cmds,
		ConfigPath:  p.configPath,
		DefaultsDir: filepath.Join(filepath.Dir(p.acfg.Workspace), "shared"),
		HomeDir:     filepath.Dir(p.acfg.Workspace),
		FileMode:    cmdFileMode,
		ListFn:      p.agentListFn,
		PreFlightFn: func(agentID string) []string {
			if p.plat == nil {
				return nil
			}
			return p.plat.AgentPreFlight(agentID)
		},
		ResolveModel: func(input string) string {
			resolved, err := config.ResolveModel(input, "", p.cfg.Models)
			if err != nil {
				return input
			}
			return resolved.Developer + "/" + resolved.ModelID
		},
		AvailableBackends: delegator.SupportedNames(),
	}

	// Construct the CommandContext
	cc := command.CommandContext{
		Agent:            p.ag,
		Sessions:         p.sessions,
		Bootstrap:        p.bootstrap,
		SessionIndex:     p.sessionIndex,
		Config:           p.cfg,
		AgentConfig:      p.acfg,
		Client:           p.client,
		ClientProvider:   p.clientProvider,
		ConnMgr:          p.connMgr,
		PromptSearchDirs: p.promptSearchDirs,
		APILogPath:       p.cfg.Logging.APIFile,
		EventLogPath:     p.cfg.Logging.EventFile,
		ConfigPath:       p.configPath,
		GroupResolver:    p.groupResolver,
		FallbackFunc:     p.fallbackFn,
		ToolsRegistry:    p.registry,
		TmuxTool:         p.tmuxTool,
		BuildInfo: command.BuildInfo{
			Version:   version,
			GoVersion: goVersion,
			GitCommit: gitCommit,
			BuildTime: buildTime,
		},
		StartTime:           p.startTime,
		CompactionThreshold: p.compactionThreshold,
		ActivityFunc:        app.ResolvedActivity,
		ModelMetaFn:         modelMetaFn(p.cfg.Models),
		SecretsStore:        p.store,
		BitwardenStore:      p.bwStore,
		BitwardenEnabled:    p.cfg.Bitwarden.Enabled,
		AgentListFn:         p.agentListFn,
		LastMessageStore:    lastMsgStore,
		TodoStore:           p.todoStore,
		ConfigSetDeps:       configSetDeps,
		AgentNewDeps:        agentNewDeps,
		SecretsDeps:         secretsDeps,
		AndroidDeps:         androidDeps,
		TokenCountCache:     command.NewTokenCountCache(),
		ConfigureFacet:      p.configureFacet,
		Resolved:            p.resolved,
		PprofControl:        pprofControl,
	}

	// Register all commands
	cmds.Register(command.PingCommand())
	cmds.Register(command.StatusCommand())
	cmds.Register(command.CacheCommand())
	cmds.Register(command.LastCommand())
	cmds.Register(command.CostCommand())
	cmds.Register(command.ContextCommand())
	cmds.Register(command.ResetCommand())
	cmds.Register(command.ModelCommand())
	cmds.Register(command.EffortCommand())
	cmds.Register(command.ThinkingCommand())
	cmds.Register(command.SpeedCommand())
	cmds.Register(command.ModeCommand())
	cmds.Register(command.DisplayCommand())
	cmds.Register(command.OverridesCommand())
	cmds.Register(command.ToolsCommand())
	cmds.Register(command.ConfigCommand())
	cmds.Register(command.PromptsCommand())
	cmds.Register(command.LogCommand())
	cmds.Register(command.ErrorsCommand())
	cmds.Register(command.VersionCommand())
	cmds.Register(command.HelpCommand(cmds))
	cmds.Register(command.CompactCommand())
	cmds.Register(command.RestartCommand())
	cmds.Register(command.SecretsCommand())
	cmds.Register(command.BitwardenCommand())
	cmds.Register(command.SessionsCommand())
	cmds.Register(command.AgentsCommand())
	cmds.Register(command.AndroidCommand())
	cmds.Register(command.PairKeyCommand())
	cmds.Register(command.RepeatCommand())
	cmds.Register(command.PassCommand())
	cmds.Register(command.TodoCommand())

	// /plan — put the coding-agent backend into plan mode. Registered iff the
	// configured backend contributed a plan delivery via delegator.RegisterPlan
	// (only delegated CC backends do). The delivery mechanism — verbatim slash
	// command (cctmux) vs EnterPlanMode turn (ccstream) — lives with each
	// backend, not as a string switch here (#857).
	if delivery, ok := delegator.PlanDeliveryFor(p.acfg.Backend); ok {
		cmds.Register(command.PlanCommand(delivery))
	}

	// Tmux command (only if tool is available)
	if p.tmuxTool != nil {
		cmds.Register(command.TmuxCommand())
	}

	// /pause and /resume toggle answer-capture for a pending foci_ask; /complete
	// ends it early with the answers given so far. Only meaningful when the ask
	// tool is wired (AskRouter set), which covers both API and delegated/CC agents.
	if p.ag.AskRouter != nil {
		cmds.Register(command.PauseCommand())
		cmds.Register(command.ResumeCommand())
		cmds.Register(command.CompleteCommand())
	}

	cmds.Register(command.MiscCommand())

	// Stop / done
	cmds.Register(command.StopCommand())
	cmds.Register(command.LoginCommand())
	cmds.Register(command.DoneCommand())

	// Stop aliases (e.g. "wait" → same as "stop")
	bc := p.resolved.Behavior
	if bc.EnableStopAliases {
		stopCmd := cmds.Get("stop")
		for _, alias := range bc.StopAliases {
			if alias != "stop" {
				cmds.Register(&command.Command{Name: alias, Hidden: true, Execute: stopCmd.Execute})
			}
		}
	}

	// Facet
	cmds.Register(command.FacetCommand())

	// Custom script commands from config
	for _, cc := range p.cfg.Commands {
		cmds.Register(command.ScriptCommand(cc.Name, cc.Description, cc.Script, cc.Timeout))
	}

	// Skill slash commands (command + script in frontmatter)
	if p.skillRegistry != nil {
		for _, s := range p.skillRegistry.All() {
			if s.Command != "" && s.Script != "" {
				name := strings.TrimPrefix(s.Command, "/")
				cmds.Register(command.ScriptCommand(name, s.Description, s.Script, 30))
			}
		}
	}

	// In-flight wizards survive restarts (mirrors the ask tool's ask_pending):
	// checkpoint to the session index on every mutation, and restore whatever a
	// previous process left mid-flow now that the full CommandContext exists to
	// re-inject each wizard's deps.
	cmds.EnableWizardPersistence(p.sessionIndex, p.acfg.ID)
	cmds.RestoreWizards(cc)

	return cmds, cc
}

// pprofControl implements command.CommandContext.PprofControl against the
// process-global pprofGate. action is "on"/"off"/"toggle"/"status";
// returns the resulting enabled state.
func pprofControl(action string) bool {
	switch action {
	case "on":
		pprofGate.Store(true)
	case "off":
		pprofGate.Store(false)
	case "toggle":
		pprofGate.Store(!pprofGate.Load())
	}
	return pprofGate.Load()
}
