package agent

import (
	"fmt"
	"regexp"
	"sync"
	"time"
)

// Normalization regexes — order matters (IP contains digits, must go first).
var (
	reIP     = regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`)
	reHex    = regexp.MustCompile(`[0-9a-fA-F]{8,}`)
	reDigits = regexp.MustCompile(`\d{2,}`)
)

// NormalizeWarning replaces variable parts of a warning message with placeholders
// so that semantically identical warnings (differing only in IPs, hex strings,
// or numbers) map to the same dedup key.
func NormalizeWarning(msg string) string {
	msg = reIP.ReplaceAllString(msg, "<IP>")
	msg = reHex.ReplaceAllString(msg, "<HEX>")
	msg = reDigits.ReplaceAllString(msg, "<N>")
	return msg
}

// warningBucket tracks rate-limiting state for one unique warning key.
type warningBucket struct {
	windowStart        time.Time
	allowed            int // messages passed through in this window
	suppressedSinceDrain int // suppressed count since last Drain()
	component          string // for summary messages
	level              string
}

// WarningQueue collects log warnings and errors for injection into agent turns.
// Thread-safe: warnings may be pushed from any goroutine and are drained
// by the agent loop before each turn.
//
// When maxPerWindow > 0, repeated identical warnings (after normalization) are
// suppressed within a time window. Drain() appends summary lines for suppressed
// messages.
type WarningQueue struct {
	mu             sync.Mutex
	warnings       []string
	maxSize        int // drop oldest if exceeded (default 50)
	maxPerWindow   int
	windowDuration time.Duration
	buckets        map[string]*warningBucket
	nowFunc        func() time.Time // for deterministic testing
}

// NewWarningQueue creates a warning queue with optional rate-limiting.
// Set maxPerWindow <= 0 to disable rate-limiting (all warnings pass through).
func NewWarningQueue(maxPerWindow int, windowDuration time.Duration) *WarningQueue {
	return &WarningQueue{
		maxSize:        50,
		maxPerWindow:   maxPerWindow,
		windowDuration: windowDuration,
		buckets:        make(map[string]*warningBucket),
		nowFunc:        time.Now,
	}
}

// Push adds a warning to the queue, subject to rate-limiting.
func (q *WarningQueue) Push(level, component, msg string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Rate-limiting disabled — pass everything through
	if q.maxPerWindow <= 0 {
		q.pushLocked(level, component, msg)
		return
	}

	now := q.nowFunc()
	normalized := NormalizeWarning(msg)
	key := level + "|" + component + "|" + normalized

	b, exists := q.buckets[key]
	if !exists || now.Sub(b.windowStart) >= q.windowDuration {
		// Flush summary for expiring bucket before resetting
		if exists && b.suppressedSinceDrain > 0 {
			dur := formatDuration(now.Sub(b.windowStart))
			summary := fmt.Sprintf("[%s] [%s] ... and %d more in last %s",
				b.level, b.component, b.suppressedSinceDrain, dur)
			q.warnings = append(q.warnings, summary)
		}
		// New bucket or window expired — reset and allow
		q.buckets[key] = &warningBucket{
			windowStart: now,
			allowed:     1,
			component:   component,
			level:       level,
		}
		q.pushLocked(level, component, msg)
		return
	}

	if b.allowed < q.maxPerWindow {
		b.allowed++
		q.pushLocked(level, component, msg)
		return
	}

	// Suppress
	b.suppressedSinceDrain++
}

// pushLocked appends a formatted warning (caller must hold mu).
func (q *WarningQueue) pushLocked(level, component, msg string) {
	entry := fmt.Sprintf("[%s] [%s] %s", level, component, msg)
	q.warnings = append(q.warnings, entry)

	if len(q.warnings) > q.maxSize {
		q.warnings = q.warnings[len(q.warnings)-q.maxSize:]
	}
}

// Drain returns all queued warnings and clears the queue.
// For any rate-limited keys with suppressed messages, a summary line is appended.
// Expired buckets are pruned to prevent unbounded growth.
func (q *WarningQueue) Drain() []string {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Append summary lines for suppressed warnings
	now := q.nowFunc()
	for key, b := range q.buckets {
		if b.suppressedSinceDrain > 0 {
			dur := formatDuration(now.Sub(b.windowStart))
			summary := fmt.Sprintf("[%s] [%s] ... and %d more in last %s",
				b.level, b.component, b.suppressedSinceDrain, dur)
			q.warnings = append(q.warnings, summary)
			b.suppressedSinceDrain = 0
		}
		// Prune expired buckets
		if now.Sub(b.windowStart) >= q.windowDuration {
			delete(q.buckets, key)
		}
	}

	if len(q.warnings) == 0 {
		return nil
	}
	result := q.warnings
	q.warnings = nil
	return result
}

// Pending returns true if there are queued warnings or suppressed warnings
// that would produce summary lines on Drain(). Used by proactive dispatch
// to check without draining.
func (q *WarningQueue) Pending() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.warnings) > 0 {
		return true
	}
	for _, b := range q.buckets {
		if b.suppressedSinceDrain > 0 {
			return true
		}
	}
	return false
}

// Len returns the number of queued warnings.
func (q *WarningQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.warnings)
}

// formatDuration returns a human-readable duration string.
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}
