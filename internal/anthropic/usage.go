package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"foci/internal/mana"
)

// tokenResolutionError wraps token resolution failures. These errors should
// not trigger error backoff because the token can be refreshed at any moment
// (e.g., background OAuth refresh) and we want to pick it up immediately.
type tokenResolutionError struct {
	err error
}

func (e *tokenResolutionError) Error() string { return e.err.Error() }
func (e *tokenResolutionError) Unwrap() error { return e.err }

const (
	// defaultCacheTTL is the default cache duration for usage API responses.
	defaultCacheTTL = 5 * time.Minute

	// maxErrBackoff caps exponential backoff for consecutive fetch errors.
	maxErrBackoff = 1 * time.Hour
)

// usageAPIResponse is the raw JSON shape from the Anthropic usage API.
// All fields are Anthropic-specific; the public GetUsage method maps these
// to the provider-neutral *mana.UsageWindow.
type usageAPIResponse struct {
	FiveHour          *usageAPIWindow `json:"five_hour"`
	SevenDay          *usageAPIWindow `json:"seven_day"`
	SevenDayOAuthApps *usageAPIWindow `json:"seven_day_oauth_apps"`
	SevenDayOpus      *usageAPIWindow `json:"seven_day_opus"`
	SevenDaySonnet    *usageAPIWindow `json:"seven_day_sonnet"`
	SevenDayCowork    *usageAPIWindow `json:"seven_day_cowork"`
	IguanaNecktie     *usageAPIWindow `json:"iguana_necktie"`
	ExtraUsage        *usageAPIExtra  `json:"extra_usage"`
}

type usageAPIWindow struct {
	Utilization *float64 `json:"utilization"` // 0–100
	ResetsAt    *string  `json:"resets_at"`   // ISO timestamp
}

type usageAPIExtra struct {
	IsEnabled    bool    `json:"is_enabled"`
	MonthlyLimit float64 `json:"monthly_limit"`
	UsedCredits  float64 `json:"used_credits"`
}

// UsageClient is a client for the Anthropic usage API (requires OAuth token).
// Implements mana.UsageClient.
type UsageClient struct {
	tokenFunc  func() (string, error) // returns the current OAuth token
	httpClient *http.Client
	baseURL    string

	mu       sync.Mutex
	cached   *mana.UsageWindow
	cachedAt time.Time
	cacheTTL time.Duration

	// Error backoff state: after a fetch failure, suppress retries with
	// exponential backoff starting at cacheTTL and doubling up to maxErrBackoff.
	lastErr    error
	lastErrAt  time.Time
	errBackoff time.Duration

	// postFetch is called after a successful API fetch, e.g. to check token
	// expiry and trigger a proactive refresh.
	postFetch func()
}

// NewUsageClient creates a new usage API client.
// tokenFunc is called on each request to get the current OAuth token.
func NewUsageClient(tokenFunc func() (string, error)) *UsageClient {
	return &UsageClient{
		tokenFunc:  tokenFunc,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    "https://api.anthropic.com",
		cacheTTL:   defaultCacheTTL,
	}
}

// SetBaseURL overrides the API base URL.
func (c *UsageClient) SetBaseURL(url string) {
	c.baseURL = url
}

// SetCacheTTL sets the cache TTL for usage API responses.
func (c *UsageClient) SetCacheTTL(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cacheTTL = d
}

// SetPostFetchHook sets a function called after each successful API fetch.
// Used to trigger proactive token refresh when credentials are near expiry.
func (c *UsageClient) SetPostFetchHook(fn func()) {
	c.postFetch = fn
}

// Invalidate clears the cached usage response and error backoff state, forcing
// the next GetUsage call to fetch from the API. Useful for /mana force-refresh or tests.
func (c *UsageClient) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = nil
	c.cachedAt = time.Time{}
	c.lastErr = nil
	c.errBackoff = 0
}

// resolveToken returns the token to use for usage API requests.
// Errors are wrapped as tokenResolutionError to distinguish them from API errors.
func (c *UsageClient) resolveToken() (string, error) {
	tok, err := c.tokenFunc()
	if err != nil {
		return "", &tokenResolutionError{err}
	}
	return tok, nil
}

// GetUsage retrieves the current usage from the Anthropic API.
// Returns the 5-hour window as the primary mana window.
// Results are cached for cacheTTL; concurrent callers share the same cached value.
// On fetch errors, retries are suppressed with exponential backoff (starting at
// cacheTTL, doubling up to maxErrBackoff). A successful fetch resets the backoff.
func (c *UsageClient) GetUsage(ctx context.Context) (*mana.UsageWindow, error) {
	c.mu.Lock()
	if c.cached != nil && time.Since(c.cachedAt) < c.cacheTTL {
		resp := c.cached
		c.mu.Unlock()
		return resp, nil
	}
	// In error backoff — return last error without retrying.
	if c.lastErr != nil && time.Since(c.lastErrAt) < c.errBackoff {
		err := c.lastErr
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Unlock()

	resp, err := c.fetchUsage(ctx)
	if err != nil {
		// Don't apply error backoff for token resolution errors — the token
		// can be refreshed at any moment and we should retry immediately.
		var te *tokenResolutionError
		if !errors.As(err, &te) {
			c.mu.Lock()
			if c.errBackoff == 0 {
				c.errBackoff = c.cacheTTL
			} else {
				c.errBackoff *= 2
			}
			if c.errBackoff > maxErrBackoff {
				c.errBackoff = maxErrBackoff
			}
			c.lastErr = err
			c.lastErrAt = time.Now()
			c.mu.Unlock()
		}
		return nil, err
	}

	c.mu.Lock()
	c.cached = resp
	c.cachedAt = time.Now()
	c.lastErr = nil
	c.errBackoff = 0
	c.mu.Unlock()

	// After successful fetch, trigger any post-fetch hook (e.g. token expiry check).
	if c.postFetch != nil {
		c.postFetch()
	}

	return resp, nil
}

// fetchUsage performs the actual HTTP request to the usage API and maps
// the Anthropic-specific response to a provider-neutral *mana.UsageWindow.
func (c *UsageClient) fetchUsage(ctx context.Context) (*mana.UsageWindow, error) {
	token, err := c.resolveToken()
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/oauth/usage", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("anthropic-beta", "oauth-2025-04-20")

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
		return nil, fmt.Errorf("API error (status %d): %s", httpResp.StatusCode, string(respBody))
	}

	var raw usageAPIResponse
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return mapUsageResponse(&raw), nil
}

// mapUsageResponse converts the Anthropic API response to a provider-neutral
// *mana.UsageWindow. The 5-hour window is primary; other windows and overage
// info are formatted into ExtraInfo for display.
func mapUsageResponse(raw *usageAPIResponse) *mana.UsageWindow {
	w := &mana.UsageWindow{
		Period: 5 * time.Hour,
	}

	if raw.FiveHour != nil {
		if raw.FiveHour.Utilization != nil {
			u := *raw.FiveHour.Utilization / 100 // API 0–100 → interface 0–1
			w.Utilization = &u
		}
		if raw.FiveHour.ResetsAt != nil {
			if t, err := time.Parse(time.RFC3339Nano, *raw.FiveHour.ResetsAt); err == nil {
				w.ResetsAt = t
			}
		}
	}

	w.ExtraInfo = formatExtraInfo(raw)
	return w
}

// formatExtraInfo builds a compact display string from secondary windows
// and overage billing data. Returns "" if nothing noteworthy.
func formatExtraInfo(raw *usageAPIResponse) string {
	var parts []string

	// Overage billing
	if raw.ExtraUsage != nil && raw.ExtraUsage.IsEnabled {
		parts = append(parts, fmt.Sprintf("Overage: $%.2f / $%.2f",
			raw.ExtraUsage.UsedCredits, raw.ExtraUsage.MonthlyLimit))
	}

	// 7-day window utilization
	if raw.SevenDay != nil && raw.SevenDay.Utilization != nil {
		parts = append(parts, fmt.Sprintf("7-day: %.0f%%", *raw.SevenDay.Utilization))
	}

	if len(parts) == 0 {
		return ""
	}

	result := parts[0]
	for _, p := range parts[1:] {
		result += " | " + p
	}
	return result
}
