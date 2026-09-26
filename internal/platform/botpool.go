package platform

import (
	"context"
	"sync"
	"time"

	"foci/internal/log"
)

var (
	interactiveLog = log.NewComponentLogger("interactive")
	platformLog    = log.NewComponentLogger(

		// PoolBot is what the generic Pool needs from a platform's bot type.
		"platform")
)

type PoolBot interface {
	SessionKey() string
	SetSessionKey(string)
}

// PooledBot is what the generic BotManager additionally needs. B is the
// bot's own pointer type (self-referential constraint), so SetSecondary
// receives the correctly-typed pool.
type PooledBot[B PoolBot] interface {
	comparable
	PoolBot
	SetSecondary(*Pool[B])
	Run(context.Context)
}

// SessionActivityChecker returns the last activity time for a session key.
// Returns "n/a" if the session doesn't exist.
type SessionActivityChecker interface {
	LastActivity(key string) string // RFC3339 or "n/a"
}

// botEntry tracks a secondary bot and its last usage time.
type botEntry[B any] struct {
	bot      B
	lastUsed time.Time
}

// Pool manages a set of secondary (facet) bots for a platform. It is shared
// by Telegram and Discord — one implementation of acquire/release, LRU
// selection, and TTL-based stale-session reclaim.
type Pool[B PoolBot] struct {
	name        string // platform name, for log tags
	bots        []*botEntry[B]
	isLive      func(B) bool // nil = every bot is live; set by the owning BotManager
	mu          sync.Mutex
	sessionTTL  time.Duration           // 0 = no auto-reclaim
	sessions    SessionActivityChecker  // nil = no activity checking
	ReclaimHook func(sessionKey string) // called before releasing a stale session; nil = no hook
}

// NewPool creates an empty bot pool. name is the platform tag used in logs.
func NewPool[B PoolBot](name string) *Pool[B] {
	return &Pool[B]{name: name}
}

// SetSessionTTL configures TTL-based auto-reclaim of stale facet sessions.
// If a bot's session has had no activity for longer than ttl, it is auto-released
// during the next Acquire call. Zero disables auto-reclaim.
func (p *Pool[B]) SetSessionTTL(ttl time.Duration, sessions SessionActivityChecker) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessionTTL = ttl
	p.sessions = sessions
}

// Add registers a secondary bot in the pool.
func (p *Pool[B]) Add(bot B) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bots = append(p.bots, &botEntry[B]{bot: bot})
}

// reclaimTarget pairs a bot with the session key to reclaim.
type reclaimTarget[B any] struct {
	bot B
	key string
}

// Acquire returns the least recently used idle bot, or false if all are busy.
// If session TTL is configured, busy bots with stale sessions (no activity within
// the TTL window) are auto-released first. If a ReclaimHook is set, it fires
// (outside the lock) before releasing each stale session.
func (p *Pool[B]) Acquire() (B, bool) {
	p.mu.Lock()

	// Collect stale sessions while locked
	var stale []reclaimTarget[B]
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
	var oldest *botEntry[B]
	for _, e := range p.bots {
		if e.bot.SessionKey() == "" && p.live(e.bot) {
			if oldest == nil || e.lastUsed.Before(oldest.lastUsed) {
				oldest = e
			}
		}
	}

	if oldest == nil {
		p.mu.Unlock()
		var zero B
		return zero, false
	}
	oldest.lastUsed = time.Now()
	p.mu.Unlock()
	return oldest.bot, true
}

// collectStaleLocked identifies bots with stale sessions. Must be called with p.mu held.
func (p *Pool[B]) collectStaleLocked() []reclaimTarget[B] {
	now := time.Now()
	var targets []reclaimTarget[B]
	for _, e := range p.bots {
		sk := e.bot.SessionKey()
		if sk == "" {
			continue // already idle
		}

		lastStr := p.sessions.LastActivity(sk)
		if lastStr == "n/a" {
			log.NewComponentLogger(p.name).Infof("reclaiming facet bot (session %s not found)", sk)
			targets = append(targets, reclaimTarget[B]{bot: e.bot, key: sk})
			continue
		}

		lastTime, err := time.Parse("2006-01-02T15:04:05Z", lastStr)
		if err != nil {
			continue // can't parse, leave it alone
		}

		if now.Sub(lastTime) > p.sessionTTL {
			log.NewComponentLogger(p.name).Infof("reclaiming facet bot (session %s idle for %v, TTL %v)",
				sk, now.Sub(lastTime).Round(time.Second), p.sessionTTL)
			targets = append(targets, reclaimTarget[B]{bot: e.bot, key: sk})
		}
	}
	return targets
}

// Release returns a bot to the pool by clearing its session key.
func (p *Pool[B]) Release(bot B) {
	bot.SetSessionKey("")
}

// Size returns the total number of bots in the pool.
func (p *Pool[B]) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.bots)
}

// ForEach calls fn for each bot in the pool.
func (p *Pool[B]) ForEach(fn func(B)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.bots {
		fn(e.bot)
	}
}

// live reports whether bot is a live connection (see BotManager.ConnectBeforeRun).
func (p *Pool[B]) live(bot B) bool {
	return p.isLive == nil || p.isLive(bot)
}

// liveSize returns the number of live bots in the pool.
func (p *Pool[B]) liveSize() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.bots {
		if p.live(e.bot) {
			n++
		}
	}
	return n
}

// Available returns the number of idle live bots.
func (p *Pool[B]) Available() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.bots {
		if e.bot.SessionKey() == "" && p.live(e.bot) {
			n++
		}
	}
	return n
}

// BotManager manages a platform's bot instances for all agents: one primary
// bot per agent, optional per-agent facet pools, and an optional shared facet
// pool. One implementation shared by Telegram and Discord; it satisfies the
// per-platform half of ConnectionSource.
//
// A bot registered with ConnectBeforeRun is PENDING until its network connect
// succeeds: it is wired (handler, commands, callbacks) but invisible to every
// lookup that hands out a connection — PrimaryBot, BotForSession, facet
// acquisition — so nothing routes to a bot that cannot send (#2043). StartAll
// runs the connect in the bot's own goroutine, so a platform outage at boot
// delays only that bot, never foci startup or another platform.
type BotManager[B PooledBot[B]] struct {
	name    string              // platform tag for logs
	primary map[string]B        // agentID → primary bot
	pools   map[string]*Pool[B] // agentID → per-agent facet pool
	shared  *Pool[B]            // shared facet pool (fallback for any agent)
	all     []B                 // all bots for iteration
	mu      sync.RWMutex
	wg      sync.WaitGroup // tracks running bot goroutines for graceful shutdown

	// pendMu guards pending and onLive. It is a leaf lock: taken after mu or a
	// pool's mu, never held while taking another.
	pendMu  sync.Mutex
	pending map[B]pendingConnect // registered, not yet connected
	onLive  map[B][]func(B)      // WhenPrimaryLive callbacks waiting on a pending bot
}

// pendingConnect is the deferred connect for a pending bot.
type pendingConnect struct {
	label   string // names the bot in the give-up log line, e.g. `agent "clutch": create bot`
	connect func(context.Context) error
}

// NewBotManager creates an empty BotManager. name is the platform tag used in logs.
func NewBotManager[B PooledBot[B]](name string) *BotManager[B] {
	return &BotManager[B]{
		name:    name,
		primary: make(map[string]B),
		pools:   make(map[string]*Pool[B]),
		pending: make(map[B]pendingConnect),
		onLive:  make(map[B][]func(B)),
	}
}

// AddPrimary registers the primary bot for an agent.
func (m *BotManager[B]) AddPrimary(agentID string, bot B) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.primary[agentID] = bot
	m.all = append(m.all, bot)
}

// AddFacet registers a facet bot for an agent.
// Creates the pool for the agent if it doesn't exist.
func (m *BotManager[B]) AddFacet(agentID string, bot B) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pool, ok := m.pools[agentID]
	if !ok {
		pool = m.newPool()
		m.pools[agentID] = pool
	}
	bot.SetSecondary(pool)
	pool.Add(bot)
	m.all = append(m.all, bot)
}

// newPool creates a pool that hides this manager's pending bots.
func (m *BotManager[B]) newPool() *Pool[B] {
	p := NewPool[B](m.name)
	p.isLive = m.isLive
	return p
}

// ConnectBeforeRun marks a registered bot pending: StartAll will call connect
// (in the bot's goroutine) and run the bot only once it returns nil. connect
// owns its retry policy — the platforms use netretry.Do with ctx, so shutdown
// ends a pending retry — and should finish any identity setup the bot needs
// (username, restored facet session) before returning, because the bot is
// visible to lookups from the moment connect succeeds. A connect error means
// the bot never runs; it is logged here under label.
//
// Call after AddPrimary/AddFacet/AddSharedFacet and before StartAll.
func (m *BotManager[B]) ConnectBeforeRun(bot B, label string, connect func(context.Context) error) {
	m.pendMu.Lock()
	defer m.pendMu.Unlock()
	m.pending[bot] = pendingConnect{label: label, connect: connect}
}

// isLive reports whether bot may be handed out as a connection.
func (m *BotManager[B]) isLive(bot B) bool {
	m.pendMu.Lock()
	defer m.pendMu.Unlock()
	_, pending := m.pending[bot]
	return !pending
}

// liveOrZero returns bot if it is live, else the zero value.
func (m *BotManager[B]) liveOrZero(bot B) B {
	if isZero(bot) || m.isLive(bot) {
		return bot
	}
	var zero B
	return zero
}

// markLive ends bot's pending state and runs the callbacks waiting on it.
func (m *BotManager[B]) markLive(bot B) {
	m.pendMu.Lock()
	delete(m.pending, bot)
	waiting := m.onLive[bot]
	delete(m.onLive, bot)
	m.pendMu.Unlock()
	for _, fn := range waiting {
		fn(bot)
	}
}

// WhenPrimaryLive calls fn exactly once with the agent's primary bot when it is
// live: now if it already is, or from the bot's goroutine the moment its
// background connect succeeds. It never fires for an agent without a primary
// or for one whose connect fails for good.
func (m *BotManager[B]) WhenPrimaryLive(agentID string, fn func(B)) {
	bot := m.RegisteredPrimary(agentID)
	if isZero(bot) {
		return
	}
	m.pendMu.Lock()
	if _, pending := m.pending[bot]; pending {
		m.onLive[bot] = append(m.onLive[bot], fn)
		m.pendMu.Unlock()
		return
	}
	m.pendMu.Unlock()
	fn(bot)
}

// PrimaryBot returns the agent's primary bot if it is live, or the zero value.
func (m *BotManager[B]) PrimaryBot(agentID string) B {
	return m.liveOrZero(m.RegisteredPrimary(agentID))
}

// RegisteredPrimary returns the agent's primary bot whether or not it has
// connected yet — for setup-time wiring (lifecycle callbacks, display
// defaults) that must reach a bot still connecting in the background. Never
// use it to pick a connection to send through: that is PrimaryBot.
func (m *BotManager[B]) RegisteredPrimary(agentID string) B {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.primary[agentID]
}

// Pool returns the per-agent facet pool for an agent, or nil if not configured.
func (m *BotManager[B]) Pool(agentID string) *Pool[B] {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.pools[agentID]
}

// AddSharedFacet registers a bot in the shared facet pool.
// Creates the shared pool if it doesn't exist.
func (m *BotManager[B]) AddSharedFacet(bot B) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shared == nil {
		m.shared = m.newPool()
	}
	bot.SetSecondary(m.shared)
	m.shared.Add(bot)
	m.all = append(m.all, bot)
}

// SharedPool returns the shared facet pool, or nil if not configured.
func (m *BotManager[B]) SharedPool() *Pool[B] {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.shared
}

// BotForSession returns the live bot whose SessionKey matches, or the zero value.
//
// An empty sessionKey matches no specific session: idle facet bots also carry
// an empty SessionKey, so a "" lookup would return whichever idle facet happens
// to come first in m.all — an agent-less bot (agentID="") with no default chat.
// Returning the zero value here lets BotForSessionOrPrimary fall through to the
// agent's primary bot, which owns the persisted default chat (#843).
func (m *BotManager[B]) BotForSession(sessionKey string) B {
	var zero B
	if sessionKey == "" {
		return zero
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, b := range m.all {
		if b.SessionKey() == sessionKey && m.isLive(b) {
			return b
		}
	}
	return zero
}

// BotForSessionOrPrimary returns the facet bot owning sessionKey if any
// secondary bot holds it, otherwise the agent's primary bot. Returns the zero
// value if neither is available.
func (m *BotManager[B]) BotForSessionOrPrimary(sessionKey, agentID string) B {
	if fb := m.BotForSession(sessionKey); !isZero(fb) {
		return fb
	}
	return m.PrimaryBot(agentID)
}

// isZero reports whether a bot value is its type's zero value (nil pointer).
func isZero[B PoolBot](b B) bool {
	// B is always a pointer type in practice; interface-box comparison
	// against the zero value works for both pointers and other comparables.
	var zero B
	return any(b) == any(zero)
}

// AcquireFacet tries to acquire a facet bot for the given agent.
// Priority: per-agent pool first, then shared pool as fallback.
// Returns the bot and true on success, or the zero value and false if no bots
// are available.
func (m *BotManager[B]) AcquireFacet(agentID string) (B, bool) {
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

	var zero B
	return zero, false
}

// HasFacet returns true if the agent has any live facet bots
// (either per-agent or shared).
func (m *BotManager[B]) HasFacet(agentID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if pool, ok := m.pools[agentID]; ok && pool.liveSize() > 0 {
		return true
	}
	return m.shared != nil && m.shared.liveSize() > 0
}

// StartAll starts all bots as goroutines. Non-blocking: a pending bot
// connects first, inside its own goroutine (see ConnectBeforeRun).
// Use Wait() after cancelling ctx to block until all bots have finished cleanup.
func (m *BotManager[B]) StartAll(ctx context.Context) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	lg := log.NewComponentLogger(m.name)
	connecting := 0
	for _, bot := range m.all {
		m.pendMu.Lock()
		pc, pending := m.pending[bot]
		m.pendMu.Unlock()
		if pending {
			connecting++
		}
		m.wg.Add(1)
		go func(b B) {
			defer m.wg.Done()
			if pending {
				// connect sees ctx between attempts, but an attempt already in
				// flight (a getMe or gateway dial) only ends on its own network
				// timeout — so shutdown does not wait for it: Wait must stay
				// prompt. The abandoned attempt finishes in the background and
				// its bot is never run.
				errc := make(chan error, 1)
				go func() { errc <- pc.connect(ctx) }()
				var err error
				select {
				case err = <-errc:
				case <-ctx.Done():
					lg.Infof("%s: connect abandoned at shutdown", pc.label)
					return
				}
				if err != nil {
					if ctx.Err() != nil {
						lg.Infof("%s: connect abandoned at shutdown: %v", pc.label, err)
					} else {
						lg.Errorf("%s: %v (running without it)", pc.label, err)
					}
					return
				}
				m.markLive(b)
			}
			b.Run(ctx)
		}(bot)
	}
	if connecting > 0 {
		lg.Infof("started %d bot(s), %d connecting in the background", len(m.all), connecting)
	} else {
		lg.Infof("started %d bot(s)", len(m.all))
	}
}

// Wait blocks until all bot goroutines have returned, including shutdown
// cleanup such as acknowledging processed platform updates. A background
// connect pending at cancellation does not hold it up: its retry loop stops
// at the next attempt boundary, and an attempt already in flight is abandoned
// (see StartAll).
func (m *BotManager[B]) Wait() {
	m.wg.Wait()
}

// AgentIDs returns the IDs of all agents with live primary bots.
func (m *BotManager[B]) AgentIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.primary))
	for id, b := range m.primary {
		if m.isLive(b) {
			ids = append(ids, id)
		}
	}
	return ids
}
