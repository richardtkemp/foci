package discord

import (
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// TurnRenderer encapsulates all per-turn rendering state: streaming, thinking
// accumulation, tool call tracking, and response finalization.
type TurnRenderer struct {
	bot       *Bot
	msg       *discordgo.Message
	channelID string
	display   turnDisplay
	sw        *streamWriter
	tracker   *toolCallTracker
	thinking  strings.Builder
}

// newTurnRenderer creates a TurnRenderer with a tool call tracker and a stream
// writer. The stream writer is always present but only sends messages when
// streaming is enabled (live mode). The session key is used to resolve
// per-session display overrides.
func newTurnRenderer(bot *Bot, msg *discordgo.Message, sessionKey string) *TurnRenderer {
	d := bot.resolveDisplay(sessionKey)
	return &TurnRenderer{
		bot:       bot,
		msg:       msg,
		channelID: msg.ChannelID,
		display:   d,
		sw:        newStreamWriter(bot, msg.ChannelID, bot.streamInterval(), d.streamOutput),
		tracker:   &toolCallTracker{bot: bot, channelID: msg.ChannelID, display: d},
	}
}

// Cleanup finishes the stream writer if it hasn't been finished yet.
// Safe to call from defer -- Finish is idempotent.
func (r *TurnRenderer) Cleanup() {
	r.sw.Finish()
}

// onReply handles intermediate text delivery (ReplyFunc callback).
func (r *TurnRenderer) onReply(text string) {
	msgID := r.sw.Finish()
	if msgID != "" {
		// Streaming: reply content is in the stream message. Finalize it.
		content := r.sw.Content()
		if strings.TrimSpace(content) != "" {
			_, _ = r.bot.session.ChannelMessageEdit(r.channelID, msgID, content)
		}
		r.tracker.cleanupPreview()
	} else if !r.display.streamOutput {
		// Non-streaming: try editing tool call preview with reply text.
		if !r.editToolPreviewWithReply(text) {
			r.bot.sendReply(r.msg, text)
		}
		r.tracker.resetMsgID()
	}
	// Fresh stream writer for the next segment.
	r.sw = newStreamWriter(r.bot, r.channelID, r.bot.streamInterval(), r.display.streamOutput)
}

// editToolPreviewWithReply edits the tool call preview message with intermediate
// reply text. Returns true if the edit succeeded.
func (r *TurnRenderer) editToolPreviewWithReply(text string) bool {
	editID := r.tracker.lastMsgID()
	if editID == "" || r.display.showToolCalls != "preview" {
		return false
	}
	if strings.TrimSpace(text) == "" || len(text) > discordMaxChars {
		r.tracker.cleanupPreview()
		return false
	}
	_, err := r.bot.session.ChannelMessageEdit(r.channelID, editID, text)
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
	_ = r.bot.session.ChannelTyping(r.channelID)
}

// onActivity refreshes the typing indicator when tools complete.
func (r *TurnRenderer) onActivity() {
	_ = r.bot.session.ChannelTyping(r.channelID)
}

// Finalize renders the final agent response to Discord.
func (r *TurnRenderer) Finalize(response string) {
	streamMsgID := r.sw.Finish()
	if streamContent := r.sw.Content(); strings.TrimSpace(response) == "" && strings.TrimSpace(streamContent) != "" {
		response = streamContent
	}

	if strings.TrimSpace(response) == "" {
		r.bot.logger().Debugf("agent returned empty response, not sending")
		return
	}

	thinkingText := r.thinking.String()
	showThinkMode := r.display.showThinking
	hasThinking := thinkingText != "" && showThinkMode != "off" && showThinkMode != ""

	// Stream finalization: edit the stream message in-place when possible.
	if streamMsgID != "" && len(response) <= discordMaxChars {
		r.finalizeStreamShort(streamMsgID, response, thinkingText, showThinkMode, hasThinking)
		r.tracker.cleanupPreview()
		return
	}

	// Stream message exists but response is too long -- send as new message(s).
	if streamMsgID != "" {
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

// finalizeStreamShort edits the stream message in-place with the final response.
func (r *TurnRenderer) finalizeStreamShort(streamMsgID, response, thinkingText, showThinkMode string, hasThinking bool) {
	switch {
	case hasThinking && showThinkMode == "compact":
		r.editStreamWithThinking(streamMsgID, response, thinkingText)
	case hasThinking && showThinkMode == "true":
		r.editStreamWithFullThinking(streamMsgID, response, thinkingText)
	default:
		_, err := r.bot.session.ChannelMessageEdit(r.channelID, streamMsgID, response)
		if err != nil {
			r.bot.logger().Debugf("edit stream final: %v", err)
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

// tryEditToolPreview attempts to edit the tool call preview message with the final response.
func (r *TurnRenderer) tryEditToolPreview(response string, hasThinking bool) bool {
	editID := r.tracker.lastMsgID()
	if editID == "" || r.display.showToolCalls != "preview" || hasThinking || len(response) > discordMaxChars {
		return false
	}
	_, err := r.bot.session.ChannelMessageEdit(r.channelID, editID, response)
	if err != nil {
		r.bot.logger().Debugf("edit final response failed, falling back: %v", err)
		return false
	}
	return true
}

// editStreamPreview edits the stream message to a truncated preview.
func (r *TurnRenderer) editStreamPreview(streamMsgID, response string) {
	if streamMsgID == "" {
		return
	}
	preview := truncate(response, 200)
	text := preview + "\n\n*(full response below)*"
	_, _ = r.bot.session.ChannelMessageEdit(r.channelID, streamMsgID, text)
}

// sendWithFullThinking sends thinking (italic) + divider + response as a single message.
func (r *TurnRenderer) sendWithFullThinking(response, thinkingText string) {
	divider := "\n" + strings.Repeat("-", r.display.displayWidth) + "\n\n"
	combined := "*" + thinkingText + "*" + divider + response
	r.bot.sendMarkdownChunks(r.channelID, combined)
}

// sendWithCompactThinking sends a response with a "Show thinking" button.
func (r *TurnRenderer) sendWithCompactThinking(response, thinkingText string) {
	chunks := splitMessage(response, discordMaxChars)
	for i, chunk := range chunks {
		if i < len(chunks)-1 {
			r.bot.sendMarkdownChunks(r.channelID, chunk)
			continue
		}
		buttons := singleButton("Show thinking", "th:show")
		sent, err := r.bot.session.ChannelMessageSendComplex(r.channelID, &discordgo.MessageSend{
			Content:    chunk,
			Components: buttons,
		})
		if err != nil {
			r.bot.logger().Errorf("send reply with thinking button: %v", err)
			return
		}
		msgIDInt, _ := strconv.ParseInt(sent.ID, 10, 64)
		r.bot.thinkingStore.Store(msgIDInt, thinkingEntry{
			responseText: chunk,
			thinkingText: thinkingText,
		})
	}
}

// editStreamWithThinking edits the stream message in-place with the final response
// and a "Show thinking" button.
func (r *TurnRenderer) editStreamWithThinking(msgID, response, thinkingText string) {
	buttons := singleButton("Show thinking", "th:show")
	_, err := r.bot.session.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    r.channelID,
		ID:         msgID,
		Content:    &response,
		Components: &buttons,
	})
	if err != nil {
		r.bot.logger().Errorf("edit stream with thinking button: %v", err)
		return
	}
	msgIDInt, _ := strconv.ParseInt(msgID, 10, 64)
	r.bot.thinkingStore.Store(msgIDInt, thinkingEntry{
		responseText: response,
		thinkingText: thinkingText,
	})
}

// editStreamWithFullThinking edits the stream message with thinking + divider + response.
func (r *TurnRenderer) editStreamWithFullThinking(msgID, response, thinkingText string) {
	divider := "\n" + strings.Repeat("-", r.display.displayWidth) + "\n\n"
	combined := "*" + thinkingText + "*" + divider + response
	_, err := r.bot.session.ChannelMessageEdit(r.channelID, msgID, combined)
	if err != nil {
		r.bot.logger().Errorf("edit stream with full thinking: %v", err)
	}
}
