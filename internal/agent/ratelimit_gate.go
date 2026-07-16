package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"foci/internal/procx"
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
	return fmt.Sprintf("rate limited (resets %s)", e.Until.Format(time.Kitchen))
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

// resolveEndpoint returns the endpoint a session's turns run against: its
// per-session override if set, else the agent default.
func (a *Agent) resolveEndpoint(sessionKey string) string {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	if sm.modelEndpoint != "" {
		return sm.modelEndpoint
	}
	return a.Endpoint
}

// SessionRateLimited reports whether the given session may NOT fire because its
// endpoint's rate-limit gate is closed (or the session key is empty). This is
// the shared gate every periodic scheduler consults — it does NOT run the
// can_run_background script (that is background-work-only). Returns (true,
// reason) when blocked, (false, "") when clear.
func (a *Agent) SessionRateLimited(sessionKey string) (limited bool, reason string) {
	if sessionKey == "" {
		return true, "no session key"
	}
	endpoint := a.resolveEndpoint(sessionKey)
	gate := a.getOrCreateRateLimitGate(endpoint)
	if lim, until := gate.IsLimited(); lim {
		return true, fmt.Sprintf("rate limited on %s (resets %s)", endpoint, until.Format(time.Kitchen))
	}
	return false, ""
}

// EngageRateLimit closes the default-endpoint gate until `until` and fires the
// RateLimit notification hooks. Delegated backends call this when their coding
// agent reports a session/usage limit through its event stream rather than a
// direct-API 429, so classifyAPIError never runs and the gate would otherwise
// stay open. Only the background/periodic paths (SessionRateLimited) honour the
// gate — user-triggered delegated turns run through the coding agent's own
// limiting.
func (a *Agent) EngageRateLimit(until time.Time) {
	if until.IsZero() || !until.After(time.Now()) {
		return
	}
	gate := a.getOrCreateRateLimitGate(a.Endpoint)
	gate.Close(until)
	a.logger().Infof("rate limit gate (%s) closed until %s (delegated usage limit hit)", a.Endpoint, until.Format(time.Kitchen))
	for _, fn := range a.RateLimitFunc {
		fn(until)
	}
}

// CanFireBackgroundOperation checks if a background operation can run on the given session.
// Returns false if:
//   - The session key is empty
//   - The rate limit gate for the session's endpoint is closed (SessionRateLimited)
//   - The configured can_run_background executable exits non-zero
//
// This is the FULL gate (rate limit + can_run_background). Only the
// background-work scheduler and the memory hooks (compaction / session-end)
// use it; the other periodic schedulers use SessionRateLimited alone, so the
// can_run_background script never gates keepalive/reflection/consolidation/reset.
func (a *Agent) CanFireBackgroundOperation(ctx context.Context, sessionKey string) (bool, string) {
	if limited, reason := a.SessionRateLimited(sessionKey); limited {
		return false, reason
	}

	// user-provided background gate. Exit 0 = allowed, non-zero = skip.
	if a.canRunBackground() != "" && !a.runCanRunBackground(ctx, sessionKey, a.resolveEndpoint(sessionKey)) {
		return false, "can_run_background declined"
	}

	return true, ""
}

// runCanRunBackground runs the configured can_run_background executable and
// reports whether background work is permitted. A failure to execute the
// command (not found, not executable) is treated as permitted so a broken
// script never wedges all background work.
func (a *Agent) runCanRunBackground(ctx context.Context, sessionKey, endpoint string) bool {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// procx.Spawn strips the foci-secrets group from the child and puts it in
	// its own process group.
	cmd := procx.Spawn(cctx, a.canRunBackground())
	cmd.Env = append(os.Environ(),
		"FOCI_SESSION_KEY="+sessionKey,
		"FOCI_AGENT_ID="+a.AgentID,
		"FOCI_ENDPOINT="+endpoint,
	)

	err := cmd.Run()
	if err == nil {
		return true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false // explicit non-zero exit = decline
	}
	a.logger().Warnf("can_run_background %q failed to run: %v", a.canRunBackground(), err)
	return true
}
