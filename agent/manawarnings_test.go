package agent

import (
	"path/filepath"
	"testing"
	"time"

	"foci/state"
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
	mw.CheckAndWarn("25%", "in 2h", func(w string) {
		warned = w
	})

	if warned == "" {
		t.Error("expected warning at 25% mana (crossed below 50% threshold)")
	}
	if warned != "low mana: 25% remaining (resets in 2h)" {
		t.Errorf("warning = %q", warned)
	}
}

func TestManaWatcherFiresOnlyOnce(t *testing.T) {
	mw := NewManaWatcher("", []int{50})

	var count int
	mw.CheckAndWarn("25%", "in 2h", func(w string) {
		count++
	})

	mw.CheckAndWarn("25%", "in 2h", func(w string) {
		count++
	})

	if count != 1 {
		t.Errorf("count = %d, want 1 (should only fire once)", count)
	}
}

func TestManaWatcherDoesNotFireAboveThreshold(t *testing.T) {
	mw := NewManaWatcher("", []int{50, 25})

	var warned string
	mw.CheckAndWarn("75%", "in 4h", func(w string) {
		warned = w
	})

	if warned != "" {
		t.Error("should not warn when above threshold")
	}
}

func TestManaWatcherNilSafe(t *testing.T) {
	var mw *ManaWatcher

	mw.CheckAndWarn("50%", "in 2h", func(w string) {
		t.Error("should not call warnFunc when mw is nil")
	})
}

func TestManaWatcherEmptyManaString(t *testing.T) {
	mw := NewManaWatcher("", []int{50})

	var called bool
	mw.CheckAndWarn("", "in 2h", func(w string) {
		called = true
	})

	if called {
		t.Error("should not call warnFunc for empty mana string")
	}
}

func TestManaWatcherParseError(t *testing.T) {
	mw := NewManaWatcher("", []int{50})

	var called bool
	mw.CheckAndWarn("invalid", "in 2h", func(w string) {
		called = true
	})

	if called {
		t.Error("should not call warnFunc for invalid mana string")
	}
}

func TestManaWatcherResetsAtMidnight(t *testing.T) {
	yesterday := time.Now().Add(-24 * time.Hour).Truncate(24 * time.Hour)
	mw := &ManaWatcher{
		name:       "mana",
		thresholds: []int{50},
		firedToday: map[int]bool{50: true},
		lastReset:  yesterday,
	}

	var warned string
	mw.CheckAndWarn("25%", "in 2h", func(w string) {
		warned = w
	})

	if warned == "" {
		t.Error("expected warning after midnight reset")
	}
}

func TestManaWatcherCustomName(t *testing.T) {
	mw := NewManaWatcher("juice", []int{50})

	var warned string
	mw.CheckAndWarn("25%", "in 2h", func(w string) {
		warned = w
	})

	if warned != "low juice: 25% remaining (resets in 2h)" {
		t.Errorf("warning = %q, want %q", warned, "low juice: 25% remaining (resets in 2h)")
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

	mw.CheckAndWarn("45%", "in 3h", warnFn)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(warnings))
	}
	if warnings[0] != "low mana: 45% remaining (resets in 3h)" {
		t.Errorf("warning[0] = %q", warnings[0])
	}

	mw.CheckAndWarn("20%", "in 2h", warnFn)
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %d", len(warnings))
	}
	if warnings[1] != "low mana: 20% remaining (resets in 2h)" {
		t.Errorf("warning[1] = %q", warnings[1])
	}

	mw.CheckAndWarn("8%", "in 1h", warnFn)
	if len(warnings) != 3 {
		t.Fatalf("expected 3 warnings, got %d", len(warnings))
	}
	if warnings[2] != "low mana: 8% remaining (resets in 1h)" {
		t.Errorf("warning[2] = %q", warnings[2])
	}
}

func TestManaWatcherSkipsSystemMessages(t *testing.T) {
	mw := NewManaWatcher("mana", []int{50})

	var warned bool
	mw.CheckAndWarn("25%", "in 2h", func(w string) { warned = true })
	if !warned {
		t.Error("expected watcher to fire for user message scenario")
	}
}

func TestManaWatcherNilWarnFunc(t *testing.T) {
	mw := NewManaWatcher("mana", []int{50})
	mw.CheckAndWarn("25%", "in 2h", nil)
}

func TestManaWatcherExactThresholdValue(t *testing.T) {
	mw := NewManaWatcher("mana", []int{50})

	var warned string
	mw.CheckAndWarn("50%", "in 2h 30m", func(w string) { warned = w })

	if warned == "" {
		t.Error("expected warning when mana equals threshold exactly")
	}
	if warned != "low mana: 50% remaining (resets in 2h 30m)" {
		t.Errorf("warning = %q", warned)
	}
}

func TestManaWatcherEmptyResetTime(t *testing.T) {
	mw := NewManaWatcher("mana", []int{50})

	var warned string
	mw.CheckAndWarn("25%", "", func(w string) { warned = w })

	if warned == "" {
		t.Error("expected warning even without reset time")
	}
	if warned != "low mana: 25% remaining" {
		t.Errorf("warning = %q, want %q", warned, "low mana: 25% remaining")
	}
}

func TestManaWatcherPersistenceSavesFiredThreshold(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")
	store := state.New(statePath)

	mw := NewManaWatcher("mana", []int{50})
	mw.SetStore(store)

	var warned bool
	mw.CheckAndWarn("25%", "in 2h", func(w string) { warned = true })

	if !warned {
		t.Fatal("expected warning to fire")
	}

	store2 := state.New(statePath)
	if err := store2.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	var savedState manaWatcherState
	if !store2.Get("mana:mana", &savedState) {
		t.Fatal("expected state to be saved")
	}

	if !savedState.FiredToday[50] {
		t.Error("expected threshold 50 to be marked as fired")
	}
}

func TestManaWatcherRestoreLoadsFiredThreshold(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")
	store := state.New(statePath)

	today := time.Now().Truncate(24 * time.Hour)
	initialState := manaWatcherState{
		FiredToday: map[int]bool{50: true, 25: true},
		LastReset:  today,
	}
	if err := store.Set("mana:mana", initialState); err != nil {
		t.Fatalf("set initial state: %v", err)
	}

	store2 := state.New(statePath)
	if err := store2.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	mw := NewManaWatcher("mana", []int{50, 25})
	mw.SetStore(store2)
	mw.Restore()

	var firedCount int
	mw.CheckAndWarn("20%", "in 1h", func(w string) { firedCount++ })

	if firedCount != 0 {
		t.Errorf("expected no warning (thresholds already fired), got %d", firedCount)
	}
}

func TestManaWatcherRestoreIgnoresStaleState(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")
	store := state.New(statePath)

	yesterday := time.Now().Add(-24 * time.Hour).Truncate(24 * time.Hour)
	staleState := manaWatcherState{
		FiredToday: map[int]bool{50: true},
		LastReset:  yesterday,
	}
	if err := store.Set("mana:mana", staleState); err != nil {
		t.Fatalf("set stale state: %v", err)
	}

	store2 := state.New(statePath)
	if err := store2.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	mw := NewManaWatcher("mana", []int{50})
	mw.SetStore(store2)
	mw.Restore()

	var warned bool
	mw.CheckAndWarn("25%", "in 2h", func(w string) { warned = true })

	if !warned {
		t.Error("expected warning (stale state should be ignored)")
	}
}

func TestManaWatcherPersistenceAfterRestart(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")

	store1 := state.New(statePath)
	mw1 := NewManaWatcher("mana", []int{50, 25})
	mw1.SetStore(store1)

	mw1.CheckAndWarn("30%", "in 2h", func(w string) {})

	store2 := state.New(statePath)
	if err := store2.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}
	mw2 := NewManaWatcher("mana", []int{50, 25})
	mw2.SetStore(store2)
	mw2.Restore()

	var warned bool
	mw2.CheckAndWarn("30%", "in 2h", func(w string) { warned = true })

	if warned {
		t.Error("should not warn again after restore (50 threshold already fired)")
	}

	var warned25 bool
	mw2.CheckAndWarn("20%", "in 1h", func(w string) { warned25 = true })

	if !warned25 {
		t.Error("should warn for 25 threshold (not yet fired)")
	}
}

func TestManaWatcherNilRestore(t *testing.T) {
	var mw *ManaWatcher
	mw.Restore()
}

func TestManaWatcherRestoreWithoutStore(t *testing.T) {
	mw := NewManaWatcher("mana", []int{50})
	mw.Restore()

	var warned bool
	mw.CheckAndWarn("25%", "in 2h", func(w string) { warned = true })

	if !warned {
		t.Error("expected warning when no store set")
	}
}

func TestManaWatcherPersistenceCustomName(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")
	store := state.New(statePath)

	mw := NewManaWatcher("juice", []int{50})
	mw.SetStore(store)

	mw.CheckAndWarn("25%", "in 2h", func(w string) {})

	store2 := state.New(statePath)
	if err := store2.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	var savedState manaWatcherState
	if !store2.Get("mana:juice", &savedState) {
		t.Fatal("expected state to be saved with custom name key")
	}
}
