// Package chatmeta provides shared per-chat metadata logic used by
// platform-specific bot implementations (telegram, discord, app).
//
// Session keys are deterministic (session.NewChatSessionKey), so the Resolver
// no longer stores keys — its job is registering which platform owns each chat
// (the routing oracle behind SessionIndex.PlatformForChat) and handling
// per-chat metadata like usernames and the default-chat flag.
//
// All Resolver methods are nil-receiver safe: calling on a nil *Resolver
// returns zero values without panicking.
package chatmeta

import (
	"sync"

	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
)

// Resolver manages per-chat metadata backed by a platform.SessionIndex.
// All methods are safe to call on a nil receiver or when Index is nil.
type Resolver struct {
	Index        platform.SessionIndex // may be nil (no persistence)
	AgentID      string
	PlatformName string
	Logger       func() *log.ComponentLogger // lazy logger (borrows the bot's logger)

	registered sync.Map // chatID → struct{}: chats already registered this process
}

// SessionKeyForChat returns the stable session key for a chat ID and records
// the (agent, platform, chat) association on first contact so outbound routing
// can find the owning platform (SessionIndex.PlatformForChat).
func (r *Resolver) SessionKeyForChat(chatID int64) string {
	if r == nil {
		return session.NewChatSessionKey("", chatID)
	}
	r.RegisterChat(chatID)
	return session.NewChatSessionKey(r.AgentID, chatID)
}

// RegisterChat persists the platform-ownership row for a chat. Idempotent and
// cached: only the first call per chat per process hits the database.
func (r *Resolver) RegisterChat(chatID int64) {
	if r == nil || r.Index == nil || r.AgentID == "" || r.PlatformName == "" {
		return
	}
	if _, seen := r.registered.LoadOrStore(chatID, struct{}{}); seen {
		return
	}
	if err := r.Index.SetChatMetadata(r.AgentID, r.PlatformName, chatID, "registered", "true"); err != nil {
		r.Logger().Errorf("register chat %d: %v", chatID, err)
		r.registered.Delete(chatID) // retry on next contact
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
