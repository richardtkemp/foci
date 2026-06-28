// Package opencode implements a delegated backend using the OpenCode server
// (https://opencode.ai/docs/server). One opencode server process is shared
// across all of a foci agent's sessions; this package is its HTTP/SSE client.
//
// This is a Step 1.4 TDD-red-bar stub: the surface compiles and the kept
// tests fail because the methods panic or return zero values. Real
// implementations land in Steps 2–14 of OPENCODE_DELEGATOR_PLAN.md.
package opencode

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"foci/internal/delegator"
)

// serverPool is the package-level registry of live Servers, keyed by
// foci agent ID. One Server per agent; refcounted so the subprocess is
// shut down when the last session closes. Step 3 introduces a real
// bounded-shutdown Close; for Step 2 the pool just constructs Servers
// without starting them.
var (
	serverPoolMu sync.Mutex
	serverPool   = map[string]*Server{}
)

// acquireServer returns the live Server for agentID, constructing and
// starting one if none exists yet. Step 5's Backend.Start calls this.
//
// Start runs OUTSIDE the pool mutex — slow subprocess startup must not
// block acquireServer for unrelated agents. DelegatedManager serialises
// Backend.Start per-agent in production so the new-server race window
// (two concurrent acquires for a brand-new agent both reaching the
// Start call) does not occur in practice; if that changes, add a
// per-agent sync.Once.
func acquireServer(agentID string, cfg serverConfig) (*Server, error) {
	serverPoolMu.Lock()
	if s, ok := serverPool[agentID]; ok {
		s.refCount++
		serverPoolMu.Unlock()
		return s, nil
	}
	serverPoolMu.Unlock()

	s := newServer(agentID, cfg)
	if err := s.Start(context.Background()); err != nil {
		return nil, err
	}
	serverPoolMu.Lock()
	// Defensive: if a concurrent acquire for the same agent raced ahead
	// and inserted a Server while we were starting this one, prefer the
	// existing one and close ours. DelegatedManager serialises per-agent
	// so this is unreachable in production; the check is cheap insurance.
	if existing, ok := serverPool[agentID]; ok {
		serverPoolMu.Unlock()
		go func() { _ = s.Close() }() // bounded shutdown, doesn't block the caller
		existing.refCount++
		return existing, nil
	}
	serverPool[agentID] = s
	s.refCount = 1
	serverPoolMu.Unlock()
	return s, nil
}

// releaseServer decrements the refcount for agentID's Server. When the
// refcount hits zero, the Server is removed from the pool and Close is
// called in a goroutine. Pool mutex is released before Close so a slow
// shutdown doesn't block unrelated agents — matches the plan's
// concurrency note (§3.2).
func releaseServer(agentID string) {
	serverPoolMu.Lock()
	s, ok := serverPool[agentID]
	if !ok {
		serverPoolMu.Unlock()
		return
	}
	s.refCount--
	if s.refCount > 0 {
		serverPoolMu.Unlock()
		return
	}
	delete(serverPool, agentID)
	serverPoolMu.Unlock()
	// Close outside the mutex — bounded shutdown so worst-case ~9s, but
	// we don't want to hold the pool lock for any of that.
	go func() { _ = s.Close() }()
}

// init registers the constructor with the delegator registry. Real
// registration (with plan delivery etc.) lands in Step 14; for now this
// keeps the package importable in tests without side effects.
func init() {
	delegator.Register("opencode", newFromConfig, false)
}

// newFromConfig is the constructor delegator.New("opencode", cfg) calls.
// Step 1.4 stub: returns a Backend with initialised channels/maps so KEPT
// tests can construct one. Real session/server wiring lands in Step 5.
func newFromConfig(cfg map[string]any) (delegator.Delegator, error) {
	b := &Backend{
		cfg:           cfg,
		readyCh:       make(chan struct{}),
		pendingPerms:  make(map[string]*pendingPermission),
		outstanding:   NewOutstandingRegistry(),
		compactDoneCh: make(chan struct{}, 1),
	}
	return b, nil
}

// Backend implements delegator.Delegator using the OpenCode HTTP server.
// One Backend exists per foci session; all Backends for a given agent
// share a Server (Step 3 introduces the Server layer).
//
// Step 1.4 stub: fields are present so KEPT tests can construct and poke
// a Backend directly; methods panic or return zero values. Real behaviour
// lands in later steps. Fields without an immediate user are deferred to
// their respective plan steps (workDir/agentID/label/model/etc. → Step 5).
type Backend struct {
	cfg           map[string]any
	agentID       string            // acquired Server is keyed by this
	server        *Server           // shared with sibling Backends on this agent
	startOpts     delegator.StartOptions // saved at Start for restart/inspection
	readyCh       chan struct{}     // closed when POST /session returns
	pendingPerms  map[string]*pendingPermission
	outstanding   *OutstandingRegistry
	compactDoneCh chan struct{}     // buffered(1); closed by OnSessionCompacted
	sessionID     string

	// Per-session event channel — Server.route pushes decoded rawEvents
	// here; Backend.dispatchLoop drains and invokes handlers (Step 7).
	// nil until Backend registers with its Server (Step 5); buffered
	// eventBufferSize so a transient dispatcher stall doesn't drop.
	events chan rawEvent

	// Dispatcher — one goroutine drains events serially. Started by
	// Server.registerSession, stopped by Server.unregisterSession. The
	// dispatchHandler is captured at goroutine start; Step 7 calls
	// SetDispatchHandler before registerSession to bind the real
	// per-Event-Type dispatch.
	dispatchHandler eventHandler
	stopDispatcher  func()
	dispatchWg      sync.WaitGroup

	// Lifecycle — Step 5.
	mu      sync.Mutex
	running bool

	// Session-scoped delivery — Step 6/7.
	sessionEvents atomic.Pointer[delegator.SessionEvents]

	// Turn bookkeeping — mirrors ccstream's invariants; Step 6/7.
	turnMu       sync.Mutex
	turnActive   bool
	turnEvents   *delegator.TurnEvents
	turnResultCh chan *ResultMessage
	turnText     strings.Builder
	turnTools    int
	lastUsage    *TokenUsage

	// Steer buffer (plan §6 divergence). opencode has no mid-turn
	// queue, so SourceUser / SourceSteer arriving during an in-flight
	// turn are buffered here and flushed by flushSteerBuf when the
	// dispatcher's OnSessionIdle fires. Guarded by turnMu.
	steerBuf []string

	// Callbacks — set before Start, read-only after. Each is referenced
	// by the matching Set* method below; that's enough production-code
	// use for the unused linter (which excludes tests) to be satisfied.
	permPromptFn      delegator.PermissionPromptFunc
	onSessionReady    func(sessionID string)
	typingFunc        func(typing bool)
	onCompactionStart func()
	onCompactionDone  func(preTokens int)

	// Agent spawn tracking — shared with ccstream via delegator.AgentTracker.
	agents delegator.AgentTracker
}

// IsRunning reports whether the OpenCode subprocess is alive. Step 1.4
// stub: reads b.running under b.mu.
func (b *Backend) IsRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// SessionID returns the OpenCode session identifier. Step 1.4 stub.
func (b *Backend) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}

// SessionFilePath returns empty — the OpenCode backend identifies a
// session by ID, not a file path. Callers should use SessionID().
func (b *Backend) SessionFilePath() string {
	return ""
}

// SendKeystroke returns "not supported" — OpenCode has no TUI pane to
// send literal keypresses to.
func (b *Backend) SendKeystroke(_ context.Context, _ string) error {
	return errors.New("opencode: SendKeystroke not supported")
}

// SendSpecialKey returns "not supported" — same reason as SendKeystroke.
func (b *Backend) SendSpecialKey(_ context.Context, _ string) error {
	return errors.New("opencode: SendSpecialKey not supported")
}

// SetPermissionPromptFunc stores the permission-prompt callback. Step 9
// wires the real surfacing path.
func (b *Backend) SetPermissionPromptFunc(fn delegator.PermissionPromptFunc) {
	b.permPromptFn = fn
}

// SetOnPromptsCleared wires the OutstandingRegistry's onEmpty drain hook
// — DelegatedManager.WaitForPermission blocks on this.
func (b *Backend) SetOnPromptsCleared(fn func()) {
	b.outstanding.SetOnEmpty(fn)
}

// SetOnSessionReady stores the callback fired once when the backend
// discovers its session ID. Step 5 invokes it after POST /session.
func (b *Backend) SetOnSessionReady(fn func(sessionID string)) {
	b.onSessionReady = fn
}

// SetTypingFunc stores the typing-indicator callback.
func (b *Backend) SetTypingFunc(fn func(typing bool)) {
	b.typingFunc = fn
}

// SetOnCompactionStart stores the callback fired when CC signals
// compaction is underway. OpenCode has no "compacting started" event,
// so Step 8.2 synthesises one.
func (b *Backend) SetOnCompactionStart(fn func()) {
	b.onCompactionStart = fn
}

// SetOnCompactionDone stores the callback fired on session.compacted.
func (b *Backend) SetOnCompactionDone(fn func(preTokens int)) {
	b.onCompactionDone = fn
}

// SetOnAgentStatus stores the callback on the shared AgentTracker.
func (b *Backend) SetOnAgentStatus(fn func(text string)) {
	b.agents.OnStatus = fn
}

// Turn-lifecycle methods (AttachSessionEvents, beginTurn, cancelTurn,
// IsTurnInFlight, WaitForTurn, ArmCompactionWait, WaitForCompaction)
// live in inject.go per plan §6.2.



// newRequestID generates a simple unique request ID for control messages.
// Not a real UUID, but unique within a process lifetime which is
// sufficient for request correlation. Ported from ccstream/ccstream.go.
//
// Wired through backend_lifecycle.go's Start (called once to keep deadcode
// aware of the call graph); Step 6 uses it for control-message correlation.
func newRequestID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// describeExitError returns a human-readable description of a process
// exit error including exit code, signal, and stderr snippet when
// available. Ported from ccstream/lifecycle.go — backend-agnostic helper.
//
// Wired through backend_lifecycle.go's WaitReady (called when the Server
// dies before readyCh closes); also called from lifecycle.go's
// Server.Start waiter goroutine + finalizeExit.
func describeExitError(err error) string {
	if err == nil {
		return "exit status 0"
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return err.Error()
	}

	ps := exitErr.ProcessState
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok {
		return fmt.Sprintf("exit code %d", exitErr.ExitCode())
	}

	var parts []string
	if ws.Exited() {
		parts = append(parts, fmt.Sprintf("exit code %d", ws.ExitStatus()))
	}
	if ws.Signaled() {
		parts = append(parts, fmt.Sprintf("signal %s", ws.Signal()))
		if ws.CoreDump() {
			parts = append(parts, "core dumped")
		}
	}
	if len(exitErr.Stderr) > 0 {
		snippet := string(exitErr.Stderr)
		if len(snippet) > 512 {
			snippet = snippet[:512] + "…"
		}
		parts = append(parts, "stderr: "+snippet)
	}

	if len(parts) == 0 {
		return err.Error()
	}
	return strings.Join(parts, ", ")
}

// Inject routes user-role events to the backend. Implementation lives
// in inject.go (Step 6).

// RegisterPromptCancelListener registers a per-prompt cancel callback.
// Step 9 wires the real permission-handling path.
func (b *Backend) RegisterPromptCancelListener(_ string, _ func(reason string)) {
	panic("opencode: Backend.RegisterPromptCancelListener not implemented — Step 9")
}

// Interrupt aborts any in-flight turn. Step 8 wires POST /session/:id/abort.
func (b *Backend) Interrupt(_ context.Context) error {
	panic("opencode: Backend.Interrupt not implemented — Step 8")
}
