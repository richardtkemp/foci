// Package gemini implements provider.Client for Google's Gemini API.
package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/provider"

	"google.golang.org/genai"
)

// Client wraps the Google genai SDK to implement provider.Client.
//
// Note: unlike the Anthropic/OpenAI clients, this client keeps the
// http.Client.Timeout (a per-request wall-clock cap). Gemini is non-streaming
// here — it has no StreamMessage — so the P2-6 streaming-truncation bug does
// not apply. The SDK also manages its own retries (HandlesOwnRetries); a
// per-request timeout bounds each attempt, whereas a single total context
// deadline would cut the SDK's retry/backoff sequence short. Left unchanged
// deliberately.
type Client struct {
	client *genai.Client
	cache  *CacheManager // nil if caching disabled
	apiKey string        // kept for debug key-suffix logging
}

// Option configures a Client.
type Option func(*clientConfig)

type clientConfig struct {
	httpClient  *http.Client
	httpTimeout time.Duration
	cacheTTL    time.Duration // 0 = caching disabled
}

// WithHTTPTimeout sets the HTTP client timeout.
func WithHTTPTimeout(d time.Duration) Option {
	return func(c *clientConfig) { c.httpTimeout = d }
}

// WithCacheTTL enables context caching with the given TTL.
func WithCacheTTL(d time.Duration) Option {
	return func(c *clientConfig) { c.cacheTTL = d }
}

// NewClient creates a Gemini API client.
func NewClient(ctx context.Context, apiKey string, opts ...Option) (*Client, error) {
	cfg := &clientConfig{
		httpTimeout: 120 * time.Second,
	}
	for _, o := range opts {
		o(cfg)
	}

	httpClient := cfg.httpClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: cfg.httpTimeout}
	}

	// Note: The Gemini SDK has built-in retry logic (5 attempts, exponential backoff
	// from 1s to 60s). We let the SDK handle retries rather than implementing our own,
	// since the SDK doesn't expose a way to disable retries and double-retry would be
	// redundant. The provider layer respects HandlesOwnRetries() to skip provider-level
	// retry logic for this client.
	gc, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:     apiKey,
		Backend:    genai.BackendGeminiAPI,
		HTTPClient: httpClient,
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: create client: %w", err)
	}

	c := &Client{client: gc, apiKey: apiKey}

	if cfg.cacheTTL > 0 {
		c.cache = NewCacheManager(gc, cfg.cacheTTL)
	}

	return c, nil
}

// Endpoint returns a human-readable name for this client's API endpoint.
func (c *Client) Endpoint() string {
	return "Gemini API"
}

// SendMessage sends a message to the Gemini API and returns a provider-neutral response.
func (c *Client) SendMessage(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	// Strip developer prefix (e.g., "google/gemini-2.5-flash" → "gemini-2.5-flash")
	modelID := config.StripDeveloperPrefix(req.Model)

	contents := messagesToGenai(req.Messages)
	config := buildConfig(req)

	// Try to use cached content for system+tools
	if c.cache != nil {
		system := systemToGenai(req.System)
		tools := toolsToGenai(req.Tools)
		if cacheName := c.cache.EnsureCache(ctx, modelID, system, tools); cacheName != "" {
			config.CachedContent = cacheName
			config.SystemInstruction = nil
			config.Tools = nil
		}
	}

	resp, err := c.client.Models.GenerateContent(ctx, modelID, contents, config)
	if err != nil {
		return nil, classifyError(err)
	}

	result, err := responseFromGenai(resp, modelID)
	if err != nil {
		return nil, err
	}
	result.KeySuffix = log.FormatKeySuffix(c.apiKey)
	return result, nil
}

// Close releases resources held by the client, including any active caches.
func (c *Client) Close(ctx context.Context) {
	if c.cache != nil {
		c.cache.Close(ctx)
	}
}

// HandlesOwnRetries returns true to indicate the Gemini SDK has built-in retry logic.
// The provider layer will skip its retry logic and let the SDK handle retries.
func (c *Client) HandlesOwnRetries() bool {
	return true
}

// CountTokens returns the input token count for a request.
//
// The genai SDK rejects SystemInstruction and Tools in CountTokensConfig for
// the Gemini API backend (only Vertex supports them), so they are folded into
// the counted contents instead: the system prompt tokenizes identically as a
// user content, and tool declarations are approximated by counting their JSON
// serialization.
func (c *Client) CountTokens(ctx context.Context, req *provider.MessageRequest) (int, error) {
	// Strip developer prefix (e.g., "google/gemini-2.5-flash" → "gemini-2.5-flash")
	modelID := config.StripDeveloperPrefix(req.Model)

	contents := messagesToGenai(req.Messages)

	if system := systemToGenai(req.System); system != nil {
		contents = append([]*genai.Content{system}, contents...)
	}
	if tools := toolsToGenai(req.Tools); len(tools) > 0 {
		raw, err := json.Marshal(tools)
		if err != nil {
			return 0, fmt.Errorf("gemini: marshal tools for count: %w", err)
		}
		contents = append(contents, &genai.Content{
			Parts: []*genai.Part{{Text: string(raw)}},
			Role:  "user",
		})
	}

	resp, err := c.client.Models.CountTokens(ctx, modelID, contents, nil)
	if err != nil {
		return 0, fmt.Errorf("gemini: count tokens: %w", err)
	}

	return int(resp.TotalTokens), nil
}

// IsCachingAvailable returns true if caching is supported and available.
// Returns false if: (1) CacheManager is nil, or (2) free tier was detected.
func (c *Client) IsCachingAvailable() bool {
	if c.cache == nil {
		return false
	}
	return !c.cache.IsCachingNotSupported()
}

// buildConfig translates a provider.MessageRequest into genai.GenerateContentConfig.
func buildConfig(req *provider.MessageRequest) *genai.GenerateContentConfig {
	config := &genai.GenerateContentConfig{
		MaxOutputTokens: int32(req.MaxTokens), // #nosec G115 - token limits are well within int32 range
	}

	// System instruction
	if len(req.System) > 0 {
		config.SystemInstruction = systemToGenai(req.System)
	}

	// Tools
	if tools := toolsToGenai(req.Tools); len(tools) > 0 {
		config.Tools = tools
	}

	// Thinking
	if req.Thinking != nil {
		config.ThinkingConfig = thinkingToGenai(req.Thinking)
	}

	return config
}
