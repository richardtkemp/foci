package telegram

import (
	"context"

	"foci/internal/agent"
	"foci/internal/agent/turnevent"
	"foci/internal/platform"
	"foci/internal/turn"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// commandWorker processes queued slash commands in its own goroutine,
// concurrent with the per-session agent workers in agent.Inbox. Commands
// like /status, /mana, /model are stateless reads or only touch session
// config — there is no good reason to serialise them behind in-flight
// agent turns. Commands that must interrupt a turn (e.g. /stop) are
// marked Immediate and dispatched inline by the polling goroutine
// instead of going through this channel at all.
//
// Concurrency note: commands run on this goroutine while a turn may be
// active in agent.Inbox. State-mutating commands (e.g. /reset, /compact)
// acquire whatever locks they need via cc.Agent — the worker boundary
// itself does not provide synchronisation.
func (b *Bot) commandWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			b.logger().Debugf("commandWorker: ctx done, exiting")
			return
		case cmd := <-b.mq.CmdChan():
			b.logger().Debugf("commandWorker: dequeued command %q", cmd.Text)
			b.processQueuedCommand(ctx, cmd)
		}
	}
}

// agentMessagePump drains the message queue's main channel and hands each
// message to the agent's per-session inbox. The agent's session worker
// (one goroutine per session key) handles batching, in-flight tracking,
// orphan-steer drain, and turn execution via Bot.Drive.
//
// Sessions on the same bot run their turns in parallel — one slow turn on
// session A no longer blocks session B's worker. This pump goroutine just
// fans messages out by session key; the actual concurrency lives in the
// agent's Inbox.
//
// Falls back to inline dispatch if no agent reference is set (test mode
// without an agent — exercises only the receive path).
func (b *Bot) agentMessagePump(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			b.logger().Debugf("agentMessagePump: ctx done, exiting")
			return
		case qm := <-b.mq.Chan():
			b.handoffToAgent(qm)
		}
	}
}

// handoffToAgent converts a platform.QueuedMessage to an agent.Envelope
// and pushes it through the agent's per-session inbox. Each envelope
// carries this Bot as its Driver so the agent's worker can call back
// into the bot for renderer/tracker/sink construction.
func (b *Bot) handoffToAgent(qm platform.QueuedMessage) {
	if b.agentRef == nil {
		b.logger().Debugf("handoffToAgent: no agent ref, dropping message")
		return
	}
	sk := b.sessionKeyForQueuedMessage(qm)
	if sk == "" {
		b.logger().Debugf("handoffToAgent: no session key for chatID=%d", qm.ChatID)
		return
	}
	env := agent.Envelope{
		SessionKey:  sk,
		Text:        qm.Text,
		Attachments: qm.Attachments,
		UserID:      qm.UserID,
		SenderName:  qm.SenderName,
		ChatID:      qm.ChatID,
		IsGroupChat: qm.IsGroupChat,
		ReceivedAt:  qm.ReceivedAt,
		Original:    qm.Original,
		Driver:      b,
	}
	b.agentRef.Enqueue(env)
}

// sessionKeyForQueuedMessage resolves the session key for a queued message
// using the same rules as processAgentMessage used to: secondary bots use
// their override, primary bots derive from chat ID, fallback to bot's
// default session key.
func (b *Bot) sessionKeyForQueuedMessage(qm platform.QueuedMessage) string {
	if b.isSecondary {
		return b.SessionKey()
	}
	if b.agentID != "" {
		return b.sessionKeyForMsg(qm.ChatID)
	}
	return b.SessionKey()
}

// processQueuedCommand dispatches a command that was routed via the command
// channel rather than the polling goroutine. Commands are routed here
// (instead of being dispatched inline in the polling loop) so that blocking
// commands like /reset do not prevent getUpdates from running, which would
// stop callback_query delivery (e.g. permission "Allow" buttons).
func (b *Bot) processQueuedCommand(ctx context.Context, qm platform.QueuedMessage) {
	origMsg, _ := qm.Original.(*gotgbot.Message)
	if origMsg == nil {
		b.logger().Warnf("processQueuedCommand: missing original message")
		return
	}
	if b.dispatcher == nil {
		return
	}
	outcome := b.dispatcher.DispatchCommand(ctx, qm.Text, qm.ChatID, qm.UserID)
	if !outcome.NotHandled {
		b.renderCommandOutcome(origMsg, &outcome)
	}
}

// NewLateDeliverySink implements agent.Driver. Returns a turn.SessionSink
// that the SessionRouter will dispatch to when no per-turn sink is
// registered (i.e. for events that arrive after Drive's defer chain has
// cleared the per-turn StreamingSink). Late deliveries become fresh
// standalone messages in the session's chat — semantically appropriate
// for content that arrives after the previous turn's UI thread is past.
//
// See TODO #745 for the bug this fix addresses (rearmed responses lost
// when ccstream emits text after the per-turn renderer is gone).
func (b *Bot) NewLateDeliverySink(sk string) turnevent.Sink {
	return turn.NewSessionSink(b, sk, "late-delivery",
		turn.WithSessionSinkErrorHandler(func(trigger string, err error) {
			b.logger().Warnf("late-delivery send failed sk=%s trigger=%s: %v", sk, trigger, err)
		}))
}

// NewTurnSink implements agent.Driver. Builds the per-turn rendering glue:
// renderer, tool tracker, StreamingSink. Returns the sink plus a cleanup
// closure (renderer.Cleanup) for the agent to defer.
//
// Returns (nil, nil) when env.Original isn't a *gotgbot.Message — the
// envelope is for a different platform and this Telegram bot can't render
// it. agent.runTurn skips silently in that case.
//
// Part of TODO #746 Stage A — extracts the renderer/tracker/sink wiring
// out of Drive so agent.runTurn can own per-turn pipeline assembly.
func (b *Bot) NewTurnSink(env agent.Envelope) (turnevent.Sink, func()) {
	origMsg, _ := env.Original.(*gotgbot.Message)
	if origMsg == nil {
		return nil, nil
	}
	d := b.resolveDisplay(env.SessionKey)
	tracker := newToolCallTracker(b, env.ChatID, d)
	renderer := newTurnRenderer(b, origMsg, tracker, d)
	sink := turn.NewStreamingSink(renderer, tracker, b)
	return sink, renderer.Cleanup
}

// Connection implements agent.Driver. Returns the bot itself, which
// implements platform.Connection for delivery operations the agent
// initiates (late-delivery sinks, notify flows, platform identification).
func (b *Bot) Connection() platform.Connection {
	return b
}

// Drive implements agent.Driver. Called by the agent's per-session worker
// after batching a turn's worth of envelopes. Owns the platform-specific
// concerns: renderer/tracker construction, sink wiring, cancellable turn
// context (so /stop can cancel), error sanitisation, and lifecycle hooks.
//
// Steerer is supplied by the agent for API-mode mid-turn buffer drain —
// it returns texts queued in this session's inbox steer buffer since the
// last drain, pasted into the user message at the next tool boundary.
//
// router is the session's SessionRouter (TODO #745). Drive registers its
// per-turn StreamingSink with the router at start and clears at end via
// defer; in-turn events flow through the streaming sink, late events
// (post-Drive-exit) flow through the router's late-delivery fallback.
// router may be nil if Drive is invoked outside the agent's session
// worker context (defensive — current call sites always pass non-nil).
func (b *Bot) Drive(ctx context.Context, sk string, batch []agent.Envelope, steerer turnevent.Steerer, router *turnevent.SessionRouter) error {
	if len(batch) == 0 || sk == "" {
		return nil
	}

	b.logger().Debugf("Drive: enter sk=%s batch_size=%d", sk, len(batch))
	b.turnActive.Store(true)

	// Cancellable turn ctx + /stop wiring moved to agent.driveOnce
	// (TODO #746 Stage B). Bot.Drive keeps only platform-domain
	// lifecycle: drainPendingNotifications + OnTurnEnd + OnTurnComplete +
	// turnActive flag for SendNotification buffering.
	defer func() {
		b.turnActive.Store(false)
		b.logger().Debugf("Drive: defer calling drainPendingNotifications sk=%s", sk)
		b.drainPendingNotifications()
		if b.OnTurnEnd != nil {
			b.logger().Debugf("Drive: defer calling OnTurnEnd sk=%s", sk)
			b.OnTurnEnd()
		}
		b.logger().Debugf("Drive: defer complete sk=%s", sk)
	}()

	if b.agentRef == nil {
		b.logger().Warnf("Drive: agentRef nil sk=%s, cannot run turn", sk)
		return nil
	}
	err := b.agentRef.RunTurn(ctx, sk, batch, steerer, router, b)
	if err != nil && ctx.Err() != nil {
		b.logger().Infof("agent turn cancelled")
		return nil // /stop was called, "Stopped." already sent
	}
	if err != nil {
		b.logger().Errorf("agent error: %s", b.sanitizeError(err))
	}

	if b.OnTurnComplete != nil {
		b.logger().Debugf("Drive: calling OnTurnComplete sk=%s", sk)
		b.OnTurnComplete()
	}
	b.logger().Debugf("Drive: exit sk=%s", sk)
	return nil
}

// cancelTurn cancels the in-flight agent turn for THIS bot's primary
// session, if any. Retained for /done compatibility (which uses the
// command's StopFunc to cancel a secondary bot's turn before detaching).
// /stop has migrated to agent.CancelSession(sk) directly — see
// command/admin_session.go and TODO #746 Stage B.
//
// Multi-session bots: cancellation here targets the bot's currently-known
// sk via the bot's own tracking; Agent.CancelSession is the precise per-
// session API.
func (b *Bot) cancelTurn() {
	if b.agentRef == nil {
		return
	}
	sk := b.SessionKey()
	if sk == "" {
		return
	}
	b.logger().Infof("cancelTurn → Agent.CancelSession sk=%s", sk)
	b.agentRef.CancelSession(sk)
}
