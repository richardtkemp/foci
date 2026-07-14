// Package ccstream implements a Claude Code backend using the stream-json
// NDJSON protocol (--input-format stream-json --output-format stream-json).
// This replaces the tmux-based backend with structured stdin/stdout
// communication — no pane management, no screen scraping, no JSONL file watching.
package ccstream

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// autonomousInjectGrace is how long after an autonomous run goes idle that
// SourceSystem injects (reflection/keepalive) keep deferring. It bridges the
// sub-second gap before CC starts the next back-to-back autonomous run, so
// reflection can't slip in and get its silent sink to swallow that run —
// hardening #1047's core fix (#1048). A var so tests can shorten it.
var autonomousInjectGrace = 5 * time.Second

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
	permMode  string        // permission mode (from init, updated on /mode)
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
	// lastAutonomousEnd is when the most recent autonomous run went idle. For a
	// short grace after it (autonomousInjectGrace), SourceSystem injects still
	// defer in tryBeginTurn — see there.
	lastAutonomousEnd time.Time
	// lastGraceLogEnd dedups the Phase 4 grace-instrumentation log to one line
	// per grace window (guarded by turnMu) rather than one per inject retry.
	lastGraceLogEnd time.Time
	// autonomousStreamed records whether any top-level assistant text was
	// streamed to the session sink (se.OnText) during the current autonomous
	// run. When true, the run's text already reached the chat via the
	// late-delivery fallback, so onSessionIdle must NOT re-deliver the stashed
	// result (double-send guard). Reset when an autonomous run begins. See
	// onSessionIdle's autonomous branch (#1063).
	autonomousStreamed bool
	turnEvents        *delegator.TurnEvents // current turn's bookkeeping (OnTurnComplete, nudges); nil between turns
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
	contextWindow        int         // from modelUsage.contextWindow
	lastModel            string      // from assistant message
	lastUsage            *TokenUsage // per-call usage from last assistant message
	lastRateLimitWarnKey string      // OnRateLimit dedup key (status|type|resetsAt)

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

	// Agent tracking (shared with tmux backend via AgentTracker).
	agents delegator.SubagentTracker

	// subagentTails tails foreground subagent transcript files and forwards
	// their assistant text as subagent progress (foreground subagent text is
	// otherwise absent from the parent stdout stream). Lazily created on first
	// use via subagentTailMgr; nil-safe. See subagent_tail.go.
	subagentTailMu  sync.Mutex
	subagentTailMgr *subagentTailManager

	// Activity tracking — updated on every inbound stream event.
	lastActivity atomic.Int64 // unix nanos of most recent stream event

	// Callbacks (set before Start, read-only after)
	permPromptFn      delegator.PermissionPromptFunc
	onSessionReady    func(sessionID string)
	typingFunc        func(typing bool)
	onCompactionStart func()              // fired when status="compacting"
	onCompactionDone  func(preTokens int) // fired on compact_boundary
	onAuthFailure     func(detail string) // fired when CC reports a 401 auth failure (#843)
	onRateLimited     func(detail string) // fired with a rate_limit_event warning notice (#1211/#1238)
	onSessionLimit    func(until time.Time) // fired when CC reports a session limit was hit (synthetic message), with the parsed reset time
	// onAutonomousStart/onAutonomousEnd bracket a CC autonomous run (one foci
	// opened no turn for). onAutonomousStart fires on the false→true edge of
	// autonomousActive; onAutonomousEnd fires on the true→false edge, from
	// whichever site ends the run — CC idle, subprocess exit, or a foci turn
	// adopting it. The agent wires these to markInFlight/release so the run is
	// adopted as an in-flight delivering turn and concurrent injections are held
	// (#1070). Set before Start, read-only after. Both edges route through
	// setAutonomousActiveLocked so the pair is always balanced.
	onAutonomousStart func()
	onAutonomousEnd   func()

	// edgeCallbacks is the FIFO of pending onAutonomousStart/End callbacks.
	// setAutonomousActiveLocked appends here under turnMu (so enqueue order ==
	// true state-transition order); drainEdgeCallbacks fires them under fireMu
	// in that order. This is what keeps a start (reader goroutine) and an
	// adopting end (turn goroutine) from firing reversed — which would release
	// before adopt and leak a phantom in-flight adoption, wedging the inject
	// gate until the next run.
	edgeCallbacks []func()
	// fireMu serialises drainEdgeCallbacks so exactly one goroutine fires the
	// queued edges, in order. Distinct from turnMu — never held across turnMu,
	// and the fired callbacks (markInFlight) are agent-side, so no lock cycle.
	fireMu sync.Mutex

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

// SessionFilePath returns the on-disk path of this session's Claude Code
// transcript (~/.claude/projects/<slug>/<sessionID>.jsonl), or "" if the
// session id isn't known yet. Unlike SessionID(), which callers use to resume,
// this path lets foci's session-index sweeps (ArchiveSweep, PruneOrphans)
// manage CC transcripts on the same lifecycle as native session files: old
// inactive transcripts get gzipped, and index rows for manually-deleted
// transcripts get pruned. The path is derived (not stored) from the session id
// and workdir — the same construction ForkSession uses to locate a parent.
func (b *Backend) SessionFilePath() string {
	b.mu.Lock()
	sid := b.sessionID
	b.mu.Unlock()
	if sid == "" || b.workDir == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ccProjectsDir, projectSlug(b.workDir), sid+".jsonl")
}

// subagentTails returns the lazily-created foreground subagent transcript
// tailer for this backend. The deliver closure reads the current SessionEvents
// on each call so text always routes through the live session sink.
func (b *Backend) subagentTails() *subagentTailManager {
	b.subagentTailMu.Lock()
	defer b.subagentTailMu.Unlock()
	if b.subagentTailMgr == nil {
		b.subagentTailMgr = newSubagentTailManager(func(groupKey, text string) {
			if se := b.sessionEvents.Load(); se != nil && se.OnSubagentText != nil {
				se.OnSubagentText(groupKey, text)
			}
		})
	}
	return b.subagentTailMgr
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
