package discord

import (
	"strconv"

	"foci/internal/platform"
	"foci/internal/session"
)

// BuildTurnObservers returns platform-specific tool call observer callbacks
// for the given session key. Returns nil if there is no channel to send to.
// The returned observers wrap a toolCallTracker which handles display mode
// checks internally (e.g. showToolCalls "off" → observeToolCall is a no-op).
func (b *Bot) BuildTurnObservers(sessionKey string) *platform.TurnObservers {
	chatID := session.ChatIDFromKey(sessionKey)
	if chatID == 0 {
		chatID = b.DefaultChatID()
	}
	if chatID == 0 {
		return nil
	}

	channelID := strconv.FormatInt(chatID, 10)
	d := b.resolveDisplay(sessionKey)
	tracker := &toolCallTracker{bot: b, channelID: channelID, display: d}

	return &platform.TurnObservers{
		OnToolCall:   tracker.observeToolCall,
		OnToolResult: tracker.observeToolResult,
		OnRetry:      tracker.notifyRetry,
		OnRetryClear: tracker.clearRetryNotification,
		Cleanup:      tracker.cleanupPreview,
	}
}
