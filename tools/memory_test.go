package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clod/memory"
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

	return NewMemorySearchTool(idx), memDir
}

func TestMemorySearch(t *testing.T) {
	tool, memDir := testMemoryTool(t)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("Remember to buy milk\nThe sky is blue\n"), 0644)
	os.WriteFile(filepath.Join(memDir, "todo.md"), []byte("Buy groceries\nClean house\nBuy a new book\n"), 0644)

	// Re-index after writing files (the index was created before files existed)
	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, _ := memory.NewIndex(filepath.Join(filepath.Dir(memDir), "memory.db"), sources, 0, 0.1)
	defer idx.Close()
	idx.Reindex()

	tool2 := NewMemorySearchTool(idx)
	params, _ := json.Marshal(map[string]string{"query": "buy"})

	result, err := tool2.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should find "buy" in both files
	if !strings.Contains(result, "notes.md") {
		t.Errorf("missing notes.md in result: %q", result)
	}
	if !strings.Contains(result, "todo.md") {
		t.Errorf("missing todo.md in result: %q", result)
	}

	// Verify it's not using the uninitialized tool
	_ = tool
}

func TestMemorySearchNoMatches(t *testing.T) {
	tool, memDir := testMemoryTool(t)
	os.WriteFile(filepath.Join(memDir, "test.md"), []byte("nothing relevant here\n"), 0644)

	// Need to reindex with a fresh connection to pick up the file
	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, _ := memory.NewIndex(filepath.Join(filepath.Dir(memDir), "memory.db"), sources, 0, 0.1)
	defer idx.Close()
	idx.Reindex()

	tool = NewMemorySearchTool(idx)
	params, _ := json.Marshal(map[string]string{"query": "xyzzy"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "No matches found." {
		t.Errorf("result = %q", result)
	}
}

func TestMemorySearchEmpty(t *testing.T) {
	tool, _ := testMemoryTool(t)
	params, _ := json.Marshal(map[string]string{"query": "anything"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "No matches found." {
		t.Errorf("result = %q", result)
	}
}

func TestMemorySearchShowsSource(t *testing.T) {
	tool, memDir := testMemoryTool(t)
	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("The weather is sunny today"), 0644)

	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, _ := memory.NewIndex(filepath.Join(filepath.Dir(memDir), "memory.db"), sources, 0, 0.1)
	defer idx.Close()
	idx.Reindex()
	idx.IndexConversation("We talked about the weather yesterday", "agent:main:main")

	tool = NewMemorySearchTool(idx)
	params, _ := json.Marshal(map[string]string{"query": "weather"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should show both memory and conversation results with source labels
	if !strings.Contains(result, "[memory]") {
		t.Errorf("missing [memory] source label in result: %q", result)
	}
	if !strings.Contains(result, "[conversation]") {
		t.Errorf("missing [conversation] source label in result: %q", result)
	}
}
