package main

import (
	"context"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/state"
	"foci/prompts"
)

// handleWelcomeAndFirstRun injects welcome file content and first-run onboarding prompts.
func handleWelcomeAndFirstRun(
	agents map[string]*agentInstance,
	agentOrder []string,
	sessions *session.Store,
	stateStore *state.Store,
	cfg *config.Config,
	ctx context.Context,
	connMgr platform.ConnectionManager,
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
				msg := prompts.FormatInjectedMessage("SYSTEM UPDATE", time.Now(), content)
				deliverInjectedTurn(inst.ag, ctx, "restart", connMgr, agentOrder[0], sk, msg)
			}()
		}
	}

	// First-run onboarding — store the prompt so it gets prepended to the
	// first inbound message as a separate content block. The agent clears it
	// after consumption, and we mark first_run_completed via OnActivity.
	for _, agentID := range agentOrder {
		inst := agents[agentID]
		if msg := checkFirstRun(stateStore, inst.agentCfg); msg != "" {
			inst.ag.FirstRunMessage.Store(msg)
			agentID := agentID
			var once sync.Once
			inst.ag.OnActivity.Add(func(_ string) {
				once.Do(func() {
					if err := stateStore.Set("agent/"+agentID+"/first_run_completed", true); err != nil {
						log.Errorf("main", "set first_run_completed for %s: %v", agentID, err)
					}
					log.Infof("main", "first-run onboarding completed for %s", agentID)
				})
			})
		}
	}
}
