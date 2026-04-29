package main

import (
	"context"
	"sync"
	"time"

	"foci/internal/agent"
	"foci/internal/agent/turnevent"
	"foci/internal/log"
	"foci/internal/memory"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/turn"
	"foci/shared/prompts"
)

// mostRecentSessionKey returns the session key with the most recent user activity
// across all platform connections for the agent. Returns "" if no active sessions.
func mostRecentSessionKey(ag *agent.Agent, connMgr platform.ConnectionManager, agentID string) string {
	if connMgr == nil {
		return ""
	}
	conns := connMgr.AllForAgent(agentID)
	var bestKey string
	var bestTime time.Time
	for _, conn := range conns {
		sk := conn.DefaultSessionKey()
		if sk == "" {
			continue
		}
		t := ag.LastUserMessageTime(sk)
		if bestKey == "" || t.After(bestTime) {
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

			// If replyToSession is set, route the target's response back
			// to the calling session via an injected [SESSION RESPONSE].
			if replyToSession != "" {
				notifyCtx := agent.WithTrigger(ctx, trigger)
				buf := turnevent.NewBufferSink()
				notifyCtx = turnevent.WithSink(notifyCtx, buf)

				if err := getAgent().HandleMessage(notifyCtx, target, []string{message}, nil); err != nil {
					log.Errorf(trigger, "error processing on target %s: %v", target, err)
					return
				}
				resp := buf.FinalText()
				if resp == "" {
					return
				}

				// Route the target session's response through HandleMessage on
				// the calling session. This maintains role alternation and lets
				// the agent process/relay the response.
				formattedResp := "Response from session " + target + ":\n" + resp
				injected := prompts.FormatInjectedMessage("SESSION RESPONSE", time.Now(), formattedResp,
					"[Inter-session response — the target session processed your message and returned this result. Relay the result to the user.]")
				deliverInjectedTurn(getAgent(), ctx, trigger, connMgr, agentID, replyToSession, injected)
				return
			}

			// Otherwise deliver the response to the target session's chat.
			conn := connMgr.ForSessionOrPrimary(target, agentID)

			// Branch sessions without their own facet connection should not
			// deliver replies to chat — they'd leak into the parent's chat.
			// The response still gets written to the branch JSONL via HandleMessage.
			sk, parseErr := session.ParseSessionKey(target)
			isBranchWithoutConn := parseErr == nil && !sk.IsRoot() && connMgr.ForSession(target) == nil

			notifyCtx := agent.WithTrigger(ctx, trigger)
			if conn == nil || isBranchWithoutConn {
				if err := getAgent().HandleMessage(notifyCtx, target, []string{message}, nil); err != nil {
					log.Errorf(trigger, "error: %v", err)
					return
				}
				if isBranchWithoutConn {
					log.Debugf(trigger, "branch session %s has no dedicated connection, skipping platform delivery", target)
				} else {
					log.Warnf(trigger, "no connection for agent %s session %s, response not delivered", agentID, target)
				}
				return
			}

			// Typing indicator is driven by SessionSink on TurnStart /
			// TurnComplete — the Bot.SetTyping implementation on both
			// telegram and discord starts its own 4s refresh ticker
			// internally, so one call at turn start is sufficient.
			sink := turn.NewSessionSink(conn, target, trigger,
				turn.WithSessionSinkErrorHandler(func(t string, err error) {
					log.Errorf(t, "platform delivery: %v", err)
				}))
			notifyCtx = turnevent.WithSink(notifyCtx, sink)

			if err := getAgent().HandleMessage(notifyCtx, target, []string{message}, nil); err != nil {
				log.Errorf(trigger, "error: %v", err)
				return
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
			if conn == nil {
				if err := inst.ag.HandleMessage(notifyCtx, targetSessionKey, []string{message}, nil); err != nil {
					log.Errorf("session_notify", "error for session %s: %v", targetSessionKey, err)
					return
				}
				log.Warnf("session_notify", "no connection for agent %s session %s, response not delivered", targetAgentID, targetSessionKey)
				return
			}

			sink := turn.NewSessionSink(conn, targetSessionKey, "session_notify",
				turn.WithSessionSinkErrorHandler(func(t string, err error) {
					log.Errorf(t, "platform delivery for session %s: %v", targetSessionKey, err)
				}))
			notifyCtx = turnevent.WithSink(notifyCtx, sink)

			if err := inst.ag.HandleMessage(notifyCtx, targetSessionKey, []string{message}, nil); err != nil {
				log.Errorf("session_notify", "error for session %s: %v", targetSessionKey, err)
				return
			}
		}()
	})
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
	if conn == nil {
		if err := ag.HandleMessage(triggerCtx, sessionKey, []string{message}, nil); err != nil {
			log.Errorf(trigger, "error: %v", err)
			return
		}
		log.Warnf(trigger, "no connection for session %s agent %s, response not delivered", sessionKey, agentID)
		return
	}

	// Typing indicator is driven by SessionSink on TurnStart /
	// TurnComplete — the Bot.SetTyping implementation on both telegram
	// and discord starts its own 4s refresh ticker internally, so one
	// call at turn start is sufficient.
	sink := turn.NewSessionSink(conn, sessionKey, trigger,
		turn.WithSessionSinkErrorHandler(func(t string, err error) {
			log.Errorf(t, "platform delivery: %v", err)
		}))
	triggerCtx = turnevent.WithSink(triggerCtx, sink)

	if err := ag.HandleMessage(triggerCtx, sessionKey, []string{message}, nil); err != nil {
		log.Errorf(trigger, "error: %v", err)
		return
	}
}

// buildWakeScheduler creates the agent-scoped wake-scheduling machinery and
// restores any pending wakes from the database. Returns the schedule callback
// for use by NewRemindTool. Returns nil if reminderStore is nil (reminder
// support disabled for this agent).
//
// Transport-independent: call once per agent at setup time. Tool registration
// is the caller's responsibility — register tools.NewRemindTool(reminderStore,
// agentID, wakeScheduleFn) into whichever registry the transport uses.
func buildWakeScheduler(
	getAgent func() *agent.Agent,
	reminderStore *memory.ReminderStore,
	agentID string,
	ctx context.Context,
	connMgr platform.ConnectionManager,
) tools.ScheduleWakeFn {
	if reminderStore == nil {
		return nil
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

	// Restore pending wakes from DB (survives restart).
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

	return wakeScheduleFn
}
