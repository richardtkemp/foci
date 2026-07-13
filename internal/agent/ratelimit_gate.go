package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
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

// CanFireBackgroundOperation checks if a background operation can run on the given session.
// Returns false if:
//   - The rate limit gate for the session's endpoint is closed
//   - The configured can_run_background executable exits non-zero
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
		return false, fmt.Sprintf("rate limited on %s (resets %s)", endpoint, until.Format(time.Kitchen))
	}

	// Check 2: user-provided background gate. Exit 0 = allowed, non-zero = skip.
	if a.CanRunBackground != "" && !a.runCanRunBackground(ctx, sessionKey, endpoint) {
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
	cmd := procx.Spawn(cctx, a.CanRunBackground)
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
	a.logger().Warnf("can_run_background %q failed to run: %v", a.CanRunBackground, err)
	return true
}

// MarkRateLimited closes this agent's rate-limit gate(s) until `until`, so
// background / periodic work (keepalive, reflection, consolidation, memory
// compaction) is suppressed while a rate/session limit is in force. A CC
// "session limit" is account-wide — one subscription across every model — so
// every known endpoint gate is closed, plus the agent default, matching
// whatever endpoint CanFireBackgroundOperation resolves for each session.
//
// User turns are unaffected: on the delegated path they do not consult this
// gate (DelegatedTransport.RateLimitGate is a no-op), so a human can still
// drive the session; only unattended periodic work is held back. The gate
// auto-reopens once time.Now passes `until` (RateLimitGate.IsLimited).
func (a *Agent) MarkRateLimited(until time.Time) {
	a.getOrCreateRateLimitGate("").Close(until) // agent default endpoint
	a.rateLimitGatesMu.RLock()
	gates := make([]*RateLimitGate, 0, len(a.rateLimitGates))
	for _, g := range a.rateLimitGates {
		gates = append(gates, g)
	}
	a.rateLimitGatesMu.RUnlock()
	for _, g := range gates {
		g.Close(until)
	}
}

var (
	// resetClockRe extracts the reset clock time from a CC limit notice, e.g.
	// "…resets 10:30pm (Europe/London)" or "…resets at 9am". Minutes and the
	// am/pm marker are optional.
	resetClockRe = regexp.MustCompile(`(?i)reset[s]?\s+(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?`)
	// resetZoneRe extracts an IANA zone in parentheses, e.g. "(Europe/London)".
	resetZoneRe = regexp.MustCompile(`\(([A-Za-z]+/[A-Za-z_]+)\)`)
)

// ParseRateLimitReset extracts the absolute reset time from a CC rate/session/
// usage-limit message such as
//
//	"You've hit your session limit · resets 10:30pm (Europe/London)"
//
// It returns (resetTime, true) when a clock time is parsed, or (zero, false)
// when the text carries no recognisable reset clock — callers then apply their
// own fallback. The clock is interpreted in the message's stated IANA zone if
// present, else in now's location; a time at/before now rolls forward one day
// (the limit resets later today or tomorrow).
func ParseRateLimitReset(detail string, now time.Time) (time.Time, bool) {
	m := resetClockRe.FindStringSubmatch(detail)
	if m == nil {
		return time.Time{}, false
	}
	hour, err := strconv.Atoi(m[1])
	if err != nil {
		return time.Time{}, false
	}
	minute := 0
	if m[2] != "" {
		minute, _ = strconv.Atoi(m[2])
	}
	switch strings.ToLower(m[3]) {
	case "pm":
		if hour < 12 {
			hour += 12
		}
	case "am":
		if hour == 12 {
			hour = 0
		}
	}
	if hour > 23 || minute > 59 {
		return time.Time{}, false
	}
	loc := now.Location()
	if z := resetZoneRe.FindStringSubmatch(detail); z != nil {
		if l, err := time.LoadLocation(z[1]); err == nil {
			loc = l
		}
	}
	nowLoc := now.In(loc)
	reset := time.Date(nowLoc.Year(), nowLoc.Month(), nowLoc.Day(), hour, minute, 0, 0, loc)
	if !reset.After(nowLoc) {
		reset = reset.Add(24 * time.Hour)
	}
	return reset, true
}
