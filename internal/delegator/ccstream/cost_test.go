package ccstream

import "testing"

// The #1674 regression suite. CC's ModelUsage counters are CUMULATIVE over the
// CC process, so every one of these asserts that a per-turn figure is recovered
// by SUBTRACTION. Reading them raw is what inflated api.db totals ~13x.

// TestModelUsageDelta_FirstSnapshotIsWholeValue: with nothing seen before, the
// current snapshot IS this turn's figure. A fresh Backend means a fresh CC
// process, whose counters started at zero.
func TestModelUsageDelta_FirstSnapshotIsWholeValue(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	got := b.modelUsageDelta("m", ModelUsage{OutputTokens: 56, CacheReadInputTokens: 21624, CostUSD: 0.0099234})

	if got.OutputTokens != 56 {
		t.Errorf("OutputTokens = %d, want 56", got.OutputTokens)
	}
	if got.CacheReadInputTokens != 21624 {
		t.Errorf("CacheReadInputTokens = %d, want 21624", got.CacheReadInputTokens)
	}
	if got.CostUSD != 0.0099234 {
		t.Errorf("CostUSD = %v, want 0.0099234", got.CostUSD)
	}
}

// TestModelUsageDelta_SubtractsPreviousSnapshot uses the real probe numbers
// (2026-08-05, one CC process, four identical trivial prompts). Every field
// must come back as the step, not the running total.
func TestModelUsageDelta_SubtractsPreviousSnapshot(t *testing.T) {
	t.Parallel()
	b := &Backend{}

	snapshots := []ModelUsage{
		{OutputTokens: 56, CacheReadInputTokens: 21624, CostUSD: 0.0099234},
		{OutputTokens: 105, CacheReadInputTokens: 46722, CostUSD: 0.0141382},
		{OutputTokens: 141, CacheReadInputTokens: 72545, CostUSD: 0.0170425},
		{OutputTokens: 174, CacheReadInputTokens: 98434, CostUSD: 0.0199124},
	}
	wantOut := []int{56, 49, 36, 33}
	wantCacheRead := []int{21624, 25098, 25823, 25889}

	for i, snap := range snapshots {
		got := b.modelUsageDelta("m", snap)
		if got.OutputTokens != wantOut[i] {
			t.Errorf("turn %d: OutputTokens = %d, want %d (raw cumulative was %d)",
				i+1, got.OutputTokens, wantOut[i], snap.OutputTokens)
		}
		if got.CacheReadInputTokens != wantCacheRead[i] {
			t.Errorf("turn %d: CacheReadInputTokens = %d, want %d (raw cumulative was %d)",
				i+1, got.CacheReadInputTokens, wantCacheRead[i], snap.CacheReadInputTokens)
		}
	}
}

// TestModelUsageDelta_ResetsWhenCounterGoesBackwards is the --resume case: the
// CC process restarted beneath a reused Backend, so its counters restarted at
// zero while the session id stayed the same.
//
// Probe-verified 2026-08-05: a process ended cumulative at $0.0243122 / 139
// output tokens; resuming that SAME session in a new process reported
// $0.0035427 / 38 on its first turn. Subtracting blindly would mint a large
// negative, which is why this case is guarded per field.
func TestModelUsageDelta_ResetsWhenCounterGoesBackwards(t *testing.T) {
	t.Parallel()
	b := &Backend{}

	b.modelUsageDelta("m", ModelUsage{OutputTokens: 139, CacheReadInputTokens: 68022, CostUSD: 0.0243122})
	got := b.modelUsageDelta("m", ModelUsage{OutputTokens: 38, CacheReadInputTokens: 25667, CostUSD: 0.0035427})

	if got.OutputTokens != 38 {
		t.Errorf("OutputTokens = %d, want 38 (post-reset value taken whole)", got.OutputTokens)
	}
	if got.CacheReadInputTokens != 25667 {
		t.Errorf("CacheReadInputTokens = %d, want 25667", got.CacheReadInputTokens)
	}
	if got.CostUSD != 0.0035427 {
		t.Errorf("CostUSD = %v, want 0.0035427", got.CostUSD)
	}
	if got.OutputTokens < 0 || got.CacheReadInputTokens < 0 || got.CostUSD < 0 {
		t.Error("negative delta: the counter-went-backwards guard did not fire")
	}
}

// TestModelUsageDelta_MixedResetGuardsEachFieldSeparately: a snapshot where
// only SOME counters went backwards must not produce a negative on any field.
// Guarding on one field (say cost) and trusting it to speak for the rest is
// exactly how a negative token count would get through.
func TestModelUsageDelta_MixedResetGuardsEachFieldSeparately(t *testing.T) {
	t.Parallel()
	b := &Backend{}

	b.modelUsageDelta("m", ModelUsage{OutputTokens: 500, CacheReadInputTokens: 90000, CostUSD: 1.0})
	got := b.modelUsageDelta("m", ModelUsage{OutputTokens: 10, CacheReadInputTokens: 95000, CostUSD: 0.02})

	if got.OutputTokens != 10 {
		t.Errorf("OutputTokens = %d, want 10 (went backwards → taken whole)", got.OutputTokens)
	}
	if got.CacheReadInputTokens != 5000 {
		t.Errorf("CacheReadInputTokens = %d, want 5000 (went forwards → subtracted)", got.CacheReadInputTokens)
	}
	if got.CostUSD != 0.02 {
		t.Errorf("CostUSD = %v, want 0.02 (went backwards → taken whole)", got.CostUSD)
	}
}

// TestModelUsageDelta_PerModelIndependence: a subagent model's counters must
// not be subtracted from the primary's. They are separate keys in CC's map and
// separate running totals.
func TestModelUsageDelta_PerModelIndependence(t *testing.T) {
	t.Parallel()
	b := &Backend{}

	b.modelUsageDelta("opus", ModelUsage{OutputTokens: 1000})
	if got := b.modelUsageDelta("haiku", ModelUsage{OutputTokens: 30}); got.OutputTokens != 30 {
		t.Errorf("haiku OutputTokens = %d, want 30 — opus's total must not leak across models", got.OutputTokens)
	}
	if got := b.modelUsageDelta("opus", ModelUsage{OutputTokens: 1200}); got.OutputTokens != 200 {
		t.Errorf("opus OutputTokens = %d, want 200 — haiku's snapshot must not disturb opus", got.OutputTokens)
	}
}
