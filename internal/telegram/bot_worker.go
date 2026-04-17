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

// agentWorker processes queued messages and commands, batching messages that
// arrive while busy. Commands are given priority over agent turns: they are
// drained before starting a new turn, preventing long turns from starving
// pending commands.
func (b *Bot) agentWorker(ctx context.Context) {
	for {
		// Priority: drain any pending commands before starting a new agent turn.
		select {
		case cmd := <-b.mq.CmdChan():
			b.logger().Debugf("worker: dequeued command %q", cmd.Text)
			b.processQueuedCommand(ctx, cmd)
			continue
		default:
		}

		select {
		case <-ctx.Done():
			b.logger().Debugf("worker: ctx done, exiting")
			return
		case cmd := <-b.mq.CmdChan():
			b.logger().Debugf("worker: dequeued command %q", cmd.Text)
			b.processQueuedCommand(ctx, cmd)
		case qm := <-b.mq.Chan():
			// Batch with any other immediately-available messages.
			batch := append([]platform.QueuedMessage{qm}, b.mq.DrainQueue()...)
			b.logger().Debugf("worker: dequeued batch of %d, entering processAgentMessage", len(batch))
			b.processAgentMessage(ctx, batch)
			b.logger().Debugf("worker: processAgentMessage returned, draining orphans")

			// After the turn: drain orphan steers + any newly queued messages.
			// Loop because new steers/messages can arrive during processing.
			for {
				orphans := b.mq.DrainSteer()
				extras := b.mq.DrainQueue()
				if len(orphans) == 0 && len(extras) == 0 {
					break
				}
				var followUp []platform.QueuedMessage
				for _, s := range orphans {
					followUp = append(followUp, platform.QueuedMessage{
						Original:   qm.Original,
						UserID:     qm.UserID,
						Text:       s.Text,
						ChatID:     qm.ChatID,
						ReceivedAt: s.ReceivedAt,
					})
				}
				followUp = append(followUp, extras...)
				b.logger().Infof("steer: processing %d orphan(s) + %d queued as follow-up turn", len(orphans), len(extras))
				b.processAgentMessage(ctx, followUp)
				b.logger().Debugf("worker: follow-up processAgentMessage returned")
			}
			b.logger().Debugf("worker: returning to select for next message")
		}
	}
}

// processQueuedCommand dispatches a command that was routed via the command
// channel rather than the polling goroutine. Commands are routed here (instead
// of being dispatched inline in the polling loop) so that blocking commands
// like /reset do not prevent getUpdates from running, which would stop
// callback_query delivery (e.g. permission "Allow" buttons).
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

// processAgentMessage handles a batched agent turn with a cancellable context.
// Session key, typing indicator, and renderer are derived from batch[0].
func (b *Bot) processAgentMessage(ctx context.Context, batch []platform.QueuedMessage) {
	first := batch[0]
	origMsg, _ := first.Original.(*gotgbot.Message)
	if origMsg == nil {
		b.logger().Warnf("processAgentMessage: missing original message")
		return
	}

	var sk string
	if b.isSecondary {
		sk = b.SessionKey()
	} else if b.agentID != "" {
		sk = b.sessionKeyForMsg(first.ChatID)
	} else {
		sk = b.SessionKey()
	}
	if sk == "" {
		return // no session assigned (idle secondary bot)
	}

	b.logger().Debugf("processAgentMessage: enter sk=%s batch_size=%d", sk, len(batch))

	// Create a cancellable context for this turn
	turnCtx, cancel := context.WithCancel(ctx)

	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()

	defer func() {
		b.logger().Debugf("processAgentMessage: defer start sk=%s", sk)
		b.turnMu.Lock()
		b.turnCancel = nil
		b.turnMu.Unlock()
		cancel()
		b.logger().Debugf("processAgentMessage: defer calling drainPendingNotifications sk=%s", sk)
		b.drainPendingNotifications()
		if b.OnTurnEnd != nil {
			b.logger().Debugf("processAgentMessage: defer calling OnTurnEnd sk=%s", sk)
			b.OnTurnEnd()
		}
		b.logger().Debugf("processAgentMessage: defer complete sk=%s", sk)
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
	for _, qm := range batch {
		text := qm.Text
		if qm.IsGroupChat && qm.SenderName != "" {
			text = fmt.Sprintf("[%s] %s", qm.SenderName, text)
		} else if qm.IsGroupChat && qm.UserID != "" {
			text = fmt.Sprintf("[user:%s] %s", qm.UserID, text)
		}
		texts = append(texts, text)
		allAttachments = append(allAttachments, qm.Attachments...)
	}

	b.logger().Debugf("processAgentMessage: calling turn.RunTurn sk=%s", sk)
	err := turn.RunTurn(turnCtx, b.handler, sink, turnevent.SteererFunc(b.mq.DrainSteerTexts), sk, texts, allAttachments)
	b.logger().Debugf("processAgentMessage: RunTurn returned sk=%s err=%v", sk, err)
	if err != nil && turnCtx.Err() != nil {
		b.logger().Infof("agent turn cancelled")
		return // /stop was called, "Stopped." already sent
	}
	if err != nil {
		b.logger().Errorf("agent error: %s", b.sanitizeError(err))
	}

	if b.OnTurnComplete != nil {
		b.logger().Debugf("processAgentMessage: calling OnTurnComplete sk=%s", sk)
		b.OnTurnComplete()
	}
	b.logger().Debugf("processAgentMessage: exit sk=%s", sk)
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
