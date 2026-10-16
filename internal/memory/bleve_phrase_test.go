package memory

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"foci/internal/log"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/mapping"
	index "github.com/blevesearch/bleve_index_api"
)

// phraseTestIndex wraps the real bleve index for the fail-open and search-work
// tests: it counts Search calls, can fail the Nth one (1-based; 0 = never),
// and can override Mapping() to simulate a mapping whose analyzer lookup
// fails. Every method delegates to the wrapped index except Search and
// Mapping; embedding bleve.Index instead is not possible because the embedded
// field would be named Index, shadowing the interface's own Index method.
type phraseTestIndex struct {
	inner      bleve.Index
	calls      int
	failOnCall int
	mapping    mapping.IndexMapping // non-nil overrides Mapping()
}

func (p *phraseTestIndex) Search(req *bleve.SearchRequest) (*bleve.SearchResult, error) {
	p.calls++
	if p.failOnCall > 0 && p.calls == p.failOnCall {
		return nil, fmt.Errorf("injected failure on bleve search call %d", p.calls)
	}
	return p.inner.Search(req)
}

func (p *phraseTestIndex) Mapping() mapping.IndexMapping {
	if p.mapping != nil {
		return p.mapping
	}
	return p.inner.Mapping()
}

func (p *phraseTestIndex) Index(id string, data interface{}) error { return p.inner.Index(id, data) }
func (p *phraseTestIndex) Delete(id string) error                  { return p.inner.Delete(id) }
func (p *phraseTestIndex) NewBatch() *bleve.Batch                  { return p.inner.NewBatch() }
func (p *phraseTestIndex) Batch(b *bleve.Batch) error              { return p.inner.Batch(b) }
func (p *phraseTestIndex) Document(id string) (index.Document, error) {
	return p.inner.Document(id)
}
func (p *phraseTestIndex) DocCount() (uint64, error) { return p.inner.DocCount() }
func (p *phraseTestIndex) SearchInContext(ctx context.Context, req *bleve.SearchRequest) (*bleve.SearchResult, error) {
	return p.inner.SearchInContext(ctx, req)
}
func (p *phraseTestIndex) Fields() ([]string, error) { return p.inner.Fields() }
func (p *phraseTestIndex) FieldDict(field string) (index.FieldDict, error) {
	return p.inner.FieldDict(field)
}
func (p *phraseTestIndex) FieldDictRange(field string, startTerm, endTerm []byte) (index.FieldDict, error) {
	return p.inner.FieldDictRange(field, startTerm, endTerm)
}
func (p *phraseTestIndex) FieldDictPrefix(field string, termPrefix []byte) (index.FieldDict, error) {
	return p.inner.FieldDictPrefix(field, termPrefix)
}
func (p *phraseTestIndex) Close() error                     { return p.inner.Close() }
func (p *phraseTestIndex) Stats() *bleve.IndexStat          { return p.inner.Stats() }
func (p *phraseTestIndex) StatsMap() map[string]interface{} { return p.inner.StatsMap() }
func (p *phraseTestIndex) GetInternal(key []byte) ([]byte, error) {
	return p.inner.GetInternal(key)
}
func (p *phraseTestIndex) SetInternal(key, val []byte) error {
	return p.inner.SetInternal(key, val)
}
func (p *phraseTestIndex) DeleteInternal(key []byte) error { return p.inner.DeleteInternal(key) }
func (p *phraseTestIndex) Name() string                    { return p.inner.Name() }
func (p *phraseTestIndex) SetName(name string)             { p.inner.SetName(name) }
func (p *phraseTestIndex) Advanced() (index.Index, error)  { return p.inner.Advanced() }

// noAnalyzerMapping answers analyzer lookups the way a mapping with an
// unregistered analyzer name would: AnalyzerNamed returns nil.
type noAnalyzerMapping struct{ mapping.IndexMapping }

func (noAnalyzerMapping) AnalyzerNameForPath(string) string      { return "en" }
func (noAnalyzerMapping) AnalyzerNamed(string) analysis.Analyzer { return nil }

// phraseTestIndexWith builds a bleve index with temporal decay off (so recency
// cannot interfere with rank comparisons) and indexes the given files
// (name -> content) via Reindex.
func phraseTestIndexWith(t *testing.T, files map[string]string) (*BleveIndex, string) {
	t.Helper()
	idx, memDir := testBleveIndex(t)
	idx.SetTemporalDecay(false, 0, 0, nil)
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(memDir, name), []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := idx.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	return idx, memDir
}

// ranksByPath maps result path -> Rank.
func ranksByPath(results []Result) map[string]float64 {
	ranks := make(map[string]float64, len(results))
	for _, r := range results {
		ranks[r.Path] = r.Rank
	}
	return ranks
}

// unboostedRanks returns each hit's rank for the same query WITHOUT the phrase
// boost or the recency re-rank: "newest" skips both (decay is off in these
// tests) while bleve still scores every hit, so the ranks are the weighted
// pre-boost ranks. It baselines the relevance-sort ratio assertions; that time
// sorts never boost is pinned by TestBleveSearchPhraseBoostRelevanceSortOnly,
// which compares hits with each other rather than against this helper — a
// baseline routed through the sort under test would hide a boost wrongly
// applied to it.
func unboostedRanks(t *testing.T, idx *BleveIndex, q string, opts *SearchOptions) map[string]float64 {
	t.Helper()
	results, err := idx.Search(q, "newest", opts)
	if err != nil {
		t.Fatalf("baseline Search(%q, newest): %v", q, err)
	}
	ranks := ranksByPath(results)
	for path, rank := range ranks {
		if rank <= 0 {
			t.Fatalf("baseline rank for %s = %g, want > 0 (premise: newest-sorted hits still carry scores)", path, rank)
		}
	}
	return ranks
}

// assertRankRatio checks got == want x base within a tiny relative tolerance.
func assertRankRatio(t *testing.T, got, base, want float64, msg string) {
	t.Helper()
	if math.Abs(got-want*base) > 1e-9*math.Max(1, math.Abs(want*base)) {
		t.Errorf("%s: got rank %.17g, want %.17g x %.17g = %.17g", msg, got, want, base, want*base)
	}
}

func TestBleveSearchPhraseBoostAdjacentPair(t *testing.T) {
	// Proves the boost: for "todo chart", the file carrying the words next to
	// each other ranks exactly 1.1x the file with the same words (and same
	// length) far apart, comes first, and the apart file keeps its baseline
	// rank exactly.
	idx, _ := phraseTestIndexWith(t, map[string]string{
		"a.md": "todo chart alpha beta",
		"b.md": "todo alpha beta chart",
	})
	base := unboostedRanks(t, idx, "todo chart", nil)
	if base["a.md"] != base["b.md"] {
		t.Fatalf("premise: equal baseline ranks (same words, same length, b=0), got a=%.17g b=%.17g", base["a.md"], base["b.md"])
	}

	results, err := idx.Search("todo chart", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Path != "a.md" {
		t.Errorf("first result = %s, want a.md (words adjacent)", results[0].Path)
	}
	ranks := ranksByPath(results)
	assertRankRatio(t, ranks["a.md"], base["a.md"], 1.1, "a.md (todo chart adjacent)")
	assertRankRatio(t, ranks["b.md"], base["b.md"], 1.0, "b.md (words apart)")
}

func TestAdjacentPairs(t *testing.T) {
	// Pins the pair construction the boost counts: neighbouring words joined
	// by one space, in query order, and nothing for fewer than two words.
	if got := adjacentPairs(nil); got != nil {
		t.Errorf("adjacentPairs(nil) = %v, want nil", got)
	}
	if got := adjacentPairs([]string{"todo"}); got != nil {
		t.Errorf("adjacentPairs(one word) = %v, want nil", got)
	}
	got := adjacentPairs([]string{"a", "b", "c"})
	want := []string{"a b", "b c"}
	if !slices.Equal(got, want) {
		t.Errorf("adjacentPairs = %v, want %v", got, want)
	}
}

func TestBleveSearchPhraseBoostPartialPairs(t *testing.T) {
	// Proves the boost is proportional: for the three-word query "todo chart
	// gamma", a file with one of the two adjacent pairs ("todo chart" but not
	// "chart gamma") gets exactly half the full boost: 1.05x the file with the
	// same words all apart.
	idx, _ := phraseTestIndexWith(t, map[string]string{
		"a.md": "todo chart alpha gamma",
		"b.md": "todo gamma alpha chart",
	})
	base := unboostedRanks(t, idx, "todo chart gamma", nil)
	if base["a.md"] != base["b.md"] {
		t.Fatalf("premise: equal baseline ranks, got a=%.17g b=%.17g", base["a.md"], base["b.md"])
	}

	results, err := idx.Search("todo chart gamma", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ranks := ranksByPath(results)
	assertRankRatio(t, ranks["a.md"], base["a.md"], 1.05, "a.md (1 of 2 pairs)")
	assertRankRatio(t, ranks["b.md"], base["b.md"], 1.0, "b.md (0 of 2 pairs)")
}

func TestBleveSearchPhraseBoostRespectsWordOrder(t *testing.T) {
	// Proves word order is respected: for the query "chart todo", the file
	// containing "todo chart" gets nothing (ranks equal to the apart file),
	// while the file actually containing "chart todo" is boosted 1.1x — the
	// positive control that fails without the boost.
	idx, _ := phraseTestIndexWith(t, map[string]string{
		"a.md": "todo chart alpha beta",
		"b.md": "todo alpha beta chart",
		"c.md": "chart todo alpha beta",
	})
	base := unboostedRanks(t, idx, "chart todo", nil)

	results, err := idx.Search("chart todo", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ranks := ranksByPath(results)
	if ranks["a.md"] != ranks["b.md"] {
		t.Errorf("'todo chart' in content must not boost the query 'chart todo': a=%.17g b=%.17g, want equal", ranks["a.md"], ranks["b.md"])
	}
	assertRankRatio(t, ranks["a.md"], base["a.md"], 1.0, "a.md (reversed order)")
	assertRankRatio(t, ranks["c.md"], base["c.md"], 1.1, "c.md (chart todo adjacent — positive control)")
}

func TestBleveSearchPhraseBoostIgnoresStopWordPair(t *testing.T) {
	// Proves a pair the analyzer reduces below two terms counts neither as
	// matched nor in the total: in "the chart todo", the pair "the chart"
	// analyses to the single term "chart" and is dropped, so the one remaining
	// pair ("chart todo") gives a full 1.1x — not the 1.05x a counting
	// implementation would give — and the other files keep their baseline.
	idx, _ := phraseTestIndexWith(t, map[string]string{
		"a.md": "todo chart alpha beta",
		"b.md": "todo alpha beta chart",
		"c.md": "chart todo alpha beta",
	})
	base := unboostedRanks(t, idx, "the chart todo", nil)

	results, err := idx.Search("the chart todo", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ranks := ranksByPath(results)
	assertRankRatio(t, ranks["c.md"], base["c.md"], 1.1, "c.md (only counted pair matched -> full boost)")
	assertRankRatio(t, ranks["a.md"], base["a.md"], 1.0, "a.md")
	assertRankRatio(t, ranks["b.md"], base["b.md"], 1.0, "b.md")
}

func TestBleveSearchPhraseBoostNoCountedPair(t *testing.T) {
	// Characterises requirement 2: when no pair counts ("the chart" analyses to
	// one term), nothing changes — every rank equals its baseline.
	idx, _ := phraseTestIndexWith(t, map[string]string{
		"a.md": "todo chart alpha beta",
		"b.md": "todo alpha beta chart",
		"c.md": "chart todo alpha beta",
	})
	base := unboostedRanks(t, idx, "the chart", nil)

	results, err := idx.Search("the chart", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ranks := ranksByPath(results)
	for path, want := range base {
		assertRankRatio(t, ranks[path], want, 1.0, path)
	}
}

func TestBleveSearchPhraseBoostSingleWordUnchanged(t *testing.T) {
	// Characterises requirement 4: one-word queries — including a hyphenated
	// token, which is one word — never boost, even when a document carries the
	// word(s) adjacent in its content.
	idx, _ := phraseTestIndexWith(t, map[string]string{
		"a.md": "todo chart alpha beta",
		"b.md": "todo alpha beta chart",
	})
	for _, q := range []string{"todo", "todo-chart"} {
		base := unboostedRanks(t, idx, q, nil)
		results, err := idx.Search(q, "", nil)
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		ranks := ranksByPath(results)
		if len(ranks) != len(base) {
			t.Fatalf("Search(%q): got %d results, want same %d as baseline", q, len(ranks), len(base))
		}
		for path, want := range base {
			assertRankRatio(t, ranks[path], want, 1.0, q+" "+path)
		}
	}
}

func TestBleveSearchPhraseBoostRelevanceSortOnly(t *testing.T) {
	// Proves newest/oldest sorts never boost, without routing the baseline
	// through the sort under test: a.md and b.md carry the same words (same
	// source, decay off), so under a time sort their ranks are equal — a
	// boost wrongly applied there would lift the adjacent file (a.md) 1.1x
	// above the apart one (b.md). Relevance sort in the same test boosts the
	// adjacent file 1.1x (the positive control that fails without the boost).
	idx, _ := phraseTestIndexWith(t, map[string]string{
		"a.md": "todo chart alpha beta",
		"b.md": "todo alpha beta chart",
	})

	for _, sort := range []string{"newest", "oldest"} {
		results, err := idx.Search("todo chart", sort, nil)
		if err != nil {
			t.Fatalf("Search(%q): %v", sort, err)
		}
		ranks := ranksByPath(results)
		if len(ranks) != 2 {
			t.Fatalf("Search(%q): got %d results, want 2", sort, len(ranks))
		}
		assertRankRatio(t, ranks["a.md"], ranks["b.md"], 1.0, sort+": a.md (adjacent) vs b.md")
	}

	results, err := idx.Search("todo chart", "relevance", nil)
	if err != nil {
		t.Fatalf("Search(relevance): %v", err)
	}
	ranks := ranksByPath(results)
	assertRankRatio(t, ranks["a.md"], ranks["b.md"], 1.1, "relevance a.md vs b.md — positive control")
}

func TestBleveSearchPhraseBoostConversation(t *testing.T) {
	// Proves the boost applies to conversation hits too: of two same-length
	// messages with the same words, the one carrying "todo chart" adjacently
	// ranks exactly 1.1x the other.
	idx, _ := phraseTestIndexWith(t, nil)
	idx.IndexConversation("todo chart alpha beta", "sess", 1)
	idx.IndexConversation("todo alpha beta chart", "sess", 2)

	results, err := idx.Search("todo chart", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ranks := map[int64]float64{}
	for _, r := range results {
		if r.Source != "conversation" {
			t.Fatalf("result %s is source %q, want conversation", r.Path, r.Source)
		}
		ranks[r.RowID] = r.Rank
	}
	if len(ranks) != 2 {
		t.Fatalf("got %d conversation results, want 2", len(ranks))
	}
	assertRankRatio(t, ranks[1], ranks[2], 1.1, "conversation row 1 (adjacent) vs row 2")
	if results[0].RowID != 1 {
		t.Errorf("first result row = %d, want 1 (adjacent)", results[0].RowID)
	}
}

func TestBleveSearchPhraseBoostTodoDocs(t *testing.T) {
	// Proves the boost applies to todo documents too (requirement 1: every
	// source): of two same-length todo texts with the same words, the one
	// carrying "todo chart" adjacently ranks exactly 1.1x the other in
	// memory_search (SearchTodos itself is a separate, unboosted path).
	idx, _ := phraseTestIndexWith(t, nil)
	idx.IndexTodo("agent", 1, "todo chart alpha beta", 1)
	idx.IndexTodo("agent", 2, "todo alpha beta chart", 1)

	results, err := idx.Search("todo chart", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ranks := map[int64]float64{}
	for _, r := range results {
		if r.Source != "todo" {
			t.Fatalf("result %s is source %q, want todo", r.Path, r.Source)
		}
		if id, err := strconv.ParseInt(strings.TrimPrefix(r.Path, "#"), 10, 64); err == nil {
			ranks[id] = r.Rank
		}
	}
	if len(ranks) != 2 {
		t.Fatalf("got %d todo results, want 2", len(ranks))
	}
	assertRankRatio(t, ranks[1], ranks[2], 1.1, "todo 1 (adjacent) vs todo 2")
}

func TestBleveSearchPhraseBoostCutKeepsUnboostedSet(t *testing.T) {
	// Proves requirement 3 where it can actually break: with the return
	// limit binding, the boost reorders the returned results but never
	// changes which ones they are. The corpus is tuned (per-word term
	// counts; BM25 at b=0 ignores length) so the unboosted order is
	// a.md > p.md > c.md > r.md: p.md and r.md carry "todo chart"
	// adjacently, a.md and c.md do not, and r.md's full boost would lift it
	// past c.md — into the top 3 only if the boost acted before the cut.
	// The reversed query "chart todo" is the unboosted baseline: same
	// terms, same per-doc scores, but its pair matches nothing in the
	// corpus, so it never boosts.
	idx, _ := phraseTestIndexWith(t, map[string]string{
		"a.md": "todo alpha todo beta todo gamma todo delta chart alpha chart beta chart gamma chart delta chart epsilon",
		"p.md": "todo chart alpha chart beta chart gamma chart delta chart epsilon todo beta todo gamma",
		"c.md": "todo alpha todo beta todo gamma chart alpha chart beta chart gamma chart delta",
		"r.md": "todo chart alpha chart beta chart gamma todo beta todo gamma",
	})
	idx.SetSearchLimit(3)

	base, err := idx.Search("chart todo", "", nil)
	if err != nil {
		t.Fatalf("baseline Search(chart todo): %v", err)
	}
	if got, want := paths(base), []string{"a.md", "p.md", "c.md"}; !slices.Equal(got, want) {
		t.Fatalf("premise: unboosted top 3 = %v, want %v (r.md 4th, outside the cut)", got, want)
	}
	baseRank := ranksByPath(base)

	rel, err := idx.Search("todo chart", "", nil)
	if err != nil {
		t.Fatalf("Search(todo chart): %v", err)
	}
	got := paths(rel)
	if len(got) != 3 {
		t.Fatalf("got %d results, want the binding limit of 3", len(got))
	}
	if slices.Contains(got, "r.md") {
		t.Errorf("r.md (outside the unboosted top 3) entered the results: %v — the boost changed the result set", got)
	}
	for _, want := range []string{"a.md", "p.md", "c.md"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s (in the unboosted top 3) missing from %v — the boost changed the result set", want, got)
		}
	}

	// The boost still reorders within the unchanged set: p.md (1 pair of 1,
	// +10%) lifts past a.md, whose margin over it unboosted is under 10%.
	if got[0] != "p.md" {
		t.Errorf("first result = %s, want p.md (boosted past a.md)", got[0])
	}
	ranks := ranksByPath(rel)
	assertRankRatio(t, ranks["p.md"], baseRank["p.md"], 1.1, "p.md (phrase inside the set)")
	assertRankRatio(t, ranks["a.md"], baseRank["a.md"], 1.0, "a.md (no phrase)")
	assertRankRatio(t, ranks["c.md"], baseRank["c.md"], 1.0, "c.md (no phrase)")
}

func TestBleveSearchPhraseBoostWithFilters(t *testing.T) {
	// Proves date and exclude-path filters keep working alongside the boost:
	// the boosted ranks inside the filtered set are baseline x 1.1 / x 1.0 as
	// usual, and the set itself is exactly what the filters allow.
	idx, memDir := phraseTestIndexWith(t, map[string]string{
		"a.md": "todo chart alpha beta",
		"b.md": "todo alpha beta chart",
	})
	// Distinct mtimes so the date filter can separate the files.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(memDir, "b.md"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := idx.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	dateFrom := time.Now().Add(-24 * time.Hour)
	filtered := unboostedRanks(t, idx, "todo chart", &SearchOptions{DateFrom: &dateFrom})
	if len(filtered) != 1 || filtered["a.md"] == 0 {
		t.Fatalf("premise: date filter keeps only a.md, got %v", filtered)
	}
	results, err := idx.Search("todo chart", "", &SearchOptions{DateFrom: &dateFrom})
	if err != nil {
		t.Fatalf("Search with date filter: %v", err)
	}
	ranks := ranksByPath(results)
	if len(ranks) != 1 || ranks["a.md"] == 0 {
		t.Fatalf("date filter: got %v, want only a.md", ranks)
	}
	assertRankRatio(t, ranks["a.md"], filtered["a.md"], 1.1, "a.md under date filter")

	base := unboostedRanks(t, idx, "todo chart", &SearchOptions{ExcludePath: "a.md"})
	results, err = idx.Search("todo chart", "", &SearchOptions{ExcludePath: "a.md"})
	if err != nil {
		t.Fatalf("Search with exclude: %v", err)
	}
	ranks = ranksByPath(results)
	if _, ok := ranks["a.md"]; ok {
		t.Error("excluded a.md must not be returned")
	}
	for path, want := range base {
		assertRankRatio(t, ranks[path], want, 1.0, path+" under exclude filter")
	}
}

func TestBleveSearchPhraseBoostNeverFilters(t *testing.T) {
	// Characterises requirement 3: the boost never adds or removes results —
	// proved here where the return limit does not bind, plus the Finding-1
	// window guarantee. The binding-limit case (a boost applied at the cut
	// could swap the last places) is pinned by
	// TestBleveSearchPhraseBoostCutKeepsUnboostedSet.
	// (a) With the return limit not binding (3 docs, default limit 20), the
	// result set of the boosting relevance query equals the set of the same
	// query under "newest", which never boosts — any difference would be a
	// result added or removed by the boost; (b) with a small search limit, a
	// long memory file with the words far apart stays in the results and ranks
	// first even when 10 short conversation messages contain the phrase — the
	// exact #2234/#2327 Finding-1 shape that a phrase clause inside the bleve
	// query would break.
	idx, _ := phraseTestIndexWith(t, map[string]string{
		"a.md": "todo chart alpha beta",
		"b.md": "todo alpha beta chart",
		"c.md": "chart todo alpha beta",
	})
	rel, err := idx.Search("todo chart", "", nil)
	if err != nil {
		t.Fatalf("Search(todo chart, relevance): %v", err)
	}
	timeOrder, err := idx.Search("todo chart", "newest", nil)
	if err != nil {
		t.Fatalf("Search(todo chart, newest): %v", err)
	}
	relPaths := map[string]bool{}
	for _, r := range rel {
		relPaths[r.Path] = true
	}
	timePaths := map[string]bool{}
	for _, r := range timeOrder {
		timePaths[r.Path] = true
	}
	if len(relPaths) != 3 || len(timePaths) != 3 {
		t.Fatalf("premise: both sorts return all 3 files, got relevance %v, newest %v", relPaths, timePaths)
	}
	for path := range timePaths {
		if !relPaths[path] {
			t.Errorf("result %s appears without the boost (newest) but not with it (relevance) — the boost changed the result set", path)
		}
	}
	for path := range relPaths {
		if !timePaths[path] {
			t.Errorf("result %s appears with the boost (relevance) but not without it (newest) — the boost changed the result set", path)
		}
	}

	// (b) Window displacement: memFile has "todo" twice and "chart" once, all
	// far apart, so its raw BM25 strictly outranks each one-occurrence
	// conversation message but by well under the 1.2x a phrase clause inside
	// the bleve query adds. The 80 filler files mention only "todo" (40 of
	// them) or neither word, keeping "chart" rare so the margins hold.
	idx2, memDir := phraseTestIndexWith(t, map[string]string{
		"long.md": "todo gamma delta epsilon zeta " + strings.Repeat("filler words in between here ", 60) +
			"chart alpha bravo charlie " + strings.Repeat("more filler text spread out wide ", 60) + "todo",
	})
	for i := 0; i < 80; i++ {
		content := fmt.Sprintf("plain filler note %d about widgets", i)
		if i < 40 {
			content = fmt.Sprintf("todo filler note %d about widgets", i)
		}
		if err := os.WriteFile(filepath.Join(memDir, fmt.Sprintf("fill%02d.md", i)), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := idx2.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	for i := 0; i < 10; i++ {
		idx2.IndexConversation("todo chart alpha beta", "s", int64(i+1))
	}
	idx2.SetSearchLimit(3)

	results, err := idx2.Search("todo chart", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 || results[0].Source != "memory" || results[0].Path != "long.md" {
		paths := make([]string, 0, len(results))
		for _, r := range results {
			paths = append(paths, r.Source+":"+r.Path)
		}
		t.Fatalf("top result = %v, want memory:long.md — conversation phrase hits displaced the memory file", paths)
	}
}

func TestBleveSearchPhraseBoostFailsOpen(t *testing.T) {
	// Proves requirement 5: when the phrase matching fails — a phrase search
	// error, a later pair's error (no partial boost), or a mapping with no
	// analyzer for content — Search still succeeds, logs a WARN via memoryLog,
	// and returns exactly the unboosted results. A one-word query never
	// starts the phrase work at all, so even a broken mapping stays silent.
	files := map[string]string{
		"a.md": "todo chart alpha gamma",
		"b.md": "todo gamma alpha chart",
	}
	cases := []struct {
		name       string
		query      string
		failOnCall int // 1-based bleve search call to fail; 0 = never
		mapping    mapping.IndexMapping
		wantWarn   bool
	}{
		{"phrase search fails", "todo chart", 2, nil, true},
		{"second pair fails, no partial boost", "todo chart gamma", 3, nil, true},
		{"no analyzer for content", "todo chart", 0, noAnalyzerMapping{}, true},
		{"one-word query does no phrase work", "todo", 0, noAnalyzerMapping{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx, _ := phraseTestIndexWith(t, files)
			base := unboostedRanks(t, idx, tc.query, nil)

			wrap := &phraseTestIndex{inner: idx.index, failOnCall: tc.failOnCall, mapping: tc.mapping}
			idx.index = wrap
			t.Cleanup(func() { idx.index = wrap.inner })

			var warned bool
			remove := log.AddTap(log.WARN, func(level log.Level, component, msg string) {
				if component == "memory" && strings.Contains(msg, "phrase boost") && strings.Contains(msg, tc.query) {
					warned = true
				}
			})
			defer remove()

			results, err := idx.Search(tc.query, "", nil)
			if err != nil {
				t.Fatalf("Search must not fail when the phrase matching fails: %v", err)
			}
			if warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v", warned, tc.wantWarn)
			}
			ranks := ranksByPath(results)
			if len(ranks) != len(base) {
				t.Fatalf("got %d results, want the same %d as unboosted", len(ranks), len(base))
			}
			for path, want := range base {
				assertRankRatio(t, ranks[path], want, 1.0, path)
			}
		})
	}
}

func TestBleveSearchPhraseBoostSearchWork(t *testing.T) {
	// Proves where the boost costs extra search work: one-word queries
	// (hyphenated included), a no-counted-pair query, and newest/oldest sorts
	// do exactly ONE bleve search; a boosting relevance query does more.
	idx, _ := phraseTestIndexWith(t, map[string]string{
		"a.md": "todo chart alpha beta",
		"b.md": "todo alpha beta chart",
	})
	oneSearch := func(q, sort string) {
		t.Helper()
		wrap := &phraseTestIndex{inner: idx.index}
		idx.index = wrap
		defer func() { idx.index = wrap.inner }()
		if _, err := idx.Search(q, sort, nil); err != nil {
			t.Fatalf("Search(%q, %q): %v", q, sort, err)
		}
		if wrap.calls != 1 {
			t.Errorf("Search(%q, %q) did %d bleve searches, want 1", q, sort, wrap.calls)
		}
	}
	oneSearch("todo", "")
	oneSearch("todo-chart", "")
	oneSearch("the chart", "")
	oneSearch("todo chart", "newest")
	oneSearch("todo chart", "oldest")

	wrap := &phraseTestIndex{inner: idx.index}
	idx.index = wrap
	if _, err := idx.Search("todo chart", "", nil); err != nil {
		t.Fatalf("Search(todo chart, relevance): %v", err)
	}
	idx.index = wrap.inner
	if wrap.calls < 2 {
		t.Errorf("boosting relevance search did %d bleve searches, want > 1", wrap.calls)
	}
}
