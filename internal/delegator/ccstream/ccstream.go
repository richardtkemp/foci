// Package ccstream implements a Claude Code backend using the stream-json
// NDJSON protocol (--input-format stream-json --output-format stream-json).
// This replaces the tmux-based backend with structured stdin/stdout
// communication — no pane management, no screen scraping, no JSONL file watching.
package ccstream

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/delegator"
)

func init() {
	delegator.Register("claude-code", newFromConfig, true)
	delegator.RegisterPlan("claude-code", planDelivery)
}

func newFromConfig(cfg map[string]any) (delegator.Delegator, error) {
	b := &Backend{
		readyCh:        make(chan struct{}),
		pendingPerms:   make(map[string]*pendingPermission),
		pendingElicits: make(map[string]*pendingElicitation),
		outstanding:    delegator.NewOutstandingRegistry(),
	}
	b.cfg = cfg
	return b, nil
}

// Backend implements delegator.Delegator using Claude Code's stream-json
// NDJSON protocol. CC runs as a subprocess with structured stdin/stdout
// communication — no tmux, no pane scraping, no JSONL file watching.
type Backend struct {
	// Configuration (immutable after Start)
	cfg          map[string]any
	workDir      string
	agentID      string
	label        string
	model        string
	systemPrompt string
	startOpts    delegator.StartOptions // saved for Restart

	// Process
	cmd     *exec.Cmd
	writer  *Writer
	cancel  context.CancelFunc // cancels reader goroutine + keep-alive
	done    chan struct{}      // closed when reader goroutine exits
	waitCh  chan error         // receives cmd.Wait() result (reaps zombie)
	exitCh  chan struct{}      // closed when exitErr is set
	exitErr error              // set by waiter goroutine when process exits

	// State
	mu        sync.Mutex
	running   bool
	closing   bool          // set by Close() before shutdown; tells OnReaderStopped this is expected
	sessionID string        // from init message
	initMsg   *InitMessage  // from init message
	readyCh   chan struct{} // closed when init received
	readyOnce sync.Once     // ensures readyCh closed once
	initReqID string        // request_id of the initialize control request

	// finalizeOnce gates the dead-process cleanup so it runs exactly once,
	// regardless of whether the waiter goroutine (cmd.Wait returned) or the
	// reader goroutine (scanner EOF / ctx cancel) notices first. See
	// finalizeExit. Reset in Restart() before relaunching the subprocess.
	finalizeOnce sync.Once

	// closeOnce gates the shutdown kill-ladder so it runs exactly once and is
	// driven off the subprocess being started, NOT the `running` flag — a
	// finalize path can flip running=false on a still-alive process, and Close
	// must still reap it (P1-9). Reset in Restart() before relaunching.
	closeOnce sync.Once

	// Session-scoped delivery callbacks. Set once by AttachSessionEvents
	// when the backend is acquired for a session, then live for the
	// session's lifetime. Reads are atomic.Pointer-protected so concurrent
	// readers (OnAssistant, OnTextDelta, OnThinkingDelta, hook dispatch)
	// don't need to take turnMu. Never nil after the first attach — text
	// and tool delivery never drop on a nil handler.
	sessionEvents atomic.Pointer[delegator.SessionEvents]

	// Turn state
	turnMu         sync.Mutex
	turnActive     bool
	// autonomousActive is true while CC runs a turn foci didn't open (an
	// "autonomous turn": a background-agent-completion or task-notification run
	// that produces work with no foci TurnEvents). Set on session_state=running
	// while !turnActive, cleared on idle / turn adoption. It gates SourceSystem
	// injects (tryBeginTurn) so reflection/keepalive can't inject into — and get
	// its silent-sink turn to swallow — live autonomous work (#1047).
	autonomousActive bool
	turnEvents       *delegator.TurnEvents // current turn's bookkeeping (OnTurnComplete, nudges); nil between turns
	turnResultCh   chan *ResultMessage   // buffered(1), receives result
	compactDoneCh  chan struct{}         // buffered(1), armed by ArmCompactionWait; fired on compact_boundary
	compactStartCh chan struct{}         // buffered(1), armed by ArmCompactionStartWait; fired on status="compacting"
	turnText       strings.Builder       // accumulates text across assistant messages
	turnTools      int                   // tool_use count this turn
	// Idle-keyed turn completion (#813 successor). The turn boundary is CC's
	// own `session_state_changed` running/idle SDK stream (enabled via
	// CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS=1 at launch): running/idle bracket
	// CC's entire internal run loop — every ask cycle, every drained
	// steer/follow-up, the background-agent wait, and the held-result flush —
	// so one foci turn == one CC run and `idle` is the authoritative "no more
	// results are coming" signal. `result` events are per-internal-ask-cycle
	// data carriers, NOT turn boundaries: a "now" steer aborts the current ask
	// and mints an extra result, a steer landing mid-tool folds and mints
	// none, and results are withheld (and can be silently dropped) while
	// background agents run — so no amount of result counting can recover the
	// turn boundary. OnResult stashes; OnSystem(idle) completes. See
	// docs/WIRING.md → "Idle-keyed turn completion".
	stashedResult      *delegator.TurnResult // latest per-ask-cycle result this turn; delivered at idle
	stashedResultMsg   *ResultMessage        // raw message of the stash, for WaitForTurn signalling
	turnOutputTokens   int                   // output tokens summed across this turn's ask cycles
	turnCalls          int                   // ask cycles (result events) observed this turn
	redispatchInFlight bool                  // pre-answer follow-up sent at idle; hold the turn open until its result arrives
	stateEventsSeen    bool                  // CC emitted ≥1 session_state_changed this session; gates the legacy complete-on-result fallback
	fallbackWarned     bool                  // one-shot Warnf when falling back to complete-on-result

	// Pending control responses (request_id → channel)
	pendingControlMu sync.Mutex
	pendingControls  map[string]chan json.RawMessage

	// Permissions
	permMu       sync.Mutex
	pendingPerms map[string]*pendingPermission

	// Elicitations (MCP user-input requests). Separate from pendingPerms
	// because elicitations aren't keyed to tool_use_ids and have a richer
	// lifecycle (sequential field walks, URL completion notifications).
	elicMu         sync.Mutex
	pendingElicits map[string]*pendingElicitation

	// Context tracking (from result/assistant messages)
	contextWindow int         // from modelUsage.contextWindow
	lastModel     string      // from assistant message
	lastUsage     *TokenUsage // per-call usage from last assistant message

	// Auto-approve rules (compiled from config, immutable after Start)
	autoApproveRules []autoApproveRule

	// Hook install state. Set by prepareHooks at Start so
	// handleHookResponse can filter events belonging to this backend from
	// events belonging to user-configured hooks. hookCmd is the full
	// shell-command string passed to CC via --settings; hookInstallID is
	// the unique ID bound into it and echoed back by foci-cc-hook. No
	// file state — CC receives the hook config as a JSON argv and the
	// subprocess-scoped temp file CC derives from it vanishes with the
	// process. See hooks.go for the full flow.
	hookCmd       string
	hookInstallID string

	// Rate limit state (shared across all backends for an agent).
	rateLimitState *RateLimitState

	// Agent tracking (shared with tmux backend via AgentTracker).
	agents delegator.AgentTracker

	// Activity tracking — updated on every inbound stream event.
	lastActivity atomic.Int64 // unix nanos of most recent stream event

	// Callbacks (set before Start, read-only after)
	permPromptFn      delegator.PermissionPromptFunc
	onSessionReady    func(sessionID string)
	typingFunc        func(typing bool)
	onCompactionStart func()              // fired when status="compacting"
	onCompactionDone  func(preTokens int) // fired on compact_boundary
	onAuthFailure     func(detail string) // fired when CC reports a 401 auth failure (#843)

	// outstanding tracks every prompt awaiting a user response (permissions,
	// AskUserQuestion sequences, MCP elicitations) under one lifecycle layer.
	// The kind-specific stores (pendingPerms, pendingElicits) keep their own
	// state — the registry coordinates registration, resolution, cancellation,
	// and the "all clear" drain hook used by DelegatedManager.WaitForPermission.
	outstanding *delegator.OutstandingRegistry
}

// newRequestID generates a simple unique request ID for control messages.
// Not a real UUID, but unique within a process lifetime which is sufficient
// for request correlation.
func newRequestID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// IsRunning reports whether the Claude Code subprocess is alive.
func (b *Backend) IsRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// SessionID returns the CC session identifier.
func (b *Backend) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}

// SessionFilePath returns empty — the stream backend stores session_id directly,
// not a file path. Callers should use SessionID() instead.
func (b *Backend) SessionFilePath() string {
	return ""
}

// touchActivity records the current time as the most recent stream event.
// Called from every On* handler to track backend liveness.
func (b *Backend) touchActivity() {
	b.lastActivity.Store(time.Now().UnixNano())
}

// LastActivity returns the time of the most recent stream event from CC.
// Implements delegator.ActivityChecker.
func (b *Backend) LastActivity() time.Time {
	ns := b.lastActivity.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// logComponent returns the log component string for this backend.
func (b *Backend) logComponent() string {
	if b.label != "" {
		return "ccstream:" + b.label
	}
	return "ccstream"
}
