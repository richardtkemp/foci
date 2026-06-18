package agent

import (
	"testing"
	"time"

	"foci/internal/session"
)

// TestIsCompacting_Latch verifies the compaction-in-flight latch used by
// /status (#725): mark sets it, clear removes it, and the key is normalised to
// SessionKeyBase so a version-rotated key (compaction rotates mid-flight) still
// matches.
func TestIsCompacting_Latch(t *testing.T) {
	t.Parallel()
	a := &Agent{}

	const full = "clutch/main123/v4"
	if a.IsCompacting(full) {
		t.Fatal("should not be compacting before mark")
	}

	a.markCompacting(full)
	if !a.IsCompacting(full) {
		t.Fatal("should be compacting after mark")
	}

	// A rotated version of the same base key must still read as compacting —
	// this is the whole reason for keying by SessionKeyBase.
	rotated := session.SessionKeyBase(full) + "/v5"
	if !a.IsCompacting(rotated) {
		t.Error("rotated session key should still read as compacting (base-keyed)")
	}

	a.clearCompacting(rotated)
	if a.IsCompacting(full) {
		t.Error("should not be compacting after clear (via rotated key)")
	}
}

// TestIsCompacting_SelfHeal verifies that an expired latch (compaction that
// errored between mark and a skipped clear) reads as not-compacting and is
// purged on read, so the flag can never wedge "compacting" forever (#725).
func TestIsCompacting_SelfHeal(t *testing.T) {
	t.Parallel()
	a := &Agent{}

	const full = "clutch/main123/v4"
	base := session.SessionKeyBase(full)

	// Stale deadline in the past.
	a.compacting.Store(base, time.Now().Add(-time.Second))
	if a.IsCompacting(full) {
		t.Error("expired latch should read as not-compacting")
	}
	if _, ok := a.compacting.Load(base); ok {
		t.Error("expired latch should be purged on read")
	}

	// Fresh deadline in the future.
	a.compacting.Store(base, time.Now().Add(time.Minute))
	if !a.IsCompacting(full) {
		t.Error("fresh latch should read as compacting")
	}
}
