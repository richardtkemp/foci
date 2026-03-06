package agent

import (
	"fmt"
	"sync"
	"time"

	"foci/internal/mana"
)

// RateLimitGate blocks all session injection when the API is rate-limited.
// When closed, messages are queued for replay when the limit expires.
type RateLimitGate struct {
	mu             sync.Mutex
	until          time.Time    // zero = not rate-limited
	queue          []QueuedItem // pending work to replay on open
	noHeaderStreak int          // consecutive closes without a retry-after header
}

// QueuedItem is a pending message queued while rate-limited.
type QueuedItem struct {
	SessionKey string
	Message    string
	Trigger    string // original trigger context (e.g. "user", "keepalive")
}

// RateLimitedError is returned when the gate blocks a message.
// Callers can type-assert to access the reset time.
type RateLimitedError struct {
	Until time.Time
}

func (e *RateLimitedError) Error() string {
	resetStr := mana.ParseResetTime(e.Until.Format(time.RFC3339Nano))
	if resetStr == "" {
		resetStr = e.Until.Format(time.Kitchen)
	}
	return fmt.Sprintf("rate limited (resets %s)", resetStr)
}

// Close marks the gate as rate-limited until the given time.
func (g *RateLimitGate) Close(until time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.until = until
}

// IsLimited returns true if the gate is currently closed (rate-limited).
func (g *RateLimitGate) IsLimited() (limited bool, until time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.until.IsZero() || time.Now().After(g.until) {
		return false, time.Time{}
	}
	return true, g.until
}

// Enqueue adds a message to the queue for replay when the gate opens.
func (g *RateLimitGate) Enqueue(sessionKey, message, trigger string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.queue = append(g.queue, QueuedItem{
		SessionKey: sessionKey,
		Message:    message,
		Trigger:    trigger,
	})
}

// ComputeResetTime determines the best reset time for the rate limit gate.
// Uses retry-after header if available (and resets the backoff streak).
// Without a header, applies exponential backoff: 60s, 120s, 240s, ... capped
// at 1h. Each consecutive no-header 429 doubles the wait; a successful header
// or gate drain resets the streak.
func (g *RateLimitGate) ComputeResetTime(retryAfterSec int) time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()

	if retryAfterSec > 0 {
		g.noHeaderStreak = 0
		return time.Now().Add(time.Duration(retryAfterSec) * time.Second)
	}

	backoff := 60 * time.Second
	for i := 0; i < g.noHeaderStreak; i++ {
		backoff *= 2
		if backoff >= time.Hour {
			backoff = time.Hour
			break
		}
	}
	g.noHeaderStreak++
	return time.Now().Add(backoff)
}

// DrainQueue returns and clears the queue if the gate is now open.
// Returns nil if still rate-limited.
func (g *RateLimitGate) DrainQueue() []QueuedItem {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.until.IsZero() && time.Now().Before(g.until) {
		return nil // still limited
	}
	if len(g.queue) == 0 {
		return nil
	}
	items := g.queue
	g.queue = nil
	g.until = time.Time{}
	g.noHeaderStreak = 0
	return items
}
