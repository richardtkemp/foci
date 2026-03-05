// Package gemini implements provider.Client for Google's Gemini API.
package gemini

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"foci/internal/config"
	"foci/internal/provider"

	"google.golang.org/genai"
)

// Client wraps the Google genai SDK to implement provider.Client.
type Client struct {
	client *genai.Client
	model  string        // default model (can be overridden per request)
	cache  *CacheManager // nil if caching disabled
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

	gc, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:     apiKey,
		Backend:    genai.BackendGeminiAPI,
		HTTPClient: httpClient,
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: create client: %w", err)
	}

	c := &Client{client: gc}

	if cfg.cacheTTL > 0 {
		c.cache = NewCacheManager(gc, cfg.cacheTTL)
	}

	return c, nil
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

	return responseFromGenai(resp, modelID)
}

// Close releases resources held by the client, including any active caches.
func (c *Client) Close(ctx context.Context) {
	if c.cache != nil {
		c.cache.Close(ctx)
	}
}

// CountTokens returns the input token count for a request.
func (c *Client) CountTokens(ctx context.Context, req *provider.MessageRequest) (int, error) {
	// Strip developer prefix (e.g., "google/gemini-2.5-flash" → "gemini-2.5-flash")
	modelID := config.StripDeveloperPrefix(req.Model)

	contents := messagesToGenai(req.Messages)

	countConfig := &genai.CountTokensConfig{}
	if len(req.System) > 0 {
		countConfig.SystemInstruction = systemToGenai(req.System)
	}
	if tools := toolsToGenai(req.Tools); len(tools) > 0 {
		countConfig.Tools = tools
	}

	resp, err := c.client.Models.CountTokens(ctx, modelID, contents, countConfig)
	if err != nil {
		return 0, fmt.Errorf("gemini: count tokens: %w", err)
	}

	return int(resp.TotalTokens), nil
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
