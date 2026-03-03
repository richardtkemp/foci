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

	"foci/anthropic"
	"foci/log"
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

// FormatPercent returns a compact mana percentage string from usage data.
// Returns "" if unavailable.
func FormatPercent(usage *anthropic.UsageResponse) string {
	if usage == nil || usage.FiveHour == nil || usage.FiveHour.Utilization == nil {
		return ""
	}
	m := FromUtilization(*usage.FiveHour.Utilization)
	if m < 1 {
		return fmt.Sprintf("%.1f%%", m)
	}
	return fmt.Sprintf("%.0f%%", m)
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
		utilStr := ""
		if util < 1 {
			utilStr = fmt.Sprintf("%.1f%%", util)
		} else {
			utilStr = fmt.Sprintf("%.0f%%", util)
		}
		parts = append(parts, fmt.Sprintf("%s used", utilStr))

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

// IsGoodFor checks whether we can afford to run background work with the given invest interval.
func (m *Monitor) IsGoodFor(ctx context.Context, investInterval time.Duration) bool {
	if m.usageClient == nil {
		return true // no usage client = no rate limiting
	}

	m.mu.Lock()
	needPoll := time.Since(m.lastUsagePoll) >= PollInterval
	m.mu.Unlock()

	if needPoll {
		usage, err := m.usageClient.GetUsage(ctx)
		if err != nil {
			m.log.Warnf("usage API: %v", err)
			return false // err on the side of caution
		}

		m.mu.Lock()
		m.lastUsagePoll = time.Now()
		if usage.FiveHour != nil && usage.FiveHour.Utilization != nil {
			m.cachedMana = FromUtilization(*usage.FiveHour.Utilization)
		}
		if usage.FiveHour != nil && usage.FiveHour.ResetsAt != nil {
			m.cachedReset, _ = time.Parse(time.RFC3339Nano, *usage.FiveHour.ResetsAt)
		}
		m.mu.Unlock()
	}

	// Staleness guard: if the last successful poll is too old, deny spending.
	m.mu.Lock()
	pollAge := time.Since(m.lastUsagePoll)
	staleThreshold := m.stalenessTimeout
	m.mu.Unlock()

	if m.lastUsagePoll.IsZero() || pollAge > staleThreshold {
		m.log.Debugf("mana stale (last poll %s ago, threshold %s)", pollAge.Round(time.Second), staleThreshold)
		return false
	}

	m.mu.Lock()
	manaVal := m.cachedMana
	resetsAt := m.cachedReset
	m.mu.Unlock()

	return IsGood(manaVal, resetsAt, investInterval, time.Now())
}
