package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/provider"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// Client is an Anthropic API client with prompt caching support.
type Client struct {
	tokenFunc      func() (string, error) // returns the current Bearer token
	httpClient     *http.Client
	baseURL        string
	retryBaseDelay time.Duration // initial backoff for server error retries; 0 = default 2s

	overloadMu      sync.Mutex
	overloadRecover chan struct{} // closed to signal recovery from 529; nil when not overloaded

	// Lazy SDK client — initialized once on first SDK call.
	sdkOnce   sync.Once
	sdkClient *sdk.Client
}

// StaticToken wraps a fixed API key as a token function for NewClient.
func StaticToken(key string) func() (string, error) {
	return func() (string, error) { return key, nil }
}

// resolveToken returns the token to use for API requests.
func (c *Client) resolveToken() (string, error) {
	return c.tokenFunc()
}

// NewClient creates a new Anthropic API client.
// tokenFunc is called on each request to get the current Bearer token.
// Use StaticToken to wrap a fixed API key.
func NewClient(tokenFunc func() (string, error), timeout time.Duration) *Client {
	return &Client{
		tokenFunc:  tokenFunc,
		httpClient: &http.Client{Timeout: timeout},
		baseURL:    "https://api.anthropic.com",
	}
}

// SetBaseURL overrides the API base URL. Must be called before any API requests.
func (c *Client) SetBaseURL(url string) {
	c.baseURL = url
}

// Endpoint returns a human-readable name for this client's API endpoint.
func (c *Client) Endpoint() string {
	if strings.Contains(c.baseURL, "anthropic.com") {
		return "Anthropic API"
	}
	return provider.EndpointNameFromURL(c.baseURL)
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

// SetRetryBaseDelay sets the base delay for standard retries (phase 1).
// Tests use this to avoid multi-second backoff.
func (c *Client) SetRetryBaseDelay(d time.Duration) { c.retryBaseDelay = d }

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

// ServerErrorMaxDuration returns the maximum duration for extended server error (5xx) retries.
// Production: 5min. Test mode (retryBaseDelay < 1s): retryBaseDelay * 100.
func (c *Client) ServerErrorMaxDuration() time.Duration {
	if c.retryBaseDelay > 0 && c.retryBaseDelay < time.Second {
		return c.retryBaseDelay * 100
	}
	return 5 * time.Minute
}

// overloadBaseDelay returns the initial backoff for the extended retry loop.
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

// sendOnce sends a message using the official Anthropic SDK.
func (c *Client) sendOnce(ctx context.Context, req *MessageRequest) (*MessageResponse, error) {
	token, err := c.resolveToken()
	if err != nil {
		return nil, fmt.Errorf("resolve token: %w", err)
	}

	params := buildSDKParams(req)
	wireReq, _ := json.Marshal(params)
	sc := c.ensureSDKClient()

	log.Debugf("anthropic", "sdk_call_start: model=%s", req.Model)
	callStart := time.Now()

	msg, err := sc.Messages.New(ctx, params, sdkRequestOptions(token, req.Speed)...)

	callDur := time.Since(callStart)
	if err != nil {
		log.Debugf("anthropic", "sdk_call_error: duration=%s error=%v", callDur, err)
		sdkErr := classifySDKError(err)
		attachWireRequest(sdkErr, wireReq)
		return nil, sdkErr
	}
	log.Debugf("anthropic", "sdk_call_done: duration=%s stop_reason=%s", callDur, msg.StopReason)

	resp := responseFromSDK(msg)
	resp.WireRequest = wireReq
	resp.KeySuffix = log.FormatKeySuffix(token)
	return resp, nil
}

// SendMessage sends a message request and returns the response.
// Retry logic is handled by the provider layer.
func (c *Client) SendMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error) {
	// Strip developer prefix (e.g., "anthropic/claude-opus-4-6" → "claude-opus-4-6").
	req.Model = config.StripDeveloperPrefix(req.Model)

	// Strip params the target model doesn't support (avoids 400 errors).
	stripUnsupportedParams(req)

	return c.sendOnce(ctx, req)
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
	if req.Speed != "" && !caps.Speed {
		req.Speed = ""
	}
}

// CountTokens calls the /v1/messages/count_tokens endpoint to get exact
// input token counts for a request. The endpoint is free (no tokens billed).
func (c *Client) CountTokens(ctx context.Context, req *MessageRequest) (int, error) {
	token, err := c.resolveToken()
	if err != nil {
		return 0, fmt.Errorf("resolve token: %w", err)
	}

	params := buildSDKCountParams(req)
	sc := c.ensureSDKClient()

	result, err := sc.Messages.CountTokens(ctx, params, sdkRequestOptions(token, "")...)
	if err != nil {
		return 0, classifySDKError(err)
	}
	return int(result.InputTokens), nil
}

// IsCachingAvailable returns true as Anthropic prompt caching is always available.
func (c *Client) IsCachingAvailable() bool {
	return true
}

// ListModels calls the /v1/models endpoint to list available models.
func (c *Client) ListModels() ([]ModelInfo, error) {
	token, err := c.resolveToken()
	if err != nil {
		return nil, fmt.Errorf("resolve token: %w", err)
	}

	sc := c.ensureSDKClient()
	page, err := sc.Models.List(context.Background(), sdk.ModelListParams{
		Limit: param.NewOpt(int64(100)),
	}, sdkRequestOptions(token, "")...)
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
