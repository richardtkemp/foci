package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/log"

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
		tok, err := c.tokenFunc()
		if err == nil {
			log.KeySuffix("anthropic", tok)
		}
		return tok, err
	}
	log.KeySuffix("anthropic", c.apiKey)
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

// OnRetrySuccess signals recovery to wake other waiting goroutines.
// Called by the provider layer when a request succeeds after retries.
func (c *Client) OnRetrySuccess() {
	c.signalRecovery()
}

// WaitForRecovery returns a channel that closes when another goroutine recovers.
// Called by the provider layer during extended overload retry.
func (c *Client) WaitForRecovery() <-chan struct{} {
	return c.enterOverload()
}

// RetryBaseDelay returns the base delay for standard retries (phase 1).
// Returns configured delay if set, otherwise 2s default. Tests can set to 1ms.
func (c *Client) RetryBaseDelay() time.Duration {
	if c.retryBaseDelay > 0 {
		return c.retryBaseDelay
	}
	return 2 * time.Second
}

// OverloadBaseDelay returns the initial backoff for extended overload retries (phase 2).
// Production: 5s. Test mode (retryBaseDelay < 1s): retryBaseDelay * 5.
func (c *Client) OverloadBaseDelay() time.Duration {
	return c.overloadBaseDelay()
}

// OverloadMaxDuration returns the maximum duration for extended overload retries (phase 2).
// Production: 2h. Test mode (retryBaseDelay < 1s): retryBaseDelay * 500.
func (c *Client) OverloadMaxDuration() time.Duration {
	return c.overloadMaxDuration()
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
	wireReq, _ := json.Marshal(params)
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

	resp := responseFromSDK(msg)
	resp.WireRequest = wireReq
	return resp, nil
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
// Retry logic is handled by the provider layer.
func (c *Client) SendMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error) {
	// Strip params the target model doesn't support (avoids 400 errors).
	// Done here so both raw and SDK paths benefit.
	stripUnsupportedParams(req)

	// For raw transport, pre-marshal the body once. SDK transport uses req directly.
	var body []byte
	if !c.useSDK {
		var err error
		body, err = json.Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
	}

	return c.sendOnce(ctx, body, req)
}

// stripUnsupportedParams removes API parameters that the target model
// doesn't support. This prevents 400 errors when effort or thinking
// is configured globally but the request targets a model like Haiku.
func stripUnsupportedParams(req *MessageRequest) {
	caps := config.ModelCapabilities(req.Model)
	if req.Output != nil && !caps.Effort {
		req.Output = nil
	}
	if req.Thinking != nil && !caps.Thinking {
		req.Thinking = nil
	}
}


// CountTokens calls the /v1/messages/count_tokens endpoint to get exact
// input token counts for a request. The endpoint is free (no tokens billed).
func (c *Client) CountTokens(ctx context.Context, req *MessageRequest) (int, error) {
	if c.useSDK {
		return c.countTokensSDK(ctx, req)
	}
	return c.countTokensRaw(ctx, req)
}

// IsCachingAvailable returns true as Anthropic prompt caching is always available.
func (c *Client) IsCachingAvailable() bool {
	return true
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
			ID:        m.ID,
			CreatedAt: m.CreatedAt,
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
		Data []struct {
			ID        string    `json:"id"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	models := make([]ModelInfo, len(resp.Data))
	for i, m := range resp.Data {
		models[i] = ModelInfo{ID: m.ID, CreatedAt: m.CreatedAt}
	}
	return models, nil
}
