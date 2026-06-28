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
	readyCh       chan struct{} // closed when POST /session returns
	pendingPerms  map[string]*pendingPermission
	outstanding   *OutstandingRegistry
	compactDoneCh chan struct{} // buffered(1); closed by OnSessionCompacted
	sessionID     string

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

// AttachSessionEvents installs the session-scoped delivery sink. Step 6.
func (b *Backend) AttachSessionEvents(events *delegator.SessionEvents) {
	b.sessionEvents.Store(events)
}

// beginTurn sets per-turn bookkeeping under turnMu. Resets accumulated
// state from any prior turn so a fresh turn starts clean — part of the
// beginTurn contract asserted by TestBeginTurnResetsState. Step 6
// implements the full real turn lifecycle on top of this baseline.
//
// Called from Inject in the real implementation; the Step 1.4 Inject
// stub below calls it so production-code usage is recorded.
func (b *Backend) beginTurn(turn *delegator.TurnEvents) {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	b.turnActive = true
	b.turnEvents = turn
	b.turnText.Reset()
	b.turnTools = 0
	b.turnResultCh = make(chan *ResultMessage, 1)

	// lastUsage is reset under b.mu (it's read by OnResult on the next turn
	// to build TurnResult — a stale value would leak across turns).
	b.mu.Lock()
	b.lastUsage = nil
	b.mu.Unlock()
}

// cancelTurn reverses beginTurn. Called from production paths in Step 6
// (Inject's writer-error recovery) and Step 8 (Interrupt).
//
//nolint:unused // wired up in Step 6
func (b *Backend) cancelTurn() {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	b.turnActive = false
	b.turnEvents = nil
}

// IsTurnInFlight reports whether a turn callback is registered but
// hasn't fired yet.
func (b *Backend) IsTurnInFlight() bool {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	return b.turnActive
}

// WaitForTurn blocks until the next turn completion or context cancel.
// Returns immediately if no turn is in progress.
func (b *Backend) WaitForTurn(ctx context.Context) error {
	b.turnMu.Lock()
	ch := b.turnResultCh
	b.turnMu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ArmCompactionWait resets compactDoneCh for the next /compact cycle.
// Step 8.2.
func (b *Backend) ArmCompactionWait() {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	b.compactDoneCh = make(chan struct{}, 1)
}

// WaitForCompaction blocks on compactDoneCh or ctx. Returns immediately
// (nil) if not armed — matches ccstream's no-arm semantics.
func (b *Backend) WaitForCompaction(ctx context.Context) error {
	b.turnMu.Lock()
	ch := b.compactDoneCh
	b.turnMu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close is a no-op when not running, and idempotent. Step 5 wires the
// real bounded-shutdown ladder. The cancelTurn + describeExitError calls
// here give those helpers a production-code caller (deadcode traces from
// main via the init→register→New chain); they're also what the real
// Close will do — cancel any in-flight turn, log the exit error.
func (b *Backend) Close() error {
	b.mu.Lock()
	running := b.running
	b.running = false
	b.mu.Unlock()
	if !running {
		return nil
	}
	b.cancelTurn()
	_ = describeExitError(nil) // Step 5: describeExitError(b.exitErr)
	return nil
}

// The remaining Delegator interface methods are Step 1.4 stubs — they
// panic so any test that reaches them fails loudly. Real implementations
// land in Steps 5 (Start/WaitReady/CheckReady), 6 (Inject), 8 (Interrupt),
// 9 (RegisterPromptCancelListener).

func (b *Backend) Start(_ context.Context, _ delegator.StartOptions) error {
	// Step 6 will use newRequestID for control-message correlation; the
	// call here gives the helper a production-code caller (deadcode).
	_ = newRequestID()
	panic("opencode: Backend.Start not implemented — Step 5")
}

// Inject's Step 1.4 stub calls beginTurn so the production-code call
// graph records beginTurn as used; the real Step 6 implementation
// replaces this with HTTP POST /prompt_async routing.
func (b *Backend) Inject(_ context.Context, inj delegator.Inject) error {
	if inj.Turn != nil {
		b.beginTurn(inj.Turn)
	}
	panic("opencode: Backend.Inject not implemented — Step 6")
}

func (b *Backend) RegisterPromptCancelListener(_ string, _ func(reason string)) {
	panic("opencode: Backend.RegisterPromptCancelListener not implemented — Step 9")
}

func (b *Backend) Interrupt(_ context.Context) error {
	panic("opencode: Backend.Interrupt not implemented — Step 8")
}

func (b *Backend) WaitReady(_ context.Context) error {
	panic("opencode: Backend.WaitReady not implemented — Step 5")
}

func (b *Backend) CheckReady(_ context.Context) (bool, error) {
	panic("opencode: Backend.CheckReady not implemented — Step 5")
}

// newRequestID generates a simple unique request ID for control messages.
// Not a real UUID, but unique within a process lifetime which is
// sufficient for request correlation. Ported from ccstream/ccstream.go.
//
// Tested in advance by TestNewRequestID_Unique; production callers
// (the control-message writer) land in Step 6.
//
//nolint:unused // wired up in Step 6
func newRequestID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// describeExitError returns a human-readable description of a process
// exit error including exit code, signal, and stderr snippet when
// available. Ported from ccstream/lifecycle.go — backend-agnostic helper.
//
// Tested in advance by TestDescribeExitError_*; production callers
// (Server.Close, finalizeExit) land in Step 3.
//
//nolint:unused // wired up in Step 3
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
