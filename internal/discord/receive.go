package discord

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"foci/internal/platform"
	"foci/internal/toolformat"

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
	b.mq.Enqueue(b.toPlatformMessage(msg, qm))
}

// toPlatformMessage converts a discord queuedMessage to a platform.QueuedMessage.
func (b *Bot) toPlatformMessage(msg *discordgo.Message, qm queuedMessage) platform.QueuedMessage {
	var atts []platform.Attachment
	for _, a := range qm.attachments {
		atts = append(atts, platform.Attachment{
			MimeType:  a.mediaType,
			Data:      a.data,
			SavedPath: a.savedPath,
		})
	}

	channelID, _ := strconv.ParseInt(msg.ChannelID, 10, 64)
	isGroup := msg.GuildID != ""
	isMention := isGroup && b.messageContainsMention(msg)

	senderName := ""
	if msg.Author != nil {
		senderName = msg.Author.Username
	}

	return platform.QueuedMessage{
		UserID:      qm.userID,
		SenderName:  senderName,
		Text:        qm.text,
		Attachments: atts,
		ChatID:      channelID,
		IsGroupChat: isGroup,
		IsMention:   isMention,
		Original:    msg,
	}
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

	_ = changed // channelID tracked in-memory only; DB is source of truth for default

	// Per-chat session routing: set default channel on first message, record username
	if !b.isSecondary && b.agentID != "" && b.sessionIndex != nil {
		if chatID, _ := b.sessionIndex.DefaultChatForAgent(b.agentID); chatID == 0 {
			if err := b.sessionIndex.SetDefaultChat(b.agentID, platformName, channelID); err != nil {
				b.logger().Errorf("set default channel: %v", err)
			} else {
				b.logger().Infof("set default channel %d for agent %s", channelID, b.agentID)
			}
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


// truncate is a package-local alias for toolformat.Truncate, used throughout
// the discord package for log messages, stream previews, etc.
func truncate(s string, max int) string {
	return toolformat.Truncate(s, max)
}
