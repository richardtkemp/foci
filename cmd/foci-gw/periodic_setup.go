package main

import (
	"context"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/memory"
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
	gc := inst.resolved.Groups

	// Resolve model from powerful group to get endpoint information
	groupResolver := config.NewGroupResolver(gc, p.cfg.Models)
	resolved := groupResolver.ResolveGroup(config.GroupPowerful)
	var endpoint string
	var client provider.Client
	if resolved != nil {
		endpoint = resolved.Endpoint
		client = p.resolveEndpointClient(endpoint, resolved.Format)
	}

	// Check if keepalive should be enabled, considering both:
	// 1. Per-model auto-detection (OpenAI/DeepSeek have auto caching)
	// 2. Client-reported caching availability (Anthropic/Gemini)
	var cachingOverride *bool
	ka := inst.resolved.Keepalive
	if resolved != nil {
		modelKAEnabled, modelKAInterval := config.ResolveModelKeepalive(resolved)
		if modelKAEnabled {
			// Model config says keepalive is appropriate — override client check
			t := true
			cachingOverride = &t
			if modelKAInterval > 0 {
				s := modelKAInterval.String()
				ka.Interval = &s // override on local copy, not the resolved config
			}
		}
	}

	bg := inst.resolved.Background
	mf := inst.resolved.MemoryFormation

	cachingAvailable := true
	if cachingOverride != nil {
		cachingAvailable = *cachingOverride
	} else if client != nil {
		cachingAvailable = client.IsCachingAvailable()
	}
	kaEnabled := config.DerefBool(ka.Enabled) && cachingAvailable

	hasAgentWarnings := anyNotifyEnabled(inst.resolved, p.cfg, func(n config.NotifyConfig) bool { return n.InjectAgentWarningsLevel().Enabled() })
	hasChatWarnings := anyNotifyEnabled(inst.resolved, p.cfg, func(n config.NotifyConfig) bool { return n.InjectChatWarningsLevel().Enabled() })
	if !kaEnabled && !config.DerefBool(bg.Enabled) && !hasMemoryFormation(mf) && !hasAgentWarnings && !hasChatWarnings {
		return nil
	}

	kaOrientPrompt := config.DerefStr(config.First(acfg.Sessions.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt))
	buildOrient := func(branchKey, parentKey, branchType string) string {
		return prompts.BuildBranchOrientation(kaOrientPrompt, branchKey, parentKey, branchType, false, inst.promptSearchDirs)
	}
	branchFn := buildBranchFunc(
		acfg.ID, inst.ag, p.sessions, inst.defaultSessionKey,
		buildOrient, p.ctx,
		func(branchType, branchKey string) {
			if branchType != "background" {
				return
			}
			// Fire memory formation on the completed background branch.
			// skipMetaCheck=true because background branches set NoResetHook
			// but should still get memory formation on completion.
			agent.FireSessionEndMemory(inst.ag, p.sessions, branchKey, mf,
				buildOrient, inst.promptSearchDirs, p.ctx, true)
		},
	)

	// Shared timing config for warning dispatchers
	warningActiveInterval, _ := time.ParseDuration(p.cfg.Logging.WarningProactiveActiveInterval)
	warningInactiveInterval, _ := time.ParseDuration(p.cfg.Logging.WarningProactiveInactiveInterval)
	warningActivityThreshold, _ := time.ParseDuration(p.cfg.Logging.WarningProactiveActivityThreshold)
	agentID := acfg.ID
	lastUserMsgFn := func() time.Time {
		sk := inst.defaultSessionKey()
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
			Queue:          inst.ag.Warnings(),
			PeerQueues:     []*warnings.Queue{inst.ag.ChatWarnings()},
			IsProcessingFn: inst.ag.IsProcessing,
			FormatFn: func(body string) string {
				return prompts.FormatInjectedMessage("PROACTIVE WARNINGS", time.Now(), body)
			},
			DispatchFn: func(warningText string) {
				sk := inst.defaultSessionKey()
				if sk == "" {
					log.Warnf("warning", "[%s] no default session for proactive warning dispatch", agentID)
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

	ka.Enabled = &kaEnabled
	runner := periodic.New(periodic.RunnerConfig{
		AgentID:            acfg.ID,
		Client:             client,
		CachingOverride:    cachingOverride,
		Keepalive:          ka,
		Background:         bg,
		MemoryFormation:    mf,
		ManaInvestInterval: config.DerefStr(p.cfg.Mana.InvestInterval),
		PromptSearchDirs:   inst.promptSearchDirs,
		TodoStore:          p.todoStore,
		SessionIndex:       p.sessionIndex,
		BranchFunc:         branchFn,

		WarningDispatcher:      warningDispatcher,
		ChatWarningDispatcher:  chatWarningDispatcher,
		HasActiveWorkFn: func() int {
			if inst.tmuxWatchCount == nil {
				return 0
			}
			return inst.tmuxWatchCount()
		},
		DrainFn: func() {
			inst.ag.DrainRateLimitQueue(p.ctx)
		},
		// Session-aware availability checking
		SessionKeyFunc: inst.defaultSessionKey,
		CanFireFunc: func(ctx context.Context, sessionKey string) (bool, string) {
			return inst.ag.CanFireBackgroundOperation(ctx, sessionKey)
		},
	})
	runner.Start(p.ctx)
	inst.kaRunner = runner

	log.Infof("main", "agent %q periodic runner started (ka=%v bg=%v)", acfg.ID, kaEnabled, config.DerefBool(bg.Enabled))
	return runner
}
