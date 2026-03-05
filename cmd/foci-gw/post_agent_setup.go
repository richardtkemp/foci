package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/resources"
	"foci/internal/secrets"
	"foci/internal/session"
	"foci/internal/startup"
	"foci/internal/state"
	"foci/internal/telegram"
	"foci/internal/tools"
	"foci/internal/voice"
)

// setupSharedMultiball creates shared multiball bots available to any agent.
func setupSharedMultiball(
	botMgr *telegram.BotManager,
	agents map[string]*agentInstance,
	agentOrder []string,
	cfg *config.Config,
	store *secrets.Store,
	sessions *session.Store,
	sttProvider voice.STT,
	ttsProvider voice.TTS,
	toolDetailStore *telegram.ToolDetailStore,
	stateStore *state.Store,
	ctx context.Context,
) {
	if len(cfg.Telegram.MultiballBots) == 0 || len(agentOrder) == 0 {
		return
	}

	firstInst := agents[agentOrder[0]]
	for _, botName := range cfg.Telegram.MultiballBots {
		mbToken := config.ResolveBotToken(botName, "", store)
		if mbToken == "" {
			log.Errorf("main", "shared multiball bot %q: token not found", botName)
			continue
		}
		mbBot, err := telegram.NewBot(mbToken, cfg.Telegram.AllowedUsers,
			firstInst.ag, firstInst.cmds, command.NewLastMessageStore(), "")
		if err != nil {
			log.Errorf("main", "shared multiball bot %q: create: %v", botName, err)
			continue
		}
		configureMultiballBot(mbBot, multiballBotConfig{
			sttProvider:     sttProvider,
			ttsProvider:     ttsProvider,
			stopAliases:     cfg.Telegram.StopAliases,
			enableStopAlias: cfg.Telegram.EnableStopAliases,
			acfg:            firstInst.agentCfg,
			cfg:             cfg,
			toolDetailStore: toolDetailStore,
			stateStore:      stateStore,
		})
		botMgr.AddSharedMultiball(mbBot)
	}

	if pool := botMgr.SharedPool(); pool != nil && pool.Size() > 0 {
		ttl, _ := time.ParseDuration(cfg.Telegram.MultiballSessionTTL)
		if ttl > 0 {
			pool.SetSessionTTL(ttl, sessions)
		}
		pool.ReclaimHook = func(sessionKey string) {
			for _, id := range agentOrder {
				inst := agents[id]
				prefix := "agent:" + id + ":"
				if strings.HasPrefix(sessionKey, prefix) {
					orientPath := resolveOrientPath(inst.agentCfg.BranchOrientationHeadlessPrompt, cfg.Sessions.BranchOrientationHeadlessPrompt, inst.agentCfg.BranchOrientationPrompt, cfg.Sessions.BranchOrientationPrompt)
					fireSessionEndMemory(inst.ag, sessions, sessionKey, inst.agentCfg.MemoryFormation, func(bk, pk, bt string) string {
						return buildBranchOrientation(orientPath, bk, pk, bt, false, inst.promptSearchDirs)
					}, inst.promptSearchDirs, ctx)
					return
				}
			}
		}
		log.Infof("main", "%d shared multiball bots ready", pool.Size())
	}
}

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
			if inst.ag.Warnings != nil {
				inst.ag.Warnings.Push(level.String(), component, msg)
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
	botMgr *telegram.BotManager,
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
				if bot := botMgr.PrimaryBot(id); bot != nil {
					bot.SendNotification(msg)
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
				if inst.ag.Warnings != nil {
					inst.ag.Warnings.Push("WARN", "memory_guard", msg)
				}
			}
		},
	)
	memGuard.Start(ctx)
	return memGuard.Stop
}

// setupToolDetailCleanup starts periodic tool detail expiry when all users are idle.
func setupToolDetailCleanup(
	toolDetailStore *telegram.ToolDetailStore,
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

// sendStartupNotifications diagnoses restart type and sends notifications via Telegram.
func sendStartupNotifications(
	agents map[string]*agentInstance,
	agentOrder []string,
	botMgr *telegram.BotManager,
	stateStore *state.Store,
	cfg *config.Config,
	startTime time.Time,
) *startup.DiagnosisResult {
	logsDir := filepath.Dir(cfg.Logging.EventFile)
	if logsDir == "" || logsDir == "." {
		logsDir = ""
	}
	diagnosis := startup.DiagnoseRestart(stateStore, startTime, logsDir)
	if diagnosis.Class != startup.ClassClean && diagnosis.Class != startup.ClassUnknown {
		log.Infof("startup", "restart classified as %s: %s", diagnosis.Class, diagnosis.Summary)
	}

	for _, id := range agentOrder {
		inst := agents[id]
		enabled := cfg.Telegram.EnableStartupNotify
		if inst.agentCfg.StartupNotification != nil {
			enabled = *inst.agentCfg.StartupNotification
		}
		if enabled {
			if bot := botMgr.PrimaryBot(id); bot != nil {
				bot.SendStartupNotificationWithDiagnosis(id, diagnosis)
			}
		}
	}
	return diagnosis
}
