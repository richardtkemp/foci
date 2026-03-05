package main

import (
	"context"
	"strconv"
	"time"

	"foci/agent"
	"foci/config"
	"foci/log"
	"foci/prompts"
	"foci/session"
	"foci/state"
	"foci/telegram"
)

// handleWelcomeAndFirstRun injects welcome file content and first-run onboarding prompts.
func handleWelcomeAndFirstRun( // nolint:unparam
	agents map[string]*agentInstance,
	agentOrder []string,
	sessions *session.Store,
	stateStore *state.Store,
	botMgr *telegram.BotManager,
	cfg *config.Config,
	ctx context.Context,
) {
	// Welcome file (written by setup.sh on update)
	if len(agentOrder) > 0 {
		if content := injectWelcomeFile(cfg.WelcomeFile, agents, agentOrder, sessions); content != "" {
			inst := agents[agentOrder[0]]
			go func() {
				sk := inst.defaultSessionKey()
				if sk == "" {
					log.Warnf("main", "no default session for welcome file injection, skipping")
					return
				}
				restartCtx := agent.WithTrigger(ctx, "restart")
				inst.ag.SetSessionNoCompact(sk, true)
				msg := prompts.FormatInjectedMessage("SYSTEM UPDATE", time.Now(), content)
				if _, err := inst.ag.HandleMessage(restartCtx, sk, msg); err != nil {
					log.Errorf("main", "restart turn failed: %v", err)
				}
			}()
		}
	}

	// First-run onboarding — inject prompt for new agents
	for _, agentID := range agentOrder {
		inst := agents[agentID]
		if msg := checkFirstRun(stateStore, inst.agentCfg); msg != "" {
			agentID := agentID
			go func() {
				sk := inst.defaultSessionKey()
				if sk == "" {
					// On first run, no Telegram message has arrived yet.
					// Construct session key from first allowed user ID.
					users := inst.agentCfg.AllowedUsers
					if len(users) == 0 {
						users = cfg.Telegram.AllowedUsers
					}
					if len(users) > 0 {
						if chatID, err := strconv.ParseInt(users[0], 10, 64); err == nil {
							sk = telegram.SessionKeyForChat(agentID, chatID)
						}
					}
				}
				if sk == "" {
					log.Warnf("main", "no default session for first-run injection on %s, skipping", agentID)
					return
				}
				firstRunCtx := agent.WithTrigger(ctx, "first_run")
				if _, err := inst.ag.HandleMessage(firstRunCtx, sk, msg); err != nil {
					log.Errorf("main", "first-run turn for %s failed: %v", agentID, err)
					return
				}
				if err := stateStore.Set("agent:"+agentID+":first_run_completed", true); err != nil {
					log.Errorf("main", "set first_run_completed for %s: %v", agentID, err)
				}
				log.Infof("main", "first-run onboarding completed for %s", agentID)
			}()
		}
	}
}
