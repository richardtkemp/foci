package agent

import (
	"context"
	"errors"
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

// DrainRateLimitQueue checks if any rate limit gates have opened, and replays
// queued messages through HandleMessage. Called from keepalive tick.
func (a *Agent) DrainRateLimitQueue(ctx context.Context) {
	a.rateLimitGatesMu.RLock()
	gates := make(map[string]*RateLimitGate, len(a.rateLimitGates))
	for endpoint, gate := range a.rateLimitGates {
		gates[endpoint] = gate
	}
	a.rateLimitGatesMu.RUnlock()

	// Drain each endpoint's queue
	for endpoint, gate := range gates {
		items := gate.DrainQueue()
		if len(items) == 0 {
			continue
		}

		a.logger().Infof("rate limit lifted for %s, replaying %d queued items", endpoint, len(items))
		for i, item := range items {
			itemCtx := WithTrigger(ctx, item.Trigger)
			if err := a.HandleMessage(itemCtx, item.SessionKey, []string{item.Message}, nil); err != nil {
				a.logger().Errorf("replay queued message session=%s trigger=%s: %v", item.SessionKey, item.Trigger, err)
				// If we hit another rate limit on THIS endpoint, stop replaying its queue
				var rlErr *RateLimitedError
				if errors.As(err, &rlErr) {
					a.logger().Infof("rate limited again during replay on %s, %d items re-queued", endpoint, len(items)-i)
					break // stop replaying THIS endpoint, but continue with others
				}
				continue
			}
			a.logger().Debugf("replayed message session=%s trigger=%s", item.SessionKey, item.Trigger)
		}
	}
}

// getOrCreateRateLimitGate returns the rate limit gate for the given endpoint,
// creating it if it doesn't exist yet. Thread-safe.
func (a *Agent) getOrCreateRateLimitGate(endpoint string) *RateLimitGate {
	if endpoint == "" {
		endpoint = a.Endpoint
	}

	// Fast path: read lock
	a.rateLimitGatesMu.RLock()
	if gate, ok := a.rateLimitGates[endpoint]; ok {
		a.rateLimitGatesMu.RUnlock()
		return gate
	}
	a.rateLimitGatesMu.RUnlock()

	// Slow path: write lock
	a.rateLimitGatesMu.Lock()
	defer a.rateLimitGatesMu.Unlock()

	// Double-check (another goroutine might have created it)
	if gate, ok := a.rateLimitGates[endpoint]; ok {
		return gate
	}

	// Lazy init map
	if a.rateLimitGates == nil {
		a.rateLimitGates = make(map[string]*RateLimitGate)
	}

	// Create new gate
	gate := &RateLimitGate{}
	a.rateLimitGates[endpoint] = gate
	return gate
}

// CanFireBackgroundOperation checks if a background operation can run on the given session.
// Returns false if:
//   - The rate limit gate for the session's endpoint is closed
//   - Mana is insufficient (session-aware check using agent's configured invest interval)
func (a *Agent) CanFireBackgroundOperation(ctx context.Context, sessionKey string) (bool, string) {
	if sessionKey == "" {
		return false, "no session key"
	}

	// Resolve session's endpoint
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	endpoint := sm.modelEndpoint
	if endpoint == "" {
		endpoint = a.Endpoint
	}
	a.metaMu.Unlock()

	// Check 1: Rate limit gate for this endpoint
	gate := a.getOrCreateRateLimitGate(endpoint)
	if limited, until := gate.IsLimited(); limited {
		resetStr := mana.ParseResetTime(until.Format(time.RFC3339Nano))
		if resetStr == "" {
			resetStr = until.Format(time.Kitchen)
		}
		return false, fmt.Sprintf("rate limited on %s (resets %s)", endpoint, resetStr)
	}

	// Check 2: Mana availability (session-aware).
	// NewMonitor(nil).IsGoodFor() returns false — no usage client means we
	// can't verify mana, so we conservatively block background work.
	if a.ManaInvestInterval > 0 {
		monitor := mana.NewMonitor(a.SessionUsageClient(sessionKey))
		if !monitor.IsGoodFor(ctx, a.ManaInvestInterval) {
			return false, "mana insufficient"
		}
	}

	return true, ""
}
