// Package warnings provides self-contained warning collection and dispatch.
//
// Queue collects log warnings and errors for injection into agent turns.
// Dispatcher handles rate-limited proactive warning delivery.
package warnings

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
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
	windowStart          time.Time
	allowed              int    // messages passed through in this window
	suppressedSinceDrain int    // suppressed count since last Drain()
	component            string // for summary messages
	level                string
	quiet                bool // entered after first saturated window expires
}

// Queue collects log warnings and errors for injection into agent turns.
// Thread-safe: warnings may be pushed from any goroutine and are drained
// by the agent loop before each turn.
//
// When maxPerWindow > 0, repeated identical warnings (after normalization) are
// suppressed within a time window. Drain() appends summary lines for suppressed
// messages.
type Queue struct {
	mu             sync.Mutex
	warnings       []string
	maxSize        int // drop oldest if exceeded (default 50)
	maxPerWindow   int
	windowDuration time.Duration
	buckets        map[string]*warningBucket
	errorsOnly     bool             // when true, Push silently drops non-ERROR entries
	nowFunc        func() time.Time // for deterministic testing
	suppressed     atomic.Int32     // when > 0, Push is a no-op (breaks dispatch→warn→push loops)
}

// NewQueue creates a warning queue with optional rate-limiting.
// Set maxPerWindow <= 0 to disable rate-limiting (all warnings pass through).
func NewQueue(maxPerWindow int, windowDuration time.Duration) *Queue {
	return &Queue{
		maxSize:        50,
		maxPerWindow:   maxPerWindow,
		windowDuration: windowDuration,
		buckets:        make(map[string]*warningBucket),
		nowFunc:        time.Now,
	}
}

// quietWindow returns the extended window used during quiet mode (12× normal window).
func (q *Queue) quietWindow() time.Duration {
	return q.windowDuration * 12
}

// SetErrorsOnly configures the queue to silently drop non-ERROR entries on Push.
func (q *Queue) SetErrorsOnly(v bool) {
	q.mu.Lock()
	q.errorsOnly = v
	q.mu.Unlock()
}

// Suppress increments the suppression counter. While suppressed, Push is a no-op.
// Used by the Dispatcher to prevent feedback loops: when a dispatch triggers a
// log warning (e.g. "no channel ID"), that warning must not re-enter the same
// queue, otherwise each failed dispatch grows the next diagnostic payload
// indefinitely.
func (q *Queue) Suppress() { q.suppressed.Add(1) }

// Unsuppress decrements the suppression counter.
func (q *Queue) Unsuppress() { q.suppressed.Add(-1) }

// Push adds a warning to the queue, subject to rate-limiting.
func (q *Queue) Push(level, component, msg string) {
	if q.suppressed.Load() > 0 {
		return
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	// Severity filter: drop WARN-level when errorsOnly is set.
	if q.errorsOnly && level != "ERROR" {
		return
	}

	// Rate-limiting disabled — pass everything through
	if q.maxPerWindow <= 0 {
		q.pushLocked(level, component, msg)
		return
	}

	now := q.nowFunc()
	normalized := NormalizeWarning(msg)
	key := level + "|" + component + "|" + normalized

	b, exists := q.buckets[key]
	if !exists {
		// Brand new — allow through
		q.buckets[key] = &warningBucket{
			windowStart: now,
			allowed:     1,
			component:   component,
			level:       level,
		}
		q.pushLocked(level, component, msg)
		return
	}

	// Determine effective window duration
	effectiveWindow := q.windowDuration
	if b.quiet {
		effectiveWindow = q.quietWindow()
	}

	if now.Sub(b.windowStart) >= effectiveWindow {
		// Window expired — flush any suppressed summary
		if b.suppressedSinceDrain > 0 {
			summary := summaryMessage(b.level, b.component, b.suppressedSinceDrain, now.Sub(b.windowStart))
			q.warnings = append(q.warnings, summary)
		}

		if b.quiet {
			// Quiet window expired — restart quiet, suppress this push
			b.windowStart = now
			b.suppressedSinceDrain = 1
			return
		}

		if b.allowed >= q.maxPerWindow {
			// Saturated window expired — enter quiet mode, suppress this push
			b.windowStart = now
			b.quiet = true
			b.allowed = 0
			b.suppressedSinceDrain = 1
			return
		}

		// Non-saturated window expired — normal reset, allow through
		b.windowStart = now
		b.allowed = 1
		b.suppressedSinceDrain = 0
		q.pushLocked(level, component, msg)
		return
	}

	// Window NOT expired
	if b.quiet {
		// Quiet mode — always suppress
		b.suppressedSinceDrain++
		return
	}

	if b.allowed < q.maxPerWindow {
		b.allowed++
		q.pushLocked(level, component, msg)
		return
	}

	// At max — suppress
	b.suppressedSinceDrain++
}

// pushLocked appends a formatted warning (caller must hold mu).
func (q *Queue) pushLocked(level, component, msg string) {
	entry := fmt.Sprintf("[%s] [%s] %s", level, component, msg)
	q.warnings = append(q.warnings, entry)

	if len(q.warnings) > q.maxSize {
		q.warnings = q.warnings[len(q.warnings)-q.maxSize:]
	}
}

// summaryMessage creates a formatted summary of suppressed warnings.
func summaryMessage(level, component string, suppressedCount int, elapsed time.Duration) string {
	return fmt.Sprintf("[%s] [%s] ... and %d more in last %s",
		level, component, suppressedCount, FormatDuration(elapsed))
}

// Drain returns all queued warnings and clears the queue.
// For any rate-limited keys with suppressed messages, a summary line is appended.
// Expired buckets are pruned to prevent unbounded growth.
// Quiet buckets are skipped unless their quiet window has expired.
func (q *Queue) Drain() []string {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := q.nowFunc()
	for key, b := range q.buckets {
		if b.quiet {
			// Quiet bucket: only flush when quiet window expires
			if now.Sub(b.windowStart) >= q.quietWindow() {
				if b.suppressedSinceDrain > 0 {
					summary := summaryMessage(b.level, b.component, b.suppressedSinceDrain, now.Sub(b.windowStart))
					q.warnings = append(q.warnings, summary)
				}
				delete(q.buckets, key)
			}
			continue
		}

		// Normal bucket
		if b.suppressedSinceDrain > 0 {
			summary := summaryMessage(b.level, b.component, b.suppressedSinceDrain, now.Sub(b.windowStart))
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
func (q *Queue) Pending() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.warnings) > 0 {
		return true
	}
	for _, b := range q.buckets {
		if b.quiet {
			continue
		}
		if b.suppressedSinceDrain > 0 {
			return true
		}
	}
	return false
}

// Len returns the number of queued warnings.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.warnings)
}

// FormatList formats a list of warning entries as a bullet list.
func FormatList(entries []string) string {
	var b strings.Builder
	for i, w := range entries {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("- ")
		b.WriteString(w)
	}
	return b.String()
}

// FormatDuration returns a human-readable duration string.
func FormatDuration(d time.Duration) string {
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
