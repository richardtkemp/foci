package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func TestParseRateLimitReset(t *testing.T) {
	london := mustLoad(t, "Europe/London")

	t.Run("observed session-limit notice, reset later today", func(t *testing.T) {
		now := time.Date(2026, 7, 12, 22, 26, 0, 0, london)
		got, ok := ParseRateLimitReset("You've hit your session limit · resets 10:30pm (Europe/London)", now)
		if !ok {
			t.Fatal("expected ok=true")
		}
		want := time.Date(2026, 7, 12, 22, 30, 0, 0, london)
		if !got.Equal(want) {
			t.Errorf("got %s; want %s", got, want)
		}
	})

	t.Run("clock at/before now rolls forward a day", func(t *testing.T) {
		now := time.Date(2026, 7, 12, 23, 0, 0, 0, london)
		got, ok := ParseRateLimitReset("resets 10:30pm (Europe/London)", now)
		if !ok {
			t.Fatal("expected ok=true")
		}
		want := time.Date(2026, 7, 13, 22, 30, 0, 0, london)
		if !got.Equal(want) {
			t.Errorf("got %s; want %s", got, want)
		}
	})

	t.Run("no explicit zone uses now's location; 'at 9am'", func(t *testing.T) {
		now := time.Date(2026, 7, 12, 6, 0, 0, 0, london)
		got, ok := ParseRateLimitReset("usage limit — resets at 9am", now)
		if !ok {
			t.Fatal("expected ok=true")
		}
		want := time.Date(2026, 7, 12, 9, 0, 0, 0, london)
		if !got.Equal(want) {
			t.Errorf("got %s; want %s", got, want)
		}
	})

	t.Run("12am/12pm boundaries", func(t *testing.T) {
		now := time.Date(2026, 7, 12, 5, 0, 0, 0, london)
		got, _ := ParseRateLimitReset("resets 12pm", now)
		if got.Hour() != 12 {
			t.Errorf("12pm → hour %d; want 12", got.Hour())
		}
		now2 := time.Date(2026, 7, 12, 23, 0, 0, 0, london)
		got2, _ := ParseRateLimitReset("resets 12am", now2)
		if got2.Hour() != 0 {
			t.Errorf("12am → hour %d; want 0", got2.Hour())
		}
	})

	t.Run("no reset clock → ok=false", func(t *testing.T) {
		if _, ok := ParseRateLimitReset("you have hit a limit but no time given", time.Now()); ok {
			t.Error("expected ok=false when no clock present")
		}
	})
}

func TestMarkRateLimited(t *testing.T) {
	ag := &Agent{Endpoint: "anthropic"}
	// Pre-existing gate on a per-session endpoint should also be closed
	// (account-wide session limit).
	other := ag.getOrCreateRateLimitGate("some-other-endpoint")

	ag.MarkRateLimited(time.Now().Add(2 * time.Hour))

	canFire, reason := ag.CanFireBackgroundOperation(context.Background(), "test/c123")
	if canFire {
		t.Errorf("expected canFire=false after MarkRateLimited, reason=%q", reason)
	}
	if !strings.Contains(reason, "rate limited") {
		t.Errorf("expected rate-limited reason, got %q", reason)
	}
	if limited, _ := other.IsLimited(); !limited {
		t.Error("expected the pre-existing per-endpoint gate to also be closed")
	}
}

func TestMarkRateLimitedDoesNotBlockUserPathGate(t *testing.T) {
	// MarkRateLimited closes the background gate; it must leave the gate open
	// again once the reset time passes (auto-reopen), not wedge forever.
	ag := &Agent{Endpoint: "anthropic"}
	ag.MarkRateLimited(time.Now().Add(-1 * time.Second)) // already expired
	if canFire, _ := ag.CanFireBackgroundOperation(context.Background(), "test/c123"); !canFire {
		t.Error("expected canFire=true when the reset time is already in the past")
	}
}
