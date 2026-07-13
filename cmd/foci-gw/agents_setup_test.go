package main

import (
	"testing"

	"foci/internal/config"
)

// TestBuildCompactor_LiveConfigEditUpdatesCompactor proves the OnChange
// callback buildCompactor registers on p.resolvedLive actually reaches the
// live Compactor instance — not just that the resolved snapshot swaps
// (already covered by TestLiveApplyResolvedActuallySwaps), but that the
// derived handle itself picks up the new values without being reconstructed.
func TestBuildCompactor_LiveConfigEditUpdatesCompactor(t *testing.T) {
	p := minimalSetupParams(t, "test")
	p.resolved.Compaction = config.ResolvedCompaction{
		CompactionThreshold:        0.5,
		CompactionThresholdSet:     true,
		CompactionPreserveMessages: 10,
	}
	p.resolvedLive.Store(p.resolved)

	compactor, _ := buildCompactor(p, nil)
	if got := compactor.Threshold(); got != 0.5 {
		t.Fatalf("initial Threshold() = %v, want 0.5", got)
	}
	if got := compactor.PreserveMessages(); got != 10 {
		t.Fatalf("initial PreserveMessages() = %v, want 10", got)
	}

	fresh := *p.resolved
	fresh.Compaction.CompactionThreshold = 0.9
	fresh.Compaction.CompactionThresholdSet = false // switches to the non-linear curve
	fresh.Compaction.CompactionPreserveMessages = 3
	p.resolvedLive.Store(&fresh)

	if got := compactor.Threshold(); got != 0.9 {
		t.Errorf("after live edit, Threshold() = %v, want 0.9 — OnChange did not take effect", got)
	}
	if got := compactor.PreserveMessages(); got != 3 {
		t.Errorf("after live edit, PreserveMessages() = %v, want 3 — OnChange did not take effect", got)
	}
	// EffectiveThreshold now uses the non-linear curve for large windows
	// instead of the flat 0.9 fraction — proves SetNonlinear also applied.
	if got := compactor.EffectiveThreshold(1_000_000); got == int(0.9*1_000_000) {
		t.Errorf("EffectiveThreshold(1M) = %d still using flat fraction — SetNonlinear did not take effect", got)
	}
}
