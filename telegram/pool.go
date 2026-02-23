package telegram

import (
	"sync"
	"time"

	"clod/log"
)

// SessionActivityChecker returns the last activity time for a session key.
// Returns zero time if the session doesn't exist.
type SessionActivityChecker interface {
	LastActivity(key string) string // RFC3339 or "n/a"
}

// botEntry tracks a secondary bot and its last usage time.
type botEntry struct {
	bot      *Bot
	lastUsed time.Time
}

// Pool manages a set of secondary Telegram bots for multiball sessions.
type Pool struct {
	bots       []*botEntry
	mu         sync.Mutex
	sessionTTL time.Duration            // 0 = no auto-reclaim
	sessions   SessionActivityChecker   // nil = no activity checking
}

// NewPool creates an empty bot pool.
func NewPool() *Pool {
	return &Pool{}
}

// SetSessionTTL configures TTL-based auto-reclaim of stale multiball sessions.
// If a bot's session has had no activity for longer than ttl, it is auto-released
// during the next Acquire call. Zero disables auto-reclaim.
func (p *Pool) SetSessionTTL(ttl time.Duration, sessions SessionActivityChecker) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessionTTL = ttl
	p.sessions = sessions
}

// Add registers a secondary bot in the pool.
func (p *Pool) Add(bot *Bot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bots = append(p.bots, &botEntry{bot: bot})
}

// Acquire returns the least recently used idle bot, or false if all are busy.
// If session TTL is configured, busy bots with stale sessions (no activity within
// the TTL window) are auto-released first.
func (p *Pool) Acquire() (*Bot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Auto-reclaim stale sessions before looking for idle bots
	if p.sessionTTL > 0 && p.sessions != nil {
		p.reclaimStaleLocked()
	}

	var oldest *botEntry
	for _, e := range p.bots {
		if e.bot.SessionKey() == "" {
			if oldest == nil || e.lastUsed.Before(oldest.lastUsed) {
				oldest = e
			}
		}
	}

	if oldest == nil {
		return nil, false
	}
	oldest.lastUsed = time.Now()
	return oldest.bot, true
}

// reclaimStaleLocked releases bots whose sessions have been inactive longer than
// the configured TTL. Must be called with p.mu held.
func (p *Pool) reclaimStaleLocked() {
	now := time.Now()
	for _, e := range p.bots {
		sk := e.bot.SessionKey()
		if sk == "" {
			continue // already idle
		}

		lastStr := p.sessions.LastActivity(sk)
		if lastStr == "n/a" {
			// Session file doesn't exist — bot assigned to a phantom session, release it
			log.Infof("telegram", "reclaiming multiball bot (session %s not found)", sk)
			e.bot.SetSessionKey("")
			continue
		}

		lastTime, err := time.Parse("2006-01-02T15:04:05Z", lastStr)
		if err != nil {
			continue // can't parse, leave it alone
		}

		if now.Sub(lastTime) > p.sessionTTL {
			log.Infof("telegram", "reclaiming multiball bot (session %s idle for %v, TTL %v)",
				sk, now.Sub(lastTime).Round(time.Second), p.sessionTTL)
			e.bot.SetSessionKey("")
		}
	}
}

// Release returns a bot to the pool by clearing its session key.
func (p *Pool) Release(bot *Bot) {
	bot.SetSessionKey("")
}

// Size returns the total number of bots in the pool.
func (p *Pool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.bots)
}

// Available returns the number of idle bots.
func (p *Pool) Available() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.bots {
		if e.bot.SessionKey() == "" {
			n++
		}
	}
	return n
}
