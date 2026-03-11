package agent

import (
	"fmt"

	"foci/internal/provider"
)

// repairInterruptedToolCalls checks if the last message in the history is an
// assistant message with tool_use blocks that have no following tool_result.
// This happens when SIGTERM kills the process during tool execution — the defer
// flushes the assistant message but no tool_result was ever created.
// Returns a synthetic tool_result message to append, or nil if no repair needed.
func repairInterruptedToolCalls(messages []provider.Message) *provider.Message {
	if len(messages) == 0 {
		return nil
	}
	last := messages[len(messages)-1]
	if last.Role != "assistant" {
		return nil
	}

	var toolUseIDs []string
	for _, block := range last.Content {
		if block.Type == "tool_use" {
			toolUseIDs = append(toolUseIDs, block.ID)
		}
	}
	if len(toolUseIDs) == 0 {
		return nil
	}

	var results []provider.ContentBlock
	for _, id := range toolUseIDs {
		results = append(results, provider.ToolResultBlock(id, "Tool call interrupted by service restart", true))
	}
	return &provider.Message{Role: "user", Content: results}
}

// repairDuplicateToolUseIDs scans all messages for tool_use blocks with duplicate IDs.
// When found, later occurrences get their ID rewritten to a unique suffix variant,
// and a warning is logged. Returns (repairedMessages, true) if repairs were made.
// The Anthropic API rejects requests with duplicate tool_use IDs (400 error).
func repairDuplicateToolUseIDs(messages []provider.Message, logger func(string, ...any)) ([]provider.Message, bool) {
	// First pass: collect all tool_use IDs and find duplicates
	seen := make(map[string]bool)
	hasDuplicates := false
	for _, msg := range messages {
		if msg.Role != "assistant" {
			continue
		}
		for _, block := range msg.Content {
			if block.Type != "tool_use" {
				continue
			}
			if seen[block.ID] {
				hasDuplicates = true
				break
			}
			seen[block.ID] = true
		}
		if hasDuplicates {
			break
		}
	}
	if !hasDuplicates {
		return messages, false
	}

	// Two sub-passes: first rewrite all tool_use IDs in assistant messages,
	// then rewrite all tool_result IDs in user messages using the complete
	// rewrite map. This handles both cross-message duplicates (where the
	// original tool_result precedes the duplicate tool_use) and same-message
	// duplicates (Gemini emitting all tool_use blocks with the same ID).
	seen = make(map[string]bool)
	rewrites := make(map[string][]string) // oldID → queue of newIDs
	suffix := 0
	result := make([]provider.Message, len(messages))

	// Sub-pass 1: rewrite duplicate tool_use IDs in assistant messages.
	for i, msg := range messages {
		if msg.Role != "assistant" {
			result[i] = msg
			continue
		}
		newContent := make([]provider.ContentBlock, len(msg.Content))
		copy(newContent, msg.Content)
		for j, block := range newContent {
			if block.Type != "tool_use" {
				continue
			}
			if seen[block.ID] {
				suffix++
				newID := fmt.Sprintf("%s_dedup%d", block.ID, suffix)
				logger("repaired duplicate tool_use ID %s → %s at message %d", block.ID, newID, i)
				rewrites[block.ID] = append(rewrites[block.ID], newID)
				newContent[j].ID = newID
			} else {
				seen[block.ID] = true
			}
		}
		result[i] = provider.Message{Role: msg.Role, Content: newContent}
	}

	// Sub-pass 2: rewrite tool_result IDs in user messages.
	// For each original ID, one tool_use kept the original ID — the first
	// tool_result we encounter (across all messages) keeps it too.
	// Subsequent tool_results pop dedup IDs from the queue.
	originalPaired := make(map[string]bool)
	for i, msg := range result {
		if msg.Role != "user" {
			continue
		}
		needsCopy := false
		for _, block := range msg.Content {
			if block.Type == "tool_result" {
				if queue, ok := rewrites[block.ToolUseID]; ok && len(queue) > 0 {
					needsCopy = true
					break
				}
			}
		}
		if !needsCopy {
			continue
		}
		newContent := make([]provider.ContentBlock, len(msg.Content))
		copy(newContent, msg.Content)
		for j, block := range newContent {
			if block.Type != "tool_result" {
				continue
			}
			queue, ok := rewrites[block.ToolUseID]
			if !ok || len(queue) == 0 {
				continue
			}
			if !originalPaired[block.ToolUseID] {
				originalPaired[block.ToolUseID] = true
				continue
			}
			newContent[j].ToolUseID = queue[0]
			rewrites[block.ToolUseID] = queue[1:]
		}
		result[i] = provider.Message{Role: msg.Role, Content: newContent}
	}

	return result, true
}

// sanitizeEmptyTextBlocks removes empty text content blocks from messages.
// Both Anthropic and Gemini APIs reject requests containing text blocks with empty text.
// This can happen from corrupted sessions or API responses that returned empty content.
func sanitizeEmptyTextBlocks(messages []provider.Message) []provider.Message {
	for i, msg := range messages {
		hasEmpty := false
		for _, block := range msg.Content {
			if block.Type == "text" && block.Text == "" {
				hasEmpty = true
				break
			}
		}
		if !hasEmpty {
			continue
		}
		var filtered []provider.ContentBlock
		for _, block := range msg.Content {
			if block.Type == "text" && block.Text == "" {
				continue
			}
			filtered = append(filtered, block)
		}
		if len(filtered) == 0 {
			// Replace entirely empty message with placeholder
			filtered = provider.TextContent("(empty)")
		}
		messages[i].Content = filtered
	}
	return messages
}
