package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"foci/internal/backend"
	"foci/internal/session"
)

// BackendManager creates and manages per-session Backend instances lazily.
// Each session key gets its own Backend (own tmux pane, own CC session).
type BackendManager struct {
	mu       sync.Mutex
	backends map[string]backend.Backend // sessionKeyBase → Backend

	// NewBackend creates a fresh Backend instance (does not start it).
	NewBackend func() (backend.Backend, error)

	// StartOpts returns the StartOptions for a new Backend.
	// Label is set by the manager based on the session key.
	StartOpts backend.StartOptions

	// SendFunc routes text to the correct platform chat for a session key.
	SendFunc func(sessionKey, text string)
}

// Get returns the Backend for the given session key, creating and starting
// one if it doesn't exist yet. The session key is collapsed to its base
// (agentID/c12345) so that compaction-rotated keys share the same Backend.
func (m *BackendManager) Get(ctx context.Context, sessionKey string) (backend.Backend, error) {
	base := session.SessionKeyBase(sessionKey)

	m.mu.Lock()
	if be, ok := m.backends[base]; ok {
		m.mu.Unlock()
		return be, nil
	}

	// Create and start a new Backend for this session.
	be, err := m.NewBackend()
	if err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("create backend for %s: %w", base, err)
	}

	opts := m.StartOpts
	// Label for tmux window: replace / with - for a clean window name.
	opts.Label = strings.ReplaceAll(base, "/", "-")

	if m.backends == nil {
		m.backends = make(map[string]backend.Backend)
	}
	m.backends[base] = be
	m.mu.Unlock()

	// Set the reply function before starting so the watcher can deliver output.
	sk := sessionKey
	if m.SendFunc != nil {
		be.SetReplyFunc(func(text string) {
			m.SendFunc(sk, text)
		})
	}

	if err := be.Start(ctx, opts); err != nil {
		m.mu.Lock()
		delete(m.backends, base)
		m.mu.Unlock()
		return nil, fmt.Errorf("start backend for %s: %w", base, err)
	}

	return be, nil
}

// Close shuts down all managed backends.
func (m *BackendManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, be := range m.backends {
		_ = be.Close()
		delete(m.backends, key)
	}
}

// Count returns the number of active backends.
func (m *BackendManager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.backends)
}
