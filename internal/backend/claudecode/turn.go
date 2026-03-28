package claudecode

import (
	"context"
	"fmt"

	"foci/internal/backend"
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

	// Send the prompt to the pane.
	if err := pane.sendText(ctx, prompt); err != nil {
		return nil, fmt.Errorf("send prompt: %w", err)
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
// result, then nils it (one-shot). Also notifies any WaitForTurn caller.
func (b *Backend) fireTurnComplete(result *backend.TurnResult) {
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

func (b *Backend) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
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
