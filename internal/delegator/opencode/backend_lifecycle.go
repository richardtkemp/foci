// backend_lifecycle.go — Backend.Start / Close / WaitReady / CheckReady.
// Replaces the Step 1.4 panic stubs in opencode.go with real HTTP-driven
// implementations. Server acquisition goes through acquireServer (the
// per-agent pool from Step 3); HTTP calls hit server.baseURL; the
// dispatcher goroutine + per-session registry are wired via
// server.registerSession (Step 4).
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

	log.Infof(b.logComponent(), "Start: agentID=%s workDir=%s", opts.AgentID, opts.WorkDir)

	// keep newRequestID reachable for deadcode (Step 6 wires real callers).
	_ = newRequestID()

	b.agentID = opts.AgentID
	b.startOpts = opts

	if b.server == nil {
		srv, err := acquireServer(opts.AgentID, b.serverConfigFromOpts(opts), opts.Env)
		if err != nil {
			return fmt.Errorf("opencode: acquire server: %w", err)
		}
		b.server = srv
	}

	// POST /session — body is `{title?: string}`. opencode returns the
	// newly-created Session with its server-assigned ID.
	sessionID, err := b.createSession(ctx)
	if err != nil {
		return fmt.Errorf("opencode: create session: %w", err)
	}
	b.sessionID = sessionID
	log.Infof(b.logComponent(), "Start: session created id=%s", sessionID)

	// Register with the Server so SSE events route to us. Side effect:
	// launches the dispatcher goroutine (Step 4) which drains b.events
	// and invokes the per-Backend handler (Step 7 sets a real handler;
	// defaultDispatchHandler logs at DEBUG until then).
	//
	// SetDispatchHandler MUST be called before registerSession — the
	// handler is captured at goroutine-start time.
	b.SetDispatchHandler(b.handleEvent)
	b.server.registerSession(b)
	log.Debugf(b.logComponent(), "Start: registered with server, dispatcher started")

	// Apply default permission mode if configured. opencode's defaults
	// are permissive (most tools "allow"); foci wants "ask" for side-
	// effecting tools so the user gets prompted via the permission
	// keyboard. Per-agent backend_config.default_permission overrides.
	if dp, ok := b.cfg["default_permission"].(string); ok && dp != "" {
		if err := b.patchConfig(ctx, map[string]any{
			"permission": map[string]string{"*": dp},
		}); err != nil {
			log.Warnf(b.logComponent(), "default_permission PATCH failed: %v — using opencode defaults", err)
		}
	}

	// Inject system prompt if provided. noReply:true so opencode treats
	// it as context-only and doesn't trigger an AI response — mirrors
	// ccstream's --append-system-prompt flag. Best-effort: a failure
	// here logs but doesn't fail Start, since the session is usable
	// without the prompt (subsequent user turns work fine).
	if prompt := resolveSystemPrompt(opts); prompt != "" {
		if err := b.injectSystemPrompt(ctx, prompt); err != nil {
			log.Warnf(b.logComponent(), "system prompt injection failed: %v", err)
		}
	}

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
// after any in-flight handler invocation completes), DELETEs the opencode
// session (best-effort), and releases the per-agent Server reference
// (which triggers Server shutdown if this was the last session).
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

	// Deregister from Server — stops the dispatcher and waits for any
	// in-flight handler call to complete (Step 4 contract). Safe to
	// call when b.server is nil (test Backend that bypassed Start).
	if b.server != nil {
		b.server.unregisterSession(b.sessionID)
	}

	// Best-effort DELETE — opencode cleans up session state. Ignore
	// errors: a failed DELETE doesn't leak anything significant (the
	// session becomes idle on the server side and is reaped by the
	// server's own idle timeout).
	if b.server != nil && b.server.baseURL != "" && b.sessionID != "" {
		url := fmt.Sprintf("%s/session/%s", b.server.baseURL, b.sessionID)
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, url, nil)
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}

	// Release the per-agent Server reference. No-op if b.server wasn't
	// acquired via acquireServer (test Backends that set b.server
	// directly — agentID isn't in the pool so releaseServer returns
	// immediately). Production Backends always go through acquireServer.
	if b.agentID != "" {
		releaseServer(b.agentID, b.server)
	}

	b.cancelTurn()
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
// Plan §5.1 calls out a (false, nil) "recovery initiated" return for
// ProviderAuthError. That branch lands in Step 11 once the Server
// tracks auth-failure state; for Step 5 the auth case is reported as a
// generic err since /global/health itself doesn't surface per-provider
// auth state.
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

// injectSystemPrompt POSTs the prompt as a noReply user message so
// opencode treats it as context-only and doesn't trigger an AI response.
// Mirrors ccstream's --append-system-prompt behaviour.
func (b *Backend) injectSystemPrompt(ctx context.Context, prompt string) error {
	body, err := json.Marshal(map[string]any{
		"noReply": true,
		"parts": []map[string]string{
			{"type": "text", "text": prompt},
		},
	})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/session/%s/message", b.server.baseURL, b.sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST /session/%s/message: HTTP %d: %s", b.sessionID, resp.StatusCode, string(respBody))
	}
	return nil
}

// resolveSystemPrompt returns opts.SystemPromptFunc() if non-nil and
// non-empty, else opts.SystemPrompt. Empty → caller skips injection.
func resolveSystemPrompt(opts delegator.StartOptions) string {
	if opts.SystemPromptFunc != nil {
		if s := opts.SystemPromptFunc(); s != "" {
			return s
		}
	}
	return opts.SystemPrompt
}

// serverConfigFromOpts builds a serverConfig from StartOptions + the
// Backend's cfg map (populated from [opencode_backend] + per-agent
// backend_config by cmd/foci-gw/agents_delegated.go). Per-agent values
// override the defaults.
func (b *Backend) serverConfigFromOpts(opts delegator.StartOptions) serverConfig {
	cfg := defaultServerConfig(opts.WorkDir)
	if v, ok := b.cfg["opencode_binary"].(string); ok && v != "" {
		cfg.binaryPath = v
	}
	if v, ok := b.cfg["hostname"].(string); ok && v != "" {
		cfg.hostname = v
	}
	if v, ok := b.cfg["server_auth"].(string); ok {
		cfg.serverPassword = v
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
//
// Step 11 will extend this to inspect a server-side auth-failure state
// and return a typed error the Backend.CheckReady wrapper can map to
// the plan's (false, nil) "recovery initiated" return.
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
