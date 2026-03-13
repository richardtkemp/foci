package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/memory"
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
	// Proves the round-trip: writing a value and reading it back returns the same content.
	t.Parallel()
	tool := testScratchpadTool(t)
	ctx := context.Background()

	// Write
	params, _ := json.Marshal(map[string]string{"action": "write", "key": "notes", "content": "working on auth"})
	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(result.Text,"written") {
		t.Errorf("write result = %q", result.Text)
	}

	// Read
	params, _ = json.Marshal(map[string]string{"action": "read", "key": "notes"})
	result, err = tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if result.Text != "working on auth" {
		t.Errorf("read result = %q", result.Text)
	}
}

func TestScratchpadToolReadEmpty(t *testing.T) {
	// Proves that reading a key that was never written returns an "empty" indicator rather than an error.
	t.Parallel()
	tool := testScratchpadTool(t)
	params, _ := json.Marshal(map[string]string{"action": "read", "key": "missing"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(result.Text,"empty") {
		t.Errorf("expected empty message, got %q", result.Text)
	}
}

func TestScratchpadToolClear(t *testing.T) {
	// Proves that clearing a key removes its content, so a subsequent read returns empty.
	t.Parallel()
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
	if !strings.Contains(result.Text,"cleared") {
		t.Errorf("clear result = %q", result.Text)
	}

	// Verify cleared
	params, _ = json.Marshal(map[string]string{"action": "read", "key": "temp"})
	result, _ = tool.Execute(ctx, params)
	if !strings.Contains(result.Text,"empty") {
		t.Errorf("after clear, read = %q", result.Text)
	}
}

func TestScratchpadToolWriteMissingKey(t *testing.T) {
	// Proves that writing with an empty key is rejected with an error.
	t.Parallel()
	tool := testScratchpadTool(t)
	params, _ := json.Marshal(map[string]string{"action": "write", "key": "", "content": "data"})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestScratchpadToolListEmpty(t *testing.T) {
	// Proves that listing an empty scratchpad returns the "no entries" message rather than an error.
	t.Parallel()
	tool := testScratchpadTool(t)
	params, _ := json.Marshal(map[string]string{"action": "list"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if result.Text != "No scratchpad entries." {
		t.Errorf("expected empty message, got %q", result.Text)
	}
}

func TestScratchpadToolListWithEntries(t *testing.T) {
	// Proves that listing after writing multiple keys returns all key names with a header.
	t.Parallel()
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
	if !strings.Contains(result.Text,"notes") {
		t.Errorf("missing notes in result: %q", result.Text)
	}
	if !strings.Contains(result.Text,"context") {
		t.Errorf("missing context in result: %q", result.Text)
	}
	if !strings.Contains(result.Text,"Scratchpad entries:") {
		t.Errorf("missing header in result: %q", result.Text)
	}
}

func TestScratchpadToolUnknownAction(t *testing.T) {
	// Proves that an unrecognized action returns an "unknown action" error.
	t.Parallel()
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
