package telegram

import (
	"context"
	"fmt"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/platform"

	"github.com/PaulSonOfLars/gotgbot/v2"
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
			// isn't silently dropped.
			if orphan := b.drainSteer(); orphan != "" {
				b.logger().Infof("steer: processing orphaned steer message as follow-up turn")
				b.processAgentMessage(ctx, queuedMessage{msg: qm.msg, userID: qm.userID, text: orphan})
			}
		}
	}
}

// processAgentMessage handles a single agent turn with a cancellable context.
func (b *Bot) processAgentMessage(ctx context.Context, qm queuedMessage) {
	var sk string
	if b.isSecondary {
		// Secondary bots use their override session key
		sk = b.SessionKey()
	} else if b.agentID != "" {
		// Primary bots derive session key from the message's chat ID
		sk = b.sessionKeyForMsg(qm.msg.Chat.Id)
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
	}()

	// Send typing indicator and keep it alive throughout the agent turn.
	// Telegram typing expires after ~5s, so we re-send every 4s.
	_, _ = b.client.SendChatAction(qm.msg.Chat.Id, "typing", nil)
	typingTicker := time.NewTicker(4 * time.Second)
	go func() {
		for {
			select {
			case <-typingTicker.C:
				_, _ = b.client.SendChatAction(qm.msg.Chat.Id, "typing", nil)
			case <-turnCtx.Done():
				return
			}
		}
	}()
	defer typingTicker.Stop()

	// Track tool calls for live visibility via send+edit pattern.
	tracker := &toolCallTracker{bot: b, chatID: qm.msg.Chat.Id}

	// Stream writer: real-time message updates during streaming.
	var sw *streamWriter
	if b.effectiveStreamOutput() {
		interval := b.streamUpdateInterval
		if interval == 0 {
			interval = 250 * time.Millisecond
		}
		sw = newStreamWriter(b.client, qm.msg.Chat.Id, interval)
	}

	// Accumulate thinking blocks for the turn
	var thinkingBuf strings.Builder

	// Per-turn callbacks scoped to context -- no cross-turn races.
	cb := &agent.TurnCallbacks{
		// Intermediate reply delivery (deferred replies).
		// When intermediate text fires, reset the tracker so the next tool call
		// creates a fresh message below the text instead of editing the stale
		// earlier message (which would appear above the text in chat).
		ReplyFunc: func(text string) {
			b.sendReply(qm.msg, text)
			tracker.resetMsgID()
		},
		// Refresh typing indicator when tools complete
		ActivityFunc: func() {
			_, _ = b.client.SendChatAction(qm.msg.Chat.Id, "typing", nil)
		},
		ToolCallObserver:   tracker.observeToolCall,
		ToolResultObserver: tracker.observeToolResult,
		// Thinking block accumulator (gated by showThinking)
		ThinkingObserver: func(thinking string) {
			if b.effectiveShowThinking() == "off" || b.effectiveShowThinking() == "" {
				return
			}
			if thinkingBuf.Len() > 0 {
				thinkingBuf.WriteString("\n")
			}
			thinkingBuf.WriteString(thinking)
		},
		// Streaming delta callbacks: update stream writer + refresh typing indicator.
		TextDeltaObserver: func(delta string) {
			if sw != nil {
				sw.OnDelta(delta)
			}
			_, _ = b.client.SendChatAction(qm.msg.Chat.Id, "typing", nil)
		},
		// Steer: drain buffered user messages for injection between tool calls.
		SteerCheckFunc: b.drainSteer,
		// Retry notifications: inform user when API is retrying.
		RetryNotifyFunc:  tracker.notifyRetry,
		RetrySuccessFunc: tracker.clearRetryNotification,
	}
	turnCtx = agent.WithTurnCallbacks(turnCtx, cb)
	turnCtx = agent.WithTrigger(turnCtx, "telegram")
	turnCtx = agent.WithTurnMetadata(turnCtx, &agent.TurnMetadata{
		UserID:   qm.userID,
		Username: qm.msg.From.Username,
		ChatID:   qm.msg.Chat.Id,
	})

	var response string
	var err error
	if len(qm.attachments) > 0 {
		// Convert telegram attachments to platform attachment data
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

	// Finish the stream writer and get the message ID it created (if any).
	var streamMsgID int64
	if sw != nil {
		streamMsgID = sw.Finish()
	}

	// Guard against empty responses (e.g. end_turn after tool use with no text).
	// Sending an empty string to Telegram would fail with "message text is empty".
	if strings.TrimSpace(response) == "" {
		b.logger().Debugf("agent returned empty response for %s, not sending", sk)
		return
	}

	// Show thinking: prepend thinking to response ("true" mode) or attach
	// a toggle button ("compact" mode).
	thinkingText := thinkingBuf.String()

	// Determine which message to edit with the final response.
	// The stream message takes priority (it's what the user sees as "in-progress"),
	// then the tool call preview message.
	showThinkMode := b.effectiveShowThinking()
	hasThinking := thinkingText != "" && showThinkMode != "off" && showThinkMode != ""

	editID := tracker.lastMsgID()
	if streamMsgID != 0 {
		editID = streamMsgID
	}

	// Try to edit an existing message with the final HTML-formatted response.
	// Works for both stream messages and tool call preview messages.
	// Skip when thinking will be shown — thinking-aware send functions handle the full reply.
	var canEditInPlace bool
	if streamMsgID != 0 {
		// Stream messages can always be edited (no show_tool_calls mode gate).
		canEditInPlace = editID != 0 && !hasThinking && len(response) <= 4096
	} else {
		// Tool call preview: only edit in preview mode.
		canEditInPlace = editID != 0 && b.effectiveShowToolCalls() == "preview" && !hasThinking && len(response) <= 4096
	}

	if canEditInPlace {
		htmlResp := ConvertToTelegramHTML(response, b.tableOpts())
		_, _, editErr := b.client.EditMessageText(htmlResp, &gotgbot.EditMessageTextOpts{
			ChatId:    qm.msg.Chat.Id,
			MessageId: editID,
			ParseMode: "HTML",
		})
		if editErr == nil {
			return
		}
		b.logger().Debugf("edit final response failed, falling back: %v", editErr)
	}

	// Full thinking: prepend italic thinking + divider to response
	if showThinkMode == "true" && thinkingText != "" {
		b.sendReplyWithFullThinking(qm.msg, response, thinkingText)
		b.editStreamPreview(streamMsgID, qm.msg.Chat.Id, response)
		return
	}

	// Compact thinking: send response with "Show thinking" toggle button
	if showThinkMode == "compact" && thinkingText != "" {
		b.sendReplyWithThinking(qm.msg, response, thinkingText)
		b.editStreamPreview(streamMsgID, qm.msg.Chat.Id, response)
		return
	}

	b.sendReply(qm.msg, response)
	b.editStreamPreview(streamMsgID, qm.msg.Chat.Id, response)
}

// editStreamPreview edits the stream message to a truncated preview when the
// final response was sent as a separate message (too long, has thinking, etc.).
func (b *Bot) editStreamPreview(streamMsgID, chatID int64, response string) {
	if streamMsgID == 0 {
		return
	}
	preview := truncate(response, 200)
	_, _, _ = b.client.EditMessageText(
		htmlEscapeBot(preview)+"\n\n<i>(full response below)</i>",
		&gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: streamMsgID,
			ParseMode: "HTML",
		})
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
