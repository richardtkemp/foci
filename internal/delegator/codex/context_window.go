package codex

import (
	"context"

	"foci/internal/delegator"
)

// GetContextWindow returns the model's context window size and current
// usage. Uses config/read to get model_context_window; falls back to 0
// (unknown) if unavailable.
func (b *Backend) GetContextWindow(ctx context.Context) (*delegator.ContextWindow, error) {
	b.mu.Lock()
	maxTokens := b.contextWindow
	model := b.pendingModel
	if model == "" {
		model = b.model
	}
	if model == "" {
		model = b.launchModel
	}
	b.mu.Unlock()
	if model == "" {
		model = b.requestedModelFromOpts()
	}

	cw := &delegator.ContextWindow{
		MaxTokens: maxTokens,
		Model:     model,
	}

	b.turnMu.Lock()
	usage := b.stashedUsage
	b.turnMu.Unlock()
	if usage != nil {
		cw.TotalTokens = usage.InputTokens + usage.OutputTokens
	}

	return cw, nil
}
