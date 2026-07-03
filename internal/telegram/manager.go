package telegram

import "foci/internal/platform"

// BotManager and Pool are the shared generic implementations from the
// platform package, instantiated for Telegram bots. See platform/botpool.go
// for the acquire/release, LRU, TTL-reclaim, and lifecycle semantics.
type (
	BotManager = platform.BotManager[*Bot]
	Pool       = platform.Pool[*Bot]
)

// SessionActivityChecker is re-exported for callers that configure pool TTLs.
type SessionActivityChecker = platform.SessionActivityChecker

// NewBotManager creates an empty Telegram BotManager.
func NewBotManager() *BotManager {
	return platform.NewBotManager[*Bot]("telegram")
}
