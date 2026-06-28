// server.go — Server struct definition. One Server exists per foci agent,
// shared across all of that agent's sessions. Owns the opencode serve
// subprocess, the HTTP client, the SSE subscriber goroutine, and the
// per-session Backend registry.
//
// Step 2 stub: struct fields only. Methods (Start, Close, OnSubscriberStopped,
// finalizeExit, route) land in Step 3 (lifecycle) and Step 4 (event routing).
// The pool helpers acquireServer/releaseServer in opencode.go construct
// Servers but don't start them yet.
//
// The //nolint:unused directives on the fields below silence golangci-lint's
// unused-field check (the project's lint config runs with tests:false, so
// test-only usage doesn't count). Each directive points at the plan step
// that wires a production caller; Step 3+ removes them as methods land.

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

	// Process (Step 3).
	cmd     *exec.Cmd          //nolint:unused // Step 3 lifecycle
	baseURL string             //nolint:unused // Step 3 lifecycle
	http    *http.Client       //nolint:unused // Step 3 lifecycle
	cancel  context.CancelFunc //nolint:unused // Step 3 lifecycle (cancels SSE subscriber + keep-alive)
	done    chan struct{}      //nolint:unused // Step 3 lifecycle (closed when subprocess exits)
	waitCh  chan error         //nolint:unused // Step 3 lifecycle (receives cmd.Wait() result)
	exitErr error              //nolint:unused // Step 3 lifecycle (set by waiter goroutine)

	// Lifecycle (Step 3).
	mu           sync.Mutex //nolint:unused // Step 3 lifecycle
	refCount     int        //nolint:unused // Step 3 lifecycle (read by pool via acquireServer/releaseServer)
	running      bool       //nolint:unused // Step 3 lifecycle
	closing      bool       //nolint:unused // Step 3 lifecycle
	finalizeOnce sync.Once  //nolint:unused // Step 3 lifecycle
	closeOnce    sync.Once  //nolint:unused // Step 3 lifecycle

	// Per-session registry (Step 4). Backends register under their opencode
	// sessionID; the SSE subscriber routes events by looking up here.
	sessionsMu sync.RWMutex        // Step 4 per-session routing
	sessions   map[string]*Backend // Step 4 per-session routing

	// SSE subscriber cancel (Step 4).
	subscriberCancel context.CancelFunc // Step 4 SSE subscriber

	// Activity — updated on every inbound SSE frame (Step 12).
	lastActivity atomic.Int64 //nolint:unused // Step 12 activity tracker (unix nanos)
}

// newServer constructs a Server from cfg without starting it. Step 3's
// acquireServer calls Start after registering the first Backend.
func newServer(agentID string, cfg serverConfig) *Server {
	s := &Server{
		agentID:        agentID,
		workDir:        cfg.workDir,
		binaryPath:     cfg.binaryPath,
		hostname:       cfg.hostname,
		port:           cfg.port,
		serverPassword: cfg.serverPassword,
		sessions:       make(map[string]*Backend),
		http:           &http.Client{Timeout: 30 * time.Second},
	}
	s.wrapAuthCheckingTransport()
	return s
}

// serverConfig is the resolved configuration used to construct a Server.
// Built from [opencode_backend] config + per-agent overrides in Step 14;
// Step 2 stub just carries the fields.
type serverConfig struct {
	workDir        string
	binaryPath     string
	hostname       string
	port           int
	serverPassword string
}

// defaultServerConfig returns a Server config with the documented defaults.
// Step 14 overrides per [opencode_backend] / per-agent backend_config.
func defaultServerConfig(workDir string) serverConfig {
	return serverConfig{
		workDir:        workDir,
		binaryPath:     "",            // $PATH lookup
		hostname:       "127.0.0.1",   // loopback only
		port:           0,             // pick free
		serverPassword: "",            // no auth on loopback
	}
}
