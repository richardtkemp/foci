package telegram

import (
	"context"
	"fmt"
	"time"

	"foci/internal/agent"
	"foci/internal/platform"
)

// agentWorker processes queued messages, batching any that arrive while busy.
func (b *Bot) agentWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case qm := <-b.queue:
			// Batch with any other immediately-available messages.
			batch := append([]queuedMessage{qm}, b.drainQueue()...)
			b.processAgentMessage(ctx, batch)

			// After the turn: drain orphan steers + any newly queued messages.
			// Loop because new steers/messages can arrive during processing.
			for {
				orphans := b.drainSteer()
				extras := b.drainQueue()
				if len(orphans) == 0 && len(extras) == 0 {
					break
				}
				var followUp []queuedMessage
				for _, s := range orphans {
					followUp = append(followUp, queuedMessage{msg: qm.msg, userID: qm.userID, text: s})
				}
				followUp = append(followUp, extras...)
				b.logger().Infof("steer: processing %d orphan(s) + %d queued as follow-up turn", len(orphans), len(extras))
				b.processAgentMessage(ctx, followUp)
			}
		}
	}
}

// drainQueue non-blocking drains all immediately available messages from the queue.
func (b *Bot) drainQueue() []queuedMessage {
	var msgs []queuedMessage
	for {
		select {
		case qm := <-b.queue:
			msgs = append(msgs, qm)
		default:
			return msgs
		}
	}
}

// processAgentMessage handles a batched agent turn with a cancellable context.
// Session key, typing indicator, and renderer are derived from batch[0].
func (b *Bot) processAgentMessage(ctx context.Context, batch []queuedMessage) {
	first := batch[0]
	var sk string
	if b.isSecondary {
		sk = b.SessionKey()
	} else if b.agentID != "" {
		sk = b.sessionKeyForMsg(first.msg.Chat.Id)
	} else {
		sk = b.SessionKey()
	}
	if sk == "" {
		return // no session assigned (idle secondary bot)
	}

	// Create a cancellable context for this turn
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

	// Send typing indicator and keep it alive throughout the agent turn.
	// Telegram typing expires after ~5s, so we re-send every 4s.
	_, _ = b.client.SendChatAction(first.msg.Chat.Id, "typing", nil)
	typingTicker := time.NewTicker(4 * time.Second)
	go func() {
		for {
			select {
			case <-typingTicker.C:
				_, _ = b.client.SendChatAction(first.msg.Chat.Id, "typing", nil)
			case <-turnCtx.Done():
				return
			}
		}
	}()
	defer typingTicker.Stop()

	d := b.resolveDisplay(sk)
	tracker := newToolCallTracker(b, first.msg.Chat.Id, d)
	renderer := newTurnRenderer(b, first.msg, tracker, d)
	defer renderer.Cleanup()

	cb := &agent.TurnCallbacks{
		ReplyFunc:          renderer.OnReply,
		ActivityFunc:       renderer.OnActivity,
		ToolCallObserver:   tracker.ObserveToolCall,
		ToolResultObserver: tracker.ObserveToolResult,
		ThinkingObserver:   renderer.OnThinking,
		TextDeltaObserver:  renderer.OnTextDelta,
		SteerCheckFunc:     b.drainSteer,
		RetryNotifyFunc:    tracker.NotifyRetry,
		RetrySuccessFunc:   tracker.ClearRetryNotification,
	}
	turnCtx = agent.WithTurnCallbacks(turnCtx, cb)
	turnCtx = agent.WithTrigger(turnCtx, "telegram")
	turnCtx = agent.WithTurnMetadata(turnCtx, &agent.TurnMetadata{
		UserID:   first.userID,
		Username: first.msg.From.Username,
		ChatID:   first.msg.Chat.Id,
	})

	// Collect texts and attachments across the batch.
	var texts []string
	var allAttachments []platform.Attachment
	for _, qm := range batch {
		texts = append(texts, qm.text)
		for _, att := range qm.attachments {
			allAttachments = append(allAttachments, platform.Attachment{
				MimeType:  att.mediaType,
				Data:      att.data,
				SavedPath: att.savedPath,
			})
		}
	}

	response, err := b.handler.HandleMessageWithAttachments(turnCtx, sk, texts, allAttachments)
	if err != nil {
		if turnCtx.Err() != nil {
			b.logger().Infof("agent turn cancelled")
			return // /stop was called, "Stopped." already sent
		}
		b.logger().Errorf("agent error: %s", b.sanitizeError(err))
		response = fmt.Sprintf("Error: %s", b.sanitizeError(err))
	}
	if b.OnTurnComplete != nil {
		b.OnTurnComplete()
	}

	renderer.Finalize(response)
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
