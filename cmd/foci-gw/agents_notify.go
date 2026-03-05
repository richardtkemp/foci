package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"foci/internal/agent"
	"foci/internal/log"
	"foci/internal/memory"
	"foci/prompts"
	"foci/internal/telegram"
	"foci/internal/tools"
)

// newAsyncNotifier creates the async notifier callback for exec/tmux auto-background results.
// getAgent is a lazy getter since the agent is nil at creation time.
func newAsyncNotifier(
	getAgent func() *agent.Agent,
	defaultSessionKey func() string,
	botMgr *telegram.BotManager,
	agentID string,
	ctx context.Context,
) *tools.AsyncNotifier {
	return tools.NewAsyncNotifier(func(originSession, message string) {
		go func() {
			target := originSession
			if target == "" {
				target = defaultSessionKey()
			}

			bot := botMgr.BotForSessionOrPrimary(target, agentID)

			notifyCtx := agent.WithTrigger(ctx, "async_notify")
			if bot != nil {
				notifyCtx = agent.WithTurnCallbacks(notifyCtx, &agent.TurnCallbacks{
					ReplyFunc: func(text string) {
						if err := bot.SendToSession(target, text); err != nil {
							log.Errorf("async_notify", "intermediate telegram delivery: %v", err)
						}
					},
				})
			}

			resp, err := getAgent().HandleMessage(notifyCtx, target, message)
			if err != nil {
				log.Errorf("async_notify", "error: %v", err)
				return
			}
			log.Debugf("async_notify", "response length: %d", len(resp))
			if resp == "" {
				return
			}
			if bot == nil {
				log.Warnf("async_notify", "no bot for agent %s session %s, response not delivered", agentID, target)
				return
			}
			if err := bot.SendToSession(target, resp); err != nil {
				log.Errorf("async_notify", "telegram delivery: %v", err)
			}
		}()
	})
}

// newSessionNotifyFn creates the session notify callback for cross-agent message routing.
// When a send_to_session tool targets another agent's session, this function handles
// dispatching the message to the target agent and delivering the response.
func newSessionNotifyFn(
	agentResolverFn func(agentID string) *agentInstance,
	botMgr *telegram.BotManager,
	ctx context.Context,
) tools.SessionNotifyFn {
	return tools.SessionNotifyFn(func(targetSessionKey, message string) {
		go func() {
			parts := strings.Split(targetSessionKey, ":")
			if len(parts) < 2 {
				log.Errorf("session_notify", "invalid session key: %s", targetSessionKey)
				return
			}
			targetAgentID := parts[1]

			inst := agentResolverFn(targetAgentID)
			if inst == nil {
				log.Errorf("session_notify", "unknown agent %q for session %s", targetAgentID, targetSessionKey)
				return
			}

			resp, err := inst.ag.HandleMessage(agent.WithTrigger(ctx, "session_notify"), targetSessionKey, message)
			if err != nil {
				log.Errorf("session_notify", "error: %v", err)
				return
			}
			if resp == "" {
				return
			}

			bot := botMgr.BotForSessionOrPrimary(targetSessionKey, targetAgentID)
			if bot == nil {
				log.Warnf("session_notify", "no bot for agent %s session %s, response not delivered", targetAgentID, targetSessionKey)
				return
			}

			if err := bot.SendToSession(targetSessionKey, resp); err != nil {
				log.Errorf("session_notify", "telegram delivery: %v", err)
			}
		}()
	})
}

// setupWakeScheduler creates the wake scheduling function and registers the remind tool.
// It also restores any pending wakes from the database.
// Returns the wakeScheduleFn for use by other components.
func setupWakeScheduler(
	getAgent func() *agent.Agent,
	defaultSessionKey func() string,
	registry *tools.Registry,
	reminderStore *memory.ReminderStore,
	agentID string,
	ctx context.Context,
) {
	if reminderStore == nil {
		return
	}

	var wakesMu sync.Mutex
	wakes := make(map[int64]context.CancelFunc)

	wakeScheduleFn := func(id int64, delay time.Duration, message string) error {
		wakeCtx, wakeCancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-time.After(delay):
				log.Infof("remind", "firing wake id=%d after %v for agent %s: %q", id, delay, agentID, message)
				_ = reminderStore.Dismiss(id)
				sk := defaultSessionKey()
				if sk == "" {
					log.Warnf("remind", "no default session for agent %s, skipping", agentID)
					return
				}
				resp, err := getAgent().HandleMessage(agent.WithTrigger(ctx, "scheduled_wake"), sk, prompts.FormatInjectedMessage("SCHEDULED WAKE", time.Now(), message))
				if err != nil {
					log.Errorf("remind", "error: %v", err)
				} else {
					log.Debugf("remind", "response: %s", resp)
				}
				wakesMu.Lock()
				delete(wakes, id)
				wakesMu.Unlock()
			case <-wakeCtx.Done():
				_ = reminderStore.Dismiss(id)
				wakesMu.Lock()
				delete(wakes, id)
				wakesMu.Unlock()
			}
		}()
		wakesMu.Lock()
		wakes[id] = wakeCancel
		wakesMu.Unlock()
		return nil
	}

	registry.Register(tools.NewRemindTool(reminderStore, agentID, wakeScheduleFn))

	// Restore pending wakes from DB (survives restart)
	if pending, err := reminderStore.PendingWakes(agentID); err != nil {
		log.Errorf("remind", "failed to load pending wakes for %s: %v", agentID, err)
	} else if len(pending) > 0 {
		for _, r := range pending {
			delay := time.Until(r.DueAt)
			if delay < 0 {
				delay = 0
			}
			_ = wakeScheduleFn(r.ID, delay, r.Text)
		}
		log.Infof("remind", "restored %d pending wake(s) for agent %s", len(pending), agentID)
	}
}
