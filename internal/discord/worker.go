package discord

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"foci/internal/agent"
	"foci/internal/platform"

	"github.com/bwmarrin/discordgo"
)

// agentWorker processes queued messages, batching any that arrive while busy.
func (b *Bot) agentWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case qm := <-b.mq.Chan():
			// Batch with any other immediately-available messages.
			batch := append([]platform.QueuedMessage{qm}, b.mq.DrainQueue()...)
			b.processAgentMessage(ctx, batch)

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
						Original: qm.Original,
						UserID:   qm.UserID,
						Text:     s,
						ChatID:   qm.ChatID,
					})
				}
				followUp = append(followUp, extras...)
				b.logger().Infof("steer: processing %d orphan(s) + %d queued as follow-up turn", len(orphans), len(extras))
				b.processAgentMessage(ctx, followUp)
			}
		}
	}
}

// processAgentMessage handles a batched agent turn with a cancellable context.
// Session key, typing indicator, and renderer are derived from batch[0].
func (b *Bot) processAgentMessage(ctx context.Context, batch []platform.QueuedMessage) {
	first := batch[0]
	origMsg, _ := first.Original.(*discordgo.Message)
	if origMsg == nil {
		b.logger().Warnf("processAgentMessage: missing original message")
		return
	}

	channelID := first.ChatID
	channelIDStr := strconv.FormatInt(channelID, 10)

	var sk string
	if b.isSecondary {
		sk = b.SessionKey()
	} else if b.agentID != "" {
		sk = b.sessionKeyForMsg(channelID)
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
	// Discord typing expires after ~10s, so we re-send every 8s.
	_ = b.session.ChannelTyping(channelIDStr)
	typingTicker := time.NewTicker(8 * time.Second)
	go func() {
		for {
			select {
			case <-typingTicker.C:
				_ = b.session.ChannelTyping(channelIDStr)
			case <-turnCtx.Done():
				return
			}
		}
	}()
	defer typingTicker.Stop()

	d := b.resolveDisplay(sk)
	tracker := newToolCallTracker(b, channelIDStr, d)
	renderer := newTurnRenderer(b, origMsg, tracker, d)
	defer renderer.Cleanup()

	cb := &agent.TurnCallbacks{
		ReplyFunc:          renderer.OnReply,
		ActivityFunc:       renderer.OnActivity,
		ToolCallObserver:   tracker.ObserveToolCall,
		ToolResultObserver: tracker.ObserveToolResult,
		ThinkingObserver:   renderer.OnThinking,
		TextDeltaObserver:  renderer.OnTextDelta,
		SteerCheckFunc:     b.mq.DrainSteer,
		RetryNotifyFunc:    tracker.NotifyRetry,
		RetrySuccessFunc:   tracker.ClearRetryNotification,
	}
	turnCtx = agent.WithTurnCallbacks(turnCtx, cb)
	turnCtx = agent.WithTrigger(turnCtx, "discord")
	turnCtx = agent.WithTurnMetadata(turnCtx, &agent.TurnMetadata{
		UserID:   first.UserID,
		Username: origMsg.Author.Username,
		ChatID:   channelID,
	})

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

	var response string
	var err error
	if len(allAttachments) > 0 {
		response, err = b.handler.HandleMessageWithAttachments(turnCtx, sk, texts, allAttachments)
	} else {
		response, err = b.handler.HandleMessageWithAttachments(turnCtx, sk, texts, nil)
	}
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
