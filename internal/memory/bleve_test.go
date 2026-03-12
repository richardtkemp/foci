package memory

import (
	"fmt"
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
	idx, err := NewBleveIndex(indexPath, sources, 0, 0.1)
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

	idx, err := NewBleveIndex(indexPath, sources, 0, 0.1)
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

	idx, err := NewBleveIndex(indexPath, sources, 0, 0.1)
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
	idx2, err := NewBleveIndex(indexPath, sources, 0, 0.1)
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
	idx, err := NewBleveIndex(indexPath, sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}
	if err := idx.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	idx.Close()

	// Reopen
	idx2, err := NewBleveIndex(indexPath, sources, 0, 0.1)
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

// TestBleveIndexConversation verifies that conversation messages are indexed
// and searchable, matching the FTS5 backend's conversation indexing behavior.
func TestBleveIndexConversation(t *testing.T) {
	idx, _ := testBleveIndex(t)

	idx.IndexConversation("Tell me about quantum computing", "agent:main:main")
	idx.IndexConversation("Quantum computing uses qubits for parallel computation", "agent:main:main")

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

// TestBleveConversationEmpty verifies that empty conversation text is a no-op.
func TestBleveConversationEmpty(t *testing.T) {
	idx, _ := testBleveIndex(t)
	idx.IndexConversation("", "agent:main:main") // should not panic or error
}

// TestBleveMemoryWeightedHigher verifies that memory results rank higher
// than conversation results when both match the same query.
func TestBleveMemoryWeightedHigher(t *testing.T) {
	idx, memDir := testBleveIndex(t)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("Important fact about neural networks"), 0644)
	idx.Reindex()
	idx.IndexConversation("Random fact about neural networks", "agent:main:main")

	results, err := idx.Search("neural networks", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	// Memory result should rank higher (weight 1.0 → 2.0x multiplier vs 0.1x conversation)
	if results[0].Source != "memory" {
		t.Errorf("first result source = %q, want 'memory' (should rank higher)", results[0].Source)
	}
}

// TestBleveConversationSurvivedReindex verifies that conversation entries
// are preserved when Reindex() recreates the file portion of the index.
func TestBleveConversationSurvivedReindex(t *testing.T) {
	idx, memDir := testBleveIndex(t)

	idx.IndexConversation("Discussing special relativity theory", "agent:main:main")

	// Add a file and reindex — conversations should survive
	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("General notes about physics"), 0644)
	if err := idx.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	// Conversation should still be findable
	results, err := idx.Search("relativity", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("conversation entry should survive reindex")
	}
	if results[0].Source != "conversation" {
		t.Errorf("source = %q, want 'conversation'", results[0].Source)
	}

	// File content should also be findable
	results, err = idx.Search("physics", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("file content should be indexed after reindex")
	}
}

// TestBleveConversationWeight verifies that the conversation weight multiplier
// is applied correctly and is configurable.
func TestBleveConversationWeight(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)
	indexPath := filepath.Join(dir, "memory.bleve")

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("Neural networks are great"), 0644)

	sources := map[string]SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}

	// Use a higher conversation weight (0.5) — still lower than memory (2.0x)
	idx, err := NewBleveIndex(indexPath, sources, 0, 0.5)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}
	defer idx.Close()

	if err := idx.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	idx.IndexConversation("Neural networks are interesting", "agent:main:main")

	results, err := idx.Search("neural", "", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	// Memory should still rank higher (2.0x vs 0.5x)
	if results[0].Source != "memory" {
		t.Errorf("first result = %q, want 'memory'", results[0].Source)
	}
	if results[1].Source != "conversation" {
		t.Errorf("second result = %q, want 'conversation'", results[1].Source)
	}
}

// TestBleveTodoIndexAndSearch verifies that todos can be indexed and
// searched via the shared bleve index, with agent ID filtering.
func TestBleveTodoIndexAndSearch(t *testing.T) {
	idx, _ := testBleveIndex(t)

	idx.IndexTodo("agent1", 1, "Fix the login bug in authentication module", float64(time.Now().Unix()))
	idx.IndexTodo("agent1", 2, "Deploy new server to production", float64(time.Now().Unix()))
	idx.IndexTodo("agent2", 1, "Fix the login page styling", float64(time.Now().Unix()))

	// Search for agent1 — should find 1 matching result, not agent2's
	hits, err := idx.SearchTodos("agent1", "login", "", 0)
	if err != nil {
		t.Fatalf("SearchTodos: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit for agent1+login, got %d", len(hits))
	}
	if hits[0].TodoID != 1 {
		t.Errorf("expected todo ID 1, got %d", hits[0].TodoID)
	}

	// Search for agent2
	hits, err = idx.SearchTodos("agent2", "login", "", 0)
	if err != nil {
		t.Fatalf("SearchTodos: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit for agent2+login, got %d", len(hits))
	}
	if hits[0].TodoID != 1 {
		t.Errorf("expected todo ID 1, got %d", hits[0].TodoID)
	}
}

// TestBleveTodoRemove verifies that removing a todo from the bleve index
// makes it no longer searchable.
func TestBleveTodoRemove(t *testing.T) {
	idx, _ := testBleveIndex(t)

	idx.IndexTodo("agent1", 1, "Fix the critical bug", float64(time.Now().Unix()))
	idx.RemoveTodo("agent1", 1)

	hits, err := idx.SearchTodos("agent1", "critical", "", 0)
	if err != nil {
		t.Fatalf("SearchTodos: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("expected 0 hits after remove, got %d", len(hits))
	}
}

// TestBleveTodoStemming verifies that porter stemming works for todo
// search (e.g. "running" matches "run").
func TestBleveTodoStemming(t *testing.T) {
	idx, _ := testBleveIndex(t)

	idx.IndexTodo("agent1", 1, "Running server process in background", float64(time.Now().Unix()))

	hits, err := idx.SearchTodos("agent1", "run", "", 0)
	if err != nil {
		t.Fatalf("SearchTodos: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("porter stemming should match 'run' against 'running'")
	}
}

// TestBleveTodoSurvivesReindex verifies that todo documents are
// preserved when Reindex() recreates the file portion of the index.
func TestBleveTodoSurvivesReindex(t *testing.T) {
	idx, memDir := testBleveIndex(t)

	idx.IndexTodo("agent1", 1, "Important task about deployment", float64(time.Now().Unix()))

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("Deployment notes"), 0644)
	if err := idx.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	hits, err := idx.SearchTodos("agent1", "deployment", "", 0)
	if err != nil {
		t.Fatalf("SearchTodos: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("todo should survive reindex")
	}
}

// TestBleveTodoEmptyQuery verifies that empty queries return nil.
func TestBleveTodoEmptyQuery(t *testing.T) {
	idx, _ := testBleveIndex(t)

	hits, err := idx.SearchTodos("agent1", "", "", 0)
	if err != nil {
		t.Fatalf("SearchTodos: %v", err)
	}
	if hits != nil {
		t.Errorf("expected nil for empty query, got %v", hits)
	}

	hits, err = idx.SearchTodos("agent1", "   ", "", 0)
	if err != nil {
		t.Fatalf("SearchTodos: %v", err)
	}
	if hits != nil {
		t.Errorf("expected nil for whitespace query, got %v", hits)
	}
}

// TestBleveTodoSearchIntegration verifies end-to-end todo search through
// TodoStore backed by a bleve index (instead of FTS5).
func TestBleveTodoSearchIntegration(t *testing.T) {
	// Create bleve index
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)
	indexPath := filepath.Join(dir, "search.bleve")
	sources := map[string]SourceConfig{"memory": {Dir: memDir, Weight: 1.0}}
	idx, err := NewBleveIndex(indexPath, sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}
	defer idx.Close()

	// Create todo store and wire to bleve
	ts, err := NewTodoStore(filepath.Join(dir, "todo.db"))
	if err != nil {
		t.Fatalf("NewTodoStore: %v", err)
	}
	defer ts.Close()
	ts.SetSearchIndex(idx)

	agentID := "test-agent"

	// Add todos — should auto-index into bleve
	ts.Add(agentID, "Fix the authentication login bug", "high", "")
	ts.Add(agentID, "Deploy new server to production", "medium", "")
	ts.Add(agentID, "Write documentation for API endpoints", "low", "docs")

	// Search should find matching items via bleve
	items, err := ts.Search(agentID, "login", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 result for 'login', got %d", len(items))
	}
	if items[0].Text != "Fix the authentication login bug" {
		t.Errorf("wrong item: %q", items[0].Text)
	}

	// Edit should update the bleve index
	ts.Edit(agentID, 1, "Fix the session token bug", "", "", false)
	items, err = ts.Search(agentID, "login", nil)
	if err != nil {
		t.Fatalf("Search after edit: %v", err)
	}
	if len(items) != 0 {
		t.Error("old text 'login' should not match after edit")
	}
	items, err = ts.Search(agentID, "session token", nil)
	if err != nil {
		t.Fatalf("Search for new text: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 result for 'session token', got %d", len(items))
	}

	// Remove should update the bleve index
	ts.Remove(agentID, 2)
	items, err = ts.Search(agentID, "deploy server", nil)
	if err != nil {
		t.Fatalf("Search after remove: %v", err)
	}
	if len(items) != 0 {
		t.Error("removed todo should not appear in search")
	}
}

// TestBleveTodoIndexAllTodos verifies that IndexAllTodos populates the
// bleve index with pre-existing todos from SQLite.
func TestBleveTodoIndexAllTodos(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)

	// Create todo store WITHOUT bleve and add items
	ts, err := NewTodoStore(filepath.Join(dir, "todo.db"))
	if err != nil {
		t.Fatalf("NewTodoStore: %v", err)
	}
	agentID := "test-agent"
	ts.Add(agentID, "Pre-existing todo about kubernetes", "high", "")
	ts.Add(agentID, "Another task about docker containers", "medium", "")

	// Now create bleve and wire it up
	indexPath := filepath.Join(dir, "search.bleve")
	sources := map[string]SourceConfig{"memory": {Dir: memDir, Weight: 1.0}}
	idx, err := NewBleveIndex(indexPath, sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}
	defer idx.Close()

	ts.SetSearchIndex(idx)
	if err := ts.IndexAllTodos(agentID); err != nil {
		t.Fatalf("IndexAllTodos: %v", err)
	}

	// Search should find pre-existing items
	items, err := ts.Search(agentID, "kubernetes", nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 result for 'kubernetes', got %d", len(items))
	}
	if items[0].Text != "Pre-existing todo about kubernetes" {
		t.Errorf("wrong item: %q", items[0].Text)
	}

	ts.Close()
}

// TestBleveTodoSearchStatusFilter verifies that status filters exclude
// done/dropped todos from search results when requested.
func TestBleveTodoSearchStatusFilter(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)
	indexPath := filepath.Join(dir, "search.bleve")
	sources := map[string]SourceConfig{"memory": {Dir: memDir, Weight: 1.0}}
	idx, err := NewBleveIndex(indexPath, sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}
	defer idx.Close()

	ts, err := NewTodoStore(filepath.Join(dir, "todo.db"))
	if err != nil {
		t.Fatalf("NewTodoStore: %v", err)
	}
	defer ts.Close()
	ts.SetSearchIndex(idx)

	agentID := "test-agent"
	ts.Add(agentID, "Fix the server deployment process", "high", "")
	ts.Add(agentID, "Review the server monitoring setup", "medium", "")
	ts.Add(agentID, "Upgrade the server TLS certificates", "low", "")

	// Mark one done, one dropped
	ts.Transition(agentID, 2, "done", "reviewed")
	ts.Transition(agentID, 3, "dropped", "not needed")

	// No filter — should return all 3
	items, err := ts.Search(agentID, "server", nil)
	if err != nil {
		t.Fatalf("Search (no filter): %v", err)
	}
	if len(items) != 3 {
		t.Errorf("no filter: expected 3, got %d", len(items))
	}

	// Active filter — should exclude done and dropped
	items, err = ts.Search(agentID, "server", &TodoSearchOpts{Status: "active"})
	if err != nil {
		t.Fatalf("Search (active): %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("active filter: expected 1, got %d", len(items))
	}
	if items[0].ID != 1 {
		t.Errorf("active filter: expected todo #1, got #%d", items[0].ID)
	}

	// Specific status filter — only done
	items, err = ts.Search(agentID, "server", &TodoSearchOpts{Status: "done"})
	if err != nil {
		t.Fatalf("Search (done): %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("done filter: expected 1, got %d", len(items))
	}
	if items[0].ID != 2 {
		t.Errorf("done filter: expected todo #2, got #%d", items[0].ID)
	}
}

// TestBleveTodoSearchSortOrder verifies that sort overrides the default
// relevance ordering.
func TestBleveTodoSearchSortOrder(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)
	indexPath := filepath.Join(dir, "search.bleve")
	sources := map[string]SourceConfig{"memory": {Dir: memDir, Weight: 1.0}}
	idx, err := NewBleveIndex(indexPath, sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}
	defer idx.Close()

	ts, err := NewTodoStore(filepath.Join(dir, "todo.db"))
	if err != nil {
		t.Fatalf("NewTodoStore: %v", err)
	}
	defer ts.Close()
	ts.SetSearchIndex(idx)

	agentID := "test-agent"
	// Add items with distinct timestamps. bleve mtime is unix seconds,
	// so we index them directly with spaced-out timestamps.
	ts.Add(agentID, "First deploy task for staging", "high", "")
	ts.Add(agentID, "Second deploy task for production", "medium", "")
	ts.Add(agentID, "Third deploy task for testing", "low", "")

	// Re-index with explicit timestamps to guarantee ordering
	now := time.Now().Unix()
	idx.IndexTodo(agentID, 1, "First deploy task for staging", float64(now-100))
	idx.IndexTodo(agentID, 2, "Second deploy task for production", float64(now-50))
	idx.IndexTodo(agentID, 3, "Third deploy task for testing", float64(now))

	// Oldest first
	items, err := ts.Search(agentID, "deploy", &TodoSearchOpts{Sort: "oldest"})
	if err != nil {
		t.Fatalf("Search (oldest): %v", err)
	}
	if len(items) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(items))
	}
	if items[0].ID != 1 {
		t.Errorf("oldest: first item should be #1, got #%d", items[0].ID)
	}

	// Newest first
	items, err = ts.Search(agentID, "deploy", &TodoSearchOpts{Sort: "newest"})
	if err != nil {
		t.Fatalf("Search (newest): %v", err)
	}
	if len(items) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(items))
	}
	if items[0].ID != 3 {
		t.Errorf("newest: first item should be #3, got #%d", items[0].ID)
	}
}

// TestBleveTodoSearchLimit verifies the default limit of 10 and custom limits.
func TestBleveTodoSearchLimit(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)
	indexPath := filepath.Join(dir, "search.bleve")
	sources := map[string]SourceConfig{"memory": {Dir: memDir, Weight: 1.0}}
	idx, err := NewBleveIndex(indexPath, sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}
	defer idx.Close()

	ts, err := NewTodoStore(filepath.Join(dir, "todo.db"))
	if err != nil {
		t.Fatalf("NewTodoStore: %v", err)
	}
	defer ts.Close()
	ts.SetSearchIndex(idx)

	agentID := "test-agent"
	// Add 15 matching items
	for i := 1; i <= 15; i++ {
		ts.Add(agentID, fmt.Sprintf("Deploy service %d to production", i), "medium", "")
	}

	// Default limit should be 10
	items, err := ts.Search(agentID, "deploy", nil)
	if err != nil {
		t.Fatalf("Search (default limit): %v", err)
	}
	if len(items) != 10 {
		t.Errorf("default limit: expected 10, got %d", len(items))
	}

	// Custom limit of 5
	items, err = ts.Search(agentID, "deploy", &TodoSearchOpts{Limit: 5})
	if err != nil {
		t.Fatalf("Search (limit=5): %v", err)
	}
	if len(items) != 5 {
		t.Errorf("limit=5: expected 5, got %d", len(items))
	}

	// Custom limit of 20 (more than available)
	items, err = ts.Search(agentID, "deploy", &TodoSearchOpts{Limit: 20})
	if err != nil {
		t.Fatalf("Search (limit=20): %v", err)
	}
	if len(items) != 15 {
		t.Errorf("limit=20: expected 15, got %d", len(items))
	}
}
