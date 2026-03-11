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

// repairDuplicateToolIDs fixes two classes of session corruption:
//
//  1. Duplicate tool_use IDs: multiple tool_use blocks share the same ID
//     (e.g. Gemini synthesised IDs like "toolu_gemini_todo" for every call
//     to the same tool). Later occurrences get rewritten to unique suffixed IDs,
//     and their corresponding tool_results are updated to match.
//
//  2. Duplicate tool_results: multiple tool_result blocks reference the same
//     tool_use_id (e.g. from a defer safety-net replay after a partial write).
//     Only the first tool_result for each tool_use_id is kept; extras are dropped.
//
// Returns (repairedMessages, true) if any repairs were made.
func repairDuplicateToolIDs(messages []provider.Message, logger func(string, ...any)) ([]provider.Message, bool) {
	// Quick scan: anything to repair?
	seenUse := make(map[string]bool)
	seenResult := make(map[string]bool)
	hasDupUse, hasDupResult := false, false
	for _, msg := range messages {
		for _, block := range msg.Content {
			switch block.Type {
			case "tool_use":
				if seenUse[block.ID] {
					hasDupUse = true
				}
				seenUse[block.ID] = true
			case "tool_result":
				if seenResult[block.ToolUseID] {
					hasDupResult = true
				}
				seenResult[block.ToolUseID] = true
			}
		}
		if hasDupUse && hasDupResult {
			break
		}
	}
	if !hasDupUse && !hasDupResult {
		return messages, false
	}

	result := make([]provider.Message, len(messages))

	// --- Phase 1: deduplicate tool_use IDs ---
	seenUse = make(map[string]bool)
	rewrites := make(map[string][]string) // oldID → queue of newIDs
	suffix := 0
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
			if seenUse[block.ID] {
				suffix++
				newID := fmt.Sprintf("%s_dedup%d", block.ID, suffix)
				logger("repaired duplicate tool_use ID %s → %s at message %d", block.ID, newID, i)
				rewrites[block.ID] = append(rewrites[block.ID], newID)
				newContent[j].ID = newID
			} else {
				seenUse[block.ID] = true
			}
		}
		result[i] = provider.Message{Role: msg.Role, Content: newContent}
	}

	// --- Phase 2: rewrite + deduplicate tool_results ---
	// First tool_result for each original ID keeps it; subsequent ones either
	// get a dedup ID from the queue (if a matching tool_use was renamed) or
	// are dropped (if they're pure duplicates with no corresponding tool_use).
	pairedResult := make(map[string]bool) // tool_use_id → already has a tool_result
	for i, msg := range result {
		if msg.Role != "user" {
			continue
		}
		var filtered []provider.ContentBlock
		changed := false
		for _, block := range msg.Content {
			if block.Type != "tool_result" {
				filtered = append(filtered, block)
				continue
			}
			if !pairedResult[block.ToolUseID] {
				// First tool_result for this tool_use_id — keep as-is.
				pairedResult[block.ToolUseID] = true
				filtered = append(filtered, block)
				continue
			}
			// Duplicate tool_result. If there's a renamed tool_use waiting,
			// rewrite the ID to match it; otherwise drop entirely.
			origID := block.ToolUseID
			queue := rewrites[origID]
			if len(queue) > 0 {
				block.ToolUseID = queue[0]
				rewrites[origID] = queue[1:]
				pairedResult[block.ToolUseID] = true
				filtered = append(filtered, block)
				changed = true
			} else {
				logger("dropped duplicate tool_result for tool_use_id %s at message %d", origID, i)
				changed = true
			}
		}
		if changed {
			if len(filtered) == 0 {
				filtered = provider.TextContent("(removed duplicate tool results)")
			}
			result[i] = provider.Message{Role: msg.Role, Content: filtered}
		}
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
