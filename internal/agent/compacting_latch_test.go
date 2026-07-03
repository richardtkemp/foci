package agent

import (
	"testing"
	"time"
)

// TestIsCompacting_Latch verifies the compaction-in-flight latch used by
// /status (#725): mark sets it, clear removes it. Session keys are stable
// identities (compaction archives in place, no rotation), so the latch is
// keyed directly by the session key.
func TestIsCompacting_Latch(t *testing.T) {
	t.Parallel()
	a := &Agent{}

	const key = "clutch/cmain123"
	if a.IsCompacting(key) {
		t.Fatal("should not be compacting before mark")
	}

	a.markCompacting(key)
	if !a.IsCompacting(key) {
		t.Fatal("should be compacting after mark")
	}

	// A different session's key must not read as compacting — the latch is
	// per-session-key.
	if a.IsCompacting("clutch/cother") {
		t.Error("unrelated session key should not read as compacting")
	}

	a.clearCompacting(key)
	if a.IsCompacting(key) {
		t.Error("should not be compacting after clear")
	}
}

// TestIsCompacting_SelfHeal verifies that an expired latch (compaction that
// errored between mark and a skipped clear) reads as not-compacting and is
// purged on read, so the flag can never wedge "compacting" forever (#725).
func TestIsCompacting_SelfHeal(t *testing.T) {
	t.Parallel()
	a := &Agent{}

	const key = "clutch/cmain123"

	// Stale deadline in the past.
	a.compacting.Store(key, time.Now().Add(-time.Second))
	if a.IsCompacting(key) {
		t.Error("expired latch should read as not-compacting")
	}
	if _, ok := a.compacting.Load(key); ok {
		t.Error("expired latch should be purged on read")
	}

	// Fresh deadline in the future.
	a.compacting.Store(key, time.Now().Add(time.Minute))
	if !a.IsCompacting(key) {
		t.Error("fresh latch should read as compacting")
	}
}
