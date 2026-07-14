package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/procx"
	"foci/internal/session"
	"foci/internal/tools"

	"golang.org/x/sync/singleflight"
)

// DefaultIdleTimeout is the default duration after which idle delegated backends are closed.
const DefaultIdleTimeout = 3 * time.Hour

// defaultBackendReadyTimeout is the wall-clock budget WaitReady gets to see
// the coding-agent backend complete its init handshake before foci moves on.
// 60s is comfortable for cold starts on busy hosts; tests that want to
// observe the timeout path explicitly can shrink it via the
// FOCI_BACKEND_READY_TIMEOUT env var.
const defaultBackendReadyTimeout = 60 * time.Second

// backendReadyTimeout returns the duration WaitReady should wait. The
// FOCI_BACKEND_READY_TIMEOUT env var (parsed via time.ParseDuration) lets
// integration tests dial this down to a few seconds so the
// init-timeout-then-recovery scenario completes in reasonable wall-clock.
// Invalid or empty values fall back to defaultBackendReadyTimeout.
func backendReadyTimeout() time.Duration {
	raw := os.Getenv("FOCI_BACKEND_READY_TIMEOUT")
	if raw == "" {
		return defaultBackendReadyTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Warnf("delegated", "ignoring invalid FOCI_BACKEND_READY_TIMEOUT=%q (using %s)", raw, defaultBackendReadyTimeout)
		return defaultBackendReadyTimeout
	}
	return d
}

// typingFuncTimeout bounds how long the SetTypingFunc shim will wait for the
// downstream platform call (e.g. Telegram SetChatTyping) before giving up.
// Typing indicator is fire-and-forget by design — no caller cares about
// completion. See TODO #749.
const typingFuncTimeout = 2 * time.Second

// DelegatedManager creates and manages per-session Backend instances lazily
// for the delegated transport path. Each session key gets its own Backend
// (own CC session). Idle backends are closed after IdleTimeout and resumed
// on next message.
type DelegatedManager struct {
	mu       sync.Mutex
	backends map[string]*managedBackend // sessionKey → managed backend

	// NewBackend creates a fresh Backend instance (does not start it).
	NewBackend func() (delegator.Delegator, error)

	// AttachDelivery binds session-scoped delivery (SessionEvents) to a backend
	// at creation/respawn (setBackendCallbacks), so it is attached ONCE per
	// backend rather than rebuilt per turn (#1068 Phase 1). Nil in tests that
	// don't exercise delivery.
	AttachDelivery func(be delegator.Delegator, sessionKey string)

	// StartOpts returns the StartOptions for a new Backend.
	// Label and ResumeSessionID are set by the manager.
	StartOpts delegator.StartOptions

	// PermissionPromptFunc sends a permission prompt with keyboard choices.
	// requestID is the CC protocol request ID. The platform layer should
	// register a per-prompt cancel listener via RegisterPromptCancelListener
	// at the same time it sends the interactive UI, so the UI is cleaned up
	// if CC cancels the prompt before the user responds (e.g. follow-up
	// message aborted the in-flight tool).
	PermissionPromptFunc func(sessionKey, requestID, text, summary, attachmentPath string, choices []delegator.PromptChoice)

	// TypingFunc controls the platform typing indicator for a session.
	// Called with true when CC starts working, false on turn complete.
	TypingFunc func(sessionKey string, typing bool)

	// SubagentStatusFunc reports the running-subagent (CC Agent-tool spawn)
	// status for a session: detail is the comma-joined running descriptions, or
	// "" when none are running. The app maps this onto the conversation's
	// unified Activity indicator (subagents kind). Nil = subagent status is not
	// surfaced (e.g. non-app platforms, tests).
	SubagentStatusFunc func(sessionKey, detail string)

	// SystemNoticeFunc sends an out-of-band system notice directly to a
	// session's platform chat (NOT through an agent turn). Used to tell the
	// user when a requested session could not be resumed and a fresh one was
	// started in its place. Nil = notices are silently dropped (e.g. tests).
	SystemNoticeFunc func(sessionKey, text string)

	// AdoptAutonomousRun is called when a backend detects CC has begun an
	// autonomous run (one foci opened no turn for — a background-agent
	// completion, task-notification, or proactive tick). It must return a
	// release closure invoked when the run ends. Wired by the Agent to
	// markInFlight(sessionKey, delivering=true) so the run is adopted as an
	// in-flight delivering turn and the inbox worker holds concurrent
	// reflection/keepalive injections that would otherwise poison its shared
	// session sink (#1070). Nil = adoption disabled (tests, non-CC backends).
	AdoptAutonomousRun func(sessionKey string) (release func())

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

	// createGroup serializes backend creation per session key so concurrent
	// Get callers for the same key spawn exactly one CC instead of racing to
	// create (and orphan) duplicates. (P2-3.)
	createGroup singleflight.Group
}

// logger returns a component logger scoped to this manager's agent, producing
// a component of the form "delegated:<agentID>" (e.g. "delegated:clutch") so
// every delegated log line names the agent it belongs to.
func (m *DelegatedManager) logger() *log.ComponentLogger {
	return log.NewComponentLogger("delegated:" + m.AgentID)
}

// managedBackend wraps a Backend with idle tracking and resume state.
type managedBackend struct {
	be         delegator.Delegator
	bridge     *tools.ExecBridge // exec bridge for shell functions; nil if not configured
	lastActive time.Time
	sessionKey string // full session key from last message (for reply routing)

	// systemPromptHash fingerprints the system prompt this CC session was
	// launched with (SystemHash of the effective StartOptions.SystemPrompt).
	// Set once at creation; immutable for the backend's lifetime. Lets a later
	// compaction skip the reload-bounce when the on-disk prompt is unchanged.
	systemPromptHash string

	// Permission prompt gating. When a permission prompt is outstanding,
	// incoming messages and injections must wait — the backend cannot
	// process new input until the prompt is resolved.
	// See WaitForPermission / SetPermissionPending.
	permMu      sync.Mutex
	permPending bool
	permCond    *sync.Cond // lazy-init on first WaitForPermission

	// Autonomous-run adoption (#1070). autoRelease holds the release closure
	// returned by AdoptAutonomousRun for the currently-adopted autonomous run,
	// or nil when none is in flight. Set on the backend's onAutonomousStart,
	// cleared+invoked on onAutonomousEnd. The backend edge-balances start/end,
	// and session_state events are serialized on the reader goroutine, so the
	// pair is ordered; autoMu guards against the reader/reaper racing a stray
	// end. A start that finds a non-nil prior release fires it defensively so an
	// unbalanced pair can never leak the agent's in-flight adoption.
	autoMu      sync.Mutex
	autoRelease func()
}

// adoptAutonomous stores the release closure for a freshly-adopted autonomous
// run, defensively releasing any prior un-released adoption.
func (mb *managedBackend) adoptAutonomous(release func()) {
	mb.autoMu.Lock()
	prev := mb.autoRelease
	mb.autoRelease = release
	mb.autoMu.Unlock()
	if prev != nil {
		prev()
	}
}

// releaseAutonomous invokes and clears the current adoption's release closure.
// No-op when no autonomous run is adopted.
func (mb *managedBackend) releaseAutonomous() {
	mb.autoMu.Lock()
	release := mb.autoRelease
	mb.autoRelease = nil
	mb.autoMu.Unlock()
	if release != nil {
		release()
	}
}

// getManaged looks up the managed backend for a session key under the lock.
func (m *DelegatedManager) getManaged(sessionKey string) (*managedBackend, bool) {
	m.mu.Lock()
	mb, ok := m.backends[sessionKey]
	m.mu.Unlock()
	return mb, ok
}

// CacheTTL returns the prompt-cache TTL reported by the session's live backend,
// or 0 if there is no running backend or it doesn't implement CacheTTLProvider.
// Non-creating.
func (m *DelegatedManager) CacheTTL(sessionKey string) time.Duration {
	mb, ok := m.getManaged(sessionKey)
	if !ok || !mb.be.IsRunning() {
		return 0
	}
	p, ok := mb.be.(delegator.CacheTTLProvider)
	if !ok {
		return 0
	}
	return p.CacheTTL()
}

// StaticCacheTTL returns the backend type's prompt-cache TTL without needing a
// running session — it constructs a throwaway backend and reads its constant
// CacheTTL(). 0 if the backend can't be built or doesn't report a TTL. Used at
// startup (before any session is live) to validate the keepalive interval.
func (m *DelegatedManager) StaticCacheTTL() time.Duration {
	if m.NewBackend == nil {
		return 0
	}
	be, err := m.NewBackend()
	if err != nil {
		return 0
	}
	p, ok := be.(delegator.CacheTTLProvider)
	if !ok {
		return 0
	}
	return p.CacheTTL()
}

// BackendAwaitingAutonomousRun reports whether the (already-running) backend for
// sessionKey is holding across background work — a pending subagent/Bash, a live
// autonomous run, or the post-run grace (spec §4). Non-creating: an idle session
// with no live backend, or a backend that doesn't implement AutonomousRunAwaiter
// (cctmux/opencode), reports false. The inbox uses it to gate system injects.
func (m *DelegatedManager) BackendAwaitingAutonomousRun(sessionKey string) bool {
	mb, ok := m.getManaged(sessionKey)
	if !ok || !mb.be.IsRunning() {
		return false
	}
	aw, ok := mb.be.(delegator.AutonomousRunAwaiter)
	return ok && aw.AwaitingAutonomousRun()
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
// one if it doesn't exist yet. Each session key gets its own backend.
func (m *DelegatedManager) Get(ctx context.Context, sessionKey string) (delegator.Delegator, error) {
	// Fast path: an existing, running backend needs no creation, so don't
	// serialize it through the singleflight group.
	m.mu.Lock()
	if mb, ok := m.backends[sessionKey]; ok && mb.be.IsRunning() {
		mb.lastActive = time.Now()
		mb.sessionKey = sessionKey
		m.mu.Unlock()
		return mb.be, nil
	}
	m.mu.Unlock()

	// Slow path: create (or replace a dead) backend. singleflight ensures that
	// concurrent callers for the same key run getOrCreate once and share the
	// result, so exactly one CC is spawned. (P2-3.)
	v, err, _ := m.createGroup.Do(sessionKey, func() (interface{}, error) {
		return m.getOrCreate(ctx, sessionKey)
	})
	if err != nil {
		return nil, err
	}
	return v.(delegator.Delegator), nil
}

// getOrCreate looks up or creates the backend for sessionKey. It is always
// invoked through createGroup.Do, so per-key creation is serialized.
func (m *DelegatedManager) getOrCreate(ctx context.Context, sessionKey string) (delegator.Delegator, error) {
	m.mu.Lock()
	// dead is the corpse to clean up after the lock is released. Holding
	// m.mu across be.Close() risks deadlocking the entire agent if the
	// subprocess can't shut down promptly (observed 2026-05-06: a /reset
	// caught CC mid-rearm, CC died with exit 1, the waiter goroutine
	// stalled, Close blocked forever, m.mu was held forever, and every
	// subsequent inbound message silently stalled). Both the resume-ID
	// save and the map delete happen under the lock so a concurrent caller
	// observes a consistent state — only the slow IO is moved out.
	var dead *managedBackend
	if mb, ok := m.backends[sessionKey]; ok {
		if mb.be.IsRunning() {
			mb.lastActive = time.Now()
			mb.sessionKey = sessionKey
			m.mu.Unlock()
			return mb.be, nil
		}
		// Backend subprocess is dead — clean up and fall through to respawn.
		// Save the resume ID so the new subprocess can resume the CC session.
		m.logger().Warnf("backend for %s is dead, respawning", sessionKey)
		m.saveResumeID(sessionKey, mb.be.SessionID())
		delete(m.backends, sessionKey)
		dead = mb
	}

	// Check for a saved session UUID to resume.
	resumeID := m.loadResumeID(sessionKey)
	m.mu.Unlock()

	// Close the dead backend AFTER releasing m.mu — Close has bounded
	// timeouts but can still take ~10s in the pathological case.
	if dead != nil {
		_ = dead.be.Close()
		if dead.bridge != nil {
			dead.bridge.Close()
		}
	}

	// Create and start a new Backend for this session.
	be, err := m.NewBackend()
	if err != nil {
		return nil, fmt.Errorf("create delegated backend for %s: %w", sessionKey, err)
	}

	opts := m.StartOpts
	opts.Label = strings.ReplaceAll(sessionKey, "/", "-")
	opts.ResumeSessionID = resumeID
	opts.SessionKey = sessionKey

	// Rebuild the system prompt from disk at every session-start, so a fresh
	// session (reset, idle-respawn, emulated compaction) picks up character-
	// file edits instead of the prompt frozen at agent setup. Non-empty result
	// wins over the static SystemPrompt. See #828 / #706.
	if opts.SystemPromptFunc != nil {
		if p := opts.SystemPromptFunc(sessionKey); p != "" {
			opts.SystemPrompt = p
		}
	}
	// Fingerprint the effective prompt this session launches with, so a later
	// compaction can skip the reload-bounce when nothing on disk changed.
	promptHash := log.SystemHash([]string{opts.SystemPrompt})

	// Resolve the launch effort fresh for this session so a post-/effort
	// bounce relaunches at the latest level (apply_flag_settings is runtime-
	// only and resets on relaunch). Per-session, mirroring SystemPromptFunc. (#840)
	if opts.EffortFunc != nil {
		opts.Effort = opts.EffortFunc(sessionKey)
	}
	if opts.ModelFunc != nil {
		if m := opts.ModelFunc(sessionKey); m != "" {
			opts.Model = m
		}
	}

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
			bridge, bridgeErr = tools.NewSessionExecBridge(reg, bridgeCtx, sessionKey)
		} else {
			bridge, bridgeErr = tools.NewExecBridge(reg, bridgeCtx)
		}
		if bridgeErr != nil {
			m.logger().Warnf("exec bridge creation failed for %s (continuing without): %v", sessionKey, bridgeErr)
		} else {
			// Merge BASH_ENV/FOCI_SOCK into a copy of StartOpts.Env. The
			// pre-existing map carries any per-agent backend_config.env
			// entries (e.g. CCSTUB_* vars set by integration tests); a
			// plain assignment here used to clobber them. Copy first so
			// concurrent sessions don't end up mutating a shared map ref.
			merged := make(map[string]string, len(opts.Env)+2)
			for k, v := range opts.Env {
				merged[k] = v
			}
			merged["BASH_ENV"] = bridge.FuncsPath()
			merged["FOCI_SOCK"] = bridge.SockPath()
			opts.Env = merged
			m.logger().Infof("exec bridge started for %s: sock=%s funcs=%s", sessionKey, bridge.SockPath(), bridge.FuncsPath())
		}
	}

	// Create the managedBackend early so callbacks can reference it.
	mb := &managedBackend{
		be:               be,
		bridge:           bridge,
		lastActive:       time.Now(),
		sessionKey:       sessionKey,
		systemPromptHash: promptHash,
	}
	m.setBackendCallbacks(mb)

	// resumeFellBack becomes true if a requested resume ended up creating a
	// fresh session (either retry path below), so we don't log a misleading
	// "resumed session" for what was actually a fallback.
	resumeFellBack := false

	if err := be.Start(ctx, opts); err != nil {
		// If resume failed (e.g. stale UUID), retry without resume.
		if resumeID != "" {
			m.logger().Warnf("start with --resume %s failed for %s: %v — retrying without resume", resumeID, sessionKey, err)
			_ = be.Close()
			newBe, err := m.NewBackend()
			if err != nil {
				if bridge != nil {
					bridge.Close()
				}
				return nil, fmt.Errorf("create delegated backend for %s (retry): %w", sessionKey, err)
			}
			mb.be = newBe
			m.setBackendCallbacks(mb)
			opts.ResumeSessionID = ""
			if err := newBe.Start(ctx, opts); err != nil {
				if bridge != nil {
					bridge.Close()
				}
				return nil, fmt.Errorf("start delegated backend for %s (no resume): %w", sessionKey, err)
			}
			// Fresh session started in place of the missing one — tell the user.
			m.notifyResumeMissed(sessionKey, resumeID)
			resumeFellBack = true
		} else {
			if bridge != nil {
				bridge.Close()
			}
			return nil, fmt.Errorf("start delegated backend for %s: %w", sessionKey, err)
		}
	}

	m.mu.Lock()
	if m.backends == nil {
		m.backends = make(map[string]*managedBackend)
	}
	m.backends[sessionKey] = mb

	// Start the idle reaper on first delegated backend creation.
	if m.reaperStop == nil {
		ctx, cancel := context.WithCancel(context.Background())
		m.reaperStop = cancel
		go m.idleReaper(ctx)
	}
	m.mu.Unlock()

	// Wait for the coding agent to be ready to accept prompts.
	// Without this, early SendToPane hits a CC that's still loading.
	readyCtx, readyCancel := context.WithTimeout(ctx, backendReadyTimeout())
	defer readyCancel()
	if err := mb.be.WaitReady(readyCtx); err != nil {
		m.logger().Warnf("WaitReady for %s: %v", sessionKey, err)

		// If the process is already dead, a stale --resume UUID may have
		// caused CC to exit silently. Clear the resume ID and retry fresh.
		if !mb.be.IsRunning() && resumeID != "" {
			m.logger().Warnf("backend for %s died during init with --resume %s — retrying without resume", sessionKey, resumeID)
			_ = mb.be.Close()
			m.clearResumeID(sessionKey)

			newBe, err := m.NewBackend()
			if err != nil {
				if bridge != nil {
					bridge.Close()
				}
				return nil, fmt.Errorf("create delegated backend for %s (retry after init death): %w", sessionKey, err)
			}
			mb.be = newBe
			m.setBackendCallbacks(mb)
			opts.ResumeSessionID = ""
			if err := newBe.Start(ctx, opts); err != nil {
				if bridge != nil {
					bridge.Close()
				}
				return nil, fmt.Errorf("start delegated backend for %s (retry after init death): %w", sessionKey, err)
			}
			// Fresh session started in place of the missing one — tell the user.
			m.notifyResumeMissed(sessionKey, resumeID)
			resumeFellBack = true

			m.mu.Lock()
			m.backends[sessionKey] = mb
			m.mu.Unlock()

			readyCtx2, readyCancel2 := context.WithTimeout(ctx, backendReadyTimeout())
			defer readyCancel2()
			if err := mb.be.WaitReady(readyCtx2); err != nil {
				m.logger().Warnf("WaitReady for %s (retry): %v (proceeding anyway)", sessionKey, err)
			}
		}
	}

	// Log a genuine resume only once we know neither retry path fell back.
	if resumeID != "" && !resumeFellBack {
		m.logger().Infof("resumed session %s for %s", resumeID, sessionKey)
	}

	return mb.be, nil
}

// notifyResumeMissed alerts the user, out of band via their platform chat, that
// a requested session could not be resumed and a fresh session was started in
// its place. All three delegated backends converge here: opencode fails Start
// on a 404, ccstream/cctmux exit non-zero on a stale --resume — both routed
// through the retry-without-resume path above. Safe with a nil SystemNoticeFunc.
func (m *DelegatedManager) notifyResumeMissed(sessionKey, resumeID string) {
	if m.SystemNoticeFunc == nil {
		return
	}
	m.SystemNoticeFunc(sessionKey,
		"⚠️ Couldn't resume your previous session (`"+resumeID+"`) — it may have been "+
			"evicted or cleared. Started a fresh session instead; earlier context won't carry over.")
}

// StopSession interrupts the current agent turn. The mechanism is
// backend-specific (tmux: Escape×2 + Ctrl-C; stream: interrupt message).
// Returns an error if no backend exists for the session.
func (m *DelegatedManager) StopSession(ctx context.Context, sessionKey string) error {
	mb, ok := m.getManaged(sessionKey)
	if !ok {
		return fmt.Errorf("no delegated backend for session %s", sessionKey)
	}
	return mb.be.Interrupt(ctx)
}

// RegisterPromptCancelListener appends a listener fired when the prompt with
// requestID is cancelled by a non-user path (e.g. CC's control_cancel_request
// after a follow-up message aborted the in-flight tool). The listener does
// NOT fire on normal user responses. Used by the platform layer to clean up
// per-prompt UI (e.g. disable the inline keyboard) so the user can't click
// an already-resolved button. No-op if no managed backend exists for
// sessionKey, or if the backend doesn't track outstanding prompts.
func (m *DelegatedManager) RegisterPromptCancelListener(sessionKey, requestID string, fn func(reason string)) {
	mb, ok := m.getManaged(sessionKey)
	if !ok {
		return
	}
	mb.be.RegisterPromptCancelListener(requestID, fn)
}

// SetPermissionPending marks a session as having an outstanding permission
// prompt (pending=true) or clears it (pending=false). When pending, all
// calls to WaitForPermission block until cleared.
func (m *DelegatedManager) SetPermissionPending(sessionKey string, pending bool) {
	mb, ok := m.getManaged(sessionKey)
	if !ok {
		return
	}
	if pending {
		mb.permMu.Lock()
		mb.permPending = true
		mb.permMu.Unlock()
		m.logger().Debugf("permission pending for %s", sessionKey)
	} else {
		mb.clearPermission()
		m.logger().Debugf("permission cleared for %s", sessionKey)
	}
}

// WaitForPermission blocks until no permission prompt is outstanding for
// the session. Returns immediately if no prompt is pending. Returns
// ctx.Err() if the context is cancelled (e.g. /stop).
func (m *DelegatedManager) WaitForPermission(ctx context.Context, sessionKey string) error {
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

	m.logger().Infof("waiting for permission to clear on %s", sessionKey)

	// Wait with context cancellation support. sync.Cond doesn't natively
	// support context, so we use a goroutine to broadcast on cancel.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// Hold permMu around the broadcast: otherwise it can land between
			// the waiter's predicate check and its Wait() call, and the wakeup
			// is lost — the waiter then blocks until the next clearPermission
			// broadcast, which on a cancelled turn may never come.
			mb.permMu.Lock()
			mb.permCond.Broadcast() // wake up the waiter
			mb.permMu.Unlock()
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

	m.logger().Infof("permission cleared on %s, proceeding", sessionKey)
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
		return fmt.Errorf("no delegated backend for session %s", sessionKey)
	}
	return mb.be.WaitForTurn(ctx)
}

// ResetSession closes the delegated backend for a specific session key WITHOUT saving
// the resume ID, so the next Get() creates a completely fresh CC session.
//
// The map mutation is performed under m.mu, but the (potentially slow)
// be.Close() runs after the lock is released. Holding m.mu across Close
// risked freezing every other agent operation if the backend was unable
// to shut down promptly — a single stuck CC subprocess could deadlock
// inbound message handling for the entire agent (observed 2026-05-06).
// Close itself has bounded timeouts (see ccstream Close), so even in the
// pathological case this method returns within ~10s.
func (m *DelegatedManager) ResetSession(sessionKey string) {
	if m.closeManaged(sessionKey, true) {
		m.logger().Infof("reset session %s (closed, resume ID cleared)", sessionKey)
	}
}

// BounceSession closes the backend for sessionKey but KEEPS its saved resume
// ID, so the next message respawns CC with --resume <same session> — picking up
// the same (now compacted) conversation while rebuilding the system prompt from
// disk via StartOptions.SystemPromptFunc. Used after a delegated compaction so
// character/skill file edits reload without losing context (#828 Part B). The
// real-CC behaviour this relies on — --resume honours a freshly-sent initialize
// systemPrompt — is verified (clutch docs/resume_prompt_probe.py).
func (m *DelegatedManager) BounceSession(sessionKey string) {
	if m.closeManaged(sessionKey, false) {
		m.logger().Infof("bounced session %s (closed, resume ID kept — prompt reloads on respawn)", sessionKey)
	}
}

// BounceSessionIfPromptChanged bounces the session only when the system prompt
// rebuilt from disk now differs from the one the running CC session launched
// with. Returns true if it bounced. This is the optimisation over the
// unconditional #828 post-compaction bounce: when character files are unchanged
// and no skill has been added or removed, the running session's prompt is
// already current, so the restart — and the flow interruption it causes — is
// pure cost.
//
// The fingerprint is the same string getOrCreate launches with: the full
// effective prompt from SystemPromptFunc — environment block (## Platform
// section, shell-tool list, permission allowlist) + character files + skill
// blocks. It therefore captures character-file edits, skill add/remove (the
// skill list lives in the prompt), and env-visible changes (permission
// allowlist edits, shell-tool set, the session's platform claim) — but NOT
// skill body-content edits (skill bodies are read on demand and never appear
// in the prompt). The env block resolves the platform from the durable chat
// claim rather than connection liveness (see cmd/foci-gw platformForSession),
// so the fingerprint is stable across startup transients and a bounce here
// always reflects a real change. Falls back to an unconditional bounce when no
// SystemPromptFunc is configured (the prompt can't be fingerprinted).
func (m *DelegatedManager) BounceSessionIfPromptChanged(sessionKey string) bool {
	// Every return path emits one "compaction reload gate:" DEBUG line with an
	// explicit restart=yes/no token, so a single grep over the log confirms both
	// outcomes (restart and no-restart) actually occur in practice.
	mb, ok := m.getManaged(sessionKey)
	if !ok {
		m.logger().Debugf("compaction reload gate: session=%s restart=no reason=no-live-backend", sessionKey)
		return false // no live backend → nothing to reload
	}
	if m.StartOpts.SystemPromptFunc == nil {
		m.logger().Debugf("compaction reload gate: session=%s restart=yes reason=no-prompt-fingerprint (no SystemPromptFunc)", sessionKey)
		m.BounceSession(sessionKey)
		return true
	}
	p := m.StartOpts.SystemPromptFunc(sessionKey)
	if p == "" {
		p = m.StartOpts.SystemPrompt
	}
	live := log.SystemHash([]string{p})
	if live == mb.systemPromptHash {
		m.logger().Debugf("compaction reload gate: session=%s restart=no reason=prompt-unchanged hash=%s", sessionKey, live)
		return false
	}
	m.logger().Debugf("compaction reload gate: session=%s restart=yes reason=prompt-changed hash=%s->%s", sessionKey, mb.systemPromptHash, live)
	m.BounceSession(sessionKey)
	return true
}

// closeManaged closes and unmaps the backend for sessionKey, returning whether
// one was present. When clearResume is true the saved CC resume ID is dropped
// (next session starts fresh); when false it is kept (next session resumes the
// same CC conversation). The unmap and optional resume-ID clear happen under
// m.mu so a concurrent Get() observes consistent state; the slow Close() calls
// run after unlock.
func (m *DelegatedManager) closeManaged(sessionKey string, clearResume bool) bool {
	m.mu.Lock()
	mb, ok := m.backends[sessionKey]
	if !ok {
		m.mu.Unlock()
		return false
	}
	delete(m.backends, sessionKey)
	if clearResume {
		m.clearResumeID(sessionKey)
	}
	m.mu.Unlock()

	mb.clearPermission()
	_ = mb.be.Close()
	if mb.bridge != nil {
		mb.bridge.Close()
	}
	return true
}

// Close shuts down all managed delegated backends and the idle reaper.
//
// Map mutation happens under m.mu; the (potentially slow) be.Close() calls
// run after the lock is released. This mirrors the pattern in Get and
// ResetSession (see 3af4dce5) — and is critical because the bounded
// typingFunc wrapper in setBackendCallbacks acquires m.mu via sk() on the
// waiter goroutine. Holding m.mu across be.Close would deadlock the
// waiter against this caller, the 2s typingFunc timer would never arm
// (sk() is synchronous, before the timer), and Close would only return
// via the ccstream bounded-shutdown fallback. See TODO #749.
func (m *DelegatedManager) Close() {
	m.mu.Lock()
	if m.reaperStop != nil {
		m.reaperStop()
		m.reaperStop = nil
	}
	dead := make([]*managedBackend, 0, len(m.backends))
	deadKeys := make([]string, 0, len(m.backends))
	for key, mb := range m.backends {
		mb.clearPermission()
		m.saveResumeID(key, mb.be.SessionID())
		dead = append(dead, mb)
		deadKeys = append(deadKeys, key)
	}
	for _, key := range deadKeys {
		delete(m.backends, key)
	}
	m.mu.Unlock()

	if len(dead) > 0 {
		m.logger().Infof("closing %d delegated backend(s): %v", len(dead), deadKeys)
	}
	for _, mb := range dead {
		_ = mb.be.Close()
		if mb.bridge != nil {
			mb.bridge.Close()
		}
	}
}

// Count returns the number of active delegated backends.
func (m *DelegatedManager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.backends)
}

// setBackendCallbacks wires up reply, permission, typing, and session-ready
// callbacks on a managed backend. Callbacks read mb.sessionKey dynamically
// so they always use the backend's current key.
func (m *DelegatedManager) setBackendCallbacks(mb *managedBackend) {
	sk := func() string {
		m.mu.Lock()
		defer m.mu.Unlock()
		return mb.sessionKey
	}
	if m.PermissionPromptFunc != nil {
		mb.be.SetPermissionPromptFunc(func(requestID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
			key := sk()
			m.SetPermissionPending(key, true)
			m.PermissionPromptFunc(key, requestID, text, summary, attachmentPath, choices)
		})
	}
	mb.be.SetOnPromptsCleared(func() {
		m.SetPermissionPending(sk(), false)
	})
	if m.TypingFunc != nil {
		mb.be.SetTypingFunc(func(typing bool) {
			// Typing indicator is fire-and-forget — no return value, no error.
			// Run with a bounded timeout so a hung downstream call (e.g. a
			// stale-connection-pool SetChatTyping waiting on a server response
			// that won't come) cannot wedge the caller.
			//
			// sk() MUST be called from inside the inner goroutine. It acquires
			// m.mu, and this wrapper runs synchronously on the ccstream waiter
			// goroutine during finalizeExit. If sk() ran outside the goroutine,
			// and any caller of be.Close() held m.mu (the historical bug behind
			// TODO #749 root-cause), the wrapper would block before its timer
			// armed — defeating the 2s bound entirely. Inside the goroutine,
			// the outer select fires its 2s timer regardless of what the inner
			// goroutine is blocked on, so the wrapper always returns within
			// typingFuncTimeout.
			done := make(chan struct{})
			var keyHolder atomic.Pointer[string]
			go func() {
				defer close(done)
				key := sk()
				keyHolder.Store(&key)
				m.TypingFunc(key, typing)
			}()
			select {
			case <-done:
			case <-time.After(typingFuncTimeout):
				keyStr := "<unknown — sk() still blocked>"
				if kp := keyHolder.Load(); kp != nil {
					keyStr = *kp
				}
				m.logger().Warnf("typingFunc(typing=%v) for %s did not return within %s — abandoning (possible Telegram SetChatTyping stall or m.mu held by a slow caller)", typing, keyStr, typingFuncTimeout)
			}
		})
	}
	// Wire the subagent (Agent-tool) status tracker → SubagentStatusFunc. The
	// setter lives on the concrete CC backends (ccstream/opencode), not the
	// Delegator interface, so we reach it via a narrow type assertion; backends
	// without it (e.g. the legacy tmux backend, which self-wires its own status
	// sink) are left untouched. Fixes the ccstream gap where the tracker's
	// OnStatus was never wired at all. sk() resolves the current session key
	// dynamically, mirroring the typing/session-ready wiring above.
	if m.SubagentStatusFunc != nil {
		if setter, ok := mb.be.(interface {
			SetOnSubagentStatus(fn func(detail string))
		}); ok {
			setter.SetOnSubagentStatus(func(detail string) {
				m.SubagentStatusFunc(sk(), detail)
			})
		}
	}

	mb.be.SetOnSessionReady(func(sessionID string) {
		m.saveResumeID(sk(), sessionID)
	})

	// Bind session-scoped delivery once, here at acquisition — the SessionEvents
	// live for the backend's lifetime and are never rebuilt per turn (#1068).
	if m.AttachDelivery != nil {
		m.AttachDelivery(mb.be, sk())
	}

	// Adopt CC autonomous runs as in-flight delivering turns (#1070). The
	// setters live on the concrete CC backend, not the Delegator interface, so
	// reach them via a narrow type assertion (mirrors SetOnSubagentStatus).
	// onAutonomousStart marks the run in flight and stashes the release; the
	// paired onAutonomousEnd fires it. The backend edge-balances the pair from
	// every run-ending site (idle, exit, adoption), so releases stay matched.
	if m.AdoptAutonomousRun != nil {
		if setter, ok := mb.be.(interface {
			SetOnAutonomousStart(fn func())
			SetOnAutonomousEnd(fn func())
		}); ok {
			setter.SetOnAutonomousStart(func() {
				mb.adoptAutonomous(m.AdoptAutonomousRun(sk()))
			})
			setter.SetOnAutonomousEnd(func() {
				mb.releaseAutonomous()
			})
		}
	}
}

// resumeIDKey is the session_metadata key under which CC backend resume UUIDs
// are stored. The data is session-scoped (each post-compact JSONL is a
// distinct UUID) and the session key already encodes the agent ID, so we
// don't store agent_id separately.
const resumeIDKey = "cc_resume_id"

// saveResumeID persists the CC session UUID to state.db and appends it to
// the cc_resume_history provenance timeline, so "which CC session was live
// for this key at time T" stays answerable after resets and respawns.
func (m *DelegatedManager) saveResumeID(sessionKey, sessionID string) {
	if sessionID == "" || m.SessionIndex == nil {
		return
	}
	if err := m.SessionIndex.SetSessionMetadata(sessionKey, resumeIDKey, sessionID); err != nil {
		m.logger().Warnf("save resume ID for %s: %v", sessionKey, err)
	}
	m.SessionIndex.RecordCCResume(sessionKey, sessionID)
}

// clearResumeID removes a saved CC session UUID from state.db.
func (m *DelegatedManager) clearResumeID(sessionKey string) {
	if m.SessionIndex == nil {
		return
	}
	if err := m.SessionIndex.DeleteSessionMetadata(sessionKey, resumeIDKey); err != nil {
		m.logger().Warnf("clear resume ID for %s: %v", sessionKey, err)
	}
}

// RemapSession moves the live backend (if any) and the saved CC resume ID
// from oldKey to newKey. Used when a reflection branch adopts an expiring
// session's backend: the branch drives the existing CC session — which holds
// the conversation context in-process (or on disk via the resume ID) — while
// the old key is left clean for a fresh backend on next contact.
func (m *DelegatedManager) RemapSession(oldKey, newKey string) {
	if oldKey == newKey || newKey == "" {
		return
	}
	m.mu.Lock()
	var remapped *managedBackend
	if mb, ok := m.backends[oldKey]; ok {
		delete(m.backends, oldKey)
		mb.sessionKey = newKey
		m.backends[newKey] = mb
		remapped = mb
		m.logger().Infof("backend remapped %s → %s", oldKey, newKey)
	}
	m.mu.Unlock()

	// Re-point session-scoped delivery from oldKey's router to newKey's.
	// SessionEvents are bound to a router ONCE, at acquisition (AttachDelivery
	// in setBackendCallbacks — never per turn, #1068), captured around the key's
	// string at that moment. RemapSession alone leaves the backend emitting into
	// oldKey's router, but the branch turn that now drives this backend registers
	// its sink on newKey's router (a.sessionRouter(ts.SessionKey), turn_orchestrator).
	// For a session-end/reflection turn that sink is a NopSink — meant to suppress
	// the reflection text. Without this re-attach the two routers diverge: the
	// NopSink lands on newKey's router while the backend's output falls through
	// oldKey's late-delivery fallback and leaks the reflection/memory text to the
	// app. Re-attaching binds the backend to newKey's router so the NopSink
	// actually suppresses it. Safe re: #1068 — this is a one-time lifecycle
	// re-attach (like acquisition), not a per-turn ctx-sink rebind.
	if remapped != nil && m.AttachDelivery != nil {
		m.AttachDelivery(remapped.be, newKey)
	}

	if id := m.loadResumeID(oldKey); id != "" {
		m.saveResumeID(newKey, id)
		m.clearResumeID(oldKey)
	}
}

// BackendCanBranch reports whether this agent's backend can fork its
// conversation (implements delegator.BackendBrancher). It constructs an
// unstarted backend instance and probes the interface — no process is spawned.
// Used by BranchStrategyFor to choose BranchForkBackend.
func (m *DelegatedManager) BackendCanBranch() bool {
	if m.NewBackend == nil {
		return false
	}
	be, err := m.NewBackend()
	if err != nil {
		return false
	}
	_, ok := be.(delegator.BackendBrancher)
	return ok
}

// CleanupBackendSession deletes the on-disk backend session file for sessionID
// (via BackendBrancher.CleanupSession). No-op if the backend can't branch or
// sessionID is empty. A pure filesystem delete — no process is spawned.
func (m *DelegatedManager) CleanupBackendSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	be, err := m.NewBackend()
	if err != nil {
		return err
	}
	br, ok := be.(delegator.BackendBrancher)
	if !ok {
		return nil
	}
	return br.CleanupSession(ctx, delegator.CleanupRequest{SessionID: sessionID, WorkDir: m.StartOpts.WorkDir, AgentID: m.StartOpts.AgentID})
}

// ForkParentSession forks parentKey's backend conversation and returns the new
// backend session id (a clone ready to resume). The parent is left untouched.
//
// It resolves the parent's live backend session id (or persisted cc_resume_id),
// then asks a freshly-constructed (unstarted) backend to fork that session on
// disk. Returns ("", nil) when the fork can't happen (backend not
// branch-capable, or the parent has no known backend session yet — the caller
// should fall back), and ("", err) on a genuine fork failure. It deliberately
// does NOT create a branch session key or persist anything: the caller mints
// the branch key only on success, so a failed fork leaves no orphan.
func (m *DelegatedManager) ForkParentSession(ctx context.Context, parentKey string) (string, error) {
	parentID := m.parentSessionID(parentKey)
	if parentID == "" {
		return "", nil // no known backend session to fork
	}
	be, err := m.NewBackend()
	if err != nil {
		return "", fmt.Errorf("fork parent: new backend: %w", err)
	}
	br, ok := be.(delegator.BackendBrancher)
	if !ok {
		return "", nil // backend can't branch
	}
	res, err := br.ForkSession(ctx, delegator.ForkRequest{
		ParentSessionID: parentID,
		WorkDir:         m.StartOpts.WorkDir,
		AgentID:         m.StartOpts.AgentID,
	})
	if err != nil {
		return "", fmt.Errorf("fork parent %s (%s): %w", parentKey, parentID, err)
	}
	if res.SessionID == "" {
		return "", fmt.Errorf("fork parent %s (%s): empty forked session id", parentKey, parentID)
	}
	return res.SessionID, nil
}

// parentSessionID returns the authoritative backend session id for a key: the
// live backend's current SessionID() if one is running (it may hold a UUID not
// yet persisted), otherwise the saved cc_resume_id.
func (m *DelegatedManager) parentSessionID(sessionKey string) string {
	m.mu.Lock()
	if mb, ok := m.backends[sessionKey]; ok && mb.be.IsRunning() {
		if id := mb.be.SessionID(); id != "" {
			m.mu.Unlock()
			return id
		}
	}
	m.mu.Unlock()
	return m.loadResumeID(sessionKey)
}

// loadResumeID reads a saved CC session UUID from state.db.
func (m *DelegatedManager) loadResumeID(sessionKey string) string {
	if m.SessionIndex == nil {
		return ""
	}
	id, err := m.SessionIndex.GetSessionMetadata(sessionKey, resumeIDKey)
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

	// Honour the same claude_binary override that ccstream uses, so
	// integration tests pointing foci at bin/cc-stub also intercept
	// RunOnce invocations (nudge extraction, memory consolidation,
	// first-run onboarding). Empty = "claude" on $PATH.
	claudeBin := "claude"
	if m.StartOpts.ClaudeBinary != "" {
		claudeBin = m.StartOpts.ClaudeBinary
	}
	cmd := procx.Spawn(ctx, claudeBin, args...)
	cmd.Dir = m.StartOpts.WorkDir
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	m.logger().Infof("RunOnce: starting %s --print (workdir=%s, system_prompt=%d bytes)",
		claudeBin, m.StartOpts.WorkDir, len(systemPrompt))

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude --print failed: %w (stderr: %s)", err, stderr.String())
	}

	result := strings.TrimSpace(stdout.String())
	m.logger().Infof("RunOnce: complete (%d bytes)", len(result))
	return result, nil
}

// BackendInfo returns a human-readable status line for the backend serving
// the given session key. Returns "" if no backend exists. compacting is the
// agent-level compaction-in-flight state (see Agent.IsCompacting) — passed in
// because compaction is a fire-and-forget slash command that never sets the
// backend's turn-in-flight flag (#725).
func (m *DelegatedManager) BackendInfo(sessionKey string, compacting bool) string {
	mb, ok := m.getManaged(sessionKey)
	if !ok {
		return ""
	}

	running := mb.be.IsRunning()
	inFlight := mb.be.IsTurnInFlight()

	status := "idle"
	if !running {
		status = "dead"
	} else if inFlight {
		status = "processing"
	} else if compacting {
		// Compaction is a fire-and-forget slash command: it never sets the
		// in-flight flag, so without this the backend reads "idle" mid-compact (#725).
		status = "compacting"
	}

	info := status
	if ac, ok := mb.be.(delegator.ActivityChecker); ok {
		if t := ac.LastActivity(); !t.IsZero() {
			info += fmt.Sprintf(" | last event: %s ago", time.Since(t).Round(time.Second))
		}
	}

	if sid := mb.be.SessionID(); sid != "" {
		info += fmt.Sprintf(" | session: %s", sid)
	}

	if detail := mb.be.StatusDetail(); detail != "" {
		info += " | " + detail
	}

	return info
}

// closeIdle closes delegated backends that have been idle longer than timeout.
//
// Map mutation happens under m.mu; the (potentially slow) be.Close() calls
// run after the lock is released. Holding m.mu across be.Close would
// deadlock the waiter goroutine — it calls finalizeExit → typingFunc on
// its way to waitCh, and the bounded typingFunc wrapper takes m.mu via
// sk(). The 2s timer is set up *after* sk() returns, so the timer never
// arms and Close blocks the full bounded-shutdown timeout. See TODO #749.
func (m *DelegatedManager) closeIdle(timeout time.Duration) {
	m.mu.Lock()

	now := time.Now()
	type corpse struct {
		key string
		mb  *managedBackend
	}
	var dead []corpse
	for key, mb := range m.backends {
		// Use backend stream activity if available (tracks actual CC events),
		// falling back to lastActive (when foci last sent a message).
		lastSeen := mb.lastActive
		if ac, ok := mb.be.(delegator.ActivityChecker); ok {
			if t := ac.LastActivity(); !t.IsZero() && t.After(lastSeen) {
				lastSeen = t
			}
		}
		if now.Sub(lastSeen) < timeout {
			continue
		}
		mb.clearPermission()
		m.saveResumeID(key, mb.be.SessionID())
		m.logger().Infof("closing idle session %s (idle %s, session %s)",
			key, now.Sub(lastSeen).Round(time.Minute), mb.be.SessionID())
		delete(m.backends, key)
		dead = append(dead, corpse{key: key, mb: mb})
	}
	m.mu.Unlock()

	for _, c := range dead {
		_ = c.mb.be.Close()
		if c.mb.bridge != nil {
			c.mb.bridge.Close()
		}
	}
}
