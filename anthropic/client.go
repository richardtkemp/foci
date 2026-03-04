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
	"sync"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// CountTokensResponse is the response from the /v1/messages/count_tokens endpoint.
type CountTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

// Client is an Anthropic API client with prompt caching support.
type Client struct {
	apiKey         string
	tokenFunc      func() (string, error) // dynamic token source (overrides apiKey)
	httpClient     *http.Client
	baseURL        string
	retryBaseDelay time.Duration // initial backoff for server error retries; 0 = default 2s
	useSDK         bool          // use SDK transport (true) or raw HTTP (false)

	overloadMu      sync.Mutex
	overloadRecover chan struct{} // closed to signal recovery from 529; nil when not overloaded

	// Lazy SDK client — initialized once on first SDK call.
	sdkOnce   sync.Once
	sdkClient *sdk.Client
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
		useSDK:     true,
	}
}

// NewClientWithTimeout creates a client with a custom HTTP timeout.
func NewClientWithTimeout(apiKey string, timeout time.Duration) *Client {
	return &Client{
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: timeout},
		baseURL:    "https://api.anthropic.com",
		useSDK:     true,
	}
}

// NewClientWithTokenFunc creates a client that calls tokenFunc for each request
// to get the current Bearer token. Used with OAuthManager for auto-refreshing tokens.
func NewClientWithTokenFunc(tokenFunc func() (string, error), timeout time.Duration) *Client {
	return &Client{
		tokenFunc:  tokenFunc,
		httpClient: &http.Client{Timeout: timeout},
		baseURL:    "https://api.anthropic.com",
		useSDK:     true,
	}
}

// NewClientWithBase creates a client with a custom base URL (for testing).
func NewClientWithBase(baseURL, apiKey string) *Client {
	return &Client{
		apiKey:         apiKey,
		httpClient:     &http.Client{Timeout: 120 * time.Second},
		baseURL:        baseURL,
		retryBaseDelay: time.Millisecond, // fast retries for tests
		useSDK:         false,            // tests use mock HTTP server
	}
}

// SetUseSDK configures whether the client uses the SDK transport (true) or raw HTTP (false).
func (c *Client) SetUseSDK(useSDK bool) {
	c.useSDK = useSDK
}

// SetBaseURL overrides the API base URL. Must be called before any API requests.
func (c *Client) SetBaseURL(url string) {
	c.baseURL = url
}

// ensureSDKClient lazily initializes the SDK client.
func (c *Client) ensureSDKClient() *sdk.Client {
	c.sdkOnce.Do(func() {
		sc := sdk.NewClient(
			option.WithBaseURL(c.baseURL),
			option.WithHTTPClient(c.httpClient),
			option.WithMaxRetries(0), // we handle retries ourselves
			option.WithAuthToken("placeholder"), // overridden per-request
		)
		c.sdkClient = &sc
	})
	return c.sdkClient
}

// enterOverload lazily creates the overload recovery channel and returns it.
// Callers should listen on the returned channel to wake early when another
// goroutine's API call succeeds (proving the server has recovered).
func (c *Client) enterOverload() <-chan struct{} {
	c.overloadMu.Lock()
	defer c.overloadMu.Unlock()
	if c.overloadRecover == nil {
		c.overloadRecover = make(chan struct{})
	}
	return c.overloadRecover
}

// signalRecovery closes the overload recovery channel (waking all waiting
// goroutines) and nils it. Safe to call when not overloaded (no-op).
func (c *Client) signalRecovery() {
	c.overloadMu.Lock()
	defer c.overloadMu.Unlock()
	if c.overloadRecover != nil {
		close(c.overloadRecover)
		c.overloadRecover = nil
	}
}

// overloadBaseDelay returns the initial backoff for the extended 529 retry loop.
// Production: 5s. Test mode (retryBaseDelay < 1s): retryBaseDelay * 5.
func (c *Client) overloadBaseDelay() time.Duration {
	if c.retryBaseDelay > 0 && c.retryBaseDelay < time.Second {
		return c.retryBaseDelay * 5
	}
	return 5 * time.Second
}

// overloadMaxDuration returns the maximum wall-clock time for extended 529 retries.
// Production: 2h. Test mode (retryBaseDelay < 1s): retryBaseDelay * 500.
func (c *Client) overloadMaxDuration() time.Duration {
	if c.retryBaseDelay > 0 && c.retryBaseDelay < time.Second {
		return c.retryBaseDelay * 500
	}
	return 2 * time.Hour
}

// sendOnce dispatches to SDK or raw HTTP transport based on c.useSDK.
func (c *Client) sendOnce(ctx context.Context, body []byte, req *MessageRequest) (*MessageResponse, error) {
	if c.useSDK {
		return c.sendOnceSDK(ctx, req)
	}
	return c.sendOnceRaw(ctx, body)
}

// sendOnceSDK sends a message using the official Anthropic SDK.
func (c *Client) sendOnceSDK(ctx context.Context, req *MessageRequest) (*MessageResponse, error) {
	token, err := c.resolveToken()
	if err != nil {
		return nil, fmt.Errorf("resolve token: %w", err)
	}

	params := buildSDKParams(req)
	sc := c.ensureSDKClient()

	slog.Debug("anthropic: sdk_call_start", "model", req.Model)
	callStart := time.Now()

	msg, err := sc.Messages.New(ctx, params, sdkRequestOptions(token)...)

	callDur := time.Since(callStart)
	if err != nil {
		slog.Debug("anthropic: sdk_call_error", "duration", callDur, "error", err)
		return nil, classifySDKError(err)
	}
	slog.Debug("anthropic: sdk_call_done", "duration", callDur, "stop_reason", msg.StopReason)

	return responseFromSDK(msg), nil
}

// sendOnceRaw performs a single HTTP request to the messages API and returns the
// parsed response, or an error. The original hand-rolled transport.
func (c *Client) sendOnceRaw(ctx context.Context, body []byte) (*MessageResponse, error) {
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

	deadline, hasDeadline := ctx.Deadline()
	httpTimeout := c.httpClient.Timeout
	slog.Debug("anthropic: http_call_start", "url", c.baseURL+"/v1/messages", "http_timeout", httpTimeout, "ctx_has_deadline", hasDeadline, "ctx_deadline", deadline, "body_bytes", len(body))
	callStart := time.Now()

	httpResp, err := c.httpClient.Do(httpReq)

	callDur := time.Since(callStart)
	if err != nil {
		slog.Debug("anthropic: http_call_error", "duration", callDur, "error", err, "ctx_err", ctx.Err())
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	slog.Debug("anthropic: http_call_done", "duration", callDur, "status", httpResp.StatusCode)

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
//
// Phase 1: Retries up to 3 times on retryable server errors (500, 502, 503, 529)
// with exponential backoff (2s, 4s, 8s).
//
// Phase 2 (529 only): If phase 1 exhausts on a 529, enters an extended
// duration-based retry loop (~2h production, scaled in tests) with 5s base
// backoff doubling without cap. A cross-goroutine recovery signal wakes
// sleeping retriers early when any SendMessage on the same Client succeeds.
func (c *Client) SendMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error) {
	// For raw transport, pre-marshal the body once. SDK transport uses req directly.
	var body []byte
	if !c.useSDK {
		var err error
		body, err = json.Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
	}

	// Phase 1: standard retries for all retryable errors.
	const maxRetries = 3
	backoff := c.retryBaseDelay
	if backoff == 0 {
		backoff = 2 * time.Second
	}

	var lastErr error
	loopStart := time.Now()
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			slog.Warn("anthropic: retrying after server error", "attempt", attempt, "status", lastErr.Error(), "backoff", backoff.String())
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		slog.Debug("anthropic: send_attempt_start", "attempt", attempt, "elapsed_total", time.Since(loopStart))
		attemptStart := time.Now()
		resp, err := c.sendOnce(ctx, body, req)
		attemptDur := time.Since(attemptStart)
		if err == nil {
			slog.Debug("anthropic: send_attempt_ok", "attempt", attempt, "duration", attemptDur, "elapsed_total", time.Since(loopStart))
			c.signalRecovery()
			return resp, nil
		}
		lastErr = err
		slog.Debug("anthropic: send_attempt_fail", "attempt", attempt, "duration", attemptDur, "error", err, "elapsed_total", time.Since(loopStart))

		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			return nil, err
		}

		if !apiErr.IsRetryable() {
			return nil, err // non-retryable errors fail immediately
		}
	}

	slog.Debug("anthropic: send_exhausted_retries", "elapsed_total", time.Since(loopStart), "last_error", lastErr)

	// Phase 2: extended overload retries (529 only).
	var apiErr *APIError
	if !errors.As(lastErr, &apiErr) || !apiErr.IsOverloaded() {
		return nil, lastErr
	}

	overloadBackoff := c.overloadBaseDelay()
	maxDuration := c.overloadMaxDuration()
	overloadStart := time.Now()
	recoverCh := c.enterOverload()

	for time.Since(overloadStart) < maxDuration {
		slog.Warn("anthropic: overload retry", "backoff", overloadBackoff.String(), "elapsed", time.Since(overloadStart).String(), "max", maxDuration.String())

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(overloadBackoff):
		case <-recoverCh:
			slog.Info("anthropic: overload recovery signal received, retrying immediately")
			recoverCh = c.enterOverload() // re-acquire for next iteration
		}

		resp, err := c.sendOnce(ctx, body, req)
		if err == nil {
			slog.Info("anthropic: recovered from overload", "elapsed", time.Since(overloadStart).String())
			c.signalRecovery()
			return resp, nil
		}
		lastErr = err

		var retryAPIErr *APIError
		if !errors.As(err, &retryAPIErr) || !retryAPIErr.IsRetryable() {
			return nil, err
		}

		overloadBackoff *= 2
	}

	slog.Warn("anthropic: overload retries exhausted", "elapsed", time.Since(overloadStart).String())
	return nil, lastErr
}

// CountTokens calls the /v1/messages/count_tokens endpoint to get exact
// input token counts for a request. The endpoint is free (no tokens billed).
func (c *Client) CountTokens(ctx context.Context, req *MessageRequest) (int, error) {
	if c.useSDK {
		return c.countTokensSDK(ctx, req)
	}
	return c.countTokensRaw(ctx, req)
}

// countTokensSDK counts tokens using the SDK.
func (c *Client) countTokensSDK(ctx context.Context, req *MessageRequest) (int, error) {
	token, err := c.resolveToken()
	if err != nil {
		return 0, fmt.Errorf("resolve token: %w", err)
	}

	params := buildSDKCountParams(req)
	sc := c.ensureSDKClient()

	result, err := sc.Messages.CountTokens(ctx, params, sdkRequestOptions(token)...)
	if err != nil {
		return 0, classifySDKError(err)
	}
	return int(result.InputTokens), nil
}

// countTokensRaw counts tokens using raw HTTP.
func (c *Client) countTokensRaw(ctx context.Context, req *MessageRequest) (int, error) {
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
	defer func() { _ = httpResp.Body.Close() }()

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

// ModelInfo represents a model returned by the /v1/models endpoint.
type ModelInfo struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
}

// ListModels calls the /v1/models endpoint to list available models.
func (c *Client) ListModels() ([]ModelInfo, error) {
	if c.useSDK {
		return c.listModelsSDK()
	}
	return c.listModelsRaw()
}

// listModelsSDK lists models using the SDK.
func (c *Client) listModelsSDK() ([]ModelInfo, error) {
	token, err := c.resolveToken()
	if err != nil {
		return nil, fmt.Errorf("resolve token: %w", err)
	}

	sc := c.ensureSDKClient()
	page, err := sc.Models.List(context.Background(), sdk.ModelListParams{
		Limit: param.NewOpt(int64(100)),
	}, sdkRequestOptions(token)...)
	if err != nil {
		return nil, classifySDKError(err)
	}

	var models []ModelInfo
	for _, m := range page.Data {
		models = append(models, ModelInfo{
			ID:          m.ID,
			DisplayName: m.DisplayName,
			CreatedAt:   m.CreatedAt,
		})
	}
	return models, nil
}

// listModelsRaw lists models using raw HTTP.
func (c *Client) listModelsRaw() ([]ModelInfo, error) {
	token, err := c.resolveToken()
	if err != nil {
		return nil, fmt.Errorf("resolve token: %w", err)
	}

	httpReq, err := http.NewRequest("GET", c.baseURL+"/v1/models?limit=100", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		return nil, &APIError{
			StatusCode: httpResp.StatusCode,
			Body:       string(respBody),
		}
	}

	var resp struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return resp.Data, nil
}
