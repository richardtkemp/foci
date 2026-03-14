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

	// Set the turn session key so display overrides resolve correctly.
	b.turnSessionKey = sk
	defer func() { b.turnSessionKey = "" }()

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
		sw = newStreamWriter(b.client, qm.msg.Chat.Id, interval, b.tableOpts())
	}

	// Accumulate thinking blocks for the turn
	var thinkingBuf strings.Builder

	// Per-turn callbacks scoped to context -- no cross-turn races.
	cb := &agent.TurnCallbacks{
		// Intermediate reply delivery (deferred replies).
		// When intermediate text fires, reset the tracker so the next tool call
		// creates a fresh message below the text instead of editing the stale
		// earlier message (which would appear above the text in chat).
		// When streaming is active, the text was already delivered via the
		// stream writer. Finalize that message so the next API call's text
		// goes to a fresh Telegram message (no duplicate send).
		ReplyFunc: func(text string) {
			if sw != nil {
				msgID := sw.Finish()
				if msgID != 0 {
					content := sw.Content()
					if strings.TrimSpace(content) != "" {
						html := ConvertToTelegramHTML(content, b.tableOpts())
						_, _, _ = b.client.EditMessageText(html, &gotgbot.EditMessageTextOpts{
							ChatId:    qm.msg.Chat.Id,
							MessageId: msgID,
							ParseMode: "HTML",
						})
					}
				}
				interval := b.streamUpdateInterval
				if interval == 0 {
					interval = 250 * time.Millisecond
				}
				sw = newStreamWriter(b.client, qm.msg.Chat.Id, interval, b.tableOpts())
				return
			}
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
	//
	// Streaming + nudge interaction:
	// During a turn, the model's text is delivered two ways simultaneously:
	//   1. TextDeltaObserver → stream writer (real-time edits to a Telegram message)
	//   2. ReplyFunc (called when the agent loop splits a turn — nudges, deferred replies)
	// Without streaming, only #2 exists. With streaming, both fire for the same
	// text, creating duplicates. We suppress #2 (see ReplyFunc above) and rely
	// solely on the stream writer during streaming.
	//
	// However, the agent loop's return value (`response`) only contains text from
	// the *last* API call — nudges consume earlier text into intermediate replies
	// that would normally be sent via ReplyFunc. Since we skipped those sends, the
	// stream writer's buffer is the only place that has the full text. When
	// `response` is empty but the stream has content, we use the stream's buffer
	// so the message gets properly HTML-finalized below.
	var streamMsgID int64
	if sw != nil {
		streamMsgID = sw.Finish()
		if streamContent := sw.Content(); strings.TrimSpace(response) == "" && strings.TrimSpace(streamContent) != "" {
			response = streamContent
		}
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

	showThinkMode := b.effectiveShowThinking()
	hasThinking := thinkingText != "" && showThinkMode != "off" && showThinkMode != ""

	// Stream finalization: when streaming delivered the response, edit the stream
	// message in-place rather than sending a new message alongside a useless preview.
	if streamMsgID != 0 && len(response) <= 4096 {
		chatID := qm.msg.Chat.Id
		switch {
		case hasThinking && showThinkMode == "compact":
			b.editStreamWithThinking(streamMsgID, chatID, response, thinkingText)
		case hasThinking && showThinkMode == "true":
			b.editStreamWithFullThinking(streamMsgID, chatID, response, thinkingText)
		default:
			// No thinking — edit stream message with final HTML.
			// If edit fails (e.g. "message is not modified"), the stream
			// already has the content, so just return — no duplicate.
			htmlResp := ConvertToTelegramHTML(response, b.tableOpts())
			_, _, editErr := b.client.EditMessageText(htmlResp, &gotgbot.EditMessageTextOpts{
				ChatId:    chatID,
				MessageId: streamMsgID,
				ParseMode: "HTML",
			})
			if editErr != nil {
				b.logger().Debugf("edit stream final: %v (stream already has content)", editErr)
			}
		}
		return
	}

	// Stream message exists but response is too long for a single edit — send as
	// new message(s) and convert the stream message to a truncated preview.
	if streamMsgID != 0 {
		if hasThinking && showThinkMode == "true" {
			b.sendReplyWithFullThinking(qm.msg, response, thinkingText)
		} else if hasThinking && showThinkMode == "compact" {
			b.sendReplyWithThinking(qm.msg, response, thinkingText)
		} else {
			b.sendReply(qm.msg, response)
		}
		b.editStreamPreview(streamMsgID, qm.msg.Chat.Id, response)
		return
	}

	// No streaming — use tool call preview edit or send a new message.
	editID := tracker.lastMsgID()
	if editID != 0 && b.effectiveShowToolCalls() == "preview" && !hasThinking && len(response) <= 4096 {
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

	if hasThinking && showThinkMode == "true" {
		b.sendReplyWithFullThinking(qm.msg, response, thinkingText)
		return
	}
	if hasThinking && showThinkMode == "compact" {
		b.sendReplyWithThinking(qm.msg, response, thinkingText)
		return
	}
	b.sendReply(qm.msg, response)
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
