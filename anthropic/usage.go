package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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

// GetUsage retrieves the current usage from the Anthropic API
func (c *UsageClient) GetUsage(ctx context.Context) (*UsageResponse, error) {
	if c.oauthToken == "" {
		return nil, fmt.Errorf("OAuth token not configured")
	}

	httpReq, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/oauth/usage", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Authorization", "Bearer "+c.oauthToken)
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

// FormatMana returns a compact mana percentage string from usage data.
// Mana = 100 - utilization (5-hour window). Returns "" if unavailable.
func FormatMana(usage *UsageResponse) string {
	if usage == nil || usage.FiveHour == nil || usage.FiveHour.Utilization == nil {
		return ""
	}
	mana := 100 - *usage.FiveHour.Utilization
	if mana < 0 {
		mana = 0
	}
	if mana < 1 {
		return fmt.Sprintf("%.1f%%", mana)
	}
	return fmt.Sprintf("%.0f%%", mana)
}

// FormatManaReset returns a human-readable reset time string from usage data.
// Returns "" if no reset time available.
func FormatManaReset(usage *UsageResponse) string {
	if usage == nil || usage.FiveHour == nil || usage.FiveHour.ResetsAt == nil {
		return ""
	}
	return parseResetTime(*usage.FiveHour.ResetsAt)
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

	// Show relative time (in Xh Ym, in Xm, etc)
	if until < time.Minute {
		return "in <1m"
	}
	if until < time.Hour {
		return fmt.Sprintf("in %dm", int(until.Minutes()))
	}
	hours := int(until.Hours())
	mins := int(until.Minutes()) % 60
	if mins == 0 {
		return fmt.Sprintf("in %dh", hours)
	}
	return fmt.Sprintf("in %dh %dm", hours, mins)
}
