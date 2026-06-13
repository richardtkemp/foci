package main

// commands.go — slash command registration, extracted from setupAgent().
//
// registerAgentCommands creates the command registry for a single agent
// and builds the CommandContext that all commands share.

import (
	"path/filepath"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/mana"
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
	cfg                 *config.Config
	configPath          string
	sessions            *session.Store
	sessionIndex        *session.SessionIndex
	client              provider.Client
	clientProvider      provider.ClientProvider
	usageClientProvider mana.UsageClientProvider
	store               *secrets.Store
	bwStore             *bitwarden.Store
	startTime           time.Time

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
			return config.SetInFile(path, target, value, fm)
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
		ManaName:            config.DerefStr(p.cfg.Mana.Name),
		StartTime:           p.startTime,
		CompactionThreshold: p.compactionThreshold,
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
		SkillsDirs:          p.skillsDirs,
		TokenCountCache:     command.NewTokenCountCache(),
		ConfigureFacet:      p.configureFacet,
		UsageClientProvider: p.usageClientProvider,
		Resolved:            p.resolved,
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
	// /reload rebuilds the system prompt from disk so the NEXT turn sees
	// edited workspace/character files. That only works for API backends,
	// which reassemble the prompt per turn. A delegated backend (CC) captures
	// its base system prompt once at session start (StartOpts.SystemPrompt) and
	// can't be refreshed mid-session — only /restart, /reset, or compaction
	// rebuild it. Registering /reload for delegated agents would falsely imply
	// the edit took effect, so gate it to API backends. Same predicate as
	// agents.go's isDelegated. (TODO #799)
	if p.acfg.Backend == "" || p.acfg.Backend == "api" {
		cmds.Register(command.ReloadCommand())
	}
	cmds.Register(command.CompactCommand())
	cmds.Register(command.RestartCommand())
	cmds.Register(command.SecretsCommand())
	cmds.Register(command.BitwardenCommand())
	cmds.Register(command.SessionsCommand())
	cmds.Register(command.AgentsCommand())
	cmds.Register(command.RepeatCommand())
	cmds.Register(command.PassCommand())
	cmds.Register(command.TodoCommand())

	// Tmux command (only if tool is available)
	if p.tmuxTool != nil {
		cmds.Register(command.TmuxCommand())
	}

	// Stop / done
	cmds.Register(command.StopCommand())
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

	// Dynamic mana command — only register if the provider supports usage tracking.
	if p.ag.UsageClient != nil {
		manaName := config.DerefStr(p.cfg.Mana.Name)
		if manaName == "" {
			manaName = "mana"
		}
		cc.ManaName = manaName
		manaCmd := command.ManaCommand(manaName)
		cmds.Register(manaCmd)
		cmds.Register(&command.Command{
			Name: "usage", Hidden: true,
			Execute: manaCmd.Execute,
		})
		if manaName != "m" {
			cmds.Register(&command.Command{
				Name: "m", Hidden: true,
				Execute: manaCmd.Execute,
			})
		}
	}

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

	return cmds, cc
}
