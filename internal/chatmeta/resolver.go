// Package chatmeta provides shared per-chat metadata logic used by
// platform-specific bot implementations (telegram, discord, app, slack).
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
	"time"

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

	// NeverDefault marks the chats this resolver registers as
	// never-default (session.ChatMetaNeverDefault): conversations that must
	// never be resolved as the agent's default session or receive agent-wide
	// traffic. Set by transports whose chats belong to other people (Slack,
	// #2459); ordinary transports leave it unset.
	NeverDefault bool

	// registered maps each chat registered this process to when it was last
	// seen, so a repeat contact skips the database write. Entries not seen for
	// registeredIdleAfter are dropped by a sweep run from RegisterChat itself
	// (at most every registeredSweepEvery), so the map does not grow with
	// every chat ever seen (#2283). Guarded by regMu.
	regMu      sync.Mutex
	registered map[int64]time.Time
	lastSweep  time.Time

	// idleAfter and sweepEvery override registeredIdleAfter and
	// registeredSweepEvery in tests; zero = the defaults.
	idleAfter  time.Duration
	sweepEvery time.Duration
	now        func() time.Time // test clock; nil = time.Now
}

// A registered chat not seen for registeredIdleAfter is dropped from the
// in-process cache; the sweep runs at most every registeredSweepEvery. Not
// configurable: they match the 3h / 10m inbox idle eviction (#2283).
const (
	registeredIdleAfter  = 3 * time.Hour
	registeredSweepEvery = 10 * time.Minute
)

func (r *Resolver) registeredIdle() time.Duration {
	if r.idleAfter > 0 {
		return r.idleAfter
	}
	return registeredIdleAfter
}

func (r *Resolver) registeredSweep() time.Duration {
	if r.sweepEvery > 0 {
		return r.sweepEvery
	}
	return registeredSweepEvery
}

// markSeen records chatID as seen now and reports whether it was already
// registered and seen within the idle threshold (no write needed). It first
// sweeps out chats idle past the threshold, when a sweep is due.
func (r *Resolver) markSeen(chatID int64, now time.Time) (fresh bool) {
	r.regMu.Lock()
	defer r.regMu.Unlock()
	if r.registered == nil {
		r.registered = make(map[int64]time.Time)
	}
	idle := r.registeredIdle()
	if now.Sub(r.lastSweep) >= r.registeredSweep() {
		for id, seen := range r.registered {
			if now.Sub(seen) >= idle {
				delete(r.registered, id)
			}
		}
		r.lastSweep = now
	}
	last, ok := r.registered[chatID]
	r.registered[chatID] = now
	return ok && now.Sub(last) < idle
}

// forget drops chatID so its next contact writes again.
func (r *Resolver) forget(chatID int64) {
	r.regMu.Lock()
	delete(r.registered, chatID)
	r.regMu.Unlock()
}

// SessionKeyForChat returns the stable session key for a chat ID and records
// the (agent, platform, chat) association on first contact so outbound routing
// can find the owning platform (SessionIndex.PlatformForChat).
//
// For the ordinary transports (NeverDefault unset) the registration is
// best-effort: a failed row is logged by RegisterChat and retried on the next
// contact, and the key is returned regardless. A NeverDefault resolver fails
// closed (#2459 req 6): when either write fails it returns "" — no session
// key for a chat that could not be marked — so a caller can never address,
// and so never make default-eligible, a conversation that belongs to other
// people. (Slack's targetStore calls RegisterChat directly and refuses the
// triggering message on the error.)
func (r *Resolver) SessionKeyForChat(chatID int64) string {
	if r == nil {
		return session.NewChatSessionKey("", chatID)
	}
	if err := r.RegisterChat(chatID); err != nil && r.NeverDefault {
		return ""
	}
	return session.NewChatSessionKey(r.AgentID, chatID)
}

// RegisterChat persists the platform-ownership row for a chat and, for a
// NeverDefault resolver, the never-default row FIRST. Idempotent and cached:
// a chat seen within the last 3h makes no database write; one idle longer is
// dropped from the cache (#2283) and writes its (idempotent) rows once more. Every write
// failure returns its error with the cache entry dropped (the next contact
// retries) and writes nothing further:
//
//   - a failed never-default mark leaves NO row: without the flag the chat
//     would be default-eligible, so the caller (slack's targetStore) refuses
//     the triggering message rather than let a colleague's conversation into
//     default-session resolution (#2459 req 6);
//   - a failed registered row keeps the never-default mark (it only ever
//     excludes) and still returns the error, so the caller treats the chat as
//     unregistered until the write lands.
func (r *Resolver) RegisterChat(chatID int64) error {
	if r == nil || r.Index == nil || r.AgentID == "" || r.PlatformName == "" {
		return nil
	}
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	if r.markSeen(chatID, now()) {
		return nil
	}
	if r.NeverDefault {
		if err := r.Index.SetChatMetadata(r.AgentID, r.PlatformName, chatID, session.ChatMetaNeverDefault, "true"); err != nil {
			r.Logger().Errorf("mark chat %d never-default: %v", chatID, err)
			r.forget(chatID) // retry on next contact
			return err
		}
	}
	// registered='true' is the universal "this is a real user-facing chat"
	// signal: written here on first contact by every messaging platform
	// (telegram/discord here, app in Hub.sessionKeyForChat, slack in its targetStore). Default-session
	// routing (SessionIndex.DefaultSessionKeyForAgentOn) filters candidate
	// chats on it, so a chat carrying only some other metadata row
	// (alias/username/features) can't be mistaken for a routable chat.
	if err := r.Index.SetChatMetadata(r.AgentID, r.PlatformName, chatID, "registered", "true"); err != nil {
		r.Logger().Errorf("register chat %d: %v", chatID, err)
		r.forget(chatID) // retry on next contact
		return err
	}
	return nil
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
