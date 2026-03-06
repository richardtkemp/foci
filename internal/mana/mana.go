// Package mana provides mana budget logic for background work throttling.
//
// Pure math + usage formatting. Caching is handled by UsageClient.
// No coupling beyond foci/provider and foci/log.
package mana

import (
	"context"
	"fmt"
	"strings"
	"time"

	"foci/internal/log"
	"foci/internal/provider"
)

const (
	// Window is the 5-hour mana budget window used by the Anthropic API.
	Window = 5 * time.Hour
)

// FromUtilization converts a provider utilization percentage (0-100) to mana
// (available quota). Mana = 100 - utilization, clamped to [0, 100].
// Provider-agnostic: works with any API that reports utilization out of 100%.
func FromUtilization(utilization float64) float64 {
	m := 100 - utilization
	if m < 0 {
		return 0
	}
	return m
}

// formatPercentValue formats a percentage as a compact string (decimal for <1%, integer for ≥1%).
func formatPercentValue(percent float64) string {
	if percent < 1 {
		return fmt.Sprintf("%.1f%%", percent)
	}
	return fmt.Sprintf("%.0f%%", percent)
}

// FormatPercent returns a compact mana percentage string from usage data.
// Returns "" if unavailable.
func FormatPercent(usage *provider.UsageResponse) string {
	if usage == nil || usage.FiveHour == nil || usage.FiveHour.Utilization == nil {
		return ""
	}
	m := FromUtilization(*usage.FiveHour.Utilization)
	return formatPercentValue(m)
}

// FormatReset returns a human-readable reset time string from usage data.
// Returns "" if no reset time available.
func FormatReset(usage *provider.UsageResponse) string {
	if usage == nil || usage.FiveHour == nil || usage.FiveHour.ResetsAt == nil {
		return ""
	}
	return ParseResetTime(*usage.FiveHour.ResetsAt)
}

// FormatUsage returns a human-readable usage summary string.
func FormatUsage(usage *provider.UsageResponse) string {
	if usage == nil {
		return "No usage data"
	}

	var parts []string

	if usage.FiveHour != nil && usage.FiveHour.Utilization != nil {
		util := *usage.FiveHour.Utilization
		parts = append(parts, fmt.Sprintf("%s used", formatPercentValue(util)))

		if usage.FiveHour.ResetsAt != nil {
			if resetTime := ParseResetTime(*usage.FiveHour.ResetsAt); resetTime != "" {
				parts = append(parts, fmt.Sprintf("resets %s", resetTime))
			}
		}
	}

	if usage.ExtraUsage != nil && usage.ExtraUsage.IsEnabled && usage.ExtraUsage.UsedCredits > 0 {
		parts = append(parts, fmt.Sprintf("overage $%.2f", usage.ExtraUsage.UsedCredits))
	}

	if len(parts) == 0 {
		return "No active usage limits"
	}

	return strings.Join(parts, ", ")
}

// ParseResetTime converts ISO timestamp to human-readable relative time.
// Returns formats like "2pm", "in 2h", "in 45m", or "" if parsing fails.
func ParseResetTime(isoTime string) string {
	t, err := time.Parse(time.RFC3339Nano, isoTime)
	if err != nil {
		return ""
	}

	now := time.Now().UTC()
	until := t.Sub(now)

	if until > 24*time.Hour {
		return t.Format("2pm")
	}
	if until < 0 {
		return "now"
	}
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

// IsGood implements the manamometer check.
//
// Logic:
//  1. Calculate time_since_reset = window - (resetsAt - now)
//  2. If time_since_reset < investInterval: return false (investing period)
//  3. expected_mana = 100 * (window - time_since_reset) / (window - investInterval)
//  4. Return actualMana > expectedMana
func IsGood(actualMana float64, resetsAt time.Time, investInterval time.Duration, now time.Time) bool {
	if resetsAt.IsZero() {
		return false // no data = don't spend
	}

	timeSinceReset := Window - resetsAt.Sub(now)
	if timeSinceReset < 0 {
		timeSinceReset = 0
	}

	// Investing period — don't spend
	if timeSinceReset < investInterval {
		return false
	}

	// Linear interpolation: expected mana line from 100% at investInterval to 0% at window end
	denominator := Window - investInterval
	if denominator <= 0 {
		return actualMana > 0
	}

	expectedMana := 100 * float64(Window-timeSinceReset) / float64(denominator)

	return actualMana > expectedMana
}

// Monitor wraps a UsageClient for mana state checks.
// Caching is handled by UsageClient; Monitor just calls GetUsage and evaluates.
type Monitor struct {
	log         *log.ComponentLogger
	usageClient provider.UsageClient                        // static client (if not using getClient)
	getClient   func() provider.UsageClient                // dynamic client getter (for session-aware)
}

// NewMonitor creates a Monitor. If usageClient is nil, IsGoodFor always returns true.
func NewMonitor(usageClient provider.UsageClient) *Monitor {
	return &Monitor{
		log:         log.NewComponentLogger("mana"),
		usageClient: usageClient,
	}
}

// NewMonitorWithFunc creates a Monitor that lazily resolves UsageClient on each check.
// Used by keepalive to track default session's current endpoint.
func NewMonitorWithFunc(getClient func() provider.UsageClient) *Monitor {
	return &Monitor{
		log:       log.NewComponentLogger("mana"),
		getClient: getClient,
	}
}

// IsGoodFor checks whether we can afford to run background work with the given invest interval.
// Calls GetUsage (which is cached by UsageClient) and evaluates via IsGood.
func (m *Monitor) IsGoodFor(ctx context.Context, investInterval time.Duration) bool {
	var client provider.UsageClient
	if m.getClient != nil {
		client = m.getClient()  // Lazily resolve
	} else {
		client = m.usageClient  // Static
	}

	if client == nil {
		return true // no usage client = no rate limiting
	}

	usage, err := client.GetUsage(ctx)
	if err != nil {
		m.log.Warnf("usage API: %v", err)
		return false
	}

	manaVal, resetsAt := extractManaAndReset(usage)
	return IsGood(manaVal, resetsAt, investInterval, time.Now())
}

// ManaAndReset returns mana percentage, reset time strings, and whether
// mana is "good" (above invest threshold). Returns empty strings and false if
// UsageClient is nil or on error.
func ManaAndReset(usageClient provider.UsageClient, investInterval time.Duration) (pct, reset string, good bool) {
	if usageClient == nil {
		return "", "", false
	}

	usage, err := usageClient.GetUsage(context.Background())
	if err != nil {
		return "", "", false
	}

	pct = FormatPercent(usage)
	reset = FormatReset(usage)
	good = computeManaGood(usage, investInterval)
	return pct, reset, good
}

// extractManaAndReset extracts mana value and reset time from a usage response.
func extractManaAndReset(usage *provider.UsageResponse) (float64, time.Time) {
	var manaVal float64
	var resetsAt time.Time
	if usage != nil && usage.FiveHour != nil {
		if usage.FiveHour.Utilization != nil {
			manaVal = FromUtilization(*usage.FiveHour.Utilization)
		}
		if usage.FiveHour.ResetsAt != nil {
			resetsAt, _ = time.Parse(time.RFC3339Nano, *usage.FiveHour.ResetsAt)
		}
	}
	return manaVal, resetsAt
}

// computeManaGood evaluates whether current mana is above the invest threshold.
func computeManaGood(usage *provider.UsageResponse, investInterval time.Duration) bool {
	if investInterval == 0 {
		return false
	}
	if usage == nil || usage.FiveHour == nil || usage.FiveHour.Utilization == nil {
		return false
	}
	manaVal, resetsAt := extractManaAndReset(usage)
	return IsGood(manaVal, resetsAt, investInterval, time.Now())
}
