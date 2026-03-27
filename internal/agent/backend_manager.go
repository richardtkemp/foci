package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"foci/internal/backend"
	"foci/internal/log"
	"foci/internal/session"
)

// DefaultIdleTimeout is the default duration after which idle backends are closed.
const DefaultIdleTimeout = 24 * time.Hour

// BackendManager creates and manages per-session Backend instances lazily.
// Each session key gets its own Backend (own tmux pane, own CC session).
// Idle backends are closed after IdleTimeout and resumed on next message.
type BackendManager struct {
	mu       sync.Mutex
	backends map[string]*managedBackend // sessionKeyBase → managed backend

	// NewBackend creates a fresh Backend instance (does not start it).
	NewBackend func() (backend.Backend, error)

	// StartOpts returns the StartOptions for a new Backend.
	// Label and ResumeSessionID are set by the manager.
	StartOpts backend.StartOptions

	// SendFunc routes text to the correct platform chat for a session key.
	SendFunc func(sessionKey, text string)

	// PermissionPromptFunc sends a permission prompt with keyboard choices.
	// If nil, backends fall back to plain text via SendFunc.
	PermissionPromptFunc func(sessionKey, text, summary string, choices []backend.PromptChoice)

	// IdleTimeout is how long a backend can be idle before being closed.
	// Zero uses DefaultIdleTimeout.
	IdleTimeout time.Duration

	// SessionIndex persists CC session UUIDs for resume-after-restart.
	// Nil disables persistence (resume IDs lost on restart).
	SessionIndex *session.SessionIndex

	// AgentID is used as the key prefix for state.db persistence.
	AgentID string

	// reaperStop cancels the idle reaper goroutine.
	reaperStop context.CancelFunc
}

// managedBackend wraps a Backend with idle tracking and resume state.
type managedBackend struct {
	be         backend.Backend
	lastActive time.Time
	sessionKey string // full session key from last message (for reply routing)
}

// Get returns the Backend for the given session key, creating and starting
// one if it doesn't exist yet. The session key is collapsed to its base
// (agentID/c12345) so that compaction-rotated keys share the same Backend.
func (m *BackendManager) Get(ctx context.Context, sessionKey string) (backend.Backend, error) {
	base := session.SessionKeyBase(sessionKey)

	m.mu.Lock()
	if mb, ok := m.backends[base]; ok {
		mb.lastActive = time.Now()
		mb.sessionKey = sessionKey
		m.mu.Unlock()
		return mb.be, nil
	}

	// Check for a saved session UUID to resume.
	resumeID := m.loadResumeID(base)
	m.mu.Unlock()

	// Create and start a new Backend for this session.
	be, err := m.NewBackend()
	if err != nil {
		return nil, fmt.Errorf("create backend for %s: %w", base, err)
	}

	opts := m.StartOpts
	opts.Label = strings.ReplaceAll(base, "/", "-")
	opts.ResumeSessionID = resumeID
	opts.SessionKey = sessionKey

	// Set the reply and permission prompt functions before starting.
	sk := sessionKey
	if m.SendFunc != nil {
		be.SetReplyFunc(func(text string) {
			m.SendFunc(sk, text)
		})
	}
	if m.PermissionPromptFunc != nil {
		be.SetPermissionPromptFunc(func(text, summary string, choices []backend.PromptChoice) {
			m.PermissionPromptFunc(sk, text, summary, choices)
		})
	}
	be.SetOnSessionReady(func(sessionID string) {
		m.saveResumeID(base, sessionID)
	})

	if err := be.Start(ctx, opts); err != nil {
		// If resume failed (e.g. stale UUID), retry without resume.
		if resumeID != "" {
			log.Warnf("backend", "start with --resume %s failed for %s: %v — retrying without resume", resumeID, base, err)
			_ = be.Close()
			be, err = m.NewBackend()
			if err != nil {
				return nil, fmt.Errorf("create backend for %s (retry): %w", base, err)
			}
			// Re-set reply functions on the new backend.
			if m.SendFunc != nil {
				be.SetReplyFunc(func(text string) { m.SendFunc(sk, text) })
			}
			if m.PermissionPromptFunc != nil {
				be.SetPermissionPromptFunc(func(text, summary string, choices []backend.PromptChoice) {
					m.PermissionPromptFunc(sk, text, summary, choices)
				})
			}
			opts.ResumeSessionID = ""
			if err := be.Start(ctx, opts); err != nil {
				return nil, fmt.Errorf("start backend for %s (no resume): %w", base, err)
			}
		} else {
			return nil, fmt.Errorf("start backend for %s: %w", base, err)
		}
	}

	m.mu.Lock()
	if m.backends == nil {
		m.backends = make(map[string]*managedBackend)
	}
	m.backends[base] = &managedBackend{
		be:         be,
		lastActive: time.Now(),
		sessionKey: sessionKey,
	}

	// Start the idle reaper on first backend creation.
	if m.reaperStop == nil {
		ctx, cancel := context.WithCancel(context.Background())
		m.reaperStop = cancel
		go m.idleReaper(ctx)
	}
	m.mu.Unlock()

	if resumeID != "" {
		log.Infof("backend", "resumed session %s for %s", resumeID, base)
	}

	return be, nil
}

// Close shuts down all managed backends and the idle reaper.
func (m *BackendManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reaperStop != nil {
		m.reaperStop()
		m.reaperStop = nil
	}
	for key, mb := range m.backends {
		m.saveResumeID(key, mb.be.SessionID())
		_ = mb.be.Close()
		delete(m.backends, key)
	}
}

// Count returns the number of active backends.
func (m *BackendManager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.backends)
}

// stateKey returns the agent_metadata key for a CC session UUID.
func (m *BackendManager) stateKey(base string) string {
	return "cc_session:" + base
}

// saveResumeID persists the CC session UUID to state.db.
func (m *BackendManager) saveResumeID(base, sessionID string) {
	if sessionID == "" || m.SessionIndex == nil {
		return
	}
	if err := m.SessionIndex.SetAgentMetadata(m.AgentID, m.stateKey(base), sessionID); err != nil {
		log.Warnf("backend", "save resume ID for %s: %v", base, err)
	}
}

// loadResumeID reads a saved CC session UUID from state.db.
func (m *BackendManager) loadResumeID(base string) string {
	if m.SessionIndex == nil {
		return ""
	}
	id, err := m.SessionIndex.GetAgentMetadata(m.AgentID, m.stateKey(base))
	if err != nil {
		return ""
	}
	return id
}

// idleReaper periodically checks for idle backends and closes them.
func (m *BackendManager) idleReaper(ctx context.Context) {
	timeout := m.IdleTimeout
	if timeout == 0 {
		timeout = DefaultIdleTimeout
	}
	// Check every 10 minutes.
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.closeIdle(timeout)
		}
	}
}

// closeIdle closes backends that have been idle longer than timeout.
func (m *BackendManager) closeIdle(timeout time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for base, mb := range m.backends {
		if now.Sub(mb.lastActive) < timeout {
			continue
		}
		m.saveResumeID(base, mb.be.SessionID())
		log.Infof("backend", "closing idle backend %s (idle %s, session %s)",
			base, now.Sub(mb.lastActive).Round(time.Minute), mb.be.SessionID())
		_ = mb.be.Close()
		delete(m.backends, base)
	}
}
