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
	"foci/internal/route"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/turn"
	"foci/shared/prompts"
)

// defaultSessionKeyFor resolves an agent's default session through the ONE
// routing oracle (route.Resolver → SessionIndex.DefaultSessionKeyForAgent:
// is_default chat, else most-recently-active root). Every "no explicit
// session named" path in the gateway resolves here so cron sends, restart
// injections, warnings, and status all agree on the destination.
// Returns "" when the agent has no sessions yet.
func defaultSessionKeyFor(ag *agent.Agent, agentID string) string {
	if ag == nil {
		return ""
	}
	r := &route.Resolver{
		Index:             ag.SessionIndex,
		PreferredPlatform: func(string) string { return ag.DefaultPlatform },
	}
	res, err := r.Resolve(route.Target{Agent: agentID})
	if err != nil {
		return ""
	}
	return res.SessionKey
}

// ───────────────────────────────────────────────────────────────────────────
// Injection delivery paths
//
// An "injection" is a turn NOT triggered by a direct inbound platform message —
// a system event (restart/wake/warning), a backgrounded tool result, or a
// cross-session send. Each runs on a session's inbox worker (via Inject.Run) so
// it serialises with that session's platform turns and defers behind a pending
// foci_ask, instead of racing them in a detached goroutine.
//
// Despite the several trigger names, there are only TWO shapes:
//
//  1. DELIVER-TO-CHAT — deliverToSessionChat(): run a turn on a session and
//     render its output to that session's own platform chat. The single shared
//     primitive behind restart changelogs, scheduled wakes, proactive warnings,
//     backgrounded async-tool results, and cross-agent send_to_session. When the
//     session has no live connection of its own, delivery falls back to the
//     agent's primary chat (route.PolicyFallback).
//
//  2. CAPTURE-AND-RELAY — relayResponseToCaller(): the one shape that is NOT a
//     plain delivery. It runs a turn on the TARGET session but CAPTURES its
//     output with a BufferSink instead of delivering it, then relays that text
//     to a DIFFERENT (calling) session. Because it READS the turn's output
//     rather than showing it, it cannot collapse into deliverToSessionChat.
//     Used only by send_to_session with reply_to=caller.
//
// Owner resolution (which Agent owns the target session) differs per caller and
// stays at each call site; the primitives below take an already-resolved Agent.
// ───────────────────────────────────────────────────────────────────────────

// newAsyncNotifier builds the callback fired when a backgrounded tool
// (exec/tmux auto-background) completes and reports its result. The result
// either goes to the target session's own chat (DELIVER-TO-CHAT) or, when
// reply_to names a calling session, is captured and relayed back to it
// (CAPTURE-AND-RELAY). See the path taxonomy above.
//
// getAgent is a lazy getter (the agent is nil at construction time).
// agentResolverFn resolves a session key's agent ID to its owning instance: for
// a cross-agent target the TARGET's Agent must run the turn — running it on the
// caller's Agent would put the foreign session in the wrong
// workdir/backend/permission scope.
func newAsyncNotifier(
	getAgent func() *agent.Agent,
	agentID string,
	agentResolverFn func(agentID string) *agentInstance,
	ctx context.Context,
	connMgr platform.ConnectionManager,
) *tools.AsyncNotifier {
	return tools.NewAsyncNotifier(func(targetSession, message, replyToSession, trigger string) {
		target := targetSession
		if target == "" {
			target = defaultSessionKeyFor(getAgent(), agentID)
		}
		if trigger == "" {
			trigger = "async_notify"
		}

		// Resolve which Agent owns the target session: the caller's Agent
		// (same-agent fast path), or the target's Agent when the key names a
		// different agent. An unknown agent is a hard drop — nowhere to go.
		targetAg, targetAgentID := getAgent(), agentID
		if sk, err := session.ParseSessionKey(target); err == nil && sk.AgentID != agentID {
			inst := agentResolverFn(sk.AgentID)
			if inst == nil {
				log.NewComponentLogger(trigger).Errorf("unknown target agent %q for session %s, message dropped", sk.AgentID, target)
				return
			}
			targetAg, targetAgentID = inst.ag, sk.AgentID
		}
		if targetAg == nil {
			log.NewComponentLogger(trigger).Errorf("no target agent for session %s, injection dropped", target)
			return
		}

		if replyToSession != "" {
			// CAPTURE-AND-RELAY: run on the target, hand its output to the caller.
			relayResponseToCaller(getAgent, ctx, connMgr, agentID, targetAg, target, replyToSession, message, trigger)
			return
		}
		// DELIVER-TO-CHAT: the result belongs to the target's own chat.
		deliverToSessionChat(targetAg, ctx, trigger, connMgr, targetAgentID, target, message)
	})
}

// relayResponseToCaller runs the target session's turn, CAPTURES its output with
// a BufferSink instead of delivering it anywhere, then relays that text to the
// calling session as an injected [SESSION RESPONSE].
//
// This is the one injection shape that cannot be a deliverToSessionChat: every
// other path DELIVERS its turn's output to a platform chat; this one READS the
// output so it can hand it to a different session. Two turns on two sessions — a
// capture on the target, a delivery on the caller — is why it can't collapse
// into a single deliver call. See the path taxonomy above.
func relayResponseToCaller(
	getAgent func() *agent.Agent,
	ctx context.Context,
	connMgr platform.ConnectionManager,
	callerAgentID string,
	targetAg *agent.Agent,
	targetSession, callerSession, message, trigger string,
) {
	enqueueInject(targetAg, targetSession, trigger, func() {
		// Capture the target's output instead of delivering it to a chat.
		buf := turnevent.NewBufferSink()
		notifyCtx := turnevent.WithSink(agent.WithTrigger(ctx, trigger), buf)
		if err := targetAg.HandleMessage(notifyCtx, targetSession, []string{message}, nil); err != nil {
			log.NewComponentLogger(trigger).Errorf("error processing on target %s: %v", targetSession, err)
			return
		}
		resp := buf.FinalText()
		if resp == "" {
			return
		}

		// Relay it to the caller's own chat as a normal injected turn. Running
		// it through HandleMessage on the caller maintains role alternation and
		// lets the agent process/relay it.
		formattedResp := "Response from session " + targetSession + ":\n" + resp
		injected := prompts.FormatInjectedMessage("SESSION RESPONSE", time.Now(), formattedResp,
			"[Inter-session response — the target session processed your message and returned this result. Relay the result to the user.]")
		deliverToSessionChat(getAgent(), ctx, trigger, connMgr, callerAgentID, callerSession, injected)
	})
}

// newSessionNotifyFn builds the callback that injects a message into a target
// session and delivers the response to that session's chat — the DELIVER-TO-CHAT
// path (see the taxonomy above) for send_to_session (trigger "session_notify",
// via=agent) and the ask tool's answer/grader delivery (trigger "ask_grader",
// via=ask-grader). Cross-agent by nature: the target key names its own owning
// agent, resolved here.
func newSessionNotifyFn(
	agentResolverFn func(agentID string) *agentInstance,
	ctx context.Context,
	connMgr platform.ConnectionManager,
	trigger string,
) tools.SessionNotifyFn {
	return tools.SessionNotifyFn(func(targetSessionKey, message string) {
		sk, err := session.ParseSessionKey(targetSessionKey)
		if err != nil {
			log.NewComponentLogger(trigger).Errorf("invalid session key %q: %v", targetSessionKey, err)
			return
		}
		inst := agentResolverFn(sk.AgentID)
		if inst == nil {
			log.NewComponentLogger(trigger).Errorf("unknown agent %q for session %s", sk.AgentID, targetSessionKey)
			return
		}
		deliverToSessionChat(inst.ag, ctx, trigger, connMgr, sk.AgentID, targetSessionKey, message)
	})
}

// turnSinkForConn returns the best turn-event sink for a connection. For app
// connections (which implement agent.Driver), uses the driver's appSink so the
// app receives activity frames (warming → thinking → typing → idle). For
// Telegram/Discord, falls back to SessionSink (drives SetTyping). The returned
// cleanup func (nil for SessionSink) must be deferred.
func turnSinkForConn(conn platform.Connection, sessionKey, trigger string) (turnevent.Sink, func()) {
	if driver, ok := conn.(agent.Driver); ok {
		if sink, cleanup := driver.NewTurnSink(agent.Envelope{SessionKey: sessionKey}); sink != nil {
			return sink, cleanup
		}
	}
	return turn.NewSessionSink(conn, sessionKey, trigger,
		turn.WithSessionSinkErrorHandler(func(t string, err error) {
			log.NewComponentLogger(t).Errorf("platform delivery: %v", err)
		})), nil
}

// enqueueInject queues body as a system injection on sessionKey's inbox worker —
// the ONE place injection queueing/serialisation/safety is defined. body runs
// wrapped in panic recovery (a bad injection must never crash the foci process),
// and a rejected enqueue (full inbox) is logged and dropped. Queueing serialises
// the turn with the session's platform turns and defers it behind a pending
// foci_ask — a system injection waits for an in-flight turn, never steers it.
func enqueueInject(ag *agent.Agent, sessionKey, trigger string, body func()) {
	run := func() {
		defer func() {
			if r := recover(); r != nil {
				log.NewComponentLogger(trigger).Errorf("injection panicked: %v (session=%s)", r, sessionKey)
			}
		}()
		body()
	}
	if !ag.Enqueue(agent.Envelope{
		SessionKey: sessionKey,
		Inject:     &agent.InjectMeta{Trigger: trigger, Run: run},
	}) {
		log.NewComponentLogger(trigger).Warnf("inbox rejected injected turn for session %s", sessionKey)
	}
}

// deliverToSessionChat runs a system-injected turn on sessionKey and renders its
// response to that session's platform chat — the shared DELIVER-TO-CHAT
// primitive (see the path taxonomy above), used by restart changelogs, scheduled
// wakes, proactive warnings, inter-session reply delivery, backgrounded
// async-tool results, and cross-agent send_to_session.
//
// (ag, agentID) is the ALREADY-RESOLVED owner of sessionKey — resolution differs
// per caller and stays at the call site; this only delivers. Uses
// route.PolicyFallback: if the session has no live connection of its own,
// delivery falls back to the agent's primary chat.
func deliverToSessionChat(
	ag *agent.Agent,
	ctx context.Context,
	trigger string,
	connMgr platform.ConnectionManager,
	agentID, sessionKey, message string,
) {
	enqueueInject(ag, sessionKey, trigger, func() {
		conn, outcome := route.ConnFor(connMgr, agentID, sessionKey, route.PolicyFallback)
		notifyCtx := agent.WithTrigger(ctx, trigger)
		if conn == nil {
			// No deliverable connection anywhere. Still run the turn (it lands
			// in the session JSONL); just don't render to a chat.
			if err := ag.HandleMessage(notifyCtx, sessionKey, []string{message}, nil); err != nil {
				log.NewComponentLogger(trigger).Errorf("error for session %s: %v", sessionKey, err)
				return
			}
			log.NewComponentLogger(trigger).Warnf("no connection for agent %s session %s, response not delivered", agentID, sessionKey)
			return
		}
		if outcome == route.DeliveredViaPrimary {
			log.NewComponentLogger(trigger).Infof("session %s has no live connection — delivering via agent %s primary", sessionKey, agentID)
		}

		// turnSinkForConn selects appSink for app connections (activity frames)
		// or SessionSink for TG/Discord (SetTyping).
		sink, cleanup := turnSinkForConn(conn, sessionKey, trigger)
		if cleanup != nil {
			defer cleanup()
		}
		notifyCtx = turnevent.WithSink(notifyCtx, sink)

		if err := ag.HandleMessage(notifyCtx, sessionKey, []string{message}, nil); err != nil {
			log.NewComponentLogger(trigger).Errorf("error for session %s: %v", sessionKey, err)
		}
	})
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
				remindLog.Infof("firing wake id=%d after %v for agent %s: %q", id, delay, agentID, message)
				_ = reminderStore.Dismiss(id)
				// Use the originating session key if stored, otherwise
				// pick the most recently active session.
				sk := sessionKey
				if sk == "" {
					sk = defaultSessionKeyFor(getAgent(), agentID)
				}
				if sk == "" {
					remindLog.Warnf("no session for agent %s, skipping", agentID)
					return
				}
				// deliverToSessionChat queues on the session's inbox worker,
				// which serialises with any in-flight turn on THIS session —
				// no manual in-flight wait needed. A facet key queues on the
				// facet's own inbox, so a turn on another session does not
				// delay the wake (#719).
				deliverToSessionChat(getAgent(), ctx, "scheduled_wake", connMgr, agentID, sk, prompts.FormatInjectedMessage("SCHEDULED WAKE", time.Now(), message))
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
		remindLog.Errorf("failed to load pending wakes for %s: %v", agentID, err)
	} else if len(pending) > 0 {
		for _, r := range pending {
			delay := time.Until(r.DueAt)
			if delay < 0 {
				delay = 0
			}
			_ = wakeScheduleFn(r.ID, delay, r.Text, r.SessionKey)
		}
		remindLog.Infof("restored %d pending wake(s) for agent %s", len(pending), agentID)
	}

	return wakeScheduleFn
}
