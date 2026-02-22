package memory

import (
	"os"
	"path/filepath"
	"testing"
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
	idx, _ := testIndex(t)
	if idx.db == nil {
		t.Fatal("db should not be nil")
	}
}

func TestReindex(t *testing.T) {
	idx, memDir := testIndex(t)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("The Go programming language is great for systems work."), 0644)
	os.WriteFile(filepath.Join(memDir, "journal.md"), []byte("Today I learned about Go interfaces and embedding."), 0644)

	if err := idx.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	results, err := idx.Search("Go programming")
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
	idx, memDir := testIndex(t)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("unique content for testing"), 0644)

	idx.Reindex()
	idx.Reindex() // second reindex should not duplicate

	results, err := idx.Search("unique content")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("got %d results, want 1 (reindex should not duplicate)", len(results))
	}
}

func TestReindexSkipsNonMarkdown(t *testing.T) {
	idx, memDir := testIndex(t)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("markdown content here"), 0644)
	os.WriteFile(filepath.Join(memDir, "data.json"), []byte(`{"content": "json content here"}`), 0644)

	idx.Reindex()

	results, err := idx.Search("content")
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
	idx, _ := testIndex(t)

	idx.IndexConversation("Tell me about quantum computing", "agent:main:main")
	idx.IndexConversation("Quantum computing uses qubits for parallel computation", "agent:main:main")

	results, err := idx.Search("quantum")
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
	idx, memDir := testIndex(t)

	// Add same content as both memory and conversation
	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("Important fact about neural networks"), 0644)
	idx.Reindex()
	idx.IndexConversation("Random fact about neural networks", "agent:main:main")

	results, err := idx.Search("neural networks")
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

	idx.IndexConversation("Neural networks are interesting", "agent:main:main")

	results, err := idx.Search("neural")
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
	idx, memDir := testIndex(t)
	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("nothing relevant here"), 0644)
	idx.Reindex()

	results, err := idx.Search("xyzzy")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("got %d results, want 0", len(results))
	}
}

func TestSearchEmptyIndex(t *testing.T) {
	idx, _ := testIndex(t)

	results, err := idx.Search("anything")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("got %d results, want 0", len(results))
	}
}

func TestSearchSubdirectories(t *testing.T) {
	idx, memDir := testIndex(t)

	sub := filepath.Join(memDir, "2024")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "01-15.md"), []byte("January notes about winter activities"), 0644)

	idx.Reindex()

	results, err := idx.Search("winter")
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
	idx, _ := testIndex(t)
	// Should not panic or error on empty text
	idx.IndexConversation("", "agent:main:main")
}

func TestPorterStemming(t *testing.T) {
	idx, memDir := testIndex(t)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("The programmer was programming a programmatic solution"), 0644)
	idx.Reindex()

	// Porter stemmer should match "program" against "programmer", "programming", "programmatic"
	results, err := idx.Search("program")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("porter stemmer should match 'program' against stemmed variants")
	}
}

func TestMultiSourceIndexing(t *testing.T) {
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

	results, err := idx.Search("Go interfaces")
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

	results, err := idx.Search("quick brown")
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

	results, err := idx.Search("backward compatibility")
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
