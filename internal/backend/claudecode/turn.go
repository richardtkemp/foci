package claudecode

import (
	"context"
	"fmt"

	"foci/internal/backend"
)

func (b *Backend) SendTurn(ctx context.Context, prompt string, handler *backend.EventHandler) (*backend.TurnResult, error) {
	b.mu.Lock()
	if b.pane == nil || b.watcher == nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("claude-code backend not started")
	}
	pane := b.pane
	watcher := b.watcher
	b.mu.Unlock()

	// Reset turn state so we don't carry over from a previous turn.
	watcher.resetTurn()

	// Set up a completion channel — the watcher will signal when
	// stop_reason == "end_turn" is seen.
	done := make(chan *backend.TurnResult, 1)
	wrappedHandler := &backend.EventHandler{
		OnText:      handler.OnText,
		OnToolStart: handler.OnToolStart,
		OnToolEnd:   handler.OnToolEnd,
		OnTurnComplete: func(result *backend.TurnResult) {
			if handler.OnTurnComplete != nil {
				handler.OnTurnComplete(result)
			}
			select {
			case done <- result:
			default:
			}
		},
	}

	// Start watching for output.
	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()
	go watcher.watchLoop(watchCtx, wrappedHandler)

	// Send the prompt.
	if err := pane.sendKeys(ctx, prompt); err != nil {
		return nil, fmt.Errorf("send prompt: %w", err)
	}

	// Wait for the turn to complete.
	select {
	case result := <-done:
		return result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *Backend) SendCommand(ctx context.Context, command string) error {
	b.mu.Lock()
	pane := b.pane
	b.mu.Unlock()

	if pane == nil {
		return fmt.Errorf("claude-code backend not started")
	}
	return pane.sendKeys(ctx, command)
}
