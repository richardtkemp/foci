package main

import (
	"context"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/memory"
	"foci/internal/modelinfo"
	"foci/internal/periodic"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/warnings"
	"foci/shared/prompts"
)

type periodicParams struct {
	cfg                   *config.Config
	sessions              *session.Store
	usageClientReg        *usageClientRegistry
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
		groupResolver := config.NewGroupResolver(inst.resolved.Groups, p.cfg.Models, p.cfg.HasAPIAgent())
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
	ka := inst.resolved.Keepalive
	if resolved != nil {
		modelKAEnabled, modelKAInterval := config.ResolveModelKeepalive(resolved)
		if modelKAEnabled {
			// Model config says keepalive is appropriate — override client check
			t := true
			cachingOverride = &t
			if modelKAInterval > 0 {
				ka.Interval = modelKAInterval.String() // override on local copy, not the resolved config
			}
		}
	}

	bg := inst.resolved.Background
	refl := inst.resolved.Reflection
	maint := inst.resolved.Maintenance

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

	hasAgentWarnings := anyNotifyEnabled(inst.resolved, p.cfg, func(n config.ResolvedNotify) bool { return n.InjectAgentWarnings.Enabled() })
	hasChatWarnings := anyNotifyEnabled(inst.resolved, p.cfg, func(n config.ResolvedNotify) bool { return n.InjectChatWarnings.Enabled() })
	if !kaEnabled && !bg.Enabled && !hasReflection(refl) && !hasMaintenance(maint) && !hasAgentWarnings && !hasChatWarnings {
		return nil
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
	lastUserMsgFn := func() time.Time {
		sk := mostRecentSessionKey(inst.ag, p.connMgr, agentID)
		if sk == "" {
			return time.Time{}
		}
		return inst.ag.LastUserMessageTime(sk)
	}

	// Proactive warning dispatcher (agent session injection).
	// PeerQueues: the warn hook pushes every WARN/ERROR to both queues. Without
	// cross-queue suppression, a failed dispatch here (e.g. "no default session")
	// generates a WARN that enters the ChatWarnings queue, whose dispatch also
	// fails and re-enters this queue — an infinite cross-queue feedback loop.
	var warningDispatcher *warnings.Dispatcher
	if hasAgentWarnings {
		warningDispatcher = warnings.NewDispatcher(warnings.DispatcherConfig{
			Queue:      inst.ag.Warnings(),
			PeerQueues: []*warnings.Queue{inst.ag.ChatWarnings()},
			// Defer proactive warnings only while the session they'd be injected
			// into (the most recent one) is mid-turn — not agent-wide.
			IsProcessingFn: func() bool {
				sk := mostRecentSessionKey(inst.ag, p.connMgr, agentID)
				return sk != "" && inst.ag.IsTurnInFlight(session.SessionKeyBase(sk))
			},
			FormatFn: func(body string) string {
				return prompts.FormatInjectedMessage("PROACTIVE WARNINGS", time.Now(), body)
			},
			DispatchFn: func(warningText string) {
				sk := mostRecentSessionKey(inst.ag, p.connMgr, agentID)
				if sk == "" {
					log.Warnf("warning", "[%s] no active session for proactive warning dispatch", agentID)
					return
				}
				deliverInjectedTurn(inst.ag, p.ctx, "proactive_warning", p.connMgr, agentID, sk, warningText)
			},
			ActiveInterval:        warningActiveInterval,
			InactiveInterval:      warningInactiveInterval,
			ActivityThreshold:     warningActivityThreshold,
			LastUserMessageTimeFn: lastUserMsgFn,
		})
	}

	// Chat warning dispatcher (platform notifications).
	// PeerQueues: same cross-queue feedback prevention as above — a failed
	// SendNotification (e.g. "no channel ID") must not enter the agent queue.
	var chatWarningDispatcher *warnings.Dispatcher
	if hasChatWarnings {
		chatWarningDispatcher = warnings.NewDispatcher(warnings.DispatcherConfig{
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
	}

	ka.Enabled = kaEnabled
	runner := periodic.New(periodic.RunnerConfig{
		AgentID:            acfg.ID,
		Client:             client,
		CachingOverride:    cachingOverride,
		Keepalive:          ka,
		Background:         bg,
		Reflection:         refl,
		TickInterval:       inst.resolved.Scheduler.TickInterval,
		Maintenance:        maint,
		ManaInvestInterval: inst.resolved.Mana.InvestInterval,
		PromptSearchDirs:   inst.promptSearchDirs,
		TodoStore:          p.todoStore,
		SessionIndex:       p.sessionIndex,

		// The schedulers' single dependency: branch dispatch, in-flight checks,
		// rate-limit/mana gating, reset, etc. (see background_agent.go). Test
		// overrides and the consolidation RunOnce/Branch and reset_time feature
		// flags are resolved inside the adapter / by IsDelegatedAgent + ResetTime.
		Agent: &backgroundAgent{inst: inst, connMgr: p.connMgr, agentID: agentID, branch: branchFn},

		WarningDispatcher:     warningDispatcher,
		ChatWarningDispatcher: chatWarningDispatcher,
		IsDelegatedAgent:      inst.ag.DelegatedManager != nil,
	})
	runner.Start(p.ctx)
	inst.kaRunner = runner

	log.Infof("main", "agent %q periodic runner started (ka=%v bg=%v)", acfg.ID, kaEnabled, bg.Enabled)
	return runner
}
