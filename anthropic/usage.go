package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// UsageWindow represents a usage window (5-hour, 7-day, etc.)
type UsageWindow struct {
	Utilization *float64  `json:"utilization"`
	ResetsAt    *string   `json:"resets_at"`
}

// ExtraUsage represents overage billing information
type ExtraUsage struct {
	IsEnabled      bool     `json:"is_enabled"`
	MonthlyLimit   float64  `json:"monthly_limit"`
	UsedCredits    float64  `json:"used_credits"`
	Utilization    *float64 `json:"utilization"`
}

// UsageResponse is the response from the usage API endpoint
type UsageResponse struct {
	FiveHour           *UsageWindow `json:"five_hour"`
	SevenDay           *UsageWindow `json:"seven_day"`
	SevenDayOAuthApps  *UsageWindow `json:"seven_day_oauth_apps"`
	SevenDayOpus       *UsageWindow `json:"seven_day_opus"`
	SevenDaySonnet     *UsageWindow `json:"seven_day_sonnet"`
	SevenDayCowork     *UsageWindow `json:"seven_day_cowork"`
	IguanaNecktie      *UsageWindow `json:"iguana_necktie"`
	ExtraUsage         *ExtraUsage  `json:"extra_usage"`
}

// UsageClient is a client for the Anthropic usage API (requires OAuth token)
type UsageClient struct {
	oauthToken string          // static token (legacy)
	tokenFunc  func() string   // dynamic token getter (preferred, overrides static)
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

// NewUsageClientWithFunc creates a usage API client that reads the token
// dynamically via a function. This supports auto-refreshing tokens.
func NewUsageClientWithFunc(tokenFunc func() string) *UsageClient {
	return &UsageClient{
		tokenFunc:  tokenFunc,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    "https://api.anthropic.com",
	}
}

// getToken returns the current OAuth token, preferring the dynamic getter.
func (c *UsageClient) getToken() string {
	if c.tokenFunc != nil {
		return c.tokenFunc()
	}
	return c.oauthToken
}

// GetUsage retrieves the current usage from the Anthropic API
func (c *UsageClient) GetUsage(ctx context.Context) (*UsageResponse, error) {
	token := c.getToken()
	if token == "" {
		return nil, fmt.Errorf("OAuth token not configured")
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
	defer httpResp.Body.Close()

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

// ReadCredentialsToken reads the OAuth access token from a Claude Code
// credentials file (typically ~/.claude/.credentials.json). The token
// is auto-refreshed by Claude Code, so this should be called on each
// usage request rather than cached at startup.
func ReadCredentialsToken(path string) (string, error) {
	// Expand ~ to home directory
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		path = home + path[1:]
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read credentials file: %w", err)
	}

	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("parse credentials: %w", err)
	}

	return creds.ClaudeAiOauth.AccessToken, nil
}

// FormatUsage returns a human-readable usage string
func FormatUsage(usage *UsageResponse) string {
	if usage == nil {
		return "No usage data"
	}

	var parts []string

	// 5-hour window
	if usage.FiveHour != nil && usage.FiveHour.Utilization != nil {
		util := *usage.FiveHour.Utilization

		// Format percentage (no decimals unless <1%)
		utilStr := ""
		if util < 1 {
			utilStr = fmt.Sprintf("%.1f%%", util)
		} else {
			utilStr = fmt.Sprintf("%.0f%%", util)
		}

		parts = append(parts, fmt.Sprintf("%s used", utilStr))

		// Format reset time
		if usage.FiveHour.ResetsAt != nil {
			resetTime := parseResetTime(*usage.FiveHour.ResetsAt)
			if resetTime != "" {
				parts = append(parts, fmt.Sprintf("resets %s", resetTime))
			}
		}
	}

	// Extra usage (only if nonzero)
	if usage.ExtraUsage != nil && usage.ExtraUsage.IsEnabled && usage.ExtraUsage.UsedCredits > 0 {
		parts = append(parts, fmt.Sprintf("overage $%.2f", usage.ExtraUsage.UsedCredits))
	}

	if len(parts) == 0 {
		return "No active usage limits"
	}

	return strings.Join(parts, ", ")
}

// parseResetTime converts ISO timestamp to human-readable time
// Returns formats like "1am", "3:30pm", "in 2h", or "" if parsing fails
func parseResetTime(isoTime string) string {
	t, err := time.Parse(time.RFC3339Nano, isoTime)
	if err != nil {
		return ""
	}

	now := time.Now().UTC()
	until := t.Sub(now)

	// If reset is more than 24 hours away, show as time
	if until > 24*time.Hour {
		return t.Format("2pm")
	}

	// If reset is very soon or in the past, show "now"
	if until < 0 {
		return "now"
	}

	// Show relative time (in Xh, in Xm, etc)
	if until < time.Minute {
		return "in <1m"
	}
	if until < time.Hour {
		return fmt.Sprintf("in %dm", int(until.Minutes()))
	}
	return fmt.Sprintf("in %dh", int(until.Hours()))
}
