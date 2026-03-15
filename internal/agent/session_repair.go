package agent

import (
	"fmt"

	msgutil "foci/internal/messages"
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

	toolUseIDs := msgutil.ToolUseIDs(last)
	if len(toolUseIDs) == 0 {
		return nil
	}

	var results []provider.ContentBlock
	for _, id := range toolUseIDs {
		results = append(results, provider.ToolResultBlock(id, "Tool call interrupted", true))
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
	type dedupEntry struct {
		newID  string
		msgIdx int // assistant message index (for orphan injection in Phase 3)
	}
	rewrites := make(map[string][]dedupEntry) // origID → queue of renames
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
				rewrites[block.ID] = append(rewrites[block.ID], dedupEntry{newID, i})
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
	seenResult = make(map[string]bool)
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
			if !seenResult[block.ToolUseID] {
				// First tool_result for this tool_use_id — keep as-is.
				seenResult[block.ToolUseID] = true
				filtered = append(filtered, block)
				continue
			}
			// Duplicate tool_result. If there's a renamed tool_use waiting,
			// rewrite the ID to match it; otherwise drop entirely.
			origID := block.ToolUseID
			queue := rewrites[origID]
			if len(queue) > 0 {
				block.ToolUseID = queue[0].newID
				rewrites[origID] = queue[1:]
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

	// --- Phase 3: synthesize tool_results for orphaned dedup IDs ---
	// Any entries remaining in rewrites had no tool_result to pair with.
	// Group by assistant message index and inject synthetic results.
	orphansByMsg := make(map[int][]string) // msg index → orphan IDs
	for _, queue := range rewrites {
		for _, entry := range queue {
			orphansByMsg[entry.msgIdx] = append(orphansByMsg[entry.msgIdx], entry.newID)
		}
	}
	// Process in reverse so slice insertions don't shift indices.
	for i := len(result) - 1; i >= 0; i-- {
		ids := orphansByMsg[i]
		if len(ids) == 0 {
			continue
		}
		var synth []provider.ContentBlock
		for _, id := range ids {
			logger("synthesized tool_result for orphaned tool_use %s after message %d", id, i)
			synth = append(synth, provider.ToolResultBlock(id, "Tool call result unavailable (session corruption repair)", true))
		}
		if i+1 < len(result) && result[i+1].Role == "user" {
			result[i+1] = provider.Message{
				Role:    "user",
				Content: append(result[i+1].Content, synth...),
			}
		} else {
			newMsg := provider.Message{Role: "user", Content: synth}
			result = append(result[:i+1], append([]provider.Message{newMsg}, result[i+1:]...)...)
		}
	}

	return result, true
}

// stripUnmatchedToolUse removes tool_use blocks from an assistant message's
// content when no matching tool_result exists in the results. This is used
// after a steer interruption: the model requested N tool calls but only some
// executed before the user redirected. Rather than leaving unexecuted tool_use
// blocks (which confuse the model into retrying), we rewrite history so it
// looks like the model only requested the tools that actually ran.
//
// Preserves text, thinking, and other non-tool_use blocks. If all content
// blocks are stripped, inserts a "(interrupted)" text placeholder so the
// assistant message isn't empty.
//
// Returns (filtered, true) if any blocks were stripped; (original, false) otherwise.
func stripUnmatchedToolUse(assistantContent []provider.ContentBlock, toolResults []provider.ContentBlock) ([]provider.ContentBlock, bool) {
	// Build set of tool_use IDs that have matching tool_results.
	matchedIDs := make(map[string]bool)
	for _, block := range toolResults {
		if block.Type == "tool_result" {
			matchedIDs[block.ToolUseID] = true
		}
	}

	// Check if any tool_use blocks lack a matching result.
	hasUnmatched := false
	for _, block := range assistantContent {
		if block.Type == "tool_use" && !matchedIDs[block.ID] {
			hasUnmatched = true
			break
		}
	}
	if !hasUnmatched {
		return assistantContent, false
	}

	// Filter out unmatched tool_use blocks.
	var filtered []provider.ContentBlock
	for _, block := range assistantContent {
		if block.Type == "tool_use" && !matchedIDs[block.ID] {
			continue
		}
		filtered = append(filtered, block)
	}
	if len(filtered) == 0 {
		filtered = provider.TextContent("(interrupted)")
	}
	return filtered, true
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
