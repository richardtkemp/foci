package compaction

import (
	"math"
	"testing"
)

// TestEffectiveThreshold_Nonlinear locks the concave usable-context curve:
// anchor 0.8 up to the 200k pivot, tapering to ~0.48 of 1M and ~0.38 of 2M.
func TestEffectiveThreshold_Nonlinear(t *testing.T) {
	c := NewCompactor(nil, 0.8).SetNonlinear(true)

	// At/below the 200k pivot the anchor (0.8) applies flat.
	if got := c.EffectiveThreshold(100_000); got != 80_000 {
		t.Errorf("100k window: got %d, want 80000", got)
	}
	if got := c.EffectiveThreshold(200_000); got != 160_000 {
		t.Errorf("200k window: got %d, want 160000", got)
	}

	// Above the pivot the fraction tapers. Assert the fraction, not exact tokens.
	cases := []struct {
		window   int
		wantFrac float64
	}{
		{400_000, 0.641},
		{500_000, 0.597},
		{1_000_000, 0.478},
		{1_500_000, 0.420},
		{2_000_000, 0.383},
	}
	for _, tc := range cases {
		frac := float64(c.EffectiveThreshold(tc.window)) / float64(tc.window)
		if math.Abs(frac-tc.wantFrac) > 0.005 {
			t.Errorf("%d window: fraction %.4f, want ~%.3f", tc.window, frac, tc.wantFrac)
		}
	}

	// The fraction must strictly decrease as the window grows past the pivot,
	// while absolute usable tokens must keep rising.
	prevFrac, prevTokens := 1.0, 0
	for _, w := range []int{200_000, 400_000, 1_000_000, 2_000_000} {
		tok := c.EffectiveThreshold(w)
		frac := float64(tok) / float64(w)
		if frac > prevFrac {
			t.Errorf("fraction not decreasing at %d: %.4f > %.4f", w, frac, prevFrac)
		}
		if tok <= prevTokens {
			t.Errorf("usable tokens not increasing at %d: %d <= %d", w, tok, prevTokens)
		}
		prevFrac, prevTokens = frac, tok
	}
}

// TestEffectiveThreshold_ExplicitFlat proves an explicit compaction_threshold
// (nonlinear=false) stays a flat fraction of any window.
func TestEffectiveThreshold_ExplicitFlat(t *testing.T) {
	c := NewCompactor(nil, 0.5) // nonlinear defaults false
	for _, w := range []int{100_000, 1_000_000, 2_000_000} {
		if got, want := c.EffectiveThreshold(w), w/2; got != want {
			t.Errorf("flat 0.5 at %d: got %d, want %d", w, got, want)
		}
	}
}
