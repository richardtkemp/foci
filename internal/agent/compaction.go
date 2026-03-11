package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"foci/internal/compaction"
	"foci/internal/provider"
	"foci/prompts"
)

// maybeCompact checks whether context compaction is needed and performs it.
func (a *Agent) maybeCompact(ctx context.Context, client provider.Client, sessionKey string, messages []provider.Message, system []provider.SystemBlock, usage *provider.Usage, sm *sessionMeta) {
	if a.Compactor == nil || a.AsyncNotifier.HasPending(sessionKey) || !a.Compactor.ShouldCompact(sessionKey, messages, usage) {
		return
	}
	if a.SessionNoCompact(sessionKey) {
		totalTokens := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
		limit := compaction.ContextLimit(a.Model)
		percent := int(float64(totalTokens) / float64(limit) * 100)
		a.logger().Infof("context at %d%% capacity for no_compact session", percent)
		return
	}
	oldCount := len(messages)
	if a.CompactionNotifyFunc != nil {
		a.CompactionNotifyFunc(sessionKey, "⏳ Compacting context...")
	}
	summaryPrompt := prompts.ResolvePrompt(a.CompactionSummaryPromptPath, "compaction-summary.md", prompts.CompactionSummary(), a.PromptSearchDirs...)
	handoffMsg := a.CompactionHandoffMsg
	if handoffMsg == "" {
		handoffMsg = prompts.ResolvePrompt("", "compaction-handoff.md", prompts.CompactionHandoff(), a.PromptSearchDirs...)
	}
	if summary, err := a.Compactor.Compact(ctx, client, sessionKey, system, summaryPrompt, handoffMsg, false); err != nil {
		a.logger().Errorf("session=%s compaction failed: %v", sessionKey, err)
	} else {
		if a.CompactionNotifyFunc != nil {
			a.CompactionNotifyFunc(sessionKey, fmt.Sprintf("✅ Context compacted — %d messages summarised.", oldCount))
		}
		if a.CompactionDebugFunc != nil && summary != "" {
			a.CompactionDebugFunc(sessionKey, summary)
		}
	}
	// Reload system prompt — compaction may have changed memory files.
	// Only invalidate THIS session's cached system blocks so other sessions
	// keep their byte-identical prompts and don't suffer cache busts.
	a.Bootstrap.Reload()
	sm.systemBlocks = nil
	// Reset cache baseline — next request will have a different prefix
	sm.prevCacheRead = 0
}

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
// and a warning is logged. Returns (messages, true) if repairs were made.
// The Anthropic API rejects requests with duplicate tool_use IDs (400 error).
func repairDuplicateToolUseIDs(messages []provider.Message, logger func(string, ...any)) []provider.Message {
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
		return messages
	}

	// Second pass: rewrite duplicates. We rebuild tool_result references too
	// so tool_use/tool_result pairs stay matched.
	seen = make(map[string]bool)
	rewrites := make(map[string]string) // oldID → newID
	suffix := 0
	result := make([]provider.Message, len(messages))

	for i, msg := range messages {
		if msg.Role == "assistant" {
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
					rewrites[block.ID] = newID
					newContent[j].ID = newID
				} else {
					seen[block.ID] = true
				}
			}
			result[i] = provider.Message{Role: msg.Role, Content: newContent}
		} else if msg.Role == "user" && len(rewrites) > 0 {
			// Rewrite tool_result references to match rewritten tool_use IDs
			newContent := make([]provider.ContentBlock, len(msg.Content))
			copy(newContent, msg.Content)
			for j, block := range newContent {
				if block.Type != "tool_result" {
					continue
				}
				if newID, ok := rewrites[block.ToolUseID]; ok {
					newContent[j].ToolUseID = newID
					delete(rewrites, block.ToolUseID) // consumed
				}
			}
			result[i] = provider.Message{Role: msg.Role, Content: newContent}
		} else {
			result[i] = msg
		}
	}

	return result
}

// summarizeServerToolResult extracts a brief text summary from a server tool result block.
// Server tool result blocks (web_search_tool_result, web_fetch_tool_result) contain
// structured data in their Raw JSON. We extract a human-readable snippet for observers.
func summarizeServerToolResult(block provider.ContentBlock) string {
	// Try to extract content from the raw JSON
	if len(block.Raw) > 0 {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(block.Raw, &raw); err == nil {
			// web_search_tool_result has a "content" array with search results
			if content, ok := raw["content"]; ok {
				var items []json.RawMessage
				if json.Unmarshal(content, &items) == nil && len(items) > 0 {
					return fmt.Sprintf("%d results", len(items))
				}
			}
		}
	}
	return block.Type
}
