package main

import (
	"context"
	"time"

	"foci/internal/app"
	"foci/internal/config"
	"foci/internal/memory"
	"foci/internal/modelinfo"
	"foci/internal/periodic"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/route"
	"foci/internal/session"
	"foci/internal/warnings"
	"foci/internal/workspace"
	"foci/shared/prompts"
)

type periodicParams struct {
	cfg                   *config.Config
	sessions              *session.Store
	connMgr               platform.ConnectionManager
	sessionIndex          *session.SessionIndex
	todoStore             *memory.TodoStore
	ctx                   context.Context
	resolveEndpointClient func(endpoint, format string) provider.Client
}

// setupPeriodic creates and starts a periodic runner for an agent instance.
// Returns the runner (also set on inst.kaRunner), or nil if not needed.
func setupPeriodic(inst *agentInstance, acfg config.AgentConfig, p periodicParams) *periodic.Runner {
	// The chat-call model resolution exists ONLY to find the API client + model
	// for keepalive cache-warming pings, which is an API-agent concern. Delegated
	// backends (claude-code, etc.) manage their own caching and route everything
	// through the backend — they need no resolver, no API client, and resolving
	// one would reach for anthropic credentials that don't exist on a
	// keyless/login-only deployment. So skip resolution entirely for them and
	// leave `resolved` nil; the `if resolved != nil` guards below handle it.
	var resolved *config.ResolvedModel
	if inst.ag.DelegatedManager == nil {
		groupResolver := config.NewGroupResolver(inst.LiveConfig().Groups, p.cfg.Models, p.cfg.HasAPIAgent())
		resolved = groupResolver.ResolveCall(config.CallChat)
	}

	var client provider.Client
	if resolved != nil {
		client = p.resolveEndpointClient(resolved.Endpoint, resolved.Format)
	}

	// Check if keepalive should be enabled. Per-model keepalive config can force
	// it on (cachingOverride); otherwise caching is assumed available — see the
	// FIXME(#848) block below for why the old client-probe was removed.
	var cachingOverride *bool
	var modelKAInterval time.Duration
	ka := inst.LiveConfig().Keepalive
	if resolved != nil {
		var modelKAEnabled bool
		modelKAEnabled, modelKAInterval = config.ResolveModelKeepalive(resolved)
		if modelKAEnabled {
			// Model config says keepalive is appropriate — override client check
			t := true
			cachingOverride = &t
			if modelKAInterval > 0 {
				ka.Interval = modelKAInterval.String() // override on local copy, not the resolved config
			}
		}
	}

	bg := inst.LiveConfig().Background
	refl := inst.LiveConfig().Reflection
	maint := inst.LiveConfig().Maintenance

	// Caching availability gates keepalive cache-warming. It's a STATIC model
	// capability, so we read it from the registry (#848) rather than forcing an
	// API client to be instantiated just to ask — which for delegated/claude-code
	// agents would reach for anthropic credentials that don't exist.
	//   - Delegated agents (resolved == nil): default true. The cache-warming is
	//     agent.Branch(), which runs a real turn through their backend (CC) and
	//     warms ITS prompt cache. So keepalive applies; it's gated purely by the
	//     existing [keepalive] enabled toggle below.
	//   - API agents (resolved != nil): derive from model metadata. Only Anthropic
	//     models have the explicit, TTL-bounded cache that pings warm.
	//   - Per-model keepalive config (cachingOverride) still wins, applied last.
	cachingAvailable := true
	if resolved != nil {
		cachingAvailable = modelinfo.Caching(resolved.ModelID)
	}
	if cachingOverride != nil {
		cachingAvailable = *cachingOverride
	}
	kaEnabled := ka.Enabled && cachingAvailable

	// cacheTTL is the backend's static prompt-cache lifetime, resolved once: it
	// bounds the keepalive window [interval, cacheTTL) and never varies at
	// runtime. It must come from the STATIC backend constant, not a per-session
	// lookup — at startup no session is live, and DelegatedManager.CacheTTL
	// returns 0 for a non-running backend (0 = unknown → no expiry ceiling).
	var cacheTTL time.Duration
	if inst.ag.DelegatedManager != nil {
		cacheTTL = inst.ag.DelegatedManager.StaticCacheTTL()
	}

	// Warn on a self-defeating config: if the interval is >= the cache TTL the
	// window is empty and keepalive can never fire, so warming is silently off.
	if kaEnabled && cacheTTL > 0 {
		if kaInterval, err := time.ParseDuration(ka.Interval); err == nil && kaInterval >= cacheTTL {
			keepaliveLog.Warnf("agent %q keepalive interval %s >= backend cache TTL %s: warming can never fire (interval must be shorter than the cache lifetime)", acfg.ID, kaInterval, cacheTTL)
		}
	}

	// Every agent gets a runner even when nothing is currently enabled: the
	// live config-apply path (liveapply.go) can switch these features on at
	// runtime, and an idle runner is one goroutine ticking every 30s.

	// periodicRederive recomputes the runner's live-tunable settings from a
	// freshly loaded config, preserving the static model-derived keepalive
	// adjustments made above. Called by the live config-apply path.
	inst.periodicRederive = func(freshCfg *config.Config, freshAcfg config.AgentConfig) periodic.Settings {
		rc := config.Resolve(freshCfg, freshAcfg)
		freshKA := rc.Keepalive
		if cachingOverride != nil && modelKAInterval > 0 {
			freshKA.Interval = modelKAInterval.String()
		}
		freshKA.Enabled = freshKA.Enabled && cachingAvailable
		return periodic.Settings{
			Keepalive:              freshKA,
			Background:             rc.Background,
			Reflection:             rc.Reflection,
			Maintenance:            rc.Maintenance,
			TickInterval:           rc.Scheduler.TickInterval,
			EphemeralRetentionDays: freshAcfg.Sessions.EffectiveEphemeralRetentionDays(freshCfg.Sessions.EphemeralRetentionDays),
		}
	}

	kaOrientPrompt := config.DerefStr(config.First(acfg.Sessions.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt))
	orientTemplate := prompts.ResolveOrientationTemplate(kaOrientPrompt, false, inst.promptSearchDirs...)
	branchFn := buildBranchFunc(
		acfg.ID, inst.ag, p.sessions,
		orientTemplate, p.ctx,
		func(branchType, branchKey string) {
			if branchType != "background" {
				return
			}
			// Fire memory formation on the completed background branch.
			// skipMetaCheck=true because background branches set NoResetHook
			// but should still get memory formation on completion.
			inst.ag.FireSessionEndMemory(p.ctx, branchKey, orientTemplate, true)
		},
	)

	// Shared timing config for warning dispatchers
	warningActiveInterval, _ := time.ParseDuration(p.cfg.Logging.WarningProactiveActiveInterval)
	warningInactiveInterval, _ := time.ParseDuration(p.cfg.Logging.WarningProactiveInactiveInterval)
	warningActivityThreshold, _ := time.ParseDuration(p.cfg.Logging.WarningProactiveActivityThreshold)
	agentID := acfg.ID
	// The dispatcher's active/inactive cadence keys off genuine human attention,
	// so read the clean per-session last_user_activity signal (derived max over
	// the agent's sessions) — NOT lastMessageTime, which any system-initiated
	// turn (keepalive/reflection/cron) advances, making keepalive agents look
	// permanently user-active.
	lastUserMsgFn := func() time.Time {
		if inst.ag == nil || inst.ag.SessionIndex == nil {
			return time.Time{}
		}
		last, _ := inst.ag.SessionIndex.LastUserActivityForAgent(agentID)
		return last
	}

	// Proactive warning dispatcher (agent session injection).
	// PeerQueues: the warn hook pushes every WARN/ERROR to both queues. Without
	// cross-queue suppression, a failed dispatch here (e.g. "no default session")
	// generates a WARN that enters the ChatWarnings queue, whose dispatch also
	// fails and re-enters this queue — an infinite cross-queue feedback loop.
	// Both dispatchers are always constructed; a dispatcher whose queue is
	// disabled (injection level off) no-ops on MaybeFire, so an off→on level
	// change applies live without spinning up a goroutine (#1225).
	warningDispatcher := warnings.NewDispatcher(warnings.DispatcherConfig{
		Name:       "agent",
		AgentID:    agentID,
		Queue:      inst.ag.Warnings(),
		PeerQueues: []*warnings.Queue{inst.ag.ChatWarnings()},
		// Defer proactive warnings only while the session they'd be injected
		// into (the most recent one) is mid-turn — not agent-wide.
		IsProcessingFn: func() bool {
			sk := defaultSessionKeyFor(inst.ag, agentID)
			return sk != "" && inst.ag.IsTurnInFlight(sk)
		},
		FormatFn: func(body string) string {
			return prompts.FormatInjectedMessage("PROACTIVE WARNINGS", time.Now(), body)
		},
		DispatchFn: func(warningText string) {
			sk := defaultSessionKeyFor(inst.ag, agentID)
			if sk == "" {
				warningLog.Warnf("[%s] no active session for proactive warning dispatch", agentID)
				return
			}
			deliverToSessionChat(inst.ag, p.ctx, "proactive_warning", p.connMgr, agentID, sk, warningText)
		},
		ActiveInterval:        warningActiveInterval,
		InactiveInterval:      warningInactiveInterval,
		ActivityThreshold:     warningActivityThreshold,
		LastUserMessageTimeFn: lastUserMsgFn,
	})

	// Chat warning dispatcher (platform notifications).
	// PeerQueues: same cross-queue feedback prevention as above — a failed
	// SendNotification (e.g. "no channel ID") must not enter the agent queue.
	chatWarningDispatcher := warnings.NewDispatcher(warnings.DispatcherConfig{
		Name:       "chat",
		AgentID:    agentID,
		Queue:      inst.ag.ChatWarnings(),
		PeerQueues: []*warnings.Queue{inst.ag.Warnings()},
		FormatFn: func(body string) string {
			return "[system diagnostics]\n" + body
		},
		DispatchFn: func(warningText string) {
			if conn := p.connMgr.Primary(agentID); conn != nil {
				conn.SendNotification(warningText)
			}
		},
		ActiveInterval:        warningActiveInterval,
		InactiveInterval:      warningInactiveInterval,
		ActivityThreshold:     warningActivityThreshold,
		LastUserMessageTimeFn: lastUserMsgFn,
	})

	ka.Enabled = kaEnabled
	runner := periodic.New(periodic.RunnerConfig{
		AgentID:          acfg.ID,
		Client:           client,
		CachingOverride:  cachingOverride,
		Keepalive:        ka,
		Background:       bg,
		Reflection:       refl,
		TickInterval:     inst.LiveConfig().Scheduler.TickInterval,
		CacheTTL:         cacheTTL,
		Maintenance:      maint,
		PromptSearchDirs: inst.promptSearchDirs,
		TodoStore:        p.todoStore,
		SessionIndex:     p.sessionIndex,

		EphemeralRetentionDays: acfg.Sessions.EffectiveEphemeralRetentionDays(p.cfg.Sessions.EphemeralRetentionDays),

		// The schedulers' single dependency: branch dispatch, in-flight checks,
		// rate-limit/can_run_background gating, reset, etc. (see background_agent.go). Test
		// overrides and the consolidation RunOnce/Branch and reset_time feature
		// flags are resolved inside the adapter / by IsDelegatedAgent + ResetTime.
		Agent: &backgroundAgent{inst: inst, connMgr: p.connMgr, agentID: agentID, branch: branchFn},

		OpenSessionsFn: openSessionsFn(ka.WarmOpenAppChats, agentID),

		WarningDispatcher:     warningDispatcher,
		ChatWarningDispatcher: chatWarningDispatcher,
		IsDelegatedAgent:      inst.ag.DelegatedManager != nil,
		CharacterSystemPromptFunc: func() string {
			fileOrder := acfg.System.SystemFiles
			if len(fileOrder) == 0 {
				fileOrder = workspace.DefaultFileOrder
			}
			return workspace.CharacterSystemPrompt(acfg.Workspace, fileOrder)
		},
		SkillDirs:             inst.skillsDirs,
		NotifySkillChange: func(sessionKey, text string) {
			route.NotifySessionChat(p.connMgr, agentID, sessionKey, text)
		},
	})
	runner.Start(p.ctx)
	inst.kaRunner = runner

	mainLog.Infof("agent %q periodic runner started (ka=%v bg=%v)", acfg.ID, kaEnabled, bg.Enabled)
	return runner
}

// openSessionsFn returns the keepalive open-chats resolver, or nil when the
// feature is disabled for this agent (keepalive then warms only the default).
func openSessionsFn(enabled bool, agentID string) func() []string {
	if !enabled {
		return nil
	}
	return func() []string { return app.OpenSessionsForAgent(agentID) }
}
