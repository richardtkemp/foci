package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"foci/memory"
)

func testScratchpadTool(t *testing.T) *Tool {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := memory.NewScratchpad(dbPath)
	if err != nil {
		t.Fatalf("NewScratchpad: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return NewScratchpadTool(s, "test")
}

func TestScratchpadToolWriteRead(t *testing.T) {
	tool := testScratchpadTool(t)
	ctx := context.Background()

	// Write
	params, _ := json.Marshal(map[string]string{"action": "write", "key": "notes", "content": "working on auth"})
	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(result, "written") {
		t.Errorf("write result = %q", result)
	}

	// Read
	params, _ = json.Marshal(map[string]string{"action": "read", "key": "notes"})
	result, err = tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if result != "working on auth" {
		t.Errorf("read result = %q", result)
	}
}

func TestScratchpadToolReadEmpty(t *testing.T) {
	tool := testScratchpadTool(t)
	params, _ := json.Marshal(map[string]string{"action": "read", "key": "missing"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(result, "empty") {
		t.Errorf("expected empty message, got %q", result)
	}
}

func TestScratchpadToolClear(t *testing.T) {
	tool := testScratchpadTool(t)
	ctx := context.Background()

	// Write then clear
	params, _ := json.Marshal(map[string]string{"action": "write", "key": "temp", "content": "temporary"})
	tool.Execute(ctx, params)

	params, _ = json.Marshal(map[string]string{"action": "clear", "key": "temp"})
	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if !strings.Contains(result, "cleared") {
		t.Errorf("clear result = %q", result)
	}

	// Verify cleared
	params, _ = json.Marshal(map[string]string{"action": "read", "key": "temp"})
	result, _ = tool.Execute(ctx, params)
	if !strings.Contains(result, "empty") {
		t.Errorf("after clear, read = %q", result)
	}
}

func TestScratchpadToolWriteMissingKey(t *testing.T) {
	tool := testScratchpadTool(t)
	params, _ := json.Marshal(map[string]string{"action": "write", "key": "", "content": "data"})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestScratchpadToolListEmpty(t *testing.T) {
	tool := testScratchpadTool(t)
	params, _ := json.Marshal(map[string]string{"action": "list"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if result != "No scratchpad entries." {
		t.Errorf("expected empty message, got %q", result)
	}
}

func TestScratchpadToolListWithEntries(t *testing.T) {
	tool := testScratchpadTool(t)
	ctx := context.Background()

	// Write some entries
	tool.Execute(ctx, json.RawMessage(`{"action": "write", "key": "notes", "content": "some data here"}`))
	tool.Execute(ctx, json.RawMessage(`{"action": "write", "key": "context", "content": "more content for testing"}`))

	params, _ := json.Marshal(map[string]string{"action": "list"})
	result, err := tool.Execute(ctx, params)
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

func TestScratchpadToolUnknownAction(t *testing.T) {
	tool := testScratchpadTool(t)
	params, _ := json.Marshal(map[string]string{"action": "delete"})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for unknown action")
	}
	if !strings.Contains(err.Error(), "unknown action") {
		t.Errorf("error = %q", err.Error())
	}
}
