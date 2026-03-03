// Package mana provides mana budget logic for background work throttling.
//
// Pure math + cached poller. No coupling beyond foci/anthropic and foci/log.
package mana

import (
	"context"
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
			m.cachedMana = 100 - *usage.FiveHour.Utilization
			if m.cachedMana < 0 {
				m.cachedMana = 0
			}
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
