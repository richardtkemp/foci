package provider

import (
	"context"
	"errors"
	"time"
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

// sendWithRetry sends a message request with retry logic but no fallback.
// Use Send for the full pipeline (strip unsupported params + fallback chain).
//
// Retry logic:
// - Clients implementing selfRetryingClient.HandlesOwnRetries() == true use SDK retries (e.g., Gemini)
// - Other clients get provider-level retry:
//   - Phase 1: Standard exponential backoff (3 retries, 2s→4s→8s) for all retryable errors
//   - Phase 2: Extended retry for retryableClient implementations:
//     - 529 overload: up to ~2h (configurable via OverloadMaxDuration)
//     - 5xx server errors: up to ~5min (configurable via ServerErrorMaxDuration)
func sendWithRetry(ctx context.Context, client Client, req *MessageRequest, handler *StreamHandler) (*MessageResponse, error) {
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

	// Phase 2: Extended retry (retryableClient implementations only, e.g. Anthropic)
	var apiErr *APIError
	if errors.As(lastErr, &apiErr) && apiErr.IsRetryable() {
		if rc, ok := client.(retryableClient); ok {
			maxDuration := rc.ServerErrorMaxDuration()
			if apiErr.IsOverloaded() {
				maxDuration = rc.OverloadMaxDuration()
			}
			return retryExtended(ctx, rc, req, handler, maxDuration)
		}
	}

	return nil, lastErr
}

// CredentialResolver handles format-specific credential resolution.
// Formats with complex auth (e.g. OAuth, setup tokens, credential chaining)
// implement this interface. The resolver captures its secrets store at construction
// time — callers don't pass credentials per-call.
type CredentialResolver interface {
	// ResolveClient returns a configured Client for the given endpoint.
	// apiKeyName is the secret name for the API key (e.g., "anthropic.api_key").
	// baseURL is the endpoint base URL (empty = default).
	// httpTimeout is the HTTP client timeout from the endpoint config.
	ResolveClient(ctx context.Context, endpointName, apiKeyName, baseURL string, httpTimeout time.Duration) (Client, error)

	// ResolveUsageClient returns a configured UsageClient for the given endpoint,
	// or nil if usage tracking is not supported or credentials are unavailable.
	ResolveUsageClient(endpointName, apiKeyName string) (UsageClient, error)

	// GetReloadFunc returns a function that reloads credentials from disk,
	// or nil if hot-reload is not supported.
	GetReloadFunc(secretsPath string) func() error
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

	// ResolveEndpointClient resolves the client for an endpoint+format pair.
	// Falls back to openai format if the endpoint doesn't support the given format.
	ResolveEndpointClient(endpoint, format string) Client
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
