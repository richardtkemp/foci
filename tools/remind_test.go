package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"clod/memory"
)

func testRemindTool(t *testing.T) *Tool {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	rs, err := memory.NewReminderStore(dbPath)
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })
	return NewMemoryRemindTool(rs)
}

func TestMemoryRemind(t *testing.T) {
	tool := testRemindTool(t)
	params, _ := json.Marshal(map[string]string{
		"text": "check FTS5 phrase boosting",
		"when": "next_heartbeat",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "next_heartbeat") {
		t.Errorf("result = %q, expected mention of next_heartbeat", result)
	}
	if !strings.Contains(result, "check FTS5 phrase boosting") {
		t.Errorf("result = %q, expected mention of text", result)
	}
}

func TestMemoryRemindTomorrow(t *testing.T) {
	tool := testRemindTool(t)
	params, _ := json.Marshal(map[string]string{
		"text": "ask about Greece",
		"when": "tomorrow",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "tomorrow") {
		t.Errorf("result = %q", result)
	}
}

func TestMemoryRemindMissingText(t *testing.T) {
	tool := testRemindTool(t)
	params, _ := json.Marshal(map[string]string{
		"text": "",
		"when": "now",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for empty text")
	}
}

func TestMemoryRemindMissingWhen(t *testing.T) {
	tool := testRemindTool(t)
	params, _ := json.Marshal(map[string]string{
		"text": "something",
		"when": "",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for empty when")
	}
}
