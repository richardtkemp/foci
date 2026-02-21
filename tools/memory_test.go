package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemorySearch(t *testing.T) {
	dir := t.TempDir()

	// Create some memory files
	os.WriteFile(filepath.Join(dir, "notes.md"), []byte("Remember to buy milk\nThe sky is blue\n"), 0644)
	os.WriteFile(filepath.Join(dir, "todo.md"), []byte("Buy groceries\nClean house\nBuy a new book\n"), 0644)

	tool := NewMemorySearchTool(dir)
	params, _ := json.Marshal(map[string]string{"query": "buy"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should find "buy" in both files (case-insensitive)
	if !strings.Contains(result, "notes.md") {
		t.Errorf("missing notes.md match in result: %q", result)
	}
	if !strings.Contains(result, "todo.md") {
		t.Errorf("missing todo.md match in result: %q", result)
	}
	// "Buy" appears 3 times total (1 in notes, 2 in todo)
	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) != 3 {
		t.Errorf("got %d matches, want 3", len(lines))
	}
}

func TestMemorySearchCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.md"), []byte("Hello World\nhello world\nHELLO WORLD\n"), 0644)

	tool := NewMemorySearchTool(dir)
	params, _ := json.Marshal(map[string]string{"query": "hello"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) != 3 {
		t.Errorf("got %d matches, want 3 (case insensitive)", len(lines))
	}
}

func TestMemorySearchNoMatches(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.md"), []byte("nothing relevant here\n"), 0644)

	tool := NewMemorySearchTool(dir)
	params, _ := json.Marshal(map[string]string{"query": "xyzzy"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "No matches found." {
		t.Errorf("result = %q", result)
	}
}

func TestMemorySearchSkipsNonMarkdown(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "notes.md"), []byte("keyword here\n"), 0644)
	os.WriteFile(filepath.Join(dir, "data.json"), []byte(`{"keyword": true}`), 0644)
	os.WriteFile(filepath.Join(dir, "script.sh"), []byte("echo keyword\n"), 0644)

	tool := NewMemorySearchTool(dir)
	params, _ := json.Marshal(map[string]string{"query": "keyword"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) != 1 {
		t.Errorf("got %d matches, want 1 (only .md files)", len(lines))
	}
}

func TestMemorySearchSubdirectories(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "2024")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "01-15.md"), []byte("found it here\n"), 0644)

	tool := NewMemorySearchTool(dir)
	params, _ := json.Marshal(map[string]string{"query": "found"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "2024") {
		t.Errorf("missing subdirectory in result: %q", result)
	}
}

func TestMemorySearchEmptyDir(t *testing.T) {
	dir := t.TempDir()

	tool := NewMemorySearchTool(dir)
	params, _ := json.Marshal(map[string]string{"query": "anything"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "No matches found." {
		t.Errorf("result = %q", result)
	}
}
