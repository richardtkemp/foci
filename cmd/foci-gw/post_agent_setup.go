package main

import (
	"context"
	"os/exec"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/resources"
	"foci/internal/tools"
)

// setupWarningHooks wires log warnings into agent sessions for agents with inject_agent_warnings.
func setupWarningHooks(agents map[string]*agentInstance, cfg *config.Config) {
	anyInjection := false
	for _, acfg := range cfg.Agents {
		if acfg.InjectAgentWarnings {
			anyInjection = true
			break
		}
	}
	if !anyInjection {
		return
	}
	log.SetWarnHook(func(level log.Level, component string, msg string) {
		for _, inst := range agents {
			if w := inst.ag.Warnings(); w != nil {
				w.Push(level.String(), component, msg)
			}
		}
	})
	log.Infof("main", "warning injection into agent sessions enabled")
}

// setupTmuxMemoryMonitor starts the tmux memory monitor if tmux is available.
// Returns a stop function (may be nil).
func setupTmuxMemoryMonitor(
	agents map[string]*agentInstance,
	agentOrder []string,
	cfg *config.Config,
	connMgr platform.ConnectionManager,
	ctx context.Context,
) func() {
	if _, err := exec.LookPath("tmux"); err != nil {
		return nil
	}
	if cfg.Tools.TmuxMemoryCheckInterval == "0" {
		return nil
	}
	checkInterval, _ := time.ParseDuration(cfg.Tools.TmuxMemoryCheckInterval)
	if checkInterval <= 0 {
		return nil
	}
	tmuxMemMon := tools.NewTmuxMemoryMonitor(
		tools.TmuxMemoryConfig{
			CheckInterval: checkInterval,
			WarnStr:       cfg.Tools.TmuxMemoryWarn,
			CriticalStr:   cfg.Tools.TmuxMemoryCritical,
			KillStr:       cfg.Tools.TmuxMemoryKill,
		},
		func(msg string) {
			for _, id := range agentOrder {
				inst := agents[id]
				if inst.agentCfg.InjectAgentWarnings {
					continue
				}
				if conn := connMgr.Primary(id); conn != nil {
					conn.SendNotification(msg)
				}
			}
		},
		func() {
			for _, id := range agentOrder {
				if fn := agents[id].tmuxClearAll; fn != nil {
					fn()
				}
			}
		},
	)
	tmuxMemMon.Start(ctx)
	return tmuxMemMon.Stop
}

// setupMemoryGuard starts the system memory guard if enabled. Returns a stop function (may be nil).
func setupMemoryGuard(agents map[string]*agentInstance, cfg *config.Config, ctx context.Context) func() {
	if !cfg.Resources.MemoryGuardEnabled {
		return nil
	}
	guardInterval, _ := time.ParseDuration(cfg.Resources.MemoryGuardInterval)
	if guardInterval <= 0 {
		return nil
	}
	memGuard := resources.NewMemoryGuard(
		resources.MemoryGuardConfig{
			Interval:          guardInterval,
			WarnPercent:       cfg.Resources.MemoryWarnPercent,
			KillPercent:       cfg.Resources.MemoryKillPercent,
			PressureThreshold: cfg.Resources.MemoryPressureThreshold,
		},
		func(msg string) {
			for _, inst := range agents {
				if w := inst.ag.Warnings(); w != nil {
					w.Push("WARN", "memory_guard", msg)
				}
			}
		},
	)
	memGuard.Start(ctx)
	return memGuard.Stop
}

// setupToolDetailCleanup starts periodic tool detail expiry when all users are idle.
func setupToolDetailCleanup(
	toolDetailStore platform.ToolDetailStore,
	agents map[string]*agentInstance,
	agentOrder []string,
	ctx context.Context,
) {
	if toolDetailStore == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				allIdle := true
				for _, id := range agentOrder {
					inst := agents[id]
					sk := inst.defaultSessionKey()
					if sk == "" {
						continue
					}
					if t := inst.ag.LastUserMessageTime(sk); !t.IsZero() && time.Since(t) < 10*time.Minute {
						allIdle = false
						break
					}
				}
				if allIdle {
					toolDetailStore.ExpireAndVacuum()
				}
			}
		}
	}()
}
