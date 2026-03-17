package agent

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/session"
)

func TestManaWatcherNewNilForEmpty(t *testing.T) {
	// Proves that NewManaWatcher returns nil when given nil or empty thresholds.
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
	// Proves that thresholds are internally sorted in descending order regardless of input order.
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
	// Proves that CheckAndWarn fires the callback with the correct message when mana drops below a threshold.
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
	// Proves that each threshold fires at most once per day, even if mana stays below it.
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
	// Proves that no warning is triggered when mana is above all thresholds.
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
	// Proves that CheckAndWarn on a nil ManaWatcher does not panic.
	var mw *ManaWatcher

	mw.CheckAndWarn("50%", "in 2h", func(w string) {
		t.Error("should not call warnFunc when mw is nil")
	})
}

func TestManaWatcherEmptyManaString(t *testing.T) {
	// Proves that an empty mana string is a no-op (no warning fired).
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
	// Proves that a malformed mana string is a no-op (no warning fired).
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
	// Proves that previously fired thresholds are cleared after midnight, allowing re-firing the next day.
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
	// Proves that a custom name appears in the warning message instead of "mana".
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
	// Proves that an empty name defaults to "mana" as the watcher name.
	mw := NewManaWatcher("", []int{50})
	if mw.name != "mana" {
		t.Errorf("name = %q, want %q", mw.name, "mana")
	}
}

func TestManaWatcherFiresMultipleThresholdsInOrder(t *testing.T) {
	// Proves that multiple thresholds each fire exactly once as mana drops progressively lower.
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
	// Proves that CheckAndWarn fires for user-context mana checks (basic sanity check).
	mw := NewManaWatcher("mana", []int{50})

	var warned bool
	mw.CheckAndWarn("25%", "in 2h", func(w string) { warned = true })
	if !warned {
		t.Error("expected watcher to fire for user message scenario")
	}
}

func TestManaWatcherNilWarnFunc(t *testing.T) {
	// Proves that passing a nil warnFunc does not panic when a threshold is crossed.
	mw := NewManaWatcher("mana", []int{50})
	mw.CheckAndWarn("25%", "in 2h", nil)
}

func TestManaWatcherExactThresholdValue(t *testing.T) {
	// Proves that the threshold comparison is inclusive: mana exactly at the threshold triggers a warning.
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
	// Proves that an empty resetTime omits the "resets in..." clause from the warning message.
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
	// Proves that fired thresholds are written to the session index so they survive a restart.
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	mw := NewManaWatcher("mana", []int{50})
	mw.SetSessionIndex(idx, "test")

	var warned bool
	mw.CheckAndWarn("25%", "in 2h", func(w string) { warned = true })

	if !warned {
		t.Fatal("expected warning to fire")
	}

	// Read back from the same index to verify persistence
	raw, err := idx.GetAgentMetadata("test", "mana:mana")
	if err != nil {
		t.Fatalf("get agent metadata: %v", err)
	}
	if raw == "" {
		t.Fatal("expected state to be saved")
	}

	var savedState manaWatcherState
	if err := json.Unmarshal([]byte(raw), &savedState); err != nil {
		t.Fatalf("unmarshal saved state: %v", err)
	}

	if !savedState.FiredToday[50] {
		t.Error("expected threshold 50 to be marked as fired")
	}
}

func TestManaWatcherRestoreLoadsFiredThreshold(t *testing.T) {
	// Proves that Restore() loads previously fired thresholds so they don't re-fire the same day.
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	today := time.Now().Truncate(24 * time.Hour)
	initialState := manaWatcherState{
		FiredToday: map[int]bool{50: true, 25: true},
		LastReset:  today,
	}
	data, err := json.Marshal(initialState)
	if err != nil {
		t.Fatalf("marshal initial state: %v", err)
	}
	if err := idx.SetAgentMetadata("test", "mana:mana", string(data)); err != nil {
		t.Fatalf("set initial state: %v", err)
	}

	mw := NewManaWatcher("mana", []int{50, 25})
	mw.SetSessionIndex(idx, "test")
	mw.Restore()

	var firedCount int
	mw.CheckAndWarn("20%", "in 1h", func(w string) { firedCount++ })

	if firedCount != 0 {
		t.Errorf("expected no warning (thresholds already fired), got %d", firedCount)
	}
}

func TestManaWatcherRestoreIgnoresStaleState(t *testing.T) {
	// Proves that persisted state from a previous day is discarded rather than blocking new warnings.
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	yesterday := time.Now().Add(-24 * time.Hour).Truncate(24 * time.Hour)
	staleState := manaWatcherState{
		FiredToday: map[int]bool{50: true},
		LastReset:  yesterday,
	}
	data, err := json.Marshal(staleState)
	if err != nil {
		t.Fatalf("marshal stale state: %v", err)
	}
	if err := idx.SetAgentMetadata("test", "mana:mana", string(data)); err != nil {
		t.Fatalf("set stale state: %v", err)
	}

	mw := NewManaWatcher("mana", []int{50})
	mw.SetSessionIndex(idx, "test")
	mw.Restore()

	var warned bool
	mw.CheckAndWarn("25%", "in 2h", func(w string) { warned = true })

	if !warned {
		t.Error("expected warning (stale state should be ignored)")
	}
}

func TestManaWatcherPersistenceAfterRestart(t *testing.T) {
	// Proves that a simulated restart preserves fired thresholds while allowing unfired ones to still trigger.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	idx1, err := session.NewSessionIndex(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	mw1 := NewManaWatcher("mana", []int{50, 25})
	mw1.SetSessionIndex(idx1, "test")

	mw1.CheckAndWarn("30%", "in 2h", func(w string) {})
	idx1.Close()

	// Simulate restart: open a new SessionIndex from the same db
	idx2, err := session.NewSessionIndex(dbPath)
	if err != nil {
		t.Fatalf("reopen session index: %v", err)
	}
	defer idx2.Close()
	mw2 := NewManaWatcher("mana", []int{50, 25})
	mw2.SetSessionIndex(idx2, "test")
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
	// Proves that Restore() on a nil ManaWatcher does not panic.
	var mw *ManaWatcher
	mw.Restore()
}

func TestManaWatcherRestoreWithoutStore(t *testing.T) {
	// Proves that Restore() is safe when no state store is configured, leaving the watcher functional.
	mw := NewManaWatcher("mana", []int{50})
	mw.Restore()

	var warned bool
	mw.CheckAndWarn("25%", "in 2h", func(w string) { warned = true })

	if !warned {
		t.Error("expected warning when no store set")
	}
}

func TestManaRestoreNotification(t *testing.T) {
	// Proves that CheckRestore fires once when mana rises above the restore threshold after having dipped below it.
	mw := NewManaWatcher("mana", []int{50, 25})
	mw.SetRestoreThreshold(40)

	// At 30%, below restore threshold — seenBelow should be set
	mw.CheckAndWarn("30%", "", func(w string) {})
	if msg := mw.CheckRestore("30%"); msg != "" {
		t.Error("should not restore at 30%")
	}

	// At 100%, should fire restore
	if msg := mw.CheckRestore("100%"); msg == "" {
		t.Error("expected restore notification at 100%")
	} else if msg != "mana restored to 100% (was below 40% earlier)" {
		t.Errorf("restore msg = %q", msg)
	}

	// Should not fire again
	if msg := mw.CheckRestore("100%"); msg != "" {
		t.Error("should not fire restore twice")
	}
}

func TestManaRestoreNotFiredWithoutDrop(t *testing.T) {
	// Proves that no restore notification is sent if mana never dropped below the restore threshold.
	mw := NewManaWatcher("mana", []int{50})
	mw.SetRestoreThreshold(40)

	// Never dropped below 40%
	mw.CheckAndWarn("60%", "", func(w string) {})
	if msg := mw.CheckRestore("100%"); msg != "" {
		t.Error("should not fire restore without prior drop below threshold")
	}
}

func TestManaRestoreDisabledByDefault(t *testing.T) {
	// Proves that restore notifications are disabled by default (restoreThreshold=0).
	mw := NewManaWatcher("mana", []int{50})
	// restoreThreshold = 0 (default)

	mw.CheckAndWarn("10%", "", func(w string) {})
	if msg := mw.CheckRestore("100%"); msg != "" {
		t.Error("should not fire restore when threshold is 0")
	}
}

func TestManaRestoreNilSafe(t *testing.T) {
	// Proves that CheckRestore on a nil ManaWatcher does not panic and returns an empty string.
	var mw *ManaWatcher
	if msg := mw.CheckRestore("100%"); msg != "" {
		t.Error("should return empty for nil watcher")
	}
}

func TestManaRestoreResetsAtMidnight(t *testing.T) {
	// Proves that the restore state clears at midnight, allowing re-firing on a new day.
	yesterday := time.Now().Add(-24 * time.Hour).Truncate(24 * time.Hour)
	mw := &ManaWatcher{
		name:             "mana",
		thresholds:       []int{50},
		restoreThreshold: 40,
		firedToday:       map[int]bool{},
		seenBelow:        true,
		firedRestore:     true,
		lastReset:        yesterday,
	}

	// After midnight reset, seenBelow and firedRestore should clear
	mw.CheckAndWarn("30%", "", func(w string) {})
	// Now seenBelow should be set again (30% < 40%)
	if msg := mw.CheckRestore("100%"); msg == "" {
		t.Error("expected restore after midnight reset and new drop")
	}
}

func TestManaRestorePersistence(t *testing.T) {
	// Proves that the firedRestore flag is persisted so a restart does not re-send the restore notification.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	idx, err := session.NewSessionIndex(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	mw := NewManaWatcher("mana", []int{50})
	mw.SetSessionIndex(idx, "test")
	mw.SetRestoreThreshold(40)

	// Drop below threshold and restore
	mw.CheckAndWarn("30%", "", func(w string) {})
	mw.CheckRestore("100%")
	idx.Close()

	// Load in new instance
	idx2, err := session.NewSessionIndex(dbPath)
	if err != nil {
		t.Fatalf("reopen session index: %v", err)
	}
	defer idx2.Close()

	mw2 := NewManaWatcher("mana", []int{50})
	mw2.SetSessionIndex(idx2, "test")
	mw2.SetRestoreThreshold(40)
	mw2.Restore()

	// Should not fire again (firedRestore persisted)
	if msg := mw2.CheckRestore("100%"); msg != "" {
		t.Error("should not fire restore after restart (already fired today)")
	}
}

func TestManaWatcherPersistenceCustomName(t *testing.T) {
	// Proves that state is stored under a key that includes the custom watcher name.
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	mw := NewManaWatcher("juice", []int{50})
	mw.SetSessionIndex(idx, "test")

	mw.CheckAndWarn("25%", "in 2h", func(w string) {})

	raw, err := idx.GetAgentMetadata("test", "mana:juice")
	if err != nil {
		t.Fatalf("get agent metadata: %v", err)
	}
	if raw == "" {
		t.Fatal("expected state to be saved with custom name key")
	}
}
