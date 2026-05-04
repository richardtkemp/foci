package telegram

import (
	"context"
	"fmt"

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

// Drive implements agent.Driver. Called by the agent's per-session worker
// after batching a turn's worth of envelopes. Owns the platform-specific
// concerns: renderer/tracker construction, sink wiring, cancellable turn
// context (so /stop can cancel), error sanitisation, and lifecycle hooks.
//
// Steerer is supplied by the agent for API-mode mid-turn buffer drain —
// it returns texts queued in this session's inbox steer buffer since the
// last drain, pasted into the user message at the next tool boundary.
func (b *Bot) Drive(ctx context.Context, sk string, batch []agent.Envelope, steerer turnevent.Steerer) error {
	if len(batch) == 0 {
		return nil
	}
	first := batch[0]
	origMsg, _ := first.Original.(*gotgbot.Message)
	if origMsg == nil {
		b.logger().Warnf("Drive: missing original message for sk=%s", sk)
		return nil
	}
	if sk == "" {
		return nil // no session assigned (idle secondary bot)
	}

	b.logger().Debugf("Drive: enter sk=%s batch_size=%d", sk, len(batch))

	// Cancellable turn context — /stop calls b.cancelTurn() to fire this.
	turnCtx, cancel := context.WithCancel(ctx)
	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()
	defer func() {
		b.logger().Debugf("Drive: defer start sk=%s", sk)
		b.turnMu.Lock()
		b.turnCancel = nil
		b.turnMu.Unlock()
		cancel()
		b.logger().Debugf("Drive: defer calling drainPendingNotifications sk=%s", sk)
		b.drainPendingNotifications()
		if b.OnTurnEnd != nil {
			b.logger().Debugf("Drive: defer calling OnTurnEnd sk=%s", sk)
			b.OnTurnEnd()
		}
		b.logger().Debugf("Drive: defer complete sk=%s", sk)
	}()

	d := b.resolveDisplay(sk)
	tracker := newToolCallTracker(b, first.ChatID, d)
	renderer := newTurnRenderer(b, origMsg, tracker, d)
	defer renderer.Cleanup()

	sink := turn.NewStreamingSink(renderer, tracker, b)

	turnCtx = agent.WithTrigger(turnCtx, "telegram")
	turnCtx = agent.WithTurnMetadata(turnCtx, &agent.TurnMetadata{
		UserID:   first.UserID,
		Username: origMsg.From.Username,
		ChatID:   first.ChatID,
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

	b.logger().Debugf("Drive: calling turn.RunTurn sk=%s", sk)
	err := turn.RunTurn(turnCtx, b.handler, sink, steerer, sk, texts, allAttachments)
	b.logger().Debugf("Drive: RunTurn returned sk=%s err=%v", sk, err)
	if err != nil && turnCtx.Err() != nil {
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

// cancelTurn cancels the in-flight agent turn, if any.
func (b *Bot) cancelTurn() {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	if b.turnCancel != nil {
		b.logger().Infof("cancelling agent turn via /stop")
		b.turnCancel()
	}
}
