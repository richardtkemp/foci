// server.go — Server struct definition. One Server exists per foci agent,
// shared across all of that agent's sessions. Owns the opencode serve
// subprocess, the HTTP client, the SSE subscriber goroutine, and the
// per-session Backend registry.

package opencode

import (
	"context"
	"net/http"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// Server owns the shared opencode-server subprocess plus its HTTP client
// and SSE subscriber. One per foci agent; refcounted via the package-level
// serverPool. Sessions within an agent share the Server; per-session
// dispatch happens via the sessions map keyed by opencode session ID.
type Server struct {
	// Config (immutable post-Start).
	agentID        string
	workDir        string
	binaryPath     string // "" = resolve "opencode" via $PATH
	hostname       string // default "127.0.0.1"
	port           int    // 0 = pick free port per Server
	serverPassword string // "" = no auth (loopback only)
	logLevel       string // "" = opencode default (INFO)

	// Process.
	cmd     *exec.Cmd
	baseURL string
	http    *http.Client
	cancel  context.CancelFunc // cancels SSE subscriber + keep-alive
	done    chan struct{}      // closed when subprocess exits
	waitCh  chan error         // receives cmd.Wait() result
	exitErr error              // set by waiter goroutine

	// subscribed is closed once the SSE subscriber's GET /event has
	// established (HTTP 200). Start waits on it before returning, because
	// health and /event become ready at DIFFERENT times and a prompt POSTed
	// in that gap loses its completion event (#1722).
	//
	// Created lazily by subscribedCh, NOT in newServer: a Server built as a
	// bare struct literal must work, because ~33 tests across this package do
	// exactly that and would otherwise close a nil channel.
	subscribedMu   sync.Mutex
	subscribed     chan struct{}
	subscribedOnce sync.Once

	// Lifecycle.
	mu           sync.Mutex
	refCount     int  // read/written by pool via acquireServer/releaseServer
	running      bool // set by Start/finalizeExit; read by isAlive (pool liveness check)
	closing      bool
	finalizeOnce sync.Once
	closeOnce    sync.Once

	// Close-ladder waits, per-Server so a test can shorten them on its own
	// Server rather than mutating a shared package global (#975). Defaults
	// from defaultClose*Wait, set in newServer.
	closeGracefulWait time.Duration
	closeSigtermWait  time.Duration
	closeSigkillWait  time.Duration

	// Per-session registry. Backends register under their opencode
	// sessionID; the SSE subscriber routes events by looking up here.
	// childToParent maps a subagent (child) session ID to its parent,
	// learned from session.created events. opencode never registers child
	// sessions as Backends, so route() uses this to walk a child's
	// permission requests up to the owning Backend (else they'd be dropped
	// and the subagent — and the parent turn — would block forever).
	// childToCallID extends this: child session ID → parent tool callID,
	// learned from the Task tool's part metadata (trackTaskTool), so child
	// text events can be grouped with their OnSubagentStart/End.
	// Both guarded by sessionsMu.
	sessionsMu    sync.RWMutex
	sessions      map[string]*Backend
	childToParent map[string]string
	childToCallID map[string]string

	// SSE subscriber cancel.
	subscriberCancel context.CancelFunc

	// Activity — updated on every inbound SSE frame.
	lastActivity atomic.Int64 // unix nanos

	// extraEnv carries optional environment variables (BASH_ENV,
	// FOCI_SOCK from the exec bridge) applied to the subprocess on
	// first launch. Set by acquireServer from the first Backend's
	// StartOptions.Env. Only the first session's env takes effect —
	// the subprocess is shared (v1 limitation).
	extraEnv map[string]string
	// effectiveEnv is the launch-time environment inherited by the shared
	// OpenCode process. It is retained so later session backends do not rebuild
	// approval state from a potentially changed gateway environment.
	effectiveEnv map[string]string
}

// isAlive reports whether the Server's subprocess is believed to be running.
// The pool consults this before handing back a pooled Server so a dead one is
// evicted + respawned instead of reused. Backed by the running flag, which
// finalizeExit clears on ANY death path (including subscriber-EOF, which can
// fire before cmd.Wait reaps the process) — broader than the done channel.
func (s *Server) isAlive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// newServer constructs a Server from cfg without starting it.
// acquireServer calls Start after registering the first Backend.
func newServer(agentID string, cfg serverConfig) *Server {
	s := &Server{
		agentID:           agentID,
		workDir:           cfg.workDir,
		binaryPath:        cfg.binaryPath,
		hostname:          cfg.hostname,
		port:              cfg.port,
		serverPassword:    cfg.serverPassword,
		logLevel:          cfg.logLevel,
		sessions:          make(map[string]*Backend),
		childToParent:     make(map[string]string),
		childToCallID:     make(map[string]string),
		http:              &http.Client{Timeout: 30 * time.Second},
		closeGracefulWait: defaultCloseGracefulWait,
		closeSigtermWait:  defaultCloseSigtermWait,
		closeSigkillWait:  defaultCloseSigkillWait,
	}
	s.wrapAuthCheckingTransport()
	return s
}

// subscribedCh returns the attach signal, creating it on first use so the
// Server zero value is usable.
func (s *Server) subscribedCh() chan struct{} {
	s.subscribedMu.Lock()
	defer s.subscribedMu.Unlock()
	if s.subscribed == nil {
		s.subscribed = make(chan struct{})
	}
	return s.subscribed
}

// markSubscribed reports that the SSE stream is established. Idempotent —
// the subscriber's connect loop is the only caller, but a mid-stream
// reconnect (deferred future work, see runSubscriber) would call it again.
func (s *Server) markSubscribed() {
	ch := s.subscribedCh()
	s.subscribedOnce.Do(func() { close(ch) })
}

// waitForSubscriber blocks until the SSE stream is established, the
// subprocess dies, ctx expires, or the bound elapses. It reports whether the
// stream attached.
//
// Start calls this after the health probe because the two readiness signals
// are NOT the same event: GET /global/health returned 200 a full 8 seconds
// before GET /event did on 2026-08-16, and the prompt POSTed in between ran
// to completion with nobody listening, wedging the session worker forever
// (#1722). Waiting closes that window at the only point that covers every
// caller — no prompt of any kind is sent before Start returns.
func (s *Server) waitForSubscriber(ctx context.Context, bound time.Duration) bool {
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-s.subscribedCh():
		return true
	case <-s.done:
		return false
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

// serverConfig is the resolved configuration used to construct a Server.
// Built from [opencode_backend] config + per-agent overrides in
// serverConfigFromOpts (backend_lifecycle.go).
type serverConfig struct {
	workDir        string
	binaryPath     string
	hostname       string
	port           int
	serverPassword string
	logLevel       string
}

// defaultServerConfig returns a Server config with the documented defaults.
// Overridden per [opencode_backend] / per-agent backend_config in
// serverConfigFromOpts.
func defaultServerConfig(workDir string) serverConfig {
	return serverConfig{
		workDir:        workDir,
		binaryPath:     "",          // $PATH lookup
		hostname:       "127.0.0.1", // loopback only
		port:           0,           // pick free
		serverPassword: "",          // no auth on loopback
		logLevel:       "",          // opencode default (INFO)
	}
}
