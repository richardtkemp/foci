package provider

import (
	"context"
	"errors"
)

// Client is the interface that all LLM providers implement.
type Client interface {
	SendMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error)
	CountTokens(ctx context.Context, req *MessageRequest) (int, error)
	IsCachingAvailable() bool // true if provider supports and has caching available
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

// selfRetryingClient is an optional interface for clients that implement their own retry logic.
// When implemented, the provider layer skips retry logic and lets the client's SDK handle it.
type selfRetryingClient interface {
	HandlesOwnRetries() bool
}

// Send sends a message request using streaming only when a handler is provided.
// Pass a non-nil handler to enable streaming; pass nil to use non-streaming SendMessage.
//
// Retry logic:
// - Clients implementing selfRetryingClient.HandlesOwnRetries() == true use SDK retries (e.g., Gemini)
// - Other clients get provider-level retry:
//   - Phase 1: Standard exponential backoff (3 retries, 2s→4s→8s) for all retryable errors
//   - Phase 2: Extended overload retry (~2h) for 529 errors (Anthropic only)
func Send(ctx context.Context, client Client, req *MessageRequest, handler *StreamHandler) (*MessageResponse, error) {
	// Check if client handles its own retries (e.g., Gemini SDK has built-in retry)
	if src, ok := client.(selfRetryingClient); ok && src.HandlesOwnRetries() {
		// Let the client's SDK handle retries
		return sendOnce(ctx, client, req, handler)
	}

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

// ClientProvider provides access to API clients for different endpoint:format pairs.
// Implementations manage client lifecycle (creation, caching, initialization).
type ClientProvider interface {
	// GetClient returns the client for an endpoint:format pair, initializing it on first use.
	// Returns nil if the endpoint/format is not configured or initialization fails.
	GetClient(endpoint, format string) Client

	// PeekClient returns an existing client for an endpoint:format pair without initializing it.
	// Returns nil if the client hasn't been created yet or doesn't exist.
	PeekClient(endpoint, format string) Client

	// ResolveEndpointClient resolves the client for an endpoint+modelID pair.
	// Infers wire format from model name, falls back to openai if endpoint doesn't support it.
	ResolveEndpointClient(endpoint, modelID string) Client
}

// UsageClientProvider provides access to usage tracking clients for different endpoints.
type UsageClientProvider interface {
	// GetUsageClient returns the usage client for the given endpoint.
	// Returns nil if usage tracking is not available for this endpoint.
	GetUsageClient(endpoint string) UsageClient
}

// CombinedClientProvider combines both client and usage client provision.
// Implementations can satisfy both interfaces.
type CombinedClientProvider interface {
	ClientProvider
	UsageClientProvider
}
