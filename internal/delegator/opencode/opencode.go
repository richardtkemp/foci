// Package opencode implements a delegated backend using the OpenCode server
// (https://opencode.ai/docs/server). One opencode server process is shared
// across all of a foci agent's sessions; this package is its HTTP/SSE client.
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
	"foci/internal/delegator/autoapprove"
	"foci/internal/ratelimit"
)

// serverPool is the package-level registry of live Servers, keyed by
// foci agent ID. One Server per agent; refcounted so the subprocess is
// shut down when the last session closes.
var (
	serverPoolMu sync.Mutex
	serverPool   = map[string]*Server{}
)

// acquireServer returns the live Server for agentID, constructing and
// starting one if none exists yet. env carries optional environment
// variables (BASH_ENV, FOCI_SOCK from the exec bridge) that are applied
// to the subprocess on first launch. Only the first session's env vars
// take effect — the subprocess is shared across all sessions on this
// agent (documented v1 limitation).
func acquireServer(agentID string, cfg serverConfig, env map[string]string) (*Server, error) {
	serverPoolMu.Lock()
	if s, ok := serverPool[agentID]; ok {
		if s.isAlive() {
			s.refCount++
			serverPoolMu.Unlock()
			return s, nil
		}
		// Dead pooled entry (the subprocess died but the Server was never
		// evicted — the bug this fixes). Evict it and fall through to spawn a
		// fresh one rather than handing back a Server whose port no longer
		// answers ("connection refused" forever, with no respawn).
		delete(serverPool, agentID)
	}
	serverPoolMu.Unlock()

	s := newServer(agentID, cfg)
	s.extraEnv = env
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

// releaseServer decrements the refcount for the caller's Server s. When the
// refcount hits zero, the Server is removed from the pool and Close is
// called in a goroutine. Pool mutex is released before Close so a slow
// shutdown doesn't block unrelated agents — matches the plan's
// concurrency note (§3.2).
//
// releaseServer is POINTER-AWARE: it only acts when the pooled entry for
// agentID is still s. If s was already evicted (it died and was removed by
// finalizeExit/acquireServer) and a fresh Server took its place, a stale
// session releasing its old s must NOT decrement or Close the replacement —
// doing so would corrupt the live Server's refcount and could close a server
// other sessions are still using.
func releaseServer(agentID string, s *Server) {
	serverPoolMu.Lock()
	cur, ok := serverPool[agentID]
	if !ok || cur != s {
		// Our Server was already evicted/replaced. Leave the current pooled
		// Server's refcount untouched.
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
	// Close outside the mutex — bounded shutdown so worst-case ~4.5s, but
	// we don't want to hold the pool lock for any of that.
	go func() { _ = s.Close() }()
}

// CloseAllServers SYNCHRONOUSLY shuts down every pooled opencode server,
// ignoring refcounts, and returns the number closed. It is the load-bearing
// backstop for foci's shutdown path (#948): the per-session releaseServer Close
// is async (`go func`), so on a foci restart/shutdown the process exits before
// those detached goroutines finish the SIGTERM→SIGKILL ladder, orphaning the
// `opencode serve` subprocess (observed: 10h-old, ppid=1, ~130MB, one leaked per
// deploy per opencode agent). Draining the pool here — and WAITING for each
// bounded Close to complete before main returns — guarantees no server survives
// the restart, regardless of whether every Backend.Close ran or the refcount
// reached zero. Servers are closed in parallel so total wall-clock is bounded by
// the slowest single Close, not their sum.
func CloseAllServers() int {
	serverPoolMu.Lock()
	servers := make([]*Server, 0, len(serverPool))
	for id, s := range serverPool {
		servers = append(servers, s)
		delete(serverPool, id)
	}
	serverPoolMu.Unlock()

	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func(s *Server) { defer wg.Done(); _ = s.Close() }(s)
	}
	wg.Wait()
	return len(servers)
}

// init registers the constructor with the delegator registry.
func init() {
	delegator.Register("opencode", newFromConfig, true)
	delegator.RegisterPlan("opencode", planDelivery)
}

// newFromConfig is the constructor delegator.New("opencode", cfg) calls.
// Returns a Backend with initialised channels/maps.
func newFromConfig(cfg map[string]any) (delegator.Delegator, error) {
	b := &Backend{
		cfg:            cfg,
		readyCh:        make(chan struct{}),
		pendingPerms:   make(map[string]*pendingPermission),
		outstanding:    delegator.NewOutstandingRegistry(),
		compactDoneCh:  make(chan struct{}, 1),
		resolveModelFn: resolveModel,
	}
	return b, nil
}

// Backend implements delegator.Delegator using the OpenCode HTTP server.
// One Backend exists per foci session; all Backends for a given agent
// share a Server.
type Backend struct {
	cfg            map[string]any
	agentID        string                                                                       // acquired Server is keyed by this
	server         *Server                                                                      // shared with sibling Backends on this agent
	startOpts      delegator.StartOptions                                                       // saved at Start for restart/inspection
	systemPrompt   string                                                                       // resolved system prompt, supplied via POST body "system" field
	model          string                                                                       // configured model, supplied via POST body "model" field ("" = opencode config default)
	resolveModelFn func(ctx context.Context, binaryPath, workDir, model string) (string, error) // defaults to resolveModel; overridable in tests
	readyCh        chan struct{}                                                                // closed when POST /session returns
	pendingPerms   map[string]*pendingPermission
	permMu         sync.Mutex
	outstanding    *delegator.OutstandingRegistry
	compactDoneCh  chan struct{} // buffered(1); closed by OnSessionCompacted
	compactStartCh chan struct{} // buffered(1); closed by handleCompactionPart (compaction-part = start signal)
	sessionID      string

	// Per-session event channel — Server.route pushes decoded rawEvents
	// here; Backend.dispatchLoop drains and invokes handlers.
	// nil until Backend registers with its Server; buffered
	// eventBufferSize so a transient dispatcher stall doesn't drop.
	events chan rawEvent

	// Dispatcher — one goroutine drains events serially. Started by
	// Server.registerSession, stopped by Server.unregisterSession. The
	// dispatchHandler is captured at goroutine start; Start calls
	// SetDispatchHandler before registerSession to bind the real
	// per-Event-Type dispatch.
	dispatchHandler eventHandler
	stopDispatcher  func()
	dispatchWg      sync.WaitGroup

	// Lifecycle.
	mu      sync.Mutex
	running bool

	// Session-scoped delivery.
	sessionEvents atomic.Pointer[delegator.SessionEvents]

	// Turn bookkeeping — mirrors ccstream's invariants.
	turnMu        sync.Mutex
	turnActive    bool
	turnEvents    *delegator.TurnEvents
	turnResultCh  chan *ResultMessage
	turnText      strings.Builder
	turnTools     int
	lastModel     string
	lastProvider  string // paired with lastModel; required by /summarize compaction
	lastUsage     *TokenUsage
	ctxLimitCache int               // cached context window from /config/providers (GetContextWindow)
	ctxLimitModel string            // model the cached limit belongs to; re-query on model change
	seenToolCalls map[string]bool   // reset in beginTurn; dedupes OnToolStart
	seenTextParts map[string]bool   // reset in beginTurn; dedupes OnText
	partTypes     map[string]string // reset in beginTurn; partID→Type for message.part.delta routing

	// Steer buffer (plan §6 divergence). opencode has no mid-turn
	// queue, so SourceUser / SourceSteer arriving during an in-flight
	// turn are buffered here and flushed by flushSteerBuf when the
	// dispatcher's OnSessionIdle fires. Guarded by turnMu.
	steerBuf []string

	// Abort-drain state for a mid-turn SourceSteer. Empirically (opencode
	// 1.17.11): opencode queues a mid-turn prompt_async behind the active
	// turn, and POST /abort discards that queue, so a steer must ABORT the
	// active turn, drain the abort's event burst (session.error +
	// 2× session.idle), then send the buffered steer as a fresh turn. A
	// turn sent before/during the abort is lost; a turn sent after survives.
	// aborting gates the drain; abortIdlesSeen counts burst idles;
	// abortTimer is the backstop; abortDrainTimeout is the backstop delay
	// (default 500ms, overridable for tests). Guarded by turnMu.
	aborting          bool
	abortIdlesSeen    int
	abortTimer        *time.Timer
	abortDrainTimeout time.Duration

	// Callbacks — set before Start, read-only after. Each is referenced
	// by the matching Set* method below; that's enough production-code
	// use for the unused linter (which excludes tests) to be satisfied.
	permPromptFn      delegator.PermissionPromptFunc
	onSessionReady    func(sessionID string)
	typingFunc        func(typing bool)
	onCompactionStart func()
	onCompactionDone  func(preTokens int)
	onAuthFailure     func(detail string)
	onRateLimited     func(signal ratelimit.Signal)

	// Auto-approve rules — compiled from StartOptions.AutoApproveRules.
	// When non-empty, incoming permission.asked events are checked against
	// these rules before surfacing to the user. Matched permissions are
	// auto-approved via sendPermissionReply without prompting.
	autoApproveRules []autoapprove.Rule
	autoApproveEnv   map[string]string // exact environment inherited by OpenCode
	workDir          string            // workspace directory for path-canonicalization

	// authFailureFired gates onAuthFailure so a flaky 401 loop doesn't
	// spam repeated notifications. CAS'd to true on first fire; resets
	// when the Backend is recreated (session restart).
	authFailureFired atomic.Bool

	// Subagent spawn tracking — shared with ccstream via delegator.SubagentTracker.
	agents delegator.SubagentTracker
}

// IsRunning reports whether the OpenCode subprocess is alive. It gates on the
// shared Server's liveness, not just b.running: when the subprocess dies
// unexpectedly, finalizeExit clears the Server's running flag but cannot reach
// into every registered Backend's b.running. Without the server check IsRunning
// would stay true, DelegatedManager.getOrCreate would hand back this Backend,
// and every subsequent turn would dial the dead port forever (connection
// refused) instead of respawning a fresh server + resuming the session.
func (b *Backend) IsRunning() bool {
	b.mu.Lock()
	running := b.running
	srv := b.server
	b.mu.Unlock()
	return running && srv != nil && srv.isAlive()
}

// SessionID returns the OpenCode session identifier.
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

// SetPermissionPromptFunc stores the permission-prompt callback.
// permissions.go surfaces pending permissions through it.
func (b *Backend) SetPermissionPromptFunc(fn delegator.PermissionPromptFunc) {
	b.permPromptFn = fn
}

// SetOnPromptsCleared wires the delegator.OutstandingRegistry's onEmpty drain hook
// — DelegatedManager.WaitForPermission blocks on this.
func (b *Backend) SetOnPromptsCleared(fn func()) {
	b.outstanding.SetOnEmpty(fn)
}

// SetOnSessionReady stores the callback fired once when the backend
// discovers its session ID (after POST /session).
func (b *Backend) SetOnSessionReady(fn func(sessionID string)) {
	b.onSessionReady = fn
}

// SetTypingFunc stores the typing-indicator callback.
func (b *Backend) SetTypingFunc(fn func(typing bool)) {
	b.typingFunc = fn
}

// SetOnCompactionStart stores the callback fired when CC signals
// compaction is underway. OpenCode has no "compacting started" event,
// so compaction.go synthesises one (documented divergence, plan §8.2).
func (b *Backend) SetOnCompactionStart(fn func()) {
	b.onCompactionStart = fn
}

// SetOnCompactionDone stores the callback fired on session.compacted.
func (b *Backend) SetOnCompactionDone(fn func(preTokens int)) {
	b.onCompactionDone = fn
}

// SetOnSubagentStatus stores the callback on the shared SubagentTracker. The
// callback receives the running-subagent detail string (or "" when none).
func (b *Backend) SetOnSubagentStatus(fn func(detail string)) {
	b.agents.OnStatus = fn
}

// SetOnAuthFailure stores the auth-failure callback. Fired by handlers.go
// when a ProviderAuthError surfaces via message.updated or session.error
// SSE events. authfail.go provides the Server-level fanout + relogin gate.
func (b *Backend) SetOnAuthFailure(fn func(detail string)) {
	b.onAuthFailure = fn
}

// SetOnRateLimited stores the callback fired when OpenCode reports a rejected
// usage/rate limit through session.status. The callback receives a neutral
// signal so the agent's shared policy can resolve and engage the gate.
func (b *Backend) SetOnRateLimited(fn func(signal ratelimit.Signal)) {
	b.onRateLimited = fn
}

// Turn-lifecycle methods (AttachSessionEvents, beginTurn, cancelTurn,
// IsTurnInFlight, WaitForTurn, ArmCompactionWait, WaitForCompaction)
// live in inject.go per plan §6.2.

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
// in inject.go.

// RegisterPromptCancelListener registers a per-prompt cancel callback
// via the OutstandingRegistry.
func (b *Backend) RegisterPromptCancelListener(requestID string, fn func(reason string)) {
	b.outstanding.AddCancelListener(requestID, fn)
}

// Interrupt is implemented in control.go (POST /session/:id/abort).
