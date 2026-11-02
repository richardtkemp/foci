package memory

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
)

func TestIdentifierParts(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"deploy the server", nil},
		{"FociRepository.seedReliabilityFromDb",
			[]string{"FociRepository", "Foci", "Repository", "seedReliabilityFromDb", "seed", "Reliability", "From", "Db"}},
		{"set seed_reliability now", []string{"seed", "reliability"}},
		{"HTTPServer", []string{"HTTPServer", "HTTP", "Server"}},
		{"sha256Sum", []string{"sha256Sum", "sha256", "Sum"}},
		{"pkg:Name", []string{"pkg", "Name"}},
		{"(see foo.Bar, foo.Baz)", []string{"foo", "Bar", "Baz"}},
	}
	for _, c := range cases {
		if got := identifierParts(c.in); !slices.Equal(got, c.want) {
			t.Errorf("identifierParts(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestBleveTodoSearchIdentifierParts is #2815's repro: a ticket naming
// FociRepository.seedReliabilityFromDb was not found by a search for
// seedReliabilityFromDb, because the analyzer indexes the dotted name as one
// token. Every part of a code identifier must find the ticket, and the whole
// identifier must still find it.
func TestBleveTodoSearchIdentifierParts(t *testing.T) {
	idx, _ := testBleveIndex(t)
	now := float64(time.Now().Unix())
	idx.IndexTodo("a", 1, "Fix FociRepository.seedReliabilityFromDb on cold start", now)
	idx.IndexTodo("a", 2, "rename parse_status_filter in the todo tool", now)
	idx.IndexTodo("a", 3, "an unrelated ticket about lunch", now)

	for _, q := range []struct {
		query string
		want  int64
	}{
		{"seedReliabilityFromDb", 1},
		{"FociRepository", 1},
		{"FociRepository.seedReliabilityFromDb", 1},
		{"reliability", 1},
		{"Repository", 1},
		{"parse_status_filter", 2},
		{"status", 2},
		{"filter", 2},
	} {
		hits, err := idx.SearchTodos("a", q.query, "", 0)
		if err != nil {
			t.Fatalf("SearchTodos(%q): %v", q.query, err)
		}
		var ids []int64
		for _, h := range hits {
			ids = append(ids, h.TodoID)
		}
		if !slices.Equal(ids, []int64{q.want}) {
			t.Errorf("search %q = %v, want [%d]", q.query, ids, q.want)
		}
	}
}

// TestBleveTodoIdentPartsOnPreMappingIndex checks the claim in
// buildBleveMapping that an index created before ident_parts was mapped
// still finds identifier parts: the default mapping is dynamic, so the field
// is indexed with the default analyzer and included in _all. If this fails,
// openOrRebuildBleve must rebuild indexes that lack the field mapping.
func TestBleveTodoIdentPartsOnPreMappingIndex(t *testing.T) {
	idx, _ := testBleveIndex(t)
	old := buildBleveMapping().(*mapping.IndexMappingImpl)
	if _, ok := old.DefaultMapping.Properties[bleveIdentPartsField]; !ok {
		t.Fatalf("mapping has no %q property to remove", bleveIdentPartsField)
	}
	delete(old.DefaultMapping.Properties, bleveIdentPartsField)
	raw, err := bleve.New(filepath.Join(t.TempDir(), "old.bleve"), old)
	if err != nil {
		t.Fatalf("bleve.New: %v", err)
	}
	_ = idx.index.Close()
	idx.index = raw

	idx.IndexTodo("a", 1, "Fix FociRepository.seedReliabilityFromDb", float64(time.Now().Unix()))
	hits, err := idx.SearchTodos("a", "seedReliabilityFromDb", "", 0)
	if err != nil {
		t.Fatalf("SearchTodos: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("pre-mapping index: got %d hits, want 1", len(hits))
	}
}
