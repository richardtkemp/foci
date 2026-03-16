package discord

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"foci/internal/agent"
	"foci/internal/platform"
)

// agentWorker processes queued messages one at a time.
func (b *Bot) agentWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case qm := <-b.queue:
			b.processAgentMessage(ctx, qm)
			// Drain steer messages that arrived after the turn completed.
			// Process them as normal follow-up turns so the user's redirection
			// isn't silently dropped. Loop because a new steer can arrive
			// during orphan processing itself.
			for orphan := b.drainSteer(); orphan != ""; orphan = b.drainSteer() {
				b.logger().Infof("steer: processing orphaned steer message as follow-up turn")
				b.processAgentMessage(ctx, queuedMessage{msg: qm.msg, userID: qm.userID, text: orphan})
			}
		}
	}
}

// processAgentMessage handles a single agent turn with a cancellable context.
func (b *Bot) processAgentMessage(ctx context.Context, qm queuedMessage) {
	channelID, _ := strconv.ParseInt(qm.msg.ChannelID, 10, 64)

	var sk string
	if b.isSecondary {
		// Secondary bots use their override session key
		sk = b.SessionKey()
	} else if b.agentID != "" {
		// Primary bots derive session key from the message's channel ID
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
	_ = b.session.ChannelTyping(qm.msg.ChannelID)
	typingTicker := time.NewTicker(8 * time.Second)
	go func() {
		for {
			select {
			case <-typingTicker.C:
				_ = b.session.ChannelTyping(qm.msg.ChannelID)
			case <-turnCtx.Done():
				return
			}
		}
	}()
	defer typingTicker.Stop()

	renderer := newTurnRenderer(b, qm.msg, sk)
	defer renderer.Cleanup()

	cb := &agent.TurnCallbacks{
		ReplyFunc:          renderer.onReply,
		ActivityFunc:       renderer.onActivity,
		ToolCallObserver:   renderer.tracker.observeToolCall,
		ToolResultObserver: renderer.tracker.observeToolResult,
		ThinkingObserver:   renderer.onThinking,
		TextDeltaObserver:  renderer.onTextDelta,
		SteerCheckFunc:     b.drainSteer,
		RetryNotifyFunc:    renderer.tracker.notifyRetry,
		RetrySuccessFunc:   renderer.tracker.clearRetryNotification,
	}
	turnCtx = agent.WithTurnCallbacks(turnCtx, cb)
	turnCtx = agent.WithTrigger(turnCtx, "discord")
	turnCtx = agent.WithTurnMetadata(turnCtx, &agent.TurnMetadata{
		UserID:   qm.userID,
		Username: qm.msg.Author.Username,
		ChatID:   channelID,
	})

	var response string
	var err error
	if len(qm.attachments) > 0 {
		// Convert discord attachments to platform attachment data
		platformAttachments := make([]platform.Attachment, len(qm.attachments))
		for i, att := range qm.attachments {
			platformAttachments[i] = platform.Attachment{MimeType: att.mediaType, Data: att.data, SavedPath: att.savedPath}
		}
		response, err = b.handler.HandleMessageWithAttachments(turnCtx, sk, qm.text, platformAttachments)
	} else {
		response, err = b.handler.HandleMessage(turnCtx, sk, qm.text)
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
