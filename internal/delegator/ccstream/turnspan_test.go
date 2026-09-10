package ccstream

import (
	"strings"
	"testing"
	"time"
)

// The turn's own duration and the span the priced delta covers are different
// measurements, and the whole point of #1880 is that a reader cannot tell an
// expensive turn from a cheap one that closed last unless BOTH are shown.

func TestSpanSuffix_UnmeasuredSpanPrintsNothing(t *testing.T) {
	// First snapshot for a model has no predecessor, so the span is unknown.
	// Zero must read as "not measured" — printing "0s" would assert the turn
	// was priced over no time at all, which is a stronger and false claim.
	bd := costBreakdown{turnDur: 90 * time.Second, pricedDur: 0}
	if got := bd.spanSuffix(); got != "" {
		t.Fatalf("unmeasured priced span should print nothing, got %q", got)
	}
}

func TestSpanSuffix_MatchedSpansPrintBothWithoutFlag(t *testing.T) {
	bd := costBreakdown{turnDur: 200 * time.Second, pricedDur: 210 * time.Second}
	got := bd.spanSuffix()
	if !strings.Contains(got, "turn=3m20s") || !strings.Contains(got, "priced_span=3m30s") {
		t.Fatalf("both durations must appear, got %q", got)
	}
	if strings.Contains(got, "SPAN") {
		t.Fatalf("comparable spans must not raise the flag, got %q", got)
	}
}

func TestSpanSuffix_OverlongSpanIsFlagged(t *testing.T) {
	// The measured 2026-09-10 case: a 3.5-minute turn priced over 34 minutes
	// because a background subagent outlived its parent.
	bd := costBreakdown{turnDur: 214 * time.Second, pricedDur: 2067 * time.Second}
	got := bd.spanSuffix()
	if !strings.Contains(got, "SPAN") || !strings.Contains(got, "outside this turn") {
		t.Fatalf("a span far exceeding the turn must be flagged, got %q", got)
	}
	if !strings.Contains(got, "9.7x") {
		t.Fatalf("the flag must quantify the ratio, got %q", got)
	}
}

func TestPricedSpanStart_FirstSnapshotHasNoPredecessor(t *testing.T) {
	b := &Backend{}
	if got := b.pricedSpanStart("opus", time.Now()); !got.IsZero() {
		t.Fatalf("first snapshot must report an unmeasured start, got %v", got)
	}
}

func TestPricedSpanStart_ReturnsPreviousSnapshotTime(t *testing.T) {
	b := &Backend{}
	first := time.Now().Add(-30 * time.Minute)
	b.pricedSpanStart("opus", first)
	got := b.pricedSpanStart("opus", time.Now())
	if !got.Equal(first) {
		t.Fatalf("second snapshot must report the first's time: want %v got %v", first, got)
	}
}

func TestPricedSpanStart_PerModelNotShared(t *testing.T) {
	// A turn routinely touches several models. One shared timestamp would
	// report one model's window as another's, which is the same class of
	// error as pricing one model's tokens at another's rate.
	b := &Backend{}
	opusFirst := time.Now().Add(-20 * time.Minute)
	b.pricedSpanStart("opus", opusFirst)
	if got := b.pricedSpanStart("sonnet", time.Now()); !got.IsZero() {
		t.Fatalf("a different model must not inherit opus's snapshot time, got %v", got)
	}
	if got := b.pricedSpanStart("opus", time.Now()); !got.Equal(opusFirst) {
		t.Fatalf("opus's own window must survive sonnet's snapshot: want %v got %v", opusFirst, got)
	}
}
