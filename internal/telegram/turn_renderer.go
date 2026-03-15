package telegram

import (
	"strings"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// TurnRenderer encapsulates all per-turn rendering state: streaming, thinking
// accumulation, tool call tracking, and response finalization. It collapses the
// combinatorial explosion of ~8 finalization code paths into a single Finalize
// method.
type TurnRenderer struct {
	bot      *Bot
	msg      *gotgbot.Message
	chatID   int64
	display  turnDisplay
	sw       *streamWriter
	tracker  *toolCallTracker
	thinking strings.Builder
}

// newTurnRenderer creates a TurnRenderer with a tool call tracker and a stream
// writer. The stream writer is always present but only sends messages when
// streaming is enabled (live mode). The session key is used to resolve
// per-session display overrides.
func newTurnRenderer(bot *Bot, msg *gotgbot.Message, sessionKey string) *TurnRenderer {
	d := bot.resolveDisplay(sessionKey)
	return &TurnRenderer{
		bot:     bot,
		msg:     msg,
		chatID:  msg.Chat.Id,
		display: d,
		sw:      newStreamWriter(bot.client, msg.Chat.Id, bot.streamInterval(), d.renderOpts, d.streamOutput),
		tracker: &toolCallTracker{bot: bot, chatID: msg.Chat.Id, display: d},
	}
}

// Cleanup finishes the stream writer if it hasn't been finished yet.
// Safe to call from defer — Finish is idempotent.
func (r *TurnRenderer) Cleanup() {
	r.sw.Finish()
}

// onReply handles intermediate text delivery (ReplyFunc callback).
// When streaming is active, the text was already delivered via the stream
// writer — finalize that message and clean up any tool call preview (since the
// reply content is in the stream message). When not streaming, overwrite the
// tool call preview with the reply text (preview mode) or send a new message.
func (r *TurnRenderer) onReply(text string) {
	msgID := r.sw.Finish()
	if msgID != 0 {
		// Streaming: reply content is in the stream message. Finalize it
		// and delete any lingering tool call preview.
		content := r.sw.Content()
		if strings.TrimSpace(content) != "" {
			html := ConvertToTelegramHTML(content, r.display.renderOpts)
			_, _, _ = r.bot.client.EditMessageText(html, &gotgbot.EditMessageTextOpts{
				ChatId:    r.chatID,
				MessageId: msgID,
				ParseMode: "HTML",
			})
		}
		r.tracker.cleanupPreview()
	} else if !r.display.streamOutput {
		// Non-streaming: if there's a tool call preview, overwrite it with
		// the reply text. Otherwise send a new message.
		if !r.editToolPreviewWithReply(text) {
			r.bot.sendReply(r.msg, text)
		}
		r.tracker.resetMsgID()
	}
	// Fresh stream writer for the next segment.
	r.sw = newStreamWriter(r.bot.client, r.chatID, r.bot.streamInterval(), r.display.renderOpts, r.display.streamOutput)
}

// editToolPreviewWithReply edits the tool call preview message with intermediate
// reply text, replacing the tool call indicator with the actual reply content.
// Returns true if the edit succeeded. Falls back to false when there's no
// preview, the mode isn't "preview", or the text is too long to edit in-place.
func (r *TurnRenderer) editToolPreviewWithReply(text string) bool {
	editID := r.tracker.lastMsgID()
	if editID == 0 || r.display.showToolCalls != "preview" {
		return false
	}
	if strings.TrimSpace(text) == "" || len(text) > 4096 {
		r.tracker.cleanupPreview()
		return false
	}
	html := ConvertToTelegramHTML(text, r.display.renderOpts)
	_, _, err := r.bot.client.EditMessageText(html, &gotgbot.EditMessageTextOpts{
		ChatId:    r.chatID,
		MessageId: editID,
		ParseMode: "HTML",
	})
	if err != nil {
		r.bot.logger().Debugf("edit tool preview with reply: %v", err)
		return false
	}
	return true
}

// onThinking accumulates thinking blocks (gated by showThinking config).
func (r *TurnRenderer) onThinking(thinking string) {
	mode := r.display.showThinking
	if mode == "off" || mode == "" {
		return
	}
	if r.thinking.Len() > 0 {
		r.thinking.WriteString("\n")
	}
	r.thinking.WriteString(thinking)
}

// onTextDelta handles streaming delta callbacks: updates the stream writer
// and refreshes the typing indicator.
func (r *TurnRenderer) onTextDelta(delta string) {
	r.sw.OnDelta(delta)
	_, _ = r.bot.client.SendChatAction(r.chatID, "typing", nil)
}

// onActivity refreshes the typing indicator when tools complete.
func (r *TurnRenderer) onActivity() {
	_, _ = r.bot.client.SendChatAction(r.chatID, "typing", nil)
}

// Finalize renders the final agent response to Telegram. It handles all
// combinations of streaming/non-streaming, thinking modes, response length,
// and tool call previews.
func (r *TurnRenderer) Finalize(response string) {
	// Finish the stream writer and get the message ID it created (if any).
	//
	// During a turn, model text is delivered two ways simultaneously:
	//   1. TextDeltaObserver -> stream writer (real-time edits)
	//   2. ReplyFunc (agent loop splits a turn -- nudges, deferred replies)
	// Without streaming, only #2 exists. With streaming, both fire for the
	// same text; we suppress #2 (see onReply) and rely on the stream writer.
	//
	// The agent loop's return value only contains text from the *last* API
	// call. When response is empty but the stream has content, use the
	// stream's buffer so the message gets properly HTML-finalized.
	streamMsgID := r.sw.Finish()
	if streamContent := r.sw.Content(); strings.TrimSpace(response) == "" && strings.TrimSpace(streamContent) != "" {
		response = streamContent
	}

	// Guard against empty responses.
	if strings.TrimSpace(response) == "" {
		r.bot.logger().Debugf("agent returned empty response, not sending")
		return
	}

	thinkingText := r.thinking.String()
	showThinkMode := r.display.showThinking
	hasThinking := thinkingText != "" && showThinkMode != "off" && showThinkMode != ""

	// Stream finalization: edit the stream message in-place when possible.
	if streamMsgID != 0 && len(response) <= 4096 {
		r.finalizeStreamShort(streamMsgID, response, thinkingText, showThinkMode, hasThinking)
		r.tracker.cleanupPreview()
		return
	}

	// Stream message exists but response is too long -- send as new message(s)
	// and convert the stream message to a truncated preview.
	if streamMsgID != 0 {
		r.sendWithThinkingMode(response, thinkingText, showThinkMode, hasThinking)
		r.editStreamPreview(streamMsgID, response)
		r.tracker.cleanupPreview()
		return
	}

	// No streaming -- try editing the tool call preview in-place.
	if r.tryEditToolPreview(response, hasThinking) {
		return
	}

	// Response sent as a new message -- clean up any lingering tool call preview.
	r.tracker.cleanupPreview()
	r.sendWithThinkingMode(response, thinkingText, showThinkMode, hasThinking)
}

// finalizeStreamShort edits the stream message in-place with the final response
// (which fits within 4096 chars).
func (r *TurnRenderer) finalizeStreamShort(streamMsgID int64, response, thinkingText, showThinkMode string, hasThinking bool) {
	switch {
	case hasThinking && showThinkMode == "compact":
		r.editStreamWithThinking(streamMsgID, response, thinkingText)
	case hasThinking && showThinkMode == "true":
		r.editStreamWithFullThinking(streamMsgID, response, thinkingText)
	default:
		htmlResp := ConvertToTelegramHTML(response, r.display.renderOpts)
		_, _, editErr := r.bot.client.EditMessageText(htmlResp, &gotgbot.EditMessageTextOpts{
			ChatId:    r.chatID,
			MessageId: streamMsgID,
			ParseMode: "HTML",
		})
		if editErr != nil {
			r.bot.logger().Debugf("edit stream final: %v (stream already has content)", editErr)
		}
	}
}

// sendWithThinkingMode sends a response as a new message, applying the
// appropriate thinking display mode.
func (r *TurnRenderer) sendWithThinkingMode(response, thinkingText, showThinkMode string, hasThinking bool) {
	switch {
	case hasThinking && showThinkMode == "true":
		r.sendWithFullThinking(response, thinkingText)
	case hasThinking && showThinkMode == "compact":
		r.sendWithCompactThinking(response, thinkingText)
	default:
		r.bot.sendReply(r.msg, response)
	}
}

// tryEditToolPreview attempts to edit the tool call preview message with the
// final response. Returns true if successful.
func (r *TurnRenderer) tryEditToolPreview(response string, hasThinking bool) bool {
	editID := r.tracker.lastMsgID()
	if editID == 0 || r.display.showToolCalls != "preview" || hasThinking || len(response) > 4096 {
		return false
	}
	htmlResp := ConvertToTelegramHTML(response, r.display.renderOpts)
	_, _, editErr := r.bot.client.EditMessageText(htmlResp, &gotgbot.EditMessageTextOpts{
		ChatId:    r.chatID,
		MessageId: editID,
		ParseMode: "HTML",
	})
	if editErr != nil {
		r.bot.logger().Debugf("edit final response failed, falling back: %v", editErr)
		return false
	}
	return true
}

// editStreamPreview edits the stream message to a truncated preview when the
// final response was sent as a separate message (too long, has thinking, etc.).
func (r *TurnRenderer) editStreamPreview(streamMsgID int64, response string) {
	if streamMsgID == 0 {
		return
	}
	preview := truncate(response, 200)
	_, _, _ = r.bot.client.EditMessageText(
		htmlEscape(preview)+"\n\n<i>(full response below)</i>",
		&gotgbot.EditMessageTextOpts{
			ChatId:    r.chatID,
			MessageId: streamMsgID,
			ParseMode: "HTML",
		})
}

// buildThinkingHTML builds a combined thinking + divider + response HTML string.
func buildThinkingHTML(responseHTML, thinkingText string, displayWidth int) string {
	thinkingHTML := "<i>" + htmlEscape(thinkingText) + "</i>"
	divider := "\n" + strings.Repeat("—", displayWidth) + "\n\n"
	return thinkingHTML + divider + responseHTML
}

// sendWithFullThinking sends thinking (italic) + divider + response as a single message.
func (r *TurnRenderer) sendWithFullThinking(response, thinkingText string) {
	responseHTML := ConvertToTelegramHTML(response, r.display.renderOpts)
	r.bot.sendHTMLChunks(r.chatID, buildThinkingHTML(responseHTML, thinkingText, r.display.displayWidth))
}

// sendWithCompactThinking sends a response with a "Show thinking" inline keyboard button.
func (r *TurnRenderer) sendWithCompactThinking(response, thinkingText string) {
	responseHTML := ConvertToTelegramHTML(response, r.display.renderOpts)

	sendOpts := &gotgbot.SendMessageOpts{
		ParseMode:   "HTML",
		ReplyMarkup: singleButtonKeyboard("Show thinking", "th:show"),
	}

	chunks := splitMessage(responseHTML, 4096)
	for i, chunk := range chunks {
		if i < len(chunks)-1 {
			r.bot.sendHTMLChunks(r.chatID, chunk)
			continue
		}
		sent, err := r.bot.client.SendMessage(r.chatID, chunk, sendOpts)
		if err != nil {
			r.bot.logger().Errorf("send reply with thinking button: %v", err)
			return
		}
		r.bot.thinkingStore.Store(sent.MessageId, thinkingEntry{
			responseHTML: chunk,
			thinkingText: thinkingText,
		})
	}
}

// editStreamWithThinking edits the stream message in-place with the final HTML
// response and a "Show thinking" inline keyboard button.
func (r *TurnRenderer) editStreamWithThinking(msgID int64, response, thinkingText string) {
	responseHTML := ConvertToTelegramHTML(response, r.display.renderOpts)
	kb := singleButtonKeyboard("Show thinking", "th:show")
	_, _, err := r.bot.client.EditMessageText(responseHTML, &gotgbot.EditMessageTextOpts{
		ChatId:      r.chatID,
		MessageId:   msgID,
		ParseMode:   "HTML",
		ReplyMarkup: kb,
	})
	if err != nil {
		r.bot.logger().Errorf("edit stream with thinking button: %v", err)
		return
	}
	r.bot.thinkingStore.Store(msgID, thinkingEntry{
		responseHTML: responseHTML,
		thinkingText: thinkingText,
	})
}

// editStreamWithFullThinking edits the stream message in-place with thinking
// (italic) + divider + response HTML.
func (r *TurnRenderer) editStreamWithFullThinking(msgID int64, response, thinkingText string) {
	responseHTML := ConvertToTelegramHTML(response, r.display.renderOpts)
	combined := buildThinkingHTML(responseHTML, thinkingText, r.display.displayWidth)
	_, _, err := r.bot.client.EditMessageText(combined, &gotgbot.EditMessageTextOpts{
		ChatId:    r.chatID,
		MessageId: msgID,
		ParseMode: "HTML",
	})
	if err != nil {
		r.bot.logger().Errorf("edit stream with full thinking: %v", err)
	}
}
