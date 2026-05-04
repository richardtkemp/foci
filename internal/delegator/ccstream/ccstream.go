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
)

func init() {
	delegator.Register("claude-code", newFromConfig)
}

func newFromConfig(cfg map[string]any) (delegator.Delegator, error) {
	b := &Backend{
		readyCh:        make(chan struct{}),
		pendingPerms:   make(map[string]*pendingPermission),
		pendingElicits: make(map[string]*pendingElicitation),
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
	cmd    *exec.Cmd
	writer *Writer
	cancel context.CancelFunc // cancels reader goroutine + keep-alive
	done      chan struct{} // closed when reader goroutine exits
	waitCh    chan error   // receives cmd.Wait() result (reaps zombie)
	exitCh    chan struct{} // closed when exitErr is set
	exitErr   error        // set by waiter goroutine when process exits

	// State
	mu           sync.Mutex
	running      bool
	closing      bool // set by Close() before shutdown; tells OnReaderStopped this is expected
	sessionID    string       // from init message
	initMsg      *InitMessage // from init message
	readyCh      chan struct{} // closed when init received
	readyOnce    sync.Once    // ensures readyCh closed once
	initReqID    string       // request_id of the initialize control request

	// Turn state
	turnMu       sync.Mutex
	turnActive   bool
	turnHandler  *delegator.EventHandler // current turn's handler
	turnResultCh chan *ResultMessage    // buffered(1), receives result
	compactDoneCh  chan struct{}         // buffered(1), armed by ArmCompactionWait; fired on compact_boundary
	compactStartCh chan struct{}         // buffered(1), armed by ArmCompactionStartWait; fired on status="compacting"
	turnText      strings.Builder       // accumulates text across assistant messages
	turnTools     int                   // tool_use count this turn
	nudgePending  bool                  // set when PostToolNudge sends PriorityNow; cleared on next OnResult or beginTurn
	steerInjected bool                  // set when checkAndSendSteers sends PriorityNow; cleared on next OnResult or beginTurn. Triggers handler re-arm so the steered response isn't dropped after CC's abort-result fires (TODO #726).
	followUpQueued bool                 // set when SendCommand sends a follow-up via priority="next" (RunInference's IsTurnInFlight branch). Cleared on next OnResult or beginTurn. Triggers handler re-arm so the follow-up's response isn't dropped after the original turn's result clears the handler (TODO #726 sub-mode 3).

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
	permPromptFn       delegator.PermissionPromptFunc
	permCancelFn       func(requestID, toolName, reason string)
	onPermCleared      func()
	onPermPending      func()
	onSessionReady     func(sessionID string)
	typingFunc         func(typing bool)
	onCompactionStart  func()            // fired when status="compacting"
	onCompactionDone   func(preTokens int) // fired on compact_boundary
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

	log.Infof(component, "launching: claude %s (workdir=%s)", strings.Join(args, " "), opts.WorkDir)

	// Create command with its own cancellable context. The CC process is
	// long-lived (surviving across turns), so it must NOT be tied to the
	// caller's context — otherwise the process is killed when the turn
	// context expires or is cancelled.
	cmdCtx, cmdCancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(cmdCtx, "claude", args...)
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
	component := b.logComponent()
	select {
	case <-b.waitCh:
		// Process already exited (or just did).
	case <-time.After(5 * time.Second):
		// SIGTERM.
		log.Warnf(component, "process did not exit after 5s, sending SIGTERM")
		if b.cmd.Process != nil {
			_ = b.cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case <-b.waitCh:
		case <-time.After(2 * time.Second):
			// SIGKILL.
			log.Warnf(component, "process did not exit after SIGTERM, sending SIGKILL")
			if b.cmd.Process != nil {
				_ = b.cmd.Process.Kill()
			}
			<-b.waitCh
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
	b.mu.Lock()
	b.initReqID = ""
	b.mu.Unlock()

	b.permMu.Lock()
	b.pendingPerms = make(map[string]*pendingPermission)
	b.permMu.Unlock()

	b.elicMu.Lock()
	b.pendingElicits = make(map[string]*pendingElicitation)
	b.elicMu.Unlock()

	return b.Start(ctx, b.startOpts)
}

// IsRunning reports whether the Claude Code subprocess is alive.
func (b *Backend) IsRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// WaitReady blocks until the init message is received from CC.
func (b *Backend) WaitReady(ctx context.Context) error {
	select {
	case <-b.readyCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// Turn methods
// ---------------------------------------------------------------------------

// beginTurn initialises all turn-related state for a new turn.
func (b *Backend) beginTurn(handler *delegator.EventHandler) {
	b.turnMu.Lock()
	b.turnActive = true
	b.turnHandler = handler
	b.turnText.Reset()
	b.turnTools = 0
	b.nudgePending = false
	b.steerInjected = false
	b.followUpQueued = false
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
	b.turnHandler = nil
	b.turnMu.Unlock()
}

// rearmForNudgeResponse re-arms the turn with a delivery-only handler
// derived from the original. The nudge turn delivers text and tracks
// tools normally, but its OnTurnComplete is nil — it doesn't signal
// the foci turn (which already completed on the prior result).
// PostToolNudgeFunc is preserved so chained nudges work correctly.
func (b *Backend) rearmForNudgeResponse(orig *delegator.EventHandler) {
	log.Debugf("ccstream", "rearmForNudgeResponse: installing nudge handler OnText=%v OnTurnComplete=nil(intentional) OnToolStart=%v OnToolEnd=%v PostToolNudgeFunc=%v SteerCheckFunc=%v",
		orig.OnText != nil, orig.OnToolStart != nil, orig.OnToolEnd != nil, orig.PostToolNudgeFunc != nil, orig.SteerCheckFunc != nil)
	b.turnMu.Lock()
	b.turnActive = true
	b.turnHandler = &delegator.EventHandler{
		OnTextDelta:       orig.OnTextDelta,
		OnThinkingDelta:   orig.OnThinkingDelta,
		OnText:            orig.OnText,
		OnToolStart:       orig.OnToolStart,
		OnToolEnd:         orig.OnToolEnd,
		PostToolNudgeFunc: orig.PostToolNudgeFunc,
		SteerCheckFunc:    orig.SteerCheckFunc,
		// OnTurnComplete: nil — nudge CC turn doesn't end the foci turn.
		// PreAnswerNudgeFunc: nil — pre-answer gate doesn't apply to nudge turns.
	}
	b.turnText.Reset()
	b.turnTools = 0
	b.nudgePending = false
	b.steerInjected = false
	b.followUpQueued = false
	b.turnResultCh = make(chan *ResultMessage, 1)
	b.turnMu.Unlock()

	b.mu.Lock()
	b.lastUsage = nil
	b.mu.Unlock()

	b.touchActivity()
}

// SendToPane sends a composed prompt to Claude Code and streams events back
// via the handler. Returns immediately — the turn completes asynchronously.
// Use WaitForTurn to block until the result is received.
func (b *Backend) SendToPane(ctx context.Context, prompt string, handler *delegator.EventHandler) (*delegator.TurnResult, error) {
	b.beginTurn(handler)

	if b.typingFunc != nil {
		b.typingFunc(true)
	}

	b.logger().Debugf("SendToPane: calling writer.SendUser (%d bytes)", len(prompt))
	sendStart := time.Now()
	if err := b.writer.SendUser(prompt); err != nil {
		b.cancelTurn()
		return nil, fmt.Errorf("ccstream: send user message: %w", err)
	}
	if elapsed := time.Since(sendStart); elapsed > 5*time.Second {
		b.logger().Warnf("SendToPane: writer.SendUser took %s (slow — possible mutex contention or blocked stdin)", elapsed.Round(time.Millisecond))
	} else {
		b.logger().Debugf("SendToPane: writer.SendUser returned in %s", elapsed.Round(time.Millisecond))
	}

	return &delegator.TurnResult{}, nil
}

// SendToPaneWithAttachments sends a prompt with file attachments as structured
// content blocks. Images become "image" blocks, PDFs become "document" blocks,
// all alongside the text prompt. This preserves binary data through to CC
// instead of flattening to text.
func (b *Backend) SendToPaneWithAttachments(ctx context.Context, prompt string, attachments []delegator.Attachment, handler *delegator.EventHandler) (*delegator.TurnResult, error) {
	b.beginTurn(handler)

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

	b.logger().Debugf("SendToPaneWithAttachments: calling writer.Send (%d blocks)", len(blocks))
	sendStart := time.Now()
	if err := b.writer.Send(NewUserMessageBlocks(blocks)); err != nil {
		b.cancelTurn()
		return nil, fmt.Errorf("ccstream: send user message with attachments: %w", err)
	}
	if elapsed := time.Since(sendStart); elapsed > 5*time.Second {
		b.logger().Warnf("SendToPaneWithAttachments: writer.Send took %s (slow)", elapsed.Round(time.Millisecond))
	} else {
		b.logger().Debugf("SendToPaneWithAttachments: writer.Send returned in %s", elapsed.Round(time.Millisecond))
	}

	return &delegator.TurnResult{}, nil
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

// SendCommand sends a slash command or steered message directly to CC.
// priority controls CC's processing order — use PriorityNow for steer
// messages that should interrupt tool execution, or "" for default.
//
// Priority "next" is used by RunInference's follow-up path (turn_delegated.go)
// when IsTurnInFlight is true: the new user message is queued behind the
// in-flight turn rather than starting a fresh turn pipeline. Mark
// followUpQueued so OnResult re-arms the handler instead of clearing it —
// without this, the follow-up's response text hits b.turnHandler == nil
// and gets silently dropped if an unrelated OnResult (e.g. a nudge response
// completing) fires between SendCommand and CC processing the follow-up.
// See TODO #726 sub-mode 3.
func (b *Backend) SendCommand(ctx context.Context, command string, priority string) error {
	if priority != "" {
		err := b.writer.SendUserWithPriority(command, priority)
		if err == nil && priority == "next" {
			b.turnMu.Lock()
			b.followUpQueued = true
			b.turnMu.Unlock()
		}
		return err
	}
	return b.writer.SendUser(command)
}

// checkAndSendSteers drains the handler's SteerCheckFunc and sends any
// pending steer messages to CC with "now" priority. Called at tool execution
// boundaries (after tool_use blocks, during tool progress) so that steered
// messages buffered by the platform MessageQueue are injected mid-turn
// rather than waiting for the turn to complete.
func (b *Backend) checkAndSendSteers() {
	b.turnMu.Lock()
	handler := b.turnHandler
	b.turnMu.Unlock()
	if handler == nil || handler.SteerCheckFunc == nil {
		return
	}
	steers := handler.SteerCheckFunc()
	for _, text := range steers {
		if text == "" {
			continue
		}
		// Surface PriorityNow steer sends — these are documented to
		// "interrupt the current operation (aborts tool execution)" on
		// the CC side, so they're a leading suspect when a permission
		// is auto-cancelled (handleControlCancel) without user input.
		preview := text
		if len(preview) > 80 {
			preview = preview[:80] + "..."
		}
		log.Debugf("ccstream/steer", "sending PriorityNow steer to CC: bytes=%d preview=%q", len(text), preview)
		if err := b.writer.SendUserWithPriority("[user] "+text, PriorityNow); err == nil {
			// PriorityNow aborts CC's in-flight tool execution and triggers
			// a result message for the cancelled turn. Mark the turn so
			// OnResult re-arms the handler instead of clearing it — the
			// steered response that follows must reach OnText.
			b.turnMu.Lock()
			b.steerInjected = true
			b.turnMu.Unlock()
		}
	}
}

// ---------------------------------------------------------------------------
// Callback setters
// ---------------------------------------------------------------------------

// SetPermissionPromptFunc sets the function used to send permission prompts.
func (b *Backend) SetPermissionPromptFunc(fn delegator.PermissionPromptFunc) { b.permPromptFn = fn }

// SetOnPermissionCleared sets a callback fired when permissions are resolved.
func (b *Backend) SetOnPermissionCleared(fn func()) { b.onPermCleared = fn }

// SetOnPermissionCancelled sets a callback fired when a specific pending
// permission is cleared by CC's control_cancel_request (e.g. a PriorityNow
// steer aborted the in-flight tool execution). Distinct from
// SetOnPermissionCleared, which fires only when the *last* pending
// permission goes away — this fires for each individual cancellation,
// allowing the platform layer to update per-prompt UI state.
func (b *Backend) SetOnPermissionCancelled(fn func(requestID, toolName, reason string)) {
	b.permCancelFn = fn
}

// SetOnPermissionPending sets a callback fired when a new permission is pending.
func (b *Backend) SetOnPermissionPending(fn func()) { b.onPermPending = fn }

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

	b.turnMu.Lock()
	handler := b.turnHandler
	b.turnMu.Unlock()

	if !isTopLevel {
		// Surface sub-agent text as blockquoted intermediate replies so
		// the user can follow sub-agent progress. Tool_use blocks are not
		// forwarded — the parent tracker owns tool visibility.
		if handler != nil && handler.OnText != nil {
			for _, block := range msg.Message.Content {
				if block.Type == "text" && block.Text != "" {
					handler.OnText(blockquote(block.Text))
				}
			}
		}
		// Keep typing indicator alive during sub-agent work.
		if b.typingFunc != nil {
			b.typingFunc(true)
		}
		return
	}

	hasToolUse := false
	for _, block := range msg.Message.Content {
		switch block.Type {
		case "text":
			b.turnMu.Lock()
			b.turnText.WriteString(block.Text)
			b.turnMu.Unlock()

			if handler != nil && handler.OnText != nil {
				handler.OnText(block.Text)
			} else if block.Text != "" {
				// Top-level text block produced but no handler is
				// armed (or has no OnText) — text is silently dropped
				// from the delivery path. This is the failure shape
				// observed in TODO #726. Loud so it's caught quickly.
				preview := block.Text
				if len(preview) > 120 {
					preview = preview[:120] + "..."
				}
				log.Warnf("ccstream", "text block dropped (no handler/OnText): bytes=%d handler_nil=%v preview=%q",
					len(block.Text), handler == nil, preview)
			}

		case "tool_use":
			hasToolUse = true
			b.turnMu.Lock()
			b.turnTools++
			b.turnMu.Unlock()

			if handler != nil && handler.OnToolStart != nil {
				inputStr := string(block.Input)
				handler.OnToolStart(block.ID, block.Name, inputStr)
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

	// Check for steer messages after processing tool_use blocks. CC is about
	// to execute tools — this is the natural injection point for redirecting
	// the agent mid-turn.
	if hasToolUse {
		b.checkAndSendSteers()
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

	// Capture turn state. Handler clearing is deferred — the nudge re-arm
	// and pre-answer gate paths need the handler alive to re-arm or fire
	// OnTurnComplete. The normal path clears handler/turnActive below.
	b.turnMu.Lock()
	handler := b.turnHandler
	nudgePending := b.nudgePending
	b.nudgePending = false
	steerInjected := b.steerInjected
	b.steerInjected = false
	followUpQueued := b.followUpQueued
	b.followUpQueued = false
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

	// Post-tool nudge re-arm: a nudge was injected via PriorityNow but CC
	// ended the turn before processing it. CC will process the nudge as a
	// new CC-internal turn. Complete the foci turn normally (fire
	// OnTurnComplete), then re-arm with a delivery-only handler so the
	// nudge response reaches the platform.
	if nudgePending && handler != nil {
		b.agents.ClearAll()
		if handler.OnTurnComplete != nil {
			handler.OnTurnComplete(result)
		}
		if b.typingFunc != nil {
			b.typingFunc(true)
		}
		b.rearmForNudgeResponse(handler)
		b.logger().Infof("OnResult: re-armed for pending nudge response")
		if resultCh != nil {
			select {
			case resultCh <- msg:
			default:
			}
		}
		return
	}

	// Steer-injected re-arm: a steer was sent via PriorityNow during this
	// turn. CC aborted the in-flight work and emitted this result; CC will
	// then process the steered message and produce more output. Without
	// re-arming, that output's text blocks hit b.turnHandler == nil and
	// get silently dropped (TODO #726). Complete the foci turn normally
	// (fire OnTurnComplete with whatever text accumulated before the
	// abort), then re-arm with a delivery-only handler so the steered
	// response reaches the platform via OnText.
	if steerInjected && handler != nil {
		b.agents.ClearAll()
		if handler.OnTurnComplete != nil {
			handler.OnTurnComplete(result)
		}
		if b.typingFunc != nil {
			b.typingFunc(true)
		}
		b.rearmForNudgeResponse(handler)
		b.logger().Infof("OnResult: re-armed for steered response (text=%d bytes)", len(text))
		if resultCh != nil {
			select {
			case resultCh <- msg:
			default:
			}
		}
		return
	}

	// Follow-up re-arm: SendCommand queued a follow-up message via
	// priority="next" while a turn was in-flight. CC will process the
	// follow-up after the current result and produce more output. Without
	// re-arming, the follow-up's text blocks hit b.turnHandler == nil
	// when this OnResult clears the handler, and they're silently dropped
	// (TODO #726 sub-mode 3 — observed live with scout 2026-04-30 20:07).
	// Same shape as the nudge / steer re-arm: fire OnTurnComplete on this
	// result, then re-arm with a delivery-only handler so the follow-up
	// response reaches the platform via OnText.
	if followUpQueued && handler != nil {
		b.agents.ClearAll()
		if handler.OnTurnComplete != nil {
			handler.OnTurnComplete(result)
		}
		if b.typingFunc != nil {
			b.typingFunc(true)
		}
		b.rearmForNudgeResponse(handler)
		b.logger().Infof("OnResult: re-armed for queued follow-up response (text=%d bytes)", len(text))
		if resultCh != nil {
			select {
			case resultCh <- msg:
			default:
			}
		}
		return
	}

	// Pre-answer nudge gate: give the caller a chance to re-dispatch this
	// turn with a verification prompt before finalising. When the func
	// returns a non-empty follow-up, the result is swallowed, turn state
	// is re-armed under the SAME handler, and the follow-up is sent as a
	// new user message. The next OnResult delivers the revised answer as
	// the authoritative outcome. The caller must stop returning a follow-up
	// after the first fire to break the loop (guaranteed by the scheduler's
	// internal state — CheckPreAnswer returns the same text every call but
	// the turn_delegated closure tracks "fired" locally).
	if handler != nil && handler.PreAnswerNudgeFunc != nil {
		if followUp := handler.PreAnswerNudgeFunc(result); followUp != "" {
			b.beginTurn(handler)
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

	// Normal turn completion — clear handler.
	b.turnMu.Lock()
	b.turnHandler = nil
	b.turnActive = false
	b.turnMu.Unlock()

	// Clear any agents still tracked (safety net — task_notification should
	// have already removed them individually during the turn).
	b.agents.ClearAll()

	// Fire handler callback OUTSIDE any lock.
	if handler != nil && handler.OnTurnComplete != nil {
		handler.OnTurnComplete(result)
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
	// Check for steer messages during long-running tools. Without this,
	// steers would only be checked between tool batches (OnAssistant).
	b.checkAndSendSteers()
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
	b.turnMu.Lock()
	handler := b.turnHandler
	b.turnMu.Unlock()
	if handler == nil {
		return
	}
	switch env.Event.Delta.Type {
	case "text_delta":
		if env.Event.Delta.Text != "" && handler.OnTextDelta != nil {
			handler.OnTextDelta(env.Event.Delta.Text)
		}
	case "thinking_delta":
		if env.Event.Delta.Thinking != "" && handler.OnThinkingDelta != nil {
			handler.OnThinkingDelta(env.Event.Delta.Thinking)
		}
	}
}

// OnReaderStopped handles the reader goroutine exiting for any reason, including
// expected shutdown (Close), clean process exit (io.EOF), or unexpected errors
// (broken pipe, scanner errors). It marks the backend as dead and completes
// any in-flight turn so callers don't block forever.
func (b *Backend) OnReaderStopped(err error) {
	component := b.logComponent()

	// Check whether Close() initiated this shutdown.
	b.mu.Lock()
	expected := b.closing
	b.running = false
	b.mu.Unlock()

	if expected {
		log.Infof(component, "subprocess reader stopped (session closing)")
	} else {
		log.Warnf(component, "subprocess reader stopped: %v", err)
	}

	// Wait briefly for the waiter goroutine to set exitErr. The process is
	// already dead (we got EOF/error on stdout), so cmd.Wait should return
	// almost immediately. exitCh is closed before waitCh is sent to, so
	// this doesn't steal the value that Close() needs.
	select {
	case <-b.exitCh:
	case <-time.After(2 * time.Second):
	}

	if !expected && b.exitErr != nil {
		exitDetail := describeExitError(b.exitErr)
		log.Warnf(component, "process exit detail: %s", exitDetail)
	}

	// If a turn was in-flight, fire OnTurnComplete with an error indication
	// so the caller doesn't block forever on CompletionChan.
	b.turnMu.Lock()
	handler := b.turnHandler
	b.turnHandler = nil
	b.turnActive = false
	resultCh := b.turnResultCh
	b.turnMu.Unlock()

	if handler != nil && handler.OnTurnComplete != nil {
		var msg string
		if expected {
			msg = "Session closed while turn was in flight"
		} else {
			msg = fmt.Sprintf("Error: CC process exited unexpectedly: %v", err)
			if b.exitErr != nil {
				msg += " (" + describeExitError(b.exitErr) + ")"
			}
		}
		handler.OnTurnComplete(&delegator.TurnResult{
			Text: msg,
		})
	}

	if b.typingFunc != nil {
		b.typingFunc(false)
	}

	// Unblock WaitForTurn.
	if resultCh != nil {
		select {
		case resultCh <- &ResultMessage{Subtype: "error_during_execution", IsError: true}:
		default:
		}
	}
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
func (b *Backend) captureStderr(r io.Reader) {
	component := b.logComponent()
	scanner := bufio.NewScanner(r)
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
