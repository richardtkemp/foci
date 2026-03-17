package discord

import (
	"fmt"

	"foci/internal/session"
)

// ownsSession returns true if the session key's chat ID is known to this bot.
// Checks the in-memory cache first, then the session index (persisted across
// restarts). Lazily populates the cache on session index hit.
func (b *Bot) ownsSession(sessionKey string) bool {
	chatID := session.ChatIDFromKey(sessionKey)
	if chatID == 0 {
		return false
	}
	b.chatKeysMu.RLock()
	_, ok := b.chatSessionKeys[chatID]
	b.chatKeysMu.RUnlock()
	if ok {
		return true
	}
	// Check session index (survives restarts).
	if b.sessionIndex != nil && b.agentID != "" {
		if key, err := b.sessionIndex.GetChatMetadata(b.agentID, chatID, "session_key"); err == nil && key != "" {
			b.chatKeysMu.Lock()
			b.chatSessionKeys[chatID] = key
			b.chatKeysMu.Unlock()
			return true
		}
	}
	return false
}

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
		channelID := b.defaultChannelID()
		if channelID != 0 {
			return b.sessionKeyForMsg(channelID)
		}
	}
	return ""
}

// NewSessionKeyForChat creates a NEW session key for a specific chat ID.
// Each call generates a unique key. Use Bot.SessionKeyForChat for stable cached keys.
func NewSessionKeyForChat(agentID string, chatID int64) string {
	return session.NewChatSessionKey(agentID, chatID)
}

// SessionKeyForChat returns the stable, cached session key for a specific chat ID.
// Creates a new key on first call for a given chat, then returns the cached value.
func (b *Bot) SessionKeyForChat(chatID int64) string {
	return b.sessionKeyForMsg(chatID)
}

// sessionKeyForMsg returns the session key for the message's channel.
// Uses an in-memory cache and session index to avoid regenerating keys on every message.
// Persists new keys to the session index for continuity across restarts.
func (b *Bot) sessionKeyForMsg(chatID int64) string {
	// Check in-memory cache first
	b.chatKeysMu.RLock()
	if key, ok := b.chatSessionKeys[chatID]; ok {
		b.chatKeysMu.RUnlock()
		return key
	}
	b.chatKeysMu.RUnlock()

	// In-memory cache miss -- check session index (persisted across restarts)
	if b.sessionIndex != nil && b.agentID != "" {
		if persistedKey, err := b.sessionIndex.GetChatMetadata(b.agentID, chatID, "session_key"); err == nil && persistedKey != "" {
			// Found persisted key -- populate in-memory cache and return
			b.chatKeysMu.Lock()
			b.chatSessionKeys[chatID] = persistedKey
			b.chatKeysMu.Unlock()
			b.logger().Infof("restored session key for channel %d: %s", chatID, persistedKey)
			return persistedKey
		}
	}

	// No persisted key -- generate new key and persist it
	key := NewSessionKeyForChat(b.agentID, chatID)
	b.chatKeysMu.Lock()
	b.chatSessionKeys[chatID] = key
	b.chatKeysMu.Unlock()

	// Persist to session index for future restarts
	if b.sessionIndex != nil && b.agentID != "" {
		if err := b.sessionIndex.SetChatMetadata(b.agentID, chatID, "session_key", key); err != nil {
			b.logger().Errorf("persist session key for channel %d: %v", chatID, err)
		} else {
			b.logger().Infof("persisted new session key for channel %d: %s", chatID, key)
		}
	}

	return key
}

// UpdateChatSessionKey updates the cached and persisted session key for a chat.
// Called when a session key is rotated (compaction, /reset).
func (b *Bot) UpdateChatSessionKey(chatID int64, newKey string) {
	b.chatKeysMu.Lock()
	b.chatSessionKeys[chatID] = newKey
	b.chatKeysMu.Unlock()

	if b.sessionIndex != nil && b.agentID != "" {
		if err := b.sessionIndex.SetChatMetadata(b.agentID, chatID, "session_key", newKey); err != nil {
			b.logger().Errorf("update session key for channel %d: %v", chatID, err)
		} else {
			b.logger().Infof("rotated session key for channel %d: %s", chatID, newKey)
		}
	}
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
	return b.defaultChannelID()
}

// defaultChannelID reads the default channel from the state store.
func (b *Bot) defaultChannelID() int64 {
	if b.stateStore == nil || b.agentID == "" {
		return 0
	}
	var channelID int64
	if b.stateStore.Get("agent/"+b.agentID+"/default_channel", &channelID) {
		return channelID
	}
	return 0
}

// setDefaultChannel persists the default channel ID for this agent.
func (b *Bot) setDefaultChannel(channelID int64) {
	if b.stateStore == nil || b.agentID == "" {
		return
	}
	if err := b.stateStore.Set("agent/"+b.agentID+"/default_channel", channelID); err != nil {
		b.logger().Errorf("persist default channel: %v", err)
	}
}

// recordChannelUsername persists the username for a channel ID (for /sessions display).
func (b *Bot) recordChannelUsername(channelID int64, username string) {
	if b.stateStore == nil || b.agentID == "" || username == "" {
		return
	}
	key := fmt.Sprintf("agent/%s/chat/%d/username", b.agentID, channelID)
	if err := b.stateStore.Set(key, username); err != nil {
		b.logger().Errorf("persist channel username: %v", err)
	}
}

// DefaultSessionKey returns the session key for the default channel.
// Used by keepalive, cron, and other proactive features.
func (b *Bot) DefaultSessionKey() string {
	if b.agentID == "" {
		return ""
	}
	channelID := b.defaultChannelID()
	if channelID == 0 {
		return ""
	}
	return b.sessionKeyForMsg(channelID)
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
