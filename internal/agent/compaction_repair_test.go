package agent

import (
	"encoding/json"
	"testing"

	"foci/internal/provider"
)

func TestRepairDuplicateToolUseIDs_NoDuplicates(t *testing.T) {
	// No duplicates: messages should be returned unchanged.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("hello")},
		{Role: "assistant", Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "toolu_1", Name: "read", Input: json.RawMessage(`{}`)},
		}},
		{Role: "user", Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_1", Content: "ok"},
		}},
		{Role: "assistant", Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "toolu_2", Name: "write", Input: json.RawMessage(`{}`)},
		}},
		{Role: "user", Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_2", Content: "ok"},
		}},
	}

	var warnings []string
	result, repaired := repairDuplicateToolUseIDs(msgs, func(format string, args ...any) {
		warnings = append(warnings, format)
	})

	if repaired {
		t.Error("expected repaired=false when no duplicates")
	}
	if len(warnings) > 0 {
		t.Errorf("expected no warnings, got %d", len(warnings))
	}
	// Should return the same slice (no copy) when no duplicates
	if &result[0] != &msgs[0] {
		t.Error("expected same slice returned when no duplicates")
	}
}

func TestRepairDuplicateToolUseIDs_WithDuplicates(t *testing.T) {
	// Duplicate tool_use ID: toolu_1 appears in messages 1 and 3.
	// The second occurrence and its matching tool_result should be rewritten.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("hello")},
		{Role: "assistant", Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "toolu_1", Name: "read", Input: json.RawMessage(`{}`)},
		}},
		{Role: "user", Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_1", Content: "first result"},
		}},
		{Role: "assistant", Content: []provider.ContentBlock{
			{Type: "text", Text: "I'll read again"},
			{Type: "tool_use", ID: "toolu_1", Name: "read", Input: json.RawMessage(`{}`)}, // duplicate!
			{Type: "tool_use", ID: "toolu_3", Name: "write", Input: json.RawMessage(`{}`)},
		}},
		{Role: "user", Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_1", Content: "second result"}, // should be rewritten
			{Type: "tool_result", ToolUseID: "toolu_3", Content: "ok"},
		}},
	}

	var warnings []string
	result, repaired := repairDuplicateToolUseIDs(msgs, func(format string, args ...any) {
		warnings = append(warnings, format)
	})

	if !repaired {
		t.Error("expected repaired=true when duplicates exist")
	}
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(warnings))
	}

	// The duplicate in message 3 should have a new ID
	msg3 := result[3]
	if msg3.Content[1].ID == "toolu_1" {
		t.Error("duplicate tool_use ID was not rewritten")
	}
	newID := msg3.Content[1].ID
	if newID == "" {
		t.Fatal("rewritten ID is empty")
	}

	// The non-duplicate in message 3 should be unchanged
	if msg3.Content[2].ID != "toolu_3" {
		t.Errorf("non-duplicate ID was changed: %s", msg3.Content[2].ID)
	}

	// The tool_result in message 4 should reference the new ID
	msg4 := result[4]
	if msg4.Content[0].ToolUseID != newID {
		t.Errorf("tool_result not rewritten: got %s, want %s", msg4.Content[0].ToolUseID, newID)
	}
	// The other tool_result should be unchanged
	if msg4.Content[1].ToolUseID != "toolu_3" {
		t.Errorf("non-duplicate tool_result changed: %s", msg4.Content[1].ToolUseID)
	}

	// Original message 1 should be unchanged
	if result[1].Content[0].ID != "toolu_1" {
		t.Error("first occurrence of toolu_1 was changed")
	}
	// Original tool_result should be unchanged
	if result[2].Content[0].ToolUseID != "toolu_1" {
		t.Error("first tool_result was changed")
	}
}

func TestRepairDuplicateToolUseIDs_EmptyMessages(t *testing.T) {
	// Empty messages should be handled gracefully.
	result, repaired := repairDuplicateToolUseIDs(nil, func(format string, args ...any) {
		t.Error("unexpected warning")
	})
	if repaired {
		t.Error("expected repaired=false for nil input")
	}
	if result != nil {
		t.Error("expected nil for nil input")
	}
}
