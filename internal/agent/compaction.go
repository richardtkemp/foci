package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/provider"
	"foci/prompts"
)

// maybeCompact checks whether context compaction is needed and performs it.
// Supports idle-aware pressure and mana-refresh compaction modes.
func (a *Agent) maybeCompact(ctx context.Context, sessionKey string, messages []provider.Message, system []provider.SystemBlock, usage *provider.Usage, sm *sessionMeta) {
	if a.Compactor == nil {
		return
	}

	totalTokens := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	effectiveModel := a.SessionModel(sessionKey)
	contextLimit := compaction.ContextLimit(effectiveModel)

	// Calculate idle duration from session metadata
	var idleDuration time.Duration
	if !sm.lastMessageTime.IsZero() {
		idleDuration = time.Since(sm.lastMessageTime)
	}

	// Get mana reset time (if available)
	var manaResetsAt time.Time
	usageClient := a.sessionUsageClient(sessionKey)
	if usageClient != nil {
		if usageResp, err := usageClient.GetUsage(ctx); err == nil && usageResp.FiveHour != nil && usageResp.FiveHour.ResetsAt != nil {
			manaResetsAt, _ = time.Parse(time.RFC3339Nano, *usageResp.FiveHour.ResetsAt)
		}
	}

	// Parse idle threshold (with "0" special case for disable)
	idleThreshold, err := time.ParseDuration(a.CompactionIdleThreshold)
	if err != nil || a.CompactionIdleThreshold == "0" {
		idleThreshold = 0 // disabled
	}

	// Parse mana refresh threshold
	manaRefreshThreshold := parseDurationFallback(a.CompactionManaRefreshThreshold, 15*time.Minute)

	// Get pressure-adjusted threshold
	adjustedThreshold := a.Compactor.Threshold()
	isManaRefresh := false

	if idleThreshold > 0 {
		adjustedThreshold, isManaRefresh = compaction.CalculateIdlePressure(
			a.Compactor.Threshold(),
			idleDuration,
			idleThreshold,
			a.CompactionIdlePressureStart,
			a.CompactionIdlePressureMax,
			manaResetsAt,
			manaRefreshThreshold,
			totalTokens,
			contextLimit,
		)
	}

	// Check if we should compact with adjusted threshold
	triggerPoint := int(float64(contextLimit) * adjustedThreshold)
	shouldCompact := totalTokens > triggerPoint

	// Fall back to standard ShouldCompact if idle pressure didn't trigger
	if !shouldCompact && !isManaRefresh {
		if !a.Compactor.ShouldCompactWithLimit(sessionKey, messages, usage, contextLimit) {
			return
		}
	} else if !shouldCompact {
		return
	}

	if a.SessionNoCompact(sessionKey) {
		percent := int(float64(totalTokens) / float64(contextLimit) * 100)
		a.logger().Infof("session=%s context at %d%% capacity for no_compact session", sessionKey, percent)
		return
	}

	// Log compaction reason
	if isManaRefresh {
		untilReset := time.Until(manaResetsAt).Round(time.Minute)
		a.logger().Infof("session=%s mana-refresh compaction (reset in %s, %d/%d tokens)",
			sessionKey, untilReset, totalTokens, contextLimit)
	} else if idleDuration > idleThreshold && idleThreshold > 0 {
		a.logger().Infof("session=%s idle compaction (idle %s, threshold %.1f%%, %d/%d tokens)",
			sessionKey, idleDuration.Round(time.Minute), adjustedThreshold*100, totalTokens, contextLimit)
	}

	// Mana-refresh mode: preserve more messages than normal compaction.
	// Normal compaction preserves a fixed count (default 25). Mana-refresh preserves
	// a larger fraction of messages because it triggers proactively (at ~40% context
	// fill), not under pressure — the goal is context reduction while retaining more
	// recent conversation.
	//
	// Priority: explicit *int count > percentage-based > normal preserve count.
	if isManaRefresh {
		oldPreserve := a.Compactor.PreserveMessages()
		defer a.Compactor.SetPreserveMessages(oldPreserve)

		if a.CompactionManaRefreshPreserve != nil {
			// Explicit message count configured — use it directly.
			a.Compactor.SetPreserveMessages(*a.CompactionManaRefreshPreserve)
		} else {
			// Percentage-based: preserve CompactionManaRefreshPreservePct of messages
			// (default 0.5 = 50%). This ensures meaningful summarisation of older messages
			// while keeping the recent half of the conversation intact.
			pct := a.CompactionManaRefreshPreservePct
			if pct <= 0 || pct > 1.0 {
				pct = 0.5
			}
			preserveN := int(float64(len(messages)) * pct)
			a.Compactor.SetPreserveMessages(preserveN)
		}
	}

	oldCount := len(messages)
	for _, fn := range a.CompactionMemoryFunc {
		fn(sessionKey)
	}
	for _, fn := range a.CompactionStartFunc {
		fn(sessionKey, "⏳ Compacting context...")
	}
	compactClient, compactModel, compactFormat := a.ResolveCallSite(config.CallCompaction, sessionKey)
	summaryPrompt := prompts.ResolvePrompt(a.CompactionSummaryPromptPath, "compaction-summary.md", prompts.CompactionSummary(), a.PromptSearchDirs...)
	handoffMsg := a.CompactionHandoffMsg
	if handoffMsg == "" {
		handoffMsg = prompts.ResolvePrompt("", "compaction-handoff.md", prompts.CompactionHandoff(), a.PromptSearchDirs...)
	}
	summary, newKey, err := a.Compactor.Compact(ctx, compactClient, sessionKey, compactModel, compactFormat, system, summaryPrompt, handoffMsg, false)
	if err != nil {
		a.logger().Errorf("session=%s compaction failed: %v", sessionKey, err)
	} else {
		if newKey != "" {
			a.RotateSession(sessionKey, newKey)
			a.logger().Infof("session=%s compaction rotated → %s (pre_messages=%d)", sessionKey, newKey, oldCount)
		}
		for _, fn := range a.CompactionNotifyFunc {
			fn(sessionKey, fmt.Sprintf("✅ Context compacted — %d messages summarised.", oldCount))
		}
		if summary != "" {
			for _, fn := range a.CompactionDebugFunc {
				fn(sessionKey, summary)
			}
		}
	}
	// Reload system prompt — compaction may have changed memory files.
	// Only invalidate THIS session's cached system blocks so other sessions
	// keep their byte-identical prompts and don't suffer cache busts.
	a.Bootstrap.Reload()
	if a.NudgeReloadFunc != nil {
		a.NudgeReloadFunc()
	}
	sm.systemBlocks = nil
	// Reset cache baseline — next request will have a different prefix
	sm.prevCacheRead = 0
}

// parseDurationFallback parses a Go duration string, returning fallback on error or empty.
func parseDurationFallback(s string, fallback time.Duration) time.Duration {
	if s == "" || s == "0" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

// sessionUsageClient returns the usage client for a session (per-session override or agent default).
func (a *Agent) sessionUsageClient(sessionKey string) provider.UsageClient {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	uc := sm.usageClient
	a.metaMu.Unlock()
	if uc != nil {
		return uc
	}
	return a.UsageClient
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
