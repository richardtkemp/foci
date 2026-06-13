package ccstream

import (
	"context"
	"sync"
	"time"

	"foci/internal/mana"
)

// rateLimitTypePeriod maps CC's rateLimitType strings to window durations.
var rateLimitTypePeriod = map[string]time.Duration{
	"five_hour":        5 * time.Hour,
	"seven_day":        7 * 24 * time.Hour,
	"seven_day_opus":   7 * 24 * time.Hour,
	"seven_day_sonnet": 7 * 24 * time.Hour,
}

// RateLimitState holds the latest rate limit info from CC's rate_limit_event
// stream. Shared across all backends for an agent (rate limits are account-wide).
// Implements mana.UsageClient directly — no separate adapter needed.
type RateLimitState struct {
	mu   sync.Mutex
	info *RateLimitInfo
}

// Update stores the latest rate limit info. Called by Backend.OnRateLimit.
func (s *RateLimitState) Update(info *RateLimitInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.info = info
}

// GetUsage returns the cached rate limit data as a provider-neutral UsageWindow.
// Returns (nil, nil) if no rate_limit_event has been received yet.
func (s *RateLimitState) GetUsage(_ context.Context) (*mana.UsageWindow, error) {
	s.mu.Lock()
	info := s.info
	s.mu.Unlock()

	if info == nil {
		return nil, nil
	}

	w := &mana.UsageWindow{}

	// Map utilization (0–1 passthrough — CC already uses 0–1 scale).
	if info.Utilization != nil {
		u := *info.Utilization
		w.Utilization = &u
	}

	// Map reset time (unix epoch seconds → time.Time).
	if info.ResetsAt != nil {
		sec := int64(*info.ResetsAt)
		nsec := int64((*info.ResetsAt - float64(sec)) * 1e9)
		w.ResetsAt = time.Unix(sec, nsec)
	}

	// Map rate limit type to period.
	if p, ok := rateLimitTypePeriod[info.RateLimitType]; ok {
		w.Period = p
	} else {
		w.Period = 5 * time.Hour // default to 5h
	}

	return w, nil
}

// Invalidate is a no-op — data is push-based from the CC stream.
func (s *RateLimitState) Invalidate() {}

// SetCacheTTL is a no-op — data is push-based from the CC stream.
func (s *RateLimitState) SetCacheTTL(time.Duration) {}
