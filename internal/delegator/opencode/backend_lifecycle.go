// backend_lifecycle.go — Backend.Start / Close / WaitReady / CheckReady.
// HTTP-driven implementations of the delegator.Delegator lifecycle methods.
// Server acquisition goes through acquireServer (the per-agent pool);
// HTTP calls hit server.baseURL; the dispatcher goroutine + per-session
// registry are wired via server.registerSession.
//
// Tests bypass the subprocess-spawning acquireServer by setting
// b.server directly (pointing at httptest.Server) before calling Start;
// Start detects an existing server and skips acquisition. The production
// path always goes through acquireServer because Backend zero-value has
// server == nil.

package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"foci/internal/delegator"
	"foci/internal/delegator/autoapprove"
	"foci/internal/log"
)

// httpTimeout caps individual HTTP requests to the opencode server.
// Long enough for normal request/response (incl. model time when using
// the synchronous /message endpoint); short enough that a wedged server
// doesn't hang the caller indefinitely. The async /prompt_async endpoint
// returns immediately so this is generous for it.
const httpTimeout = 60 * time.Second

// Start acquires a Server (lazily starting one if none exists for this
// agent), creates an opencode session via POST /session, registers self
// with the Server's per-session registry (which starts the dispatcher
// goroutine), and optionally injects a system prompt as a noReply user
// message. Fires onSessionReady once the sessionID is known, closes
// readyCh, and marks the Backend running.
//
// Idempotent: a second Start on an already-running Backend is a no-op.
// DelegatedManager creates a fresh Backend per session so double-Start
// doesn't occur in production, but the guard prevents a panic from
// close(b.readyCh) on an already-closed channel if a test or future
// caller accidentally double-invokes.
//
// Acquire is skipped if b.server is already populated — tests use this
// to inject a Server pointing at httptest. The production path always
// acquires because Backend zero-value has server == nil.
func (b *Backend) Start(ctx context.Context, opts delegator.StartOptions) error {
	// Idempotent guard — see method doc.
	if b.IsRunning() {
		return nil
	}

	log.NewComponentLogger(b.logComponent()).Infof("Start: agentID=%s workDir=%s", opts.AgentID, opts.WorkDir)

	b.agentID = opts.AgentID
	b.startOpts = opts
	b.workDir = opts.WorkDir
	b.autoApproveRules = autoapprove.Compile(opts.AutoApproveRules)

	// Ensure the foci plugins exist BEFORE acquiring the server. opencode
	// loads plugins at subprocess startup; if we write them after
	// acquireServer starts the subprocess, they won't be loaded until the
	// next server restart. Both are idempotent so they're cheap on every Start.
	//   - session-env: per-session FOCI_SOCK/BASH_ENV routing (shell.env hook)
	//   - blank-system: suppresses opencode's default system prompt so only
	//     foci's prompt (POST "system" field) reaches the model
	EnsureSessionEnvPlugin(opts.WorkDir)
	EnsureBlankSystemPlugin(opts.WorkDir)

	if b.server == nil {
		srv, err := acquireServer(opts.AgentID, b.serverConfigFromOpts(opts), opts.Env)
		if err != nil {
			return fmt.Errorf("opencode: acquire server: %w", err)
		}
		b.server = srv
	}
	b.autoApproveEnv = b.server.effectiveEnv
	if b.autoApproveEnv == nil {
		// Test-injected servers have no subprocess environment snapshot.
		b.autoApproveEnv = autoapprove.EnvironmentFromList(b.server.buildCmdEnv())
	}

	// Acquire a session ID: resume the saved session if one was provided
	// and still exists on the server, otherwise create a new one. Resume
	// avoids orphaning sessions across foci restarts and preserves
	// conversation context. A 404 on the GET (session evicted, opencode.db
	// wiped, etc.) — like any other resume failure — fails Start so the
	// manager's retry-without-resume path runs; that path creates the fresh
	// session and alerts the user their old session couldn't be resumed.
	sessionID := ""
	resumed := false
	if opts.ResumeSessionID != "" {
		ok, err := b.resumeSession(ctx, opts.ResumeSessionID)
		if err != nil {
			return fmt.Errorf("opencode: probe resume session %s: %w", opts.ResumeSessionID, err)
		}
		if ok {
			sessionID = opts.ResumeSessionID
			resumed = true
			log.NewComponentLogger(b.logComponent()).Infof("Start: resumed session id=%s", sessionID)
		} else {
			// Requested session is gone (404). Rather than silently creating a
			// new session inline, fail Start so DelegatedManager's
			// retry-without-resume path runs — the single place (shared with
			// ccstream/cctmux, whose CLI exits non-zero on a stale --resume)
			// that both creates the fresh session AND alerts the user that
			// their old session could not be resumed.
			return fmt.Errorf("opencode: resume session %s not found", opts.ResumeSessionID)
		}
	}
	if sessionID == "" {
		newID, err := b.createSession(ctx)
		if err != nil {
			return fmt.Errorf("opencode: create session: %w", err)
		}
		sessionID = newID
		log.NewComponentLogger(b.logComponent()).Infof("Start: session created id=%s", sessionID)
	}
	b.sessionID = sessionID

	// Write the per-session env mapping. The plugin (ensured above,
	// before server start) reads this on every bash spawn and injects
	// the correct FOCI_SOCK/BASH_ENV, overriding the shared subprocess's
	// first-session-pinned bridge.
	WriteSessionEnvFile(sessionID, opts.Env)

	// Register with the Server so SSE events route to us. Side effect:
	// launches the dispatcher goroutine which drains b.events
	// and invokes the per-Backend handler (handleEvent, set above).
	//
	// SetDispatchHandler MUST be called before registerSession — the
	// handler is captured at goroutine-start time.
	b.SetDispatchHandler(b.handleEvent)
	b.server.registerSession(b)
	log.NewComponentLogger(b.logComponent()).Debugf("Start: registered with server, dispatcher started")

	// Apply default permission mode if configured. opencode's defaults
	// are permissive (most tools "allow"); foci wants "ask" for side-
	// effecting tools so the user gets prompted via the permission
	// keyboard. Per-agent backend_config.default_permission overrides.
	if dp, ok := b.cfg["default_permission"].(string); ok && dp != "" {
		if err := b.patchConfig(ctx, map[string]any{
			"permission": map[string]string{"*": dp},
		}); err != nil {
			log.NewComponentLogger(b.logComponent()).Warnf("default_permission PATCH failed: %v — using opencode defaults", err)
		}
	}

	// Resolve and store the model for inclusion in every prompt body.
	// opencode's per-message "model" field is the actual runtime model
	// override (PATCH /config is documented but doesn't take effect on
	// 1.17.x). Empty = let opencode's own config stand.
	//
	// opts.Model is often foci's generic cross-backend default (e.g.
	// "sonnet") rather than something meant for opencode specifically — CC
	// accepts that bare name, opencode never did. Unlike the interactive
	// /model path (SendControl), where an unresolved model is a genuine user
	// request that should fail loudly, a launch-time default that doesn't
	// resolve is not a reason to fail Start: fall back to opencode's own
	// config (opencode.json), matching the pre-validation behavior. See
	// foci bug: 232dc546 turned this into a hard Start failure and broke
	// every opencode agent whose config model isn't a real opencode id.
	if opts.Model != "" {
		binaryPath, _ := b.cfg["binary"].(string)
		resolved, err := b.resolveModelFn(ctx, binaryPath, opts.WorkDir, opts.Model)
		if err != nil {
			log.NewComponentLogger(b.logComponent()).Warnf("Start: model %q not resolved by opencode (%v) — using opencode's own default", opts.Model, err)
		} else {
			b.mu.Lock()
			b.model = resolved
			b.mu.Unlock()
		}
	}

	// Resolve and store the system prompt (rebuilt from disk here so a resume/
	// compaction-bounce picks up character-file edits). It reaches the model
	// two ways: written to the per-session file that the blank-system plugin
	// reads to REPLACE opencode's default (blank_system.go), and — as a
	// fallback if that file is ever missing — sent in the "system" field of
	// every POST /prompt_async body.
	if opts.SystemPromptFunc != nil {
		b.systemPrompt = opts.SystemPromptFunc(b.sessionID)
	} else {
		b.systemPrompt = opts.SystemPrompt
	}
	WriteSessionSystemFile(b.sessionID, b.systemPrompt)

	// Resolve the compaction summary prompt (foci's compaction-summary.md) and
	// write it to the per-session file the plugin's session.compacting hook
	// reads, so opencode's internal /summarize follows foci's format. Empty =
	// leave opencode's default compaction template.
	if opts.CompactionPromptFunc != nil {
		WriteSessionCompactFile(b.sessionID, opts.CompactionPromptFunc(b.sessionID))
	}

	_ = resumed // resume handled above via SystemPromptFunc rebuild + per-session file rewrite

	b.mu.Lock()
	b.running = true
	b.mu.Unlock()

	if b.onSessionReady != nil {
		b.onSessionReady(sessionID)
	}
	close(b.readyCh)
	return nil
}

// Close deregisters from the Server (stopping the dispatcher goroutine
// after any in-flight handler invocation completes) and releases the
// per-agent Server reference (which triggers Server shutdown if this was
// the last session).
//
// Non-destructive by design: the opencode session is intentionally left
// in place on the server (and in opencode.db) so it can be resumed later
// — by the post-compaction bounce, the idle reaper, or after a foci
// restart. Deleting it here would defeat every "keep resume ID, close,
// resume later" path: the opencode session is server-side state (unlike
// ccstream's local conversation file, whose Close only kills a process).
// Orphaned sessions accumulate in opencode.db — opencode does NOT reap
// idle sessions itself (verified: rows survive indefinitely; ~100 orphaned
// over 4 months at the time of writing). Growth is slow and SQLite stays
// performant to GB scale, so cleanup is deferred hygiene (a periodic
// sweep) rather than urgent. If a session ever genuinely needs destroying
// before such a sweep exists, that is an explicit, separate operation —
// never folded into Close.
//
// Idempotent via the mu-guarded running flag: a second Close is a no-op.
// Safe to call on a never-Started Backend (server == nil, sessionID == "").
func (b *Backend) Close() error {
	b.mu.Lock()
	running := b.running
	b.running = false
	b.mu.Unlock()
	if !running {
		return nil
	}

	// Remove the per-session plugin data (env mapping + system + compaction
	// prompts) so the plugins don't read stale entries for a closed session.
	RemoveSessionEnvFile(b.sessionID)
	RemoveSessionSystemFile(b.sessionID)
	RemoveSessionCompactFile(b.sessionID)

	// Deregister from Server — stops the dispatcher and waits for any
	// in-flight handler call to complete (dispatcher contract). Safe to
	// call when b.server is nil (test Backend that bypassed Start).
	if b.server != nil {
		b.server.unregisterSession(b.sessionID)
	}

	// Release the per-agent Server reference. No-op if b.server wasn't
	// acquired via acquireServer (test Backends that set b.server
	// directly — agentID isn't in the pool so releaseServer returns
	// immediately). Production Backends always go through acquireServer.
	if b.agentID != "" {
		releaseServer(b.agentID, b.server)
	}

	// Force-complete any in-flight turn instead of silently dropping it.
	// cancelTurn only clears turnActive/turnEvents — it never fires
	// OnTurnComplete, so the agent-side turn (runPostTurn blocked on
	// CompletionChan) would hang forever: no TurnComplete, hasTurn/in-flight
	// dangling until a restart. This bites the idle reaper most: when a
	// backend whose completion signal never arrived (e.g. the SSE subscriber
	// failed to connect, so no session.idle ever came) is closed after the
	// idle timeout, finalizeExit's synthetic session.error can't rescue the
	// turn — unregisterSession above already removed us from s.sessions, so
	// finalizeExit iterates zero backends. failInFlightTurn is the same
	// force-completion path a real session.error uses; it fires
	// OnTurnComplete (closing CompletionChan → runPostTurn unblocks →
	// TurnComplete emitted → in-flight cleared) and is a no-op when no turn
	// is active. The completion is completeOnce-guarded on the agent side, so
	// racing a genuine session.idle is safe. Delivery of the buffered text is
	// gated downstream (silent wakes route through a BufferSink), so this
	// closes the turn WITHOUT surfacing output. See foci bug #1051.
	b.failInFlightTurn("session closed")
	return nil
}

// WaitReady blocks until Start completes (readyCh closes), the context
// expires, or the Server dies (detected via server.done). Returning an
// error on early-Server-death lets callers retry without burning the
// full ready-timeout budget waiting for a readyCh that will never close.
func (b *Backend) WaitReady(ctx context.Context) error {
	var doneCh <-chan struct{}
	if b.server != nil {
		doneCh = b.server.done
	}
	select {
	case <-b.readyCh:
		return nil
	case <-doneCh:
		if b.server != nil {
			return fmt.Errorf("opencode: server died before Start completed: %s",
				describeExitError(b.server.exitErr))
		}
		return errors.New("opencode: server died before Start completed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CheckReady is the runtime readiness probe. Proxies to
// Server.healthCheck (GET /global/health). Returns (true, nil) when the
// server reports healthy; (false, err) on transport error or non-200
// response.
//
// Auth recovery is event-driven, not health-check-driven: ProviderAuthError
// surfaces via message.updated / session.error during turns, caught by
// handlers.go → authfail.go (onAuthFailure). /global/health does not expose
// per-provider auth state, so CheckReady cannot detect auth failure at
// startup. The (false, nil) "recovery initiated" return pattern is used by
// the ccstream backend (which can probe auth status at startup); the opencode
// backend recovers at first-turn time instead.
func (b *Backend) CheckReady(ctx context.Context) (bool, error) {
	if b.server == nil {
		return false, errors.New("opencode: backend has no server (Start not called)")
	}
	if err := b.server.healthCheck(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// createSession POSTs /session and returns the new session's ID.
func (b *Backend) createSession(ctx context.Context) (string, error) {
	body := []byte(`{}`) // no title — opencode assigns a default
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.server.baseURL+"/session", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("POST /session: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var session struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return "", fmt.Errorf("decode /session response: %w", err)
	}
	if session.ID == "" {
		return "", errors.New("POST /session returned empty id")
	}
	return session.ID, nil
}

// resumeSession probes whether session ID still exists on the opencode
// server (GET /session/:id). Returns (true, nil) if present and reusable;
// (false, nil) on 404 (evicted, db wiped, never existed); (false, err) on
// any other status or transport error. The distinction matters: a 404
// falls through to createSession within the same Start call, while a real
// error fails Start so DelegatedManager's retry-without-resume path runs —
// preventing a transient hiccup from silently discarding the resume ID.
func (b *Backend) resumeSession(ctx context.Context, id string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.server.baseURL+"/session/"+id, nil)
	if err != nil {
		return false, err
	}
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusOK:
		return true, nil
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	default:
		respBody, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("GET /session/%s: HTTP %d: %s", id, resp.StatusCode, string(respBody))
	}
}

// serverConfigFromOpts builds a serverConfig from StartOptions + the
// Backend's cfg map (populated from [opencode_backend] + per-agent
// backend_config by cmd/foci-gw/agents_delegated.go). Per-agent values
// override the defaults.
func (b *Backend) serverConfigFromOpts(opts delegator.StartOptions) serverConfig {
	cfg := defaultServerConfig(opts.WorkDir)
	if v, ok := b.cfg["binary"].(string); ok && v != "" {
		cfg.binaryPath = v
	}
	if v, ok := b.cfg["hostname"].(string); ok && v != "" {
		cfg.hostname = v
	}
	if v, ok := b.cfg["server_auth"].(string); ok {
		cfg.serverPassword = v
	}
	if v, ok := b.cfg["log_level"].(string); ok && v != "" {
		cfg.logLevel = v
	}
	// Port can be int or int64 depending on TOML unmarshalling.
	switch v := b.cfg["port"].(type) {
	case int:
		cfg.port = v
	case int64:
		cfg.port = int(v)
	case float64:
		cfg.port = int(v)
	}
	return cfg
}

// httpClient returns the Server's shared HTTP client if available,
// else a fresh one. Tests that bypass Server construction get an
// isolated client.
func (b *Backend) httpClient() *http.Client {
	if b.server != nil && b.server.http != nil {
		return b.server.http
	}
	return &http.Client{Timeout: httpTimeout}
}

// logComponent returns the log component string for this Backend.
// Matches Server.logComponent's shape so logs group naturally.
func (b *Backend) logComponent() string {
	if b.agentID != "" {
		return "opencode:" + b.agentID
	}
	return "opencode"
}

// ---------------------------------------------------------------------------
// Server.healthCheck — one-shot readiness probe
// ---------------------------------------------------------------------------

// healthCheck does a single GET /global/health and returns nil iff the
// server responds 200 with healthy=true. Distinguished from
// healthProbe (which is the polling loop in Start) — healthCheck is the
// one-shot variant Backend.CheckReady proxies to.
func (s *Server) healthCheck(ctx context.Context) error {
	if s.baseURL == "" {
		return errors.New("opencode: server has no baseURL (Start not called)")
	}
	url := s.baseURL + "/global/health"
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /global/health: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var health struct {
		Healthy bool   `json:"healthy"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		return fmt.Errorf("decode /global/health: %w", err)
	}
	if !health.Healthy {
		return errors.New("opencode: server reports unhealthy")
	}
	return nil
}
