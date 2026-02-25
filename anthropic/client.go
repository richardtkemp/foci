package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// APIError is returned when the API responds with a non-200 status code.
// Use errors.As to check for this type and inspect StatusCode or RetryAfter.
type APIError struct {
	StatusCode int    // HTTP status code
	Body       string // response body
	RetryAfter string // retry-after header value (seconds or date), empty if not present
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API error (status %d): %s", e.StatusCode, e.Body)
}

// IsRateLimit returns true if this is a 429 Too Many Requests error.
func (e *APIError) IsRateLimit() bool {
	return e.StatusCode == http.StatusTooManyRequests
}

// IsOverloaded returns true if this is a 529 Overloaded error.
func (e *APIError) IsOverloaded() bool {
	return e.StatusCode == 529
}

// RetryAfterSeconds parses the retry-after header as seconds.
// Returns 0 if not present or unparseable.
func (e *APIError) RetryAfterSeconds() int {
	if e.RetryAfter == "" {
		return 0
	}
	if secs, err := strconv.Atoi(e.RetryAfter); err == nil {
		return secs
	}
	return 0
}

// Client is an Anthropic API client with prompt caching support.
type Client struct {
	apiKey     string
	httpClient *http.Client
	baseURL    string
}

// NewClient creates a new Anthropic API client.
func NewClient(apiKey string) *Client {
	return &Client{
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 120 * time.Second},
		baseURL:    "https://api.anthropic.com",
	}
}

// NewClientWithTimeout creates a client with a custom HTTP timeout.
func NewClientWithTimeout(apiKey string, timeout time.Duration) *Client {
	return &Client{
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: timeout},
		baseURL:    "https://api.anthropic.com",
	}
}

// NewClientWithBase creates a client with a custom base URL (for testing).
func NewClientWithBase(baseURL, apiKey string) *Client {
	return &Client{
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 120 * time.Second},
		baseURL:    baseURL,
	}
}

// SendMessage sends a message request and returns the response.
func (c *Client) SendMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	// prompt-caching beta removed — caching is GA as of 2024.
	// See https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching
	httpReq.Header.Set("anthropic-beta", "oauth-2025-04-20")

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		return nil, &APIError{
			StatusCode: httpResp.StatusCode,
			Body:       string(respBody),
			RetryAfter: httpResp.Header.Get("Retry-After"),
		}
	}

	var resp MessageResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return &resp, nil
}
