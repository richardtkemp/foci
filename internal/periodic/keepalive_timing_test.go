package periodic

import (
	"testing"
	"time"
)

// TestTickIntervalResolution proves the scheduler poll cadence resolves
// correctly through New(): an unset config keeps the 30s production default
// (the cadence all periodic logic was tuned against), a valid duration string
// overrides it (the path integration tests use to run fast), and an
// unparseable value falls back to the default rather than producing a zero
// ticker.
func TestTickIntervalResolution(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		want time.Duration
	}{
		{"unset keeps production default", "", defaultTickInterval},
		{"default constant is 30s", "", 30 * time.Second},
		{"valid override applies", "1s", time.Second},
		{"unparseable falls back", "not-a-duration", defaultTickInterval},
		{"zero falls back", "0s", defaultTickInterval},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New(RunnerConfig{AgentID: "test", TickInterval: tc.cfg})
			if r.tickInterval != tc.want {
				t.Errorf("tickInterval = %v, want %v", r.tickInterval, tc.want)
			}
		})
	}
}
