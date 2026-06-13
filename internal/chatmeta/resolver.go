// Package chatmeta provides shared session key management logic used by
// platform-specific bot implementations (telegram, discord, etc.).
//
// The Resolver handles looking up, creating, persisting, and rotating
// per-chat session keys via a platform.SessionIndex backend. Each platform
// bot embeds or holds a *Resolver and delegates session key operations to it.
//
// All Resolver methods are nil-receiver safe: calling on a nil *Resolver
// returns zero values without panicking.
package chatmeta

import (
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
)

// Resolver manages per-chat session keys backed by a platform.SessionIndex.
// All methods are safe to call on a nil receiver or when Index is nil.
type Resolver struct {
	Index        platform.SessionIndex // may be nil (no persistence)
	AgentID      string
	PlatformName string
	Logger       func() *log.ComponentLogger // lazy logger (borrows the bot's logger)
}

// SessionKeyForChat returns the stable session key for a chat ID.
// Looks up the persisted key first; creates and persists a new one if none exists.
func (r *Resolver) SessionKeyForChat(chatID int64) string {
	if r == nil {
		return session.NewChatSessionKey("", chatID)
	}

	// Check DB for existing key.
	if r.Index != nil && r.AgentID != "" {
		if key, err := r.Index.GetChatMetadata(r.AgentID, r.PlatformName, chatID, "session_key"); err == nil && key != "" {
			return key
		}
	}

	// No persisted key -- generate and persist.
	key := session.NewChatSessionKey(r.AgentID, chatID)
	if r.Index != nil && r.AgentID != "" {
		if err := r.Index.SetChatMetadata(r.AgentID, r.PlatformName, chatID, "session_key", key); err != nil {
			r.Logger().Errorf("persist session key for chat %d: %v", chatID, err)
		} else {
			r.Logger().Infof("persisted new session key for chat %d: %s", chatID, key)
		}
	}

	return key
}

// UpdateSessionKey updates the persisted session key for a chat.
// Called when a session key is rotated (compaction, /reset).
func (r *Resolver) UpdateSessionKey(chatID int64, newKey string) {
	if r == nil {
		return
	}
	if r.Index != nil && r.AgentID != "" {
		if err := r.Index.SetChatMetadata(r.AgentID, r.PlatformName, chatID, "session_key", newKey); err != nil {
			r.Logger().Errorf("update session key for chat %d: %v", chatID, err)
		} else {
			r.Logger().Infof("rotated session key for chat %d: %s", chatID, newKey)
		}
	}
}

// DefaultChatID returns the default chat ID for this agent on this platform.
// Returns 0 if no default is set.
func (r *Resolver) DefaultChatID() int64 {
	if r == nil || r.Index == nil || r.AgentID == "" {
		return 0
	}
	return r.Index.DefaultChatForAgent(r.AgentID, r.PlatformName)
}

// RecordUsername persists the username for a chat ID (for /sessions display).
func (r *Resolver) RecordUsername(chatID int64, username string) {
	if r == nil || r.Index == nil || r.AgentID == "" || username == "" {
		return
	}
	if err := r.Index.SetChatMetadata(r.AgentID, r.PlatformName, chatID, "username", username); err != nil {
		r.Logger().Errorf("persist chat username: %v", err)
	}
}

// DefaultSessionKey returns the session key for the default chat.
// Returns "" if no agent ID is set or no default chat exists.
func (r *Resolver) DefaultSessionKey() string {
	if r == nil || r.AgentID == "" {
		return ""
	}
	chatID := r.DefaultChatID()
	if chatID == 0 {
		return ""
	}
	return r.SessionKeyForChat(chatID)
}
