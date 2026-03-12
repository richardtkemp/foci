package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/memory"
)

func testMemoryTool(t *testing.T) (*Tool, string) {
	t.Helper()
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)
	dbPath := filepath.Join(dir, "memory.db")

	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, err := memory.NewIndex(dbPath, sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { idx.Close() })

	backends := map[string]memory.Searcher{"fts5": idx}
	return NewMemorySearchTool(backends, []string{"fts5"}), memDir
}

func TestMemorySearch(t *testing.T) {
	t.Parallel()
	_, memDir := testMemoryTool(t)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("Remember to buy milk\nThe sky is blue\n"), 0644)
	os.WriteFile(filepath.Join(memDir, "todo.md"), []byte("Buy groceries\nClean house\nBuy a new book\n"), 0644)

	// Re-index after writing files (the index was created before files existed)
	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, _ := memory.NewIndex(filepath.Join(filepath.Dir(memDir), "memory.db"), sources, 0, 0.1)
	defer idx.Close()
	idx.Reindex()

	backends := map[string]memory.Searcher{"fts5": idx}
	tool2 := NewMemorySearchTool(backends, []string{"fts5"})
	params, _ := json.Marshal(map[string]string{"query": "buy"})

	result, err := tool2.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should find "buy" in both files
	if !strings.Contains(result.Text, "notes.md") {
		t.Errorf("missing notes.md in result: %q", result.Text)
	}
	if !strings.Contains(result.Text, "todo.md") {
		t.Errorf("missing todo.md in result: %q", result.Text)
	}

}

func TestMemorySearchNoMatches(t *testing.T) {
	t.Parallel()
	_, memDir := testMemoryTool(t)
	os.WriteFile(filepath.Join(memDir, "test.md"), []byte("nothing relevant here\n"), 0644)

	// Need to reindex with a fresh connection to pick up the file
	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, _ := memory.NewIndex(filepath.Join(filepath.Dir(memDir), "memory.db"), sources, 0, 0.1)
	defer idx.Close()
	idx.Reindex()

	backends := map[string]memory.Searcher{"fts5": idx}
	tool := NewMemorySearchTool(backends, []string{"fts5"})
	params, _ := json.Marshal(map[string]string{"query": "xyzzy"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Text != "No matches found." {
		t.Errorf("result = %q", result.Text)
	}
}

func TestMemorySearchEmpty(t *testing.T) {
	t.Parallel()
	tool, _ := testMemoryTool(t)
	params, _ := json.Marshal(map[string]string{"query": "anything"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Text != "No matches found." {
		t.Errorf("result = %q", result.Text)
	}
}

func TestMemorySearchShowsSource(t *testing.T) {
	t.Parallel()
	_, memDir := testMemoryTool(t)
	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("The weather is sunny today"), 0644)

	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, _ := memory.NewIndex(filepath.Join(filepath.Dir(memDir), "memory.db"), sources, 0, 0.1)
	defer idx.Close()
	idx.Reindex()
	idx.IndexConversation("We talked about the weather yesterday", "agent:main:main")

	backends := map[string]memory.Searcher{"fts5": idx}
	tool := NewMemorySearchTool(backends, []string{"fts5"})
	params, _ := json.Marshal(map[string]string{"query": "weather"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should show both memory and conversation results with source labels
	if !strings.Contains(result.Text, "[memory]") {
		t.Errorf("missing [memory] source label in result: %q", result.Text)
	}
	if !strings.Contains(result.Text, "[conversation]") {
		t.Errorf("missing [conversation] source label in result: %q", result.Text)
	}
}

func TestMemorySearchSortParam(t *testing.T) {
	t.Parallel()
	_, memDir := testMemoryTool(t)

	os.WriteFile(filepath.Join(memDir, "recent.md"), []byte("Recently added content about sorting"), 0644)

	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, _ := memory.NewIndex(filepath.Join(filepath.Dir(memDir), "memory.db"), sources, 0, 0.1)
	defer idx.Close()
	idx.Reindex()

	backends := map[string]memory.Searcher{"fts5": idx}
	tool := NewMemorySearchTool(backends, []string{"fts5"})

	// Test with sort=newest
	params, _ := json.Marshal(map[string]string{"query": "sorting", "sort": "newest"})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with sort=newest: %v", err)
	}
	if !strings.Contains(result.Text, "recent.md") {
		t.Errorf("missing recent.md in result: %q", result.Text)
	}

	// Test with sort=oldest
	params, _ = json.Marshal(map[string]string{"query": "sorting", "sort": "oldest"})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with sort=oldest: %v", err)
	}
	if !strings.Contains(result.Text, "recent.md") {
		t.Errorf("missing recent.md in result: %q", result.Text)
	}

	// Test with sort=relevance (explicit)
	params, _ = json.Marshal(map[string]string{"query": "sorting", "sort": "relevance"})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with sort=relevance: %v", err)
	}
	if !strings.Contains(result.Text, "recent.md") {
		t.Errorf("missing recent.md in result: %q", result.Text)
	}

	// Test with no sort param (default)
	params, _ = json.Marshal(map[string]string{"query": "sorting"})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with default sort: %v", err)
	}
	if !strings.Contains(result.Text, "recent.md") {
		t.Errorf("missing recent.md in result: %q", result.Text)
	}
}

func TestMemorySearchBackendParam(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("Testing backend selection parameter"), 0644)

	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}

	// Create both FTS5 and bleve backends
	fts5Idx, err := memory.NewIndex(filepath.Join(dir, "memory.db"), sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer fts5Idx.Close()
	fts5Idx.Reindex()

	bleveIdx, err := memory.NewBleveIndex(filepath.Join(dir, "memory.bleve"), sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}
	defer bleveIdx.Close()
	bleveIdx.Reindex()

	backends := map[string]memory.Searcher{
		"fts5":  fts5Idx,
		"bleve": bleveIdx,
	}
	tool := NewMemorySearchTool(backends, []string{"fts5", "bleve"})

	// Tool schema should include "backend" parameter when multiple backends
	schemaStr := string(tool.Parameters)
	if !strings.Contains(schemaStr, "backend") {
		t.Error("schema should include 'backend' parameter when multiple backends configured")
	}

	// Search with explicit backend=fts5
	params, _ := json.Marshal(map[string]string{"query": "backend selection", "backend": "fts5"})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with backend=fts5: %v", err)
	}
	if !strings.Contains(result.Text, "notes.md") {
		t.Errorf("fts5 backend should find notes.md: %q", result.Text)
	}

	// Search with explicit backend=bleve
	params, _ = json.Marshal(map[string]string{"query": "backend selection", "backend": "bleve"})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with backend=bleve: %v", err)
	}
	if !strings.Contains(result.Text, "notes.md") {
		t.Errorf("bleve backend should find notes.md: %q", result.Text)
	}

	// Search without backend (should use default)
	params, _ = json.Marshal(map[string]string{"query": "backend selection"})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with default backend: %v", err)
	}
	if !strings.Contains(result.Text, "notes.md") {
		t.Errorf("default backend should find notes.md: %q", result.Text)
	}
}

func TestMemorySearchSingleBackendHidesParam(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)

	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, err := memory.NewIndex(filepath.Join(dir, "memory.db"), sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer idx.Close()

	// Single backend — schema should NOT include "backend" parameter
	backends := map[string]memory.Searcher{"fts5": idx}
	tool := NewMemorySearchTool(backends, []string{"fts5"})

	schemaStr := string(tool.Parameters)
	if strings.Contains(schemaStr, "backend") {
		t.Error("schema should NOT include 'backend' parameter when only one backend configured")
	}
}

func TestMemorySearchDateRange(t *testing.T) {
	// Tests date_from and date_to filtering functionality.
	t.Parallel()
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)

	oldFile := filepath.Join(memDir, "old.md")
	newFile := filepath.Join(memDir, "new.md")

	os.WriteFile(oldFile, []byte("Historical document about project alpha"), 0644)
	oldTime := time.Now().Add(-7 * 24 * time.Hour)
	os.Chtimes(oldFile, oldTime, oldTime)

	os.WriteFile(newFile, []byte("Recent document about project beta"), 0644)

	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, err := memory.NewIndex(filepath.Join(dir, "memory.db"), sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer idx.Close()
	idx.Reindex()

	backends := map[string]memory.Searcher{"fts5": idx}
	tool := NewMemorySearchTool(backends, []string{"fts5"})

	// Search without date filter should find both
	params, _ := json.Marshal(map[string]string{"query": "project"})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute without date filter: %v", err)
	}
	if !strings.Contains(result.Text, "old.md") || !strings.Contains(result.Text, "new.md") {
		t.Errorf("expected both files without date filter: %q", result.Text)
	}

	// Search with date_from should exclude old file
	dateFrom := time.Now().Add(-2 * 24 * time.Hour).Format("2006-01-02")
	params, _ = json.Marshal(map[string]string{"query": "project", "date_from": dateFrom})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with date_from: %v", err)
	}
	if strings.Contains(result.Text, "old.md") {
		t.Errorf("old.md should be excluded by date_from: %q", result.Text)
	}
	if !strings.Contains(result.Text, "new.md") {
		t.Errorf("new.md should be included by date_from: %q", result.Text)
	}

	// Search with date_to should exclude new file
	dateTo := time.Now().Add(-3 * 24 * time.Hour).Format("2006-01-02")
	params, _ = json.Marshal(map[string]string{"query": "project", "date_to": dateTo})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with date_to: %v", err)
	}
	if !strings.Contains(result.Text, "old.md") {
		t.Errorf("old.md should be included by date_to: %q", result.Text)
	}
	if strings.Contains(result.Text, "new.md") {
		t.Errorf("new.md should be excluded by date_to: %q", result.Text)
	}
}

func TestMemorySearchDateRangeInvalid(t *testing.T) {
	// Tests that invalid date format returns a clear error.
	t.Parallel()
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)

	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, err := memory.NewIndex(filepath.Join(dir, "memory.db"), sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer idx.Close()

	backends := map[string]memory.Searcher{"fts5": idx}
	tool := NewMemorySearchTool(backends, []string{"fts5"})

	// Invalid date_from format
	params, _ := json.Marshal(map[string]string{"query": "test", "date_from": "2024/01/15"})
	_, err = tool.Execute(context.Background(), params)
	if err == nil {
		t.Error("expected error for invalid date_from format")
	}
	if !strings.Contains(err.Error(), "date_from") {
		t.Errorf("error should mention date_from: %v", err)
	}

	// Invalid date_to format
	params, _ = json.Marshal(map[string]string{"query": "test", "date_to": "Jan 15, 2024"})
	_, err = tool.Execute(context.Background(), params)
	if err == nil {
		t.Error("expected error for invalid date_to format")
	}
	if !strings.Contains(err.Error(), "date_to") {
		t.Errorf("error should mention date_to: %v", err)
	}
}
