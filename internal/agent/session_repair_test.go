package agent

import (
	"encoding/json"
	"fmt"
	"testing"

	"foci/internal/provider"
)

func TestRepairDuplicateToolIDs_NoDuplicates(t *testing.T) {
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
	result, repaired := repairDuplicateToolIDs(msgs, func(format string, args ...any) {
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

func TestRepairDuplicateToolIDs_DuplicateToolUse(t *testing.T) {
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
	result, repaired := repairDuplicateToolIDs(msgs, func(format string, args ...any) {
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

func TestRepairDuplicateToolIDs_DuplicateToolResults(t *testing.T) {
	// Duplicate tool_results without duplicate tool_uses: happens when the defer
	// safety-net replays messages that were already saved to disk.
	// One tool_use with ID "toolu_1", but two tool_results reference it.
	// The second tool_result should be dropped.
	msgs := []provider.Message{
		{Role: "assistant", Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "toolu_1", Name: "exec", Input: json.RawMessage(`{"cmd":"ls"}`)},
		}},
		{Role: "user", Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_1", Content: "file1"},
		}},
		// Replayed from defer safety-net:
		{Role: "user", Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_1", Content: "file1"},
		}},
		{Role: "assistant", Content: provider.TextContent("done")},
	}

	var warnings []string
	result, repaired := repairDuplicateToolIDs(msgs, func(format string, args ...any) {
		warnings = append(warnings, format)
	})

	if !repaired {
		t.Fatal("expected repaired=true for duplicate tool_results")
	}

	// The duplicate tool_result message should have been replaced with placeholder
	if len(result) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(result))
	}
	// Message 2 should have been cleaned (duplicate tool_result removed)
	msg2 := result[2]
	for _, block := range msg2.Content {
		if block.Type == "tool_result" && block.ToolUseID == "toolu_1" {
			t.Error("duplicate tool_result was not removed")
		}
	}

	// Original tool_result in message 1 should be kept
	if result[1].Content[0].ToolUseID != "toolu_1" {
		t.Error("original tool_result was changed")
	}
}

func TestRepairDuplicateToolIDs_DuplicateResultsSameMessage(t *testing.T) {
	// Two tool_results for the same tool_use_id within a single user message.
	// Only the first should be kept.
	msgs := []provider.Message{
		{Role: "assistant", Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "toolu_1", Name: "exec", Input: json.RawMessage(`{"cmd":"ls"}`)},
		}},
		{Role: "user", Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_1", Content: "first"},
			{Type: "tool_result", ToolUseID: "toolu_1", Content: "duplicate"},
		}},
	}

	var warnings []string
	result, repaired := repairDuplicateToolIDs(msgs, func(format string, args ...any) {
		warnings = append(warnings, format)
	})

	if !repaired {
		t.Fatal("expected repaired=true")
	}

	// Should have exactly one tool_result
	resultCount := 0
	for _, block := range result[1].Content {
		if block.Type == "tool_result" {
			resultCount++
			if block.Content != "first" {
				t.Errorf("kept wrong tool_result: %s", block.Content)
			}
		}
	}
	if resultCount != 1 {
		t.Errorf("expected 1 tool_result, got %d", resultCount)
	}
}

func TestSanitizeEmptyTextBlocks(t *testing.T) {
	// Empty text blocks should be removed; messages with only empty text
	// blocks get a placeholder.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("hello")},
		{Role: "assistant", Content: []provider.ContentBlock{
			{Type: "text", Text: ""},
		}},
		{Role: "assistant", Content: []provider.ContentBlock{
			{Type: "text", Text: "real text"},
			{Type: "text", Text: ""},
		}},
		{Role: "user", Content: provider.TextContent("bye")},
	}

	result := sanitizeEmptyTextBlocks(msgs)

	// Message 1 (assistant): was entirely empty text → should have placeholder
	if len(result[1].Content) != 1 || result[1].Content[0].Text != "(empty)" {
		t.Errorf("all-empty message should get placeholder, got %+v", result[1].Content)
	}

	// Message 2 (assistant): had one real + one empty → empty removed, real kept
	if len(result[2].Content) != 1 || result[2].Content[0].Text != "real text" {
		t.Errorf("mixed message should keep real text, got %+v", result[2].Content)
	}

	// Messages 0 and 3 should be unchanged
	if result[0].Content[0].Text != "hello" {
		t.Error("non-empty message 0 was changed")
	}
	if result[3].Content[0].Text != "bye" {
		t.Error("non-empty message 3 was changed")
	}
}

func TestRepairDuplicateToolIDs_SameMessageBatch(t *testing.T) {
	// Gemini emits ALL tool_use blocks with the same ID in a single message.
	// All 4 should be deduped: first keeps original, rest get dedup suffixes.
	// The corresponding 4 tool_results must be rewritten to match.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("add todos")},
		{Role: "assistant", Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "toolu_gemini_todo", Name: "todo", Input: json.RawMessage(`{"text":"a"}`)},
			{Type: "tool_use", ID: "toolu_gemini_todo", Name: "todo", Input: json.RawMessage(`{"text":"b"}`)},
			{Type: "tool_use", ID: "toolu_gemini_todo", Name: "todo", Input: json.RawMessage(`{"text":"c"}`)},
			{Type: "tool_use", ID: "toolu_gemini_todo", Name: "todo", Input: json.RawMessage(`{"text":"d"}`)},
		}},
		{Role: "user", Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_gemini_todo", Content: "Added #1"},
			{Type: "tool_result", ToolUseID: "toolu_gemini_todo", Content: "Added #2"},
			{Type: "tool_result", ToolUseID: "toolu_gemini_todo", Content: "Added #3"},
			{Type: "tool_result", ToolUseID: "toolu_gemini_todo", Content: "Added #4"},
		}},
	}

	var warnings []string
	result, repaired := repairDuplicateToolIDs(msgs, func(format string, args ...any) {
		warnings = append(warnings, format)
	})

	if !repaired {
		t.Fatal("expected repaired=true")
	}
	if len(warnings) != 3 {
		t.Fatalf("expected 3 warnings (3 duplicates renamed), got %d", len(warnings))
	}

	// Collect tool_use IDs from assistant message
	var toolUseIDs []string
	for _, block := range result[1].Content {
		if block.Type == "tool_use" {
			toolUseIDs = append(toolUseIDs, block.ID)
		}
	}
	// First should keep original, rest should be unique
	if toolUseIDs[0] != "toolu_gemini_todo" {
		t.Errorf("first tool_use should keep original ID, got %s", toolUseIDs[0])
	}

	// Collect tool_result IDs from user message
	var toolResultIDs []string
	for _, block := range result[2].Content {
		if block.Type == "tool_result" {
			toolResultIDs = append(toolResultIDs, block.ToolUseID)
		}
	}

	// Every tool_use ID must have exactly one matching tool_result
	useSet := make(map[string]int)
	for _, id := range toolUseIDs {
		useSet[id]++
	}
	resultSet := make(map[string]int)
	for _, id := range toolResultIDs {
		resultSet[id]++
	}
	// All IDs should be unique
	for id, count := range useSet {
		if count != 1 {
			t.Errorf("tool_use ID %s appears %d times (should be 1)", id, count)
		}
	}
	for id, count := range resultSet {
		if count != 1 {
			t.Errorf("tool_result ID %s appears %d times (should be 1)", id, count)
		}
	}
	// Every tool_use ID should have a matching tool_result
	for _, id := range toolUseIDs {
		if resultSet[id] != 1 {
			t.Errorf("tool_use %s has no matching tool_result", id)
		}
	}
}

func TestRepairDuplicateToolIDs_OrphanedDedupUse(t *testing.T) {
	// More duplicate tool_use blocks than tool_results (corruption from partial
	// defer replay that wrote assistant messages but not tool_results).
	// Phase 3 should synthesize missing tool_results for orphaned dedup IDs.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("do stuff")},
		// First turn — normal
		{Role: "assistant", Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "toolu_1", Name: "exec", Input: json.RawMessage(`{"cmd":"a"}`)},
		}},
		{Role: "user", Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_1", Content: "result a"},
		}},
		// Second turn — duplicate tool_use from defer replay, no tool_result
		{Role: "assistant", Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "toolu_1", Name: "exec", Input: json.RawMessage(`{"cmd":"b"}`)},
		}},
		// Third turn continues normally
		{Role: "assistant", Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "toolu_2", Name: "read", Input: json.RawMessage(`{}`)},
		}},
		{Role: "user", Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_2", Content: "result c"},
		}},
	}

	var warnings []string
	result, repaired := repairDuplicateToolIDs(msgs, func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	})

	if !repaired {
		t.Fatal("expected repaired=true")
	}

	// Collect all tool_use IDs and tool_result IDs
	allUseIDs := make(map[string]bool)
	allResultIDs := make(map[string]bool)
	for _, msg := range result {
		for _, block := range msg.Content {
			switch block.Type {
			case "tool_use":
				allUseIDs[block.ID] = true
			case "tool_result":
				allResultIDs[block.ToolUseID] = true
			}
		}
	}

	// Every tool_use must have a matching tool_result
	for id := range allUseIDs {
		if !allResultIDs[id] {
			t.Errorf("tool_use %s has no matching tool_result", id)
		}
	}
}

func TestRepairDuplicateToolIDs_EmptyMessages(t *testing.T) {
	// Empty messages should be handled gracefully.
	result, repaired := repairDuplicateToolIDs(nil, func(format string, args ...any) {
		t.Error("unexpected warning")
	})
	if repaired {
		t.Error("expected repaired=false for nil input")
	}
	if result != nil {
		t.Error("expected nil for nil input")
	}
}
