package agent

import (
	"encoding/json"
	"fmt"
	"testing"

	"foci/internal/provider"
)

func TestRepairDuplicateToolIDs_NoDuplicates(t *testing.T) {
	// Proves that sessions with no duplicate tool IDs are returned unchanged with repaired=false.
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
	// Proves that a repeated tool_use ID is renamed in later occurrences, with matching tool_results updated.
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
	// Proves that duplicate tool_result messages (from defer replay) are collapsed, keeping only the first.
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
	// Proves that two tool_results for the same tool_use_id within one message are deduplicated to one.
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
	// Proves that empty text blocks are stripped from messages, with all-empty messages replaced by a placeholder.
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
	// Proves that Gemini-style batches where all tool_use blocks share one ID are repaired with unique IDs
	// and the corresponding tool_results are rewritten to match.
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
	// Proves that orphaned tool_use blocks (no matching tool_result due to partial replay) get synthetic results.
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
	// Proves that nil input is handled gracefully, returning nil with repaired=false.
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

func TestStripUnmatchedToolUse_MidBatch(t *testing.T) {
	// Proves that when only some tool_use blocks have matching tool_results,
	// the unmatched ones are stripped while thinking and text blocks are kept.
	content := []provider.ContentBlock{
		{Type: "thinking", Thinking: "let me run both tools"},
		{Type: "tool_use", ID: "tu_1", Name: "read", Input: json.RawMessage(`{}`)},
		{Type: "tool_use", ID: "tu_2", Name: "write", Input: json.RawMessage(`{}`)},
	}
	results := []provider.ContentBlock{
		provider.ToolResultBlock("tu_1", "ok", false),
		// tu_2 has no result — it was skipped by steer
	}

	filtered, stripped := stripUnmatchedToolUse(content, results)
	if !stripped {
		t.Fatal("expected stripped=true")
	}
	if len(filtered) != 2 {
		t.Fatalf("got %d blocks, want 2 (thinking + tu_1)", len(filtered))
	}
	if filtered[0].Type != "thinking" {
		t.Errorf("filtered[0].Type = %q, want thinking", filtered[0].Type)
	}
	if filtered[1].Type != "tool_use" || filtered[1].ID != "tu_1" {
		t.Errorf("filtered[1]: type=%q id=%q, want tool_use tu_1", filtered[1].Type, filtered[1].ID)
	}
}

func TestStripUnmatchedToolUse_AllStripped(t *testing.T) {
	// Proves that when no tool_use blocks have matching results (steer before
	// first tool), all are stripped and an "(interrupted)" placeholder is inserted.
	content := []provider.ContentBlock{
		{Type: "tool_use", ID: "tu_1", Name: "read", Input: json.RawMessage(`{}`)},
		{Type: "tool_use", ID: "tu_2", Name: "write", Input: json.RawMessage(`{}`)},
	}
	// No tool_results at all — only a steer text block
	results := []provider.ContentBlock{
		{Type: "text", Text: "[user] abort"},
	}

	filtered, stripped := stripUnmatchedToolUse(content, results)
	if !stripped {
		t.Fatal("expected stripped=true")
	}
	if len(filtered) != 1 {
		t.Fatalf("got %d blocks, want 1 (placeholder)", len(filtered))
	}
	if filtered[0].Type != "text" || filtered[0].Text != "(interrupted)" {
		t.Errorf("placeholder: type=%q text=%q", filtered[0].Type, filtered[0].Text)
	}
}

func TestRepairMissingAssistantMessages_ConsecutiveUsers(t *testing.T) {
	// Proves that two consecutive user messages get a synthetic assistant inserted between them.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("hello")},
		{Role: "assistant", Content: provider.TextContent("hi")},
		{Role: "user", Content: provider.TextContent("first")},
		{Role: "user", Content: provider.TextContent("second")}, // consecutive!
		{Role: "assistant", Content: provider.TextContent("ok")},
	}

	result, n := repairMissingAssistantMessages(msgs)
	if n != 1 {
		t.Fatalf("expected 1 repair, got %d", n)
	}
	if len(result) != 6 {
		t.Fatalf("expected 6 messages (1 inserted), got %d", len(result))
	}
	// Inserted assistant should be at index 3 (between the two user messages)
	if result[3].Role != "assistant" {
		t.Errorf("expected assistant at index 3, got %s", result[3].Role)
	}
	if result[3].Content[0].Text != "(no response recorded)" {
		t.Errorf("unexpected placeholder text: %s", result[3].Content[0].Text)
	}
	// Original messages should be preserved in order
	if result[2].Content[0].Text != "first" || result[4].Content[0].Text != "second" {
		t.Error("original user messages not preserved correctly")
	}
}

func TestRepairMissingAssistantMessages_EmptyAssistant(t *testing.T) {
	// Proves that an assistant message with zero content blocks gets a placeholder.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("hello")},
		{Role: "assistant", Content: nil}, // empty!
		{Role: "user", Content: provider.TextContent("still here?")},
		{Role: "assistant", Content: provider.TextContent("yes")},
	}

	result, n := repairMissingAssistantMessages(msgs)
	if n != 1 {
		t.Fatalf("expected 1 repair, got %d", n)
	}
	if len(result) != 4 {
		t.Fatalf("expected 4 messages (no insertions), got %d", len(result))
	}
	if result[1].Content[0].Text != "(empty response)" {
		t.Errorf("unexpected placeholder: %s", result[1].Content[0].Text)
	}
}

func TestRepairMissingAssistantMessages_MultipleConsecutive(t *testing.T) {
	// Proves that three consecutive user messages get two synthetic assistants inserted.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("a")},
		{Role: "user", Content: provider.TextContent("b")},
		{Role: "user", Content: provider.TextContent("c")},
		{Role: "assistant", Content: provider.TextContent("finally")},
	}

	result, n := repairMissingAssistantMessages(msgs)
	if n != 2 {
		t.Fatalf("expected 2 repairs, got %d", n)
	}
	if len(result) != 6 {
		t.Fatalf("expected 6 messages, got %d", len(result))
	}
	// Check alternation: user, assistant, user, assistant, user, assistant
	expected := []string{"user", "assistant", "user", "assistant", "user", "assistant"}
	for i, want := range expected {
		if result[i].Role != want {
			t.Errorf("message %d: role=%s, want %s", i, result[i].Role, want)
		}
	}
}

func TestRepairMissingAssistantMessages_CleanSession(t *testing.T) {
	// Proves that a well-formed session with proper alternation is returned unchanged.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("hello")},
		{Role: "assistant", Content: provider.TextContent("hi")},
		{Role: "user", Content: provider.TextContent("bye")},
		{Role: "assistant", Content: provider.TextContent("goodbye")},
	}

	result, n := repairMissingAssistantMessages(msgs)
	if n != 0 {
		t.Fatalf("expected 0 repairs, got %d", n)
	}
	if &result[0] != &msgs[0] {
		t.Error("expected same slice returned for clean session")
	}
}

func TestRepairMissingAssistantMessages_MixedCorruption(t *testing.T) {
	// Proves that both consecutive users and empty assistant content are repaired in one pass.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("hello")},
		{Role: "assistant", Content: nil}, // empty!
		{Role: "user", Content: provider.TextContent("retry")},
		{Role: "user", Content: provider.TextContent("again")}, // consecutive!
		{Role: "assistant", Content: provider.TextContent("ok")},
	}

	result, n := repairMissingAssistantMessages(msgs)
	if n != 2 {
		t.Fatalf("expected 2 repairs, got %d", n)
	}
	if len(result) != 6 {
		t.Fatalf("expected 6 messages, got %d", len(result))
	}
	// Empty assistant should be repaired
	if result[1].Content[0].Text != "(empty response)" {
		t.Errorf("empty assistant not repaired: %s", result[1].Content[0].Text)
	}
	// Synthetic assistant should be inserted between consecutive users
	if result[3].Role != "assistant" || result[3].Content[0].Text != "(no response recorded)" {
		t.Errorf("synthetic assistant not inserted: role=%s", result[3].Role)
	}
}

func TestStripUnmatchedToolUse_NoneStripped(t *testing.T) {
	// Proves that when all tool_use blocks have matching results, the content
	// is returned unchanged with stripped=false.
	content := []provider.ContentBlock{
		{Type: "text", Text: "I'll use two tools"},
		{Type: "tool_use", ID: "tu_1", Name: "read", Input: json.RawMessage(`{}`)},
		{Type: "tool_use", ID: "tu_2", Name: "write", Input: json.RawMessage(`{}`)},
	}
	results := []provider.ContentBlock{
		provider.ToolResultBlock("tu_1", "ok", false),
		provider.ToolResultBlock("tu_2", "done", false),
	}

	filtered, stripped := stripUnmatchedToolUse(content, results)
	if stripped {
		t.Fatal("expected stripped=false when all matched")
	}
	if len(filtered) != 3 {
		t.Fatalf("got %d blocks, want 3 (original)", len(filtered))
	}
}
