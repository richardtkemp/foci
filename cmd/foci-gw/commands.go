package main

// commands.go — slash command registration, extracted from setupAgent().
//
// registerAgentCommands creates the command registry for a single agent.
// Named helper functions below replace the large inline closures that
// previously lived inside setupAgent(), reducing its cognitive complexity.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/anthropic"
	"foci/internal/command"
	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/mana"
	"foci/prompts"
	"foci/internal/provider"
	"foci/internal/secrets"
	"foci/internal/secrets/bitwarden"
	"foci/internal/session"
	"foci/internal/skills"
	"foci/internal/state"
	"foci/internal/telegram"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// cmdRegParams holds all dependencies needed for slash command registration.
type cmdRegParams struct {
	// Per-agent state
	ag                *agent.Agent
	acfg              config.AgentConfig
	defaultSessionKey func() string
	sessionKeyFromCtx func(context.Context) string
	bootstrap         *workspace.Bootstrap
	promptSearchDirs  []string
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
	botMgr              *telegram.BotManager
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
		func(ctx context.Context, endpoint string, m string) {
			var client provider.Client
			if endpoint != "" && p.clientProvider != nil {
				client = p.clientProvider.ResolveEndpointClient(endpoint, m)
			}
			p.ag.SetSessionModel(p.sessionKeyFromCtx(ctx), m, endpoint, client)
		},
		func(input string) (string, string) {
			// Use new ResolveModel for resolution
			resolved, err := config.ResolveModel(input, "", aliases)
			if err != nil {
				// Fallback for invalid input - return empty endpoint
				return "", input
			}
			// Return endpoint and full model string (developer/model_id)
			return resolved.Endpoint, resolved.Developer + "/" + resolved.ModelID
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

	// Dynamic mana command (configurable name: /mana, /juice, /credits, etc.)
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
		return forkMultiball(p, cmds, ctx)
	}
	cmds.Register(command.NewMultiballCommand(forkFn))
	cmds.Register(&command.Command{
		Name: "mb", Description: "Fork session to a secondary bot (alias for /multiball)",
		Category: "session", Hidden: true,
		Execute: func(ctx context.Context, args string) (string, error) { return forkFn(ctx) },
	})

	agentStore := p.store.ForAgent(p.acfg.ID)
	agentNewDeps := &command.AgentNewDeps{
		ConfigPath:  p.configPath,
		DefaultsDir: filepath.Join(filepath.Dir(p.acfg.Workspace), "shared", "defaults"),
		HomeDir:     filepath.Dir(p.acfg.Workspace),
		ListFn:      p.agentListFn,
		SecretNames: func() []string { return agentStore.Names() },
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

// ---------------------------------------------------------------------------
// Named helper functions — extracted from large inline closures
// ---------------------------------------------------------------------------

// buildStatusInfo gathers status info for the /status command.
func buildStatusInfo(p cmdRegParams) command.StatusInfo {
	sk := p.defaultSessionKey()
	return command.StatusInfo{
		AgentID:          p.acfg.ID,
		SessionKey:       sk,
		MessageCount:     sessionMessageCount(p.sessions, sk),
		Model:            p.ag.Model,
		Uptime:           time.Since(p.startTime),
		StartTime:        p.startTime,
		AgentBusy:        p.ag.IsProcessing(),
		CreatedAt:        p.sessions.CreatedAt(sk),
		LastActivity:     p.sessions.LastActivity(sk),
		ContextLimit:     compaction.ContextLimit(p.ag.Model),
		CompactThreshold: p.compactionThreshold,
	}
}

// runReset clears the current session with memory formation.
func runReset(p cmdRegParams) error {
	if p.ag.IsProcessing() {
		return fmt.Errorf("agent is processing — send /stop first, then /reset")
	}
	sk := p.defaultSessionKey()
	if sk == "" {
		return fmt.Errorf("no active session to reset")
	}
	resetOrientPath := resolveOrientPath(
		p.acfg.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt,
		p.acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt,
	)
	fireSessionEndMemory(p.ag, p.sessions, sk, p.acfg.MemoryFormation, func(bk, pk, bt string) string {
		return buildBranchOrientation(resetOrientPath, bk, pk, bt, false, p.promptSearchDirs)
	}, p.promptSearchDirs, p.ctx)
	writer := p.sessions.For(sk)
	if err := writer.Clear(sk); err != nil {
		return err
	}
	p.bootstrap.Reload()
	p.ag.InvalidateSystemCaches()
	return nil
}


// runConfig handles the /config command.
func runConfig(p cmdRegParams, _ context.Context, args string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(args)) {
	case "toml":
		return config.FormatConfigTOML(p.cfg, p.acfg), nil
	case "table":
		return strings.Join(config.FormatConfigGrouped(p.cfg, p.acfg), "\x00"), nil
	case "available":
		return config.FormatAvailable(p.cfg, p.acfg), nil
	default:
		return "/config toml — raw TOML of running config (secrets redacted)\n/config table — formatted table of current config values\n/config available — unset options with defaults\n/config set [section.key=value] — edit config file", nil
	}
}

// buildPromptsData constructs the data for the /prompts command.
func buildPromptsData(p cmdRegParams) command.PromptsData {
	dirs := p.promptSearchDirs
	acfg := p.acfg
	cfg := p.cfg

	allPrompts := []command.PromptInfo{
		resolvePromptInfo("compaction_summary",
			resolveString(acfg.CompactionSummaryPrompt, cfg.Sessions.CompactionSummaryPrompt),
			"compaction-summary.md", prompts.CompactionSummary(), dirs),
		resolvePromptInfo("branch_orient_multiball",
			resolveOrientPath(acfg.BranchOrientationMultiballPrompt, cfg.Sessions.BranchOrientationMultiballPrompt, acfg.BranchOrientationPrompt, cfg.Sessions.BranchOrientationPrompt),
			"branch-orientation-multiball.md", prompts.BranchOrientationMultiball(), dirs),
		resolvePromptInfo("branch_orient_headless",
			resolveOrientPath(acfg.BranchOrientationHeadlessPrompt, cfg.Sessions.BranchOrientationHeadlessPrompt, acfg.BranchOrientationPrompt, cfg.Sessions.BranchOrientationPrompt),
			"branch-orientation-headless.md", prompts.BranchOrientationHeadless(), dirs),
		resolvePromptInfo("keepalive",
			acfg.Keepalive.Prompt,
			"keepalive.md", prompts.Keepalive(), dirs),
		resolvePromptInfo("background",
			acfg.Background.Prompt,
			"background.md", prompts.Background(), dirs),
		resolvePromptInfo("memory_formation",
			acfg.MemoryFormation.IntervalPrompt,
			"memory-formation.md", prompts.MemoryFormation(), dirs),
		resolvePromptInfo("memory_consolidation",
			acfg.MemoryFormation.ConsolidationPrompt,
			"memory-consolidation.md", prompts.MemoryConsolidation(), dirs),
		resolvePromptInfo("memory_session_end",
			acfg.MemoryFormation.SessionEndPrompt,
			"memory-formation.md", prompts.MemoryFormation(), dirs),
	}

	allPrompts = append(allPrompts,
		inlinePromptInfo("compaction_handoff",
			resolveString(acfg.CompactionHandoffMsg, cfg.Sessions.CompactionHandoffMsg),
			prompts.CompactionHandoff()),
		inlinePromptInfo("braindead_warning",
			acfg.BraindeadPrompt, ""),
	)

	embedded := map[string]string{
		"compaction-summary.md":           prompts.CompactionSummary(),
		"compaction-handoff.md":           prompts.CompactionHandoff(),
		"branch-orientation-multiball.md": prompts.BranchOrientationMultiball(),
		"branch-orientation-headless.md":  prompts.BranchOrientationHeadless(),
		"keepalive.md":                    prompts.Keepalive(),
		"background.md":                   prompts.Background(),
		"memory-formation.md":             prompts.MemoryFormation(),
		"memory-consolidation.md":         prompts.MemoryConsolidation(),
	}

	type promptDef struct {
		label, configPath, filename string
		embeddedDefault             string
	}
	fileDefs := []promptDef{
		{"compaction_summary", resolveString(acfg.CompactionSummaryPrompt, cfg.Sessions.CompactionSummaryPrompt), "compaction-summary.md", prompts.CompactionSummary()},
		{"branch_orient_multiball", resolveOrientPath(acfg.BranchOrientationMultiballPrompt, cfg.Sessions.BranchOrientationMultiballPrompt, acfg.BranchOrientationPrompt, cfg.Sessions.BranchOrientationPrompt), "branch-orientation-multiball.md", prompts.BranchOrientationMultiball()},
		{"branch_orient_headless", resolveOrientPath(acfg.BranchOrientationHeadlessPrompt, cfg.Sessions.BranchOrientationHeadlessPrompt, acfg.BranchOrientationPrompt, cfg.Sessions.BranchOrientationPrompt), "branch-orientation-headless.md", prompts.BranchOrientationHeadless()},
		{"keepalive", acfg.Keepalive.Prompt, "keepalive.md", prompts.Keepalive()},
		{"background", acfg.Background.Prompt, "background.md", prompts.Background()},
		{"memory_formation", acfg.MemoryFormation.IntervalPrompt, "memory-formation.md", prompts.MemoryFormation()},
		{"memory_consolidation", acfg.MemoryFormation.ConsolidationPrompt, "memory-consolidation.md", prompts.MemoryConsolidation()},
		{"memory_session_end", acfg.MemoryFormation.SessionEndPrompt, "memory-formation.md", prompts.MemoryFormation()},
	}
	resolvedTexts := make(map[string]string, len(fileDefs)+2)
	defaultTexts := make(map[string]string, len(fileDefs)+2)
	for _, d := range fileDefs {
		resolvedTexts[d.label] = prompts.ResolvePrompt(d.configPath, d.filename, d.embeddedDefault, dirs...)
		defaultTexts[d.label] = d.embeddedDefault
	}

	handoffVal := resolveString(acfg.CompactionHandoffMsg, cfg.Sessions.CompactionHandoffMsg)
	if handoffVal == "" {
		resolvedTexts["compaction_handoff"] = prompts.CompactionHandoff()
	} else if handoffVal != "none" {
		resolvedTexts["compaction_handoff"] = handoffVal
	}
	defaultTexts["compaction_handoff"] = prompts.CompactionHandoff()
	if acfg.BraindeadPrompt != "" && acfg.BraindeadPrompt != "none" {
		resolvedTexts["braindead_warning"] = acfg.BraindeadPrompt
	}
	defaultTexts["braindead_warning"] = ""

	configuredPaths := make(map[string]bool)
	for _, pi := range allPrompts {
		if pi.Path != "" {
			configuredPaths[pi.Path] = true
		}
	}

	var promptDirs []string
	var files []command.PromptFile
	sharedDir := filepath.Join(filepath.Dir(acfg.Workspace), "shared", "prompts")
	wsDir := filepath.Join(acfg.Workspace, "prompts")
	for _, dir := range []string{sharedDir, wsDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		promptDirs = append(promptDirs, dir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			fullPath := filepath.Join(dir, e.Name())
			files = append(files, command.PromptFile{
				Dir:        dir,
				Name:       e.Name(),
				Configured: configuredPaths[fullPath],
			})
		}
	}

	knownFilenames := make(map[string]bool, len(embedded)+1)
	for name := range embedded {
		knownFilenames[name] = true
	}
	knownFilenames["first-run.md"] = true

	return command.PromptsData{
		AgentID:             acfg.ID,
		Prompts:             allPrompts,
		PromptDirs:          promptDirs,
		Files:               files,
		KnownFilenames:      knownFilenames,
		WorkspacePromptsDir: filepath.Join(acfg.Workspace, "prompts"),
		EmbeddedPrompts:     embedded,
		ResolvedTexts:       resolvedTexts,
		DefaultTexts:        defaultTexts,
	}
}

// buildSendDocFn returns a function that sends a document via the agent's primary bot.
func buildSendDocFn(p cmdRegParams) func(path string) error {
	return func(path string) error {
		bot := p.botMgr.PrimaryBot(p.acfg.ID)
		if bot == nil {
			return fmt.Errorf("no bot available")
		}
		return bot.SendDocument(path)
	}
}

// buildDiffSummary generates an AI summary comparing custom vs default prompt text.
func buildDiffSummary(p cmdRegParams, ctx context.Context, customText, defaultText, name string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Determine cheap model alias based on agent's model
	cheapAlias := "haiku"
	_, bareModelID := config.SplitDeveloperModel(p.acfg.Model)
	if strings.HasPrefix(bareModelID, "gemini-") {
		cheapAlias = "flash"
	}

	// Resolve the cheap model
	var diffClient provider.Client
	var cheapModel string
	if resolved, err := config.ResolveModel(cheapAlias, "", p.cfg.Models.Aliases); err == nil && p.clientProvider != nil {
		diffClient = p.clientProvider.ResolveEndpointClient(resolved.Endpoint, resolved.ModelID)
		cheapModel = resolved.Developer + "/" + resolved.ModelID
	}
	if diffClient == nil {
		diffClient = p.client
		cheapModel = cheapAlias
	}

	prompt := fmt.Sprintf("Below are two versions of the %q prompt. These prompts are injected into AI agent sessions to guide agent behaviour during specific operations (compaction, keepalive, memory formation, etc).\n\n--- DEFAULT (embedded) ---\n%s\n\n--- CURRENT (resolved from config) ---\n%s\n\nConcisely summarise: 1) what the default version instructs the agent to do, 2) what the current version instructs, 3) key differences.", name, defaultText, customText)
	resp, err := provider.Send(callCtx, diffClient, &provider.MessageRequest{
		Model:     cheapModel,
		MaxTokens: 1024,
		Messages:  []provider.Message{{Role: "user", Content: provider.TextContent(prompt)}},
	}, nil)
	if err != nil {
		return "", err
	}
	return provider.TextOf(resp.Content), nil
}

// manaCheck fetches and formats the current mana/quota status.
func manaCheck(p cmdRegParams, manaName string, ctx context.Context) (string, error) {
	emojis := []string{"🔮", "✨", "🌙", "⚡", "🪄", "💎", "🌟", "🔥", "🧿", "🪬", "💫", "🌀", "🎇"}
	emoji := emojis[rand.IntN(len(emojis))] // #nosec G404 - non-security use (emoji selection)
	displayName := strings.ToUpper(manaName[:1]) + manaName[1:]

	// Get session-aware usage client
	sessionKey := p.sessionKeyFromCtx(ctx)
	usageClient := p.ag.SessionUsageClient(sessionKey)
	if usageClient == nil {
		return fmt.Sprintf("%s %s: No usage data (provider does not support usage API)", emoji, displayName), nil
	}

	usageClient.Invalidate() // force fresh fetch for explicit user query
	usage, err := usageClient.GetUsage(ctx)
	if err != nil {
		return fmt.Sprintf("%s Error fetching %s: %v", emoji, displayName, err), nil
	}
	percent := mana.FormatPercent(usage)
	if percent == "" {
		return fmt.Sprintf("%s %s: unknown", emoji, displayName), nil
	}
	result := fmt.Sprintf("%s %s: %s remaining", emoji, displayName, percent)
	if reset := mana.FormatReset(usage); reset != "" {
		result += fmt.Sprintf(" (resets %s)", reset)
	}
	return result, nil
}

// runReload reloads workspace files, skills, and system prompt.
func runReload(p cmdRegParams) (string, error) {
	p.bootstrap.Reload()
	p.ag.InvalidateSystemCaches()
	checkSystemPromptSizes(p.bootstrap, p.cfg.Sessions, p.acfg.ID)
	newSkillRegistry := skills.Load(p.skillsDirs)
	var newExtraSystemBlocks []anthropic.SystemBlock
	if newSkillRegistry.Len() > 0 {
		newExtraSystemBlocks = []anthropic.SystemBlock{
			{Type: "text", Text: newSkillRegistry.SystemBlock(p.acfg.Workspace)},
		}
	}
	maxRC := p.cfg.Tools.MaxResultChars
	if len(p.acfg.SkillsDirs) > 0 {
		maxRC = resolveInt(p.acfg.MaxResultChars, p.cfg.Tools.MaxResultChars)
	}
	checkSkillSizes(newSkillRegistry, maxRC, p.acfg.ID)
	p.ag.ExtraSystemBlocks = newExtraSystemBlocks
	return fmt.Sprintf("Reloaded:\n- workspace files (system prompt)\n- %d skills\n\nNote: foci.toml config changes require a service restart to take effect. Prompt file changes take effect immediately.", newSkillRegistry.Len()), nil
}

// forkMultiball forks the current session to a secondary multiball bot.
func forkMultiball(p cmdRegParams, cmds *command.Registry, ctx context.Context) (string, error) {
	if !p.botMgr.HasMultiball(p.acfg.ID) {
		return "", fmt.Errorf("no multiball bots configured")
	}
	secBot, ok := p.botMgr.AcquireMultiball(p.acfg.ID)
	if !ok {
		return "", fmt.Errorf("all multiball bots are busy")
	}

	secBot.SetAgentAndCommands(p.ag, cmds)
	applyAgentDisplaySettings(secBot, p.acfg, p.cfg)

	parentKey := p.defaultSessionKey()
	if chatID, ok := ctx.Value(command.ChatIDKey{}).(int64); ok && chatID != 0 {
		if bot := p.botMgr.PrimaryBot(p.acfg.ID); bot != nil {
			parentKey = bot.SessionKeyForChat(chatID)
		} else {
			parentKey = telegram.NewSessionKeyForChat(p.acfg.ID, chatID)
		}
	}
	if parentKey == "" {
		secBot.SetSessionKey("")
		return "", fmt.Errorf("no active session to fork from")
	}

	// Multiball is a branch from the parent session
	branchKey, err := session.BranchFromSession(parentKey)
	if err != nil {
		secBot.SetSessionKey("")
		return "", fmt.Errorf("create multiball key: %w", err)
	}

	orientPath := resolveOrientPath(
		p.acfg.BranchOrientationMultiballPrompt, p.cfg.Sessions.BranchOrientationMultiballPrompt,
		p.acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt,
	)
	orientText := buildBranchOrientation(orientPath, branchKey, parentKey, "multiball", true, p.promptSearchDirs)
	if err := p.sessions.CreateBranchWithOptions(parentKey, branchKey, session.BranchOptions{
		OrientationMessage: orientText,
	}); err != nil {
		secBot.SetSessionKey("")
		return "", fmt.Errorf("create branch: %w", err)
	}

	secBot.SetSessionKey(branchKey)
	if primaryBot := p.botMgr.PrimaryBot(p.acfg.ID); primaryBot != nil {
		secBot.SetChatID(primaryBot.ChatID())
	}
	secBot.SendNotification("🎱 Forked from main. What do you need?")

	return fmt.Sprintf("Forked to @%s (session: %s)", secBot.Username(), branchKey), nil
}

// runCompaction executes manual context compaction.
func runCompaction(p cmdRegParams, ctx context.Context, dryRun bool) (int, error) {
	if p.ag.Compactor == nil {
		return 0, fmt.Errorf("compaction is not configured")
	}
	sk := p.defaultSessionKey()
	if sk == "" {
		return 0, fmt.Errorf("no active session to compact")
	}
	mc, _ := p.sessions.MessageCount(sk)
	if mc < 5 {
		return 0, fmt.Errorf("too few messages to compact (%d)", mc)
	}
	if p.ag.CompactionNotifyFunc != nil {
		if dryRun {
			p.ag.CompactionNotifyFunc(sk, "⏳ Running compaction dry-run...")
		} else {
			p.ag.CompactionNotifyFunc(sk, "⏳ Compacting context...")
		}
	}

	system := p.bootstrap.SystemBlocks()
	summaryPrompt := prompts.ResolvePrompt(p.ag.CompactionSummaryPromptPath, "compaction-summary.md", prompts.CompactionSummary(), p.promptSearchDirs...)
	handoffMsg := p.ag.CompactionHandoffMsg
	if handoffMsg == "" {
		handoffMsg = prompts.ResolvePrompt("", "compaction-handoff.md", prompts.CompactionHandoff(), p.promptSearchDirs...)
	}

	summary, err := p.ag.Compactor.Compact(ctx, p.ag.SessionClient(sk), sk, system, summaryPrompt, handoffMsg, dryRun)
	if err != nil {
		return 0, fmt.Errorf("compaction failed: %w", err)
	}

	if dryRun {
		if p.ag.CompactionDebugFunc != nil && summary != "" {
			p.ag.CompactionDebugFunc(sk, summary)
		} else if summary != "" {
			if bot := p.botMgr.PrimaryBot(p.acfg.ID); bot != nil {
				f, tmpErr := os.CreateTemp("", "compaction-dryrun-*.md")
				if tmpErr == nil {
					if _, writeErr := f.WriteString(summary); writeErr == nil {
						_ = f.Close()
						if sendErr := bot.SendDocument(f.Name()); sendErr != nil {
							log.Warnf("agent", "dry-run: send document: %v", sendErr)
						}
					} else {
						_ = f.Close()
					}
					_ = os.Remove(f.Name())
				}
			}
		}
		if p.ag.CompactionNotifyFunc != nil {
			p.ag.CompactionNotifyFunc(sk, "✅ Dry-run complete — summary sent.")
		}
	} else {
		if p.ag.CompactionNotifyFunc != nil {
			p.ag.CompactionNotifyFunc(sk, fmt.Sprintf("✅ Context compacted — %d messages summarised.", mc))
		}
		if p.ag.CompactionDebugFunc != nil && summary != "" {
			p.ag.CompactionDebugFunc(sk, summary)
		}
		p.bootstrap.Reload()
		p.ag.InvalidateSystemCaches()
		p.ag.ResetCacheBaseline(sk)
	}
	return mc, nil
}

// buildSessionsDeps constructs the SessionsDeps for the /sessions command.
func buildSessionsDeps(p cmdRegParams) command.SessionsDeps {
	return command.SessionsDeps{
		AgentID: p.acfg.ID,
		ListFn: func() ([]command.SessionChatInfo, error) {
			chatSessions, err := p.sessions.ListChatSessions(p.acfg.ID)
			if err != nil {
				return nil, err
			}
			var result []command.SessionChatInfo
			for _, cs := range chatSessions {
				info := command.SessionChatInfo{
					ChatID:       cs.ChatID,
					MessageCount: cs.MessageCount,
					LastActivity: cs.LastActivity,
				}
				if p.stateStore != nil {
					var username string
					key := fmt.Sprintf("agent:%s:chat:%d:username", p.acfg.ID, cs.ChatID)
					if p.stateStore.Get(key, &username) {
						info.Username = username
					}
				}
				result = append(result, info)
			}
			return result, nil
		},
		SetDefaultFn: func(chatID int64) error {
			if p.stateStore == nil {
				return fmt.Errorf("no state store configured")
			}
			return p.stateStore.Set("agent:"+p.acfg.ID+":default_chat", chatID)
		},
		DefaultChatFn: func() int64 {
			if p.stateStore == nil {
				return 0
			}
			var chatID int64
			p.stateStore.Get("agent:"+p.acfg.ID+":default_chat", &chatID)
			return chatID
		},
		IndexFn: func(opts command.SessionIndexOpts) ([]command.SessionIndexInfo, error) {
			if p.sessionIndex == nil {
				return nil, fmt.Errorf("session index not available")
			}
			qopts := session.QueryOptions{
				SessionType: opts.TypeFilter,
				Status:      opts.StatusFilter,
				MaxAge:      opts.MaxAge,
				Limit:       50,
			}
			entries, err := p.sessionIndex.Query(qopts)
			if err != nil {
				return nil, err
			}
			var result []command.SessionIndexInfo
			for _, e := range entries {
				result = append(result, command.SessionIndexInfo{
					SessionKey:       e.SessionKey,
					CreatedAt:        e.CreatedAt,
					LastActivityAt:   e.LastActivityAt,
					ParentSessionKey: e.ParentSessionKey,
					SessionType:      string(e.SessionType),
					Status:           string(e.Status),
				})
			}
			return result, nil
		},
	}
}
