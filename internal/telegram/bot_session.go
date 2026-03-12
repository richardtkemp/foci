package telegram

import (
	"fmt"

	"foci/internal/session"
)

// SessionKey returns the current session key (thread-safe).
// For primary bots, this returns the session key for the default chat.
// For secondary bots, this returns the override session key (set by multiball).
func (b *Bot) SessionKey() string {
	b.sessionMu.RLock()
	defer b.sessionMu.RUnlock()
	if b.sessionKey != "" {
		return b.sessionKey
	}
	// Primary bot: derive from default chat
	if b.agentID != "" {
		chatID := b.defaultChatID()
		if chatID != 0 {
			return b.sessionKeyForMsg(chatID)
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

// sessionKeyForMsg returns the session key for the message's chat.
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

	// In-memory cache miss — check session index (persisted across restarts)
	if b.sessionIndex != nil && b.agentID != "" {
		if persistedKey, err := b.sessionIndex.GetChatMetadata(b.agentID, chatID, "session_key"); err == nil && persistedKey != "" {
			// Found persisted key — populate in-memory cache and return
			b.chatKeysMu.Lock()
			b.chatSessionKeys[chatID] = persistedKey
			b.chatKeysMu.Unlock()
			b.logger().Infof("restored session key for chat %d: %s", chatID, persistedKey)
			return persistedKey
		}
	}

	// No persisted key — generate new key and persist it
	key := NewSessionKeyForChat(b.agentID, chatID)
	b.chatKeysMu.Lock()
	b.chatSessionKeys[chatID] = key
	b.chatKeysMu.Unlock()

	// Persist to session index for future restarts
	if b.sessionIndex != nil && b.agentID != "" {
		if err := b.sessionIndex.SetChatMetadata(b.agentID, chatID, "session_key", key); err != nil {
			b.logger().Errorf("persist session key for chat %d: %v", chatID, err)
		} else {
			b.logger().Infof("persisted new session key for chat %d: %s", chatID, key)
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
			b.logger().Errorf("update session key for chat %d: %v", chatID, err)
		} else {
			b.logger().Infof("rotated session key for chat %d: %s", chatID, newKey)
		}
	}
}

// SetSessionKey changes the override session key (used for multiball fork/done).
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
	return b.defaultChatID()
}

// defaultChatID reads the default chat from the state store.
func (b *Bot) defaultChatID() int64 {
	if b.stateStore == nil || b.agentID == "" {
		return 0
	}
	var chatID int64
	if b.stateStore.Get("agent/"+b.agentID+"/default_chat", &chatID) {
		return chatID
	}
	return 0
}

// setDefaultChat persists the default chat ID for this agent.
func (b *Bot) setDefaultChat(chatID int64) {
	if b.stateStore == nil || b.agentID == "" {
		return
	}
	if err := b.stateStore.Set("agent/"+b.agentID+"/default_chat", chatID); err != nil {
		b.logger().Errorf("persist default chat: %v", err)
	}
}

// recordChatUsername persists the username for a chat ID (for /sessions display).
func (b *Bot) recordChatUsername(chatID int64, username string) {
	if b.stateStore == nil || b.agentID == "" || username == "" {
		return
	}
	key := fmt.Sprintf("agent/%s/chat/%d/username", b.agentID, chatID)
	if err := b.stateStore.Set(key, username); err != nil {
		b.logger().Errorf("persist chat username: %v", err)
	}
}

// DefaultSessionKey returns the session key for the default chat.
// Used by keepalive, cron, and other proactive features.
func (b *Bot) DefaultSessionKey() string {
	if b.agentID == "" {
		return ""
	}
	chatID := b.defaultChatID()
	if chatID == 0 {
		return ""
	}
	return b.sessionKeyForMsg(chatID)
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

// SetChatID sets the chat ID (used for multiball notification delivery).
func (b *Bot) SetChatID(id int64) {
	b.chatMu.Lock()
	b.chatID = id
	b.chatMu.Unlock()
}
