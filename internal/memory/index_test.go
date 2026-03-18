package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testIndex(t *testing.T) (*Index, string) {
	t.Helper()
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)
	dbPath := filepath.Join(dir, "memory.db")

	sources := map[string]SourceConfig{
		"memory": {
			Dir:    memDir,
			Weight: 1.0,
		},
	}
	idx, err := NewIndex(dbPath, sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx, memDir
}

func TestNewIndex(t *testing.T) {
	// Verifies that NewIndex returns a usable index with a non-nil database handle.
	idx, _ := testIndex(t)
	if idx.db == nil {
		t.Fatal("db should not be nil")
	}
}

func TestReindex(t *testing.T) {
	// Verifies that Reindex scans markdown files from a source directory and makes them searchable with the correct source tag.
	idx, memDir := testIndex(t)

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

func TestReindexIdempotent(t *testing.T) {
	// Verifies that calling Reindex twice does not produce duplicate search results for the same file.
	idx, memDir := testIndex(t)

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

func TestReindexSkipsNonMarkdown(t *testing.T) {
	// Verifies that Reindex only indexes .md files, ignoring other file types such as JSON.
	idx, memDir := testIndex(t)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("markdown content here"), 0644)
	os.WriteFile(filepath.Join(memDir, "data.json"), []byte(`{"content": "json content here"}`), 0644)

	idx.Reindex()

	results, err := idx.Search("content", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Only the .md file should be indexed
	for _, r := range results {
		if r.Path == "data.json" {
			t.Error("non-markdown file should not be indexed")
		}
	}
}

func TestIndexConversation(t *testing.T) {
	// Verifies that IndexConversation stores messages so they are searchable and tagged with the "conversation" source.
	idx, _ := testIndex(t)

	idx.IndexConversation("Tell me about quantum computing", "agent:main:main", 1)
	idx.IndexConversation("Quantum computing uses qubits for parallel computation", "agent:main:main", 2)

	results, err := idx.Search("quantum", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for 'quantum'")
	}
	for _, r := range results {
		if r.Source != "conversation" {
			t.Errorf("source = %q, want 'conversation'", r.Source)
		}
	}
}

func TestMemoryWeightedHigher(t *testing.T) {
	// Verifies that memory file results rank above conversation results when both contain matching content, due to the higher source weight.
	idx, memDir := testIndex(t)

	// Add same content as both memory and conversation
	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("Important fact about neural networks"), 0644)
	idx.Reindex()
	idx.IndexConversation("Random fact about neural networks", "agent:main:main", 1)

	results, err := idx.Search("neural networks", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	// Memory result should rank higher (weighted 2x)
	if results[0].Source != "memory" {
		t.Errorf("first result source = %q, want 'memory' (should rank higher)", results[0].Source)
	}
}

func TestConversationWeightConfigurable(t *testing.T) {
	// Verifies that a custom conversation weight is applied correctly, keeping memory results above conversation results in ranking.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")

	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)
	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("Neural networks are great"), 0644)

	sources := map[string]SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}

	idx, err := NewIndex(dbPath, sources, 0, 0.5)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer idx.Close()

	if err := idx.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	idx.IndexConversation("Neural networks are interesting", "agent:main:main", 1)

	results, err := idx.Search("neural", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	if results[0].Source != "memory" {
		t.Errorf("first result source = %q, want 'memory'", results[0].Source)
	}
	if results[1].Source != "conversation" {
		t.Errorf("second result source = %q, want 'conversation'", results[1].Source)
	}
}

func TestSearchNoMatches(t *testing.T) {
	// Verifies that Search returns an empty slice (not an error) when no documents match the query.
	idx, memDir := testIndex(t)
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

func TestSearchEmptyIndex(t *testing.T) {
	// Verifies that searching an empty index returns zero results without error.
	idx, _ := testIndex(t)

	results, err := idx.Search("anything", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("got %d results, want 0", len(results))
	}
}

func TestSearchSubdirectories(t *testing.T) {
	// Verifies that Reindex recurses into subdirectories and that result paths are relative to the source root.
	idx, memDir := testIndex(t)

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

func TestIndexConversationEmpty(t *testing.T) {
	// Verifies that IndexConversation with empty text is a no-op and does not panic or error.
	idx, _ := testIndex(t)
	idx.IndexConversation("", "agent:main:main", 1)
}

func TestPorterStemming(t *testing.T) {
	// Verifies that the FTS5 Porter stemmer matches a root form query against morphological variants in indexed content.
	idx, memDir := testIndex(t)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("The programmer was programming a programmatic solution"), 0644)
	idx.Reindex()

	// Porter stemmer should match "program" against "programmer", "programming", "programmatic"
	results, err := idx.Search("program", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("porter stemmer should match 'program' against stemmed variants")
	}
}

func TestMultiSourceIndexing(t *testing.T) {
	// Verifies that documents from multiple named source directories are all indexed and tagged with their correct source name.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")

	// Create two source directories
	canonical := filepath.Join(dir, "canonical")
	code := filepath.Join(dir, "code")
	os.MkdirAll(canonical, 0755)
	os.MkdirAll(code, 0755)

	// Create files in each source
	os.WriteFile(filepath.Join(canonical, "notes.md"), []byte("Important fact about Go interfaces"), 0644)
	os.WriteFile(filepath.Join(code, "example.md"), []byte("Go interfaces allow duck typing"), 0644)

	sources := map[string]SourceConfig{
		"canonical": {Dir: canonical, Weight: 1.0},
		"code":      {Dir: code, Weight: 0.3},
	}

	idx, err := NewIndex(dbPath, sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
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

func TestWeightedRanking(t *testing.T) {
	// Verifies that identical content from a higher-weight source ranks above the same content from a lower-weight source.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")

	// Create sources with different weights
	highPriority := filepath.Join(dir, "high")
	lowPriority := filepath.Join(dir, "low")
	os.MkdirAll(highPriority, 0755)
	os.MkdirAll(lowPriority, 0755)

	// Both files have the same content
	content := []byte("The quick brown fox jumps over lazy dog")
	os.WriteFile(filepath.Join(highPriority, "a.md"), content, 0644)
	os.WriteFile(filepath.Join(lowPriority, "b.md"), content, 0644)

	sources := map[string]SourceConfig{
		"high": {Dir: highPriority, Weight: 1.0}, // 2.0x multiplier
		"low":  {Dir: lowPriority, Weight: 0.0},  // 1.0x multiplier
	}

	idx, err := NewIndex(dbPath, sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
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

	// High-weight source should rank higher
	if results[0].Source != "high" {
		t.Errorf("first result source = %q, want 'high'", results[0].Source)
	}
}

func TestBackwardCompatSingleDir(t *testing.T) {
	// Verifies that a single-source configuration indexes and returns results correctly, covering the common single-directory deployment.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)

	// Create a single source with the old default weight
	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("Testing backward compatibility"), 0644)

	sources := map[string]SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}

	idx, err := NewIndex(dbPath, sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer idx.Close()

	if err := idx.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	results, err := idx.Search("backward compatibility", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for backward compat single-source test")
	}
	if results[0].Source != "memory" {
		t.Errorf("source = %q, want 'memory'", results[0].Source)
	}
}

func TestSearchRecency(t *testing.T) {
	// Verifies that "newest" and "oldest" sort modes order results by file modification time rather than relevance.
	idx, memDir := testIndex(t)

	// Create files with different mtimes
	oldFile := filepath.Join(memDir, "old.md")
	newFile := filepath.Join(memDir, "new.md")

	os.WriteFile(oldFile, []byte("Go concurrency patterns are useful"), 0644)
	// Set old file mtime to the past
	oldTime := time.Now().Add(-24 * time.Hour)
	os.Chtimes(oldFile, oldTime, oldTime)

	os.WriteFile(newFile, []byte("Go concurrency channels and goroutines"), 0644)
	// new file keeps current mtime

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

	// Newest file should come first in newest sort
	if results[0].Path != "new.md" {
		t.Errorf("first result = %q, want 'new.md' (newest first)", results[0].Path)
	}
	if results[1].Path != "old.md" {
		t.Errorf("second result = %q, want 'old.md'", results[1].Path)
	}

	// Oldest sort — old file should come first
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

func TestSearchRelevanceDefault(t *testing.T) {
	// Verifies that both an empty sort string and the explicit "relevance" sort value return matching results.
	idx, memDir := testIndex(t)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("Go programming language features"), 0644)
	idx.Reindex()

	// Empty string sort should use relevance (default)
	results, err := idx.Search("Go programming", "", nil)
	if err != nil {
		t.Fatalf("Search with empty sort: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results with empty sort")
	}

	// Explicit "relevance" should also work
	results2, err := idx.Search("Go programming", "relevance", nil)
	if err != nil {
		t.Fatalf("Search with relevance sort: %v", err)
	}
	if len(results2) == 0 {
		t.Fatal("expected results with relevance sort")
	}
}

func TestMetaTablePopulated(t *testing.T) {
	// Verifies that Reindex populates the memory_meta table with the correct source tag and a non-zero mtime for each indexed file.
	idx, memDir := testIndex(t)

	testFile := filepath.Join(memDir, "meta_test.md")
	os.WriteFile(testFile, []byte("metadata test content"), 0644)

	if err := idx.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	var count int
	if err := idx.db.QueryRow("SELECT COUNT(*) FROM memory_meta WHERE source = 'memory'").Scan(&count); err != nil {
		t.Fatalf("query memory_meta: %v", err)
	}
	if count != 1 {
		t.Errorf("memory_meta count = %d, want 1", count)
	}

	var mtime float64
	if err := idx.db.QueryRow("SELECT mtime FROM memory_meta WHERE path = 'meta_test.md'").Scan(&mtime); err != nil {
		t.Fatalf("query mtime: %v", err)
	}
	if mtime <= 0 {
		t.Errorf("mtime = %f, want > 0", mtime)
	}
}

func TestStartSweep(t *testing.T) {
	// Verifies that StartSweep periodically reindexes source directories so new files become searchable without an explicit Reindex call.
	idx, memDir := testIndex(t)

	// Write a file but don't call Reindex — the sweep should pick it up.
	os.WriteFile(filepath.Join(memDir, "sweep_test.md"), []byte("sweep discovers this file automatically"), 0644)

	idx.StartSweep(5*time.Millisecond, 1*time.Hour)

	// Wait for the initial sweep to fire and complete.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		results, err := idx.Search("sweep discovers", "", nil)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(results) > 0 {
			return // success — sweep indexed the file
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("sweep did not index the file within deadline")
}

func TestStartSweepDisabledByClose(t *testing.T) {
	// Verifies that closing the index stops the sweep goroutine so files written after Close are never indexed.
	idx, memDir := testIndex(t)

	// Start sweep with a long initial delay, then close immediately.
	idx.StartSweep(1*time.Hour, 1*time.Hour)
	idx.Close()

	// Write a file after close — should not be indexed.
	os.WriteFile(filepath.Join(memDir, "post_close.md"), []byte("this should not appear"), 0644)
	time.Sleep(20 * time.Millisecond)

	// Reopen the index to verify nothing was indexed after close.
	dbPath := filepath.Join(filepath.Dir(memDir), "memory.db")
	sources := map[string]SourceConfig{"memory": {Dir: memDir, Weight: 1.0}}
	idx2, err := NewIndex(dbPath, sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
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

func TestIndexBusyTimeout(t *testing.T) {
	// Verifies that the SQLite connection is configured with a 5-second busy timeout to avoid immediate lock failures under contention.
	idx, _ := testIndex(t)

	var timeout int
	if err := idx.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", timeout)
	}
}

func TestSanitizeFTS5Query(t *testing.T) {
	// Verifies that sanitizeFTS5Query quotes each term to prevent FTS5 from
	// interpreting hyphens as column filters and keywords as boolean operators.
	tests := []struct {
		input, want string
	}{
		{"hunter-alpha model", `"hunter-alpha" "model"`},
		{"simple query", `"simple" "query"`},
		{`has "quotes"`, `"has" """quotes"""`},
		{"OR AND NOT", `"OR" "AND" "NOT"`},
		{"", ""},
		{"single", `"single"`},
	}
	for _, tt := range tests {
		got := sanitizeFTS5Query(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeFTS5Query(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSearchHyphenatedQuery(t *testing.T) {
	// Verifies that queries containing hyphens (e.g. "hunter-alpha") don't
	// cause FTS5 column-filter errors and still return matching results.
	idx, memDir := testIndex(t)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("The hunter-alpha protocol is used for tracking."), 0644)
	if err := idx.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	results, err := idx.Search("hunter-alpha protocol", "", nil)
	if err != nil {
		t.Fatalf("Search with hyphenated query should not error: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for hyphenated query")
	}
}

func TestSearchDateRangeFilter(t *testing.T) {
	// Tests that date_from and date_to parameters correctly filter results.
	idx, memDir := testIndex(t)

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
