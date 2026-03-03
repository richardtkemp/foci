package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// UsageWindow represents a usage window (5-hour, 7-day, etc.)
type UsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

// ExtraUsage represents overage billing information
type ExtraUsage struct {
	IsEnabled    bool     `json:"is_enabled"`
	MonthlyLimit float64  `json:"monthly_limit"`
	UsedCredits  float64  `json:"used_credits"`
	Utilization  *float64 `json:"utilization"`
}

// UsageResponse is the response from the usage API endpoint
type UsageResponse struct {
	FiveHour          *UsageWindow `json:"five_hour"`
	SevenDay          *UsageWindow `json:"seven_day"`
	SevenDayOAuthApps *UsageWindow `json:"seven_day_oauth_apps"`
	SevenDayOpus      *UsageWindow `json:"seven_day_opus"`
	SevenDaySonnet    *UsageWindow `json:"seven_day_sonnet"`
	SevenDayCowork    *UsageWindow `json:"seven_day_cowork"`
	IguanaNecktie     *UsageWindow `json:"iguana_necktie"`
	ExtraUsage        *ExtraUsage  `json:"extra_usage"`
}

// UsageClient is a client for the Anthropic usage API (requires OAuth token)
type UsageClient struct {
	oauthToken string
	tokenFunc  func() (string, error) // dynamic token source (overrides oauthToken)
	httpClient *http.Client
	baseURL    string
}

// NewUsageClient creates a new usage API client with a static OAuth token.
func NewUsageClient(oauthToken string) *UsageClient {
	return &UsageClient{
		oauthToken: oauthToken,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    "https://api.anthropic.com",
	}
}

// NewUsageClientWithFunc creates a usage client that calls tokenFunc for each request.
func NewUsageClientWithFunc(tokenFunc func() (string, error)) *UsageClient {
	return &UsageClient{
		tokenFunc:  tokenFunc,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    "https://api.anthropic.com",
	}
}

// resolveToken returns the token to use for usage API requests.
func (c *UsageClient) resolveToken() (string, error) {
	if c.tokenFunc != nil {
		return c.tokenFunc()
	}
	if c.oauthToken == "" {
		return "", fmt.Errorf("OAuth token not configured")
	}
	return c.oauthToken, nil
}

// GetUsage retrieves the current usage from the Anthropic API
func (c *UsageClient) GetUsage(ctx context.Context) (*UsageResponse, error) {
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

	var resp UsageResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return &resp, nil
}


