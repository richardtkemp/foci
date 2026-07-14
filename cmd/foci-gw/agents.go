package main

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/log"
	mcpkg "foci/internal/mcp"
	"foci/internal/memory"
	"foci/internal/periodic"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/secrets"
	"foci/internal/secrets/bitwarden"
	"foci/internal/session"
	"foci/internal/skills"

	"foci/internal/tools"
	"foci/internal/voice"
	"foci/internal/workspace"
	"foci/shared/prompts"
)

// agentInstance holds all per-agent state.
type agentInstance struct {
	id               string
	ag               *agent.Agent
	cmds             *command.Registry
	cc               command.CommandContext
	registry         *tools.Registry
	bootstrap        *workspace.Bootstrap
	agentCfg         config.AgentConfig
	resolved         *config.LiveValue[*config.ResolvedAgentConfig] // hot-swappable pre-merged agent+global config; read via LiveConfig()
	promptSearchDirs []string                                       // directories to search for prompt files
	skillsDirs       []string                    // skill directories (shared + per-agent) for reflection detection
	tmuxClearAll     func()                      // clears tmux tool state (watches, owned sessions)
	tmuxWatchCount   func() int                  // returns number of active tmux watches
	kaRunner         *periodic.Runner            // keepalive & background work timer
	mcpManager       *mcpkg.Manager              // nil if no MCP servers configured

	// periodicRederive recomputes the runner's live-tunable settings from a
	// freshly loaded config (set by setupPeriodic, called from liveapply.go).
	periodicRederive func(*config.Config, config.AgentConfig) periodic.Settings

	// Test-only override fields. Used by the env-gated testharness
	// control socket (see testharness_control.go). Production code never
	// writes these; the periodic closures consult them only when the
	// matching pointer/atomic indicates an override is set. Both fields
	// are accessed concurrently from the periodic-fire goroutine and
	// from the control-socket handler, so use atomics throughout.
	//
	// testActiveWorkOverride: -1 means unset (fall back to tmuxWatchCount),
	// any value ≥ 0 means HasActiveWorkFn should return that value.
	// testCanFireOverride: nil means unset (fall back to
	// CanFireBackgroundOperation), non-nil means CanFireFunc should
	// return its allowed/reason verbatim.
	// stopped: when true the agentResolverFn returns nil for this
	// instance's ID, simulating a stopped bot for session_router /
	// session_notify drop-path testing.
	testActiveWorkOverride atomic.Int64
	testCanFireOverride    atomic.Pointer[testCanFireState]
	stopped                atomic.Bool
}

// LiveConfig returns the agent's current resolved config. It is hot-swapped by
// the config re-resolve applier (liveapply.go), so callers must read through
// it on each use rather than caching the returned pointer — that is what makes
// a field consumed here live-appliable. The returned *ResolvedAgentConfig is an
// immutable snapshot; never mutate it in place.
func (inst *agentInstance) LiveConfig() *config.ResolvedAgentConfig {
	return inst.resolved.Load()
}

// testCanFireState is the override payload for CanFireFunc when set
// via the testharness control socket. Used only by tests.
type testCanFireState struct {
	allowed bool
	reason  string
}

// setupParams holds the shared resources needed by each agent.
type setupParams struct {
	acfg                config.AgentConfig
	cfg                 *config.Config
	resolved            *config.ResolvedAgentConfig
	resolvedLive        *config.LiveValue[*config.ResolvedAgentConfig] // hot-swappable; same instance as agentInstance.resolved once finalize() runs
	configPath          string
	clientProvider      provider.ClientProvider
	sessions            *session.Store
	store               *secrets.Store
	bwStore             *bitwarden.Store
	memBackends         map[string]memory.Searcher
	convReader          *memory.ConversationReader
	reminderStore       *memory.ReminderStore
	scratchpadStore     *memory.Scratchpad
	todoStore           *memory.TodoStore
	taskListStore       *memory.TaskListStore
	sessionIndex        *session.SessionIndex
	ttsMap              map[string]voice.TTS
	sttMap              map[string]voice.STT
	braveKey            string
	gwSocketPath        string         // Unix socket path for same-user CLI auth (injected into child env as FOCI_GW_SOCK)
	skillLoader         *skills.Loader // shared across all agents so the shared skills dir is scanned/warned once, not once per agent

	startTime       time.Time
	ctx             context.Context
	agentListFn     func() []command.AgentInfo
	agentResolverFn func(agentID string) *agentInstance
	connMgr         platform.ConnectionManager
	plat            *platform.Messaging
}

// setupAgent wires up a single agent with its own tools, commands, bootstrap, and bot.
// This is the unified entry point for both API and delegated agents.
func setupAgent(p setupParams) *agentInstance {
	acfg := p.acfg

	// --- Shared preamble ---
	shared := resolveSharedSetup(p)
	p = shared.p // p.resolved is now set

	ag := shared.newAgent()

	// Universal configuration (compaction, warnings, model defaults, etc.)
	compactor, compactionThreshold := buildCompactor(p, nil)
	configureUniversal(ag, p, compactor)

	// Wake scheduler is transport-independent: build the goroutine machinery
	// and restore pending wakes here so both API and delegated transports can
	// register the remind tool into their own registries below.
	agLazy := func() *agent.Agent { return ag }
	shared.wakeScheduleFn = buildWakeScheduler(agLazy, p.reminderStore, acfg.ID, p.ctx, p.connMgr)

	// Transport-specific configuration
	isDelegated := acfg.IsDelegated()
	var fp finalizeParams
	var ok bool

	if isDelegated {
		if !delegator.IsRegistered(acfg.Backend) {
			log.Errorf("agent:"+acfg.ID, "backend %q not registered (missing blank import?)", acfg.Backend)
			return nil
		}
		fp, ok = configureDelegated(ag, p, shared, acfg.Backend, acfg.BackendConfig)
	} else {
		fp, ok = configureAPI(ag, p, shared, compactor)
	}
	if !ok {
		return nil
	}

	fp.compactionThreshold = compactionThreshold
	return shared.finalize(ag, fp)
}

// configureAPI wires up API-specific agent state: model resolution, client,
// tools, bootstrap, streaming, spawn tool, etc. Returns a finalizeParams for
// the shared finalize postamble, and false if setup fails.
func configureAPI(ag *agent.Agent, p setupParams, shared *sharedAgentSetup, compactor *compaction.Compactor) (finalizeParams, bool) {
	acfg := p.acfg
	groupResolver := shared.groupResolver
	promptSearchDirs := shared.promptSearchDirs

	gc := p.resolved.Groups // static-cfg:ignore: groups.* fields are all maps, invisible to the field registry (walkType skips maps) — no /config set path exists yet, see bucket D (a30414b8)

	// Resolve agent's primary model via the chat call site
	primaryResolved := groupResolver.ResolveCall(config.CallChat)
	if primaryResolved == nil {
		log.Errorf("agent:"+acfg.ID, "cannot resolve chat model (agent skipped)")
		return finalizeParams{}, false
	}
	defaultEndpoint := primaryResolved.Endpoint
	defaultFormat := primaryResolved.Format
	resolvedModel := primaryResolved.Developer + "/" + primaryResolved.ModelID
	primaryCacheStrategy := primaryResolved.CacheStrategy
	if primaryCacheStrategy == "" {
		primaryCacheStrategy = "auto"
	}

	// Resolve the API client for this agent's endpoint+format
	client := p.clientProvider.GetClient(defaultEndpoint, defaultFormat)
	if client == nil {
		log.Errorf("agent:"+acfg.ID, "endpoint %q unavailable for model %q (format: %s)", defaultEndpoint, primaryResolved.ModelID, defaultFormat)
		return finalizeParams{}, false
	}

	// Create fallback resolver for automatic model failover
	fallbackResolver := config.NewFallbackResolver(gc.Fallbacks, nil, p.cfg.Models)

	// Build provider-level fallback function from config resolver.
	var fallbackFn provider.FallbackFunc
	if fallbackResolver != nil {
		fallbackFn = func(model string) (string, string, string, bool) {
			rm := fallbackResolver.Resolve(model)
			if rm == nil {
				return "", "", "", false
			}
			return rm.Developer + "/" + rm.ModelID, rm.Endpoint, rm.Format, true
		}
	}

	// Set fallback on the compactor now that we have the resolved fallbackFn
	if compactor != nil && fallbackFn != nil {
		compactor.FallbackFunc = fallbackFn
	}

	connMgr := p.connMgr

	// agLazy closure for tools that need a reference to the agent
	agLazy := func() *agent.Agent { return ag }

	// Per-agent tool registry and supporting services
	registry := tools.NewRegistry()
	notifier := newAsyncNotifier(agLazy, acfg.ID, p.agentResolverFn, p.ctx, connMgr)
	agentStore := p.store.ForAgent(acfg.ID)

	// Bootstrap and skills — computed before tool registration because the spawn
	// tool needs the bootstrap.
	bs := setupBootstrapAndSkills(p, agentStore)

	blockedPaths := resolveBlockedPaths(acfg, p.cfg)
	if len(blockedPaths) > 0 {
		log.Infof("setup", "agent %s: %d blocked write/edit path(s) configured", acfg.ID, len(blockedPaths))
	}
	ttsRepls := voice.MergeReplacements(p.cfg.Voice.TTSReplacements, acfg.Voice.TTSReplacements)
	resolvedLive := p.resolvedLive
	agentTTS := func() voice.TTS {
		vc := resolvedLive.Load().Voice
		return resolveTTS(p.ttsMap, p.cfg.TTS, vc.TTS, vc.TTSRate, ttsRepls)
	}

	// Register all tools from the single data-driven table (see tool_table.go),
	// which is the one source of truth shared with the delegated exec path.
	out := &toolOutputs{}
	registerTools(&toolDeps{
		p:                p,
		path:             pathAPI,
		registry:         registry,
		agentStore:       agentStore,
		notifier:         notifier,
		connMgr:          connMgr,
		agLazy:           agLazy,
		summariser:       tools.NewAPISummariser(client, p.clientProvider, groupResolver, fallbackFn, func() int { return p.resolvedLive.Load().Summary.MaxSummaryInputChars }),
		wakeFn:           shared.wakeScheduleFn,
		sessionNotify:    newSessionNotifyFn(p.agentResolverFn, p.ctx, connMgr, "session_notify"),
		askDeliver:       newSessionNotifyFn(p.agentResolverFn, p.ctx, connMgr, "ask_grader"),
		agentTTS:         agentTTS,
		blockedPaths:     blockedPaths,
		client:           client,
		groupResolver:    groupResolver,
		fallbackFn:       fallbackFn,
		bootstrap:        bs.bootstrap,
		promptSearchDirs: promptSearchDirs,
		resolvedModel:    resolvedModel,
		defaultFormat:    defaultFormat,
		out:              out,
	})
	ag.AskRouter = out.askRouter

	// Per-agent environment block, rebuilt per session so the ## Platform block
	// matches the session's messaging platform (resolved from the durable chat
	// claim — see platformForSession) and its content tracks a live config
	// edit. Crontab count is a subprocess spawn (crontab -l), too expensive to
	// re-run per session — captured once from the startup value of
	// Environment.Enabled; a live edit that turns Environment on after startup
	// renders with crontabCount=0 until restart.
	crontabCount := 0
	if p.resolved.Environment.Enabled { // static-cfg:ignore: startup-only gate for an expensive subprocess spawn, see comment above
		crontabCount = countCrontabJobs()
	}
	envSessionIdx := p.sessionIndex
	envAgentID := acfg.ID
	envBlockFunc := func(sessionKey string) string {
		rc := resolvedLive.Load()
		if !rc.Environment.Enabled {
			return ""
		}
		sessionPlatform := platformForSession(envSessionIdx, envAgentID, sessionKey)
		return buildEnvironmentAPI(acfg, p.configPath, p.cfg, rc, crontabCount, p.plat.ActivePlatformNames(), shared.promptSearchDirs, sessionPlatform)
	}

	// API-specific agent fields. Some of these (MaxToolLoops, MaxResultChars,
	// MaxSummaryInputChars, …) are the Bucket-B static-field-as-fallback
	// pattern — LiveConfigFn takes over when set. Others (DuplicateMessages,
	// BatchPartial*, SummaryContext*, MaxImagePixels, AutoSummarise,
	// MaxOutputTokens) have no live getter yet and are genuinely still
	// restart-required — a candidate for a future pass, not fixed here.
	al := p.resolved.Loop    // static-cfg:ignore: see comment above
	sc := p.resolved.Summary // static-cfg:ignore: see comment above

	ag.Client = client
	ag.ClientProvider = p.clientProvider
	ag.Tools = registry
	ag.ServerTools = out.serverTools
	ag.EnvironmentBlockFunc = envBlockFunc
	ag.AsyncNotifier = notifier
	ag.Model = resolvedModel
	ag.Format = defaultFormat
	ag.Endpoint = defaultEndpoint
	ag.ExtraSystemBlocks = bs.extraSystemBlocks
	ag.CacheStrategy = primaryCacheStrategy
	ag.CacheBustDetect = p.resolved.Debug.CacheBustDetect                                        // static-cfg:ignore: fallback, LiveConfigFn takes over — see comment above
	ag.CacheBustIdleThreshold = time.Duration(p.resolved.Debug.CacheBustIdleMinutes) * time.Minute // static-cfg:ignore: fallback, LiveConfigFn takes over — see comment above
	ag.DuplicateMessages = al.DuplicateMessages
	ag.BatchPartialAssistantMessages = al.BatchPartialAssistantMessages
	ag.BatchPartialJoiner = al.BatchPartialJoiner
	ag.MaxResultChars = sc.MaxResultChars
	ag.ToolResultTempDir = p.cfg.Tools.TempDir
	ag.GroupResolver = groupResolver
	ag.FallbackFunc = fallbackFn
	ag.SummaryContextTurns = sc.SummaryContextTurns
	ag.SummaryContextChars = sc.SummaryContextChars
	ag.MaxSummaryChars = sc.MaxSummaryChars
	ag.MaxSummaryInputChars = sc.MaxSummaryInputChars
	ag.MaxImagePixels = sc.MaxImagePixels
	ag.AutoSummarise = sc.AutoSummarise
	ag.MaxToolLoops = al.MaxToolLoops
	ag.MaxOutputTokens = al.MaxOutputTokens
	ag.Streaming = al.Streaming // static-cfg:ignore: fallback, LiveConfigFn takes over — see comment above

	// Pre-compaction memory formation hook
	compactMemOrientPath := config.DerefStr(config.First(acfg.Sessions.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt))
	compactMemOrientTemplate := prompts.ResolveOrientationTemplate(compactMemOrientPath, false, promptSearchDirs...)
	ag.CompactionMemoryFunc.Add(func(sessionKey string) {
		ag.FireCompactionMemory(p.ctx, sessionKey, compactMemOrientTemplate)
	})

	// Post-creation agent configuration (API-specific)
	setupRedaction(ag, p, agentStore)

	return finalizeParams{
		bootstrap:           bs.bootstrap,
		registry:            registry,
		skillRegistry:       bs.skillRegistry,
		serverTools:         out.serverTools,
		client:              client,
		clientProvider:      p.clientProvider,
		fallbackFn:          fallbackFn,
		tmuxTool:            out.tmuxTool,
		tmuxClearAll:        out.tmuxClearAll,
		tmuxWatchCount:      out.tmuxWatchCount,
		ttsRepls:            ttsRepls,
		mcpManager:          out.mcpMgr,
		skillsDirs:          bs.skillsDirs,
	}, true
}

// checkFirstRun determines whether a first-run onboarding prompt should be
// injected for an agent. Returns the prompt message if injection is needed,
// empty string otherwise. Uses session index agent_metadata to track completion.
func checkFirstRun(idx *session.SessionIndex, acfg config.AgentConfig) string {
	if idx == nil {
		return ""
	}

	// Already completed — nothing to do
	val, err := idx.GetAgentMetadata(acfg.ID, "first_run_completed")
	if err == nil && val == "true" {
		return ""
	}

	// First run — inject the onboarding prompt
	prompt := prompts.FirstRun()
	if prompt == "" {
		return ""
	}

	log.Infof("main", "agent %s: first run detected, injecting onboarding prompt", acfg.ID)
	return prompt
}

// readAndConsumeWelcomeFile checks for a welcome/changelog file written by
// setup.sh on update. If found, returns the file contents and deletes the file.
// Returns empty string if no file exists or file is empty.
func readAndConsumeWelcomeFile(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "" // file doesn't exist — normal for non-update starts
	}
	content := strings.TrimSpace(string(data))
	if err := os.Remove(path); err != nil {
		log.Warnf("main", "remove welcome file: %v", err)
	}
	if content == "" {
		return ""
	}

	log.Infof("main", "found welcome file (%d bytes)", len(content))
	return content
}

// buildServerTool constructs an anthropic server tool config map with optional
// max_uses, allowed_domains, and blocked_domains fields.
func buildServerTool(toolType, toolName string, maxUses int, allowed, blocked []string) provider.ToolDef {
	cfg := map[string]interface{}{
		"type": toolType,
		"name": toolName,
	}
	if maxUses > 0 {
		cfg["max_uses"] = maxUses
	}
	if len(allowed) > 0 {
		cfg["allowed_domains"] = allowed
	}
	if len(blocked) > 0 {
		cfg["blocked_domains"] = blocked
	}
	return provider.NewServerTool(cfg)
}

// resolveMessageTransforms merges per-agent and global message transforms.
// Agent rules override matching global rules (by Find pattern); non-matching global rules fall through.
func resolveMessageTransforms(acfg config.AgentConfig, cfg *config.Config) []config.MessageTransform {
	return config.SuperveneSlice(acfg.MessageTransforms, cfg.MessageTransforms,
		func(t config.MessageTransform) string { return t.Find })
}

// resolveBlockedPaths merges per-agent and global blocked paths.
// Agent paths override matching global paths (by Path); non-matching global paths fall through.
func resolveBlockedPaths(acfg config.AgentConfig, cfg *config.Config) []config.BlockedPath {
	return config.SuperveneSlice(acfg.BlockedPaths, cfg.BlockedPaths,
		func(b config.BlockedPath) string { return b.Path })
}

