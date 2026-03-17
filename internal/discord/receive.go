package discord

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"foci/internal/platform"

	"github.com/bwmarrin/discordgo"
)

// formatUserInfo returns a display string for a Discord user.
func formatUserInfo(user *discordgo.User) string {
	if user.Username != "" {
		return fmt.Sprintf("%s (%s)", user.ID, user.Username)
	}
	return user.ID
}

func (b *Bot) receiveMessage(ctx context.Context, msg *discordgo.Message) {
	qm, ok := b.buildReceivedMessage(ctx, msg)
	if !ok {
		return
	}
	if b.tryIntercept(ctx, &qm) {
		return
	}
	b.enqueue(qm)
}

// buildReceivedMessage performs auth, text extraction, and attachment downloading.
// Returns a populated queuedMessage and true, or zero value and false if the
// message should be silently dropped (unauthorized, empty, or failed voice).
func (b *Bot) buildReceivedMessage(_ context.Context, msg *discordgo.Message) (queuedMessage, bool) {
	userID := msg.Author.ID

	if !b.allowedUsers[userID] {
		b.logger().Warnf("rejected message from %s", formatUserInfo(msg.Author))
		return queuedMessage{}, false
	}

	// Parse channel ID to int64 for session routing
	channelID, _ := strconv.ParseInt(msg.ChannelID, 10, 64)

	// Remember channel ID for notifications
	b.channelMu.Lock()
	changed := b.channelID != channelID
	b.channelID = channelID
	b.channelMu.Unlock()

	if changed && b.sessionIndex != nil && b.agentID != "" {
		if err := b.sessionIndex.SetAgentMetadata(b.agentID, "bot_channel_id", fmt.Sprintf("%d", channelID)); err != nil {
			b.logger().Errorf("persist channel ID: %v", err)
		}
	}

	// Per-chat session routing: set default channel on first message, record username
	if !b.isSecondary && b.agentID != "" {
		if b.defaultChannelID() == 0 {
			b.setDefaultChannel(channelID)
			b.logger().Infof("set default channel %d for agent %s", channelID, b.agentID)
		}
		if msg.Author != nil {
			b.recordChannelUsername(channelID, msg.Author.Username)
		}
	}

	// Record last real user activity (for --if-active gating on CLI commands).
	if !b.isSecondary && b.agentID != "" && b.sessionIndex != nil {
		_ = b.sessionIndex.SetAgentMetadata(b.agentID, "last_user_activity", fmt.Sprintf("%d", time.Now().Unix()))
	}
	if b.OnUserMessage != nil {
		b.OnUserMessage()
	}

	// Get text from message content
	text := msg.Content

	// Strip bot mention from text
	if b.botUserID != "" {
		text = strings.ReplaceAll(text, "<@"+b.botUserID+">", "")
		text = strings.ReplaceAll(text, "<@!"+b.botUserID+">", "")
		text = strings.TrimSpace(text)
	}

	// Handle reply context
	if msg.ReferencedMessage != nil && msg.ReferencedMessage.Content != "" {
		text = fmt.Sprintf("[Replying to: %s]\n\n%s", msg.ReferencedMessage.Content, text)
	}

	// Download attachments
	var attachments []attachment
	for _, att := range msg.Attachments {
		if att == nil {
			continue
		}
		if isImageMIME(att.ContentType) || isPDFMIME(att.ContentType) || platform.IsConvertibleDocMIME(att.ContentType) {
			if downloaded, ok := b.downloadAttachment(att); ok {
				attachments = append(attachments, downloaded)
			}
		}
	}

	// Drop messages with no text and no attachments
	if text == "" && len(attachments) == 0 {
		return queuedMessage{}, false
	}

	logText := text
	if len(attachments) > 0 {
		logText = fmt.Sprintf("[%d attachment(s)] %s", len(attachments), text)
	}
	if b.display.MessagesInLog {
		b.logger().Infof("message from %s: %s", formatUserInfo(msg.Author), truncate(logText, 100))
	} else {
		b.logger().Debugf("message from %s", formatUserInfo(msg.Author))
	}

	return queuedMessage{msg: msg, userID: userID, text: text, attachments: attachments}, true
}

// tryIntercept handles local consumption of a message: wizard intercept,
// last-message recording, stale command drops, command dispatch, message
// transforms, and secondary bot idle drops. Returns true if the message
// was consumed and should not be enqueued.
func (b *Bot) tryIntercept(ctx context.Context, qm *queuedMessage) bool {
	// Wizard intercept -- route all messages to active wizard before normal dispatch
	if qm.text != "" {
		if result, ok := b.commands.HandleMessage(qm.text); ok {
			b.sendReply(qm.msg, result)
			return true
		}
	}

	// Record the message for // (repeat) command
	if qm.text != "" && !strings.HasPrefix(qm.text, "/") && !strings.HasPrefix(qm.text, ".") {
		b.lastMsgStore.Record(qm.userID, qm.text)
	}

	// Try dispatching the original message as a command (slash or dot-prefix).
	if b.tryDispatchCommand(ctx, qm.msg, qm.text) {
		return true
	}

	// Apply message transforms to non-command messages.
	if b.handler != nil {
		if transformed := b.handler.TransformMessage(qm.text); transformed != qm.text {
			qm.text = transformed
			if b.tryDispatchCommand(ctx, qm.msg, qm.text) {
				return true
			}
		}
	}

	// Secondary bots with no session silently drop non-command messages.
	if b.isSecondary && b.SessionKey() == "" {
		b.logger().Debugf("dropping message to idle secondary bot from %s", formatUserInfo(qm.msg.Author))
		return true
	}

	return false
}

// enqueue delivers the message to the steer buffer (if a turn is active and
// steer mode is on) or to the main message queue.
func (b *Bot) enqueue(qm queuedMessage) {
	// Steer mode: if a turn is active, route text to the steer buffer
	if b.display.SteerMode && qm.text != "" && len(qm.attachments) == 0 {
		b.turnMu.Lock()
		active := b.turnCancel != nil
		b.turnMu.Unlock()
		if active {
			b.appendSteer(qm.text)
			b.logger().Infof("steer: buffered message from %s", formatUserInfo(qm.msg.Author))
			return
		}
	}

	select {
	case b.queue <- qm:
	default:
		b.logger().Warnf("message queue full, dropping message from %s", formatUserInfo(qm.msg.Author))
		b.sendReply(qm.msg, "Busy -- message queue is full. Try again shortly.")
	}
}

// truncate shortens a string to max characters, appending "..." if truncated.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
