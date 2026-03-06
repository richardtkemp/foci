package main

import (
	"context"
	"time"

	"foci/internal/agent"
	"foci/internal/anthropic"
	"foci/internal/config"
	"foci/internal/keepalive"
	"foci/internal/log"
	"foci/internal/mana"
	"foci/internal/memory"
	"foci/internal/provider"
	"foci/prompts"
	"foci/internal/session"
	"foci/internal/state"
	"foci/internal/telegram"
	"foci/internal/warnings"
)

type keepaliveParams struct {
	cfg                   *config.Config
	sessions              *session.Store
	usageClientReg        *usageClientRegistry
	botMgr                *telegram.BotManager
	stateStore            *state.Store
	todoStore             *memory.TodoStore
	ctx                   context.Context
	resolveEndpointClient func(endpoint, modelID string) provider.Client
}

// setupKeepalive creates and starts a keepalive runner for an agent instance.
// Returns the runner (also set on inst.kaRunner), or nil if not needed.
func setupKeepalive(inst *agentInstance, acfg config.AgentConfig, p keepaliveParams) *keepalive.Runner {
	// Resolve model to get endpoint information
	resolved, err := config.ResolveModel(acfg.Model, acfg.Endpoint, p.cfg.Models.Aliases)
	var endpoint string
	var client provider.Client
	if err == nil {
		endpoint = resolved.Endpoint
		client = p.resolveEndpointClient(endpoint, acfg.Model)
	}

	// Check if provider supports caching
	cachingAvailable := client != nil && client.IsCachingAvailable()
	kaEnabled := acfg.Keepalive.Enabled && cachingAvailable

	if !kaEnabled && !acfg.Background.Enabled && !hasMemoryFormation(acfg.MemoryFormation) && !acfg.InjectAgentWarnings {
		return nil
	}

	kaOrientPrompt := resolveOrientPath(acfg.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt, acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt)
	branchFn := buildBranchFunc(
		acfg.ID, inst.ag, p.sessions, inst.defaultSessionKey,
		func(branchKey, parentKey, branchType string) string {
			return buildBranchOrientation(kaOrientPrompt, branchKey, parentKey, branchType, false, inst.promptSearchDirs)
		},
		p.ctx,
	)

	// Mana monitor for background work throttling
	// Keepalive should use default session's current model, not agent's configured default
	getDefaultSessionUsageClient := func() *anthropic.UsageClient {
		sk := inst.defaultSessionKey()
		if sk == "" || inst.ag == nil {
			return nil
		}
		return inst.ag.SessionUsageClient(sk)
	}
	manaMonitor := mana.NewMonitorWithFunc(getDefaultSessionUsageClient)

	// Proactive warning dispatcher
	var warningDispatcher *warnings.Dispatcher
	if acfg.InjectAgentWarnings {
		warningActiveInterval, _ := time.ParseDuration(p.cfg.Logging.WarningProactiveActiveInterval)
		warningInactiveInterval, _ := time.ParseDuration(p.cfg.Logging.WarningProactiveInactiveInterval)
		warningActivityThreshold, _ := time.ParseDuration(p.cfg.Logging.WarningProactiveActivityThreshold)

		agentID := acfg.ID
		warningDispatcher = warnings.NewDispatcher(warnings.DispatcherConfig{
			Queue: inst.ag.Warnings,
			FormatFn: func(body string) string {
				return prompts.FormatInjectedMessage("PROACTIVE WARNINGS", time.Now(), body)
			},
			DispatchFn: func(warningText string) {
				sk := inst.defaultSessionKey()
				if sk == "" {
					log.Warnf("keepalive", "no default session for proactive warning dispatch on agent %q", agentID)
					return
				}
				resp, err := inst.ag.HandleMessage(agent.WithTrigger(p.ctx, "proactive_warning"), sk, warningText)
				if err != nil {
					log.Errorf("keepalive", "proactive warning turn error: %v", err)
					return
				}
				if resp == "" {
					return
				}
				if bot := p.botMgr.BotForSessionOrPrimary(sk, agentID); bot != nil {
					if err := bot.SendToSession(sk, resp); err != nil {
						log.Errorf("keepalive", "proactive warning telegram delivery: %v", err)
					}
				}
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
	runner := keepalive.New(keepalive.RunnerConfig{
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
		ManaMonitor:        manaMonitor,
		WarningDispatcher:  warningDispatcher,
		HasActiveWorkFn: func() bool {
			return inst.tmuxWatchCount != nil && inst.tmuxWatchCount() > 0
		},
		DrainFn: func() {
			inst.ag.DrainRateLimitQueue(p.ctx)
		},
	})
	runner.Start(p.ctx)
	inst.kaRunner = runner

	// Wire Telegram bot callbacks to keepalive runner
	if bot := p.botMgr.PrimaryBot(acfg.ID); bot != nil {
		bot.OnUserMessage = func() {
			runner.NotifyInteraction()
		}
		bot.OnTurnComplete = func() {
			runner.NotifyCacheWarmed()
		}
	}

	log.Infof("main", "agent %q keepalive runner started (ka=%v bg=%v)", acfg.ID, acfg.Keepalive.Enabled, acfg.Background.Enabled)
	return runner
}
