package provider

import (
	"context"
	"errors"
)

// Client is the interface that all LLM providers implement.
type Client interface {
	SendMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error)
	CountTokens(ctx context.Context, req *MessageRequest) (int, error)
}

// StreamHandler receives delta events during a streaming response.
type StreamHandler struct {
	OnTextDelta     func(delta string)
	OnThinkingDelta func(delta string)
}

// StreamingClient extends Client with streaming support.
// Providers that support streaming implement this interface.
type StreamingClient interface {
	StreamMessage(ctx context.Context, req *MessageRequest, handler *StreamHandler) (*MessageResponse, error)
}

// Send sends a message request using streaming only when a handler is provided.
// Pass a non-nil handler to enable streaming; pass nil to use non-streaming SendMessage.
//
// Retry logic:
// Phase 1: Standard exponential backoff (3 retries, 2s→4s→8s) for all retryable errors
// Phase 2: Extended overload retry (~2h) for 529 errors (Anthropic only)
func Send(ctx context.Context, client Client, req *MessageRequest, handler *StreamHandler) (*MessageResponse, error) {
	// Phase 1: Standard exponential backoff retry (all providers)
	resp, lastErr := retryWithBackoff(ctx, client, req, handler)
	if lastErr == nil {
		return resp, nil
	}

	// Phase 2: Extended overload retry (Anthropic only, via type assertion)
	var apiErr *APIError
	if errors.As(lastErr, &apiErr) && apiErr.IsOverloaded() {
		// Type-assert to retryableClient (currently only Anthropic implements this)
		if rc, ok := client.(retryableClient); ok {
			return retryWithOverload(ctx, rc, req, handler)
		}
	}

	return nil, lastErr
}
