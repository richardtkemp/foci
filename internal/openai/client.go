// Package openai implements provider.Client for the OpenAI API and
// OpenAI-compatible endpoints (OpenRouter, Together, Groq, etc.).
package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"foci/internal/provider"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// Client wraps the OpenAI SDK to implement provider.Client.
type Client struct {
	client *openai.Client
}

// Option configures a Client.
type Option func(*clientConfig)

type clientConfig struct {
	baseURL     string
	httpTimeout time.Duration
}

// WithBaseURL sets a custom API base URL (e.g. for OpenRouter, Together, local LLMs).
func WithBaseURL(url string) Option {
	return func(c *clientConfig) { c.baseURL = url }
}

// WithHTTPTimeout sets the HTTP client timeout.
func WithHTTPTimeout(d time.Duration) Option {
	return func(c *clientConfig) { c.httpTimeout = d }
}

// NewClient creates an OpenAI API client.
func NewClient(apiKey string, opts ...Option) *Client {
	cfg := &clientConfig{
		httpTimeout: 120 * time.Second,
	}
	for _, o := range opts {
		o(cfg)
	}

	sdkOpts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(&http.Client{Timeout: cfg.httpTimeout}),
		option.WithMaxRetries(0), // disable SDK retries - provider layer handles retry
	}
	if cfg.baseURL != "" {
		sdkOpts = append(sdkOpts, option.WithBaseURL(cfg.baseURL))
	}

	client := openai.NewClient(sdkOpts...)
	return &Client{client: &client}
}

// SendMessage sends a message to the OpenAI API and returns a provider-neutral response.
func (c *Client) SendMessage(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	params := buildParams(req)

	resp, err := c.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, classifyError(err)
	}

	return responseFromOpenAI(resp, req.Model)
}

// CountTokens returns an error — OpenAI has no free token counting endpoint.
// Compaction handles this gracefully.
func (c *Client) CountTokens(ctx context.Context, req *provider.MessageRequest) (int, error) {
	return 0, fmt.Errorf("openai: token counting not supported")
}

// IsCachingAvailable returns false as OpenAI does not support prompt caching.
func (c *Client) IsCachingAvailable() bool {
	return false
}

// ListModels calls the OpenAI Models.List endpoint and returns available models.
func (c *Client) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	page, err := c.client.Models.List(ctx)
	if err != nil {
		return nil, classifyError(err)
	}

	var models []provider.ModelInfo
	for _, m := range page.Data {
		models = append(models, provider.ModelInfo{
			ID:        m.ID,
			CreatedAt: time.Unix(m.Created, 0).UTC(),
		})
	}
	return models, nil
}

// classifyError maps OpenAI SDK errors to provider.APIError for
// consistent error handling in the agent loop.
func classifyError(err error) error {
	if err == nil {
		return nil
	}

	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return &provider.APIError{
			StatusCode: apiErr.StatusCode,
			Body:       apiErr.Error(),
		}
	}

	return fmt.Errorf("openai: %w", err)
}
