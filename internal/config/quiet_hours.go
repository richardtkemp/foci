package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// QuietWindow is a parsed compaction_quiet_hours value: the half-open daily
// window [StartMin, EndMin), both in minutes since midnight of the process
// timezone. StartMin > EndMin is a window that wraps midnight ("23:00-07:00").
// This is the only representation of the format — validation and the periodic
// runner both parse through ParseQuietWindow (#2218).
type QuietWindow struct {
	StartMin int
	EndMin   int
}

// ParseQuietWindow parses the "HH:MM-HH:MM" compaction_quiet_hours format.
// The empty string is valid and yields the zero window (the feature is off).
// Start equal to end is rejected: under wrap-around semantics that would mean
// either nothing or all day, never what a person means by a window.
func ParseQuietWindow(s string) (QuietWindow, error) {
	if s == "" {
		return QuietWindow{}, nil
	}
	startStr, endStr, ok := strings.Cut(s, "-")
	if !ok {
		return QuietWindow{}, fmt.Errorf("want \"HH:MM-HH:MM\", got %q", s)
	}
	start, err := parseClockMinutes(startStr)
	if err != nil {
		return QuietWindow{}, fmt.Errorf("want \"HH:MM-HH:MM\", got %q: bad start: %w", s, err)
	}
	end, err := parseClockMinutes(endStr)
	if err != nil {
		return QuietWindow{}, fmt.Errorf("want \"HH:MM-HH:MM\", got %q: bad end: %w", s, err)
	}
	if start == end {
		return QuietWindow{}, fmt.Errorf("want \"HH:MM-HH:MM\", got %q: start equals end (the window would be empty)", s)
	}
	return QuietWindow{StartMin: start, EndMin: end}, nil
}

// parseClockMinutes parses one "HH:MM" endpoint into minutes since midnight.
func parseClockMinutes(s string) (int, error) {
	hStr, mStr, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	h, err := strconv.Atoi(hStr)
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("hour %q is not in 00..23", hStr)
	}
	m, err := strconv.Atoi(mStr)
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("minute %q is not in 00..59", mStr)
	}
	return h*60 + m, nil
}

// Contains reports whether t falls inside the window: start inclusive, end
// exclusive, wrapping windows covering both sides of midnight. It is a pure
// function of (window, t) — t's own location decides the minute-of-day, so
// callers convert to the process timezone first (periodic.Runner.now does).
// The zero window (empty config string, feature off) contains nothing.
func (w QuietWindow) Contains(t time.Time) bool {
	if w.StartMin == w.EndMin {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	if w.StartMin < w.EndMin {
		return m >= w.StartMin && m < w.EndMin
	}
	return m >= w.StartMin || m < w.EndMin
}

// OccurrenceStart returns the wall-clock moment at which the window occurrence
// covering t began: today at StartMin, or yesterday's for a wrapping window
// before the start (23:00-07:00 at 02:00 → yesterday 23:00). It anchors the
// once-per-window rule: a quiet-compaction stamp at or after this moment
// belongs to the current occurrence.
func (w QuietWindow) OccurrenceStart(t time.Time) time.Time {
	start := time.Date(t.Year(), t.Month(), t.Day(), 0, w.StartMin, 0, 0, t.Location())
	if t.Before(start) {
		// Inside a wrapping window but earlier than today's start time: the
		// occurrence began yesterday (time.Date keeps clock times stable
		// across DST, like the shared schedule parser).
		start = start.AddDate(0, 0, -1)
	}
	return start
}
