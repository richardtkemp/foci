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
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/secrets"
	"foci/internal/secrets/bitwarden"
	"foci/internal/session"
	"foci/internal/skills"
	"foci/internal/state"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// cmdRegParams holds all dependencies needed for slash command registration.
type cmdRegParams struct {
	// Per-agent state
	ag                  *agent.Agent
	acfg                config.AgentConfig
	defaultSessionKey   func() string
	bootstrap           *workspace.Bootstrap
	promptSearchDirs    []string
	compactionThreshold float64

	// Shared infrastructure
	cfg                 *config.Config
	configPath          string
	sessions            *session.Store
	stateStore          *state.Store
	sessionIndex        *session.SessionIndex
	client              provider.Client
	clientProvider      provider.ClientProvider
	usageClientProvider provider.UsageClientProvider
	store               *secrets.Store
	bwStore             *bitwarden.Store
	startTime           time.Time

	// Tools & skills
	registry      *tools.Registry
	tmuxTool      *tools.Tool
	skillsDirs    []string
	skillRegistry *skills.Registry

	// Cross-agent
	agentListFn func() []command.AgentInfo

	// Platform
	plat               *platform.Messaging
	connMgr            platform.ConnectionManager
	configureMultiball func(platform.Connection)
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
		SetInFileFn:     config.SetInFile,
	}

	// Build AgentNewDeps
	aliases := p.cfg.Models.Aliases
	agentNewDeps := &command.AgentNewDeps{
		Registry:    cmds,
		ConfigPath:  p.configPath,
		DefaultsDir: filepath.Join(filepath.Dir(p.acfg.Workspace), "shared", "defaults"),
		HomeDir:     filepath.Dir(p.acfg.Workspace),
		ListFn:      p.agentListFn,
		PreFlightFn: func(agentID string) []string {
			if p.plat == nil {
				return nil
			}
			return p.plat.AgentPreFlight(agentID)
		},
		ResolveModel: func(input string) string {
			resolved, err := config.ResolveModel(input, "", aliases)
			if err != nil {
				return input
			}
			return resolved.Developer + "/" + resolved.ModelID
		},
	}

	// Construct the CommandContext
	cc := command.CommandContext{
		Agent:               p.ag,
		Sessions:            p.sessions,
		Bootstrap:           p.bootstrap,
		StateStore:          p.stateStore,
		SessionIndex:        p.sessionIndex,
		Config:              p.cfg,
		AgentConfig:         p.acfg,
		DefaultSessionKey:   p.defaultSessionKey,
		Client:              p.client,
		ClientProvider:      p.clientProvider,
		ConnMgr:             p.connMgr,
		PromptSearchDirs:    p.promptSearchDirs,
		APILogPath:          p.cfg.Logging.APIFile,
		EventLogPath:        p.cfg.Logging.EventFile,
		ConfigPath:          p.configPath,
		ModelAliases:        aliases,
		ToolsRegistry:       p.registry,
		TmuxTool:            p.tmuxTool,
		BuildInfo: command.BuildInfo{
			Version:   version,
			GoVersion: goVersion,
			GitCommit: gitCommit,
			BuildTime: buildTime,
		},
		ManaName:            p.cfg.ManaWarnings.Name,
		StartTime:           p.startTime,
		CompactionThreshold: p.compactionThreshold,
		SecretsStore:        p.store,
		BitwardenStore:      p.bwStore,
		BitwardenEnabled:    p.cfg.Bitwarden.Enabled,
		AgentListFn:         p.agentListFn,
		LastMessageStore:    lastMsgStore,
		ConfigSetDeps:       configSetDeps,
		AgentNewDeps:        agentNewDeps,
		SkillsDirs:          p.skillsDirs,
		TokenCountCache:     command.NewTokenCountCache(),
		ConfigureMultiball:  p.configureMultiball,
		UsageClientProvider: p.usageClientProvider,
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
	cmds.Register(command.DisplayCommand())
	cmds.Register(command.ToolsCommand())
	cmds.Register(command.ConfigCommand())
	cmds.Register(command.PromptsCommand())
	cmds.Register(command.LogCommand())
	cmds.Register(command.ErrorsCommand())
	cmds.Register(command.VersionCommand())
	cmds.Register(command.HelpCommand(cmds))
	cmds.Register(command.ReloadCommand())
	cmds.Register(command.CompactCommand())
	cmds.Register(command.RestartCommand())
	cmds.Register(command.SecretsCommand())
	cmds.Register(command.BitwardenCommand())
	cmds.Register(command.SessionsCommand())
	cmds.Register(command.AgentsCommand())
	cmds.Register(command.RepeatCommand())

	// Tmux command (only if tool is available)
	if p.tmuxTool != nil {
		cmds.Register(command.TmuxCommand())
	}

	// Multiball and alias
	cmds.Register(command.MultiballCommand())
	cmds.Register(&command.Command{
		Name: "mb", Description: "Fork session to a secondary bot (alias for /multiball)",
		Category: "session", Hidden: true,
		Execute: command.MultiballCommand().Execute,
	})

	// Dynamic mana command — only register if the provider supports usage tracking.
	if p.ag.UsageClient != nil {
		manaName := p.cfg.ManaWarnings.Name
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
	for _, s := range p.skillRegistry.All() {
		if s.Command != "" && s.Script != "" {
			name := strings.TrimPrefix(s.Command, "/")
			cmds.Register(command.ScriptCommand(name, s.Description, s.Script, 30))
		}
	}

	return cmds, cc
}
