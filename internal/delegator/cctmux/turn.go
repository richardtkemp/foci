package cctmux

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
)

// WaitForTurn blocks until the watcher observes the next turn completion
// (assistant message with stop_reason "end_turn"). Returns ctx.Err() on
// cancellation or deadline. Safe to call after SendToPane — if the turn
// completes between SendToPane returning and WaitForTurn being called, the
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
		return fmt.Errorf("claude-code-tmux backend not started")
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

// SendToPane sends a prompt to the Claude Code pane. It does not block waiting
// for a response — output is delivered asynchronously via the persistent
// watcher handler using the internal replyFunc. Returns immediately.
// Use WaitForTurn to block until the turn completes.
//
// If handler.OnTurnComplete is set, it is registered as a per-turn callback
// that fires once when the watcher sees end_turn, then auto-nils. This is
// the preferred mechanism for TurnContract's CompletionChan pattern.
func (b *Backend) SendToPane(ctx context.Context, prompt string, handler *delegator.EventHandler) (*delegator.TurnResult, error) {
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

	// Signal typing indicator — CC is now working. TypingFunc propagates
	// both true (here) and false (fireTurnComplete). For backend turns,
	// processAgentMessage has already returned, so TypingFunc owns the
	// typing lifecycle from this point until end_turn.
	b.replyMu.Lock()
	typFn := b.typingFunc
	b.replyMu.Unlock()
	if typFn != nil {
		typFn(true)
	}

	// Lazily discover the session and start the long-lived watch loop.
	// ensureWatcher may take seconds (session discovery + JSONL catchup).
	// Do NOT hold b.mu across this — other goroutines need it for
	// SessionFilePath, SessionID, etc. The method uses its own internal
	// sync.Once to prevent concurrent discovery.
	if err := b.ensureWatcher(ctx); err != nil {
		return nil, fmt.Errorf("session discovery: %w", err)
	}

	return &delegator.TurnResult{}, nil
}

// fireTurnComplete fires the per-turn callback (if set) with the given
// result, then nils it (one-shot). Also notifies any WaitForTurn caller.
// Stops the typing indicator via TypingFunc(false) — for backend turns,
// processAgentMessage has already returned so this is the correct place
// to stop typing (on end_turn, when CC is actually done).
func (b *Backend) fireTurnComplete(result *delegator.TurnResult) {
	// Stop typing indicator — CC's turn is done.
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

func (b *Backend) SetPermissionPromptFunc(fn delegator.PermissionPromptFunc) {
	b.replyMu.Lock()
	defer b.replyMu.Unlock()
	b.permPromptFunc = fn
}

func (b *Backend) SetOnPermissionCleared(fn func()) {
	b.lastPromptMu.Lock()
	defer b.lastPromptMu.Unlock()
	b.onPermCleared = fn
}

// SetOnPermissionCancelled is a no-op for the legacy tmux backend. Tmux
// state is polled rather than event-driven, so it doesn't expose per-perm
// cancellations the way ccstream does. The Telegram-button race that
// motivated the hook can't occur here because tmux backends don't use
// inline keyboards in the same way.
func (b *Backend) SetOnPermissionCancelled(fn func(requestID, toolName, reason string)) {}

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
		return fmt.Errorf("claude-code-tmux backend not started")
	}
	return pane.sendKeystroke(ctx, key)
}

func (b *Backend) SendSpecialKey(ctx context.Context, key string) error {
	b.mu.Lock()
	pane := b.pane
	b.mu.Unlock()
	if pane == nil {
		return fmt.Errorf("claude-code-tmux backend not started")
	}
	return pane.sendSpecial(ctx, key)
}

// Interrupt cancels the current agent turn by sending Escape×2 (cancel
// current operation) then Ctrl-C (clear any remaining input).
func (b *Backend) Interrupt(ctx context.Context) error {
	for i := 0; i < 2; i++ {
		if err := b.SendSpecialKey(ctx, "Escape"); err != nil {
			return err
		}
		time.Sleep(150 * time.Millisecond)
	}
	return b.SendSpecialKey(ctx, "C-c")
}

// IsTurnInFlight reports whether a SendToPane callback is registered but
// hasn't fired yet. A steered follow-up message should be sent via
// SendCommand (text only, no callback) to avoid overwriting the callback.
func (b *Backend) IsTurnInFlight() bool {
	b.turnCompleteMu.Lock()
	defer b.turnCompleteMu.Unlock()
	return b.turnCompleteFn != nil
}

func (b *Backend) SendCommand(ctx context.Context, command string) error {
	b.mu.Lock()
	pane := b.pane
	b.mu.Unlock()

	if pane == nil {
		return fmt.Errorf("claude-code-tmux backend not started")
	}
	return pane.sendText(ctx, command)
}

// CaptureCommandOutput polls the tmux pane until content stabilises
// (unchanged for stableFor, checked every pollInterval). Returns the
// raw pane content. Implements delegator.CommandOutputCapturer.
func (b *Backend) CaptureCommandOutput(ctx context.Context, stableFor, pollInterval time.Duration) (string, error) {
	b.mu.Lock()
	pane := b.pane
	b.mu.Unlock()
	if pane == nil {
		return "", fmt.Errorf("claude-code-tmux backend not started")
	}

	var lastContent string
	var stableSince time.Time

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Overall timeout: stableFor + 5s grace for initial rendering.
	deadline, cancel := context.WithTimeout(ctx, stableFor+5*time.Second)
	defer cancel()

	for {
		select {
		case <-deadline.Done():
			if lastContent != "" {
				return lastContent, nil // return what we have
			}
			return "", deadline.Err()
		case <-ticker.C:
			content, err := pane.capturePane(ctx)
			if err != nil {
				continue
			}
			if content == lastContent && lastContent != "" {
				if time.Since(stableSince) >= stableFor {
					return content, nil
				}
			} else {
				lastContent = content
				stableSince = time.Now()
			}
		}
	}
}
