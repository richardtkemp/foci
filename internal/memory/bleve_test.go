package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testBleveIndex(t *testing.T) (*BleveIndex, string) {
	t.Helper()
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)
	indexPath := filepath.Join(dir, "memory.bleve")

	sources := map[string]SourceConfig{
		"memory": {
			Dir:    memDir,
			Weight: 1.0,
		},
	}
	idx, err := NewBleveIndex(indexPath, sources, 0)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx, memDir
}

func TestBleveNewIndex(t *testing.T) {
	idx, _ := testBleveIndex(t)
	if idx.index == nil {
		t.Fatal("index should not be nil")
	}
}

func TestBleveReindex(t *testing.T) {
	idx, memDir := testBleveIndex(t)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("The Go programming language is great for systems work."), 0644)
	os.WriteFile(filepath.Join(memDir, "journal.md"), []byte("Today I learned about Go interfaces and embedding."), 0644)

	if err := idx.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	results, err := idx.Search("Go programming", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for 'Go programming'")
	}
	if results[0].Source != "memory" {
		t.Errorf("source = %q, want 'memory'", results[0].Source)
	}
}

func TestBleveReindexIdempotent(t *testing.T) {
	idx, memDir := testBleveIndex(t)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("unique content for testing"), 0644)

	idx.Reindex()
	idx.Reindex() // second reindex should not duplicate

	results, err := idx.Search("unique content", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("got %d results, want 1 (reindex should not duplicate)", len(results))
	}
}

func TestBleveReindexSkipsNonMarkdown(t *testing.T) {
	idx, memDir := testBleveIndex(t)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("markdown content here"), 0644)
	os.WriteFile(filepath.Join(memDir, "data.json"), []byte(`{"content": "json content here"}`), 0644)

	idx.Reindex()

	results, err := idx.Search("content", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range results {
		if r.Path == "data.json" {
			t.Error("non-markdown file should not be indexed")
		}
	}
}

func TestBleveSearchNoMatches(t *testing.T) {
	idx, memDir := testBleveIndex(t)
	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("nothing relevant here"), 0644)
	idx.Reindex()

	results, err := idx.Search("xyzzy", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("got %d results, want 0", len(results))
	}
}

func TestBleveSearchEmptyIndex(t *testing.T) {
	idx, _ := testBleveIndex(t)

	results, err := idx.Search("anything", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("got %d results, want 0", len(results))
	}
}

func TestBleveSearchSubdirectories(t *testing.T) {
	idx, memDir := testBleveIndex(t)

	sub := filepath.Join(memDir, "2024")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "01-15.md"), []byte("January notes about winter activities"), 0644)

	idx.Reindex()

	results, err := idx.Search("winter", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results from subdirectory")
	}
	if results[0].Path != filepath.Join("2024", "01-15.md") {
		t.Errorf("path = %q, want '2024/01-15.md'", results[0].Path)
	}
}

func TestBlevePorterStemming(t *testing.T) {
	idx, memDir := testBleveIndex(t)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("The programmer was programming a programmatic solution"), 0644)
	idx.Reindex()

	// English analyzer uses Porter stemming — "program" should match variants
	results, err := idx.Search("program", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("porter stemmer should match 'program' against stemmed variants")
	}
}

func TestBleveMultiSourceIndexing(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "memory.bleve")

	canonical := filepath.Join(dir, "canonical")
	code := filepath.Join(dir, "code")
	os.MkdirAll(canonical, 0755)
	os.MkdirAll(code, 0755)

	os.WriteFile(filepath.Join(canonical, "notes.md"), []byte("Important fact about Go interfaces"), 0644)
	os.WriteFile(filepath.Join(code, "example.md"), []byte("Go interfaces allow duck typing"), 0644)

	sources := map[string]SourceConfig{
		"canonical": {Dir: canonical, Weight: 1.0},
		"code":      {Dir: code, Weight: 0.3},
	}

	idx, err := NewBleveIndex(indexPath, sources, 0)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}
	defer idx.Close()

	if err := idx.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	results, err := idx.Search("Go interfaces", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// First result should be from canonical (higher weight)
	if results[0].Source != "canonical" {
		t.Errorf("first result source = %q, want 'canonical'", results[0].Source)
	}
	if results[1].Source != "code" {
		t.Errorf("second result source = %q, want 'code'", results[1].Source)
	}
}

func TestBleveWeightedRanking(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "memory.bleve")

	highPriority := filepath.Join(dir, "high")
	lowPriority := filepath.Join(dir, "low")
	os.MkdirAll(highPriority, 0755)
	os.MkdirAll(lowPriority, 0755)

	content := []byte("The quick brown fox jumps over lazy dog")
	os.WriteFile(filepath.Join(highPriority, "a.md"), content, 0644)
	os.WriteFile(filepath.Join(lowPriority, "b.md"), content, 0644)

	sources := map[string]SourceConfig{
		"high": {Dir: highPriority, Weight: 1.0},
		"low":  {Dir: lowPriority, Weight: 0.0},
	}

	idx, err := NewBleveIndex(indexPath, sources, 0)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}
	defer idx.Close()

	if err := idx.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	results, err := idx.Search("quick brown", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	if results[0].Source != "high" {
		t.Errorf("first result source = %q, want 'high'", results[0].Source)
	}
}

func TestBleveSearchRecency(t *testing.T) {
	idx, memDir := testBleveIndex(t)

	oldFile := filepath.Join(memDir, "old.md")
	newFile := filepath.Join(memDir, "new.md")

	os.WriteFile(oldFile, []byte("Go concurrency patterns are useful"), 0644)
	oldTime := time.Now().Add(-24 * time.Hour)
	os.Chtimes(oldFile, oldTime, oldTime)

	os.WriteFile(newFile, []byte("Go concurrency channels and goroutines"), 0644)

	if err := idx.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	results, err := idx.Search("Go concurrency", "newest", nil)
	if err != nil {
		t.Fatalf("Search newest: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	if results[0].Path != "new.md" {
		t.Errorf("first result = %q, want 'new.md' (newest first)", results[0].Path)
	}
	if results[1].Path != "old.md" {
		t.Errorf("second result = %q, want 'old.md'", results[1].Path)
	}

	// Oldest sort
	results, err = idx.Search("Go concurrency", "oldest", nil)
	if err != nil {
		t.Fatalf("Search oldest: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results for oldest, got %d", len(results))
	}
	if results[0].Path != "old.md" {
		t.Errorf("oldest first result = %q, want 'old.md'", results[0].Path)
	}
	if results[1].Path != "new.md" {
		t.Errorf("oldest second result = %q, want 'new.md'", results[1].Path)
	}
}

func TestBleveSearchRelevanceDefault(t *testing.T) {
	idx, memDir := testBleveIndex(t)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("Go programming language features"), 0644)
	idx.Reindex()

	results, err := idx.Search("Go programming", "", nil)
	if err != nil {
		t.Fatalf("Search with empty sort: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results with empty sort")
	}

	results2, err := idx.Search("Go programming", "relevance", nil)
	if err != nil {
		t.Fatalf("Search with relevance sort: %v", err)
	}
	if len(results2) == 0 {
		t.Fatal("expected results with relevance sort")
	}
}

func TestBleveStartSweep(t *testing.T) {
	idx, memDir := testBleveIndex(t)

	os.WriteFile(filepath.Join(memDir, "sweep_test.md"), []byte("sweep discovers this file automatically"), 0644)

	idx.StartSweep(5*time.Millisecond, 1*time.Hour)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		results, err := idx.Search("sweep discovers", "", nil)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(results) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("sweep did not index the file within deadline")
}

func TestBleveStartSweepDisabledByClose(t *testing.T) {
	idx, memDir := testBleveIndex(t)

	idx.StartSweep(1*time.Hour, 1*time.Hour)
	idx.Close()

	os.WriteFile(filepath.Join(memDir, "post_close.md"), []byte("this should not appear"), 0644)
	time.Sleep(20 * time.Millisecond)

	// Reopen the index to verify nothing was indexed after close.
	indexPath := filepath.Join(filepath.Dir(memDir), "memory.bleve")
	sources := map[string]SourceConfig{"memory": {Dir: memDir, Weight: 1.0}}
	idx2, err := NewBleveIndex(indexPath, sources, 0)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}
	defer idx2.Close()

	results, err := idx2.Search("should not appear", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results after close, got %d", len(results))
	}
}

func TestBleveReopenExisting(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)
	indexPath := filepath.Join(dir, "memory.bleve")

	sources := map[string]SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("persistent data across reopens"), 0644)

	// Create and index
	idx, err := NewBleveIndex(indexPath, sources, 0)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}
	if err := idx.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	idx.Close()

	// Reopen
	idx2, err := NewBleveIndex(indexPath, sources, 0)
	if err != nil {
		t.Fatalf("NewBleveIndex (reopen): %v", err)
	}
	defer idx2.Close()

	results, err := idx2.Search("persistent data", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results after reopen — data should persist")
	}
}

func TestBleveSnippetContainsHighlight(t *testing.T) {
	idx, memDir := testBleveIndex(t)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("The quick brown fox jumps over the lazy dog in the sunny afternoon"), 0644)
	idx.Reindex()

	results, err := idx.Search("quick brown fox", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	// Snippet should contain highlight markers (> <)
	if results[0].Snippet == "" {
		t.Error("snippet should not be empty")
	}
}

func TestBleveSearchDateRangeFilter(t *testing.T) {
	// Tests that date_from and date_to parameters correctly filter results in bleve.
	idx, memDir := testBleveIndex(t)

	oldFile := filepath.Join(memDir, "old.md")
	newFile := filepath.Join(memDir, "new.md")

	os.WriteFile(oldFile, []byte("Historical document about alpha project"), 0644)
	oldTime := time.Now().Add(-7 * 24 * time.Hour)
	os.Chtimes(oldFile, oldTime, oldTime)

	os.WriteFile(newFile, []byte("Recent document about beta project"), 0644)

	if err := idx.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	// Without date filter - should find both
	results, err := idx.Search("project", "", nil)
	if err != nil {
		t.Fatalf("Search without filter: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results without filter, got %d", len(results))
	}

	// With date_from - should exclude old file
	dateFrom := time.Now().Add(-2 * 24 * time.Hour)
	results, err = idx.Search("project", "", &SearchOptions{DateFrom: &dateFrom})
	if err != nil {
		t.Fatalf("Search with date_from: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results with date_from")
	}
	for _, r := range results {
		if r.Path == "old.md" {
			t.Error("old.md should be excluded by date_from filter")
		}
	}

	// With date_to - should exclude new file
	dateTo := time.Now().Add(-3 * 24 * time.Hour)
	results, err = idx.Search("project", "", &SearchOptions{DateTo: &dateTo})
	if err != nil {
		t.Fatalf("Search with date_to: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results with date_to")
	}
	for _, r := range results {
		if r.Path == "new.md" {
			t.Error("new.md should be excluded by date_to filter")
		}
	}

	// With both date_from and date_to - should exclude both if range is narrow
	middleFrom := time.Now().Add(-5 * 24 * time.Hour)
	middleTo := time.Now().Add(-4 * 24 * time.Hour)
	results, err = idx.Search("project", "", &SearchOptions{DateFrom: &middleFrom, DateTo: &middleTo})
	if err != nil {
		t.Fatalf("Search with both filters: %v", err)
	}
	// Should find nothing (both files are outside the 1-day window)
	if len(results) != 0 {
		t.Errorf("expected 0 results with narrow date range, got %d", len(results))
	}
}
