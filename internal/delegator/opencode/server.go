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
	// so child text events can be grouped with their OnSubagentStart/End.
	// pendingTaskCalls tracks in-flight task tool calls per parent session
	// (FIFO), consumed when a child session is created. All guarded by
	// sessionsMu.
	sessionsMu         sync.RWMutex
	sessions           map[string]*Backend
	childToParent      map[string]string
	childToCallID      map[string]string
	pendingTaskCalls   map[string][]string // parentSessionID → FIFO of callIDs

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
		agentID:        agentID,
		workDir:        cfg.workDir,
		binaryPath:     cfg.binaryPath,
		hostname:       cfg.hostname,
		port:           cfg.port,
		serverPassword: cfg.serverPassword,
		logLevel:       cfg.logLevel,
		sessions:          make(map[string]*Backend),
		childToParent:     make(map[string]string),
		childToCallID:     make(map[string]string),
		pendingTaskCalls:  make(map[string][]string),
		http:              &http.Client{Timeout: 30 * time.Second},
		closeGracefulWait: defaultCloseGracefulWait,
		closeSigtermWait:  defaultCloseSigtermWait,
		closeSigkillWait:  defaultCloseSigkillWait,
	}
	s.wrapAuthCheckingTransport()
	return s
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
