package agent

import (
	"testing"
	"time"
)

func TestManaWatcherNewNilForEmpty(t *testing.T) {
	mw := NewManaWatcher(nil)
	if mw != nil {
		t.Error("expected nil for empty thresholds")
	}

	mw = NewManaWatcher([]int{})
	if mw != nil {
		t.Error("expected nil for empty slice")
	}
}

func TestManaWatcherThresholdsSortedDescending(t *testing.T) {
	mw := NewManaWatcher([]int{10, 50, 25, 5})
	if mw.thresholds[0] != 50 {
		t.Errorf("first threshold = %d, want 50", mw.thresholds[0])
	}
	if mw.thresholds[1] != 25 {
		t.Errorf("second threshold = %d, want 25", mw.thresholds[1])
	}
	if mw.thresholds[2] != 10 {
		t.Errorf("third threshold = %d, want 10", mw.thresholds[2])
	}
	if mw.thresholds[3] != 5 {
		t.Errorf("fourth threshold = %d, want 5", mw.thresholds[3])
	}
}

func TestManaWatcherFiresAtThreshold(t *testing.T) {
	mw := NewManaWatcher([]int{50, 25, 10, 5})

	var warned string
	mw.CheckAndWarn("25%", func(w string) {
		warned = w
	})

	if warned == "" {
		t.Error("expected warning at 25% mana (crossed below 50% threshold)")
	}
	// When mana drops to 25%, we've crossed below the 50% threshold
	if warned != "low mana: 25% remaining (threshold: 50%)" {
		t.Errorf("warning = %q", warned)
	}
}

func TestManaWatcherFiresOnlyOnce(t *testing.T) {
	mw := NewManaWatcher([]int{50})

	var count int
	mw.CheckAndWarn("25%", func(w string) {
		count++
	})

	mw.CheckAndWarn("25%", func(w string) {
		count++
	})

	if count != 1 {
		t.Errorf("count = %d, want 1 (should only fire once)", count)
	}
}

func TestManaWatcherDoesNotFireAboveThreshold(t *testing.T) {
	mw := NewManaWatcher([]int{50, 25})

	var warned string
	mw.CheckAndWarn("75%", func(w string) {
		warned = w
	})

	if warned != "" {
		t.Error("should not warn when above threshold")
	}
}

func TestManaWatcherNilSafe(t *testing.T) {
	var mw *ManaWatcher

	mw.CheckAndWarn("50%", func(w string) {
		t.Error("should not call warnFunc when mw is nil")
	})
}

func TestManaWatcherEmptyManaString(t *testing.T) {
	mw := NewManaWatcher([]int{50})

	var called bool
	mw.CheckAndWarn("", func(w string) {
		called = true
	})

	if called {
		t.Error("should not call warnFunc for empty mana string")
	}
}

func TestManaWatcherParseError(t *testing.T) {
	mw := NewManaWatcher([]int{50})

	var called bool
	mw.CheckAndWarn("invalid", func(w string) {
		called = true
	})

	if called {
		t.Error("should not call warnFunc for invalid mana string")
	}
}

func TestManaWatcherResetsAtMidnight(t *testing.T) {
	// Create watcher with lastReset set to yesterday
	yesterday := time.Now().Add(-24 * time.Hour).Truncate(24 * time.Hour)
	mw := &ManaWatcher{
		thresholds: []int{50},
		firedToday: map[int]bool{50: true},
		lastReset:  yesterday,
	}

	var warned string
	mw.CheckAndWarn("25%", func(w string) {
		warned = w
	})

	if warned == "" {
		t.Error("expected warning after midnight reset")
	}
}
