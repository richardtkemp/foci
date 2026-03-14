package main

import (
	"context"
	"sync"
	"time"

	"foci/internal/agent"
	"foci/internal/log"
	"foci/internal/memory"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/prompts"
)

// newAsyncNotifier creates the async notifier callback for exec/tmux auto-background results.
// getAgent is a lazy getter since the agent is nil at creation time.
func newAsyncNotifier(
	getAgent func() *agent.Agent,
	defaultSessionKey func() string,
	agentID string,
	ctx context.Context,
	sessions tools.SessionAppender,
	connMgr platform.ConnectionManager,
) *tools.AsyncNotifier {
	return tools.NewAsyncNotifier(func(targetSession, message, replyToSession, trigger string) {
		go func() {
			target := targetSession
			if target == "" {
				target = defaultSessionKey()
			}
			if trigger == "" {
				trigger = "async_notify"
			}

			// If replyToSession is set, route response back to caller
			if replyToSession != "" {
				notifyCtx := agent.WithTrigger(ctx, trigger)
				// Process message on target session
				resp, err := getAgent().HandleMessage(notifyCtx, target, message)
				if err != nil {
					log.Errorf(trigger, "error processing on target %s: %v", target, err)
					return
				}
				if resp == "" {
					return
				}

				// Format response with source session prefix
				formattedResp := "Message from session: " + target + "\n" + resp

				// STEP 1: Append to calling session JSONL (record the response)
				msg := provider.Message{
					Role:    "user",
					Content: provider.TextContent(formattedResp),
				}
				writer := sessions.For(replyToSession)
				if err := writer.Append(replyToSession, msg); err != nil {
					log.Errorf(trigger, "failed to append to caller %s: %v", replyToSession, err)
					return
				}

				// STEP 2: Display to user in calling session's chat.
				// Use SendToSession (not SendInjectedMessage) — the response is an
				// agent reply, not a system injection, so no header prefix.
				callerConn := connMgr.ForSession(replyToSession)
				if callerConn != nil {
					if err := callerConn.SendToSession(replyToSession, formattedResp); err != nil {
						log.Errorf(trigger, "platform delivery to caller %s: %v", replyToSession, err)
					}
				} else {
					log.Debugf(trigger, "no connection for caller session %s, response recorded but not displayed", replyToSession)
				}
				return
			}

			// Otherwise use existing behavior (display to target's chat)
			conn := connMgr.ForSessionOrPrimary(target, agentID)

			// Branch sessions without their own multiball connection should not
			// deliver replies to chat — they'd leak into the parent's chat.
			// The response still gets written to the branch JSONL via HandleMessage.
			sk, parseErr := session.ParseSessionKey(target)
			isBranchWithoutConn := parseErr == nil && !sk.IsRoot() && connMgr.ForSession(target) == nil

			notifyCtx := agent.WithTrigger(ctx, trigger)
			if conn != nil && !isBranchWithoutConn {
				notifyCtx = agent.WithTurnCallbacks(notifyCtx, &agent.TurnCallbacks{
					ReplyFunc: func(text string) {
						// Intermediate replies are agent output — use SendToSession
						// to avoid prepending the system injection header.
						if err := conn.SendToSession(target, text); err != nil {
							log.Errorf(trigger, "intermediate platform delivery: %v", err)
						}
					},
				})
			}

			resp, err := getAgent().HandleMessage(notifyCtx, target, message)
			if err != nil {
				log.Errorf(trigger, "error: %v", err)
				return
			}
			log.Debugf(trigger, "response length: %d", len(resp))
			if resp == "" {
				return
			}
			if conn == nil || isBranchWithoutConn {
				if isBranchWithoutConn {
					log.Debugf(trigger, "branch session %s has no dedicated connection, skipping platform delivery", target)
				} else {
					log.Warnf(trigger, "no connection for agent %s session %s, response not delivered", agentID, target)
				}
				return
			}
			// Final reply is agent output — use SendToSession to avoid
			// prepending the system injection header.
			if err := conn.SendToSession(target, resp); err != nil {
				log.Errorf(trigger, "platform delivery: %v", err)
			}
		}()
	})
}

// newSessionNotifyFn creates the session notify callback for cross-agent message routing.
// When a send_to_session tool targets another agent's session, this function handles
// dispatching the message to the target agent and delivering the response.
func newSessionNotifyFn(
	agentResolverFn func(agentID string) *agentInstance,
	ctx context.Context,
	connMgr platform.ConnectionManager,
) tools.SessionNotifyFn {
	return tools.SessionNotifyFn(func(targetSessionKey, message string) {
		go func() {
			sk, err := session.ParseSessionKey(targetSessionKey)
			if err != nil {
				log.Errorf("session_notify", "invalid session key %q: %v", targetSessionKey, err)
				return
			}
			targetAgentID := sk.AgentID

			inst := agentResolverFn(targetAgentID)
			if inst == nil {
				log.Errorf("session_notify", "unknown agent %q for session %s", targetAgentID, targetSessionKey)
				return
			}

			resp, err := inst.ag.HandleMessage(agent.WithTrigger(ctx, "session_notify"), targetSessionKey, message)
			if err != nil {
				log.Errorf("session_notify", "error for session %s: %v", targetSessionKey, err)
				return
			}
			if resp == "" {
				return
			}

			conn := connMgr.ForSessionOrPrimary(targetSessionKey, targetAgentID)
			if conn == nil {
				log.Warnf("session_notify", "no connection for agent %s session %s, response not delivered", targetAgentID, targetSessionKey)
				return
			}

			// Agent reply — use SendToSession to avoid prepending the
			// system injection header.
			if err := conn.SendToSession(targetSessionKey, resp); err != nil {
				log.Errorf("session_notify", "platform delivery for session %s: %v", targetSessionKey, err)
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
				// Wait for any active agent turn to finish before injecting.
				for getAgent().IsProcessing() {
					select {
					case <-time.After(2 * time.Second):
					case <-wakeCtx.Done():
						_ = reminderStore.Dismiss(id)
						return
					}
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
