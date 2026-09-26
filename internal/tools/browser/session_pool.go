package browser

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/tools"
)

// SessionIdleTTL is how long a session's browser may sit unused before the
// pool stops it. Every session gets its own browser (#1600), and most sessions
// — forks, branches, delegated subagents — end without saying so, so an idle
// stop is the only thing that bounds how many chromium processes accumulate.
// The next call from the same session simply starts a fresh browser.
const SessionIdleTTL = 30 * time.Minute

// SessionPool hands each session its own BrowserManager, keyed by the session
// key the agent loop / exec bridge attaches to the tool's context. The main
// chat, its forks and branches, and other chats never share a browser.
type SessionPool struct {
	mu       sync.Mutex
	cfg      *config.ResolvedBrowser
	fileMode os.FileMode
	idleTTL  time.Duration
	profile  *profileLock
	entries  map[string]*poolEntry
}

type poolEntry struct {
	mgr      *BrowserManager
	inflight int
	timer    *time.Timer
}

// NewSessionPool creates a pool whose managers all share cfg and fileMode.
func NewSessionPool(cfg *config.ResolvedBrowser, fileMode os.FileMode) *SessionPool {
	return &SessionPool{
		cfg:      cfg,
		fileMode: fileMode,
		idleTTL:  SessionIdleTTL,
		profile:  &profileLock{},
		entries:  make(map[string]*poolEntry),
	}
}

// acquire returns the calling session's manager, creating it on first use, and
// marks a call in flight so the idle stop cannot fire underneath it. Every
// acquire must be paired with a release.
func (p *SessionPool) acquire(key string) *BrowserManager {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[key]
	if e == nil {
		mgr := NewBrowserManager(p.cfg, p.fileMode)
		// All of an agent's sessions share one configured persistent profile
		// directory; chromium cannot open it twice, so they share its lock.
		mgr.profile = p.profile
		e = &poolEntry{mgr: mgr}
		p.entries[key] = e
	}
	e.inflight++
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	return e.mgr
}

// release ends a call and arms the idle stop for the session.
func (p *SessionPool) release(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[key]
	if e == nil {
		return
	}
	e.inflight--
	if e.inflight > 0 {
		return
	}
	e.timer = time.AfterFunc(p.idleTTL, func() { p.reapIdle(key, e) })
}

// reapIdle stops and forgets a session's browser if it is still idle. The
// entry identity check drops a stale timer whose entry was already replaced.
func (p *SessionPool) reapIdle(key string, e *poolEntry) {
	p.mu.Lock()
	if p.entries[key] != e || e.inflight > 0 {
		p.mu.Unlock()
		return
	}
	delete(p.entries, key)
	p.mu.Unlock()
	if e.mgr.IsConnected() {
		e.mgr.logger.Infof("Stopping browser for session %q after %s idle", key, p.idleTTL)
	}
	_ = e.mgr.Stop()
}

// NewSessionBrowserTool creates the browser tool backed by a per-session pool.
// It is exec-exported, so delegated backends (Claude Code, opencode, codex)
// reach it as the foci_browser shell function; the session key on the exec
// bridge's context gives each of their sessions its own browser too.
func NewSessionBrowserTool(pool *SessionPool) *tools.Tool {
	return newBrowserTool(func(ctx context.Context, params browserParams) (tools.ToolResult, error) {
		key := tools.SessionKeyFromContext(ctx)
		mgr := pool.acquire(key)
		defer pool.release(key)
		return dispatchBrowserAction(mgr, params)
	})
}

// profileLock makes the configured persistent profile directory exclusive to
// one running browser. Chromium refuses a second instance on a user-data-dir
// that is already open, so without this a second session starting with
// incognito=false would fail obscurely instead of being told why.
type profileLock struct {
	mu    sync.Mutex
	owner *BrowserManager
}

func (l *profileLock) take(m *BrowserManager) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.owner != nil && l.owner != m {
		return fmt.Errorf("the persistent browser profile is already open in another session's browser; start with incognito=true, or close that browser first")
	}
	l.owner = m
	return nil
}

func (l *profileLock) drop(m *BrowserManager) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.owner == m {
		l.owner = nil
	}
}
