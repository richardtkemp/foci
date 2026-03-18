package compaction

import (
	"testing"
	"time"
)

// TestManaResetImminent_WithinThreshold verifies that a reset time within the
// threshold window returns true.
func TestManaResetImminent_WithinThreshold(t *testing.T) {
	resetsAt := time.Now().Add(3 * time.Minute)
	if !ManaResetImminent(resetsAt, 5*time.Minute) {
		t.Error("ManaResetImminent = false, want true (3m until reset, 5m threshold)")
	}
}

// TestManaResetImminent_BeyondThreshold verifies that a reset time beyond the
// threshold window returns false.
func TestManaResetImminent_BeyondThreshold(t *testing.T) {
	resetsAt := time.Now().Add(2 * time.Hour)
	if ManaResetImminent(resetsAt, 5*time.Minute) {
		t.Error("ManaResetImminent = true, want false (2h until reset, 5m threshold)")
	}
}

// TestManaResetImminent_PastReset verifies that a reset time in the past
// returns false.
func TestManaResetImminent_PastReset(t *testing.T) {
	resetsAt := time.Now().Add(-5 * time.Minute)
	if ManaResetImminent(resetsAt, 5*time.Minute) {
		t.Error("ManaResetImminent = true, want false (reset in the past)")
	}
}

// TestManaResetImminent_ZeroTime verifies that a zero reset time returns false.
func TestManaResetImminent_ZeroTime(t *testing.T) {
	if ManaResetImminent(time.Time{}, 5*time.Minute) {
		t.Error("ManaResetImminent = true for zero time, want false")
	}
}

// TestManaResetImminent_ZeroThreshold verifies that a zero threshold disables
// the check.
func TestManaResetImminent_ZeroThreshold(t *testing.T) {
	resetsAt := time.Now().Add(3 * time.Minute)
	if ManaResetImminent(resetsAt, 0) {
		t.Error("ManaResetImminent = true with zero threshold, want false")
	}
}

// TestManaResetImminent_ExactBoundary verifies the boundary condition where
// time until reset equals the threshold (should return false since < not <=).
func TestManaResetImminent_ExactBoundary(t *testing.T) {
	// At exactly the boundary, time.Until may be slightly less due to execution
	// time, so test with a value just beyond the threshold.
	resetsAt := time.Now().Add(5*time.Minute + time.Second)
	if ManaResetImminent(resetsAt, 5*time.Minute) {
		t.Error("ManaResetImminent = true at boundary, want false")
	}
}

// TestCompactorGettersSetters verifies Threshold, PreserveMessages, and SetPreserveMessages.
func TestCompactorGettersSetters(t *testing.T) {
	c := NewCompactor(nil, 0.8).WithConfig(4096, 4, 25)

	if c.Threshold() != 0.8 {
		t.Errorf("Threshold() = %f, want 0.8", c.Threshold())
	}
	if c.PreserveMessages() != 25 {
		t.Errorf("PreserveMessages() = %d, want 25", c.PreserveMessages())
	}

	c.SetPreserveMessages(100)
	if c.PreserveMessages() != 100 {
		t.Errorf("after SetPreserveMessages(100): PreserveMessages() = %d, want 100", c.PreserveMessages())
	}
}
