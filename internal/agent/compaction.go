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
