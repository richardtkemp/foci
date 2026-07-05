package memory

import (
	"math"
	"testing"
	"time"
)

func TestRecencyBoostFactor(t *testing.T) {
	const hl, boost = 10.0, 1.0
	cases := []struct {
		name    string
		age, hl, boost, want float64
	}{
		{"today = full boost", 0, hl, boost, 2.0},
		{"one half-life = half bonus", 10, hl, boost, 1.5},
		{"two half-lives", 20, hl, boost, 1.25},
		{"future-dated = full boost", -5, hl, boost, 2.0},
		{"disabled via half-life 0", 30, 0, boost, 1.0},
		{"disabled via boost 0", 30, hl, 0, 1.0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := recencyBoostFactor(c.age, c.hl, c.boost)
			if math.Abs(got-c.want) > 1e-9 {
				t.Errorf("recencyBoostFactor(%v,%v,%v) = %v, want %v", c.age, c.hl, c.boost, got, c.want)
			}
		})
	}
}

func TestRerankByRecency(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	results := []Result{
		{Path: "a.md", Rank: 2.0, Time: now.Add(-30 * day)}, // strong but old
		{Path: "b.md", Rank: 1.2, Time: now},                // moderate but fresh
		{Path: "c.md", Rank: 0.3, Time: now},                // weak, fresh
		{Path: "MEMORY.md", Rank: 1.0, Time: now},           // evergreen, fresh
	}
	got := rerankByRecency(results, now, 10, 1.0, []string{"MEMORY.md", "research-*"}, 0, true)

	// Boosted: a=2.0*1.125=2.25, b=1.2*2=2.4, c=0.3*2=0.6, MEMORY.md exempt=1.0.
	// Order: b (2.4) > a (2.25) > MEMORY.md (1.0) > c (0.6).
	wantOrder := []string{"b.md", "a.md", "MEMORY.md", "c.md"}
	if len(got) != len(wantOrder) {
		t.Fatalf("got %d results, want %d", len(got), len(wantOrder))
	}
	for i, w := range wantOrder {
		if got[i].Path != w {
			t.Errorf("position %d = %q, want %q (full order: %v)", i, got[i].Path, w, paths(got))
		}
	}
	// Moderate-but-fresh beat strong-but-old — the whole point.
	if got[0].Path != "b.md" {
		t.Errorf("expected fresh-moderate 'b.md' to rank first, got %q", got[0].Path)
	}
	// Evergreen MEMORY.md must be unboosted (else 1.0*2=2.0 would beat 'a').
	var mem *Result
	for i := range got {
		if got[i].Path == "MEMORY.md" {
			mem = &got[i]
		}
	}
	if mem == nil || math.Abs(mem.Rank-1.0) > 1e-9 {
		t.Errorf("MEMORY.md should be unboosted (rank 1.0), got %+v", mem)
	}
}

func TestRerankByRecency_Truncates(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	results := []Result{
		{Path: "a.md", Rank: 3.0, Time: now},
		{Path: "b.md", Rank: 2.0, Time: now},
		{Path: "c.md", Rank: 1.0, Time: now},
	}
	got := rerankByRecency(results, now, 10, 1.0, nil, 2, true)
	if len(got) != 2 {
		t.Fatalf("limit=2 returned %d results", len(got))
	}
}

func TestRerankByRecency_NegativeRankFTS5(t *testing.T) {
	// FTS5 rank is negative (more-negative = better). The boost multiplies toward
	// more-negative for recent items; higherIsBetter=false sorts ascending so the
	// most-negative (best) is first. Same rank-flip must hold as for bleve.
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	results := []Result{
		{Path: "a.md", Rank: -5.0, Time: now.Add(-30 * day)}, // strong (-5) but old
		{Path: "b.md", Rank: -3.0, Time: now},                // moderate (-3) but fresh
		{Path: "c.md", Rank: -1.0, Time: now},                // weak (-1), fresh
	}
	// a: -5*1.125 = -5.625; b: -3*2 = -6.0; c: -1*2 = -2.0. Ascending: b,a,c.
	got := rerankByRecency(results, now, 10, 1.0, nil, 0, false)
	want := []string{"b.md", "a.md", "c.md"}
	for i, w := range want {
		if got[i].Path != w {
			t.Errorf("position %d = %q, want %q (order %v)", i, got[i].Path, w, paths(got))
		}
	}
}

func paths(rs []Result) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Path
	}
	return out
}
