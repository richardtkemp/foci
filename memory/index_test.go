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

	idx, err := NewIndex(dbPath, memDir)
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
