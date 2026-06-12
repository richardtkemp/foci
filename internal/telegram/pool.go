package telegram

import (
	"sync"
	"time"

	"foci/internal/log"
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

// Pool manages a set of secondary Telegram bots for facet sessions.
type Pool struct {
	bots        []*botEntry
	mu          sync.Mutex
	sessionTTL  time.Duration           // 0 = no auto-reclaim
	sessions    SessionActivityChecker  // nil = no activity checking
	ReclaimHook func(sessionKey string) // called before releasing a stale session; nil = no hook
}

// NewPool creates an empty bot pool.
func NewPool() *Pool {
	return &Pool{}
}

// SetSessionTTL configures TTL-based auto-reclaim of stale facet sessions.
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

// reclaimTarget pairs a bot with the session key to reclaim.
type reclaimTarget struct {
	bot *Bot
	key string
}

// Acquire returns the least recently used idle bot, or false if all are busy.
// If session TTL is configured, busy bots with stale sessions (no activity within
// the TTL window) are auto-released first. If a ReclaimHook is set, it fires
// (outside the lock) before releasing each stale session.
func (p *Pool) Acquire() (*Bot, bool) {
	p.mu.Lock()

	// Collect stale sessions while locked
	var stale []reclaimTarget
	if p.sessionTTL > 0 && p.sessions != nil {
		stale = p.collectStaleLocked()
	}

	// Fire hooks outside the lock (hooks may call HandleMessage, which can take seconds)
	if len(stale) > 0 && p.ReclaimHook != nil {
		p.mu.Unlock()
		for _, t := range stale {
			p.ReclaimHook(t.key)
		}
		p.mu.Lock()
	}

	// Clear reclaimed bots' session keys
	for _, t := range stale {
		t.bot.SetSessionKey("")
	}

	// Find least-recently-used idle bot
	var oldest *botEntry
	for _, e := range p.bots {
		if e.bot.SessionKey() == "" {
			if oldest == nil || e.lastUsed.Before(oldest.lastUsed) {
				oldest = e
			}
		}
	}

	if oldest == nil {
		p.mu.Unlock()
		return nil, false
	}
	oldest.lastUsed = time.Now()
	p.mu.Unlock()
	return oldest.bot, true
}

// collectStaleLocked identifies bots with stale sessions. Must be called with p.mu held.
func (p *Pool) collectStaleLocked() []reclaimTarget {
	now := time.Now()
	var targets []reclaimTarget
	for _, e := range p.bots {
		sk := e.bot.SessionKey()
		if sk == "" {
			continue // already idle
		}

		lastStr := p.sessions.LastActivity(sk)
		if lastStr == "n/a" {
			log.Infof("telegram", "reclaiming facet bot (session %s not found)", sk)
			targets = append(targets, reclaimTarget{bot: e.bot, key: sk})
			continue
		}

		lastTime, err := time.Parse("2006-01-02T15:04:05Z", lastStr)
		if err != nil {
			continue // can't parse, leave it alone
		}

		if now.Sub(lastTime) > p.sessionTTL {
			log.Infof("telegram", "reclaiming facet bot (session %s idle for %v, TTL %v)",
				sk, now.Sub(lastTime).Round(time.Second), p.sessionTTL)
			targets = append(targets, reclaimTarget{bot: e.bot, key: sk})
		}
	}
	return targets
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

// ForEach calls fn for each bot in the pool.
func (p *Pool) ForEach(fn func(*Bot)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.bots {
		fn(e.bot)
	}
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
