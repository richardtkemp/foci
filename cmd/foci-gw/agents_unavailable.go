package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"foci/internal/agent"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/usagehold"
	"foci/shared/prompts"
)

// Telling a send_to_session caller that its target cannot process the message
// (#2892, Dick 2026-11-06). The failures that count: a usage hold, a dead
// backend, a queue that never drains. A slow turn does not. A held message is
// told at once and still delivered.
//
// Each send has one callerWatch, from dispatch to the end of the target's turn.
// The arms:
//   - known at send: send_to_session's tool result says so (CheckTarget), which
//     sets AgentOrigin.CallerTold, and no later turn repeats it;
//   - unavailable after the send: the watch's poll sees it before the turn starts;
//   - refused by the inbox, or failed or stopped mid-turn: turnEnded and refused;
//   - never drains: the poll sees no target activity for callerStallAfter.
//
// The usual reply is not touched: a turn that worked tells nothing.

// callerStallAfter is how long a target may show no activity before the caller
// is told its message is not moving. A turn that emits stream events is slow,
// not stuck, and never reaches this. A single silent tool call this long does,
// so the notice says that it may be one.
const callerStallAfter = 30 * time.Minute

// callerWatchPoll is how often a watch re-checks its target.
const callerWatchPoll = 5 * time.Second

// callerWatchSeams holds the two times above and the read of a target's last
// activity; tests replace them.
var callerWatchSeams = struct {
	mu          sync.Mutex
	poll, stall time.Duration
	activity    func(ag *agent.Agent, sk string) time.Time
}{poll: callerWatchPoll, stall: callerStallAfter, activity: func(ag *agent.Agent, sk string) time.Time { return ag.SessionLastActivity(sk) }}

func callerWatchTiming() (poll, stall time.Duration) {
	callerWatchSeams.mu.Lock()
	defer callerWatchSeams.mu.Unlock()
	return callerWatchSeams.poll, callerWatchSeams.stall
}

func targetActivity(ag *agent.Agent, sk string) time.Time {
	callerWatchSeams.mu.Lock()
	read := callerWatchSeams.activity
	callerWatchSeams.mu.Unlock()
	return read(ag, sk)
}

// callerWatch follows one agent message on behalf of the session that sent it.
type callerWatch struct {
	ctx           context.Context
	connMgr       platform.ConnectionManager
	trigger       string
	callerAg      *agent.Agent
	callerAgentID string
	caller        string
	targetAg      *agent.Agent
	target        string
	replyToCaller bool
	heldTold      bool

	startedAt time.Time
	doneOnce  sync.Once
	done      chan struct{}
	startOnce sync.Once
	started   chan struct{}
}

// newCallerWatch builds a watch. Run it with go w.run() when the send is live;
// a restored held message skips that, its caller having been told already.
func newCallerWatch(ctx context.Context, connMgr platform.ConnectionManager, trigger string, callerAg *agent.Agent, callerAgentID, caller string, targetAg *agent.Agent, target string, replyToCaller, heldTold bool) *callerWatch {
	return &callerWatch{
		ctx: ctx, connMgr: connMgr, trigger: trigger,
		callerAg: callerAg, callerAgentID: callerAgentID, caller: caller,
		targetAg: targetAg, target: target, replyToCaller: replyToCaller, heldTold: heldTold,
		startedAt: time.Now(), done: make(chan struct{}), started: make(chan struct{}),
	}
}

// run polls until the turn ends or there is nothing left to tell.
func (w *callerWatch) run() {
	poll, stall := callerWatchTiming()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	quietSince := w.startedAt
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-w.done:
			return
		case <-ticker.C:
		}
		now := time.Now()
		if reason, unavailable := w.targetAg.Unavailable(); unavailable {
			// A wait with its own cause is not a stall, however long it lasts.
			quietSince = now
			if !w.heldTold && !w.hasStarted() {
				w.heldTold = true
				// A caller on the same hold cannot act now, and by the time a
				// notice could run the reply is on its way.
				if _, callerHeld := w.callerAg.Unavailable(); !callerHeld {
					w.tell(tools.UnavailableHeldNote(w.target, reason, w.replyToCaller))
				}
			}
			continue
		}
		last := targetActivity(w.targetAg, w.target)
		if w.hasStarted() && last.IsZero() {
			// A running turn with no activity to read (an API agent) cannot be
			// told apart from a slow one.
			continue
		}
		if last.After(quietSince) {
			quietSince = last
		}
		if now.Sub(quietSince) >= stall {
			w.tell(fmt.Sprintf("Session %s has shown no activity for %s and has not answered your message. It may be stuck, or running one very long command. Your message is still queued and will be processed if the session recovers. You will not be told again.", w.target, stall.Round(time.Minute)))
			return
		}
	}
}

func (w *callerWatch) hasStarted() bool {
	select {
	case <-w.started:
		return true
	default:
		return false
	}
}

func (w *callerWatch) finish() { w.doneOnce.Do(func() { close(w.done) }) }

// options are the inject options every watched send takes: the start mark, and
// the refusal arm.
func (w *callerWatch) options() []injectOption {
	return []injectOption{
		func(env *agent.Envelope) {
			run := env.Inject.Run
			env.Inject.Run = func() {
				w.startOnce.Do(func() { close(w.started) })
				run()
			}
		},
		onInjectRefused(w.refused),
	}
}

// refused is the inbox declining the message. The record of a message a hold
// parked survives the shutdown that refuses it, so there is nothing to tell.
func (w *callerWatch) refused() {
	w.finish()
	if w.targetAg.ShuttingDown() && w.targetAg.UsageHold().Phase != usagehold.Open {
		return
	}
	w.tell(fmt.Sprintf("Session %s did not take your message, so it was not delivered: its inbox refused it (full, or foci is shutting down). If it still matters, send it again.", w.target))
}

// turnEnded classifies the end of the target's turn. hasReply is false when a
// turn that should capture a reply captured nothing.
func (w *callerWatch) turnEnded(err error, hasReply bool) {
	var limited *agent.RateLimitedError
	switch {
	case err == nil && hasReply:
		w.finish()
	case errors.Is(err, agent.ErrShuttingDown):
		w.refused()
	case errors.As(err, &limited):
		w.finish()
		w.tell(fmt.Sprintf("Session %s cannot reply now: %s. Your message is queued and runs when the limit resets. The reply will not come to you.", w.target, limited.Error()))
	default:
		w.finish()
		if reason, unavailable := w.targetAg.Unavailable(); unavailable {
			w.tell(fmt.Sprintf("Session %s stopped working on your message: %s. It carries on when it is back. The reply then goes to its own chat, not to you.", w.target, reason))
			return
		}
		if err != nil {
			w.tell(fmt.Sprintf("Session %s could not process your message, so it was not delivered: %v. If it still matters, send it again.", w.target, err))
		}
	}
}

// tell runs one notice turn on the caller. Its own hold or a full inbox can
// delay or refuse it: delivery is the same as any other injected turn.
func (w *callerWatch) tell(body string) {
	log.NewComponentLogger(w.trigger).Infof("telling %s that %s is unavailable", w.caller, w.target)
	injected := prompts.FormatInjectedMessage("SESSION UNAVAILABLE", time.Now(), body,
		"[Foci notice about a message you sent to another session. It is not from your human, and your human has not seen it. Tell your human only if it changes what they are waiting for.]")
	deliverToSessionChat(w.callerAg, w.ctx, w.trigger, w.connMgr, w.callerAgentID, w.caller, injected, nil, "")
}

// durableSessionRelay is the DurableInject kind of a reply_to=caller message:
// a hold that parks it keeps it across a restart, and the restored copy still
// takes its reply to the caller.
const durableSessionRelay = "session_relay"

// relayDurable is the serialisable form of a reply_to=caller message.
func relayDurable(trigger, message, caller string, origin *tools.AgentOrigin) *agent.DurableInject {
	args := map[string]string{"caller_session": caller}
	if origin != nil {
		args["origin_session"] = origin.SessionKey
		args["origin_text"] = origin.Text
	}
	return &agent.DurableInject{Kind: durableSessionRelay, Trigger: trigger, Text: message, Args: args}
}

// withDurable gives an injection its serialisable form.
func withDurable(d *agent.DurableInject) injectOption {
	return func(env *agent.Envelope) { env.Inject.Durable = d }
}

// restoreHeldRelay re-sends a held reply_to=caller message after a restart. A
// caller that no longer exists gets nothing, so the reply goes to the target's
// own chat instead of being lost.
func restoreHeldRelay(ag *agent.Agent, ctx context.Context, connMgr platform.ConnectionManager, agentID string, resolver func(string) *agentInstance, deliverChit chitDeliverer, sk string, d agent.DurableInject, id string) {
	caller := d.Args["caller_session"]
	var callerInst *agentInstance
	callerSK, err := session.ParseSessionKey(caller)
	if err == nil && resolver != nil {
		callerInst = resolver(callerSK.AgentID)
	}
	if callerInst == nil {
		log.NewComponentLogger(d.Trigger).Warnf("held message on %s: caller %s is gone, the reply goes to the session's own chat", sk, caller)
		deliverToSessionChatThen(ag, ctx, d.Trigger, connMgr, agentID, sk, d.Text, nil, "", nil, withUsageHeldID(id))
		return
	}
	var origin *tools.AgentOrigin
	if d.Args["origin_session"] != "" {
		o := tools.NewAgentOrigin(d.Args["origin_session"], d.Args["origin_text"])
		origin = &o
	}
	w := newCallerWatch(ctx, connMgr, d.Trigger, callerInst.ag, callerSK.AgentID, caller, ag, sk, true, true)
	relayResponseToCaller(func() *agent.Agent { return callerInst.ag }, ctx, connMgr, deliverChit, callerSK.AgentID, ag, sk, caller, d.Text, d.Trigger, origin, w, withUsageHeldID(id))
}
