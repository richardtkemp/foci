package delegator

// usagelevels.go — the usage warning levels (#3145): the percents of a usage
// window at which a backend's usage warning is announced, once per level per
// window. The gateway reads them live from [notify] usage_warning_levels and
// hands them over as HostHooks.UsageWarningLevels and as the levels argument
// of Spec.UsageReadNotices.

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
