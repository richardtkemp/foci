// Package openai implements provider.Client for the OpenAI API and
// OpenAI-compatible endpoints (OpenRouter, Together, Groq, etc.).
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"foci/internal/log"
	"foci/internal/provider"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

var (
	openaiLog = log.NewComponentLogger("openai")
)

// Client wraps the OpenAI SDK to implement provider.Client.
type Client struct {
	client  *openai.Client
	apiKey  string        // kept for debug key-suffix logging
	baseURL string        // stored for Endpoint() identification
	timeout time.Duration // total cap for non-streaming, inter-chunk idle cap for streaming
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
		// No http.Client.Timeout: a wall-clock cap truncates long streaming
		// responses mid-stream (P2-6). The timeout is applied per call instead —
		// a total deadline for non-streaming, an inter-chunk idle watchdog for
		// streaming.
		option.WithHTTPClient(&http.Client{}),
		option.WithMaxRetries(0), // disable SDK retries - provider layer handles retry
	}
	if cfg.baseURL != "" {
		sdkOpts = append(sdkOpts, option.WithBaseURL(cfg.baseURL))
	}

	client := openai.NewClient(sdkOpts...)
	return &Client{client: &client, apiKey: apiKey, baseURL: cfg.baseURL, timeout: cfg.httpTimeout}
}

// Endpoint returns a human-readable name for this client's API endpoint.
func (c *Client) Endpoint() string {
	if c.baseURL == "" {
		return "OpenAI API"
	}
	return provider.EndpointNameFromURL(c.baseURL)
}

// isOpenRouter reports whether this client's configured endpoint is
// OpenRouter, identified by its base URL. Used to gate OpenRouter-only
// debug logging (foci_todo #1482) — other OpenAI-compatible providers
// (Together, Groq, plain OpenAI, ...) don't expose a queryable generation id.
func (c *Client) isOpenRouter() bool {
	return strings.Contains(c.baseURL, "openrouter.ai")
}

// logGenerationID surfaces OpenRouter's per-call generation id (resp.ID, the
// "gen-..." id) at DEBUG level so a cache-bust (or other cost/behaviour)
// investigation can be settled directly against OpenRouter's authoritative
// GET /api/v1/generation?id=<id> record instead of by inference (foci_todo
// #1482, surfaced by #1477). No-op for non-OpenRouter endpoints or when the
// response carries no id.
func (c *Client) logGenerationID(result *provider.MessageResponse, model string) {
	if !c.isOpenRouter() || result == nil || result.ID == "" {
		return
	}
	openaiLog.Debugf("openrouter generation id: id=%s model=%s key=%s", result.ID, model, result.KeySuffix)
}

// SendMessage sends a message to the OpenAI API and returns a provider-neutral response.
func (c *Client) SendMessage(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	params := buildParams(req)

	// Non-streaming: total wall-clock deadline via context (replacing the
	// removed http.Client.Timeout). (P2-6.)
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	resp, err := c.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, classifyError(err)
	}

	result, err := responseFromOpenAI(resp, req.Model)
	if err != nil {
		return nil, err
	}
	result.KeySuffix = log.FormatKeySuffix(c.apiKey)
	c.logGenerationID(result, req.Model)
	return result, nil
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
			RetryAfter: retryAfterHint(apiErr),
		}
	}

	return fmt.Errorf("openai: %w", err)
}

// retryAfterHint extracts a Retry-After hint (in seconds) from an OpenAI-format
// error so the provider retry layer can honor it on a 429. Standard providers
// set the HTTP Retry-After response header; OpenRouter instead reports the
// upstream provider's hint in the JSON body at error.metadata.retry_after_seconds.
// Returns "" when no hint is present.
func retryAfterHint(apiErr *openai.Error) string {
	if apiErr.Response != nil {
		if ra := apiErr.Response.Header.Get("Retry-After"); ra != "" {
			return ra
		}
	}
	var body struct {
		Error struct {
			Metadata struct {
				RetryAfterSeconds int `json:"retry_after_seconds"`
			} `json:"metadata"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(apiErr.RawJSON()), &body) == nil && body.Error.Metadata.RetryAfterSeconds > 0 {
		return strconv.Itoa(body.Error.Metadata.RetryAfterSeconds)
	}
	return ""
}
