package telegram

import (
	"sync"
	"time"
)

// botEntry tracks a secondary bot and its last usage time.
type botEntry struct {
	bot      *Bot
	lastUsed time.Time
}

// Pool manages a set of secondary Telegram bots for multiball sessions.
type Pool struct {
	bots []*botEntry
	mu   sync.Mutex
}

// NewPool creates an empty bot pool.
func NewPool() *Pool {
	return &Pool{}
}

// Add registers a secondary bot in the pool.
func (p *Pool) Add(bot *Bot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bots = append(p.bots, &botEntry{bot: bot})
}

// Acquire returns the least recently used idle bot, or false if all are busy.
func (p *Pool) Acquire() (*Bot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

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
