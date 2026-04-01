// Package timeutil provides centralised timestamp formatting for foci.
// All timestamps use the configured timezone (default: machine local time)
// with UTC offset (e.g. 2026-03-31T23:26:35+01:00).
//
// The timezone is set once at startup via SetLocation. If not called,
// the machine's local timezone is used (equivalent to time.Local).
//
// Use these functions instead of calling time.Now().Format(...) directly.
// The offset suffix makes timestamps unambiguous and sortable.
package timeutil

import "time"

// loc is the configured timezone. nil = use time.Local (default).
// Set once at startup via SetLocation before any goroutines use it.
var loc *time.Location

// SetLocation configures the timezone used by all timeutil functions.
// Pass nil to use the machine's local timezone (default).
// Must be called once at startup before any other timeutil function.
func SetLocation(l *time.Location) {
	loc = l
}

func location() *time.Location {
	if loc != nil {
		return loc
	}
	return time.Local
}

// Now returns the current time in the configured timezone.
func Now() time.Time {
	return time.Now().In(location())
}

// Format formats a time as RFC3339 in the configured timezone.
// Example: "2026-03-31T23:26:35+01:00"
func Format(t time.Time) string {
	return t.In(location()).Format(time.RFC3339)
}

// FormatNano formats a time as RFC3339Nano in the configured timezone.
// Example: "2026-03-31T23:26:35.123456789+01:00"
func FormatNano(t time.Time) string {
	return t.In(location()).Format(time.RFC3339Nano)
}

// FormatFilename formats a time for use in filenames (no colons).
// Example: "2026-03-31T23-26-35+0100"
func FormatFilename(t time.Time) string {
	return t.In(location()).Format("2006-01-02T15-04-05-0700")
}
