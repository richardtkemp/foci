package ccstream

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"foci/internal/ratelimit"
	"foci/internal/timeutil"
)

// fireRateLimited invokes the rate-limit warning hook if one is registered.
// Safe to call whether or not a hook is set.
func (b *Backend) fireRateLimited(detail string) {
	if b.onRateLimited != nil {
		b.onRateLimited(detail)
	}
}

// fireSessionLimit invokes the session-limit hook if one is registered.
// Safe to call whether or not a hook is set.
func (b *Backend) fireSessionLimit(signal ratelimit.Signal) {
	if b.onSessionLimit != nil {
		b.onSessionLimit(signal)
	}
}

// sessionLimitResetRe matches CC's session-limit reset clause, e.g.
// "resets 11:30pm (Europe/London)" or "resets 6pm (Europe/London)".
var sessionLimitResetRe = regexp.MustCompile(`(?i)resets\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)\s*\(([^)]+)\)`)

// parseSessionLimitReset extracts the reset instant from a CC session-limit
// message like "You've hit your session limit · resets 11:30pm (Europe/London)".
// It returns a neutral signal containing the next future occurrence of that
// wall-clock time in the named zone, and false if the clause is absent or the
// zone is unknown.
func parseSessionLimitReset(text string, now time.Time) (ratelimit.Signal, bool) {
	m := sessionLimitResetRe.FindStringSubmatch(text)
	if m == nil {
		return ratelimit.Signal{}, false
	}
	hour, _ := strconv.Atoi(m[1])
	minute := 0
	if m[2] != "" {
		minute, _ = strconv.Atoi(m[2])
	}
	switch {
	case strings.EqualFold(m[3], "pm") && hour != 12:
		hour += 12
	case strings.EqualFold(m[3], "am") && hour == 12:
		hour = 0
	}
	loc, err := time.LoadLocation(strings.TrimSpace(m[4]))
	if err != nil {
		return ratelimit.Signal{}, false
	}
	n := now.In(loc)
	reset := time.Date(n.Year(), n.Month(), n.Day(), hour, minute, 0, 0, loc)
	if !reset.After(now) {
		reset = reset.Add(24 * time.Hour)
	}
	return ratelimit.Signal{Kind: ratelimit.KindUsage, ResetAt: reset, Detail: text}, true
}

// rateLimitWarnState is the per-window high-water mark used to throttle
// rate-limit warnings. Keyed by "status|type"; resetsAt identifies the
// specific limit window so a new window re-arms warnings.
type rateLimitWarnState struct {
	resetsAt int64 // the window's reset instant; a change means a fresh window
	bucket   int   // highest utilization bucket already warned for this window
}

// RateLimitThrottle holds rate-limit warning throttle state. It is shared
// across all Backends for an agent (via SetRateLimitThrottle) so that each
// warning fires only once per utilization bucket regardless of how many
// concurrent sessions/backends are active — rate limits are account-wide.
type RateLimitThrottle struct {
	mu    sync.Mutex
	state map[string]rateLimitWarnState
}

// NewRateLimitThrottle creates a shared rate-limit warning throttle.
func NewRateLimitThrottle() *RateLimitThrottle {
	return &RateLimitThrottle{state: make(map[string]rateLimitWarnState)}
}

// rateLimitCheckResult holds the outcome of a throttle evaluation, for logging.
type rateLimitCheckResult struct {
	prev rateLimitWarnState
	seen bool
	fire bool
}

// evaluate atomically checks and updates the throttle for the given key. It
// fires when: the key is unseen, the limit window changed (resetsAt), or
// utilization climbed to a higher bucket. Returns the decision + prior state
// for diagnostic logging.
func (t *RateLimitThrottle) evaluate(key string, resetsAt int64, bucket int) rateLimitCheckResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, seen := t.state[key]
	fire := !seen || prev.resetsAt != resetsAt || bucket > prev.bucket
	if fire {
		t.state[key] = rateLimitWarnState{resetsAt: resetsAt, bucket: bucket}
	}
	return rateLimitCheckResult{prev: prev, seen: seen, fire: fire}
}

// rateLimitBucket groups a utilization fraction (0..1) into a notification
// bucket. Below 95% it coarsens to 5% steps (…, 80, 85, 90) so we warn once
// per 5% increment; at/above 95% it uses 1% steps (95, 96, 97, …) so every
// step near the limit is surfaced ("permit all >95%"). Returns -1 when
// utilization is unknown (nil) so such events warn once per window.
func rateLimitBucket(u *float64) int {
	if u == nil {
		return -1
	}
	pct := *u * 100
	if pct < 0 {
		pct = 0
	}
	if pct >= 95 {
		return int(pct) // 1% granularity near the limit
	}
	return int(pct/5) * 5 // 5% granularity below 95%
}

// rateLimitOnBoundary reports whether the utilization fraction sits exactly on
// a notification boundary: a multiple of 5% below 95%, or any integer % at/above
// 95%. The throttle only fires on boundary crossings, so intermediate values
// like 87% are silently ignored. Uses math.Round to tolerate float imprecision
// (0.85*100 can be 84.99999…).
func rateLimitOnBoundary(u *float64) bool {
	if u == nil {
		return false
	}
	pct := math.Round(*u * 100)
	if pct < 0 {
		pct = 0
	}
	if pct >= 95 {
		return true // every 1% step is a boundary near the limit
	}
	return int(pct)%5 == 0
}

// rateLimitWindowLabel maps CC's rateLimitType to a human-friendly window name.
func rateLimitWindowLabel(t string) string {
	switch t {
	case "five_hour":
		return "5-hour"
	case "seven_day":
		return "7-day"
	case "":
		return "usage"
	default:
		return t
	}
}

// humanUntil renders the time until reset compactly ("in 5d 1h", "in 42m").
func humanUntil(now, reset time.Time) string {
	d := reset.Sub(now)
	if d <= 0 {
		return "now"
	}
	days := int(d.Hours()) / 24
	hrs := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("in %dd %dh", days, hrs)
	case hrs > 0:
		return fmt.Sprintf("in %dh %dm", hrs, mins)
	default:
		return fmt.Sprintf("in %dm", mins)
	}
}

// FormatRateLimitNotice renders a CC structured rate_limit_event as a
// human-facing notice for delivery to the agent's chat (not a log line). It
// names the affected window, current utilization, and when the limit resets.
// CC emits the event on status transitions (allowed → allowed_warning →
// rejected); this is only called past the "allowed" threshold.
func FormatRateLimitNotice(info RateLimitInfo) string {
	window := rateLimitWindowLabel(info.RateLimitType)

	var b strings.Builder
	if info.Status == "rejected" {
		fmt.Fprintf(&b, "🛑 Anthropic rate limit reached — requests on the %s window are being rejected.", window)
	} else {
		fmt.Fprintf(&b, "⚠️ Approaching Anthropic %s rate limit.", window)
	}

	if info.Utilization != nil {
		fmt.Fprintf(&b, "\nUsage: %.0f%%", *info.Utilization*100)
	}
	if info.ResetsAt != nil {
		reset := time.Unix(int64(*info.ResetsAt), 0)
		fmt.Fprintf(&b, "\nResets: %s (%s)",
			reset.Local().Format("Mon 2 Jan 15:04"), humanUntil(timeutil.Now(), reset))
	}
	if info.OverageStatus != "" && info.OverageStatus != "allowed" {
		fmt.Fprintf(&b, "\nOverage: %s", info.OverageStatus)
	}
	return b.String()
}
