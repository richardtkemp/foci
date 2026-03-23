package main

import (
	"context"
	"sync"
	"time"

	"foci/internal/agent"
	"foci/internal/log"
	"foci/internal/memory"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/shared/prompts"
)

// mostRecentSessionKey returns the session key with the most recent user activity
// across all platform connections for the agent. Returns "" if no active sessions.
func mostRecentSessionKey(ag *agent.Agent, connMgr platform.ConnectionManager, agentID string) string {
	conns := connMgr.AllForAgent(agentID)
	var bestKey string
	var bestTime time.Time
	for _, conn := range conns {
		sk := conn.DefaultSessionKey()
		if sk == "" {
			continue
		}
		t := ag.LastUserMessageTime(sk)
		if t.After(bestTime) {
			bestKey = sk
			bestTime = t
		}
	}
	return bestKey
}

// newAsyncNotifier creates the async notifier callback for exec/tmux auto-background results.
// getAgent is a lazy getter since the agent is nil at creation time.
func newAsyncNotifier(
	getAgent func() *agent.Agent,
	agentID string,
	ctx context.Context,
	connMgr platform.ConnectionManager,
) *tools.AsyncNotifier {
	return tools.NewAsyncNotifier(func(targetSession, message, replyToSession, trigger string) {
		go func() {
			target := targetSession
			if target == "" {
				target = mostRecentSessionKey(getAgent(), connMgr, agentID)
			}
			if trigger == "" {
				trigger = "async_notify"
			}

			// If replyToSession is set, route response back to caller
			if replyToSession != "" {
				notifyCtx := agent.WithTrigger(ctx, trigger)
				// Wire tool call observers for the target session so tool
				// calls are visible in the user's chat during processing.
				if targetConn := connMgr.ForSessionOrPrimary(target, agentID); targetConn != nil {
					cb := &agent.TurnCallbacks{}
					defer wireTurnObservers(targetConn, target, cb)()
					notifyCtx = agent.WithTurnCallbacks(notifyCtx, cb)
				}
				// Process message on target session
				resp, err := getAgent().HandleMessage(notifyCtx, target, message)
				if err != nil {
					log.Errorf(trigger, "error processing on target %s: %v", target, err)
					return
				}
				if resp == "" {
					return
				}

				// Route the target session's response through HandleMessage on the
				// calling session. This maintains role alternation and lets the
				// agent process/relay the response.
				formattedResp := "Response from session " + target + ":\n" + resp
				injected := prompts.FormatInjectedMessage("SESSION RESPONSE", time.Now(), formattedResp,
					"[Inter-session response — the target session processed your message and returned this result. Relay the result to the user.]")
				deliverInjectedTurn(getAgent(), ctx, trigger, connMgr, agentID, replyToSession, injected)
				return
			}

			// Otherwise use existing behavior (display to target's chat)
			conn := connMgr.ForSessionOrPrimary(target, agentID)

			// Branch sessions without their own facet connection should not
			// deliver replies to chat — they'd leak into the parent's chat.
			// The response still gets written to the branch JSONL via HandleMessage.
			sk, parseErr := session.ParseSessionKey(target)
			isBranchWithoutConn := parseErr == nil && !sk.IsRoot() && connMgr.ForSession(target) == nil

			notifyCtx := agent.WithTrigger(ctx, trigger)
			if conn != nil && !isBranchWithoutConn {
				defer startTypingTicker(ctx, conn)()

				cb := &agent.TurnCallbacks{
					ReplyFunc: func(text string) {
						// Intermediate replies are agent output — use SendToSession
						// to avoid prepending the system injection header.
						if err := conn.SendToSession(target, text); err != nil {
							log.Errorf(trigger, "intermediate platform delivery: %v", err)
						}
					},
					ActivityFunc: func() {
						conn.SendTyping()
					},
				}
				defer wireTurnObservers(conn, target, cb)()
				notifyCtx = agent.WithTurnCallbacks(notifyCtx, cb)
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

			conn := connMgr.ForSessionOrPrimary(targetSessionKey, targetAgentID)
			notifyCtx := agent.WithTrigger(ctx, "session_notify")
			if conn != nil {
				cb := &agent.TurnCallbacks{}
				defer wireTurnObservers(conn, targetSessionKey, cb)()
				notifyCtx = agent.WithTurnCallbacks(notifyCtx, cb)
			}

			resp, err := inst.ag.HandleMessage(notifyCtx, targetSessionKey, message)
			if err != nil {
				log.Errorf("session_notify", "error for session %s: %v", targetSessionKey, err)
				return
			}
			if resp == "" {
				return
			}
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

// startTypingTicker sends an initial typing indicator and keeps it alive
// every 4 seconds until the returned cancel function is called.
func startTypingTicker(ctx context.Context, conn platform.Connection) (cancel func()) {
	conn.SendTyping()
	typingCtx, typingCancel := context.WithCancel(ctx)
	typingTicker := time.NewTicker(4 * time.Second)
	go func() {
		for {
			select {
			case <-typingTicker.C:
				conn.SendTyping()
			case <-typingCtx.Done():
				return
			}
		}
	}()
	return func() {
		typingTicker.Stop()
		typingCancel()
	}
}

// wireTurnObservers attaches platform-specific tool call/result/retry observers
// to the given TurnCallbacks. Returns a cleanup function that should be deferred.
func wireTurnObservers(conn platform.Connection, sessionKey string, cb *agent.TurnCallbacks) (cleanup func()) {
	obs := conn.BuildTurnObservers(sessionKey)
	if obs == nil {
		return func() {}
	}
	cb.ToolCallObserver = obs.OnToolCall
	cb.ToolResultObserver = obs.OnToolResult
	cb.RetryNotifyFunc = obs.OnRetry
	cb.RetrySuccessFunc = obs.OnRetryClear
	return obs.Cleanup
}

// deliverInjectedTurn runs a HandleMessage turn and delivers the response
// to the user's platform connection. Used by all system-initiated injections
// (restart changelog, scheduled wakes, proactive warnings).
func deliverInjectedTurn(
	ag *agent.Agent,
	ctx context.Context,
	trigger string,
	connMgr platform.ConnectionManager,
	agentID string,
	sessionKey string,
	message string,
) {
	conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
	triggerCtx := agent.WithTrigger(ctx, trigger)
	if conn != nil {
		defer startTypingTicker(ctx, conn)()

		cb := &agent.TurnCallbacks{
			ReplyFunc: func(text string) {
				if err := conn.SendToSession(sessionKey, text); err != nil {
					log.Errorf(trigger, "intermediate platform delivery: %v", err)
				}
			},
			ActivityFunc: func() {
				conn.SendTyping()
			},
		}
		defer wireTurnObservers(conn, sessionKey, cb)()
		triggerCtx = agent.WithTurnCallbacks(triggerCtx, cb)
	}
	resp, err := ag.HandleMessage(triggerCtx, sessionKey, message)
	if err != nil {
		log.Errorf(trigger, "error: %v", err)
		return
	}
	if resp == "" {
		return
	}
	if conn == nil {
		log.Warnf(trigger, "no connection for session %s agent %s, response not delivered", sessionKey, agentID)
		return
	}
	if err := conn.SendToSession(sessionKey, resp); err != nil {
		log.Errorf(trigger, "platform delivery: %v", err)
	}
}

// setupWakeScheduler creates the wake scheduling function and registers the remind tool.
// It also restores any pending wakes from the database.
// Returns the wakeScheduleFn for use by other components.
func setupWakeScheduler(
	getAgent func() *agent.Agent,
	registry *tools.Registry,
	reminderStore *memory.ReminderStore,
	agentID string,
	ctx context.Context,
	connMgr platform.ConnectionManager,
) {
	if reminderStore == nil {
		return
	}

	var wakesMu sync.Mutex
	wakes := make(map[int64]context.CancelFunc)

	wakeScheduleFn := func(id int64, delay time.Duration, message, sessionKey string) error {
		wakeCtx, wakeCancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-time.After(delay):
				log.Infof("remind", "firing wake id=%d after %v for agent %s: %q", id, delay, agentID, message)
				_ = reminderStore.Dismiss(id)
				// Use the originating session key if stored, otherwise
				// pick the most recently active session.
				sk := sessionKey
				if sk == "" {
					sk = mostRecentSessionKey(getAgent(), connMgr, agentID)
				}
				if sk == "" {
					log.Warnf("remind", "no session for agent %s, skipping", agentID)
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
				deliverInjectedTurn(getAgent(), ctx, "scheduled_wake", connMgr, agentID, sk, prompts.FormatInjectedMessage("SCHEDULED WAKE", time.Now(), message))
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
			_ = wakeScheduleFn(r.ID, delay, r.Text, r.SessionKey)
		}
		log.Infof("remind", "restored %d pending wake(s) for agent %s", len(pending), agentID)
	}
}
