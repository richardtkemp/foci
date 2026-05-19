// Package ccstream implements a Claude Code backend using the stream-json
// NDJSON protocol (--input-format stream-json --output-format stream-json).
// This replaces the tmux-based backend with structured stdin/stdout
// communication — no pane management, no screen scraping, no JSONL file watching.
package ccstream

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/procx"
)

func init() {
	delegator.Register("claude-code", newFromConfig)
}

// Close timeouts. Vars (not consts) so tests can shrink them; production
// path keeps the ~9s worst-case shutdown documented in Close.
var (
	closeGracefulWait = 5 * time.Second // wait for clean exit before SIGTERM
	closeSigtermWait  = 2 * time.Second // wait after SIGTERM before SIGKILL
	closeSigkillWait  = 2 * time.Second // wait after SIGKILL before abandoning the waiter goroutine
)

func newFromConfig(cfg map[string]any) (delegator.Delegator, error) {
	b := &Backend{
		readyCh:        make(chan struct{}),
		pendingPerms:   make(map[string]*pendingPermission),
		pendingElicits: make(map[string]*pendingElicitation),
		outstanding:    NewOutstandingRegistry(),
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
	turnEvents     *delegator.TurnEvents // current turn's bookkeeping (OnTurnComplete, nudges); nil between turns
	turnResultCh   chan *ResultMessage   // buffered(1), receives result
	compactDoneCh  chan struct{}         // buffered(1), armed by ArmCompactionWait; fired on compact_boundary
	compactStartCh chan struct{}         // buffered(1), armed by ArmCompactionStartWait; fired on status="compacting"
	turnText       strings.Builder       // accumulates text across assistant messages
	turnTools      int                   // tool_use count this turn

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

	// outstanding tracks every prompt awaiting a user response (permissions,
	// AskUserQuestion sequences, MCP elicitations) under one lifecycle layer.
	// The kind-specific stores (pendingPerms, pendingElicits) keep their own
	// state — the registry coordinates registration, resolution, cancellation,
	// and the "all clear" drain hook used by DelegatedManager.WaitForPermission.
	outstanding *OutstandingRegistry
}

// newRequestID generates a simple unique request ID for control messages.
// Not a real UUID, but unique within a process lifetime which is sufficient
// for request correlation.
func newRequestID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// Start launches the Claude Code subprocess with stream-json pipes.
func (b *Backend) Start(ctx context.Context, opts delegator.StartOptions) error {
	b.startOpts = opts
	b.workDir = opts.WorkDir
	b.agentID = opts.AgentID
	b.label = opts.Label
	b.model = opts.Model
	b.systemPrompt = opts.SystemPrompt
	b.autoApproveRules = parseAutoApproveRules(opts.AutoApproveRules)

	// Build command args.
	args := []string{
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--permission-prompt-tool", "stdio",
		"--include-partial-messages",
		"--include-hook-events",
		"--verbose",
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	// Permission pre-approval rules. cfg["allowed_tools"] is the merged
	// string produced by cmd/foci-gw/agents_delegated.go (global
	// [cc_backend] default_allowed_tools combined with the agent's
	// backend_config.allowed_tools). Rules use CC's permission syntax —
	// e.g. "Write(/tmp/**)", "Bash(git:*)".
	if v, ok := b.cfg["allowed_tools"].(string); ok && v != "" {
		args = append(args, "--allowedTools", v)
	}

	component := "ccstream"
	if opts.Label != "" {
		component = "ccstream:" + opts.Label
	}

	// Build foci's hook settings JSON and append it as a --settings argv
	// so CC loads it as a flagSettings source (always enabled, merges
	// with user hooks automatically). Skipped when the foci-cc-hook
	// binary can't be located — Warn logged, ccstream runs without
	// OnToolEnd events. See hooks.go for the full flow.
	if hookSettings, ok := b.prepareHooks(); ok {
		args = append(args, "--settings", hookSettings)
	}

	// Resolve the binary to spawn. Production runs use "claude" (resolved
	// via $PATH); integration tests inject a stub via the claude_binary
	// config knob (folded into b.cfg by cmd/foci-gw/agents_delegated.go
	// from global [cc_backend].claude_binary, with per-agent override).
	claudeBin := "claude"
	if v, ok := b.cfg["claude_binary"].(string); ok && v != "" {
		claudeBin = v
	}

	log.Infof(component, "launching: %s %s (workdir=%s)", claudeBin, strings.Join(args, " "), opts.WorkDir)

	// Create command with its own cancellable context. The CC process is
	// long-lived (surviving across turns), so it must NOT be tied to the
	// caller's context — otherwise the process is killed when the turn
	// context expires or is cancelled.
	cmdCtx, cmdCancel := context.WithCancel(context.Background())
	cmd := procx.Spawn(cmdCtx, claudeBin, args...)
	cmd.Dir = opts.WorkDir
	cmd.Env = os.Environ()

	// Apply extra environment variables from StartOptions (e.g. BASH_ENV,
	// FOCI_SOCK from the exec bridge created by DelegatedManager).
	for k, v := range opts.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	// Get pipes.
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cmdCancel()
		return fmt.Errorf("ccstream: stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cmdCancel()
		return fmt.Errorf("ccstream: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cmdCancel()
		return fmt.Errorf("ccstream: stderr pipe: %w", err)
	}

	// Start the process.
	if err := cmd.Start(); err != nil {
		cmdCancel()
		return fmt.Errorf("ccstream: start: %w", err)
	}

	b.cmd = cmd
	b.writer = NewWriter(stdinPipe)
	b.cancel = cmdCancel
	b.done = make(chan struct{})

	// Reader goroutine — dispatches CC stdout messages to handler methods.
	readerCtx, readerCancel := context.WithCancel(context.Background())
	// Store readerCancel so Close can stop reader + keep-alive independently
	// of the command context.
	origCancel := b.cancel
	b.cancel = func() {
		readerCancel()
		origCancel()
	}

	go func() {
		defer close(b.done)
		reader := NewReader(stdoutPipe, b)
		reader.Run(readerCtx)
	}()

	// Stderr capture goroutine.
	go b.captureStderr(stderrPipe)

	// Keep-alive goroutine.
	go b.runKeepAlive(readerCtx)

	// Process waiter goroutine — reaps the subprocess and logs exit status.
	// Without this, a dead subprocess becomes a zombie until Close() is called.
	//
	// This goroutine is the AUTHORITATIVE source of "process is dead". The
	// reader goroutine may exit silently (ctx cancelled, partial line, etc.)
	// and miss the death; cmd.Wait() cannot. After logging, we invoke
	// finalizeExit to guarantee in-flight turn cleanup and `running=false`
	// run exactly once, even if the reader path also reaches OnReaderStopped.
	b.waitCh = make(chan error, 1)
	b.exitCh = make(chan struct{})
	go func() {
		err := cmd.Wait()
		b.exitErr = err // store for OnError; read after exitCh is closed
		close(b.exitCh)
		comp := b.logComponent()
		if err != nil {
			log.Warnf(comp, "process exited: %s", describeExitError(err))
		} else {
			log.Infof(comp, "process exited cleanly (status 0)")
		}
		// Drive cleanup regardless of whether the reader goroutine notices.
		// finalizeExit is idempotent — if OnReaderStopped already ran, this
		// is a no-op.
		b.finalizeExit(err)
		b.waitCh <- err
	}()

	// Send initialize control request with system prompt.
	// Save the request ID so OnControlResponse can detect the response
	// and close readyCh. For fresh sessions (no --resume), CC responds
	// with a control_response rather than emitting system/init.
	initReqID := newRequestID()
	b.mu.Lock()
	b.initReqID = initReqID
	b.mu.Unlock()
	if err := b.writer.SendControl(initReqID, &InitializeRequest{
		Subtype:      "initialize",
		SystemPrompt: opts.SystemPrompt,
	}); err != nil {
		return fmt.Errorf("ccstream: send initialize: %w", err)
	}

	b.mu.Lock()
	b.running = true
	b.mu.Unlock()

	return nil
}

// Close shuts down the Claude Code subprocess gracefully.
func (b *Backend) Close() error {
	b.mu.Lock()
	if !b.running {
		b.mu.Unlock()
		return nil
	}
	b.running = false
	b.closing = true
	b.mu.Unlock()

	// Try graceful shutdown: only send an interrupt if a turn is in flight.
	// CC's interrupt handler aborts the per-turn AbortController; sent after
	// a clean turn end it cascades through stale post-turn async work and
	// flips CC's exit code from 0 to 1 (CC keys exit code on the last result
	// message's is_error flag — the abort can replace a success result with
	// an error_during_execution one). Closing stdin alone is sufficient to
	// shut CC down cleanly when there's nothing to abort.
	if b.IsTurnInFlight() {
		_ = b.writer.SendInterrupt()
	}
	_ = b.writer.Close()

	// Wait for process exit with timeout. The waiter goroutine (launched in
	// Start) calls cmd.Wait() and sends the result to waitCh. If the process
	// already exited, this returns immediately.
	//
	// Every wait has a bounded timeout: even after SIGKILL we cap the final
	// wait, because the waiter goroutine has been observed to stall inside
	// finalizeExit (e.g. handler callbacks holding locks, see TODO #749).
	// An unbounded `<-waitCh` here held m.mu in the caller (ResetSession /
	// Get) and froze the entire agent until manual restart. The timeout
	// trades a possible zombie-process leak for liveness — Close must always
	// return so callers can release locks and respawn.
	component := b.logComponent()
	select {
	case <-b.waitCh:
		// Process already exited (or just did).
	case <-time.After(closeGracefulWait):
		// SIGTERM.
		log.Warnf(component, "process did not exit after %s, sending SIGTERM", closeGracefulWait)
		if b.cmd.Process != nil {
			_ = b.cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case <-b.waitCh:
		case <-time.After(closeSigtermWait):
			// SIGKILL.
			log.Warnf(component, "process did not exit after SIGTERM, sending SIGKILL")
			if b.cmd.Process != nil {
				_ = b.cmd.Process.Kill()
			}
			select {
			case <-b.waitCh:
			case <-time.After(closeSigkillWait):
				// Bounded fallback — the waiter goroutine stalled (see
				// finalizeExit instrumentation). Process is already SIGKILL'd
				// so the OS will reap it; we just stop waiting for the
				// goroutine to confirm. Without this cap, m.mu in the
				// caller is held forever and no further messages can be
				// processed for this agent.
				log.Warnf(component, "waiter goroutine did not report after SIGKILL within %s — abandoning wait (possible zombie)", closeSigkillWait)
			}
		}
	}

	// Cancel reader + keep-alive goroutines.
	if b.cancel != nil {
		b.cancel()
	}

	// Wait for reader goroutine to exit.
	if b.done != nil {
		<-b.done
	}

	// No hook cleanup needed — the CC subprocess exits with our
	// --settings temp file still on disk, but it's owned by CC and the
	// content-hash path is stable so it naturally de-dupes across
	// backend restarts.

	return nil
}

// Restart kills and relaunches the Claude Code subprocess.
func (b *Backend) Restart(ctx context.Context) error {
	_ = b.Close()

	// Reset state for fresh start.
	b.readyCh = make(chan struct{})
	b.readyOnce = sync.Once{}
	b.finalizeOnce = sync.Once{}
	b.mu.Lock()
	b.initReqID = ""
	b.mu.Unlock()

	b.permMu.Lock()
	b.pendingPerms = make(map[string]*pendingPermission)
	b.permMu.Unlock()

	b.elicMu.Lock()
	b.pendingElicits = make(map[string]*pendingElicitation)
	b.elicMu.Unlock()

	// Drain the registry without firing onEmpty (subprocess restarts are not
	// user-driven cancellations). Reset by replacing the registry while
	// preserving the configured drain hook.
	onEmpty := b.outstanding.onEmptyHook()
	b.outstanding = NewOutstandingRegistry()
	b.outstanding.SetOnEmpty(onEmpty)

	return b.Start(ctx, b.startOpts)
}

// IsRunning reports whether the Claude Code subprocess is alive.
func (b *Backend) IsRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// WaitReady blocks until the init message is received from CC, the
// subprocess reader exits (e.g. CC died before init — happens when
// --resume points at a missing session and CC exits non-zero), or the
// caller's context expires. Returning an error on early-exit lets
// DelegatedManager's retry-without-resume path fire immediately rather
// than burning the full ready-timeout budget waiting for an init that
// can no longer arrive.
func (b *Backend) WaitReady(ctx context.Context) error {
	select {
	case <-b.readyCh:
		return nil
	case <-b.done:
		return fmt.Errorf("ccstream: subprocess exited before init")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// Turn methods
// ---------------------------------------------------------------------------

// AttachSessionEvents installs the session-scoped delivery callbacks. Stored
// in atomic.Pointer so concurrent readers (text/tool emission paths) don't
// take turnMu. Idempotent — re-attachment replaces the previous events,
// which is useful in tests and in the AttachSessionEvents-per-Get pattern
// the agent layer uses.
func (b *Backend) AttachSessionEvents(events *delegator.SessionEvents) {
	b.sessionEvents.Store(events)
}

// beginTurn initialises all turn-related state for a new turn. turn carries
// the per-turn bookkeeping callbacks (OnTurnComplete, nudges); may be nil
// for fire-and-forget paths (slash commands, tests) that don't need
// completion signalling.
func (b *Backend) beginTurn(turn *delegator.TurnEvents) {
	b.turnMu.Lock()
	b.turnActive = true
	b.turnEvents = turn
	b.turnText.Reset()
	b.turnTools = 0
	b.turnResultCh = make(chan *ResultMessage, 1)
	b.turnMu.Unlock()

	b.mu.Lock()
	b.lastUsage = nil
	b.mu.Unlock()

	// Seed activity timestamp so the idle reaper has an initial deadline
	// rather than polling indefinitely when no events arrive.
	b.touchActivity()
}

// cancelTurn reverses beginTurn on send failure.
func (b *Backend) cancelTurn() {
	b.turnMu.Lock()
	b.turnActive = false
	b.turnEvents = nil
	b.turnMu.Unlock()
}

// sendToPane is the internal begin-turn primitive: starts a fresh turn
// with the given bookkeeping callbacks and sends a plain text user message.
// Called from Inject's begin-turn path (SourceUser/Steer at idle); not part
// of the public Delegator surface.
func (b *Backend) sendToPane(_ context.Context, prompt string, turn *delegator.TurnEvents) error {
	b.beginTurn(turn)

	if b.typingFunc != nil {
		b.typingFunc(true)
	}

	b.logger().Debugf("sendToPane: calling writer.SendUser (%d bytes)", len(prompt))
	sendStart := time.Now()
	if err := b.writer.SendUser(prompt); err != nil {
		b.cancelTurn()
		return fmt.Errorf("ccstream: send user message: %w", err)
	}
	if elapsed := time.Since(sendStart); elapsed > 5*time.Second {
		b.logger().Warnf("sendToPane: writer.SendUser took %s (slow — possible mutex contention or blocked stdin)", elapsed.Round(time.Millisecond))
	} else {
		b.logger().Debugf("sendToPane: writer.SendUser returned in %s", elapsed.Round(time.Millisecond))
	}

	return nil
}

// sendToPaneWithAttachments is the internal begin-turn primitive for
// prompts that carry images/documents. Builds structured content blocks
// (text first, then each attachment as image/document) and sends a single
// user message containing all of them. Called from Inject's begin-turn
// path when len(inj.Attachments) > 0.
func (b *Backend) sendToPaneWithAttachments(_ context.Context, prompt string, attachments []delegator.Attachment, turn *delegator.TurnEvents) error {
	b.beginTurn(turn)

	if b.typingFunc != nil {
		b.typingFunc(true)
	}

	// Build content blocks: text first, then attachments.
	var blocks []ContentBlock
	if prompt != "" {
		blocks = append(blocks, ContentBlock{Type: "text", Text: prompt})
	}
	for _, att := range attachments {
		blockType := attachmentBlockType(att.MimeType)
		blocks = append(blocks, ContentBlock{
			Type: blockType,
			Source: &ContentBlockSource{
				Type:     "base64",
				MimeType: att.MimeType,
				Data:     base64.StdEncoding.EncodeToString(att.Data),
			},
		})
	}

	b.logger().Debugf("sendToPaneWithAttachments: calling writer.Send (%d blocks)", len(blocks))
	sendStart := time.Now()
	if err := b.writer.Send(NewUserMessageBlocks(blocks)); err != nil {
		b.cancelTurn()
		return fmt.Errorf("ccstream: send user message with attachments: %w", err)
	}
	if elapsed := time.Since(sendStart); elapsed > 5*time.Second {
		b.logger().Warnf("sendToPaneWithAttachments: writer.Send took %s (slow)", elapsed.Round(time.Millisecond))
	} else {
		b.logger().Debugf("sendToPaneWithAttachments: writer.Send returned in %s", elapsed.Round(time.Millisecond))
	}

	return nil
}

// attachmentBlockType returns the CC content block type for a MIME type.
func attachmentBlockType(mimeType string) string {
	if strings.HasPrefix(mimeType, "image/") {
		return "image"
	}
	return "document"
}

// WaitForTurn blocks until the current turn completes (result message received).
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

// IsTurnInFlight reports whether a turn callback is registered but hasn't
// fired yet.
func (b *Backend) IsTurnInFlight() bool {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	return b.turnActive
}

// sendUserMessage is the internal primitive that writes a user-role
// message to CC at the default priority ("next"). For mid-turn injections
// (follow-up SourceUser, post-tool nudges, slash commands), CC's mid-turn
// drain at the next tool boundary folds the message into the current
// ask() — there is no separate ask/result cycle to wait for.
func (b *Backend) sendUserMessage(text string) error {
	return b.writer.SendUser(text)
}

// sendUserMessagePriority writes a user-role message at the given queue
// priority ("now" / "next" / "later"). Used by SourceSteer dispatch so
// the message dequeues ahead of any other queued items at CC's next
// mid-turn drain.
func (b *Backend) sendUserMessagePriority(text, priority string) error {
	return b.writer.SendUserPriority(text, priority)
}

// Inject is the canonical entry point for delivering a user-role event to
// CC. It subsumes SendToPane / SendToPaneWithAttachments / SendCommand —
// the routing decision (begin turn vs queue follow-up vs interrupt+queue
// vs slash command) lives in one place rather than being scattered across
// callsites.
//
// Routing matrix:
//
//	Source   | Turn state | Action
//	---------|------------|--------------------------------------------
//	User     | idle       | begin turn (with attachments if provided)
//	User     | in-flight  | SendUser at default priority; CC folds via mid-turn drain
//	Steer    | in-flight  | SendUser at priority "now"; CC folds via mid-turn drain
//	Steer    | idle       | begin turn — degrades to User-idle
//	Compact  | any        | send slash command (fire-and-forget)
//	Pass     | any        | send slash command (fire-and-forget)
//
// All in-flight injections rely on CC's mid-turn drain
// (claude-code/src/query.ts:1570-1589) to fold the message as an
// attachment into the current ask() — no separate ask/result cycle
// is produced for them. The model addresses the message in the same
// turn and the response reaches the original handler.
//
// inj.Turn is required for SourceUser/Steer at idle (a fresh turn needs an
// OnTurnComplete sink). Ignored for in-flight injections (the existing
// TurnEvents persists) and for slash commands. Delivery (text, tool events)
// flows through the SessionEvents installed via AttachSessionEvents — not
// inj.Turn.
//
// inj.Attachments are honored only when beginning a new turn; ignored
// otherwise. They become structured content blocks alongside the text.
func (b *Backend) Inject(ctx context.Context, inj delegator.Inject) error {
	inFlight := b.IsTurnInFlight()
	b.logger().Debugf("Inject: source=%s text_bytes=%d attachments=%d in_flight=%v",
		inj.Source, len(inj.Text), len(inj.Attachments), inFlight)

	// Back-compat shim: legacy callers (cctmux-shaped mocks, older tests)
	// still pass Inject.Handler. Split it into the new SessionEvents +
	// TurnEvents form so the rest of ccstream only deals with the split
	// types. Removed once all callers migrate (TODO #747 cleanup).
	if inj.Turn == nil && inj.Handler != nil {
		h := inj.Handler
		b.AttachSessionEvents(&delegator.SessionEvents{
			OnText:          h.OnText,
			OnTextDelta:     h.OnTextDelta,
			OnThinkingDelta: h.OnThinkingDelta,
			OnToolStart:     h.OnToolStart,
			OnToolEnd:       h.OnToolEnd,
		})
		inj.Turn = &delegator.TurnEvents{
			OnTurnComplete:     h.OnTurnComplete,
			PostToolNudgeFunc:  h.PostToolNudgeFunc,
			PreAnswerNudgeFunc: h.PreAnswerNudgeFunc,
		}
	}

	switch inj.Source {
	case delegator.SourceUser:
		if !inFlight {
			return b.beginTurnWithText(ctx, inj.Text, inj.Attachments, inj.Turn)
		}
		// In-flight follow-up: SendUser at default priority. CC's mid-turn
		// drain at the next tool boundary (claude-code's
		// query.ts:1570-1589) folds the message as an attachment to the
		// current turn's tool_results — the model responds in the same
		// ask(), so the original turn's OnTurnComplete and the always-live
		// SessionEvents.OnText carry the response. inj.Turn is
		// intentionally ignored.
		return b.sendUserMessage(inj.Text)

	case delegator.SourceSteer:
		if !inFlight {
			// Edge case: steer at idle. Degrade to begin-turn.
			b.logger().Debugf("Inject(Steer): no turn in flight, degrading to begin-turn")
			return b.beginTurnWithText(ctx, inj.Text, inj.Attachments, inj.Turn)
		}
		// In-flight steer: SendUser at priority "now". CC dequeues "now"
		// ahead of "next"/"later" at the next mid-turn drain, so the
		// steer message folds in before any other queued items without
		// aborting the current ask() and killing in-flight tool work.
		// "Stop right now" semantics live in /reset hard, not Steer.
		return b.sendUserMessagePriority(inj.Text, "now")

	case delegator.SourceCompact, delegator.SourcePass:
		// Slash commands. Fire-and-forget. The caller is responsible for
		// any synchronisation (e.g. compaction.go arms CompactionWaiter
		// before calling Inject).
		if inFlight {
			b.logger().Warnf("Inject(%s): called with turn in flight — slash command will queue behind active turn", inj.Source)
		}
		return b.sendUserMessage(inj.Text)
	}
	return fmt.Errorf("ccstream: Inject: unknown source %d", inj.Source)
}

// beginTurnWithText starts a new turn, dispatching to the attachments path
// when the inject carries them and to plain text otherwise. Internal to
// Inject — callers reach turn-start through Inject(SourceUser) at idle.
func (b *Backend) beginTurnWithText(ctx context.Context, text string, atts []delegator.Attachment, turn *delegator.TurnEvents) error {
	if len(atts) > 0 {
		return b.sendToPaneWithAttachments(ctx, text, atts, turn)
	}
	return b.sendToPane(ctx, text, turn)
}

// ---------------------------------------------------------------------------
// Callback setters
// ---------------------------------------------------------------------------

// SetPermissionPromptFunc sets the function used to send permission prompts.
func (b *Backend) SetPermissionPromptFunc(fn delegator.PermissionPromptFunc) { b.permPromptFn = fn }

// SetOnPromptsCleared sets a callback fired when the last outstanding prompt
// (permission, question, or elicitation) is removed. Used by
// DelegatedManager.WaitForPermission to unblock once all pending prompts have
// been resolved or cancelled.
func (b *Backend) SetOnPromptsCleared(fn func()) { b.outstanding.SetOnEmpty(fn) }

// RegisterPromptCancelListener appends a callback fired when the prompt with
// requestID is cancelled by a non-user path (e.g. CC's control_cancel_request
// after a follow-up message aborted the in-flight tool execution). The
// listener does NOT fire on normal user responses — use it to clean up
// per-prompt UI state (e.g. disable the inline keyboard) so the user can't
// click an already-resolved button. Multiple listeners may be registered for
// the same requestID; they fire in registration order. If no prompt with
// requestID is registered, the call is a silent no-op.
func (b *Backend) RegisterPromptCancelListener(requestID string, fn func(reason string)) {
	b.outstanding.AddCancelListener(requestID, fn)
}

// SetOnSessionReady sets a callback fired once when the session ID is known.
func (b *Backend) SetOnSessionReady(fn func(string)) { b.onSessionReady = fn }

// SetTypingFunc sets a callback to control the platform's typing indicator.
func (b *Backend) SetTypingFunc(fn func(bool)) { b.typingFunc = fn }

// SetOnCompactionStart sets a callback fired when CC begins compacting.
func (b *Backend) SetOnCompactionStart(fn func()) { b.onCompactionStart = fn }

// SetOnCompactionDone sets a callback fired when CC finishes compaction.
// preTokens is the token count before compaction.
func (b *Backend) SetOnCompactionDone(fn func(preTokens int)) { b.onCompactionDone = fn }

// ArmCompactionWait sets up a one-shot channel that will be closed when
// compact_boundary is received. Must be called before the /compact command
// is sent so the signal is never missed.
func (b *Backend) ArmCompactionWait() {
	b.turnMu.Lock()
	b.compactDoneCh = make(chan struct{}, 1)
	b.turnMu.Unlock()
}

// WaitForCompaction blocks until compact_boundary is received or ctx expires.
// Returns immediately if no waiter is armed (ArmCompactionWait was not called).
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

// ArmCompactionStartWait sets up a one-shot channel that will be closed when
// status="compacting" is received. Must be called before the /compact command
// is sent so the signal is never missed.
func (b *Backend) ArmCompactionStartWait() {
	b.turnMu.Lock()
	b.compactStartCh = make(chan struct{}, 1)
	b.turnMu.Unlock()
}

// WaitForCompactionStart blocks until status="compacting" is received or ctx
// expires. Returns immediately if no waiter is armed.
func (b *Backend) WaitForCompactionStart(ctx context.Context) error {
	b.turnMu.Lock()
	ch := b.compactStartCh
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

// SetOnAgentStatus sets a callback for agent/task lifecycle events.
func (b *Backend) SetOnAgentStatus(fn func(string)) { b.agents.OnStatus = fn }

// SetRateLimitState sets the shared rate limit state that OnRateLimit writes to.
// Must be called before Start. The state is shared across all backends for an agent.
func (b *Backend) SetRateLimitState(s *RateLimitState) { b.rateLimitState = s }

// ---------------------------------------------------------------------------
// State methods
// ---------------------------------------------------------------------------

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

// SendKeystroke is a no-op for the stream backend (no TUI).
func (b *Backend) SendKeystroke(ctx context.Context, key string) error {
	return fmt.Errorf("SendKeystroke not supported by stream backend")
}

// SendSpecialKey is a no-op for the stream backend (no TUI).
func (b *Backend) SendSpecialKey(ctx context.Context, key string) error {
	return fmt.Errorf("SendSpecialKey not supported by stream backend")
}

// Interrupt cancels the current agent turn by sending an interrupt control
// message over the stdio protocol.
func (b *Backend) Interrupt(ctx context.Context) error {
	return b.writer.SendInterrupt()
}

// SetModel sends a set_model control request to CC via the generic
// ControlSender interface. Convenience method retained for direct callers.
func (b *Backend) SetModel(ctx context.Context, model string) error {
	return b.SendControl(ctx, &delegator.SetModelRequest{Model: model})
}

// GetContextUsage sends a get_context_usage control request and returns the
// parsed response. Zero API cost — CC computes this locally. ~650ms on a
// persistent session.
func (b *Backend) GetContextUsage(ctx context.Context) (*delegator.ContextUsage, error) {
	reqID := newRequestID()

	// Arm response channel before sending.
	ch := make(chan json.RawMessage, 1)
	b.pendingControlMu.Lock()
	if b.pendingControls == nil {
		b.pendingControls = make(map[string]chan json.RawMessage)
	}
	b.pendingControls[reqID] = ch
	b.pendingControlMu.Unlock()

	if err := b.writer.SendControl(reqID, &GetContextUsageRequest{
		Subtype: "get_context_usage",
	}); err != nil {
		// Clean up on send failure.
		b.pendingControlMu.Lock()
		delete(b.pendingControls, reqID)
		b.pendingControlMu.Unlock()
		return nil, fmt.Errorf("send get_context_usage: %w", err)
	}

	select {
	case raw := <-ch:
		var env controlResponseInbound
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, fmt.Errorf("unmarshal control_response envelope: %w", err)
		}
		if env.Response.Subtype != "success" {
			return nil, fmt.Errorf("get_context_usage returned subtype %q", env.Response.Subtype)
		}
		var payload contextUsagePayload
		if err := json.Unmarshal(env.Response.Response, &payload); err != nil {
			return nil, fmt.Errorf("unmarshal context_usage payload: %w", err)
		}
		cats := make([]delegator.ContextCategory, len(payload.Categories))
		for i, c := range payload.Categories {
			cats[i] = delegator.ContextCategory{Name: c.Name, Tokens: c.Tokens}
		}
		return &delegator.ContextUsage{
			TotalTokens:          payload.TotalTokens,
			MaxTokens:            payload.MaxTokens,
			Percentage:           payload.Percentage,
			AutoCompactThreshold: payload.AutoCompactThreshold,
			Model:                payload.Model,
			Categories:           cats,
		}, nil
	case <-ctx.Done():
		// Clean up on timeout.
		b.pendingControlMu.Lock()
		delete(b.pendingControls, reqID)
		b.pendingControlMu.Unlock()
		return nil, ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// Handler interface implementation (called by Reader goroutine)
// ---------------------------------------------------------------------------

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

// OnAssistant handles assistant messages from CC's stdout.
//
// Sub-agent messages (ParentToolUseID != nil) are filtered out of the
// turn-state updates and handler callbacks below — sub-agents run their own
// turn via the Agent tool, and their text / tool_use blocks belong to the
// sub-agent's transcript rather than the parent turn the caller is
// observing. Without this guard, sub-agent text would fire OnText onto the
// parent's StreamingSink (rendering nested text twice) and sub-agent
// tool_use blocks would fire OnToolStart onto the parent tracker. Model /
// usage tracking is already gated on isTopLevel to protect the primary
// model name from subagent haiku overrides.
func (b *Backend) OnAssistant(msg *AssistantMessage) {
	b.touchActivity()
	isTopLevel := msg.ParentToolUseID == nil

	// Block-type breakdown for diagnostics — distinguishes "model
	// produced text but it didn't reach delivery" from "model produced
	// no text block at all" when investigating delivery gaps.
	if isTopLevel {
		var textBlocks, toolUseBlocks, thinkingBlocks, totalTextBytes int
		for _, block := range msg.Message.Content {
			switch block.Type {
			case "text":
				textBlocks++
				totalTextBytes += len(block.Text)
			case "tool_use":
				toolUseBlocks++
			case "thinking":
				thinkingBlocks++
			}
		}
		stopReason := ""
		if msg.Message.StopReason != nil {
			stopReason = *msg.Message.StopReason
		}
		log.Debugf("ccstream", "OnAssistant: text_blocks=%d tool_use_blocks=%d thinking_blocks=%d text_bytes=%d stop_reason=%s",
			textBlocks, toolUseBlocks, thinkingBlocks, totalTextBytes, stopReason)
	}

	b.mu.Lock()
	if isTopLevel && msg.Message.Model != "" {
		b.lastModel = msg.Message.Model
	}
	if isTopLevel {
		u := msg.Message.Usage
		b.lastUsage = &u
	}
	b.mu.Unlock()

	// Delivery callbacks come from the session-scoped SessionEvents — never
	// nil after first AttachSessionEvents, so text/tool blocks always have
	// somewhere to go regardless of per-turn TurnEvents state. This is what
	// kills the "text block dropped: handler nil" failure mode at backend
	// layer; see TODO #747.
	se := b.sessionEvents.Load()

	if !isTopLevel {
		// Surface sub-agent text as blockquoted intermediate replies so
		// the user can follow sub-agent progress. Tool_use blocks are not
		// forwarded — the parent tracker owns tool visibility.
		if se != nil && se.OnText != nil {
			for _, block := range msg.Message.Content {
				if block.Type == "text" && block.Text != "" {
					se.OnText(blockquote(block.Text))
				}
			}
		}
		// Keep typing indicator alive during sub-agent work.
		if b.typingFunc != nil {
			b.typingFunc(true)
		}
		return
	}

	for _, block := range msg.Message.Content {
		switch block.Type {
		case "text":
			b.turnMu.Lock()
			b.turnText.WriteString(block.Text)
			b.turnMu.Unlock()

			if se != nil && se.OnText != nil {
				se.OnText(block.Text)
			}

		case "tool_use":
			b.turnMu.Lock()
			b.turnTools++
			b.turnMu.Unlock()

			if se != nil && se.OnToolStart != nil {
				inputStr := string(block.Input)
				se.OnToolStart(block.ID, block.Name, inputStr)
			}

			// Track Agent tool calls for status reporting (same as tmux backend).
			if block.Name == "Agent" {
				desc := delegator.ExtractAgentDescription(block.Input)
				b.agents.Add(block.ID, desc)
			}

		case "thinking":
			// Thinking blocks are informational; optionally log.
		}
	}

	// Restart typing indicator if the turn hasn't ended.
	if msg.Message.StopReason == nil || *msg.Message.StopReason != "end_turn" {
		if b.typingFunc != nil {
			b.typingFunc(true)
		}
	}
}

// OnResult handles the result message signalling turn completion.
func (b *Backend) OnResult(msg *ResultMessage) {
	b.touchActivity()

	// Capture turn state. TurnEvents clearing is deferred — the pre-answer
	// gate path needs OnTurnComplete alive across rounds. Normal path
	// clears turnEvents/turnActive below.
	b.turnMu.Lock()
	turn := b.turnEvents
	resultCh := b.turnResultCh
	turnText := b.turnText.String()
	turnTools := b.turnTools
	b.turnMu.Unlock()

	// Build TurnResult. Prefer turnText (accumulated from all assistant
	// messages in the turn) over msg.Result (which only contains the last
	// segment). Multi-segment turns (text → tool → text) need the full text.
	text := turnText
	if text == "" {
		text = msg.Result
	}

	// Determine model from lastModel (set by OnAssistant, filtered to top-level
	// messages only — subagent models are excluded). Use per-call usage from
	// the last assistant message (not the result's accumulated total) — this
	// matches what the tmux watcher reports and gives compaction the actual
	// context window fill, not a sum of all calls.
	b.mu.Lock()
	resultModel := b.lastModel
	lastUsage := b.lastUsage
	b.lastUsage = nil // reset for next turn
	b.mu.Unlock()

	// Pick context window from ModelUsage deterministically: prefer the
	// entry matching resultModel (the primary model from assistant messages);
	// otherwise take the largest context window to avoid spurious compaction
	// from subagent models (e.g. haiku) winning the random map iteration.
	if usage, ok := msg.ModelUsage[resultModel]; ok {
		b.mu.Lock()
		b.contextWindow = usage.ContextWindow
		b.mu.Unlock()
	} else {
		var bestCW int
		for _, usage := range msg.ModelUsage {
			if usage.ContextWindow > bestCW {
				bestCW = usage.ContextWindow
			}
		}
		if bestCW > 0 {
			b.mu.Lock()
			b.contextWindow = bestCW
			b.mu.Unlock()
		}
	}

	// Prefer per-call usage from last assistant message; fall back to
	// result usage (which is cumulative) if no assistant messages seen.
	var turnUsage *delegator.TurnUsage
	if lastUsage != nil {
		turnUsage = &delegator.TurnUsage{
			InputTokens:              lastUsage.InputTokens,
			OutputTokens:             lastUsage.OutputTokens,
			CacheCreationInputTokens: lastUsage.CacheCreationInputTokens,
			CacheReadInputTokens:     lastUsage.CacheReadInputTokens,
		}
	} else {
		turnUsage = &delegator.TurnUsage{
			InputTokens:              msg.Usage.InputTokens,
			OutputTokens:             msg.Usage.OutputTokens,
			CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
		}
	}

	result := &delegator.TurnResult{
		Text:      text,
		Model:     resultModel,
		ToolCalls: turnTools,
		Usage:     turnUsage,
	}

	// Pre-answer nudge gate: give the caller a chance to re-dispatch this
	// turn with a verification prompt before finalising. When the func
	// returns a non-empty follow-up, the result is swallowed, beginTurn
	// is called again with the same TurnEvents, and the follow-up is sent
	// as a new user message — explicitly starting a fresh CC ask(). The
	// next OnResult delivers the revised answer as the authoritative
	// outcome. The caller must stop returning a follow-up after the first
	// fire to break the loop (guaranteed by the scheduler's internal
	// state — CheckPreAnswer returns the same text every call but the
	// turn_delegated closure tracks "fired" locally). This is distinct
	// from the mid-turn-drain path used by SourceUser/Steer/post-tool
	// nudges: pre-answer needs a fresh ask() because it's a verification
	// re-prompt, not a fold-in.
	if turn != nil && turn.PreAnswerNudgeFunc != nil {
		if followUp := turn.PreAnswerNudgeFunc(result); followUp != "" {
			b.beginTurn(turn)
			if err := b.writer.SendUser(followUp); err != nil {
				b.logger().Errorf("pre-answer re-dispatch: send user: %v", err)
				b.cancelTurn()
				// Fall through to the normal completion path so the
				// first-round result is still delivered on failure.
			} else {
				if b.typingFunc != nil {
					b.typingFunc(true)
				}
				// Restart the idle clock; the second round is an active
				// continuation, not a completed turn.
				b.touchActivity()
				return
			}
		}
	}

	// Normal turn completion — clear TurnEvents. SessionEvents stay live for
	// the rest of the session so any post-turn text (e.g. CC running a
	// follow-up ask() from a folded steer) still delivers cleanly.
	b.turnMu.Lock()
	hadTurn := b.turnEvents != nil
	b.turnEvents = nil
	b.turnActive = false
	b.turnMu.Unlock()
	b.logger().Debugf("OnResult: turn cleared (had_turn_events=%v)", hadTurn)

	// Clear any agents still tracked (safety net — task_notification should
	// have already removed them individually during the turn).
	b.agents.ClearAll()

	// Fire OnTurnComplete OUTSIDE any lock.
	if turn != nil && turn.OnTurnComplete != nil {
		turn.OnTurnComplete(result)
	}

	// Stop typing indicator.
	if b.typingFunc != nil {
		b.typingFunc(false)
	}

	// Signal WaitForTurn (non-blocking).
	if resultCh != nil {
		select {
		case resultCh <- msg:
		default:
		}
	}
}

// OnSystem handles system messages (init, status, compact_boundary, etc.).
func (b *Backend) OnSystem(subtype string, raw json.RawMessage) {
	b.touchActivity()
	switch subtype {
	case "init":
		var init InitMessage
		if err := json.Unmarshal(raw, &init); err != nil {
			return
		}
		b.mu.Lock()
		b.sessionID = init.SessionID
		b.initMsg = &init
		b.lastModel = init.Model
		b.mu.Unlock()
		b.readyOnce.Do(func() { close(b.readyCh) })
		if b.onSessionReady != nil {
			b.onSessionReady(init.SessionID)
		}

	case "status":
		var status StatusMessage
		if err := json.Unmarshal(raw, &status); err != nil {
			return
		}
		if status.Status != nil && *status.Status == "compacting" {
			if b.onCompactionStart != nil {
				b.onCompactionStart()
			}
			// Signal any armed compaction start waiter (one-shot).
			b.turnMu.Lock()
			sch := b.compactStartCh
			b.compactStartCh = nil
			b.turnMu.Unlock()
			if sch != nil {
				select {
				case sch <- struct{}{}:
				default:
				}
			}
		}

	case "compact_boundary":
		var cb CompactBoundaryMessage
		if err := json.Unmarshal(raw, &cb); err != nil {
			return
		}
		if b.onCompactionDone != nil {
			b.onCompactionDone(cb.CompactMetadata.PreTokens)
		}
		// Signal any armed compaction waiter (one-shot; clear after firing).
		b.turnMu.Lock()
		ch := b.compactDoneCh
		b.compactDoneCh = nil
		b.turnMu.Unlock()
		if ch != nil {
			select {
			case ch <- struct{}{}:
			default:
			}
		}

	case "session_state_changed":
		var ss SessionStateMessage
		_ = json.Unmarshal(raw, &ss)

	case "task_started", "task_progress", "task_notification":
		var task TaskEvent
		if err := json.Unmarshal(raw, &task); err != nil {
			return
		}
		switch subtype {
		case "task_notification":
			if task.Status == "completed" {
				// Remove one pending agent. If the tracker had nothing
				// (e.g. tool_use detection missed it), fire a standalone
				// notification as fallback.
				if !b.agents.RemoveOne() && b.agents.OnStatus != nil {
					b.agents.OnStatus(fmt.Sprintf("✅ Task complete: %s", task.Summary))
				}
			}
		}

	case "api_retry":
		// CC handles its own API retries internally; we parse the message
		// for symmetry with the protocol but do not surface it to the user.
		// The turnevent.RetryNotice / RetrySuccess UI is for the API tool
		// loop's own retries, which don't apply when CC owns inference.
		var retry APIRetryMessage
		if err := json.Unmarshal(raw, &retry); err != nil {
			return
		}
		_ = retry

	case "hook_response":
		// PostToolUse / PostToolUseFailure hook completions. Parsed and
		// dispatched to the current turn's EventHandler.OnToolEnd via the
		// helper defined in hooks.go.
		b.handleHookResponse(raw)

	case "elicitation_complete":
		// CC re-broadcasts an MCP server's elicitation_complete notification
		// when a URL-mode flow was completed externally. Match by
		// elicitation_id and auto-accept so the user doesn't have to click
		// Done after already finishing in the browser.
		var done ElicitationCompleteMessage
		if err := json.Unmarshal(raw, &done); err != nil {
			return
		}
		b.OnElicitationComplete(&done)
	}
}

// OnPermissionRequest handles can_use_tool control requests from CC.
// Dispatches to tool-specific handlers (e.g. AskUserQuestion) or the
// standard permission prompt flow.
func (b *Backend) OnPermissionRequest(msg *PermissionRequest) {
	b.touchActivity()
	b.handleToolRequest(msg)
}

// OnControlResponse handles responses to our control requests (e.g. initialize,
// get_context_usage). Routes to pending waiters by request_id.
//
// For fresh sessions (no --resume), CC responds to the initialize control
// request with a control_response rather than emitting a system/init message.
// When we detect the initialize response, we close readyCh so WaitReady
// unblocks.
func (b *Backend) OnControlResponse(raw json.RawMessage) {
	b.touchActivity()
	var env controlResponseInbound
	if err := json.Unmarshal(raw, &env); err != nil {
		log.Debugf("ccstream", "unmarshal control_response: %v", err)
		return
	}
	reqID := env.Response.RequestID
	if reqID == "" {
		return
	}

	// Check if this is the response to our initialize request.
	b.mu.Lock()
	isInit := b.initReqID != "" && reqID == b.initReqID
	if isInit {
		b.initReqID = "" // consume — only match once
	}
	b.mu.Unlock()
	if isInit {
		b.readyOnce.Do(func() { close(b.readyCh) })
	}

	b.pendingControlMu.Lock()
	ch, ok := b.pendingControls[reqID]
	if ok {
		delete(b.pendingControls, reqID)
	}
	b.pendingControlMu.Unlock()
	if ok {
		select {
		case ch <- raw:
		default:
		}
	}
}

// OnControlCancelRequest handles CC cancelling a pending control request.
func (b *Backend) OnControlCancelRequest(reqID string) {
	b.touchActivity()
	b.handleControlCancel(reqID)
}

// OnKeepAlive handles heartbeat events. Touches activity so the idle/timeout
// tracker sees the stream as alive during periods where CC is blocked (e.g.
// waiting for a permission prompt response) and not emitting work events.
//
// NOTE: As of CC 1.x, keep_alive frames are only sent on WebSocket transports
// (remote control sessions). In --pipe mode (stdin/stdout, which foci uses),
// CC never sends keep_alive — so this handler is effectively dead code.
// The idle tracker must be kept alive by other means (e.g. touchActivity on
// permission request arrival). See also runKeepAlive which sends keep_alive
// TO CC (also a no-op: CC silently ignores them in pipe mode).
func (b *Backend) OnKeepAlive() {
	b.touchActivity()
}

// OnRateLimit handles rate limit events from CC's stdout.
func (b *Backend) OnRateLimit(msg *RateLimitEvent) {
	b.touchActivity()
	if b.rateLimitState != nil {
		b.rateLimitState.Update(&msg.RateLimitInfo)
	}
}

// OnToolProgress handles heartbeats during long-running tool execution.
func (b *Backend) OnToolProgress(msg *ToolProgressMessage) {
	b.touchActivity()
	// Keep typing indicator alive during tool execution.
	if b.typingFunc != nil {
		b.typingFunc(true)
	}
}

// OnStreamEvent handles token-level streaming events. CC wraps Anthropic
// SDK stream parts in these envelopes (services/api/claude.ts:2300), so the
// event payload is a verbatim SDK `content_block_delta` with subtypes like
// `text_delta` and `thinking_delta` that we extract separately.
//
// Sub-agent stream events (ParentToolUseID != nil) are filtered out, matching
// the guard in OnAssistant. Sub-agent text is delivered as complete blocks
// (blockquoted) via OnAssistant instead. Without this filter, sub-agent
// deltas leak into the parent turn's StreamWriter — accumulating text that
// is never Finish()ed by OnReply, which corrupts the parent's stream message
// and silently discards the parent's reply text.
func (b *Backend) OnStreamEvent(raw json.RawMessage) {
	b.touchActivity()
	var env struct {
		ParentToolUseID *string `json:"parent_tool_use_id,omitempty"`
		Event           struct {
			Type  string `json:"type"`
			Delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		} `json:"event"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Event.Type != "content_block_delta" {
		return
	}
	if env.ParentToolUseID != nil {
		return
	}
	// Deltas route through SessionEvents so they survive across stacked
	// turns / post-OnResult emission, same reasoning as OnAssistant text.
	se := b.sessionEvents.Load()
	if se == nil {
		return
	}
	switch env.Event.Delta.Type {
	case "text_delta":
		if env.Event.Delta.Text != "" && se.OnTextDelta != nil {
			se.OnTextDelta(env.Event.Delta.Text)
		}
	case "thinking_delta":
		if env.Event.Delta.Thinking != "" && se.OnThinkingDelta != nil {
			se.OnThinkingDelta(env.Event.Delta.Thinking)
		}
	}
}

// OnReaderStopped handles the reader goroutine exiting for any reason, including
// expected shutdown (Close), clean process exit (io.EOF), or unexpected errors
// (broken pipe, scanner errors). It logs the reader's observation, then defers
// the actual cleanup to finalizeExit so the work runs exactly once even if the
// waiter goroutine (cmd.Wait) reached the same conclusion first.
func (b *Backend) OnReaderStopped(err error) {
	component := b.logComponent()

	b.mu.Lock()
	expected := b.closing
	b.mu.Unlock()

	if expected {
		log.Infof(component, "subprocess reader stopped (session closing)")
	} else {
		log.Warnf(component, "subprocess reader stopped: %v", err)
	}

	b.finalizeExit(err)
}

// finalizeExit performs the one-shot cleanup when the CC subprocess has died.
//
// Two independent goroutines can observe process death: the waiter goroutine
// (cmd.Wait returns) and the reader goroutine (scanner EOF / read error / ctx
// cancel). Historically only OnReaderStopped did the cleanup, so any failure
// mode that caused the reader to exit silently (ctx cancelled, partial-line
// read stuck, etc.) left the backend wedged: `running=true` so DelegatedManager
// kept handing back the corpse, and any in-flight turn handler hung on
// CompletionChan forever.
//
// finalizeExit is gated by sync.Once so whichever path notices first wins and
// the other becomes a no-op. The waiter goroutine is the authoritative source
// of truth (cmd.Wait cannot lie), but OnReaderStopped also calls in for the
// case where the reader sees EOF before cmd.Wait has returned.
//
// Reset in Restart() before the subprocess is relaunched.
func (b *Backend) finalizeExit(reason error) {
	b.finalizeOnce.Do(func() {
		component := b.logComponent()

		// Instrumentation: bracket the cleanup so we can see whether it
		// completed and where time goes when the waiter-goroutine
		// signalling chain stalls (TODO #749). Should be sub-millisecond
		// in the happy path; >1s indicates a callback or lock issue.
		start := time.Now()
		log.Debugf(component, "finalizeExit: enter reason=%v", reason)
		defer func() {
			log.Debugf(component, "finalizeExit: exit elapsed=%s", time.Since(start))
		}()

		b.mu.Lock()
		expected := b.closing
		b.running = false
		b.mu.Unlock()
		log.Debugf(component, "finalizeExit: post-mu elapsed=%s", time.Since(start))

		// If the waiter goroutine has set exitErr, prefer its detail for the
		// user-visible message. Wait briefly in case finalizeExit was invoked
		// from the reader path before cmd.Wait returned. exitCh is closed by
		// the waiter goroutine immediately after exitErr is assigned, so this
		// doesn't race. exitCh is nil when finalizeExit is called from a unit
		// test that bypasses Start; skip the wait in that case.
		if b.exitCh != nil {
			select {
			case <-b.exitCh:
			case <-time.After(2 * time.Second):
				log.Debugf(component, "finalizeExit: exitCh wait timed out (waiter goroutine has not set exitErr) elapsed=%s", time.Since(start))
			}
		}
		log.Debugf(component, "finalizeExit: post-exitCh-wait elapsed=%s", time.Since(start))

		if !expected && b.exitErr != nil {
			log.Warnf(component, "process exit detail: %s", describeExitError(b.exitErr))
		}

		// Drain any in-flight turn so callers waiting on CompletionChan or
		// WaitForTurn don't block forever.
		b.turnMu.Lock()
		turn := b.turnEvents
		b.turnEvents = nil
		b.turnActive = false
		resultCh := b.turnResultCh
		b.turnMu.Unlock()
		log.Debugf(component, "finalizeExit: post-turnMu turn_nil=%v turn_otc_nil=%v elapsed=%s", turn == nil, turn == nil || turn.OnTurnComplete == nil, time.Since(start))

		if turn != nil && turn.OnTurnComplete != nil {
			var msg string
			if expected {
				msg = "Session closed while turn was in flight"
			} else {
				msg = fmt.Sprintf("Error: CC process exited unexpectedly: %v", reason)
				if b.exitErr != nil && b.exitErr != reason {
					msg += " (" + describeExitError(b.exitErr) + ")"
				}
			}
			log.Debugf(component, "finalizeExit: pre-OnTurnComplete elapsed=%s", time.Since(start))
			turn.OnTurnComplete(&delegator.TurnResult{Text: msg})
			log.Debugf(component, "finalizeExit: post-OnTurnComplete elapsed=%s", time.Since(start))
		}

		if b.typingFunc != nil {
			log.Debugf(component, "finalizeExit: pre-typingFunc(false) elapsed=%s", time.Since(start))
			b.typingFunc(false)
			log.Debugf(component, "finalizeExit: post-typingFunc(false) elapsed=%s", time.Since(start))
		}

		// Unblock WaitForTurn.
		if resultCh != nil {
			select {
			case resultCh <- &ResultMessage{Subtype: "error_during_execution", IsError: true}:
			default:
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Background goroutines
// ---------------------------------------------------------------------------

// runKeepAlive sends periodic keep-alive messages to CC's stdin.
//
// NOTE: As of CC 1.x, CC silently ignores keep_alive messages in --pipe mode
// (structuredIO.ts drops them). This goroutine runs but has no observable
// effect. The original intent was to prevent idle timeout, but CC's pipe
// transport has no idle timeout to prevent. Kept for forward-compatibility
// in case CC adds pipe-mode keepalive handling.
func (b *Backend) runKeepAlive(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := b.writer.SendKeepAlive(); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// captureStderr reads CC's stderr line by line and logs it. CC's stderr
// can contain progress info, warnings, and errors. Lines containing "error"
// or "fatal" are logged at warn level; everything else at debug.
//
// Buffer matches the stdout reader's 1MB cap (reader.go maxTokenSize) so
// a misbehaving subprocess writing one huge stderr line doesn't silently
// stall its own stderr pipe: bufio.Scanner with default 64KB buffer
// would return ErrTooLong and this goroutine would exit, causing the pipe
// to fill and the subprocess to block on its next stderr write — wedging
// the whole turn before stdout ever delivered a single envelope.
func (b *Backend) captureStderr(r io.Reader) {
	component := b.logComponent()
	scanner := bufio.NewScanner(r)
	const maxLine = 1 << 20 // 1MB — matches stdout reader cap
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "fatal") || strings.Contains(lower, "panic") {
			log.Warnf(component, "stderr: %s", line)
		} else {
			log.Debugf(component, "stderr: %s", line)
		}
	}
	// Surface scanner-level errors (e.g. ErrTooLong on a >1MB line). Without
	// this the goroutine exited silently and the subprocess's stderr pipe
	// would back up. EOF is the normal exit when the subprocess closes
	// stderr — don't warn on that.
	if err := scanner.Err(); err != nil {
		log.Warnf(component, "stderr capture stopped: %v", err)
	}
}

// logComponent returns the log component string for this backend.
func (b *Backend) logComponent() string {
	if b.label != "" {
		return "ccstream:" + b.label
	}
	return "ccstream"
}

// describeExitError returns a human-readable description of a process exit
// error including exit code, signal, and stderr snippet when available.
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

	// Include a stderr snippet if the ExitError captured any.
	if len(exitErr.Stderr) > 0 {
		snippet := string(exitErr.Stderr)
		if len(snippet) > 512 {
			snippet = snippet[:512] + "…"
		}
		parts = append(parts, fmt.Sprintf("stderr: %s", snippet))
	}

	if len(parts) == 0 {
		return err.Error()
	}
	return strings.Join(parts, ", ")
}

// blockquote prefixes every line with "> " for markdown blockquote rendering.
func blockquote(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = "> " + line
	}
	return strings.Join(lines, "\n")
}
