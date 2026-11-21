package delegator

// usagelevels.go — the usage warning levels (#3145): the percents of a usage
// window at which a backend's usage warning is announced, once per level per
// window. The gateway reads them live from [notify] usage_warning_levels and
// hands them over as HostHooks.UsageWarningLevels and as the levels argument
// of Spec.UsageReadNotices.

import "time"

// DefaultUsageWarningLevels are the levels a backend uses when the gateway
// wired none (a test or a bare backend). They equal config's
// DefaultUsageWarningLevels (a config test holds the two together; the
// backends do not import config).
var DefaultUsageWarningLevels = []int{50, 60, 70, 80, 90, 95, 96, 97, 98, 99}

// UsageWarningLevelsOrDefault returns the live levels from fn, or
// DefaultUsageWarningLevels when fn is nil.
func UsageWarningLevelsOrDefault(fn func() []int) []int {
	if fn == nil {
		return DefaultUsageWarningLevels
	}
	return fn()
}

// UsageLevelReached is the highest of levels (any order) at or under pct, or
// 0 when pct is under all of them.
func UsageLevelReached(levels []int, pct int) int {
	reached := 0
	for _, l := range levels {
		if pct >= l && l > reached {
			reached = l
		}
	}
	return reached
}

// UsageAheadOfWindow reports whether a usage warning at usedPct (percent of
// the window used, 0-100) is worth sending now (#3242, Dick 2026-11-21:
// "Only send these alerts if percent exceeds progress-through-window"):
// usedPct must be greater than the share of the window already elapsed, in
// percent. A window that resets at resetsAt and lasts length began at
// resetsAt-length; at now, (now - start) / length of it has gone. 50% used
// with 82% of the window gone is not worth a warning: at that pace the
// window ends before its limit.
//
// A window with no known reset or length, or whose reset is at or before
// now (the reading is about a window that has already ended, so how much of
// the current one has gone is unknown), cannot be judged and is always worth
// a warning. Callers never ask about a reached limit: it is always
// announced.
func UsageAheadOfWindow(usedPct float64, resetsAt time.Time, length time.Duration, now time.Time) bool {
	if resetsAt.IsZero() || length <= 0 || !resetsAt.After(now) {
		return true
	}
	left := resetsAt.Sub(now)
	if left > length {
		left = length
	}
	elapsedPct := 100 * float64(length-left) / float64(length)
	return usedPct > elapsedPct
}
