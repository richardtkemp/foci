package delegator

import (
	"testing"
	"time"
)

// #3242 (Dick 2026-11-21: "Only send these alerts if percent exceeds
// progress-through-window"): a usage warning is worth sending only when the
// use is greater than the share of the window already gone.
func TestUsageAheadOfWindow(t *testing.T) {
	now := time.Date(2026, 11, 21, 16, 5, 50, 0, time.UTC)
	five := 5 * time.Hour
	for _, tc := range []struct {
		name   string
		used   float64
		reset  time.Time
		length time.Duration
		want   bool
	}{
		// The ticket's case: 50% with 82% of the 5-hour window gone.
		{"50 at 82 gone", 50, now.Add(54 * time.Minute), five, false},
		{"90 at 82 gone", 90, now.Add(54 * time.Minute), five, true},
		{"50 at 40 gone", 50, now.Add(3 * time.Hour), five, true},
		{"equal is not ahead", 50, now.Add(150 * time.Minute), five, false},
		{"weekly 72 at 71 gone", 72, now.Add(48 * time.Hour), 7 * 24 * time.Hour, true},
		{"weekly 61 at 71 gone", 61, now.Add(48 * time.Hour), 7 * 24 * time.Hour, false},
		{"reset past: the reading is of an ended window, not judged", 10, now.Add(-time.Minute), five, true},
		{"reset further than the length: nothing gone", 1, now.Add(6 * time.Hour), five, true},
		{"no reset", 10, time.Time{}, five, true},
		{"no length", 10, now.Add(time.Minute), 0, true},
	} {
		if got := UsageAheadOfWindow(tc.used, tc.reset, tc.length, now); got != tc.want {
			t.Errorf("%s: UsageAheadOfWindow(%v) = %v, want %v", tc.name, tc.used, got, tc.want)
		}
	}
}
