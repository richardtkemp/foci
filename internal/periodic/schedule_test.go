package periodic

import (
	"testing"
	"time"
)

func TestParseSchedule(t *testing.T) {
	cases := []struct {
		in      string
		wantOK  bool
		wantTOD bool
		hour    int
		min     int
		dur     time.Duration
	}{
		{"", false, false, 0, 0, 0},
		{"04:00", true, true, 4, 0, 0},
		{"4:20", true, true, 4, 20, 0},
		{"23:59", true, true, 23, 59, 0},
		{"00:00", true, true, 0, 0, 0},
		{"24:00", false, false, 0, 0, 0}, // hour out of range → not TOD, not a duration
		{"25:00", false, false, 0, 0, 0}, // hour out of range
		{"12:60", false, false, 0, 0, 0}, // minute out of range
		{"20h", true, false, 0, 0, 20 * time.Hour},
		{"90m", true, false, 0, 0, 90 * time.Minute},
		{"0s", false, false, 0, 0, 0}, // non-positive duration rejected
		{"garbage", false, false, 0, 0, 0},
	}
	for _, c := range cases {
		got, ok := parseSchedule(c.in)
		if ok != c.wantOK {
			t.Errorf("parseSchedule(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.isTimeOfDay != c.wantTOD {
			t.Errorf("parseSchedule(%q) isTimeOfDay = %v, want %v", c.in, got.isTimeOfDay, c.wantTOD)
		}
		if c.wantTOD {
			if got.hour != c.hour || got.min != c.min {
				t.Errorf("parseSchedule(%q) = %02d:%02d, want %02d:%02d", c.in, got.hour, got.min, c.hour, c.min)
			}
		} else if got.interval != c.dur {
			t.Errorf("parseSchedule(%q) interval = %v, want %v", c.in, got.interval, c.dur)
		}
	}
}

func TestNextFire_Interval(t *testing.T) {
	s, ok := parseSchedule("1h")
	if !ok {
		t.Fatal("parseSchedule(1h) failed")
	}
	last := time.Date(2026, 6, 12, 10, 30, 0, 0, time.UTC)
	// Truncate(1h) → 10:00, +1h → 11:00.
	want := time.Date(2026, 6, 12, 11, 0, 0, 0, time.UTC)
	if got := s.nextFire(last, time.UTC); !got.Equal(want) {
		t.Errorf("interval nextFire = %v, want %v", got, want)
	}
}

func TestNextFire_TimeOfDay(t *testing.T) {
	s, _ := parseSchedule("04:00")

	// Scheduled time still ahead today (relative to lastFired) → today's slot.
	last := time.Date(2026, 6, 12, 3, 0, 0, 0, time.UTC)
	want := time.Date(2026, 6, 12, 4, 0, 0, 0, time.UTC)
	if got := s.nextFire(last, time.UTC); !got.Equal(want) {
		t.Errorf("ahead-today nextFire = %v, want %v", got, want)
	}

	// Scheduled time already passed today → tomorrow's slot.
	last = time.Date(2026, 6, 12, 5, 0, 0, 0, time.UTC)
	want = time.Date(2026, 6, 13, 4, 0, 0, 0, time.UTC)
	if got := s.nextFire(last, time.UTC); !got.Equal(want) {
		t.Errorf("passed-today nextFire = %v, want %v", got, want)
	}

	// Exact tie (lastFired == scheduled instant) must advance a day, never
	// return the same instant (which would double-fire).
	last = time.Date(2026, 6, 12, 4, 0, 0, 0, time.UTC)
	want = time.Date(2026, 6, 13, 4, 0, 0, 0, time.UTC)
	if got := s.nextFire(last, time.UTC); !got.Equal(want) {
		t.Errorf("exact-tie nextFire = %v, want %v", got, want)
	}
}

// TestNextFire_MissedDaysCatchUpOnce proves a daemon asleep for several days
// fires exactly once: nextFire lands in the past so now >= nextFire, and after
// firing (lastFired := now) the following nextFire is in the future.
func TestNextFire_MissedDaysCatchUp(t *testing.T) {
	s, _ := parseSchedule("04:00")
	last := time.Date(2026, 6, 10, 4, 0, 0, 0, time.UTC) // 2+ days ago
	now := time.Date(2026, 6, 12, 14, 0, 0, 0, time.UTC)

	next := s.nextFire(last, time.UTC)
	if !next.Before(now) {
		t.Fatalf("expected catch-up: nextFire %v should be before now %v", next, now)
	}
	// After firing, anchor to now; the subsequent slot must be in the future.
	after := s.nextFire(now, time.UTC)
	if !after.After(now) {
		t.Errorf("post-fire nextFire %v should be after now %v", after, now)
	}
}

// TestNextFire_DSTStable proves the wall-clock time is preserved across a DST
// transition: 04:00 stays 04:00 the next calendar day, not 03:00 or 05:00.
func TestNextFire_DSTStable(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("tz data unavailable: %v", err)
	}
	s, _ := parseSchedule("04:00")
	// 2026-03-29 is the UK spring-forward day (clocks 01:00→02:00).
	last := time.Date(2026, 3, 28, 12, 0, 0, 0, london)
	next := s.nextFire(last, london)
	if next.Hour() != 4 || next.Minute() != 0 {
		t.Errorf("DST nextFire = %v, want wall-clock 04:00", next)
	}
	if next.Day() != 29 || int(next.Month()) != 3 {
		t.Errorf("DST nextFire date = %v, want 2026-03-29", next)
	}
}
