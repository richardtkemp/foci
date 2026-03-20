package telegram

// platformName is the platform identifier used for chat_metadata queries.
const platformName = "telegram"

// SessionKey returns the current session key (thread-safe).
// For primary bots, this returns the session key for the default chat.
// For secondary bots, this returns the override session key (set by facet).
func (b *Bot) SessionKey() string {
	b.sessionMu.RLock()
	defer b.sessionMu.RUnlock()
	if b.sessionKey != "" {
		return b.sessionKey
	}
	// Primary bot: derive from default chat
	if b.agentID != "" {
		chatID := b.DefaultChatID()
		if chatID != 0 {
			return b.sessionKeyForMsg(chatID)
		}
	}
	return ""
}

// SessionKeyForChat returns the stable session key for a specific chat ID.
// Creates a new key on first call for a given chat, then returns the persisted value.
func (b *Bot) SessionKeyForChat(chatID int64) string {
	return b.sessionKeyForMsg(chatID)
}

// sessionKeyForMsg returns the session key for the message's chat.
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
// If OnSessionKeyChange is set, fires it outside the lock with the bot's username and new key.
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

// DefaultChatID returns the default chat ID for this agent (used for proactive messages).
func (b *Bot) DefaultChatID() int64 {
	return b.chatmeta.DefaultChatID()
}

// recordChatUsername persists the username for a chat ID (for /sessions display).
func (b *Bot) recordChatUsername(chatID int64, username string) {
	b.chatmeta.RecordUsername(chatID, username)
}

// DefaultSessionKey returns the session key for the default chat.
// Used by keepalive, cron, and other proactive features.
func (b *Bot) DefaultSessionKey() string {
	return b.chatmeta.DefaultSessionKey()
}

// Username returns the bot's Telegram username.
// Returns "" if the API client is not set (e.g. test bots).
func (b *Bot) Username() string {
	if b.api == nil {
		return ""
	}
	return b.api.Username
}

// ChatID returns the last known chat ID.
func (b *Bot) ChatID() int64 {
	b.chatMu.Lock()
	defer b.chatMu.Unlock()
	return b.chatID
}

// SetChatID sets the chat ID (used for facet notification delivery).
func (b *Bot) SetChatID(id int64) {
	b.chatMu.Lock()
	b.chatID = id
	b.chatMu.Unlock()
}
