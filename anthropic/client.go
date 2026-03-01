package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// IsServerError returns true if this is a 500 Internal Server Error.
func (e *APIError) IsServerError() bool {
	return e.StatusCode == http.StatusInternalServerError
}

// IsAuthError returns true if this is a 401 Unauthorized error.
func (e *APIError) IsAuthError() bool {
	return e.StatusCode == http.StatusUnauthorized
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
	apiKey         string
	tokenFunc      func() (string, error) // dynamic token source (overrides apiKey)
	httpClient     *http.Client
	baseURL        string
	retryBaseDelay time.Duration // initial backoff for 500 retries; 0 = default 2s
}

// resolveToken returns the token to use for API requests.
// If tokenFunc is set, it calls the function; otherwise returns the static apiKey.
func (c *Client) resolveToken() (string, error) {
	if c.tokenFunc != nil {
		return c.tokenFunc()
	}
	return c.apiKey, nil
}

// NewClient creates a new Anthropic API client.
func NewClient(apiKey string) *Client {
	return &Client{
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 60 * time.Second},
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

// NewClientWithTokenFunc creates a client that calls tokenFunc for each request
// to get the current Bearer token. Used with OAuthManager for auto-refreshing tokens.
func NewClientWithTokenFunc(tokenFunc func() (string, error), timeout time.Duration) *Client {
	return &Client{
		tokenFunc:  tokenFunc,
		httpClient: &http.Client{Timeout: timeout},
		baseURL:    "https://api.anthropic.com",
	}
}

// NewClientWithBase creates a client with a custom base URL (for testing).
func NewClientWithBase(baseURL, apiKey string) *Client {
	return &Client{
		apiKey:         apiKey,
		httpClient:     &http.Client{Timeout: 120 * time.Second},
		baseURL:        baseURL,
		retryBaseDelay: time.Millisecond, // fast retries for tests
	}
}

// sendOnce performs a single HTTP request to the messages API and returns the
// parsed response, or an error. Extracted from SendMessage to support retry.
func (c *Client) sendOnce(ctx context.Context, body []byte) (*MessageResponse, error) {
	token, err := c.resolveToken()
	if err != nil {
		return nil, fmt.Errorf("resolve token: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
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

// SendMessage sends a message request and returns the response.
// Retries up to 3 times on HTTP 500 errors with exponential backoff (2s, 4s, 8s).
func (c *Client) SendMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	const maxRetries = 3
	backoff := c.retryBaseDelay
	if backoff == 0 {
		backoff = 2 * time.Second
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			slog.Warn("anthropic: retrying after 500", "attempt", attempt, "backoff", backoff.String())
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		resp, err := c.sendOnce(ctx, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			return nil, err
		}

		if !apiErr.IsServerError() {
			return nil, err // non-500 errors are not retried
		}
	}

	return nil, lastErr
}

// CountTokens calls the /v1/messages/count_tokens endpoint to get exact
// input token counts for a request. The endpoint is free (no tokens billed).
func (c *Client) CountTokens(ctx context.Context, req *MessageRequest) (int, error) {
	token, err := c.resolveToken()
	if err != nil {
		return 0, fmt.Errorf("resolve token: %w", err)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return 0, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/v1/messages/count_tokens", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("anthropic-beta", "oauth-2025-04-20")

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return 0, fmt.Errorf("send request: %w", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return 0, fmt.Errorf("read response: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		return 0, &APIError{
			StatusCode: httpResp.StatusCode,
			Body:       string(respBody),
			RetryAfter: httpResp.Header.Get("Retry-After"),
		}
	}

	var resp CountTokensResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return 0, fmt.Errorf("unmarshal response: %w", err)
	}

	return resp.InputTokens, nil
}
