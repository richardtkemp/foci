// Package mana provides usage/quota tracking and budget logic for background
// work throttling.
//
// Pure math + usage formatting. Caching is handled by UsageClient implementations
// (anthropic.UsageClient for API agents, ccstream.RateLimitState for delegated).
package mana

import (
	"context"
	"fmt"
	"time"

	"foci/internal/log"
)

const (
	// Window is the 5-hour mana budget window used by the Anthropic API.
	Window = 5 * time.Hour
)

// FromUtilization converts a utilization fraction (0–1) to mana percentage
// (available quota, 0–100). Mana = (1 - utilization) * 100, clamped to [0, 100].
func FromUtilization(utilization float64) float64 {
	m := (1 - utilization) * 100
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
func FormatPercent(w *UsageWindow) string {
	if w == nil || w.Utilization == nil {
		return ""
	}
	m := FromUtilization(*w.Utilization)
	return formatPercentValue(m)
}

// FormatReset returns a human-readable reset time string from usage data.
// Returns "" if no reset time available.
func FormatReset(w *UsageWindow) string {
	if w == nil || w.ResetsAt.IsZero() {
		return ""
	}
	return FormatResetTime(w.ResetsAt)
}

// ParseResetTime converts an ISO timestamp to a human-readable relative time.
// Kept for callers that have a string; prefer FormatResetTime for time.Time values.
func ParseResetTime(isoTime string) string {
	t, err := time.Parse(time.RFC3339Nano, isoTime)
	if err != nil {
		return ""
	}
	return FormatResetTime(t)
}

// FormatResetTime converts a time to a human-readable relative string.
// Returns formats like "2pm", "in 2h", "in 45m", etc.
func FormatResetTime(t time.Time) string {
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
	usageClient UsageClient                        // static client (if not using getClient)
	getClient   func() UsageClient                // dynamic client getter (for session-aware)
}

// NewMonitor creates a Monitor. If usageClient is nil, IsGoodFor returns false
// (no client means we can't verify mana availability — conservatively assume insufficient).
func NewMonitor(usageClient UsageClient) *Monitor {
	return &Monitor{
		log:         log.NewComponentLogger("mana"),
		usageClient: usageClient,
	}
}

// IsGoodFor checks whether we can afford to run background work with the given invest interval.
// Calls GetUsage (which is cached by UsageClient) and evaluates via IsGood.
func (m *Monitor) IsGoodFor(ctx context.Context, investInterval time.Duration) bool {
	var client UsageClient
	if m.getClient != nil {
		client = m.getClient() // Lazily resolve
	} else {
		client = m.usageClient // Static
	}

	if client == nil {
		return false // no usage client = can't verify mana; assume insufficient
	}

	w, err := client.GetUsage(ctx)
	if err != nil {
		m.log.Warnf("usage API: %v", err)
		return false
	}

	manaVal, resetsAt := extractManaAndReset(w)
	return IsGood(manaVal, resetsAt, investInterval, time.Now())
}

// ManaAndReset returns mana percentage, reset time strings, and whether
// mana is "good" (above invest threshold). Returns empty strings and false if
// UsageClient is nil or on error.
func ManaAndReset(usageClient UsageClient, investInterval time.Duration) (pct, reset string, good bool) {
	if usageClient == nil {
		return "", "", false
	}

	w, err := usageClient.GetUsage(context.Background())
	if err != nil {
		return "", "", false
	}

	pct = FormatPercent(w)
	reset = FormatReset(w)
	good = computeManaGood(w, investInterval)
	return pct, reset, good
}

// extractManaAndReset extracts mana value and reset time from a usage window.
func extractManaAndReset(w *UsageWindow) (float64, time.Time) {
	var manaVal float64
	var resetsAt time.Time
	if w != nil {
		if w.Utilization != nil {
			manaVal = FromUtilization(*w.Utilization)
		}
		resetsAt = w.ResetsAt
	}
	return manaVal, resetsAt
}

// computeManaGood evaluates whether current mana is above the invest threshold.
func computeManaGood(w *UsageWindow, investInterval time.Duration) bool {
	if investInterval == 0 {
		return false
	}
	if w == nil || w.Utilization == nil {
		return false
	}
	manaVal, resetsAt := extractManaAndReset(w)
	return IsGood(manaVal, resetsAt, investInterval, time.Now())
}
