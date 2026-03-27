package discord

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"foci/internal/platform"
	"foci/internal/session"

	"github.com/bwmarrin/discordgo"
)

// discordMaxChars is the maximum message length Discord allows.
const discordMaxChars = 2000

// sendMarkdownChunks sends markdown text to a channel, splitting into chunks
// that fit within Discord's 2000 character limit.
func (b *Bot) sendMarkdownChunks(channelID string, text string) {
	for _, chunk := range splitMessage(text, discordMaxChars) {
		if _, err := b.session.ChannelMessageSend(channelID, chunk); err != nil {
			var callers [4]string
			for i := range callers {
				_, file, line, ok := runtime.Caller(i + 1)
				if !ok {
					break
				}
				callers[i] = fmt.Sprintf("%s:%d", filepath.Base(file), line)
			}
			b.logger().Errorf("send error (channel=%s callers=%s): %s",
				channelID, strings.Join(callers[:], " <- "), b.sanitizeError(err))

			if isUnknownChannel(err) {
				b.clearStaleChannel(channelID)
				return // no point sending remaining chunks
			}
		}
	}
}

// isUnknownChannel returns true if the error is a Discord API 10003 "Unknown Channel".
func isUnknownChannel(err error) bool {
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) && restErr.Message != nil {
		return restErr.Message.Code == 10003
	}
	return false
}

// clearStaleChannel removes a channel that Discord reports as unknown from
// the session index so the bot stops trying to send to it.
func (b *Bot) clearStaleChannel(channelIDStr string) {
	channelID, err := strconv.ParseInt(channelIDStr, 10, 64)
	if err != nil {
		return
	}

	b.logger().Warnf("clearing stale channel %s", channelIDStr)

	// If this was the default channel, clear it so periodic tasks stop targeting it.
	if b.DefaultChatID() == channelID && b.sessionIndex != nil && b.agentID != "" {
		if err := b.sessionIndex.ClearDefaultChat(b.agentID, platformName); err != nil {
			b.logger().Errorf("failed to clear stale default channel: %v", err)
		} else {
			b.logger().Warnf("cleared stale default channel %s for agent %s", channelIDStr, b.agentID)
		}
	}

	// Clear the in-memory last-known channel if it matches.
	b.channelMu.Lock()
	if b.channelID == channelID {
		b.channelID = 0
	}
	b.channelMu.Unlock()
}

// sendReply sends a response back to the channel where the message originated.
func (b *Bot) sendReply(msg *discordgo.Message, response string) {
	response = strings.TrimSpace(response)
	if response == "" {
		return
	}
	b.sendMarkdownChunks(msg.ChannelID, response)
}

// SendNotification sends a plain text notification to the default channel.
// Used for system alerts (cache bust, etc.) -- not an agent turn, no tokens spent.
// Silently skips empty or whitespace-only messages.
// If an agent turn is active, the notification is buffered and sent after the turn ends.
func (b *Bot) SendNotification(text string) {
	if strings.TrimSpace(text) == "" {
		b.logger().Debugf("skipping empty notification")
		return
	}

	// Buffer during active turns to avoid interrupting streaming output.
	b.turnMu.Lock()
	active := b.turnCancel != nil
	b.turnMu.Unlock()
	if active {
		b.pendingNotifsMu.Lock()
		b.pendingNotifs = append(b.pendingNotifs, text)
		b.pendingNotifsMu.Unlock()
		return
	}

	b.sendNotificationImmediate(text)
}

// SendTyping sends a typing indicator to the current channel.
func (b *Bot) SendTyping() {
	channelID := b.DefaultChatID()
	if channelID == 0 {
		b.channelMu.Lock()
		channelID = b.channelID
		b.channelMu.Unlock()
	}
	if channelID != 0 {
		_ = b.session.ChannelTyping(fmt.Sprintf("%d", channelID))
	}
}

// SendNotificationDirect sends a notification immediately, bypassing the
// turn-active buffer. Use for time-sensitive notifications (e.g. compaction start)
// that must arrive before the turn ends.
func (b *Bot) SendNotificationDirect(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	b.sendNotificationImmediate(text)
}

// sendNotificationImmediate sends a notification directly to the default channel.
func (b *Bot) sendNotificationImmediate(text string) {
	channelID := b.DefaultChatID()
	if channelID == 0 {
		// Fall back to last known channel (e.g. when no state store is configured).
		b.channelMu.Lock()
		channelID = b.channelID
		b.channelMu.Unlock()
	}
	if channelID == 0 {
		truncated := text
		if len(truncated) > 40 {
			truncated = truncated[:40] + "..."
		}
		b.logger().Warnf("no channel ID for notification: %s", truncated)
		return
	}

	channelIDStr := strconv.FormatInt(channelID, 10)
	if _, err := b.session.ChannelMessageSend(channelIDStr, text); err != nil {
		b.logger().Errorf("send notification (channel=%s): %s", channelIDStr, b.sanitizeError(err))
		if isUnknownChannel(err) {
			b.clearStaleChannel(channelIDStr)
		}
	}
}

// drainPendingNotifications sends all buffered notifications to the default channel.
// Called after an agent turn ends. Atomically swaps the buffer to nil so new
// notifications arriving during drain go directly via sendNotificationImmediate.
func (b *Bot) drainPendingNotifications() {
	b.pendingNotifsMu.Lock()
	notifs := b.pendingNotifs
	b.pendingNotifs = nil
	b.pendingNotifsMu.Unlock()

	for _, text := range notifs {
		b.sendNotificationImmediate(text)
	}
}

// SendStartupNotification sends a startup notification to the last known channel.
// Skips silently if no channel ID is available (expected on first run or fresh state).
func (b *Bot) SendStartupNotification(agentID string) {
	b.channelMu.Lock()
	channelID := b.channelID
	b.channelMu.Unlock()

	if channelID == 0 {
		b.logger().Debugf("no channel ID for startup notification (no prior messages)")
		return
	}

	botName := b.botUserID
	if botName == "" {
		botName = "foci"
	}
	text := fmt.Sprintf("%s restarted at %s", botName, time.Now().Format("15:04:05"))

	channelIDStr := strconv.FormatInt(channelID, 10)
	if _, err := b.session.ChannelMessageSend(channelIDStr, text); err != nil {
		b.logger().Errorf("send startup notification (channel=%s): %s", channelIDStr, b.sanitizeError(err))
		if isUnknownChannel(err) {
			b.clearStaleChannel(channelIDStr)
		}
	}
}

// SendText sends a text message to the default channel without any header.
// Returns an error if no channel ID is available.
// Silently skips empty or whitespace-only messages.
func (b *Bot) RawSendText(text string) error {

	channelID := b.DefaultChatID()
	if channelID == 0 {
		// Fall back to last known channel.
		b.channelMu.Lock()
		channelID = b.channelID
		b.channelMu.Unlock()
	}
	if channelID == 0 {
		return fmt.Errorf("no channel ID -- no default channel configured")
	}

	channelIDStr := strconv.FormatInt(channelID, 10)
	b.sendMarkdownChunks(channelIDStr, text)
	return nil
}

// SendInjected sends a system/injected text message to the default channel.
// Prepends the configured InjectedMessageHeader (if non-empty) so users can
// distinguish system messages from agent replies.
//
// Prefer SendToSession when a session key is available -- it routes to the
// correct channel for chat-based sessions.
func (b *Bot) SendInjected(text string) error {
	if b.display.InjectedMessageHeader != "" && strings.TrimSpace(text) != "" {
		text = b.display.InjectedMessageHeader + "\n" + text
	}
	return platform.SendText(b, text)
}

// SendInjectedMessage sends a system/injected text message to the channel
// associated with the given session key. Falls back to the bot's default channel
// if the session key doesn't contain a chat ID (e.g. independent sessions).
// Prepends the configured InjectedMessageHeader (if non-empty).
func (b *Bot) SendInjectedMessage(sessionKey, text string) error {
	if b.display.InjectedMessageHeader != "" && strings.TrimSpace(text) != "" {
		text = b.display.InjectedMessageHeader + "\n" + text
	}
	return b.SendToSession(sessionKey, text)
}

// SendToSession sends a text message (without header) to the channel
// associated with the given session key. Falls back to the bot's default channel
// if the session key doesn't contain a chat ID.
func (b *Bot) SendToSession(sessionKey, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	chatID := session.ChatIDFromKey(sessionKey)
	if chatID == 0 {
		chatID = b.DefaultChatID()
	}
	if chatID == 0 {
		return fmt.Errorf("no channel ID for session %q and no default channel", sessionKey)
	}

	channelIDStr := strconv.FormatInt(chatID, 10)
	b.sendMarkdownChunks(channelIDStr, text)
	return nil
}

// sendToLastChannel resolves the last known channel ID and calls fn with it.
func (b *Bot) sendToLastChannel(fn func(int64, string) error, filePath string) error {
	channelID, err := b.lastChannelID()
	if err != nil {
		return err
	}
	return fn(channelID, filePath)
}

// lastChannelID returns the last known channel ID, or an error if none has been set.
func (b *Bot) lastChannelID() (int64, error) {
	b.channelMu.Lock()
	channelID := b.channelID
	b.channelMu.Unlock()
	if channelID == 0 {
		return 0, fmt.Errorf("no channel ID -- no messages received yet")
	}
	return channelID, nil
}

// SendDocument sends a file as a Discord attachment to the last known channel.
func (b *Bot) SendDocument(filePath string) error {
	return b.sendToLastChannel(b.SendDocumentToChat, filePath)
}

// SendVoice sends a voice file to the last known channel.
func (b *Bot) SendVoice(filePath string) error {
	return b.sendToLastChannel(b.SendVoiceToChat, filePath)
}

// SendVideo sends a video file to the last known channel.
func (b *Bot) SendVideo(filePath string) error {
	return b.sendToLastChannel(b.SendVideoToChat, filePath)
}

// SendPhoto sends a photo to the last known channel.
func (b *Bot) SendPhoto(filePath string) error {
	return b.sendToLastChannel(b.SendPhotoToChat, filePath)
}

// SendAudio sends an audio file to the last known channel.
func (b *Bot) SendAudio(filePath string) error {
	return b.sendToLastChannel(b.SendAudioToChat, filePath)
}

// SendAnimation sends an animation (GIF) to the last known channel.
func (b *Bot) SendAnimation(filePath string) error {
	return b.sendToLastChannel(b.SendAnimationToChat, filePath)
}

// SendVoiceData sends audio bytes as a Discord voice message to the last known channel.
func (b *Bot) SendVoiceData(audioData []byte) error {
	channelID, err := b.lastChannelID()
	if err != nil {
		return err
	}
	return b.SendVoiceDataToChat(channelID, audioData)
}

// SendVoiceDataToChat sends audio bytes as a Discord message attachment to a specific channel.
func (b *Bot) SendVoiceDataToChat(chatID int64, audioData []byte) error {
	channelIDStr := strconv.FormatInt(chatID, 10)
	_, err := b.session.ChannelMessageSendComplex(channelIDStr, &discordgo.MessageSend{
		Files: []*discordgo.File{
			{
				Name:   "voice.mp3",
				Reader: bytes.NewReader(audioData),
			},
		},
	})
	return err
}

// RawSendTextToChat sends a text message to a specific channel ID without any header.
func (b *Bot) RawSendTextToChat(chatID int64, text string) error {
	channelIDStr := strconv.FormatInt(chatID, 10)
	b.sendMarkdownChunks(channelIDStr, text)
	return nil
}

// SendInjectedToChat sends an injected/system text message to a specific channel ID.
// Prepends the configured InjectedMessageHeader (if non-empty).
func (b *Bot) SendInjectedToChat(chatID int64, text string) error {
	if b.display.InjectedMessageHeader != "" && strings.TrimSpace(text) != "" {
		text = b.display.InjectedMessageHeader + "\n" + text
	}
	return platform.SendTextToChat(b, chatID, text)
}

// sendMediaFile is a generic helper for sending media files to Discord.
func (b *Bot) sendMediaFile(chatID int64, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer func() { _ = f.Close() }()

	channelIDStr := strconv.FormatInt(chatID, 10)
	_, err = b.session.ChannelMessageSendComplex(channelIDStr, &discordgo.MessageSend{
		Files: []*discordgo.File{
			{
				Name:   filepath.Base(filePath),
				Reader: f,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("send file: %w", err)
	}
	return nil
}

// SendDocumentToChat sends a file as a Discord attachment to a specific channel ID.
func (b *Bot) SendDocumentToChat(chatID int64, filePath string) error {
	return b.sendMediaFile(chatID, filePath)
}

// SendVoiceToChat sends a voice file to a specific channel ID.
func (b *Bot) SendVoiceToChat(chatID int64, filePath string) error {
	return b.sendMediaFile(chatID, filePath)
}

// SendVideoToChat sends a video file to a specific channel ID.
func (b *Bot) SendVideoToChat(chatID int64, filePath string) error {
	return b.sendMediaFile(chatID, filePath)
}

// SendPhotoToChat sends a photo to a specific channel ID.
func (b *Bot) SendPhotoToChat(chatID int64, filePath string) error {
	return b.sendMediaFile(chatID, filePath)
}

// SendAudioToChat sends an audio file to a specific channel ID.
func (b *Bot) SendAudioToChat(chatID int64, filePath string) error {
	return b.sendMediaFile(chatID, filePath)
}

// SendAnimationToChat sends an animation (GIF) to a specific channel ID.
func (b *Bot) SendAnimationToChat(chatID int64, filePath string) error {
	return b.sendMediaFile(chatID, filePath)
}
