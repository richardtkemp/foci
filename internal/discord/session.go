package discord

// platformName is the platform identifier used for chat_metadata queries.
const platformName = "discord"

// SessionKey returns the current session key (thread-safe).
// For primary bots, this returns the session key for the default channel.
// For secondary bots, this returns the override session key (set by facet).
func (b *Bot) SessionKey() string {
	b.sessionMu.RLock()
	defer b.sessionMu.RUnlock()
	if b.sessionKey != "" {
		return b.sessionKey
	}
	// Primary bot: derive from default channel
	if b.agentID != "" {
		channelID := b.DefaultChatID()
		if channelID != 0 {
			return b.sessionKeyForMsg(channelID)
		}
	}
	return ""
}

// SessionKeyForChat returns the stable session key for a specific chat ID.
// Creates a new key on first call for a given chat, then returns the persisted value.
func (b *Bot) SessionKeyForChat(chatID int64) string {
	return b.sessionKeyForMsg(chatID)
}

// sessionKeyForMsg returns the session key for the message's channel.
// Delegates to the shared chatmeta.Resolver.
func (b *Bot) sessionKeyForMsg(chatID int64) string {
	return b.chatmeta.SessionKeyForChat(chatID)
}

// UpdateChatSessionKey updates the persisted session key for a chat.
// Called when a session key is rotated (compaction, /reset).
func (b *Bot) UpdateChatSessionKey(chatID int64, newKey string) {
	b.chatmeta.UpdateSessionKey(chatID, newKey)
}

// SetSessionKey changes the override session key (used for facet fork/done).
// If OnSessionKeyChange is set, fires it outside the lock with the bot's ID and new key.
func (b *Bot) SetSessionKey(key string) {
	b.sessionMu.Lock()
	cb := b.OnSessionKeyChange
	b.sessionKey = key
	b.sessionMu.Unlock()

	if cb != nil {
		cb(b.Username(), key)
	}
}

// SetSessionKeyDirect sets the session key without firing the OnSessionKeyChange callback.
// Used during restoration to avoid re-persisting an already-persisted value.
func (b *Bot) SetSessionKeyDirect(key string) {
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	b.sessionKey = key
}

// DefaultChatID returns the default channel ID for this agent (used for proactive messages).
func (b *Bot) DefaultChatID() int64 {
	return b.chatmeta.DefaultChatID()
}

// recordChannelUsername persists the username for a channel ID (for /sessions display).
func (b *Bot) recordChannelUsername(channelID int64, username string) {
	b.chatmeta.RecordUsername(channelID, username)
}

// DefaultSessionKey returns the session key for the default channel.
// Used by keepalive, cron, and other proactive features.
func (b *Bot) DefaultSessionKey() string {
	return b.chatmeta.DefaultSessionKey()
}

// Username returns the bot's Discord user ID string.
// Returns "" if the bot user ID is not set.
func (b *Bot) Username() string {
	return b.botUserID
}

// ChatID returns the last known channel ID.
func (b *Bot) ChatID() int64 {
	b.channelMu.Lock()
	defer b.channelMu.Unlock()
	return b.channelID
}

// SetChatID sets the channel ID (used for facet notification delivery).
func (b *Bot) SetChatID(id int64) {
	b.channelMu.Lock()
	b.channelID = id
	b.channelMu.Unlock()
}
