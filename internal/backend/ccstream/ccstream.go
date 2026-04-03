// Package ccstream implements a Claude Code backend using the stream-json
// NDJSON protocol (--input-format stream-json --output-format stream-json).
// This replaces the tmux-based backend with structured stdin/stdout
// communication — no pane management, no screen scraping, no JSONL file watching.
package ccstream

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"foci/internal/backend"
	"foci/internal/log"
)

func init() {
	backend.Register("claude-code", newFromConfig)
}

func newFromConfig(cfg map[string]any) (backend.Backend, error) {
	b := &Backend{
		readyCh:      make(chan struct{}),
		pendingPerms: make(map[string]*pendingPermission),
	}
	b.cfg = cfg
	return b, nil
}

// Backend implements backend.Backend using Claude Code's stream-json
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
	startOpts    backend.StartOptions // saved for Restart

	// Process
	cmd    *exec.Cmd
	writer *Writer
	cancel context.CancelFunc // cancels reader goroutine + keep-alive
	done   chan struct{}       // closed when reader goroutine exits
	waitCh chan error          // receives cmd.Wait() result (reaps zombie)

	// State
	mu        sync.Mutex
	running   bool
	sessionID string       // from init message
	initMsg   *InitMessage // from init message
	readyCh   chan struct{} // closed when init received
	readyOnce sync.Once    // ensures readyCh closed once

	// Turn state
	turnMu       sync.Mutex
	turnActive   bool
	turnHandler  *backend.EventHandler // current turn's handler
	turnResultCh chan *ResultMessage    // buffered(1), receives result
	turnText     strings.Builder       // accumulates text across assistant messages
	turnTools    int                   // tool_use count this turn

	// Permissions
	permMu       sync.Mutex
	pendingPerms map[string]*pendingPermission

	// Context tracking (from result/assistant messages)
	contextWindow int         // from modelUsage.contextWindow
	lastModel     string      // from assistant message
	lastUsage     *TokenUsage // per-call usage from last assistant message

	// Auto-approve rules (compiled from config, immutable after Start)
	autoApproveRules []autoApproveRule

	// Agent tracking (shared with tmux backend via AgentTracker).
	agents backend.AgentTracker

	// Callbacks (set before Start, read-only after)
	replyFunc          backend.ReplyFunc
	permPromptFn       backend.PermissionPromptFunc
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
func (b *Backend) Start(ctx context.Context, opts backend.StartOptions) error {
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

	// Create command with cancellable context.
	cmdCtx, cmdCancel := context.WithCancel(ctx)
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
	go func() {
		err := cmd.Wait()
		b.waitCh <- err
	}()

	// Send initialize control request with system prompt.
	if err := b.writer.SendControl(newRequestID(), &InitializeRequest{
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
	b.mu.Unlock()

	// Try graceful shutdown: interrupt + close stdin (EOF).
	_ = b.writer.SendInterrupt()
	_ = b.writer.Close()

	// Wait for process exit with timeout. The waiter goroutine (launched in
	// Start) calls cmd.Wait() and sends the result to waitCh. If the process
	// already exited, this returns immediately.
	select {
	case <-b.waitCh:
		// Process already exited (or just did).
	case <-time.After(5 * time.Second):
		// SIGTERM.
		if b.cmd.Process != nil {
			_ = b.cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case <-b.waitCh:
		case <-time.After(2 * time.Second):
			// SIGKILL.
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

	return nil
}

// Restart kills and relaunches the Claude Code subprocess.
func (b *Backend) Restart(ctx context.Context) error {
	_ = b.Close()

	// Reset state for fresh start.
	b.readyCh = make(chan struct{})
	b.readyOnce = sync.Once{}

	b.permMu.Lock()
	b.pendingPerms = make(map[string]*pendingPermission)
	b.permMu.Unlock()

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
func (b *Backend) beginTurn(handler *backend.EventHandler) {
	b.turnMu.Lock()
	b.turnActive = true
	b.turnHandler = handler
	b.turnText.Reset()
	b.turnTools = 0
	b.turnResultCh = make(chan *ResultMessage, 1)
	b.turnMu.Unlock()

	b.mu.Lock()
	b.lastUsage = nil
	b.mu.Unlock()
}

// cancelTurn reverses beginTurn on send failure.
func (b *Backend) cancelTurn() {
	b.turnMu.Lock()
	b.turnActive = false
	b.turnHandler = nil
	b.turnMu.Unlock()
}

// SendToPane sends a composed prompt to Claude Code and streams events back
// via the handler. Returns immediately — the turn completes asynchronously.
// Use WaitForTurn to block until the result is received.
func (b *Backend) SendToPane(ctx context.Context, prompt string, handler *backend.EventHandler) (*backend.TurnResult, error) {
	b.beginTurn(handler)

	if b.typingFunc != nil {
		b.typingFunc(true)
	}

	if err := b.writer.SendUser(prompt); err != nil {
		b.cancelTurn()
		return nil, fmt.Errorf("ccstream: send user message: %w", err)
	}

	return &backend.TurnResult{}, nil
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
func (b *Backend) SendCommand(ctx context.Context, command string, priority string) error {
	if priority != "" {
		return b.writer.SendUserWithPriority(command, priority)
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
		_ = b.writer.SendUserWithPriority("[user] "+text, PriorityNow)
	}
}

// ---------------------------------------------------------------------------
// Callback setters
// ---------------------------------------------------------------------------

// SetReplyFunc sets the function used to deliver text to the user's platform chat.
func (b *Backend) SetReplyFunc(fn backend.ReplyFunc) { b.replyFunc = fn }

// SetPermissionPromptFunc sets the function used to send permission prompts.
func (b *Backend) SetPermissionPromptFunc(fn backend.PermissionPromptFunc) { b.permPromptFn = fn }

// SetOnPermissionCleared sets a callback fired when permissions are resolved.
func (b *Backend) SetOnPermissionCleared(fn func()) { b.onPermCleared = fn }

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

// SetOnAgentStatus sets a callback for agent/task lifecycle events.
func (b *Backend) SetOnAgentStatus(fn func(string)) { b.agents.OnStatus = fn }

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
	return b.SendControl(ctx, &backend.SetModelRequest{Model: model})
}

// ---------------------------------------------------------------------------
// Handler interface implementation (called by Reader goroutine)
// ---------------------------------------------------------------------------

// OnAssistant handles assistant messages from CC's stdout.
func (b *Backend) OnAssistant(msg *AssistantMessage) {
	// Track model and per-call usage (last assistant message wins, same as
	// the tmux watcher). The result message carries cumulative usage across
	// all inference calls in the turn — we need per-call for context tracking.
	b.mu.Lock()
	if msg.Message.Model != "" {
		b.lastModel = msg.Message.Model
	}
	u := msg.Message.Usage
	b.lastUsage = &u
	b.mu.Unlock()

	b.turnMu.Lock()
	handler := b.turnHandler
	b.turnMu.Unlock()

	hasToolUse := false
	for _, block := range msg.Message.Content {
		switch block.Type {
		case "text":
			b.turnMu.Lock()
			b.turnText.WriteString(block.Text)
			b.turnMu.Unlock()

			if handler != nil && handler.OnText != nil {
				handler.OnText(block.Text)
			}
			if b.replyFunc != nil {
				b.replyFunc(block.Text)
			}

		case "tool_use":
			hasToolUse = true
			b.turnMu.Lock()
			b.turnTools++
			b.turnMu.Unlock()

			if handler != nil && handler.OnToolStart != nil {
				inputStr := string(block.Input)
				handler.OnToolStart(block.Name, inputStr)
			}

			// Track Agent tool calls for status reporting (same as tmux backend).
			if block.Name == "Agent" {
				desc := backend.ExtractAgentDescription(block.Input)
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
	b.turnMu.Lock()
	handler := b.turnHandler
	b.turnHandler = nil
	b.turnActive = false
	resultCh := b.turnResultCh
	turnText := b.turnText.String()
	turnTools := b.turnTools
	b.turnMu.Unlock()

	// Build TurnResult.
	text := msg.Result
	if text == "" {
		text = turnText
	}

	// Determine model: first key from ModelUsage, falling back to lastModel.
	// Use per-call usage from the last assistant message (not the result's
	// accumulated total) — this matches what the tmux watcher reports and
	// gives compaction the actual context window fill, not a sum of all calls.
	b.mu.Lock()
	resultModel := b.lastModel
	lastUsage := b.lastUsage
	b.lastUsage = nil // reset for next turn
	b.mu.Unlock()

	for modelName, usage := range msg.ModelUsage {
		resultModel = modelName
		b.mu.Lock()
		b.contextWindow = usage.ContextWindow
		b.mu.Unlock()
		break // take first
	}

	// Prefer per-call usage from last assistant message; fall back to
	// result usage (which is cumulative) if no assistant messages seen.
	var turnUsage *backend.TurnUsage
	if lastUsage != nil {
		turnUsage = &backend.TurnUsage{
			InputTokens:              lastUsage.InputTokens,
			OutputTokens:             lastUsage.OutputTokens,
			CacheCreationInputTokens: lastUsage.CacheCreationInputTokens,
			CacheReadInputTokens:     lastUsage.CacheReadInputTokens,
		}
	} else {
		turnUsage = &backend.TurnUsage{
			InputTokens:              msg.Usage.InputTokens,
			OutputTokens:             msg.Usage.OutputTokens,
			CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
		}
	}

	result := &backend.TurnResult{
		Text:      text,
		Model:     resultModel,
		ToolCalls: turnTools,
		Usage:     turnUsage,
	}

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
		}

	case "compact_boundary":
		var cb CompactBoundaryMessage
		if err := json.Unmarshal(raw, &cb); err != nil {
			return
		}
		if b.onCompactionDone != nil {
			b.onCompactionDone(cb.CompactMetadata.PreTokens)
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
		var retry APIRetryMessage
		if err := json.Unmarshal(raw, &retry); err != nil {
			return
		}
		if b.replyFunc != nil && retry.Attempt > 1 {
			b.replyFunc(fmt.Sprintf("⏳ Rate limited, retrying in %dms (attempt %d/%d)",
				retry.RetryDelayMS, retry.Attempt, retry.MaxRetries))
		}
	}
}

// OnPermissionRequest handles can_use_tool control requests from CC.
// Dispatches to tool-specific handlers (e.g. AskUserQuestion) or the
// standard permission prompt flow.
func (b *Backend) OnPermissionRequest(msg *PermissionRequest) {
	b.handleToolRequest(msg)
}

// OnControlResponse handles responses to our control requests (e.g. initialize).
func (b *Backend) OnControlResponse(raw json.RawMessage) {
	// The init response is handled via the system/init message, not here.
	// Log at debug level if we add a logger.
}

// OnControlCancelRequest handles CC cancelling a pending control request.
func (b *Backend) OnControlCancelRequest(reqID string) {
	b.handleControlCancel(reqID)
}

// OnToolProgress handles heartbeats during long-running tool execution.
func (b *Backend) OnToolProgress(msg *ToolProgressMessage) {
	// Keep typing indicator alive during tool execution.
	if b.typingFunc != nil {
		b.typingFunc(true)
	}
	// Check for steer messages during long-running tools. Without this,
	// steers would only be checked between tool batches (OnAssistant).
	b.checkAndSendSteers()
}

// OnStreamEvent handles token-level streaming events.
func (b *Backend) OnStreamEvent(raw json.RawMessage) {
	// Quick extraction of text deltas for streaming display.
	var env struct {
		Event struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		} `json:"event"`
	}
	if json.Unmarshal(raw, &env) == nil &&
		env.Event.Type == "content_block_delta" &&
		env.Event.Delta.Type == "text_delta" &&
		env.Event.Delta.Text != "" {
		b.turnMu.Lock()
		handler := b.turnHandler
		b.turnMu.Unlock()
		if handler != nil && handler.OnText != nil {
			handler.OnText(env.Event.Delta.Text)
		}
	}
}

// OnError handles errors from the reader (scanner errors, broken pipe, EOF, etc.).
// This is called when the reader goroutine exits for any reason, including
// clean process exit (io.EOF). It marks the backend as dead and completes
// any in-flight turn so callers don't block forever.
func (b *Backend) OnError(err error) {
	component := "ccstream"
	if b.label != "" {
		component = "ccstream:" + b.label
	}
	log.Warnf(component, "subprocess reader stopped: %v", err)

	b.mu.Lock()
	b.running = false
	b.mu.Unlock()

	// If a turn was in-flight, fire OnTurnComplete with an error indication
	// so the caller doesn't block forever on CompletionChan.
	b.turnMu.Lock()
	handler := b.turnHandler
	b.turnHandler = nil
	b.turnActive = false
	resultCh := b.turnResultCh
	b.turnMu.Unlock()

	if handler != nil && handler.OnTurnComplete != nil {
		handler.OnTurnComplete(&backend.TurnResult{
			Text: fmt.Sprintf("Error: CC process exited unexpectedly: %v", err),
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

// runKeepAlive sends periodic keep-alive messages to prevent idle timeout.
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
	component := "ccstream"
	if b.label != "" {
		component = "ccstream:" + b.label
	}
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
