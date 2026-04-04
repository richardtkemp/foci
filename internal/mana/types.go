// Package mana provides usage/quota tracking and budget logic.
//
// This file defines the provider-neutral types for usage data. Implementers
// (anthropic.UsageClient, ccstream.RateLimitState) import these types;
// consumers (agent, command) use the UsageClient interface without knowing
// which provider supplied the data.
package mana

import (
	"context"
	"time"
)

// UsageWindow represents a single rate limit window from any provider.
// The implementer decides which window is primary (e.g. Anthropic's 5-hour).
type UsageWindow struct {
	Utilization *float64      // 0–1 fraction of the window consumed; nil if unknown
	Period      time.Duration // window duration (e.g. 5h, 168h)
	ResetsAt    time.Time     // when this window resets; zero if unknown
	ExtraInfo   string        // optional provider-specific display text (e.g. overage info)
}

// UsageClient provides access to usage/quota information.
// Implementations may be pull-based (HTTP API with caching) or push-based
// (stream events with no-op Invalidate/SetCacheTTL).
type UsageClient interface {
	GetUsage(ctx context.Context) (*UsageWindow, error)
	Invalidate()
	SetCacheTTL(d time.Duration)
}

// UsageClientProvider resolves a UsageClient for a given endpoint name.
// Used for per-session resolution when a user switches endpoints at runtime.
type UsageClientProvider interface {
	GetUsageClient(endpoint string) UsageClient
}
