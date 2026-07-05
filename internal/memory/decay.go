package memory

import (
	"math"
	"path/filepath"
	"sort"
	"time"
)

// Temporal decay for memory search (#352). This is a recency *boost*, not an age
// *penalty*: recent results are lifted, old ones are never pushed down. The boost
// multiplier lives in [1, 1+boost] — bounded — so recency re-orders comparable
// matches and promotes a recent-moderate hit over an old-strong one, but can
// never float a weak-recent hit above a strong-old one. Evergreen files
// (MEMORY.md, research-*, …) are exempt: timeless, so they get no boost.

// memorySearchReturn is the number of results Search returns for a relevance
// query; it over-fetches 2x this so the recency re-rank has room to promote.
const memorySearchReturn = 20

// recencyBoostFactor returns the boost multiplier for a result of the given age:
// 1+boost at age 0, halving the *bonus* every halfLifeDays, converging to 1.0
// (no penalty) for old items. A non-positive half-life/boost or negative age
// yields 1.0 (no-op).
func recencyBoostFactor(ageDays, halfLifeDays, boost float64) float64 {
	if halfLifeDays <= 0 || boost <= 0 || ageDays <= 0 {
		if ageDays <= 0 && halfLifeDays > 0 && boost > 0 {
			return 1.0 + boost // brand-new (or future-dated) → full boost
		}
		return 1.0
	}
	return 1.0 + boost*math.Exp(-math.Ln2*ageDays/halfLifeDays)
}

// isEvergreen reports whether path matches any evergreen glob (matched against
// both the basename and the full path, so "MEMORY.md" and "research-*" work
// regardless of directory).
func isEvergreen(path string, patterns []string) bool {
	base := filepath.Base(path)
	for _, p := range patterns {
		if ok, _ := filepath.Match(p, base); ok {
			return true
		}
		if ok, _ := filepath.Match(p, path); ok {
			return true
		}
	}
	return false
}

// rerankByRecency applies the recency boost to a relevance-ranked result set and
// returns the top `limit` by boosted rank. Results with a zero timestamp or an
// evergreen path are left unboosted. `now` is injected for testability. A
// non-positive limit means "no truncation".
//
// The boost multiplies Rank by a factor >= 1 in BOTH backends — this works for
// each's sign convention: bleve rank is positive-higher-is-better (multiplying
// grows a recent item's score), FTS5 rank is negative-more-negative-is-better
// (multiplying makes a recent item more negative). higherIsBetter selects the
// sort direction accordingly (bleve: true/descending; FTS5: false/ascending).
func rerankByRecency(results []Result, now time.Time, halfLifeDays, boost float64, evergreen []string, limit int, higherIsBetter bool) []Result {
	for i := range results {
		r := &results[i]
		if r.Time.IsZero() || isEvergreen(r.Path, evergreen) {
			continue
		}
		ageDays := now.Sub(r.Time).Hours() / 24
		r.Rank *= recencyBoostFactor(ageDays, halfLifeDays, boost)
	}
	sort.SliceStable(results, func(i, j int) bool {
		if higherIsBetter {
			return results[i].Rank > results[j].Rank
		}
		return results[i].Rank < results[j].Rank
	})
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results
}
