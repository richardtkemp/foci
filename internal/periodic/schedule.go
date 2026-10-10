package periodic

import (
	"time"

	"foci/internal/config"
)

// schedule is a parsed maintenance schedule. It is one of two kinds:
//
//   - time-of-day: fire daily at a fixed wall-clock time (isTimeOfDay=true)
//   - interval:    fire every N since the last run (a Go duration)
//
// Both consolidation_time and reset_time use this so a single config key can
// express "every 20h" (interval) or "04:00" (daily) interchangeably.
type schedule struct {
	isTimeOfDay bool
	hour, min   int           // valid when isTimeOfDay
	interval    time.Duration // valid when !isTimeOfDay
}

// parseSchedule parses s via config.ParseSchedule — the ONE acceptance rule
// for schedule values, shared with the config loader, /config set and the app
// editor — keeping this package's (schedule, bool) shape. ok is false when s
// is empty or matches neither form (caller should treat that as "disabled" /
// skip with a warning).
func parseSchedule(s string) (sched schedule, ok bool) {
	sc, err := config.ParseSchedule(s)
	if err != nil {
		return schedule{}, false
	}
	return schedule{isTimeOfDay: sc.Clock, hour: sc.Hour, min: sc.Min, interval: sc.Interval}, true
}

// nextFire returns the earliest instant strictly after lastFired at which this
// schedule is next due. The caller fires when now >= nextFire, so a daemon that
// was asleep past the scheduled instant fires exactly once on wake (catch-up),
// never N times for N missed days.
//
// loc is the timezone for clock-time schedules; it is ignored for intervals.
func (s schedule) nextFire(lastFired time.Time, loc *time.Location) time.Time {
	if !s.isTimeOfDay {
		// Preserve the historical interval cadence: anchor to the truncated
		// boundary so fires land on stable wall-clock-ish multiples.
		return lastFired.Truncate(s.interval).Add(s.interval)
	}
	lf := lastFired.In(loc)
	candidate := time.Date(lf.Year(), lf.Month(), lf.Day(), s.hour, s.min, 0, 0, loc)
	if !candidate.After(lastFired) {
		// Today's slot already passed (relative to lastFired) — advance one
		// calendar day. Rebuilding via time.Date (not Add(24h)) keeps the
		// wall-clock time stable across DST transitions.
		candidate = time.Date(lf.Year(), lf.Month(), lf.Day()+1, s.hour, s.min, 0, 0, loc)
	}
	return candidate
}
