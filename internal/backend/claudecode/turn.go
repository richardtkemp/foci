package claudecode

import (
	"context"
	"fmt"

	"foci/internal/backend"
)

// SendTurn sends a prompt to the Claude Code pane. It does not block waiting
// for a response — output is delivered asynchronously via the persistent
// watcher handler using the ReplyFunc set by SetReplyFunc. Returns immediately.
func (b *Backend) SendTurn(ctx context.Context, prompt string, handler *backend.EventHandler) (*backend.TurnResult, error) {
	b.mu.Lock()
	if b.pane == nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("claude-code backend not started")
	}
	pane := b.pane
	b.mu.Unlock()

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

func (b *Backend) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
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
