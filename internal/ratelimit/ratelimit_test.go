package ratelimit

import (
	"testing"
	"time"
)

// TestResolveAbsoluteReset proves a trustworthy absolute reset overrides all
// fallback state and clears the missing-hint streak.
func TestResolveAbsoluteReset(t *testing.T) {
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	reset := now.Add(37 * time.Minute)
	got := Resolve(now, Signal{Kind: KindUsage, ResetAt: reset}, 4)
	if !got.Until.Equal(reset) || got.MissingHintStreak != 0 {
		t.Errorf("resolution = %+v, want until=%v streak=0", got, reset)
	}
}

// TestResolveRetryAfter proves a duration hint is interpreted relative to now
// and resets prior missing-hint backoff.
func TestResolveRetryAfter(t *testing.T) {
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	got := Resolve(now, Signal{Kind: KindRequest, RetryAfter: 90 * time.Second}, 3)
	want := now.Add(90 * time.Second)
	if !got.Until.Equal(want) || got.MissingHintStreak != 0 {
		t.Errorf("resolution = %+v, want until=%v streak=0", got, want)
	}
}

// TestResolveUsageFallback proves an ambiguous delegated usage limit receives
// the shared conservative fallback without request-backoff state.
func TestResolveUsageFallback(t *testing.T) {
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	got := Resolve(now, Signal{Kind: KindUsage}, 5)
	want := now.Add(time.Hour)
	if !got.Until.Equal(want) || got.MissingHintStreak != 0 {
		t.Errorf("resolution = %+v, want until=%v streak=0", got, want)
	}
}

// TestResolveRequestFallback proves missing request hints use the shared
// exponential sequence and cap at one hour.
func TestResolveRequestFallback(t *testing.T) {
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		streak int
		want   time.Duration
	}{
		{0, time.Minute},
		{1, 2 * time.Minute},
		{2, 4 * time.Minute},
		{6, time.Hour},
		{20, time.Hour},
	}
	for _, tt := range tests {
		got := Resolve(now, Signal{Kind: KindRequest}, tt.streak)
		if want := now.Add(tt.want); !got.Until.Equal(want) {
			t.Errorf("streak=%d until=%v, want %v", tt.streak, got.Until, want)
		}
		if got.MissingHintStreak != tt.streak+1 {
			t.Errorf("streak=%d next=%d, want %d", tt.streak, got.MissingHintStreak, tt.streak+1)
		}
	}
}
