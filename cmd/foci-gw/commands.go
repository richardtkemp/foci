package main

// commands.go — slash command registration, extracted from setupAgent().
//
// registerAgentCommands creates the command registry for a single agent.
// Named helper functions below replace the large inline closures that
// previously lived inside setupAgent(), reducing its cognitive complexity.

import (
	"context"
	"encoding/json"
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
	sessionKeyFromCtx   func(context.Context) string
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
	ctx                 context.Context

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
// This was extracted from setupAgent() to reduce its cognitive complexity.
func registerAgentCommands(p cmdRegParams, lastMsgStore *command.LastMessageStore) *command.Registry {
	cmds := command.NewRegistry()
	aliases := p.cfg.Models.Aliases

	cmds.Register(command.NewPingCommand())
	cmds.Register(command.NewStatusCommand(func() command.StatusInfo {
		return buildStatusInfo(p)
	}, p.cfg.Logging.APIFile))
	cmds.Register(command.NewCacheCommand(p.cfg.Logging.APIFile))
	cmds.Register(command.NewLastCommand(p.cfg.Logging.APIFile))
	cmds.Register(command.NewCostCommand(p.cfg.Logging.APIFile))

	if p.tmuxTool != nil {
		cmds.Register(command.NewTmuxCommand(func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			return p.tmuxTool.Execute(ctx, params)
		}))
	}

	cmds.Register(command.NewContextCommand(p.cfg.Logging.APIFile, buildContextInfoFn(
		p.ag, p.bootstrap, p.registry, p.acfg, p.client, p.sessions, p.defaultSessionKey, p.compactionThreshold,
	)))

	cmds.Register(command.NewResetCommand(func() error {
		return runReset(p)
	}))

	cmds.Register(command.NewModelCommand(
		func(ctx context.Context) string { return p.ag.SessionModel(p.sessionKeyFromCtx(ctx)) },
		func(ctx context.Context, endpoint string, m string, format string) {
			var client provider.Client
			if endpoint != "" && format != "" && p.clientProvider != nil {
				client = p.clientProvider.ResolveEndpointClient(endpoint, format)
			}
			p.ag.SetSessionModel(p.sessionKeyFromCtx(ctx), m, endpoint, format, client)
		},
		func(input string) (string, string, string) {
			resolved, err := config.ResolveModel(input, "", aliases)
			if err != nil {
				return "", input, ""
			}
			return resolved.Endpoint, resolved.Developer + "/" + resolved.ModelID, resolved.Format
		},
		aliases,
	))

	cmds.Register(command.NewEffortCommand(
		func(ctx context.Context) string { return p.ag.SessionEffort(p.sessionKeyFromCtx(ctx)) },
		func(ctx context.Context, e string) { p.ag.SetSessionEffort(p.sessionKeyFromCtx(ctx), e) },
	))
	cmds.Register(command.NewThinkingCommand(
		func(ctx context.Context) string { return p.ag.SessionThinking(p.sessionKeyFromCtx(ctx)) },
		func(ctx context.Context, t string) { p.ag.SetSessionThinking(p.sessionKeyFromCtx(ctx), t) },
	))
	cmds.Register(command.NewToolsCommand(func() []command.ToolInfo {
		var infos []command.ToolInfo
		for _, t := range p.registry.All() {
			infos = append(infos, command.ToolInfo{Name: t.Name, Description: t.Description})
		}
		return infos
	}))
	configSetDeps := &command.ConfigSetDeps{
		ConfigPath:      p.configPath,
		AgentID:         p.acfg.ID,
		SectionsFn:      config.FieldSections,
		FieldsInSection: config.FieldsInSection,
		LookupFn:        config.LookupField,
		SetInFileFn:     config.SetInFile,
	}
	cmds.Register(command.NewConfigCommand(func(ctx context.Context, args string) (string, error) {
		return runConfig(p, ctx, args)
	}, cmds, configSetDeps))
	cmds.Register(command.NewPromptsCommand(command.PromptsCmdDeps{
		DataFn:    func() command.PromptsData { return buildPromptsData(p) },
		SendDocFn: buildSendDocFn(p),
		DiffSummaryFn: func(ctx context.Context, customText, defaultText, name string) (string, error) {
			return buildDiffSummary(p, ctx, customText, defaultText, name)
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

	// Dynamic mana command — only register if the provider supports usage tracking.
	if p.ag.UsageClient != nil {
		manaName := p.cfg.ManaWarnings.Name
		if manaName == "" {
			manaName = "mana"
		}
		manaFn := func(ctx context.Context) (string, error) { return manaCheck(p, manaName, ctx) }
		cmds.Register(command.NewManaCommand(manaName, manaFn))
		cmds.Register(&command.Command{
			Name: "usage", Hidden: true,
			Execute: func(ctx context.Context, args string) (string, error) { return manaFn(ctx) },
		})
		if manaName != "m" {
			cmds.Register(&command.Command{
				Name: "m", Hidden: true,
				Execute: func(ctx context.Context, args string) (string, error) { return manaFn(ctx) },
			})
		}
	}

	cmds.Register(command.NewReloadCommand(func() (string, error) {
		return runReload(p)
	}))

	// Custom script commands from config
	for _, cc := range p.cfg.Commands {
		cmds.Register(command.NewScriptCommand(cc.Name, cc.Description, cc.Script, cc.Timeout))
	}

	// Skill slash commands (command + script in frontmatter)
	for _, s := range p.skillRegistry.All() {
		if s.Command != "" && s.Script != "" {
			name := strings.TrimPrefix(s.Command, "/")
			cmds.Register(command.NewScriptCommand(name, s.Description, s.Script, 30))
		}
	}

	// /multiball and /mb — per-agent pool first, shared pool fallback.
	forkFn := func(ctx context.Context) (string, error) {
		return forkMultiball(p, ctx)
	}
	cmds.Register(command.NewMultiballCommand(forkFn))
	cmds.Register(&command.Command{
		Name: "mb", Description: "Fork session to a secondary bot (alias for /multiball)",
		Category: "session", Hidden: true,
		Execute: func(ctx context.Context, args string) (string, error) { return forkFn(ctx) },
	})

	agentNewDeps := &command.AgentNewDeps{
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
			// Use new ResolveModel for resolution
			resolved, err := config.ResolveModel(input, "", aliases)
			if err != nil {
				// Fallback for invalid input - return as-is
				return input
			}
			// Return full developer/model_id format
			return resolved.Developer + "/" + resolved.ModelID
		},
	}
	cmds.Register(command.NewAgentsCommand(p.agentListFn, cmds, agentNewDeps))

	cmds.Register(command.NewCompactCommand(func(ctx context.Context, dryRun bool) (int, error) {
		return runCompaction(p, ctx, dryRun)
	}))
	cmds.Register(command.NewRepeatCommand(lastMsgStore))
	cmds.Register(command.NewSessionsCommand(buildSessionsDeps(p)))
	cmds.Register(command.NewSecretsCommand(p.store))
	cmds.Register(command.NewBitwardenCommand(p.bwStore, p.cfg.Bitwarden.Enabled))
	cmds.Register(command.NewRestartCommand(nil))

	return cmds
}
