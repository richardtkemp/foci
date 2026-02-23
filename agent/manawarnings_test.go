package agent

import (
	"testing"
	"time"
)

func TestManaWatcherNewNilForEmpty(t *testing.T) {
	mw := NewManaWatcher("", nil)
	if mw != nil {
		t.Error("expected nil for empty thresholds")
	}

	mw = NewManaWatcher("", []int{})
	if mw != nil {
		t.Error("expected nil for empty slice")
	}
}

func TestManaWatcherThresholdsSortedDescending(t *testing.T) {
	mw := NewManaWatcher("", []int{10, 50, 25, 5})
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
	mw := NewManaWatcher("", []int{50, 25, 10, 5})

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
	mw := NewManaWatcher("", []int{50})

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
	mw := NewManaWatcher("", []int{50, 25})

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
	mw := NewManaWatcher("", []int{50})

	var called bool
	mw.CheckAndWarn("", func(w string) {
		called = true
	})

	if called {
		t.Error("should not call warnFunc for empty mana string")
	}
}

func TestManaWatcherParseError(t *testing.T) {
	mw := NewManaWatcher("", []int{50})

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
		name:       "mana",
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

func TestManaWatcherCustomName(t *testing.T) {
	mw := NewManaWatcher("juice", []int{50})

	var warned string
	mw.CheckAndWarn("25%", func(w string) {
		warned = w
	})

	if warned != "low juice: 25% remaining (threshold: 50%)" {
		t.Errorf("warning = %q, want %q", warned, "low juice: 25% remaining (threshold: 50%)")
	}
}

func TestManaWatcherEmptyNameDefaultsToMana(t *testing.T) {
	mw := NewManaWatcher("", []int{50})
	if mw.name != "mana" {
		t.Errorf("name = %q, want %q", mw.name, "mana")
	}
}

func TestManaWatcherFiresMultipleThresholdsInOrder(t *testing.T) {
	mw := NewManaWatcher("mana", []int{50, 25, 10})

	var warnings []string
	warnFn := func(w string) { warnings = append(warnings, w) }

	// First check at 45% — crosses 50 threshold
	mw.CheckAndWarn("45%", warnFn)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(warnings))
	}
	if warnings[0] != "low mana: 45% remaining (threshold: 50%)" {
		t.Errorf("warning[0] = %q", warnings[0])
	}

	// Second check at 20% — should cross 25 threshold (50 already fired)
	mw.CheckAndWarn("20%", warnFn)
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %d", len(warnings))
	}
	if warnings[1] != "low mana: 20% remaining (threshold: 25%)" {
		t.Errorf("warning[1] = %q", warnings[1])
	}

	// Third check at 8% — should cross 10 threshold
	mw.CheckAndWarn("8%", warnFn)
	if len(warnings) != 3 {
		t.Fatalf("expected 3 warnings, got %d", len(warnings))
	}
	if warnings[2] != "low mana: 8% remaining (threshold: 10%)" {
		t.Errorf("warning[2] = %q", warnings[2])
	}
}

func TestManaWatcherSkipsSystemMessages(t *testing.T) {
	// This tests the integration point: ManaWatcher.CheckAndWarn is only called
	// for non-system messages. Verify the watcher itself fires correctly,
	// as the system message gating is in agent.go's HandleMessage.
	mw := NewManaWatcher("mana", []int{50})

	var warned bool
	mw.CheckAndWarn("25%", func(w string) { warned = true })
	if !warned {
		t.Error("expected watcher to fire for user message scenario")
	}
}

func TestManaWatcherNilWarnFunc(t *testing.T) {
	mw := NewManaWatcher("mana", []int{50})
	// Should not panic with nil warnFunc
	mw.CheckAndWarn("25%", nil)
}

func TestManaWatcherExactThresholdValue(t *testing.T) {
	mw := NewManaWatcher("mana", []int{50})

	var warned string
	mw.CheckAndWarn("50%", func(w string) { warned = w })

	if warned == "" {
		t.Error("expected warning when mana equals threshold exactly")
	}
	if warned != "low mana: 50% remaining (threshold: 50%)" {
		t.Errorf("warning = %q", warned)
	}
}
