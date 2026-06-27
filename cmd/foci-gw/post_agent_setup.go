package main

import (
	"context"
	"os/exec"
	"time"

	"foci/internal/app"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/resources"
	"foci/internal/tools/tmux"
)

// setupWarningHooks wires log warnings into agent warning queues.
// Pushes to both agent session queues and chat notification queues when configured.
func setupWarningHooks(agents map[string]*agentInstance, cfg *config.Config) {
	anyInjection := false
	for _, inst := range agents {
		if anyNotifyEnabled(inst.resolved, cfg, func(n config.ResolvedNotify) bool { return n.InjectAgentWarnings.Enabled() }) ||
			anyNotifyEnabled(inst.resolved, cfg, func(n config.ResolvedNotify) bool { return n.InjectChatWarnings.Enabled() }) {
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
			if w := inst.ag.ChatWarnings(); w != nil {
				w.Push(level.String(), component, msg)
			}
		}
	})
	log.Infof("main", "warning injection enabled")
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
	tmuxMemMon := tmux.NewTmuxMemoryMonitor(
		tmux.TmuxMemoryConfig{
			CheckInterval: checkInterval,
			WarnStr:       cfg.Tools.TmuxMemoryWarn,
			CriticalStr:   cfg.Tools.TmuxMemoryCritical,
			KillStr:       cfg.Tools.TmuxMemoryKill,
		},
		func(msg string) {
			for _, id := range agentOrder {
				inst := agents[id]
				if anyNotifyEnabled(inst.resolved, cfg, func(n config.ResolvedNotify) bool { return n.InjectAgentWarnings.Enabled() }) {
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
	if !config.DerefBool(cfg.Resources.MemoryGuardEnabled) {
		return nil
	}
	guardInterval, _ := time.ParseDuration(cfg.Resources.MemoryGuardInterval)
	if guardInterval <= 0 {
		return nil
	}
	memGuard := resources.NewMemoryGuard(
		resources.MemoryGuardConfig{
			Interval:          guardInterval,
			WarnPercent:       config.DerefInt(cfg.Resources.MemoryWarnPercent),
			KillPercent:       config.DerefInt(cfg.Resources.MemoryKillPercent),
			PressureThreshold: config.DerefFloat(cfg.Resources.MemoryPressureThreshold),
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

// countTelegramBots returns the total number of Telegram bots that will be
// started: one primary bot per agent with a telegram token, plus all shared
// and per-agent facet bots.
func countTelegramBots(cfg *config.Config) int {
	count := 0
	if tg := cfg.Platform("telegram"); tg != nil {
		count += len(tg.FacetBots) // shared facet pool
	}
	for _, acfg := range cfg.Agents {
		// Each agent with a resolvable telegram token gets a primary bot.
		// We approximate by counting all enabled agents — the token check
		// happens later, and missing tokens just mean the bot won't start.
		count++ // primary bot
		if tg := acfg.Platform("telegram"); tg != nil {
			count += len(tg.FacetBots) // per-agent facet bots
		}
	}
	return count
}

// countDiscordBots returns the number of Discord bots that will be started:
// one primary bot per agent whose discord platform config names a bot. Discord
// has no facet pool, so it is one bot per configured agent (mirrors the guard
// in setupDiscordBots, which bails when dc == nil || dc.Bot == "").
func countDiscordBots(cfg *config.Config) int {
	count := 0
	for _, acfg := range cfg.Agents {
		if dc := acfg.Platform("discord"); dc != nil && dc.Bot != "" {
			count++
		}
	}
	return count
}

// setupGoroutineMonitor starts the goroutine count monitor if configured. Returns a stop function (may be nil).
func setupGoroutineMonitor(cfg *config.Config, numAgents int, ctx context.Context) func() {
	interval, _ := time.ParseDuration(cfg.Resources.GoroutineMonitorInterval)
	if interval <= 0 {
		return nil
	}
	threshold := cfg.Resources.GoroutineMonitorThreshold
	if threshold <= 0 {
		// 30 base (global infra + tool-execution headroom)
		// + 25 per agent (DB pools, bleve, housekeeping goroutines)
		// + 5 per telegram bot (poll loop, worker, HTTP/2 read/write)
		// + 5 per discord bot (gateway ws read/write, heartbeat, event worker)
		numBots := countTelegramBots(cfg)
		numDiscord := countDiscordBots(cfg)
		threshold = 30 + 25*numAgents + 5*numBots + 5*numDiscord
	}
	mon := resources.NewGoroutineMonitor(resources.GoroutineMonitorConfig{
		Interval:  interval,
		Threshold: threshold,
		// App (FAP) sockets are dynamic — phones connect/disconnect at will, so
		// the static formula cannot budget them. Each live socket runs a
		// writePump goroutine + its accept goroutine (readPump inline), plus
		// transient per-turn goroutines: budget 4 each, recomputed per tick.
		DynamicExtra: func() int { return 4 * app.ActiveConnCount() },
	})
	mon.Start(ctx)
	return mon.Stop
}

// setupInteractiveCleanup starts periodic cleanup of expired interactive message
// callbacks (unanswered button presses). Runs every hour; prompts older than
// maxAge are expired (resolved as a denial/cancel, see CleanupExpiredInteractive).
func setupInteractiveCleanup(ctx context.Context, maxAge time.Duration) {
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				platform.CleanupExpiredInteractive(maxAge)
			}
		}
	}()
}

// setupToolDetailCleanup starts periodic tool detail expiry when all users are idle.
func setupToolDetailCleanup(
	toolDetailStore platform.ToolDetailStore,
	agents map[string]*agentInstance,
	agentOrder []string,
	connMgr platform.ConnectionManager,
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
					sk := mostRecentSessionKey(inst.ag, connMgr, id)
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
