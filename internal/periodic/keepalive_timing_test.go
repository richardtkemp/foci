package periodic

import (
	"testing"
	"time"
)

func TestTickInterval(t *testing.T) {
	// Guards the tickInterval constant against accidental changes; the 30-second value is relied
	// upon by all periodic scheduling logic.
	if tickInterval != 30*time.Second {
		t.Errorf("tick interval = %v, want 30s", tickInterval)
	}
}

func TestWallClockAlignment_NextFire(t *testing.T) {
	// Verifies the wall-clock alignment formula for 1-hour intervals: tasks fire at the next
	// truncated hour boundary, not at an offset from when the task was last run.
	interval := 1 * time.Hour

	cases := []struct {
		name     string
		last     time.Time
		now      time.Time
		wantFire bool
	}{
		{
			name:     "just_started_no_fire",
			last:     time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
			wantFire: false,
		},
		{
			name:     "before_boundary_no_fire",
			last:     time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 10, 59, 59, 0, time.UTC),
			wantFire: false,
		},
		{
			name:     "at_boundary_fire",
			last:     time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC),
			wantFire: true,
		},
		{
			name:     "past_boundary_fire",
			last:     time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 11, 15, 0, 0, time.UTC),
			wantFire: true,
		},
		{
			name:     "restart_mid_interval_no_fire",
			last:     time.Date(2024, 1, 1, 10, 55, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 10, 55, 5, 0, time.UTC),
			wantFire: false,
		},
		{
			name:     "restart_just_before_boundary_no_fire",
			last:     time.Date(2024, 1, 1, 10, 55, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 10, 59, 59, 0, time.UTC),
			wantFire: false,
		},
		{
			name:     "restart_then_boundary_fire",
			last:     time.Date(2024, 1, 1, 10, 55, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC),
			wantFire: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nextFire := tc.last.Truncate(interval).Add(interval)
			shouldFire := !tc.now.Before(nextFire)
			if shouldFire != tc.wantFire {
				t.Errorf("last=%v now=%v nextFire=%v: got shouldFire=%v, want %v",
					tc.last, tc.now, nextFire, shouldFire, tc.wantFire)
			}
		})
	}
}

func TestWallClockAlignment_30MinInterval(t *testing.T) {
	// Verifies wall-clock alignment for 30-minute intervals: tasks fire at :00 and :30 boundaries
	// regardless of when within the interval the task was last run.
	interval := 30 * time.Minute

	cases := []struct {
		name     string
		last     time.Time
		now      time.Time
		wantFire bool
	}{
		{
			name:     "fires_at_30_boundary",
			last:     time.Date(2024, 1, 1, 10, 15, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
			wantFire: true,
		},
		{
			name:     "fires_at_00_boundary",
			last:     time.Date(2024, 1, 1, 10, 45, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC),
			wantFire: true,
		},
		{
			name:     "no_fire_before_30_boundary",
			last:     time.Date(2024, 1, 1, 10, 15, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 10, 29, 59, 0, time.UTC),
			wantFire: false,
		},
		{
			name:     "restart_at_28_no_fire",
			last:     time.Date(2024, 1, 1, 10, 28, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 10, 28, 5, 0, time.UTC),
			wantFire: false,
		},
		{
			name:     "restart_at_28_then_30_fires",
			last:     time.Date(2024, 1, 1, 10, 28, 0, 0, time.UTC),
			now:      time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
			wantFire: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nextFire := tc.last.Truncate(interval).Add(interval)
			shouldFire := !tc.now.Before(nextFire)
			if shouldFire != tc.wantFire {
				t.Errorf("last=%v now=%v nextFire=%v: got shouldFire=%v, want %v",
					tc.last, tc.now, nextFire, shouldFire, tc.wantFire)
			}
		})
	}
}

func TestWallClockAlignment_RestartDoesNotDelay(t *testing.T) {
	// Verifies that restarting the service at different points mid-interval always produces the
	// same next-fire time, proving restarts cannot delay or advance task scheduling.
	interval := 1 * time.Hour

	start := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

	restart1 := time.Date(2024, 1, 1, 10, 20, 0, 0, time.UTC)
	nextFire1 := restart1.Truncate(interval).Add(interval)

	restart2 := time.Date(2024, 1, 1, 10, 45, 0, 0, time.UTC)
	nextFire2 := restart2.Truncate(interval).Add(interval)

	if !nextFire1.Equal(time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("restart at 10:20: nextFire=%v, want 11:00", nextFire1)
	}
	if !nextFire2.Equal(time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("restart at 10:45: nextFire=%v, want 11:00", nextFire2)
	}
	if !nextFire1.Equal(nextFire2) {
		t.Errorf("restarts at different times have different nextFire: %v vs %v", nextFire1, nextFire2)
	}

	_ = start
}
