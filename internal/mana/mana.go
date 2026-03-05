// Package mana provides mana budget logic for background work throttling.
//
// Pure math + cached poller. No coupling beyond foci/anthropic and foci/log.
package mana

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"foci/internal/anthropic"
	"foci/internal/log"
)

const (
	// Window is the 5-hour mana budget window used by the Anthropic API.
	Window = 5 * time.Hour

	// PollInterval is the minimum interval between usage API calls.
	PollInterval = 60 * time.Second
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
func FormatPercent(usage *anthropic.UsageResponse) string {
	if usage == nil || usage.FiveHour == nil || usage.FiveHour.Utilization == nil {
		return ""
	}
	m := FromUtilization(*usage.FiveHour.Utilization)
	return formatPercentValue(m)
}

// FormatReset returns a human-readable reset time string from usage data.
// Returns "" if no reset time available.
func FormatReset(usage *anthropic.UsageResponse) string {
	if usage == nil || usage.FiveHour == nil || usage.FiveHour.ResetsAt == nil {
		return ""
	}
	return ParseResetTime(*usage.FiveHour.ResetsAt)
}

// FormatUsage returns a human-readable usage summary string.
func FormatUsage(usage *anthropic.UsageResponse) string {
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

// Monitor wraps a UsageClient with cached polling for mana state.
type Monitor struct {
	log              *log.ComponentLogger
	usageClient      *anthropic.UsageClient
	stalenessTimeout time.Duration

	mu            sync.Mutex
	lastUsagePoll time.Time
	cachedMana    float64   // 0-100 (100 = fully available)
	cachedReset   time.Time
}

// NewMonitor creates a Monitor. If usageClient is nil, IsGoodFor always returns true.
func NewMonitor(usageClient *anthropic.UsageClient, stalenessTimeout time.Duration) *Monitor {
	return &Monitor{
		log:              log.NewComponentLogger("mana"),
		usageClient:      usageClient,
		stalenessTimeout: stalenessTimeout,
	}
}

// needsPoll returns true if a polling interval has elapsed since last poll.
func (m *Monitor) needsPoll() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return time.Since(m.lastUsagePoll) >= PollInterval
}

// updateCachedUsage updates cached mana and reset time from usage response.
func (m *Monitor) updateCachedUsage(usage *anthropic.UsageResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastUsagePoll = time.Now()
	if usage.FiveHour != nil && usage.FiveHour.Utilization != nil {
		m.cachedMana = FromUtilization(*usage.FiveHour.Utilization)
	}
	if usage.FiveHour != nil && usage.FiveHour.ResetsAt != nil {
		m.cachedReset, _ = time.Parse(time.RFC3339Nano, *usage.FiveHour.ResetsAt)
	}
}

// getCachedState returns the cached mana, reset time, poll age, and whether it has been polled.
func (m *Monitor) getCachedState() (mana float64, resetsAt time.Time, pollAge time.Duration, isZero bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cachedMana, m.cachedReset, time.Since(m.lastUsagePoll), m.lastUsagePoll.IsZero()
}

// IsGoodFor checks whether we can afford to run background work with the given invest interval.
func (m *Monitor) IsGoodFor(ctx context.Context, investInterval time.Duration) bool {
	if m.usageClient == nil {
		return true // no usage client = no rate limiting
	}

	if m.needsPoll() {
		usage, err := m.usageClient.GetUsage(ctx)
		if err != nil {
			m.log.Warnf("usage API: %v", err)
			return false // err on the side of caution
		}
		m.updateCachedUsage(usage)
	}

	// Staleness guard: if the last successful poll is too old, deny spending.
	manaVal, resetsAt, pollAge, isZero := m.getCachedState()

	if isZero || pollAge > m.stalenessTimeout {
		m.log.Debugf("mana stale (last poll %s ago, threshold %s)", pollAge.Round(time.Second), m.stalenessTimeout)
		return false
	}

	return IsGood(manaVal, resetsAt, investInterval, time.Now())
}
