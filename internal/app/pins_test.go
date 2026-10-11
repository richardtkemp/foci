package app

import "testing"

func TestApplyPin_Idempotent(t *testing.T) {
	// Folding a pin state into a set must be idempotent and leave the input
	// slice unmutated; the set-level codec (sort/dedup) is covered by the
	// session package's own chatset tests.
	base := []string{"m1"}
	if got := applyPin(base, "m1", true); !equalIDs(got, []string{"m1"}) {
		t.Errorf("re-pinning = %v, want [m1]", got)
	}
	if got := applyPin(base, "m2", false); !equalIDs(got, []string{"m1"}) {
		t.Errorf("unpinning an absent id = %v, want [m1]", got)
	}
	if got := applyPin(base, "m1", false); len(got) != 0 {
		t.Errorf("unpinning the only id = %v, want empty", got)
	}
	if got := applyPin(base, "m0", true); !equalIDs(got, []string{"m0", "m1"}) {
		t.Errorf("pinning a second id = %v, want [m0 m1]", got)
	}
	if !equalIDs(base, []string{"m1"}) {
		t.Errorf("applyPin mutated its input: %v", base)
	}
}
