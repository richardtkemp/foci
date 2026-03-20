package telegram

import (
	"foci/internal/platform"
	"foci/internal/session"
)

// BuildTurnObservers returns platform-specific tool call observer callbacks
// for the given session key. Returns nil if there is no chat to send to.
// The returned observers wrap a shared turn.ToolCallTracker which handles
// display mode checks internally (e.g. showToolCalls "off" → no-op).
func (b *Bot) BuildTurnObservers(sessionKey string) *platform.TurnObservers {
	chatID := session.ChatIDFromKey(sessionKey)
	if chatID == 0 {
		chatID = b.DefaultChatID()
	}
	if chatID == 0 {
		return nil
	}

	d := b.resolveDisplay(sessionKey)
	tracker := newToolCallTracker(b, chatID, d)

	return &platform.TurnObservers{
		OnToolCall:   tracker.ObserveToolCall,
		OnToolResult: tracker.ObserveToolResult,
		OnRetry:      tracker.NotifyRetry,
		OnRetryClear: tracker.ClearRetryNotification,
		Cleanup:      tracker.CleanupPreview,
	}
}
