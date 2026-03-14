package telegram

import (
	"strings"
	"time"

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
	sw       *streamWriter
	tracker  *toolCallTracker
	thinking strings.Builder
}

// newTurnRenderer creates a TurnRenderer with a tool call tracker and (if
// streaming is enabled) a stream writer.
func newTurnRenderer(bot *Bot, msg *gotgbot.Message) *TurnRenderer {
	r := &TurnRenderer{
		bot:     bot,
		msg:     msg,
		chatID:  msg.Chat.Id,
		tracker: &toolCallTracker{bot: bot, chatID: msg.Chat.Id},
	}
	if bot.effectiveStreamOutput() {
		interval := bot.streamUpdateInterval
		if interval == 0 {
			interval = 250 * time.Millisecond
		}
		r.sw = newStreamWriter(bot.client, msg.Chat.Id, interval, bot.tableOpts())
	}
	return r
}

// Cleanup finishes the stream writer if it hasn't been finished yet.
// Safe to call from defer — Finish is idempotent.
func (r *TurnRenderer) Cleanup() {
	if r.sw != nil {
		r.sw.Finish()
	}
}

// onReply handles intermediate text delivery (ReplyFunc callback).
// When streaming is active, the text was already delivered via the stream
// writer. Finalize that message so the next API call's text goes to a fresh
// Telegram message (no duplicate send).
func (r *TurnRenderer) onReply(text string) {
	if r.sw != nil {
		msgID := r.sw.Finish()
		if msgID != 0 {
			content := r.sw.Content()
			if strings.TrimSpace(content) != "" {
				html := ConvertToTelegramHTML(content, r.bot.tableOpts())
				_, _, _ = r.bot.client.EditMessageText(html, &gotgbot.EditMessageTextOpts{
					ChatId:    r.chatID,
					MessageId: msgID,
					ParseMode: "HTML",
				})
			}
		}
		interval := r.bot.streamUpdateInterval
		if interval == 0 {
			interval = 250 * time.Millisecond
		}
		r.sw = newStreamWriter(r.bot.client, r.chatID, interval, r.bot.tableOpts())
		return
	}
	r.bot.sendReply(r.msg, text)
	r.tracker.resetMsgID()
}

// onThinking accumulates thinking blocks (gated by showThinking config).
func (r *TurnRenderer) onThinking(thinking string) {
	mode := r.bot.effectiveShowThinking()
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
	if r.sw != nil {
		r.sw.OnDelta(delta)
	}
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
	var streamMsgID int64
	if r.sw != nil {
		streamMsgID = r.sw.Finish()
		if streamContent := r.sw.Content(); strings.TrimSpace(response) == "" && strings.TrimSpace(streamContent) != "" {
			response = streamContent
		}
	}

	// Guard against empty responses.
	if strings.TrimSpace(response) == "" {
		r.bot.logger().Debugf("agent returned empty response, not sending")
		return
	}

	thinkingText := r.thinking.String()
	showThinkMode := r.bot.effectiveShowThinking()
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
		htmlResp := ConvertToTelegramHTML(response, r.bot.tableOpts())
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
	if editID == 0 || r.bot.effectiveShowToolCalls() != "preview" || hasThinking || len(response) > 4096 {
		return false
	}
	htmlResp := ConvertToTelegramHTML(response, r.bot.tableOpts())
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
		htmlEscapeBot(preview)+"\n\n<i>(full response below)</i>",
		&gotgbot.EditMessageTextOpts{
			ChatId:    r.chatID,
			MessageId: streamMsgID,
			ParseMode: "HTML",
		})
}

// sendWithFullThinking sends thinking (italic) + divider + response as a single message.
func (r *TurnRenderer) sendWithFullThinking(response, thinkingText string) {
	thinkingHTML := "<i>" + htmlEscapeBot(thinkingText) + "</i>"
	responseHTML := ConvertToTelegramHTML(response, r.bot.tableOpts())
	divider := "\n" + strings.Repeat("—", r.bot.effectiveDisplayWidth()) + "\n\n"
	r.bot.sendHTMLChunks(r.chatID, thinkingHTML+divider+responseHTML)
}

// sendWithCompactThinking sends a response with a "Show thinking" inline keyboard button.
func (r *TurnRenderer) sendWithCompactThinking(response, thinkingText string) {
	responseHTML := ConvertToTelegramHTML(response, r.bot.tableOpts())

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
	responseHTML := ConvertToTelegramHTML(response, r.bot.tableOpts())
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
	thinkingHTML := "<i>" + htmlEscapeBot(thinkingText) + "</i>"
	responseHTML := ConvertToTelegramHTML(response, r.bot.tableOpts())
	divider := "\n" + strings.Repeat("—", r.bot.effectiveDisplayWidth()) + "\n\n"
	combined := thinkingHTML + divider + responseHTML
	_, _, err := r.bot.client.EditMessageText(combined, &gotgbot.EditMessageTextOpts{
		ChatId:    r.chatID,
		MessageId: msgID,
		ParseMode: "HTML",
	})
	if err != nil {
		r.bot.logger().Errorf("edit stream with full thinking: %v", err)
	}
}
