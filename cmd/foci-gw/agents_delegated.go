package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"foci/internal/agent"
	"foci/internal/app"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/delegator/autoapprove"
	"foci/internal/delegator/pretool"
	"foci/internal/delegator/stoprule"
	"foci/internal/fociroot"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/relogin"
	"foci/internal/route"
	"foci/internal/secrets"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/turn"
	"foci/internal/turnevent"
	"foci/internal/voice"
	"foci/shared/prompts"
)

// configureDelegated sets up delegated transport agent state: DelegatedManager
// with all callbacks, model override, permissions, and exec registry. The
// agent's shared fields (compaction, warnings, etc.) are already set by
// setupAgent before this is called.
func configureDelegated(ag *agent.Agent, p setupParams, shared *sharedAgentSetup, backendName string, backendConfig config.BackendConfig) (finalizeParams, bool) {
	ag.Backend = backendName
	// The backend's declaration: everything below that differs per backend
	// asks it, never the backend's name (#2154).
	spec, _ := delegator.SpecFor(backendName)
	// Make prompt search dirs available for orientation template resolution
	// (webhooks, keepalive, memory formation). Delegated agents don't need
	// groupResolver since their model comes from backendConfig.
	ag.PromptSearchDirs = shared.promptSearchDirs

	// Bootstrap and skill registry — shared loader with API agents.
	//
	// Fixes two latent gaps in the previous local NewBootstrap call:
	//   1. p.acfg.System.SystemFiles is now honoured. The old call passed
	//      nil for fileOrder, which fell through to DefaultFileOrder and
	//      silently ignored per-agent system_files config.
	//   2. The skill registry is populated, so the Available Skills system
	//      block and the default skill nudge reach delegated agents (was
	//      previously inert — 0 default rules across all delegated agents).
	agentStore := p.store.ForAgent(p.acfg.ID)
	br := setupBootstrapAndSkills(p, agentStore)
	bs := br.bootstrap

	// Model for the backend — from backend_config, not from the group resolver.
	// Ladder: config value, then the backend's own default model
	// (Spec.DefaultModel; "" for opencode, TODO #1163).
	model := config.DerefStr(backendConfig.Model)
	if model == "" {
		model = spec.DefaultModel
	}

	// Work on a copy so global-default folding below doesn't mutate the
	// shared AgentConfig.BackendConfig (struct is a value type — assignment copies).
	bc := foldGlobalBackendConfig(p.cfg, spec, backendConfig)

	// Build a tool registry with exec-exported tools so foci shell commands
	// (foci_todo, foci_send_to_chat, etc.) are available in the backend's
	// shell environment via the persistent exec bridge.
	//
	// Built before buildAutoApproveRules so its ExportedNames can drive the
	// always-on FociShellRules — keeps the auto-approve list in sync with
	// what's actually wired in (no hand-list to drift).
	agLazy := func() *agent.Agent { return ag }
	registry := buildExecRegistry(p, shared.wakeScheduleFn, shared.wakeCancelFn, agLazy)
	setupUserShell(ag, p, registry, spec.BashDefaultTimeout(backendConfig.Env))
	ag.ResultSummariser = newBatchSummariser(p, agLazy)

	// Build auto-approve rules from resolved config. bakedPerms is the same
	// frozen snapshot, reused below so the environment block's Command
	// Approval description matches what's actually baked into the rules.
	bakedPerms := p.resolved.Permissions // static-cfg:ignore: must match the frozen rules buildAutoApproveRules bakes into the CC session (no live-reload path of its own), see writeCommandApproval
	autoApproveRules := buildAutoApproveRules(p, registry.ExportedNames())

	// Per-agent environment block for delegated backends ("" when disabled).
	// Captured below by the SystemPromptFunc closure so it is re-prepended on
	// every per-session rebuild — NOT just baked into the static SystemPrompt.
	// The rebuild wins over the static prompt at each session start (#828/#706),
	// and that rebuild path previously dropped the env block (and with it the
	// "Foci Shell Tools" list), so CC agents never saw foci_todo etc.
	crontabCount := 0
	if p.resolved.Environment.Enabled { // static-cfg:ignore: startup-only gate for an expensive subprocess spawn
		crontabCount = countCrontabJobs()
	}
	buildEnv := func(sessionPlatform string, spawn bool) string {
		rc := p.resolvedLive.Load()
		if !rc.Environment.Enabled {
			return ""
		}
		return buildEnvironmentDelegated(p.acfg, p.configPath, p.cfg, rc, bakedPerms, crontabCount, p.plat.ActivePlatformNames(), registry.ExportedTools(), shared.promptSearchDirs, sessionPlatform, spawn)
	}
	systemPrompt := delegatedSystemPrompt(buildEnv("", false), bs.SystemBlocks(), br.extraSystemBlocks)

	ag.Model = model

	// Wire DelegatedManager: lazy per-session Backend creation.
	connMgr := p.connMgr
	agentID := p.acfg.ID
	// Late-delivery fallback for the session router (#1068 Phase 1): resolve the
	// current delivering connection at Emit time, falling back to the agent's
	// primary when the session has no live connection of its own.
	ag.ResolveLateConn = func(sessionKey string) platform.Connection {
		conn, _ := route.ConnFor(connMgr, agentID, sessionKey, route.PolicyFallback)
		return conn
	}
	// Durable last-resort fallback for an adopted autonomous turn when
	// ResolveLateConn's normal (ownership-respecting) cascade comes up with
	// nothing at all: reach past chat-ownership routing straight to the
	// session's app registration, if it has one. Every send through it persists
	// durably before checking who's live (clutch #1350 follow-up) — see
	// app.DurableConnFor and Agent.DurableTurnSink's doc comments.
	ag.DurableTurnSink = func(sessionKey string) turnevent.Sink {
		conn := app.DurableConnFor(sessionKey)
		if conn == nil {
			return nil
		}
		return turn.NewSessionSink(conn, sessionKey, "autonomous")
	}
	// A run Claude Code starts by itself on a background spawn's branch
	// reports to the spawn's spawner, never to a chat (#2729).
	ag.SpawnBranchTurn = tools.SpawnBranchTurn
	// Resolve a session's messaging platform (telegram/app/discord) for the
	// per-session ## Platform block from the durable chat claim, so the prompt
	// (and its compaction-time fingerprint) never depends on connection
	// liveness. See platformForSession.
	sessionIdx := p.sessionIndex
	platformFor := func(sessionKey string) string {
		return platformForSession(sessionIdx, agentID, sessionKey)
	}
	// Idle timeout (#2280): the agent's backend_config.idle_timeout, else
	// [sessions] idle_timeout, else agent.DefaultIdleTimeout (0 here). Config
	// load has already refused a value that does not parse.
	idleTimeout := resolveIdleTimeout(p.cfg, bc)

	// Automated re-login trigger (#843). Built once here so both the 401
	// auth-failure callback and the manual /login command invoke the same
	// path. The gate single-flights across all agents (shared OAuth
	// credential), so only the first caller launches the driver; a second
	// caller (concurrent 401, or /login while one is running) gets false.
	// Only for a backend declaring CapRelogin (Claude Code): the driver drives
	// a `claude /login` TUI in tmux. The /login command is gated by
	// RequiresBackend; this field stays nil for other backends so that
	// command reports "unavailable" rather than mis-driving the wrong backend.
	claudeBin := config.DerefStr(bc.Binary)
	workDir := p.acfg.Workspace
	// auto marks an automatic trigger (a 401, the startup probe): it gets one
	// attempt per expiry and starts nothing while a login is pending (#3006);
	// /login always starts one.
	startRelogin := func(reason, sessionKey string, auto bool) bool {
		start := relogin.G.Start
		if auto {
			start = relogin.G.StartAuto
		}
		if !start() {
			if auto && !relogin.G.Active() {
				// INFO, not WARN: a warning here would become a proactive
				// injection, which is what this rule exists to stop.
				log.NewComponentLogger("relogin/"+agentID).Infof(
					"CC auth failure while a login is pending: no new attempt (send /login): %s", reason)
			}
			return false
		}
		// Logged under relogin/<agent> with the driver's lines, so the held
		// warning is dropped with them once the login succeeds (#2384).
		log.NewComponentLogger("relogin/"+agentID).Warnf("CC re-login starting: %s", reason)
		go relogin.Run(context.Background(), relogin.Config{
			AgentID:   agentID,
			WorkDir:   workDir,
			ClaudeBin: claudeBin,
			// The login pane's claude runs under a tmux server that may
			// predate this process, so it is told CC's config folder (and
			// the user's HOME) explicitly: a login written to the wrong
			// folder would leave the agents logged out (#2580).
			Env:  fociroot.CCEnv(),
			Gate: relogin.G,
			Link: reloginLinkTarget(connMgr, agentID, sessionKey),
			SendMessage: func(text string) error {
				// Resolve via the triggering session key so the login URL reaches
				// the chat that asked for it. An empty key falls back to the
				// agent's primary bot (which owns the persisted default chat) —
				// NOT an arbitrary idle facet, now that BotForSession("") returns
				// nil. SendToSession reads the chat ID straight from the key, so
				// delivery works whichever bot resolves, even agent-less facets
				// (the #843 "no chat ID — no default chat configured" bug).
				// Injected, not SendToSession: these are foci's notices, not
				// the agent's words, so the app shows them with the system
				// role (#3003).
				conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
				if conn == nil {
					return fmt.Errorf("no connection for agent %s", agentID)
				}
				return conn.SendInjectedMessage(sessionKey, text)
			},
		})
		return true
	}
	if spec.Supports(delegator.CapRelogin) {
		ag.ReloginTrigger = func(reason, sessionKey string) bool {
			return startRelogin(reason, sessionKey, false)
		}
		ag.ReloginLinkTarget = func(sessionKey string) *relogin.LinkTarget {
			return reloginLinkTarget(connMgr, agentID, sessionKey)
		}
	}

	// PreToolUse rules (#2028): built-in defaults, then [cc_backend], then this
	// agent's backend_config, merged by name. Re-resolved from the config file
	// at every CC launch (#2033), so an edit reaches the next session.
	var preToolRules func() []pretool.Rule
	// Stop rules (#2089): this agent's backend_config only, re-read the same way.
	var stopRules func() []stoprule.Rule
	if spec.Supports(delegator.CapPreToolRules) {
		preToolRules = livePreToolRules(agentID, p.configPath,
			resolvePreToolRules(agentID, p.cfg.CCBackend.PreToolRules, backendConfig.PreToolRules))
	}
	if spec.Supports(delegator.CapStopRules) {
		stopRules = liveStopRules(agentID, p.configPath, resolveStopRules(agentID, backendConfig.StopRules))
	}
	// backend_config.claude_ai_skills (#2953): whether CC lists the claude.ai
	// account's skills, read at every launch like the rules above.
	claudeAISkills := liveClaudeAISkills(agentID, p.configPath, config.DerefBool(backendConfig.ClaudeAISkills))

	// The gateway's callbacks into the backend, built once per agent with no
	// backend-specific branching (#2154 Phase 3, HostHooks). A backend takes
	// them iff it declares host_hooks and uses the fields its other
	// capabilities say it honours. Codex config/runtime warnings deliberately
	// have NO hook: they are operator diagnostics emitted before any thread
	// exists, delivered by the generic log.SetWarnHook path
	// (setupWarningHooks); a backend-specific hook alongside it only produced
	// duplicates.
	hostHooks := delegator.HostHooks{
		// A backend with the Claude Code re-login runs it on a 401 (#843) via
		// the shared trigger built above (same path as the manual /login
		// command); auto-401 has no triggering chat, "" → the default chat.
		// Elsewhere (opencode: per-provider auth) the failure is logged.
		OnAuthFailure: func(detail string) {
			log.NewComponentLogger("agent:"+agentID).Warnf("%s auth failure: %s", spec.DisplayName, detail)
		},
		// Surface a utilisation notice (CC's structured rate_limit_event)
		// directly to the session's chat, i.e. to the human, formatted for a
		// person — NOT as a log warning. Deliberately off the generic
		// log.Warnf→WarningQueue→PROACTIVE-WARNINGS path so it reaches the user
		// instead of being injected into the agent's own context. It reflects
		// API utilization past a threshold, not a block, so it does NOT gate
		// periodic work (#1211/#1238). Delivery target is [notify]
		// rate_limit_notify_to, read LIVE per notice (hot:"event") so an edit
		// takes effect without a restart — the backend outlives the config
		// (#1857).
		OnRateLimitNotice: func(sessionKey, notice string) {
			deliverRateLimitNotice(connMgr, agentID, sessionKey, notice,
				p.resolvedLive.Load().Notify.RateLimitNotifyTo)
		},
		// A model refusal fallback (CC's primary model refused and another
		// answered) is shown in the chat of the session it happened in, so the
		// user knows which model is now answering (Dick, 2026-10-06, #2200).
		OnModelFallbackNotice: func(sessionKey, notice string) {
			deliverModelFallbackNotice(connMgr, agentID, sessionKey, notice)
		},
		// A hard limit (CC session limit, opencode rejected usage) engages the
		// rate-limit gate so background/periodic work pauses until the window
		// resets; the gate's own hooks notify the user.
		EngageRateLimit: ag.EngageRateLimit,
		OnUsageWindow:   ag.ObserveUsage,
		PreToolRules:    preToolRules,
		StopRules:       stopRules,
		ClaudeAISkills:  claudeAISkills,
	}
	if spec.Supports(delegator.CapRelogin) {
		hostHooks.OnAuthFailure = func(detail string) {
			startRelogin("401 auth failure: "+firstLine(detail), "", true)
		}
		hostHooks.OnLoginExpiry = loginExpiryHook(loginExpiryWarner(p.cfg), connMgr, p.cfg.MasterAgent, agentID)
	}

	// The manager's two prompt surfaces (#2275): approvals land in the
	// owner's default chat, questions in the asking session's chat.
	permPromptFn, questionPromptFn := backendPromptFuncs(backendPromptDeps{
		ag: ag, connMgr: connMgr, sessionIdx: sessionIdx, agentID: agentID,
		live:       depthSessionLive,
		owner:      tools.SpawnBranchOwner,
		chatClosed: spawnerChatClosed(agentID),
		defaultSession: func() string {
			if sessionIdx == nil {
				return ""
			}
			return sessionIdx.DefaultSessionKeyForAgentOn(agentID, p.cfg.DefaultPlatformFor(agentID))
		},
	})

	ag.DelegatedManager = &agent.DelegatedManager{
		SessionIndex: p.sessionIndex,
		AgentID:      agentID,
		BackendType:  backendName,
		Spec:         spec,
		BatchModel:   config.DerefStr(bc.BatchModel),
		// A depth leaf's backend refuses every subagent path but foci_spawn
		// (#2625). Asked at each backend Start, so a resumed leaf keeps it.
		DepthLeaf: tools.InDepthLeaf,
		NewBackend: func() (delegator.Delegator, error) {
			cfgMap := bc.ToMap()
			// Sandbox grants are backend-neutral and frozen with the backend instance.
			cfgMap[delegator.WritableRootsConfigKey] = p.resolved.Sandbox.WritableRoots // static-cfg:ignore: sandbox grants are restart-required like backend sandbox/reviewer; pooled app-server launch and thread RPCs must agree
			cfgMap[delegator.FociVersionConfigKey] = version
			be, err := delegator.New(backendName, cfgMap)
			if err != nil {
				return nil, err
			}
			if hh, ok := delegator.As[delegator.HostHooksAcceptor](be); ok {
				hh.SetHostHooks(hostHooks)
			}
			return be, nil
		},
		StartOpts: delegator.StartOptions{
			WorkDir:      p.acfg.Workspace,
			SystemPrompt: systemPrompt,
			// Rebuild the prompt from disk at every session-start so a fresh
			// session (reset, idle-respawn, emulated compaction #828) picks up
			// character-file AND skill edits — instead of the prompt frozen at
			// setup (the #706 / #828 bug). bs.SystemBlocks() reflects in-place
			// Bootstrap.Reload(); ReloadSystemFn re-loads skills from disk
			// (side-effect-free — it does NOT mutate ag.ExtraSystemBlocks).
			// Falls back to the setup-time snapshot (systemPrompt) if it yields
			// empty. ag is a pointer; ReloadSystemFn is wired later in finalize,
			// but this closure only runs at runtime session-start, by when it's set.
			// The env block is re-prepended via newDelegatedSystemPromptFunc so it
			// survives this rebuild (the #828/#706 rebuild used to drop it).
			// A spawn's session (a direct spawn's batch, a background
			// spawn's branch) gets a spawn's reply-routing line (#2986).
			// ag.DelegatedManager is set by the time a session starts.
			SystemPromptFunc: newDelegatedSystemPromptFunc(buildEnv, platformFor, func(sessionKey string) bool {
				return ag.DelegatedManager.IsSpawnSession(sessionKey)
			}, func() (ws, extra []provider.SystemBlock) {
				// Re-read character files from disk so this fresh session reflects
				// edits since setup, independent of whether the caller reloaded
				// (the compaction-bounce path #828 does not). bs is shared
				// per-agent; Reload mutates it in place under its own lock.
				// ReloadSystemFn re-loads skills from disk too.
				bs.Reload()
				extra = br.extraSystemBlocks
				if ag.ReloadSystemFn != nil {
					if fresh, _ := ag.ReloadSystemFn(); fresh != nil {
						extra = fresh
					}
				}
				return bs.SystemBlocks(), extra
			}, dailyDigestFunc(p.acfg.Workspace, func() bool { return p.resolvedLive.Load().MemorySearch.DailyDigest })),
			Model: model,
			// Re-inject the session's effort at every launch so a bounce
			// (post-compaction reload, idle respawn) keeps the level the user
			// set via /effort — apply_flag_settings is runtime-only. Returns
			// "" when no override is set (CC uses the model default). (#840)
			EffortFunc: func(sk string) string { return ag.SessionEffort(sk) },
			ModelFunc:  func(sk string) string { return ag.SessionModel(sk) },
			// Drive opencode's internal compaction (/summarize) with foci's
			// compaction-summary.md — the SAME resolution the CC backend uses
			// (internal/agent/compaction.go) — instead of opencode's built-in
			// template, via the blank-system plugin's session.compacting hook.
			// Resolved fresh per Start (mirrors SystemPromptFunc); "" leaves
			// opencode's default compaction prompt untouched.
			CompactionPromptFunc: func(string) string {
				return prompts.ResolvePrompt("", "compaction-summary.md", prompts.CompactionSummary(), ag.PromptSearchDirs...)
			},
			AgentID:          agentID,
			ExecRegistry:     registry,
			AutoApproveRules: autoApproveRules,
			// Prune threshold for tracked background tasks (subagents,
			// run_in_background Bash). Empty/invalid → 0, and the tracker falls
			// back to its 2h default. Unwedge backstop for the pending-work gate.
			SubagentMaxAge: func() time.Duration {
				d, _ := time.ParseDuration(p.cfg.CCBackend.BackgroundTaskMaxAge)
				return d
			}(),
			// Per-agent backend_config.env propagates to the backend
			// subprocess. Used by integration tests to inject CCSTUB_*
			// env vars per agent (e.g. one agent gets CCSTUB_HANG, others
			// don't). DelegatedManager.Get merges these with the exec
			// bridge's BASH_ENV/FOCI_SOCK so both layers survive.
			Env: bc.Env,
		},
		PermissionPromptFunc: permPromptFn,
		// Questions the agent asks its own user go to the asking session's
		// chat (#2275); only wired on backends implementing
		// delegator.QuestionPromptSetter (see setBackendCallbacks).
		QuestionPromptFunc: questionPromptFn,
		TypingFunc: func(sessionKey string, typing bool, stage *agent.TypingStage) {
			// Each step is recorded so a call overrunning the manager's 2s
			// bound is logged with what stalled (#2320): the lookup consults
			// the session index (chat → platform) and the platform managers.
			stage.Set("connection lookup (chat-platform index, platform managers)")
			conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
			if conn == nil {
				return
			}
			stage.Set(conn.PlatformName() + " connection SetTyping")
			conn.SetTyping(typing)
		},
		SubagentStatusFunc: func(sessionKey, detail string) {
			// Subagent (CC Agent-tool) status has an app-native surface: the
			// conversation's unified Activity indicator (subagents kind). Route
			// straight to the app hub's binding; a no-op for non-app sessions.
			app.SetSubagentDetail(sessionKey, detail)
		},
		SubagentRunningFunc: func(sessionKey string, running []delegator.RunningSubagent) {
			// The structured list behind the detail (#2138): each subagent's
			// id, model and start time, for the app's stop dialog.
			app.SetSubagents(sessionKey, running)
		},
		SystemNoticeFunc: func(sessionKey, text string) {
			conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
			if conn == nil {
				log.NewComponentLogger("agent:"+agentID).Warnf("system notice: no connection for session=%s, dropped: %s", sessionKey, text)
				return
			}
			if err := conn.SendText(text); err != nil {
				log.NewComponentLogger("agent:"+agentID).Warnf("system notice send failed for session=%s: %v", sessionKey, err)
			}
		},
		// Adopt CC-initiated runs as first-class foci turns — streaming, in-flight
		// tracking, accounting, and meta all flow through the normal turn path
		// (#1261).
		OpenAutonomousTurn: ag.OpenAutonomousTurn,
		AttachDelivery:     ag.AttachDelivery,
		// Batch runs (consolidation, nudge extraction, foci_summary) are
		// ordinary turns on an ephemeral child session (#1962).
		RunBatchTurn: ag.RunBatchTurn,
		// #2684: a spawn starts in its spawner's permission mode.
		SpawnerPermissionMode: ag.SessionPermissionMode,
		IdleTimeout:           idleTimeout,
		// Live-session caps (#2280): the shared gateway limiter and this
		// agent's own cap; a backend is evictable only when neither a turn nor
		// a compaction is running on its session.
		LiveSessions:    p.liveSessions,
		MaxLiveSessions: config.DerefInt(p.acfg.Sessions.MaxLiveSessions), // static-cfg:ignore: the cap is fixed at start-up (restart required)
		SessionBusy: func(sessionKey string) bool {
			return ag.IsTurnInFlight(sessionKey) || ag.IsCompacting(sessionKey)
		},
		// Resume-missed wording reads Spec.ResumeRetention (only claude-code
		// publishes one).
		LastUseFunc: ag.PrevRequestTime,
		// Delivery tracking (#2050): an input the backend never consumed is
		// re-sent as a fresh turn in its session's chat; a consumed input the
		// app sent becomes ✓✓ on the user's bubble.
		Redeliver: func(sessionKey string, r agent.Redelivery) {
			deliverToSessionChat(ag, agent.WithRedelivery(p.ctx, r), agent.TriggerRedelivery, connMgr, agentID, sessionKey, r.Input.Text, nil, "")
		},
		InputConsumed: func(_ string, refs []delegator.InputRef) {
			for _, ref := range refs {
				app.MarkMessageConsumed(ref.ConversationID, ref.MessageID)
			}
		},
	}
	// The account-wide usage hold (#2491): this agent follows its backend's
	// account from now on.
	wireUsageHeldRestore(ag, p.ctx, connMgr, agentID, p.agentResolverFn, app.DeliverAgentMessage)
	ag.StartUsageHold()

	return finalizeParams{
		bootstrap:     bs,
		skillRegistry: br.skillRegistry,
		skillsDirs:    br.skillsDirs,
	}, true
}

// resolveIdleTimeout returns an agent's delegated idle timeout (#2280): its
// backend_config.idle_timeout, else [sessions] idle_timeout, else 0 (the
// manager then uses agent.DefaultIdleTimeout). Config load refuses a value at
// either level that does not parse or is not positive, so a parse failure here
// only means "unset".
func resolveIdleTimeout(cfg *config.Config, bc config.BackendConfig) time.Duration {
	for _, v := range []string{config.DerefStr(bc.IdleTimeout), cfg.Sessions.IdleTimeout} {
		if v == "" {
			continue
		}
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 0
}

// foldGlobalBackendConfig folds the global section of the backend's config
// family (Spec.ConfigFamily) into a copy of the per-agent backend_config —
// per-agent values always win. Other families' keys are skipped so they
// don't leak into this backend's config surface.
//
// [cc_backend] folded keys:
//
//	allowed_tools — merged (per-agent rules appended to global)
//	binary — global default; per-agent override wins
//	batch_model — global default (haiku); per-agent override wins
//
// [opencode_backend] folded keys: binary, hostname, server_auth, log_level,
// port, default_permission, batch_model (when set). default_permission folds only when SET: the
// field deliberately carries no tag default, so an unset global leaves the
// agent with no default_permission and Start sends no permission request
// (#2330) — opencode's own defaults stand.
func foldGlobalBackendConfig(cfg *config.Config, spec delegator.Spec, bc config.BackendConfig) config.BackendConfig {
	switch spec.ConfigFamily {
	case delegator.ConfigFamilyClaudeCode:
		merged := cfg.CCBackend.MergedAllowedTools(bc.AllowedTools)
		if merged != "" {
			bc.AllowedTools = strings.Split(merged, ",")
		}
		if bc.Binary == nil && cfg.CCBackend.Binary != "" {
			bc.Binary = &cfg.CCBackend.Binary
		}
		if bc.BatchModel == nil && cfg.CCBackend.BatchModel != "" {
			bc.BatchModel = &cfg.CCBackend.BatchModel
		}
	case delegator.ConfigFamilyOpencode:
		if bc.Binary == nil {
			bc.Binary = &cfg.OpencodeBackend.Binary
		}
		if bc.Hostname == nil {
			bc.Hostname = &cfg.OpencodeBackend.Hostname
		}
		if bc.ServerAuth == nil {
			bc.ServerAuth = &cfg.OpencodeBackend.ServerAuth
		}
		if bc.LogLevel == nil {
			bc.LogLevel = &cfg.OpencodeBackend.LogLevel
		}
		if bc.Port == nil {
			bc.Port = &cfg.OpencodeBackend.Port
		}
		if bc.DefaultPermission == nil && cfg.OpencodeBackend.DefaultPermission != "" {
			bc.DefaultPermission = &cfg.OpencodeBackend.DefaultPermission
		}
		if bc.BatchModel == nil && cfg.OpencodeBackend.BatchModel != "" {
			bc.BatchModel = &cfg.OpencodeBackend.BatchModel
		}
	}
	return bc
}

// backendPromptKind selects which chat a backend prompt is shown in (#2275):
// approval prompts go to the owner's default chat (Dick's 2026-10-09 ruling:
// he approves other users' requests), questions the agent asks its own user
// go to the asking session's chat.
type backendPromptKind int

const (
	// promptToDefaultChat: approval prompts (tool permissions, ExitPlanMode).
	promptToDefaultChat backendPromptKind = iota
	// promptToSessionChat: the agent asking its own user (AskUserQuestion,
	// elicitation, the question tool).
	promptToSessionChat
)

// backendPromptDeps are the agent-scoped inputs of postBackendPrompt, explicit
// so the prompt tests can drive the shared body directly.
type backendPromptDeps struct {
	ag         *agent.Agent
	connMgr    platform.ConnectionManager
	sessionIdx *session.SessionIndex
	agentID    string
	// defaultSession returns the agent's default session key, the last chat
	// a prompt is offered to (#2608). nil or "" skips that step.
	defaultSession func() string
	// failPrompt answers a prompt no chat could show (#2608). nil means
	// ag.FailUndeliverablePrompt; tests replace it to observe the failure.
	failPrompt func(sessionKey, requestID, reason string) error
	// live reports whether an ancestor's chat can still reach the user
	// (#2627): a done depth leaf's cannot, so promptTargets skips it. nil =
	// every session is live.
	live func(sessionKey string) bool
	// owner resolves the current owner of the spawn running on a branch
	// (#2632; tools.SpawnBranchOwner in the gateway). A spawn's prompts go
	// to the owner chain, as its foci_ask does (newAskChats). nil = no
	// spawn routing.
	owner func(branchKey string) (string, bool)
	// chatClosed reports that a session's app chat is closed (#2684): a
	// spawn whose owner's chat is closed has nobody to ask, so its prompt is
	// decided as an unattended ask is (the rules already declined it: deny).
	// nil = never closed.
	chatClosed func(sessionKey string) bool

	// respond delivers a permission decision for a prompt. Production leaves
	// it nil (postBackendPrompt then uses ag.SendPermissionResponse); tests
	// substitute a recorder so a fail-closed denial is observable without a
	// live backend.
	respond func(ctx context.Context, sessionKey, requestID, choice string) error
}

// backendPromptTargets is the chat list of a prompt from sessionKey: for a
// spawn's branch, the same as its foci_ask (#2632, Dick 2026-10-28 "Follow
// the owner": the spawn's current owner, then up the chain from there;
// spawned=true); for any other session, promptTargets.
func backendPromptTargets(d backendPromptDeps, sessionKey string) (targets []string, spawned bool) {
	if d.owner != nil {
		if ts, spawned := newAskChats(d.owner, d)(sessionKey); spawned {
			return ts, true
		}
	}
	return promptTargets(d, sessionKey), false
}

// appMediaChats is the walk an app media send takes when its session has no
// conversation (#2755, Dick 2026-11-04: "send up"): the same chats, in the same
// order, as a backend prompt from that session. The last step is the default
// session on the app, because only an app conversation can take the file. Each
// chat carries the origin note the file takes if it lands there (#2844).
func appMediaChats(idx *session.SessionIndex) func(agentID, sessionKey string) []app.SessionChat {
	return func(agentID, sessionKey string) []app.SessionChat {
		return mediaChats(backendPromptDeps{
			sessionIdx: idx,
			agentID:    agentID,
			live:       depthSessionLive,
			owner:      tools.SpawnBranchOwner,
			defaultSession: func() string {
				if idx == nil {
					return ""
				}
				return idx.DefaultSessionKeyForAgentOn(agentID, "app")
			},
		}, sessionKey)
	}
}

func mediaChats(d backendPromptDeps, sessionKey string) []app.SessionChat {
	targets, spawned := backendPromptTargets(d, sessionKey)
	chats := make([]app.SessionChat, len(targets))
	for i, target := range targets {
		chats[i] = app.SessionChat{SessionKey: target, Note: originNote(tools.Spawns, sessionKey, target, spawned, "file")}
	}
	return chats
}

// depthSessionLive is backendPromptDeps.live in the gateway: a session of an
// agent with depth mode is live unless it is a done depth leaf
// (DepthManager.SessionLive); every other session is live.
func depthSessionLive(sessionKey string) bool {
	m := tools.DepthManagerFor(sessionKey)
	return m == nil || m.SessionLive(sessionKey)
}

// sendPermissionResponse is deps.respond with the nil fallback applied.
func (d backendPromptDeps) sendPermissionResponse(ctx context.Context, sessionKey, requestID, choice string) error {
	if d.respond != nil {
		return d.respond(ctx, sessionKey, requestID, choice)
	}
	return d.ag.SendPermissionResponse(ctx, sessionKey, requestID, choice)
}

// refuseApproval fail-closes an approval prompt on a connection whose platform
// cannot show one to a human who may approve it (platform.ApprovalRefuser —
// Slack until #2459 part C routes approvals to an operator): the request is
// denied at once, the denial logged, and the requesting conversation gets a
// short notice. It returns true when the prompt was refused, so the caller
// returns before anything is sent or registered — nothing is left pending.
func refuseApproval(logger *log.ComponentLogger, conn platform.Connection, sessionKey, requestID, what string) bool {
	if !platform.RefusesApprovals(conn) {
		return false
	}
	if what == "" {
		// A prompt can carry no summary; the notice still needs a subject.
		what = "a tool use"
	}
	logger.Warnf("approval for session=%s reqID=%s denied: the %s platform cannot show approval prompts yet — auto-denied (#2459 C)", sessionKey, requestID, conn.PlatformName())
	conn.SendNotification(fmt.Sprintf("🔒 %s needs an approval foci cannot show here yet — it was denied. Ask the operator to run it.", what))
	return true
}

// postBackendPrompt is the one body behind the two gateway prompt closures
// (DelegatedManager.PermissionPromptFunc and QuestionPromptFunc — wired in
// configureDelegated as one-line delegations). It offers the prompt to each
// chat of backendPromptTargets in turn (which connection a chat resolves to depends
// on kind) until one shows it: it sends the optional attachment to that chat,
// posts the interactive prompt with the shared click and expiry callbacks, and
// registers the per-prompt cancel listener. Everything except the destination
// chat is identical for both kinds.
//
// A prompt no chat can show is answered as a failure with the reason (#2608,
// Dick 2026-10-28): before, the send error was only logged, and since a failed
// send leaves no prompt entry to expire, the backend waited forever.
func postBackendPrompt(d backendPromptDeps, kind backendPromptKind, sessionKey, requestID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
	logger := log.NewComponentLogger("agent:" + d.agentID)
	var buttons []platform.ButtonChoice
	for _, c := range choices {
		btn := platform.ButtonChoice{Label: c.Label, Data: c.Data}
		if c.Toggle != nil {
			btn.Toggle = &platform.ButtonToggle{
				ExtraBody: c.Toggle.ExtraBody,
				ShowLabel: c.Toggle.ShowLabel,
				HideLabel: c.Toggle.HideLabel,
			}
		}
		buttons = append(buttons, btn)
	}
	onClick := func(choice platform.ButtonChoice) string {
		logger.Debugf("prompt button pressed: sk=%s reqID=%s choice=%q", sessionKey, requestID, choice.Data)
		if err := d.sendPermissionResponse(context.Background(), sessionKey, requestID, choice.Data); err != nil {
			logger.Errorf("SendPermissionResponse failed: sk=%s reqID=%s choice=%q err=%v", sessionKey, requestID, choice.Data, err)
		}
		switch {
		case choice.Data == "deny" || choice.Data == "qa:cancel":
			if summary != "" {
				return "❌ " + summary
			}
			return "❌ Cancelled"
		case strings.HasPrefix(choice.Data, "qa:"):
			return "✅ " + choice.Label
		default:
			if summary != "" {
				return "✅ " + summary
			}
			return "✅ Approved"
		}
	}
	onExpire := func() {
		// Expiry: deny the prompt so a turn blocked in WaitForPermission
		// unblocks instead of orphaning. The message edit to an "expired"
		// notice is handled by CleanupExpiredInteractive.
		if err := d.sendPermissionResponse(context.Background(), sessionKey, requestID, "deny"); err != nil {
			logger.Warnf("expire prompt deny failed: sk=%s reqID=%s err=%v", sessionKey, requestID, err)
		}
	}

	targets, spawned := backendPromptTargets(d, sessionKey)
	if spawned && len(targets) > 0 && d.chatClosed != nil && d.chatClosed(targets[0]) {
		refuseClosedSpawnerPrompt(d, logger, kind, sessionKey, requestID, summary, targets[0])
		return
	}
	var failures []string
	delivered := false
	for _, target := range targets {
		// A cross-agent spawn's request is shown in its caller's chat
		// (#2730): that chat, and its primary-bot fallback, are the caller
		// agent's, not this one's.
		chatAgent := chatAgentID(target, d.agentID)
		resolve := connResolver(d.connMgr, target, chatAgent)
		if kind == promptToSessionChat {
			resolve = sessionChatResolver(d.connMgr, target, chatAgent)
		}
		conn := resolve()
		if conn == nil {
			failures = append(failures, target+": no connection")
			continue
		}
		if kind == promptToDefaultChat && refuseApproval(logger, conn, sessionKey, requestID, summary) {
			// Approval prompts fail closed on a platform that cannot show
			// them (Slack, #2459 A1): deny, log, notify the requester, send
			// nothing, register nothing, and try no further chat. Questions
			// (promptToSessionChat) are not approvals and post normally.
			if err := d.sendPermissionResponse(context.Background(), sessionKey, requestID, "deny"); err != nil {
				logger.Errorf("approval refusal: deny failed: sk=%s reqID=%s err=%v", sessionKey, requestID, err)
			}
			return
		}
		logger.Debugf("backend prompt: sending via %s to %s for session=%s summary=%q reqID=%s", conn.PlatformName(), target, sessionKey, summary, requestID)
		body := text
		if kind == promptToDefaultChat {
			body = prependRequestOrigin(d.sessionIdx, chatAgent, conn, sessionKey, body)
		}
		body = delegator.PrependOrigin(originNote(tools.Spawns, sessionKey, target, spawned, "prompt"), body)
		// Attachment (e.g. the full ExitPlanMode plan markdown) is sent as a
		// document before the keyboard so the user sees the content, then the
		// Allow/Deny buttons — to the chat the buttons go to. A send failure is
		// non-fatal — fall through to the prompt so the permission gate still
		// resolves.
		if attachmentPath != "" {
			if err := sendPromptAttachment(conn, kind, target, attachmentPath); err != nil {
				logger.Warnf("backend prompt: SendDocument(%q) failed for session=%s: %v", attachmentPath, target, err)
			}
		}
		// Use the requestID as the platform prompt ID so the cancel listener
		// (registered below) can find and edit this exact message later if the
		// backend cancels the request before the user responds. The resolver
		// (not the conn grabbed above) is stored for those later edits so they
		// survive a reconnect — and, for a question, so the cancel/expiry edits
		// address the chat that shows it. A failed send leaves no prompt entry,
		// so the same requestID is free for the next target.
		if _, err := platform.SendInteractiveMessageWithID(resolve, requestID, summary, body, buttons, onClick, onExpire); err != nil {
			logger.Warnf("backend prompt send failed: sk=%s target=%s reqID=%s err=%v", sessionKey, target, requestID, err)
			failures = append(failures, fmt.Sprintf("%s: %v", target, err))
			continue
		}
		switch {
		case spawned:
			logger.Infof("backend prompt: sk=%s reqID=%s shown in %s (the spawn's owner chain)", sessionKey, requestID, target)
		case target != sessionKey:
			logger.Infof("backend prompt: sk=%s reqID=%s shown in %s instead (own chat could not show it)", sessionKey, requestID, target)
		}
		delivered = true
		break
	}
	if !delivered {
		reason := fmt.Sprintf("foci could not show this prompt to the user in any chat (%s), so it was refused automatically. It was not seen or answered by a person.", strings.Join(failures, "; "))
		fail := d.failPrompt
		if fail == nil {
			fail = func(sk, reqID, why string) error {
				return d.ag.FailUndeliverablePrompt(context.Background(), sk, reqID, why)
			}
		}
		if err := fail(sessionKey, requestID, reason); err != nil {
			logger.Errorf("backend prompt undeliverable and its refusal failed: sk=%s reqID=%s err=%v (%s)", sessionKey, requestID, err, reason)
		} else {
			logger.Warnf("backend prompt undeliverable, refused: sk=%s reqID=%s (%s)", sessionKey, requestID, reason)
		}
		return
	}
	// Register a cancel listener so the orphaned inline keyboard is
	// disabled if the backend aborts this prompt before the user responds
	// (typically because a follow-up message interrupted the in-flight
	// tool). This is a per-prompt registration owned by the same function
	// that created the UI.
	d.ag.DelegatedManager.RegisterPromptCancelListener(sessionKey, requestID, func(reason string) {
		finalText := "❌ tool request cancelled by follow-up message"
		if summary != "" {
			finalText = fmt.Sprintf("❌ %s cancelled by follow-up message", summary)
		}
		if reason == agent.PromptEndedReason {
			finalText = "❌ No longer needed: the session that asked has ended"
			if summary != "" {
				finalText = fmt.Sprintf("❌ %s: no longer needed, the session that asked has ended", summary)
			}
		}
		if err := platform.CancelInteractiveMessage(requestID, finalText); err != nil {
			logger.Warnf("cancel interactive message: sk=%s reqID=%s err=%v", sessionKey, requestID, err)
		} else {
			logger.Debugf("prompt cancelled: sk=%s reqID=%s reason=%q", sessionKey, requestID, reason)
		}
	})
}

// spawnerChatClosedLabel labels the log line of a spawn's ask refused because
// its spawner's chat is closed, in the "unattended: session …" shape.
const spawnerChatClosedLabel = "spawner_chat_closed"

// refuseClosedSpawnerPrompt answers a spawn's prompt whose spawner's chat is
// closed (#2684 requirement 5): nobody is there to answer it, so it is
// decided as an unattended ask is. The backend already found no auto-approve
// rule that permits it, so a permission is denied and a question declined,
// with the unattended messages and log lines.
func refuseClosedSpawnerPrompt(d backendPromptDeps, logger *log.ComponentLogger, kind backendPromptKind, sessionKey, requestID, summary, owner string) {
	reason := delegator.UnattendedDenyMessage
	line := delegator.UnattendedDeniedLog(sessionKey, spawnerChatClosedLabel, "use "+summary+" (its spawner's chat "+owner+" is closed)", "")
	if kind == promptToSessionChat {
		reason = delegator.UnattendedQuestionMessage
		line = delegator.UnattendedQuestionLog(sessionKey, spawnerChatClosedLabel, summary+" (its spawner's chat "+owner+" is closed)")
	}
	fail := d.failPrompt
	if fail == nil {
		fail = func(sk, reqID, why string) error {
			return d.ag.FailUndeliverablePrompt(context.Background(), sk, reqID, why)
		}
	}
	if err := fail(sessionKey, requestID, reason); err != nil {
		logger.Errorf("%s req_id=%s; the refusal failed: %v", line, requestID, err)
		return
	}
	logger.Warnf("%s req_id=%s", line, requestID)
}

// promptTargets lists, in order, the sessions whose chats a backend prompt
// from sessionKey is offered to (#2608, Dick 2026-10-28): the session's own
// chat; for a branch (fork, spawn, facet) each of its ancestors in turn, up
// the recorded parent chain to the root session (#2627: a spawn of a nested
// depth leaf tries every leaf above it, not only its own); then the agent's
// default session. An ancestor that is not live (d.live: a done depth leaf,
// whose conversation the user has left) is skipped; the session's own chat
// and the root are always tried. Duplicates are dropped. A branch has no app
// conversation of its own unless it is a depth leaf, so on the app its own
// chat cannot show a prompt, and before #2608 the prompt was lost.
//
// A cross-agent spawn's session (#2662) is offered to no chat of this agent:
// not its root, not its default session. Its only target is itself, which no
// connection resolves, so a prompt from it that no owner took is answered as
// undeliverable instead of reaching this agent's chats. Its owner is the
// caller's session (#2730), a session of another agent: the walk from there
// stays in the caller's chain and does not end on this agent's default
// session either.
func promptTargets(d backendPromptDeps, sessionKey string) []string {
	targets := []string{sessionKey}
	if session.IsCrossAgentSpawnKey(sessionKey) {
		return targets
	}
	otherAgent := d.agentID != "" && chatAgentID(sessionKey, d.agentID) != d.agentID
	add := func(k string) {
		if k == "" {
			return
		}
		for _, t := range targets {
			if t == k {
				return
			}
		}
		targets = append(targets, k)
	}
	if sk, err := session.ParseSessionKey(sessionKey); err == nil && !sk.IsRoot() {
		root := sk.Root().String()
		if d.sessionIdx != nil {
			seen := map[string]bool{sessionKey: true}
			for cur := sessionKey; ; {
				e, err := d.sessionIdx.Get(cur)
				if err != nil || e.ParentSessionKey == "" || seen[e.ParentSessionKey] || e.ParentSessionKey == root {
					break
				}
				cur = e.ParentSessionKey
				seen[cur] = true
				if d.live == nil || d.live(cur) {
					add(cur)
				}
			}
		}
		add(root)
	}
	if d.defaultSession != nil && !otherAgent {
		add(d.defaultSession())
	}
	return targets
}

// chatAgentID is the agent whose chat sessionKey is: the agent its key names,
// else fallback. It differs from the prompting agent only for a cross-agent
// spawn's request shown in its caller's chat (#2730).
func chatAgentID(sessionKey, fallback string) string {
	if a := session.AgentIDFromKey(sessionKey); a != "" {
		return a
	}
	return fallback
}

// originNote is the one decision of what something from the session source says
// about where it came from when it is shown in target's chat (#2844, Dick
// 2026-11-04: "at least do not build a new one"). Backend prompts, foci_ask and
// app media moved up the family tree all take their note from here. what names
// the thing in the note's reason ("prompt", "question", "file").
//
// A spawn is named wherever it is shown (spawnOrigin). A session shown in a chat
// that is not its own says so (delegator.RedirectedOrigin), unless the target is
// on its spawn's owner chain (spawned): the spawn line names it, and it was never
// meant for the branch's own chat. A send with no source session names nothing.
func originNote(tr *tools.SpawnTracker, source, target string, spawned bool, what string) string {
	note := spawnOrigin(tr, source)
	if source == "" || target == source || spawned {
		return note
	}
	return delegator.PrependOrigin(delegator.RedirectedOrigin("session "+source, "its own chat cannot show this "+what), note)
}

// spawnOrigin names the spawn a prompt comes from (#2523). A clone
// spawn runs on its own branch session with its own backend, so its prompts
// never block the parent's turn, but they reach the same chat and otherwise
// read exactly like the parent's own: the owner must see which spawn asks.
// A direct spawn's batch session is named too (#2684,
// tools.SpawnOfSession). A cross-agent spawn names the agent it runs as
// (#2730): its request is shown in the caller's chat, where the human must
// see which agent asks. Any other session has no such line: "".
func spawnOrigin(tr *tools.SpawnTracker, sessionKey string) string {
	id, label := "", ""
	if _, sp, ok := tr.ForBranch(sessionKey); ok {
		id, label = sp.ID, sp.Label()
	} else if sp, ok := tools.SpawnOfSession(sessionKey); ok && sp.Direct {
		id, label = sp.ID, sp.Label
	} else {
		return ""
	}
	if session.IsCrossAgentSpawnKey(sessionKey) {
		return fmt.Sprintf("🐣 **From %s's spawn %s:** %s", session.AgentIDFromKey(sessionKey), id, label)
	}
	return fmt.Sprintf("🐣 **From spawn %s:** %s", id, label)
}

// backendPromptFuncs returns the two prompt closures the DelegatedManager
// takes — the pair the gateway wires as PermissionPromptFunc (approvals, the
// owner's default chat) and QuestionPromptFunc (questions, the asking
// session's chat, #2275). Both are thin delegations to postBackendPrompt; the
// pair exists as a unit so the manager can route the two kinds without the
// backend knowing either's destination.
func backendPromptFuncs(d backendPromptDeps) (approval, question func(sessionKey, requestID, text, summary, attachmentPath string, choices []delegator.PromptChoice)) {
	approval = func(sessionKey, requestID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
		postBackendPrompt(d, promptToDefaultChat, sessionKey, requestID, text, summary, attachmentPath, choices)
	}
	question = func(sessionKey, requestID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
		postBackendPrompt(d, promptToSessionChat, sessionKey, requestID, text, summary, attachmentPath, choices)
	}
	return approval, question
}

// prependRequestOrigin prepends one origin line to a permission prompt that is
// shown in a chat which is not the requesting session's own — exactly the case
// where the connection addresses chats itself (platform.SessionButtonSender —
// Telegram/Discord) and the session key's chat ID is absent or differs from
// the default chat's. The owner approving another user's request needs to see
// whose it is (#2275). Sessions of the default chat (including branch keys
// such as main/c999/b123, which parse to the root chat) and every app prompt
// (the connection is already session-bound) get the text unchanged.
func prependRequestOrigin(sessionIdx *session.SessionIndex, agentID string, conn platform.Connection, sessionKey, text string) string {
	if sessionIdx == nil {
		return text
	}
	if _, ok := conn.(platform.SessionButtonSender); !ok {
		return text
	}
	chatID := session.ChatIDFromKey(sessionKey)
	if chatID != 0 && chatID == session.ChatIDFromKey(conn.DefaultSessionKey()) {
		return text
	}
	origin := fmt.Sprintf("🔐 Request from session %s (%s", sessionKey, conn.PlatformName())
	if chatID != 0 {
		origin += fmt.Sprintf(" chat %d", chatID)
		if name, err := sessionIdx.GetChatMetadataAnyPlatform(agentID, chatID, "username"); err == nil && name != "" {
			origin += fmt.Sprintf(", @%s", name)
		}
	}
	return origin + ")\n\n" + text
}

// sendPromptAttachment sends a prompt's attachment to the chat the prompt's
// buttons go to (the destination rule of attachmentChatID). On the app that is
// the target's own conversation only (platform.OwnChatDocumentSender), so the
// target that cannot show the buttons shows no attachment either (#2755).
func sendPromptAttachment(conn platform.Connection, kind backendPromptKind, sessionKey, attachmentPath string) error {
	if own, ok := conn.(platform.OwnChatDocumentSender); ok {
		return own.SendDocumentOwnChat(attachmentPath, "")
	}
	if chatID := attachmentChatID(conn, kind, sessionKey); chatID != 0 {
		return conn.SendDocumentToChat(chatID, attachmentPath, "")
	}
	return conn.SendDocument(attachmentPath, "")
}

// attachmentChatID names the chat a session's document goes to on conn; 0 means
// the plain SendDocument. On Telegram/Discord the plain SendDocument targets
// the LAST chat, which differs from the buttons' chat when two users share the
// bot (#2275), so the target is resolved explicitly: the default chat for
// approval prompts, the session's chat for questions and command documents —
// both falling back to the default and then the last chat (the destination rule
// of SendTextWithButtons / chatIDForSession). A connection that does not address
// chats itself (the app, already session-bound) uses SendDocument unchanged.
func attachmentChatID(conn platform.Connection, kind backendPromptKind, sessionKey string) int64 {
	if _, ok := conn.(platform.SessionButtonSender); !ok {
		return 0
	}
	chatID := session.ChatIDFromKey(conn.DefaultSessionKey())
	if kind == promptToSessionChat {
		if id := session.ChatIDFromKey(sessionKey); id != 0 {
			chatID = id
		}
	}
	if chatID == 0 {
		chatID = conn.ChatID()
	}
	return chatID
}

// firstLine returns the first line of s, for compact single-line logging of a
// multi-line error detail.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// deliverRateLimitNotice sends a CC usage-limit notice to the chat(s) chosen by
// [notify] rate_limit_notify_to (#1857). sessionKey identifies the foci session
// whose backend saw the rate_limit_event — before #1857 the hook carried no
// session identity, so every notice could only land in the agent's default chat
// even when a facet or app session triggered it.
//
//   - "session" (default): the session's own chat, falling back to the agent's
//     primary connection when that session has none live.
//   - "default": always the agent's primary connection (pre-#1857 behaviour).
//   - "both": the session's chat AND the primary, when they are different
//     connections — EXCEPT when the session confines replies
//     (platform.SessionConfinesReplies: its live connection, or its owning
//     platform when none is live — Slack, #2459 req 5): one user's notice
//     must not be copied into a conversation of a different audience, so it
//     stays in the session's own conversation only, and with no live
//     session connection it is dropped, never copied out.
//
// The throttle is per-agent (rate limits are account-wide), so under "session"
// the notice reaches only the session that FIRST crossed the utilization bucket
// — "both" is the setting for also copying it to the default chat.
func deliverRateLimitNotice(connMgr platform.ConnectionManager, agentID, sessionKey, notice, target string) {
	logger := log.NewComponentLogger("agent:" + agentID)

	send := func(conn platform.Connection, outcome route.DeliveryOutcome) bool {
		if conn == nil {
			return false
		}
		conn.SendNotification(notice)
		if platform.DropsAgentWideSends(conn) {
			// The connection has no conversation of its own to address
			// (Slack's unbound conn), so SendNotification has dropped the
			// notice, with its own WARN — never log a delivery that did
			// not happen.
			logger.Debugf("rate limit notice dropped by %s (no conversation to address; target=%s outcome=%s session=%s)", conn.PlatformName(), target, outcome, sessionKey)
			return false
		}
		logger.Debugf("rate limit notice delivered (target=%s outcome=%s session=%s)", target, outcome, sessionKey)
		return true
	}

	switch target {
	case config.RateLimitNotifyDefault:
		if !send(connMgr.Primary(agentID), route.DeliveredViaPrimary) {
			logger.Debugf("rate limit notice undelivered (target=%s, no primary connection): %s", target, notice)
		}
	case config.RateLimitNotifyBoth:
		sessionConn := connMgr.ForSession(sessionKey)
		delivered := send(sessionConn, route.DeliveredToSession)
		// A confining session — declared by its live connection OR by its
		// owning platform, so a session with no live connection confines
		// too — keeps the notice in its own conversation: copying it to
		// the primary would hand one user's traffic to a different
		// audience (#2459 req 5).
		if !platform.SessionConfinesReplies(connMgr, sessionKey) {
			if primary := connMgr.Primary(agentID); primary != sessionConn {
				delivered = send(primary, route.DeliveredViaPrimary) || delivered
			}
		}
		if !delivered {
			logger.Debugf("rate limit notice undelivered (target=%s, no live connection): %s", target, notice)
		}
	default: // config.RateLimitNotifySession
		conn, outcome := route.ConnFor(connMgr, agentID, sessionKey, route.PolicyFallback)
		if !send(conn, outcome) {
			logger.Debugf("rate limit notice undelivered (target=%s, outcome=%s): %s", target, outcome, notice)
		}
	}
}

// deliverModelFallbackNotice shows a model refusal fallback notice in the chat
// of the session it happened in, falling back to the agent's primary chat.
func deliverModelFallbackNotice(connMgr platform.ConnectionManager, agentID, sessionKey, notice string) {
	conn, outcome := route.ConnFor(connMgr, agentID, sessionKey, route.PolicyFallback)
	if conn == nil {
		log.NewComponentLogger("agent:"+agentID).Infof("model fallback notice undelivered (outcome=%s session=%s): %s", outcome, sessionKey, notice)
		return
	}
	conn.SendNotification(notice)
}

// buildDelegatedSystemPrompt concatenates workspace bootstrap blocks and the
// extra system blocks (Available Skills) into the single SystemPrompt string
// that delegator.StartOptions takes. Skills come after workspace files so the
// agent's identity/character is established first and skills land as reference
// material below it. API agents inject extraSystemBlocks via
// ag.ExtraSystemBlocks (separate provider block); delegated agents have to
// concatenate because the CC subprocess takes a single string at start.
//
// Empty inputs are handled cleanly — no leading or trailing separator if
// either side is empty.
func buildDelegatedSystemPrompt(workspaceBlocks, extraBlocks []provider.SystemBlock) string {
	var b strings.Builder
	write := func(blocks []provider.SystemBlock) {
		for _, blk := range blocks {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(blk.Text)
		}
	}
	write(workspaceBlocks)
	write(extraBlocks)
	return b.String()
}

// delegatedSystemPrompt assembles the single concatenated system-prompt string
// a CC-backend agent launches with: the per-agent environment block (envBlock,
// "" when disabled) prepended ahead of the workspace identity + skills blocks.
// Both the static setup-time prompt and the per-session SystemPromptFunc
// rebuild route through here so the environment block (and its "Foci Shell
// Tools" list) is present on every session — the rebuild wins over the static
// prompt at each start (#828/#706) and used to drop the env block.
func delegatedSystemPrompt(envBlock string, workspaceBlocks, extraBlocks []provider.SystemBlock) string {
	base := buildDelegatedSystemPrompt(workspaceBlocks, extraBlocks)
	switch {
	case envBlock == "":
		return base
	case base == "":
		return envBlock
	default:
		return envBlock + "\n\n" + base
	}
}

// newDelegatedSystemPromptFunc builds the StartOptions.SystemPromptFunc closure
// that rebuilds a delegated agent's prompt from disk at every session start
// (#828/#706 — so character-file and skill edits take effect on a fresh
// session). reload re-reads the workspace and skill blocks; the captured
// envBlock is re-prepended each time so the environment context survives the
// rebuild. This closure's result wins over the static StartOptions.SystemPrompt
// whenever non-empty (see delegated_manager.go), which is why the env block
// MUST be re-applied here and not only baked into the static prompt.
// newDelegatedSystemPromptFunc returns the per-session prompt generator. buildEnv
// rebuilds the environment block for a given session platform (so the ## Platform
// block matches the session's messaging platform) and for a spawn's session or
// not; platformFor resolves that platform from the session key, and isSpawn
// (nil = never) whether the session is a spawn's (#2986).
//
// digest (nil = none) returns the session's daily-memory digest (#2827), ""
// for a session that gets none; it goes LAST so everything before it stays a
// prefix shared across sessions.
func newDelegatedSystemPromptFunc(buildEnv func(sessionPlatform string, spawn bool) string, platformFor func(sessionKey string) string, isSpawn func(sessionKey string) bool, reload func() (workspaceBlocks, extraBlocks []provider.SystemBlock), digest func(sessionKey string) string) func(sessionKey string) string {
	return func(sessionKey string) string {
		envBlock := buildEnv(platformFor(sessionKey), isSpawn != nil && isSpawn(sessionKey))
		ws, extra := reload()
		p := delegatedSystemPrompt(envBlock, ws, extra)
		if digest != nil {
			if d := digest(sessionKey); d != "" && p != "" {
				p += "\n\n" + d
			}
		}
		return p
	}
}

// buildAutoApproveRules assembles the foci-level auto-approve rules for a
// delegated backend from resolved config + workspace-scoped defaults.
//
// fociExecNames is the list of foci shell-function names exported by the
// agent's tool registry (e.g. "foci_todo", "foci_remind"). These get an
// always-on auto-approve rule via FociShellRulesFor — no toggle, since
// they're foci's own constrained wrappers.
func buildAutoApproveRules(p setupParams, fociExecNames []string) []string {
	perms := p.resolved.Permissions // static-cfg:ignore: baked into the CC backend's session config once, no live-reload path exists for it

	// Foci shell functions are always auto-approved — derived from the
	// registry so adding/removing an ExecExport tool updates the rules
	// automatically.
	rules := autoapprove.FociShellRulesFor(fociExecNames)

	// Common readonly rules if enabled.
	if perms.AutoApproveCommonReadonly {
		rules = append(rules, autoapprove.CommonReadonlyRules...)
	}
	if perms.AutoApproveCommonSafeWrite {
		rules = append(rules, autoapprove.CommonSafeWriteRules...)
	}

	// Add workspace Edit/Write rules — delegated backends always need
	// workspace file access without prompting. Use the canonical (symlink-
	// resolved) workspace so the rule boundary lives in the same path space as
	// the canonicalized candidate path the auto-approver compares against
	// (P1-6) — otherwise a symlinked workspace parent would diverge and reject
	// legitimate nested writes.
	canonWorkspace := secrets.CanonicalPath(p.acfg.Workspace)
	rules = append(rules,
		fmt.Sprintf("Edit:%s/*", canonWorkspace),
		fmt.Sprintf("Write:%s/*", canonWorkspace),
	)

	// Append user-configured rules (already merged: agent ∪ global).
	rules = append(rules, perms.AutoApproveRules...)

	return rules
}

// buildExecRegistry creates a tools.Registry containing only exec-exported
// tools. These are exposed as shell functions (foci_todo, foci_send_to_chat,
// etc.) via the persistent exec bridge in the delegated backend's tmux session.
//
// wakeScheduleFn is the agent's scheduled-wake callback (built once
// transport-independently in setupAgent). Pass nil to skip remind-tool
// registration — useful when reminderStore is unconfigured.
//
// agLazy is a closure that returns the agent — used by the async notifier so
// send_to_session can deliver replies back to the calling session. May be nil
// in tests where the notifier path isn't exercised; when nil, send_to_session
// is still registered but reply_to=caller deliveries are a no-op.
func buildExecRegistry(p setupParams, wakeScheduleFn tools.ScheduleWakeFn, wakeCancelFn tools.CancelWakeFn, agLazy func() *agent.Agent) *tools.Registry {
	registry := tools.NewRegistry()
	acfg := p.acfg
	connMgr := p.connMgr

	// Shared infrastructure that mirrors the API path's tool wiring. Build once
	// at registry construction so every tool gets the same agent-scoped store,
	// async notifier, and resolved TTS as their API-path counterparts.
	var agentStore *secrets.Store
	if p.store != nil {
		agentStore = p.store.ForAgent(acfg.ID)
	}
	var notifier *tools.AsyncNotifier
	if agLazy != nil {
		notifier = newAsyncNotifier(agLazy, acfg.ID, p.agentResolverFn, p.ctx, connMgr, app.DeliverAgentMessage)
	}
	ttsRepls := voice.MergeReplacements(p.cfg.Voice.TTSReplacements, acfg.Voice.TTSReplacements)
	resolvedLive := p.resolvedLive
	agentTTS := func() voice.TTS {
		vc := resolvedLive.Load().Voice
		return p.tts.Resolve(vc.TTSPreference(), vc.TTSRate, ttsRepls)
	}

	batchSummariser := newBatchSummariser(p, agLazy)

	// Register the exec-exported subset from the single data-driven table (see
	// tool_table.go) — the same source of truth that drives the API path. The
	// table's pathExec filter selects exactly the exec-bridge tools.
	out := &toolOutputs{}
	registerTools(&toolDeps{
		p:              p,
		path:           pathExec,
		registry:       registry,
		agentStore:     agentStore,
		notifier:       notifier,
		connMgr:        connMgr,
		agLazy:         agLazy,
		summariser:     batchSummariser,
		wakeFn:         wakeScheduleFn,
		wakeCancelFn:   wakeCancelFn,
		sessionNotify:  newSessionNotifyFn(p.agentResolverFn, p.ctx, connMgr, "session_notify", app.DeliverAgentMessage),
		askDeliver:     newAskDeliverFn(p.agentResolverFn, p.ctx, connMgr),
		askUserDeliver: newAskUserDeliverFn(p.agentResolverFn, p.ctx, connMgr),
		agentTTS:       agentTTS,
		out:            out,
	})
	if agLazy != nil {
		if a := agLazy(); a != nil {
			a.AskRouter = out.askRouter
			if out.spawner != nil {
				a.SpawnRestore = out.spawner.RestoreSpawns // #2589: main runs it once the platforms are up
			}
			if out.depth != nil {
				a.DepthRestore = out.depth.Restore // #2588: main runs it once the platforms are up
				a.DepthOnReset = out.depth.OnReset
			}
			// Mirror the API path (agents.go), which sets AsyncNotifier on the
			// agent. The delegated path built the notifier and wired it into
			// tools but never stored it here, leaving a.AsyncNotifier nil. That
			// disabled two things for delegated agents: the /plan EnterPlanMode
			// injection (command/plan.go) and the #845 compaction-resume nudge,
			// which is invoked from runDelegatedCompact (compaction.go:199) but
			// suppressed by the nil guard. notifier is non-nil here (agLazy !=
			// nil, so it was constructed above).
			a.AsyncNotifier = notifier
		}
	}

	log.NewComponentLogger("agent:"+acfg.ID).Infof("exec bridge registry: %d tools (%v)", len(registry.All()), registry.ExportedNames())
	return registry
}

// resolvePreToolRules layers the global and per-agent pretool rules over the
// preinstalled defaults. A rule the merge leaves unusable is dropped with a
// warning rather than failing the agent: the rest must still apply.
func resolvePreToolRules(agentID string, global, perAgent []pretool.Rule) []pretool.Rule {
	rules, skipped := pretool.Resolve(pretool.Defaults, global, perAgent)
	warnSkippedRules(agentID, "pretool", skipped)
	return rules
}

// resolveStopRules is resolvePreToolRules for the agent's stop rules (#2089).
func resolveStopRules(agentID string, perAgent []stoprule.Rule) []stoprule.Rule {
	rules, skipped := stoprule.Resolve(perAgent)
	warnSkippedRules(agentID, "stop", skipped)
	return rules
}

func warnSkippedRules(agentID, kind string, skipped []string) {
	for _, msg := range skipped {
		log.NewComponentLogger("agent:"+agentID).Warnf("%s rule skipped: %s", kind, msg)
	}
}

// livePreToolRules returns the pretool rule source for an agent's CC backends
// (#2033). Each call re-reads the config file, so a rule edit reaches the next
// CC launch without a foci restart. The file carries no live-apply registry
// row for the rules, and a hand edit never goes through /config set, so
// reading the file at launch is what makes an edit land. A file that fails to
// load or validate (e.g. saved mid-edit), or no longer has the agent, keeps
// the last good rules, starting from the startup ones.
func livePreToolRules(agentID, configPath string, startup []pretool.Rule) func() []pretool.Rule {
	return liveRules(agentID, configPath, gwConfigProtect, "pretool", startup, (*config.Config).PreToolRules)
}

// liveStopRules is livePreToolRules for the agent's stop rules (#2089).
func liveStopRules(agentID, configPath string, startup []stoprule.Rule) func() []stoprule.Rule {
	return liveRules(agentID, configPath, gwConfigProtect, "stop", startup, (*config.Config).StopRules)
}

// liveRules is the shared body of the live rule sources: liveValue, plus a
// warning for each rule the merge left unusable.
func liveRules[R any](agentID, configPath string, protect func(string) error, kind string, startup []R,
	resolve func(*config.Config, string) ([]R, []string, bool)) func() []R {
	return liveValue(agentID, configPath, protect, kind+" rules", startup,
		func(cfg *config.Config, id string) ([]R, bool) {
			rules, skipped, ok := resolve(cfg, id)
			if ok {
				warnSkippedRules(id, kind, skipped)
			}
			return rules, ok
		})
}

// liveClaudeAISkills is the live source of backend_config.claude_ai_skills
// (#2953), read at every CC launch like the pretool rules.
func liveClaudeAISkills(agentID, configPath string, startup bool) func() bool {
	return liveValue(agentID, configPath, gwConfigProtect, "claude_ai_skills", startup, (*config.Config).ClaudeAISkills)
}

// liveValue re-reads the config file on every call and resolves this agent's
// value with resolve. It keeps the last good value when the file doesn't load,
// fails the protection check (protect, #479; nil skips it) or has lost the
// agent. what names the value in the warnings.
func liveValue[T any](agentID, configPath string, protect func(string) error, what string, startup T,
	resolve func(*config.Config, string) (T, bool)) func() T {
	var mu sync.Mutex
	last := startup
	lg := log.NewComponentLogger("agent:" + agentID)
	return func() T {
		mu.Lock()
		defer mu.Unlock()
		if configPath == "" {
			return last
		}
		if err := checkConfigLoad(protect, configPath); err != nil {
			lg.Warnf("%s: keeping the previous value: %v", what, err)
			return last
		}
		fresh, err := config.Load(configPath, delegator.RegisteredNames())
		if err != nil {
			lg.Warnf("%s: config reload failed, keeping the previous value: %v", what, err)
			return last
		}
		v, ok := resolve(fresh, agentID)
		if !ok {
			lg.Warnf("%s: agent missing from reloaded config, keeping the previous value", what)
			return last
		}
		last = v
		return last
	}
}

// newBatchSummariser builds a delegated agent's summariser.
func newBatchSummariser(p setupParams, agLazy func() *agent.Agent) *tools.BatchSummariser {
	acfg := p.acfg
	// Delegated agents dispatch the summary tool through the agent's own
	// DelegatedManager.RunBatch (BatchSummariser) — i.e. through whichever
	// backend (claude-code, codex, opencode) the agent is actually configured
	// to use (#1317). The model is the agent's batch_model (its folded
	// backend_config) when set, else the agent's own model — never a fixed
	// name the backend may not resolve (#2032). (API agents use
	// APISummariser.)
	//
	// The runner closure resolves ag.DelegatedManager lazily: buildExecRegistry
	// runs BEFORE configureDelegated assigns ag.DelegatedManager (see below),
	// so capturing it now would capture nil — by the time Summarise() actually
	// executes, setup has long finished.
	return tools.NewBatchSummariser(func() tools.BatchRunner {
		if agLazy == nil {
			return nil
		}
		a := agLazy()
		if a == nil || a.DelegatedManager == nil {
			return nil
		}
		return a.DelegatedManager
	}, func() string {
		if a := agLazy(); a != nil && a.DelegatedManager != nil {
			return a.DelegatedManager.BatchModel
		}
		return ""
	},
		acfg.Workspace, acfg.ID, func() int { return p.resolvedLive.Load().Summary.MaxSummaryInputChars })
}
