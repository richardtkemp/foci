package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"foci/memory"
)

func testScratchpadTools(t *testing.T) (*Tool, *Tool, *Tool) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := memory.NewScratchpad(dbPath)
	if err != nil {
		t.Fatalf("NewScratchpad: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return NewScratchpadWriteTool(s, "test"), NewScratchpadReadTool(s, "test"), NewScratchpadClearTool(s, "test")
}

func testScratchpadToolsWithList(t *testing.T) (*Tool, *Tool, *Tool, *Tool) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := memory.NewScratchpad(dbPath)
	if err != nil {
		t.Fatalf("NewScratchpad: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return NewScratchpadWriteTool(s, "test"), NewScratchpadReadTool(s, "test"), NewScratchpadClearTool(s, "test"), NewScratchpadListTool(s, "test")
}

func TestScratchpadToolWriteRead(t *testing.T) {
	writeTool, readTool, _ := testScratchpadTools(t)
	ctx := context.Background()

	// Write
	params, _ := json.Marshal(map[string]string{"key": "notes", "content": "working on auth"})
	result, err := writeTool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(result, "written") {
		t.Errorf("write result = %q", result)
	}

	// Read
	params, _ = json.Marshal(map[string]string{"key": "notes"})
	result, err = readTool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if result != "working on auth" {
		t.Errorf("read result = %q", result)
	}
}

func TestScratchpadToolReadEmpty(t *testing.T) {
	_, readTool, _ := testScratchpadTools(t)
	params, _ := json.Marshal(map[string]string{"key": "missing"})

	result, err := readTool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(result, "empty") {
		t.Errorf("expected empty message, got %q", result)
	}
}

func TestScratchpadToolClear(t *testing.T) {
	writeTool, readTool, clearTool := testScratchpadTools(t)
	ctx := context.Background()

	// Write then clear
	params, _ := json.Marshal(map[string]string{"key": "temp", "content": "temporary"})
	writeTool.Execute(ctx, params)

	params, _ = json.Marshal(map[string]string{"key": "temp"})
	result, err := clearTool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if !strings.Contains(result, "cleared") {
		t.Errorf("clear result = %q", result)
	}

	// Verify cleared
	result, _ = readTool.Execute(ctx, params)
	if !strings.Contains(result, "empty") {
		t.Errorf("after clear, read = %q", result)
	}
}

func TestScratchpadToolWriteMissingKey(t *testing.T) {
	writeTool, _, _ := testScratchpadTools(t)
	params, _ := json.Marshal(map[string]string{"key": "", "content": "data"})

	_, err := writeTool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestScratchpadToolListEmpty(t *testing.T) {
	_, _, _, listTool := testScratchpadToolsWithList(t)
	params := json.RawMessage("{}")

	result, err := listTool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if result != "No scratchpad entries." {
		t.Errorf("expected empty message, got %q", result)
	}
}

func TestScratchpadToolListWithEntries(t *testing.T) {
	writeTool, _, _, listTool := testScratchpadToolsWithList(t)
	ctx := context.Background()

	// Write some entries
	writeTool.Execute(ctx, json.RawMessage(`{"key": "notes", "content": "some data here"}`))
	writeTool.Execute(ctx, json.RawMessage(`{"key": "context", "content": "more content for testing"}`))

	params := json.RawMessage("{}")
	result, err := listTool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !strings.Contains(result, "notes") {
		t.Errorf("missing notes in result: %q", result)
	}
	if !strings.Contains(result, "context") {
		t.Errorf("missing context in result: %q", result)
	}
	if !strings.Contains(result, "Scratchpad entries:") {
		t.Errorf("missing header in result: %q", result)
	}
}
