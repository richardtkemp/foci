package codex

import (
	"context"
	"time"

	"foci/internal/delegator"
)

// GetContextWindow returns the model's context window size and current
// usage. Uses config/read to get model_context_window; falls back to 0
// (unknown) if unavailable.
func (b *Backend) GetContextWindow(ctx context.Context) (*delegator.ContextWindow, error) {
	b.mu.Lock()
	maxTokens := b.contextWindow
	b.mu.Unlock()

	cw := &delegator.ContextWindow{
		MaxTokens: maxTokens,
		Model:     b.modelFromOpts(),
	}

	b.turnMu.Lock()
	usage := b.stashedUsage
	b.turnMu.Unlock()
	if usage != nil {
		cw.TotalTokens = usage.InputTokens + usage.OutputTokens
	}

	return cw, nil
}

// CacheTTL returns the prompt-cache time-to-live for OpenAI's API.
// OpenAI's prompt caching is shorter and less documented than Anthropic's;
// 5 minutes is a conservative estimate.
func (b *Backend) CacheTTL() time.Duration {
	return 5 * time.Minute
}
