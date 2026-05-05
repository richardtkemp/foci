package discord

import (
	"context"
	"strconv"

	"foci/internal/agent"
	"foci/internal/agent/turnevent"
	"foci/internal/platform"
	"foci/internal/turn"

	"github.com/bwmarrin/discordgo"
)

// agentMessagePump drains the message queue's main channel and hands each
// message to the agent's per-session inbox. Discord prioritises commands
// over agent turns: pending commands are drained before each main-channel
// dequeue, preventing long turns from starving slash commands. The agent's
// per-session inbox handles batching, in-flight tracking, orphan-steer
// drain, and turn execution via Bot.Drive.
//
// One pump goroutine per bot fans out to per-session workers in the agent's
// Inbox — sessions on the same bot run their turns in parallel.
//
// Falls back to inline drop if no agent reference is set (test mode).
func (b *Bot) agentMessagePump(ctx context.Context) {
	for {
		// Priority: drain any pending commands before pulling the next
		// agent message. Commands run on the same goroutine as the pump
		// (matching pre-Phase-6 discord behaviour); turns proceed via
		// the agent's per-session workers, not on this goroutine.
		select {
		case cmd := <-b.mq.CmdChan():
			b.logger().Debugf("pump: dequeued command %q", cmd.Text)
			b.processQueuedCommand(ctx, cmd)
			continue
		default:
		}

		select {
		case <-ctx.Done():
			return
		case cmd := <-b.mq.CmdChan():
			b.logger().Debugf("pump: dequeued command %q", cmd.Text)
			b.processQueuedCommand(ctx, cmd)
		case qm := <-b.mq.Chan():
			b.handoffToAgent(qm)
		}
	}
}

// handoffToAgent converts a platform.QueuedMessage to an agent.Envelope
// and pushes it through the agent's per-session inbox. Each envelope
// carries this Bot as its Driver so the agent's worker can call back
// into Bot.Drive for renderer/tracker/sink construction.
func (b *Bot) handoffToAgent(qm platform.QueuedMessage) {
	if b.agentRef == nil {
		b.logger().Debugf("handoffToAgent: no agent ref, dropping message")
		return
	}
	sk := b.sessionKeyForQueuedMessage(qm)
	if sk == "" {
		b.logger().Debugf("handoffToAgent: no session key for channelID=%d", qm.ChatID)
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

// sessionKeyForQueuedMessage resolves the session key using the same rules
// as processAgentMessage used to: secondary bots use their override,
// primary bots derive from channel ID, fallback to bot's default.
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
// channel. See telegram/bot_worker.go for the rationale.
func (b *Bot) processQueuedCommand(ctx context.Context, qm platform.QueuedMessage) {
	origMsg, _ := qm.Original.(*discordgo.Message)
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
// for late text deliveries that arrive after a Drive call's defer chain has
// cleared the per-turn StreamingSink. See TODO #745.
func (b *Bot) NewLateDeliverySink(sk string) turnevent.Sink {
	return turn.NewSessionSink(b, sk, "late-delivery",
		turn.WithSessionSinkErrorHandler(func(trigger string, err error) {
			b.logger().Warnf("late-delivery send failed sk=%s trigger=%s: %v", sk, trigger, err)
		}))
}

// NewTurnSink implements agent.Driver. Builds the per-turn rendering glue
// (renderer + tracker + StreamingSink) from env. Returns (nil, nil) if
// env.Original isn't a *discordgo.Message. Part of TODO #746 Stage A.
func (b *Bot) NewTurnSink(env agent.Envelope) (turnevent.Sink, func()) {
	origMsg, _ := env.Original.(*discordgo.Message)
	if origMsg == nil {
		return nil, nil
	}
	channelIDStr := strconv.FormatInt(env.ChatID, 10)
	d := b.resolveDisplay(env.SessionKey)
	tracker := newToolCallTracker(b, channelIDStr, d)
	renderer := newTurnRenderer(b, origMsg, tracker, d)
	sink := turn.NewStreamingSink(renderer, tracker, b)
	return sink, renderer.Cleanup
}

// Connection implements agent.Driver. Returns the bot itself.
func (b *Bot) Connection() platform.Connection {
	return b
}

// Drive implements agent.Driver. Called by the agent's per-session worker
// after batching a turn's worth of envelopes. Owns the platform-specific
// concerns: renderer/tracker construction, sink wiring, cancellable turn
// context (so /stop can cancel), error sanitisation, and lifecycle hooks.
//
// Steerer is supplied by the agent for API-mode mid-turn buffer drain at
// tool boundaries.
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

	turnCtx, cancel := context.WithCancel(ctx)
	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()
	defer func() {
		b.turnMu.Lock()
		b.turnCancel = nil
		b.turnMu.Unlock()
		cancel()
		b.drainPendingNotifications()
		if b.OnTurnEnd != nil {
			b.OnTurnEnd()
		}
	}()

	// Core turn execution moved to agent.RunTurn (TODO #746 Stage A).
	if b.agentRef == nil {
		b.logger().Warnf("Drive: agentRef nil sk=%s, cannot run turn", sk)
		return nil
	}
	err := b.agentRef.RunTurn(turnCtx, sk, batch, steerer, router, b)
	if err != nil && turnCtx.Err() != nil {
		b.logger().Infof("agent turn cancelled")
		return nil
	}
	if err != nil {
		b.logger().Errorf("agent error: %s", b.sanitizeError(err))
	}

	if b.OnTurnComplete != nil {
		b.OnTurnComplete()
	}
	return nil
}

// cancelTurn cancels the in-flight agent turn, if any.
func (b *Bot) cancelTurn() {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	if b.turnCancel != nil {
		b.logger().Infof("cancelling agent turn via /stop")
		b.turnCancel()
	}
}
