package agent

import (
	"fmt"
	"sync"
	"time"

	"foci/mana"
)

// RateLimitGate blocks all session injection when the API is rate-limited.
// When closed, messages are queued for replay when mana resets.
type RateLimitGate struct {
	mu    sync.Mutex
	until time.Time    // zero = not rate-limited
	queue []QueuedItem // pending work to replay on open
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
	return fmt.Sprintf("rate limited — mana exhausted (resets %s)", resetStr)
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
	g.until = time.Time{} // clear the gate
	return items
}
