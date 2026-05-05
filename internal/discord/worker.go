package discord

import (
	"context"
	"fmt"
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
	if len(batch) == 0 {
		return nil
	}
	first := batch[0]
	origMsg, _ := first.Original.(*discordgo.Message)
	if origMsg == nil {
		b.logger().Warnf("Drive: missing original message for sk=%s", sk)
		return nil
	}
	if sk == "" {
		return nil // no session assigned (idle secondary bot)
	}

	channelID := first.ChatID
	channelIDStr := strconv.FormatInt(channelID, 10)

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

	d := b.resolveDisplay(sk)
	tracker := newToolCallTracker(b, channelIDStr, d)
	renderer := newTurnRenderer(b, origMsg, tracker, d)
	defer renderer.Cleanup()

	sink := turn.NewStreamingSink(renderer, tracker, b)

	// Register per-turn streaming sink with the session router (TODO #745).
	// Late events fall through to the router's late-delivery fallback.
	var dispatchSink turnevent.Sink = sink
	if router != nil {
		router.Register(sink)
		defer router.Clear()
		dispatchSink = router
	}

	turnCtx = agent.WithTrigger(turnCtx, "discord")
	turnCtx = agent.WithTurnMetadata(turnCtx, &agent.TurnMetadata{
		UserID:   first.UserID,
		Username: origMsg.Author.Username,
		ChatID:   channelID,
	})
	if !first.ReceivedAt.IsZero() {
		turnCtx = agent.WithReceivedAt(turnCtx, first.ReceivedAt)
	}

	// Collect texts and attachments across the batch.
	// Group chat messages get sender attribution.
	var texts []string
	var allAttachments []platform.Attachment
	for _, env := range batch {
		text := env.Text
		if env.IsGroupChat && env.SenderName != "" {
			text = fmt.Sprintf("[%s] %s", env.SenderName, text)
		} else if env.IsGroupChat && env.UserID != "" {
			text = fmt.Sprintf("[user:%s] %s", env.UserID, text)
		}
		texts = append(texts, text)
		allAttachments = append(allAttachments, env.Attachments...)
	}

	err := turn.RunTurn(turnCtx, b.handler, dispatchSink, steerer, sk, texts, allAttachments)
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
