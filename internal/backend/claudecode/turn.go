package claudecode

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"foci/internal/backend"
	"foci/internal/log"
)

// WaitForTurn blocks until the watcher observes the next turn completion
// (assistant message with stop_reason "end_turn"). Returns ctx.Err() on
// cancellation or deadline. Safe to call after SendTurn — if the turn
// completes between SendTurn returning and WaitForTurn being called, the
// signal is buffered and WaitForTurn returns immediately.
func (b *Backend) WaitForTurn(ctx context.Context) error {
	ch := make(chan struct{}, 1)
	b.waitMu.Lock()
	b.waitCh = ch
	b.waitMu.Unlock()

	defer func() {
		b.waitMu.Lock()
		b.waitCh = nil
		b.waitMu.Unlock()
	}()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// notifyTurnComplete signals any active WaitForTurn caller.
func (b *Backend) notifyTurnComplete() {
	b.waitMu.Lock()
	ch := b.waitCh
	b.waitMu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// WaitReady blocks until Claude Code's TUI is ready to accept prompts.
// Detected by scraping the tmux pane for the "❯" input prompt. CC shows
// its "Claude Code" banner early in startup, but the input prompt only
// appears after session loading, tool init, and auth are complete.
func (b *Backend) WaitReady(ctx context.Context) error {
	b.mu.Lock()
	pane := b.pane
	b.mu.Unlock()
	if pane == nil {
		return fmt.Errorf("claude-code backend not started")
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for Claude Code ready: %w", ctx.Err())
		case <-ticker.C:
			content, err := pane.capturePane(ctx)
			if err != nil {
				continue
			}
			if strings.Contains(content, "❯") {
				return nil
			}
		}
	}
}

// SendTurn sends a prompt to the Claude Code pane. It does not block waiting
// for a response — output is delivered asynchronously via the persistent
// watcher handler using the ReplyFunc set by SetReplyFunc. Returns immediately.
// Use WaitForTurn to block until the turn completes.
//
// If handler.OnTurnComplete is set, it is registered as a per-turn callback
// that fires once when the watcher sees end_turn, then auto-nils. This is
// the preferred mechanism for TurnContract's CompletionChan pattern.
func (b *Backend) SendTurn(ctx context.Context, prompt string, handler *backend.EventHandler) (*backend.TurnResult, error) {
	b.mu.Lock()
	if b.pane == nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("claude-code backend not started")
	}
	pane := b.pane
	b.mu.Unlock()

	// Register per-turn completion callback (if provided).
	if handler != nil && handler.OnTurnComplete != nil {
		b.turnCompleteMu.Lock()
		b.turnCompleteFn = handler.OnTurnComplete
		b.turnCompleteMu.Unlock()
	}

	// Clear permission prompt dedup so the next prompt is forwarded.
	b.clearLastPrompt()

	// Record the JSONL file offset BEFORE sending, so the watcher
	// starts from here and catches any response written between
	// sendText and watcher start.
	b.recordPreSendOffset()

	// Send the prompt to the pane.
	if err := pane.sendText(ctx, prompt); err != nil {
		return nil, fmt.Errorf("send prompt: %w", err)
	}

	// Signal typing indicator — CC is now working.
	b.replyMu.Lock()
	typFn := b.typingFunc
	b.replyMu.Unlock()
	if typFn != nil {
		typFn(true)
	}

	// Lazily discover the session and start the long-lived watch loop.
	b.mu.Lock()
	if err := b.ensureWatcher(ctx); err != nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("session discovery: %w", err)
	}
	b.mu.Unlock()

	return &backend.TurnResult{}, nil
}

// fireTurnComplete fires the per-turn callback (if set) with the given
// result, then nils it (one-shot). Also notifies any WaitForTurn caller
// and stops the typing indicator.
func (b *Backend) fireTurnComplete(result *backend.TurnResult) {
	// Stop typing indicator — CC is done.
	b.replyMu.Lock()
	typFn := b.typingFunc
	b.replyMu.Unlock()
	if typFn != nil {
		typFn(false)
	}

	// Per-turn callback (one-shot).
	b.turnCompleteMu.Lock()
	fn := b.turnCompleteFn
	b.turnCompleteFn = nil
	b.turnCompleteMu.Unlock()
	if fn != nil {
		fn(result)
	}

	// Legacy WaitForTurn signal.
	b.notifyTurnComplete()
}

// recordPreSendOffset records the current JSONL file size so the watcher
// can start from this position. If the session file doesn't exist yet
// (first turn, CC hasn't created it), records 0 to read from the beginning.
func (b *Backend) recordPreSendOffset() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.watcher != nil {
		return // watcher already running, offset doesn't matter
	}

	// Try to find the session file via the existing discovery path.
	// If we can't find it, default to -1 (tail from end of file) rather
	// than 0 (beginning) — replaying the entire session history would
	// flood the user with every past response.
	childPID, err := findChildPID(b.pane.pid)
	if err != nil {
		log.Warnf("backend/cc", "recordPreSendOffset: findChildPID(%d) failed: %v", b.pane.pid, err)
		return // preSendOffset stays at -1 (default)
	}
	_, jsonlPath, err := discoverSessionFile(childPID, b.workDir)
	if err != nil {
		log.Warnf("backend/cc", "recordPreSendOffset: discoverSessionFile(pid=%d) failed: %v", childPID, err)
		return // preSendOffset stays at -1 (default)
	}
	info, err := os.Stat(jsonlPath)
	if err != nil {
		b.preSendOffset = 0
		return
	}
	b.preSendOffset = info.Size()
	log.Debugf("backend/cc", "recorded pre-send offset: %d bytes", b.preSendOffset)
}

func (b *Backend) SetReplyFunc(fn backend.ReplyFunc) {
	b.replyMu.Lock()
	defer b.replyMu.Unlock()
	b.replyFunc = fn
}

func (b *Backend) SetPermissionPromptFunc(fn backend.PermissionPromptFunc) {
	b.replyMu.Lock()
	defer b.replyMu.Unlock()
	b.permPromptFunc = fn
}

func (b *Backend) SetOnSessionReady(fn func(string)) {
	b.replyMu.Lock()
	defer b.replyMu.Unlock()
	b.onSessionReady = fn
}

func (b *Backend) SetTypingFunc(fn func(bool)) {
	b.replyMu.Lock()
	defer b.replyMu.Unlock()
	b.typingFunc = fn
}

func (b *Backend) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}

func (b *Backend) SessionFilePath() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.watcher != nil {
		return b.watcher.path
	}
	return ""
}

func (b *Backend) SendKeystroke(ctx context.Context, key string) error {
	b.mu.Lock()
	pane := b.pane
	b.mu.Unlock()
	if pane == nil {
		return fmt.Errorf("claude-code backend not started")
	}
	return pane.sendKeystroke(ctx, key)
}

func (b *Backend) SendSpecialKey(ctx context.Context, key string) error {
	b.mu.Lock()
	pane := b.pane
	b.mu.Unlock()
	if pane == nil {
		return fmt.Errorf("claude-code backend not started")
	}
	return pane.sendSpecial(ctx, key)
}

func (b *Backend) SendCommand(ctx context.Context, command string) error {
	b.mu.Lock()
	pane := b.pane
	b.mu.Unlock()

	if pane == nil {
		return fmt.Errorf("claude-code backend not started")
	}
	return pane.sendText(ctx, command)
}
