package config

import (
	"errors"
	"regexp"
	"strconv"
	"time"
)

// clockTimeRe matches a 24-hour "HH:MM" wall-clock time (e.g. "04:00", "4:20",
// "23:59"). Hours 0-23, minutes 00-59. A single-digit hour is allowed.
var clockTimeRe = regexp.MustCompile(`^([01]?[0-9]|2[0-3]):([0-5][0-9])$`)

// Schedule is a parsed maintenance-schedule value ([maintenance]
// consolidation_time / reset_time). It is one of two kinds:
//
//   - time-of-day: fire daily at a fixed wall-clock time (Clock true)
//   - interval:    fire every Interval since the last run
type Schedule struct {
	Clock    bool          // true: Hour/Min valid; false: Interval valid
	Hour     int           // 0-23, valid when Clock
	Min      int           // 0-59, valid when Clock
	Interval time.Duration // > 0, valid when !Clock
}

// ErrBadSchedule is the one rejection for a schedule value that is neither a
// "HH:MM" clock time nor a positive Go duration.
var ErrBadSchedule = errors.New("want HH:MM or a positive duration like 20h")

// ParseSchedule accepts exactly what the run-time scheduler always accepted:
// a "HH:MM" daily clock time (per clockTimeRe) or a Go duration greater than
// zero. Anything else — including "" — is ErrBadSchedule (the load-time walk
// skips empty values separately: reset_time = "" means "never").
func ParseSchedule(s string) (Schedule, error) {
	if m := clockTimeRe.FindStringSubmatch(s); m != nil {
		h, _ := strconv.Atoi(m[1])
		mn, _ := strconv.Atoi(m[2])
		return Schedule{Clock: true, Hour: h, Min: mn}, nil
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return Schedule{Interval: d}, nil
	}
	return Schedule{}, ErrBadSchedule
}
