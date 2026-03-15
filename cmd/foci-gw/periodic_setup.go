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
	"foci/internal/state"
	"foci/internal/warnings"
	"foci/prompts"
)

type periodicParams struct {
	cfg                   *config.Config
	sessions              *session.Store
	usageClientReg        *usageClientRegistry
	connMgr               platform.ConnectionManager
	stateStore            *state.Store
	todoStore             *memory.TodoStore
	ctx                   context.Context
	resolveEndpointClient func(endpoint, format string) provider.Client
}

// setupPeriodic creates and starts a periodic runner for an agent instance.
// Returns the runner (also set on inst.kaRunner), or nil if not needed.
func setupPeriodic(inst *agentInstance, acfg config.AgentConfig, p periodicParams) *periodic.Runner {
	// Resolve model to get endpoint information
	resolved, err := config.ResolveModel(acfg.Model, acfg.Endpoint, p.cfg.Models.Aliases)
	var endpoint string
	var client provider.Client
	if err == nil {
		endpoint = resolved.Endpoint
		client = p.resolveEndpointClient(endpoint, resolved.Format)
	}

	// Check if provider supports caching
	cachingAvailable := client != nil && client.IsCachingAvailable()
	kaEnabled := acfg.Keepalive.Enabled && cachingAvailable

	if !kaEnabled && !acfg.Background.Enabled && !hasMemoryFormation(acfg.MemoryFormation) && !acfg.InjectAgentWarnings {
		return nil
	}

	kaOrientPrompt := prompts.ResolveOrientPath(acfg.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt)
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
			agent.FireSessionEndMemory(inst.ag, p.sessions, branchKey, acfg.MemoryFormation,
				buildOrient, inst.promptSearchDirs, p.ctx, true)
		},
	)

	// Proactive warning dispatcher
	var warningDispatcher *warnings.Dispatcher
	if acfg.InjectAgentWarnings {
		warningActiveInterval, _ := time.ParseDuration(p.cfg.Logging.WarningProactiveActiveInterval)
		warningInactiveInterval, _ := time.ParseDuration(p.cfg.Logging.WarningProactiveInactiveInterval)
		warningActivityThreshold, _ := time.ParseDuration(p.cfg.Logging.WarningProactiveActivityThreshold)

		agentID := acfg.ID
		warningDispatcher = warnings.NewDispatcher(warnings.DispatcherConfig{
			Queue:          inst.ag.Warnings(),
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
			ActiveInterval:    warningActiveInterval,
			InactiveInterval:  warningInactiveInterval,
			ActivityThreshold: warningActivityThreshold,
			LastUserMessageTimeFn: func() time.Time {
				sk := inst.defaultSessionKey()
				if sk == "" {
					return time.Time{}
				}
				return inst.ag.LastUserMessageTime(sk)
			},
		})
	}

	kaCfg := acfg.Keepalive
	kaCfg.Enabled = kaEnabled
	runner := periodic.New(periodic.RunnerConfig{
		AgentID:            acfg.ID,
		Client:             client,
		Keepalive:          kaCfg,
		Background:         acfg.Background,
		MemoryFormation:    acfg.MemoryFormation,
		ManaInvestInterval: p.cfg.Mana.InvestInterval,
		PromptSearchDirs:   inst.promptSearchDirs,
		TodoStore:          p.todoStore,
		StateStore:         p.stateStore,
		BranchFunc:         branchFn,

		WarningDispatcher:  warningDispatcher,
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

	log.Infof("main", "agent %q periodic runner started (ka=%v bg=%v)", acfg.ID, acfg.Keepalive.Enabled, acfg.Background.Enabled)
	return runner
}
