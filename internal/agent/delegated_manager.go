package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/session"
	"foci/internal/tools"
)

// DefaultIdleTimeout is the default duration after which idle delegated backends are closed.
const DefaultIdleTimeout = 24 * time.Hour

// DelegatedManager creates and manages per-session Backend instances lazily
// for the delegated transport path. Each session key gets its own Backend
// (own CC session). Idle backends are closed after IdleTimeout and resumed
// on next message.
type DelegatedManager struct {
	mu       sync.Mutex
	backends map[string]*managedBackend // sessionKeyBase → managed backend

	// NewBackend creates a fresh Backend instance (does not start it).
	NewBackend func() (delegator.Delegator, error)

	// StartOpts returns the StartOptions for a new Backend.
	// Label and ResumeSessionID are set by the manager.
	StartOpts delegator.StartOptions

	// SendFunc routes text to the correct platform chat for a session key.
	SendFunc func(sessionKey, text string)

	// PermissionPromptFunc sends a permission prompt with keyboard choices.
	// requestID is the CC protocol request ID.
	// If nil, backends fall back to plain text via SendFunc.
	PermissionPromptFunc func(sessionKey, requestID, text, summary string, choices []delegator.PromptChoice)

	// TypingFunc controls the platform typing indicator for a session.
	// Called with true when CC starts working, false on turn complete.
	TypingFunc func(sessionKey string, typing bool)

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
	be         delegator.Delegator
	bridge     *tools.ExecBridge // exec bridge for shell functions; nil if not configured
	lastActive time.Time
	sessionKey string // full session key from last message (for reply routing)

	// Permission prompt gating. When a permission prompt is outstanding,
	// incoming messages and injections must wait — the backend cannot
	// process new input until the prompt is resolved.
	// See WaitForPermission / SetPermissionPending.
	permMu      sync.Mutex
	permPending bool
	permCond    *sync.Cond // lazy-init on first WaitForPermission
}

// getManaged looks up the managed backend for a session key under the lock.
func (m *DelegatedManager) getManaged(sessionKey string) (*managedBackend, bool) {
	base := session.SessionKeyBase(sessionKey)
	m.mu.Lock()
	mb, ok := m.backends[base]
	m.mu.Unlock()
	return mb, ok
}

// clearPermission unblocks any WaitForPermission waiters on this backend.
func (mb *managedBackend) clearPermission() {
	mb.permMu.Lock()
	mb.permPending = false
	if mb.permCond != nil {
		mb.permCond.Broadcast()
	}
	mb.permMu.Unlock()
}

// Get returns the Backend for the given session key, creating and starting
// one if it doesn't exist yet. The session key is collapsed to its base
// (agentID/c12345) so that compaction-rotated keys share the same Backend.
func (m *DelegatedManager) Get(ctx context.Context, sessionKey string) (delegator.Delegator, error) {
	base := session.SessionKeyBase(sessionKey)

	m.mu.Lock()
	if mb, ok := m.backends[base]; ok {
		if mb.be.IsRunning() {
			mb.lastActive = time.Now()
			mb.sessionKey = sessionKey
			m.mu.Unlock()
			return mb.be, nil
		}
		// Backend subprocess is dead — clean up and fall through to respawn.
		// Save the resume ID so the new subprocess can resume the CC session.
		log.Warnf("delegated", "backend for %s is dead, respawning", base)
		m.saveResumeID(base, mb.be.SessionID())
		_ = mb.be.Close()
		if mb.bridge != nil {
			mb.bridge.Close()
		}
		delete(m.backends, base)
	}

	// Check for a saved session UUID to resume.
	resumeID := m.loadResumeID(base)
	m.mu.Unlock()

	// Create and start a new Backend for this session.
	be, err := m.NewBackend()
	if err != nil {
		return nil, fmt.Errorf("create delegated backend for %s: %w", base, err)
	}

	opts := m.StartOpts
	opts.Label = strings.ReplaceAll(base, "/", "-")
	opts.ResumeSessionID = resumeID
	opts.SessionKey = sessionKey

	// Create the exec bridge so shell functions (foci_todo, foci_send_to_chat, etc.)
	// are available in the backend's shell environment. The bridge is created here
	// (not in individual backends) so all backend types get it automatically.
	var bridge *tools.ExecBridge
	if reg, ok := opts.ExecRegistry.(*tools.Registry); ok && reg != nil {
		bridgeCtx := context.Background()
		if sessionKey != "" {
			bridgeCtx = tools.WithSessionKey(bridgeCtx, sessionKey)
		}
		var bridgeErr error
		if sessionKey != "" {
			bridge, bridgeErr = tools.NewExecBridgeStable(reg, bridgeCtx, sessionKey)
		} else {
			bridge, bridgeErr = tools.NewExecBridge(reg, bridgeCtx)
		}
		if bridgeErr != nil {
			log.Warnf("delegated", "exec bridge creation failed for %s (continuing without): %v", base, bridgeErr)
		} else {
			opts.Env = map[string]string{
				"BASH_ENV":  bridge.FuncsPath(),
				"FOCI_SOCK": bridge.SockPath(),
			}
			log.Infof("delegated", "exec bridge started for %s: sock=%s funcs=%s", base, bridge.SockPath(), bridge.FuncsPath())
		}
	}

	// Set the reply and permission prompt functions before starting.
	sk := sessionKey
	if m.SendFunc != nil {
		be.SetReplyFunc(func(text string) {
			m.SendFunc(sk, text)
		})
	}
	if m.PermissionPromptFunc != nil {
		be.SetPermissionPromptFunc(func(requestID, text, summary string, choices []delegator.PromptChoice) {
			m.SetPermissionPending(sk, true)
			m.PermissionPromptFunc(sk, requestID, text, summary, choices)
		})
	}
	be.SetOnPermissionCleared(func() {
		m.SetPermissionPending(sk, false)
	})
	if m.TypingFunc != nil {
		be.SetTypingFunc(func(typing bool) {
			m.TypingFunc(sk, typing)
		})
	}
	be.SetOnSessionReady(func(sessionID string) {
		m.saveResumeID(base, sessionID)
	})

	if err := be.Start(ctx, opts); err != nil {
		// If resume failed (e.g. stale UUID), retry without resume.
		if resumeID != "" {
			log.Warnf("delegated", "start with --resume %s failed for %s: %v — retrying without resume", resumeID, base, err)
			_ = be.Close()
			be, err = m.NewBackend()
			if err != nil {
				if bridge != nil {
					bridge.Close()
				}
				return nil, fmt.Errorf("create delegated backend for %s (retry): %w", base, err)
			}
			// Re-set reply functions on the new instance.
			if m.SendFunc != nil {
				be.SetReplyFunc(func(text string) { m.SendFunc(sk, text) })
			}
			if m.PermissionPromptFunc != nil {
				be.SetPermissionPromptFunc(func(requestID, text, summary string, choices []delegator.PromptChoice) {
					m.SetPermissionPending(sk, true)
					m.PermissionPromptFunc(sk, requestID, text, summary, choices)
				})
			}
			be.SetOnPermissionCleared(func() {
				m.SetPermissionPending(sk, false)
			})
			if m.TypingFunc != nil {
				be.SetTypingFunc(func(typing bool) { m.TypingFunc(sk, typing) })
			}
			opts.ResumeSessionID = ""
			if err := be.Start(ctx, opts); err != nil {
				if bridge != nil {
					bridge.Close()
				}
				return nil, fmt.Errorf("start delegated backend for %s (no resume): %w", base, err)
			}
		} else {
			if bridge != nil {
				bridge.Close()
			}
			return nil, fmt.Errorf("start delegated backend for %s: %w", base, err)
		}
	}

	m.mu.Lock()
	if m.backends == nil {
		m.backends = make(map[string]*managedBackend)
	}
	m.backends[base] = &managedBackend{
		be:         be,
		bridge:     bridge,
		lastActive: time.Now(),
		sessionKey: sessionKey,
	}

	// Start the idle reaper on first delegated backend creation.
	if m.reaperStop == nil {
		ctx, cancel := context.WithCancel(context.Background())
		m.reaperStop = cancel
		go m.idleReaper(ctx)
	}
	m.mu.Unlock()

	if resumeID != "" {
		log.Infof("delegated", "resumed session %s for %s", resumeID, base)
	}

	// Wait for the coding agent to be ready to accept prompts.
	// Without this, early SendToPane hits a CC that's still loading.
	readyCtx, readyCancel := context.WithTimeout(ctx, 60*time.Second)
	defer readyCancel()
	if err := be.WaitReady(readyCtx); err != nil {
		log.Warnf("delegated", "WaitReady for %s: %v (proceeding anyway)", base, err)
	}

	return be, nil
}

// StopSession interrupts the current agent turn. The mechanism is
// backend-specific (tmux: Escape×2 + Ctrl-C; stream: interrupt message).
// Returns an error if no backend exists for the session.
func (m *DelegatedManager) StopSession(ctx context.Context, sessionKey string) error {
	mb, ok := m.getManaged(sessionKey)
	if !ok {
		return fmt.Errorf("no delegated backend for session %s", session.SessionKeyBase(sessionKey))
	}
	return mb.be.Interrupt(ctx)
}

// SetPermissionPending marks a session as having an outstanding permission
// prompt (pending=true) or clears it (pending=false). When pending, all
// calls to WaitForPermission block until cleared.
func (m *DelegatedManager) SetPermissionPending(sessionKey string, pending bool) {
	base := session.SessionKeyBase(sessionKey)
	mb, ok := m.getManaged(sessionKey)
	if !ok {
		return
	}
	if pending {
		mb.permMu.Lock()
		mb.permPending = true
		mb.permMu.Unlock()
		log.Debugf("delegated", "permission pending for %s", base)
	} else {
		mb.clearPermission()
		log.Debugf("delegated", "permission cleared for %s", base)
	}
}

// WaitForPermission blocks until no permission prompt is outstanding for
// the session. Returns immediately if no prompt is pending. Returns
// ctx.Err() if the context is cancelled (e.g. /stop).
func (m *DelegatedManager) WaitForPermission(ctx context.Context, sessionKey string) error {
	base := session.SessionKeyBase(sessionKey)
	mb, ok := m.getManaged(sessionKey)
	if !ok {
		return nil
	}

	mb.permMu.Lock()
	if !mb.permPending {
		mb.permMu.Unlock()
		return nil
	}

	// Lazy-init the condition variable.
	if mb.permCond == nil {
		mb.permCond = sync.NewCond(&mb.permMu)
	}

	log.Infof("delegated", "waiting for permission to clear on %s", base)

	// Wait with context cancellation support. sync.Cond doesn't natively
	// support context, so we use a goroutine to broadcast on cancel.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			mb.permCond.Broadcast() // wake up the waiter
		case <-done:
		}
	}()

	for mb.permPending {
		if ctx.Err() != nil {
			mb.permMu.Unlock()
			close(done)
			return ctx.Err()
		}
		mb.permCond.Wait()
	}
	mb.permMu.Unlock()
	close(done)

	log.Infof("delegated", "permission cleared on %s, proceeding", base)
	return nil
}

// IsPermissionPending returns whether a permission prompt is outstanding.
func (m *DelegatedManager) IsPermissionPending(sessionKey string) bool {
	mb, ok := m.getManaged(sessionKey)
	if !ok {
		return false
	}
	mb.permMu.Lock()
	defer mb.permMu.Unlock()
	return mb.permPending
}

// WaitForTurn blocks until the delegated backend for the given session key
// reports turn completion. Returns an error if no backend exists.
// Respects context cancellation/deadline.
// SessionFilePath returns the coding agent's session JSONL path for the
// given session key. Empty if the backend hasn't discovered its session yet.
func (m *DelegatedManager) SessionFilePath(sessionKey string) string {
	mb, ok := m.getManaged(sessionKey)
	if !ok {
		return ""
	}
	return mb.be.SessionFilePath()
}

func (m *DelegatedManager) WaitForTurn(ctx context.Context, sessionKey string) error {
	mb, ok := m.getManaged(sessionKey)
	if !ok {
		return fmt.Errorf("no delegated backend for session %s", session.SessionKeyBase(sessionKey))
	}
	return mb.be.WaitForTurn(ctx)
}

// ResetSession closes the delegated backend for a specific session key WITHOUT saving
// the resume ID, so the next Get() creates a completely fresh CC session.
func (m *DelegatedManager) ResetSession(sessionKey string) {
	base := session.SessionKeyBase(sessionKey)
	m.mu.Lock()
	defer m.mu.Unlock()
	mb, ok := m.backends[base]
	if !ok {
		return
	}
	mb.clearPermission()
	_ = mb.be.Close()
	if mb.bridge != nil {
		mb.bridge.Close()
	}
	delete(m.backends, base)
	// Clear any saved resume ID so next session starts fresh.
	if m.SessionIndex != nil {
		_ = m.SessionIndex.DeleteAgentMetadata(m.AgentID, m.stateKey(base))
	}
	log.Infof("delegated", "reset session %s (closed, resume ID cleared)", base)
}

// Close shuts down all managed delegated backends and the idle reaper.
func (m *DelegatedManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reaperStop != nil {
		m.reaperStop()
		m.reaperStop = nil
	}
	for key, mb := range m.backends {
		mb.clearPermission()
		m.saveResumeID(key, mb.be.SessionID())
		_ = mb.be.Close()
		if mb.bridge != nil {
			mb.bridge.Close()
		}
		delete(m.backends, key)
	}
}

// Count returns the number of active delegated backends.
func (m *DelegatedManager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.backends)
}

// stateKey returns the agent_metadata key for a CC session UUID.
func (m *DelegatedManager) stateKey(base string) string {
	return "cc_session:" + base
}

// saveResumeID persists the CC session UUID to state.db.
func (m *DelegatedManager) saveResumeID(base, sessionID string) {
	if sessionID == "" || m.SessionIndex == nil {
		return
	}
	if err := m.SessionIndex.SetAgentMetadata(m.AgentID, m.stateKey(base), sessionID); err != nil {
		log.Warnf("delegated", "save resume ID for %s: %v", base, err)
	}
}

// loadResumeID reads a saved CC session UUID from state.db.
func (m *DelegatedManager) loadResumeID(base string) string {
	if m.SessionIndex == nil {
		return ""
	}
	id, err := m.SessionIndex.GetAgentMetadata(m.AgentID, m.stateKey(base))
	if err != nil {
		return ""
	}
	return id
}

// idleReaper periodically checks for idle delegated backends and closes them.
func (m *DelegatedManager) idleReaper(ctx context.Context) {
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

// RunOnce executes a one-shot prompt via claude --print and returns the
// response synchronously. No tmux session, no watcher, no session index
// entry, no platform delivery. Ideal for internal tasks like nudge
// extraction and memory consolidation.
//
// systemPrompt is passed via --system-prompt; empty uses CC's default.
func (m *DelegatedManager) RunOnce(ctx context.Context, prompt string, systemPrompt string) (string, error) {
	args := []string{
		"--print",
		"--dangerously-skip-permissions",
		"--no-session-persistence",
		"--model", "sonnet",
	}
	if systemPrompt != "" {
		args = append(args, "--system-prompt", systemPrompt)
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = m.StartOpts.WorkDir
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	log.Infof("delegated", "RunOnce: starting claude --print (workdir=%s, system_prompt=%d bytes)",
		m.StartOpts.WorkDir, len(systemPrompt))

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude --print failed: %w (stderr: %s)", err, stderr.String())
	}

	result := strings.TrimSpace(stdout.String())
	log.Infof("delegated", "RunOnce: complete (%d bytes)", len(result))
	return result, nil
}

// closeIdle closes delegated backends that have been idle longer than timeout.
func (m *DelegatedManager) closeIdle(timeout time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for base, mb := range m.backends {
		if now.Sub(mb.lastActive) < timeout {
			continue
		}
		mb.clearPermission()
		m.saveResumeID(base, mb.be.SessionID())
		log.Infof("delegated", "closing idle session %s (idle %s, session %s)",
			base, now.Sub(mb.lastActive).Round(time.Minute), mb.be.SessionID())
		_ = mb.be.Close()
		if mb.bridge != nil {
			mb.bridge.Close()
		}
		delete(m.backends, base)
	}
}
