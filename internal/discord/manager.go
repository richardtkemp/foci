package discord

import (
	"context"
	"sync"

	"foci/internal/log"
	"foci/internal/session"
)

// BotManager manages Discord bot instances for all agents.
type BotManager struct {
	primary map[string]*Bot  // agentID -> primary bot
	pools   map[string]*Pool // agentID -> per-agent facet pool
	shared  *Pool            // shared facet pool (fallback for any agent)
	all     []*Bot           // all bots for iteration
	mu      sync.RWMutex
	wg      sync.WaitGroup // tracks running bot goroutines for graceful shutdown
}

// NewBotManager creates an empty BotManager.
func NewBotManager() *BotManager {
	return &BotManager{
		primary: make(map[string]*Bot),
		pools:   make(map[string]*Pool),
	}
}

// AddPrimary registers the primary bot for an agent.
func (m *BotManager) AddPrimary(agentID string, bot *Bot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.primary[agentID] = bot
	m.all = append(m.all, bot)
}

// AddFacet registers a facet bot for an agent.
// Creates the pool for the agent if it doesn't exist.
func (m *BotManager) AddFacet(agentID string, bot *Bot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pool, ok := m.pools[agentID]
	if !ok {
		pool = NewPool()
		m.pools[agentID] = pool
	}
	bot.SetSecondary(pool)
	pool.Add(bot)
	m.all = append(m.all, bot)
}

// PrimaryBot returns the primary bot for an agent, or nil if not found.
func (m *BotManager) PrimaryBot(agentID string) *Bot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.primary[agentID]
}

// Pool returns the per-agent facet pool for an agent, or nil if not configured.
func (m *BotManager) Pool(agentID string) *Pool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.pools[agentID]
}

// AddSharedFacet registers a bot in the shared facet pool.
// Creates the shared pool if it doesn't exist.
func (m *BotManager) AddSharedFacet(bot *Bot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shared == nil {
		m.shared = NewPool()
	}
	bot.SetSecondary(m.shared)
	m.shared.Add(bot)
	m.all = append(m.all, bot)
}

// SharedPool returns the shared facet pool, or nil if not configured.
func (m *BotManager) SharedPool() *Pool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.shared
}

// BotForSession returns the bot that owns the given session key.
// Checks facet pools first (exact SessionKey match), then primary bots
// (chat ID ownership via cached chat-to-session mappings). Returns nil if
// no bot on this platform owns the session.
func (m *BotManager) BotForSession(sessionKey string) *Bot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, pool := range m.pools {
		if b := findInPool(pool, sessionKey); b != nil {
			return b
		}
	}

	if m.shared != nil {
		if b := findInPool(m.shared, sessionKey); b != nil {
			return b
		}
	}

	// Check primary bots: return the first one whose in-memory cache
	// contains the session key's chat ID. Cross-platform routing is now
	// handled by the aggregatingConnMgr via PlatformForChat, so this only
	// checks the in-memory cache (no DB fallback needed here).
	chatID := session.ChatIDFromKey(sessionKey)
	if chatID != 0 {
		for _, b := range m.primary {
			b.chatKeysMu.RLock()
			_, ok := b.chatSessionKeys[chatID]
			b.chatKeysMu.RUnlock()
			if ok {
				return b
			}
		}
	}

	return nil
}

// findInPool searches a pool for a bot whose SessionKey matches the given key.
func findInPool(pool *Pool, sessionKey string) *Bot {
	var found *Bot
	pool.ForEach(func(b *Bot) {
		if b.SessionKey() == sessionKey {
			found = b
		}
	})
	return found
}

// BotForSessionOrPrimary returns the facet bot owning sessionKey if any
// secondary bot holds it, otherwise the agent's primary bot. Returns nil if
// neither is available.
func (m *BotManager) BotForSessionOrPrimary(sessionKey, agentID string) *Bot {
	if fb := m.BotForSession(sessionKey); fb != nil {
		return fb
	}
	return m.PrimaryBot(agentID)
}

// AcquireFacet tries to acquire a facet bot for the given agent.
// Priority: per-agent pool first, then shared pool as fallback.
// Returns the bot and true on success, or nil and false if no bots available.
func (m *BotManager) AcquireFacet(agentID string) (*Bot, bool) {
	m.mu.RLock()
	pool := m.pools[agentID]
	shared := m.shared
	m.mu.RUnlock()

	// Try per-agent pool first
	if pool != nil {
		if bot, ok := pool.Acquire(); ok {
			return bot, true
		}
	}

	// Fall back to shared pool
	if shared != nil {
		if bot, ok := shared.Acquire(); ok {
			return bot, true
		}
	}

	return nil, false
}

// HasFacet returns true if the agent has any facet bots available
// (either per-agent or shared).
func (m *BotManager) HasFacet(agentID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if pool, ok := m.pools[agentID]; ok && pool.Size() > 0 {
		return true
	}
	return m.shared != nil && m.shared.Size() > 0
}

// StartAll starts all bots as goroutines. Non-blocking.
// Use Wait() after cancelling ctx to block until all bots have finished cleanup.
func (m *BotManager) StartAll(ctx context.Context) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, bot := range m.all {
		m.wg.Add(1)
		go func(b *Bot) {
			defer m.wg.Done()
			b.Run(ctx)
		}(bot)
	}
	log.Infof("discord", "started %d bot(s)", len(m.all))
}

// Wait blocks until all bot goroutines have returned.
func (m *BotManager) Wait() {
	m.wg.Wait()
}

// AgentIDs returns the IDs of all agents with primary bots.
func (m *BotManager) AgentIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.primary))
	for id := range m.primary {
		ids = append(ids, id)
	}
	return ids
}
