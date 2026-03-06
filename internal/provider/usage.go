package provider

import (
	"context"
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

// UsageClient provides access to provider usage/quota information.
type UsageClient interface {
	GetUsage(ctx context.Context) (*UsageResponse, error)
	Invalidate()
	SetCacheTTL(d time.Duration)
}
