package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/memory"
	"foci/internal/platform"
	"foci/internal/route"
	"foci/internal/session"
	"foci/internal/telemetry"
	"foci/internal/tools"
	"foci/internal/turn"
	"foci/internal/turnevent"
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
// Agent-to-agent chits (#1286): a send_to_session injection also carries an
// *tools.AgentOrigin (nil for every other injection — see tools/agent_origin.go
// for why the trigger name cannot discriminate). Both shapes wrap it as an
// agentChit and fire it as the FIRST statement of the enqueued turn body, so
// the app shows the incoming agent message in the target chat (and the relayed
// reply in the caller chat) exactly where the turn it prompted begins — and
// never when the inbox refuses the injection.
//
// Owner resolution (which Agent owns the target session) differs per caller and
// stays at each call site; the primitives below take an already-resolved Agent.
// ───────────────────────────────────────────────────────────────────────────

// chitDeliverer surfaces one agent-to-agent message chit in the app
// conversation bound to sessionKey. Production wiring passes
// app.DeliverAgentMessage (app only — Telegram/Discord get nothing, #1286);
// nil means "no app provider" and shows nothing.
type chitDeliverer func(sessionKey string, msg tools.AgentOrigin)

// agentChit is a pending chit: the agent message to surface plus the deliverer
// that will surface it. A nil *agentChit (every non-agent injection) fires
// nothing.
type agentChit struct {
	msg     tools.AgentOrigin
	deliver chitDeliverer
}

// newAgentChit wraps an agent message as a fireable chit.
func newAgentChit(deliver chitDeliverer, msg tools.AgentOrigin) *agentChit {
	return &agentChit{msg: msg, deliver: deliver}
}

// agentChitFor converts an injection's origin into a chit; a nil origin (not an
// agent-to-agent message) stays nil, so fire sites need no origin check.
func agentChitFor(origin *tools.AgentOrigin, deliver chitDeliverer) *agentChit {
	if origin == nil {
		return nil
	}
	return newAgentChit(deliver, *origin)
}

// fire surfaces the chit in the conversation bound to sessionKey. Nil-safe on
// both the chit and its deliverer (nil deliverer ≈ app provider absent).
func (c *agentChit) fire(sessionKey string) {
	if c == nil || c.deliver == nil {
		return
	}
	c.deliver(sessionKey, c.msg)
}

// newAsyncNotifier builds the callback fired when a backgrounded tool
// (exec/tmux auto-background) completes and reports its result, and when
// send_to_session injects an agent-to-agent message (via InjectAgentMessage —
// the only caller that carries a non-nil origin). The result either goes to
// the target session's own chat (DELIVER-TO-CHAT) or, when reply_to names a
// calling session, is captured and relayed back to it (CAPTURE-AND-RELAY).
// See the path taxonomy above.
//
// getAgent is a lazy getter (the agent is nil at construction time).
// agentResolverFn resolves a session key's agent ID to its owning instance: for
// a cross-agent target the TARGET's Agent must run the turn — running it on the
// caller's Agent would put the foreign session in the wrong
// workdir/backend/permission scope.
// deliverChit surfaces agent-message chits in the app (app.DeliverAgentMessage
// in production); nil shows nothing.
func newAsyncNotifier(
	getAgent func() *agent.Agent,
	agentID string,
	agentResolverFn func(agentID string) *agentInstance,
	ctx context.Context,
	connMgr platform.ConnectionManager,
	deliverChit chitDeliverer,
) *tools.AsyncNotifier {
	// resolve picks the session a notification goes to and the Agent that owns
	// it: the caller's Agent (same-agent fast path), or the target's Agent when
	// the key names a different agent. An unknown agent is a hard drop —
	// nowhere to go (ok false).
	resolve := func(targetSession, trigger string) (target string, targetAg *agent.Agent, targetAgentID string, ok bool) {
		target = targetSession
		if target == "" {
			target = defaultSessionKeyFor(getAgent(), agentID)
		}
		targetAg, targetAgentID = getAgent(), agentID
		if sk, err := session.ParseSessionKey(target); err == nil && sk.AgentID != agentID {
			inst := agentResolverFn(sk.AgentID)
			if inst == nil {
				log.NewComponentLogger(trigger).Errorf("unknown target agent %q for session %s, message dropped", sk.AgentID, target)
				return "", nil, "", false
			}
			targetAg, targetAgentID = inst.ag, sk.AgentID
		}
		if targetAg == nil {
			log.NewComponentLogger(trigger).Errorf("no target agent for session %s, injection dropped", target)
			return "", nil, "", false
		}
		return target, targetAg, targetAgentID, true
	}
	return tools.NewAsyncNotifier(func(targetSession, message, replyToSession, trigger string, origin *tools.AgentOrigin) {
		if trigger == "" {
			trigger = "async_notify"
		}
		target, targetAg, targetAgentID, ok := resolve(targetSession, trigger)
		if !ok {
			return
		}

		if replyToSession != "" {
			// CAPTURE-AND-RELAY: run on the target, hand its output to the caller.
			relayResponseToCaller(getAgent, ctx, connMgr, deliverChit, agentID, targetAg, target, replyToSession, message, trigger, origin)
			return
		}
		// DELIVER-TO-CHAT: the result belongs to the target's own chat.
		deliverToSessionChat(targetAg, ctx, trigger, connMgr, targetAgentID, target, message, agentChitFor(origin, deliverChit), "")
	}).WithDeliverThen(func(targetSession, message, trigger string, done func(bool)) {
		// DELIVER-TO-CHAT that reports whether the turn took the message
		// (InjectToAgentThen): a spawn result stays persisted until it did
		// (#2589). Refused — by the shutdown drain or a full inbox — it stays
		// for the next process.
		if trigger == "" {
			trigger = "async_notify"
		}
		target, targetAg, targetAgentID, ok := resolve(targetSession, trigger)
		if !ok {
			return
		}
		var once sync.Once
		finish := func(delivered bool) { once.Do(func() { done(delivered) }) }
		deliverToSessionChatThen(targetAg, ctx, trigger, connMgr, targetAgentID, target, message, nil, "",
			func(err error) { finish(!errors.Is(err, agent.ErrShuttingDown)) },
			onInjectRefused(func() { finish(false) }))
	})
}

// relayResponseToCaller runs the target session's turn, CAPTURES its output with
// a BufferSink instead of delivering it anywhere, then relays that text to the
// calling session as an injected [SESSION RESPONSE]. Used only by
// send_to_session with reply_to=caller.
//
// This is the one injection shape that cannot be a deliverToSessionChat: every
// other path DELIVERS its turn's output to a platform chat; this one READS the
// output so it can hand it to a different session. Two turns on two sessions — a
// capture on the target, a delivery on the caller — is why it can't collapse
// into a single deliver call. See the path taxonomy above.
//
// When origin names an agent-to-agent message, the target chat gets one chit
// with it — fired at the start of the target's injected turn. The target's own
// reply stays invisible in the target chat (#480): the capture sink is exactly
// what keeps it out.
func relayResponseToCaller(
	getAgent func() *agent.Agent,
	ctx context.Context,
	connMgr platform.ConnectionManager,
	deliverChit chitDeliverer,
	callerAgentID string,
	targetAg *agent.Agent,
	targetSession, callerSession, message, trigger string,
	origin *tools.AgentOrigin,
) {
	enqueueInject(targetAg, targetSession, trigger, "", func() {
		// The chit precedes the turn it prompted: a busy target shows it after
		// the in-flight turn's output, directly before the reply it triggers.
		agentChitFor(origin, deliverChit).fire(targetSession)
		// Capture the target's output instead of delivering it to a chat.
		buf := turnevent.NewBufferSink()
		notifyCtx := turnevent.WithSink(agent.WithTrigger(ctx, trigger), buf)
		if err := targetAg.HandleMessage(notifyCtx, targetSession, []string{message}, nil); err != nil {
			logInjectionError(trigger, err, "error processing on target %s", targetSession)
			return
		}
		resp := buf.FinalText()
		if resp == "" {
			return
		}
		relayCapturedResponse(getAgent, ctx, connMgr, deliverChit, callerAgentID, callerSession, targetSession, resp, trigger)
	})
}

// relayCapturedResponse relays a captured target reply (resp) to the caller's
// own chat as a normal injected turn — running it through HandleMessage on the
// caller maintains role alternation and lets the agent process/relay it. The
// caller chat also gets one chit with the reply itself (text stripped of the
// silencing sentinels, sender = the target session), fired at the start of the
// caller's relay turn; a silent reply ([[NO_RESPONSE]]) or an empty one gets no
// chit — there is nothing worth showing — while the relay itself runs
// unchanged. Only StripSilencingSuffix is applied (not StripSpuriousPrefix):
// #1286 rules the chit text by that one transformation.
func relayCapturedResponse(
	getAgent func() *agent.Agent,
	ctx context.Context,
	connMgr platform.ConnectionManager,
	deliverChit chitDeliverer,
	callerAgentID, callerSession, targetSession, resp, trigger string,
) {
	formattedResp := "Response from session " + targetSession + ":\n" + resp
	injected := prompts.FormatInjectedMessage("SESSION RESPONSE", time.Now(), formattedResp,
		"[Inter-session response — the target session processed your message and returned this result. Relay the result to the user.]")
	// The caller's relay turn descends from the target turn that just
	// answered (its last completed turn) — the reverse edge of the link
	// send_to_session staged on the way out.
	telemetry.LinkPending(callerSession, targetSession)
	var replyChit *agentChit
	if !platform.IsSilent(resp) {
		replyChit = newAgentChit(deliverChit, tools.NewAgentOrigin(targetSession, platform.StripSilencingSuffix(resp)))
	}
	deliverToSessionChat(getAgent(), ctx, trigger, connMgr, callerAgentID, callerSession, injected, replyChit, "")
}

// newSessionNotifyFn builds the callback that injects a message into a target
// session and delivers the response to that session's chat — the DELIVER-TO-CHAT
// path (see the taxonomy above) for send_to_session with reply_to=session
// (trigger "session_notify", via=agent). Cross-agent by nature: the target key
// names its own owning agent, resolved here. deliverChit surfaces agent-message
// chits in the app (app.DeliverAgentMessage in production); nil shows nothing.
// (The ask tool's deliveries are the sibling newAskDeliverFn /
// newAskUserDeliverFn, which pass a nil origin so asks stay chit-free.)
func newSessionNotifyFn(
	agentResolverFn func(agentID string) *agentInstance,
	ctx context.Context,
	connMgr platform.ConnectionManager,
	trigger string,
	deliverChit chitDeliverer,
) tools.SessionNotifyFn {
	return tools.SessionNotifyFn(func(targetSessionKey, message string, origin *tools.AgentOrigin) {
		deliverToResolvedSession(agentResolverFn, ctx, connMgr, trigger, targetSessionKey, "", message, agentChitFor(origin, deliverChit))
	})
}

// newAskDeliverFn is newSessionNotifyFn for the ask tool's SYSTEM deliveries:
// expiry and restore notices (the user did not cause them, #2233). Identical
// resolution and delivery, but it tags the injection with the requestID of the
// ask that produced the message, and never a chit — an ask notice is not an
// agent-to-agent message. The requestID tag is what lets the inbox tell a
// verdict for the ask that just resolved from a proactive interruption, so it
// is no longer held behind the NEXT ask (#1712).
func newAskDeliverFn(
	agentResolverFn func(agentID string) *agentInstance,
	ctx context.Context,
	connMgr platform.ConnectionManager,
) tools.AskDeliverFn {
	return func(targetSessionKey, requestID, message string) {
		deliverToResolvedSession(agentResolverFn, ctx, connMgr, "ask_grader", targetSessionKey, requestID, message, nil)
	}
}

// newAskUserDeliverFn is the user-input delivery route for the ask tool's
// answer messages (#2233): full and partial answer batches (raw or graded),
// late answers, and the user-cancel notice. Same resolution and delivery as
// newAskDeliverFn, but the ctx carries the user-input marker so the injection
// is dispatched like a typed message — it steers a running turn, is never held
// behind background work, and never answers a backend prompt — while the model
// still sees via=ask-grader and the request id tags the injection (#1712).
// Never a chit, like the system route.
func newAskUserDeliverFn(
	agentResolverFn func(agentID string) *agentInstance,
	ctx context.Context,
	connMgr platform.ConnectionManager,
) tools.AskDeliverFn {
	return func(targetSessionKey, requestID, message string) {
		deliverToResolvedSession(agentResolverFn, agent.WithUserInput(ctx), connMgr, "ask_grader", targetSessionKey, requestID, message, nil)
	}
}

// deliverToResolvedSession resolves targetSessionKey's owning agent and delivers
// message into its chat as an injected turn. Shared by newSessionNotifyFn and
// newAskDeliverFn so the two cannot drift. chit (nil for ask deliveries) fires
// at the start of the injected turn.
func deliverToResolvedSession(
	agentResolverFn func(agentID string) *agentInstance,
	ctx context.Context,
	connMgr platform.ConnectionManager,
	trigger, targetSessionKey, askReqID, message string,
	chit *agentChit,
) {
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
	deliverToSessionChat(inst.ag, ctx, trigger, connMgr, sk.AgentID, targetSessionKey, message, chit, askReqID)
}

// turnSinkForConn returns the best turn-event sink for a connection. For app
// connections (which implement agent.Driver), uses the driver's appSink so the
// app receives activity frames (warming → thinking → typing → idle). For
// Telegram/Discord, falls back to SessionSink (drives SetTyping). The returned
// cleanup func (nil for SessionSink) must be deferred.
//
// BOTH branches are wrapped in WrapConversationLogging. Injected turns never
// reach Agent.RunTurn, which is where platform turns pick up that wrapper, so
// without it their replies deliver to the user and are never written to
// conversation.db (#1784). Wrapping here rather than at the call sites keeps
// the two current callers — asyncDispatch (http.go) and deliverToSessionChat —
// and any future one from having to remember.
func turnSinkForConn(ag *agent.Agent, conn platform.Connection, sessionKey, trigger string) (turnevent.Sink, func()) {
	sink, cleanup := rawTurnSinkForConn(conn, sessionKey, trigger)
	return ag.WrapConversationLogging(sink, sessionKey), cleanup
}

// rawTurnSinkForConn is turnSinkForConn's sink ladder WITHOUT the
// conversation-logging wrapper: the app driver's sink for app connections
// (activity frames), else a SessionSink (typing indicator + per-block
// delivery). The httpDriver hands its result to Agent.RunTurn, which adds
// newLoggingSink itself with the batch's real TurnMetadata — a pre-wrapped
// sink would log every TextBlock twice (#1318).
func rawTurnSinkForConn(conn platform.Connection, sessionKey, trigger string) (turnevent.Sink, func()) {
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

// logInjectionError logs a failed HandleMessage from an injected turn (system
// notifications, async tool results, session relays) at the severity the
// failure actually warrants. An err wrapping delegator.ErrBackendClosed means
// the inject raced the backend's own teardown (e.g. an async_notify landing
// the instant ccstream closes a stale subprocess, #1442) — DelegatedManager
// respawns the backend on the very next Get, so this is transient and
// self-healing, not a real delivery failure, and logging it at ERROR trips
// novel-error alarms for noise. Genuine failures (composition errors, tool
// panics, permanent backend faults) still log at ERROR.
func logInjectionError(trigger string, err error, format string, args ...interface{}) {
	logger := log.NewComponentLogger(trigger)
	if errors.Is(err, agent.ErrShuttingDown) {
		// Refused, not failed: the turn never began (#2059).
		logger.Infof(format+" (not started: shutting down)", args...)
		return
	}
	if errors.Is(err, delegator.ErrBackendClosed) {
		logger.Warnf(format+" (backend closed mid-inject — self-heals on next turn): %v", append(args, err)...)
		return
	}
	logger.Errorf(format+": %v", append(args, err)...)
}

// injectOption adjusts the injection envelope enqueueInject builds.
type injectOption func(*agent.Envelope)

// asUserInput marks the injection as the user's own input (an ask answer,
// #2233): Envelope.Text carries the message so the steer path can fold it, and
// InjectMeta.UserInput routes it through the user gates and timing.
func asUserInput(message string) injectOption {
	return func(env *agent.Envelope) {
		env.Text = message
		env.Inject.UserInput = true
	}
}

// onInjectRefused arranges fn to fire when the inbox declines to run the
// injection — a full inbox at enqueue, or a shutdown/ctx-end drop (#2059) —
// so a caller holding runtime state on the assumption the turn would run can
// undo it (a wake's fired overlay, #2233).
func onInjectRefused(fn func()) injectOption {
	return func(env *agent.Envelope) {
		env.Inject.Refused = fn
	}
}

// enqueueInject queues body as a system injection on sessionKey's inbox worker —
// the ONE place injection queueing/serialisation/safety is defined. body runs
// wrapped in panic recovery (a bad injection must never crash the foci process),
// and a rejected enqueue (full inbox) is logged, the refusal hook fired, and the
// injection dropped. Queueing serialises the turn with the session's platform
// turns and defers it behind a pending foci_ask — a system injection waits for
// an in-flight turn, never steers it. opts adjust the envelope (asUserInput,
// onInjectRefused).
func enqueueInject(ag *agent.Agent, sessionKey, trigger, askReqID string, body func(), opts ...injectOption) {
	run := func() {
		defer func() {
			if r := recover(); r != nil {
				log.NewComponentLogger(trigger).Errorf("injection panicked: %v (session=%s)", r, sessionKey)
			}
		}()
		body()
	}
	env := agent.Envelope{
		SessionKey: sessionKey,
		Inject:     &agent.InjectMeta{Trigger: trigger, Run: run, AskReqID: askReqID},
	}
	for _, opt := range opts {
		opt(&env)
	}
	if !ag.Enqueue(env) {
		log.NewComponentLogger(trigger).Warnf("inbox rejected injected turn for session %s", sessionKey)
		if env.Inject.Refused != nil {
			env.Inject.Refused()
		}
	}
}

// deliverToSessionChat runs a system-injected turn on sessionKey and renders its
// response to that session's platform chat — the shared DELIVER-TO-CHAT
// primitive (see the path taxonomy above), used by restart changelogs, scheduled
// wakes, proactive warnings, ask-answer deliveries, backgrounded async-tool
// results, and cross-agent send_to_session.
//
// (ag, agentID) is the ALREADY-RESOLVED owner of sessionKey — resolution differs
// per caller and stays at the call site; this only delivers. Uses
// route.PolicyFallback: if the session has no live connection of its own,
// delivery falls back to the agent's primary chat. That fallback governs only
// the turn's OUTPUT — a chit still goes to the session's own bound conversation
// (see agentChit).
func deliverToSessionChat(
	ag *agent.Agent,
	ctx context.Context,
	trigger string,
	connMgr platform.ConnectionManager,
	agentID, sessionKey, message string,
	chit *agentChit,
	askReqID string,
) {
	deliverToSessionChatThen(ag, ctx, trigger, connMgr, agentID, sessionKey, message, chit, askReqID, nil)
}

// deliverToSessionChatThen is deliverToSessionChat with a completion hook:
// after (when non-nil) is called with HandleMessage's result once the injected
// turn has run. It is NOT called when the inbox never runs the injection
// (shutdown drain, full inbox), so a caller that consumes a durable record in
// after — a scheduled wake dismissing its row — leaves that record pending; the
// same refusal also leaves chit unfired.
//
// When ctx carries the user-input marker (an ask answer, #2233) the injection is
// marked as the user's own input: it takes the user path and timing — steers a
// running turn, never held behind background work — and Envelope.Text carries
// the message so the steer path can fold it. opts pass further injectOptions
// through to enqueueInject.
func deliverToSessionChatThen(
	ag *agent.Agent,
	ctx context.Context,
	trigger string,
	connMgr platform.ConnectionManager,
	agentID, sessionKey, message string,
	chit *agentChit,
	askReqID string,
	after func(err error),
	opts ...injectOption,
) {
	if agent.UserInputFromContext(ctx) {
		opts = append([]injectOption{asUserInput(message)}, opts...)
	}
	enqueueInject(ag, sessionKey, trigger, askReqID, func() {
		// The chit (if any) fires with the turn it precedes, never at enqueue
		// time — an injection the inbox refuses shows nothing (#1286).
		chit.fire(sessionKey)
		var err error
		if after != nil {
			defer func() { after(err) }()
		}
		conn, outcome := route.ConnFor(connMgr, agentID, sessionKey, route.PolicyFallback)
		notifyCtx := agent.WithTrigger(ctx, trigger)
		if conn == nil {
			// No deliverable connection anywhere. Still run the turn (it lands
			// in the session JSONL); just don't render to a chat.
			if err = ag.HandleMessage(notifyCtx, sessionKey, []string{message}, nil); err != nil {
				logInjectionError(trigger, err, "error for session %s", sessionKey)
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
		sink, cleanup := turnSinkForConn(ag, conn, sessionKey, trigger)
		if cleanup != nil {
			defer cleanup()
		}
		notifyCtx = turnevent.WithSink(notifyCtx, sink)

		if err = ag.HandleMessage(notifyCtx, sessionKey, []string{message}, nil); err != nil {
			logInjectionError(trigger, err, "error for session %s", sessionKey)
		}
	}, opts...)
}

// wakeTurnDone returns the completion hook for wake id's turn: it dismisses the
// wake's row, unless the turn was refused because foci is shutting down — then
// the row stays pending and the next process re-fires it (#2059), and the fired
// overlay goes too (the next process re-marks it when its timer fires).
func wakeTurnDone(store *memory.ReminderStore, id int64) func(error) {
	return func(err error) {
		if errors.Is(err, agent.ErrShuttingDown) {
			store.ClearWakeFired(id)
			remindLog.Infof("wake id=%d not run (shutting down) — left pending for restart", id)
			return
		}
		_ = store.Dismiss(id)
	}
}

// buildWakeScheduler creates the agent-scoped wake-scheduling machinery and
// restores any pending wakes from the database. Returns the schedule and
// cancel callbacks for use by NewRemindTool. Both are nil if reminderStore is
// nil (reminder support disabled for this agent).
//
// Transport-independent: call once per agent at setup time. Tool registration
// is the caller's responsibility — register tools.NewRemindTool(reminderStore,
// agentID, wakeScheduleFn, wakeCancelFn) into whichever registry the transport
// uses.
func buildWakeScheduler(
	getAgent func() *agent.Agent,
	reminderStore *memory.ReminderStore,
	agentID string,
	ctx context.Context,
	connMgr platform.ConnectionManager,
) (tools.ScheduleWakeFn, tools.CancelWakeFn) {
	if reminderStore == nil {
		return nil, nil
	}

	// wakes holds the cancel func of every wake whose timer is still live. The
	// entry is the wake's claim: whichever of cancel and fire removes it (under
	// wakesMu) owns the wake, so a cancel that reports true has really stopped
	// it, and a wake that fires is no longer cancellable (#2036).
	var wakesMu sync.Mutex
	wakes := make(map[int64]context.CancelFunc)

	// take removes the wake's map entry and hands back its cancel func — the
	// claim primitive the cancel path routes through (see wakes).
	take := func(id int64) (context.CancelFunc, bool) {
		wakesMu.Lock()
		defer wakesMu.Unlock()
		cancel, ok := wakes[id]
		delete(wakes, id)
		return cancel, ok
	}

	// claimForFire claims the wake for firing AND marks it fired in the
	// store's overlay under the same hold (#2233): from the moment the timer
	// fires until the turn runs, the remind tool reports the wake as fired
	// rather than overdue, and a cancel can never observe the gap between "no
	// live timer" and "not fired".
	claimForFire := func(id int64, sk string) bool {
		wakesMu.Lock()
		defer wakesMu.Unlock()
		if _, ok := wakes[id]; !ok {
			return false
		}
		delete(wakes, id)
		reminderStore.MarkWakeFired(id, sk)
		return true
	}

	wakeScheduleFn := func(id int64, delay time.Duration, message, sessionKey string) error {
		wakeCtx, wakeCancel := context.WithCancel(context.Background())
		// Register before the goroutine starts: a zero-delay (restored,
		// overdue) wake can fire at once, and its claim must find the entry.
		wakesMu.Lock()
		wakes[id] = wakeCancel
		wakesMu.Unlock()
		go func() {
			select {
			case <-time.After(delay):
				// Resolve the destination session FIRST so the fired overlay is
				// stamped with the key the hold predicate will be asked for.
				sk := sessionKey
				if sk == "" {
					sk = defaultSessionKeyFor(getAgent(), agentID)
				}
				if sk == "" {
					// Nowhere to fire: claim anyway (a cancel racing this must
					// lose, #2036) and dismiss — the row and the claim go
					// together, no turn will run.
					if claimForFire(id, "") {
						_ = reminderStore.Dismiss(id)
					}
					remindLog.Warnf("no session for agent %s, skipping wake id=%d", agentID, id)
					return
				}
				if !claimForFire(id, sk) {
					// A cancel won the claim while the timer fired; it
					// cancelled wakeCtx, which this select no longer watches.
					_ = reminderStore.Dismiss(id)
					return
				}
				wakeCancel()
				remindLog.Infof("firing wake id=%d after %v for agent %s: %q", id, delay, agentID, message)
				// deliverToSessionChat queues on the session's inbox worker,
				// which serialises with any in-flight turn on THIS session —
				// no manual in-flight wait needed. A facet key queues on the
				// facet's own inbox, so a turn on another session does not
				// delay the wake (#719).
				//
				// The row is dismissed only once the wake's turn has actually
				// run (#2059); until then the fired overlay reports the wake as
				// fired (held behind background work or waiting to run, #2233),
				// and the refusal hook below clears the overlay when the inbox
				// declines to run the injection at all.
				deliverToSessionChatThen(getAgent(), ctx, "scheduled_wake", connMgr, agentID, sk,
					prompts.FormatInjectedMessage("SCHEDULED WAKE", time.Now(), message), nil, "",
					wakeTurnDone(reminderStore, id),
					onInjectRefused(func() { reminderStore.ClearWakeFired(id) }))
			case <-wakeCtx.Done():
				// wakeCancelFn already claimed the entry.
				_ = reminderStore.Dismiss(id)
			}
		}()
		return nil
	}

	// wakeCancelFn stops a pending wake. It claims the map entry itself, so the
	// wake is uncancellable again the moment this returns; cancelling the
	// context then makes the wake goroutine take its Done branch, which
	// dismisses the DB row — so the row cleanup stays in one place.
	wakeCancelFn := func(id int64) bool {
		cancel, ok := take(id)
		if !ok {
			return false
		}
		cancel()
		return true
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

	return wakeScheduleFn, wakeCancelFn
}
