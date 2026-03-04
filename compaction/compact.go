package compaction

import (
	"context"
	"fmt"
	"strings"
	"time"

	"foci/log"
	"foci/memory"
	"foci/prompts"
	"foci/provider"
	"foci/session"
)

// Compactor handles session compaction when context gets too large.
type Compactor struct {
	log              *log.ComponentLogger
	sessions         *session.Store
	model            string
	threshold        float64 // fraction of context window (e.g. 0.8)
	maxTokens        int
	minMessages      int
	preserveMessages int                // preserve last N messages through compaction (0 disables)
	effort           string             // effort level for compaction API call (empty = omit)
	Scratchpad       *memory.Scratchpad // nil disables scratchpad injection
	AgentID          string             // agent ID for per-agent scratchpad queries
}

// NewCompactor creates a new Compactor with defaults.
func NewCompactor(sessions *session.Store, model string, threshold float64) *Compactor {
	return &Compactor{
		log:         log.NewComponentLogger("compaction"),
		sessions:    sessions,
		model:       model,
		threshold:   threshold,
		maxTokens:   4096,
		minMessages: 4,
	}
}

// WithConfig updates compactor settings from configuration.
func (c *Compactor) WithConfig(maxTokens, minMessages, preserveMessages int) *Compactor {
	if maxTokens > 0 {
		c.maxTokens = maxTokens
	}
	if minMessages > 0 {
		c.minMessages = minMessages
	}
	if preserveMessages >= 0 {
		c.preserveMessages = preserveMessages
	}
	c.checkConfig()
	return c
}

// WithEffort sets the effort level for compaction API calls.
func (c *Compactor) WithEffort(effort string) *Compactor {
	c.effort = effort
	return c
}

// SetLogger replaces the component logger (e.g. after AgentID is known).
func (c *Compactor) SetLogger(l *log.ComponentLogger) { c.log = l }

// checkConfig warns if compaction settings could exceed the context window.
func (c *Compactor) checkConfig() {
	limit := contextLimit(c.model)
	triggerPoint := int(float64(limit) * c.threshold)
	if triggerPoint+c.maxTokens > limit {
		c.log.Warnf("compaction_max_tokens (%d) + threshold trigger point (%d) exceeds context window (%d) — summary may not fit",
			c.maxTokens, triggerPoint, limit)
	}
}

// contextLimit returns the approximate context window for a model.
func contextLimit(model string) int {
	switch {
	case strings.HasPrefix(model, "claude-"):
		return 200_000
	case strings.Contains(model, "gemini-2.5-pro"),
		strings.Contains(model, "gemini-2.5-flash"):
		return 1_000_000
	case strings.Contains(model, "gemini-2.0"):
		return 1_000_000
	case strings.Contains(model, "gemini-1.5"):
		return 2_000_000
	case strings.Contains(model, "gemini-"):
		return 1_000_000
	default:
		return 200_000
	}
}

// ContextLimit returns the approximate context window for a model (exported).
func ContextLimit(model string) int {
	return contextLimit(model)
}

// hasToolUse returns true if the message contains any tool_use content blocks.
func hasToolUse(msg provider.Message) bool {
	for _, b := range msg.Content {
		if b.Type == "tool_use" {
			return true
		}
	}
	return false
}

// toolUseIDs returns the IDs of all tool_use blocks in the message.
func toolUseIDs(msg provider.Message) []string {
	var ids []string
	for _, b := range msg.Content {
		if b.Type == "tool_use" {
			ids = append(ids, b.ID)
		}
	}
	return ids
}

// toolResultIDs returns the tool_use_id values of all tool_result blocks in the message.
func toolResultIDs(msg provider.Message) map[string]bool {
	ids := make(map[string]bool)
	for _, b := range msg.Content {
		if b.Type == "tool_result" {
			ids[b.ToolUseID] = true
		}
	}
	return ids
}

// safeSplitPoint adjusts splitIdx backward (up to maxWalkBack steps) so that
// tool_use/tool_result pairs are not broken across the split boundary.
// An assistant message with tool_use blocks must be followed by a user message
// with matching tool_result blocks — splitting between them creates orphans.
func safeSplitPoint(messages []provider.Message, splitIdx, maxWalkBack int) int {
	for steps := 0; steps < maxWalkBack && splitIdx > 0; steps++ {
		prev := messages[splitIdx-1]
		if prev.Role != "assistant" || !hasToolUse(prev) {
			break
		}
		// The message before the split is an assistant with tool_use.
		// Its tool_results should be at splitIdx — pull it into preserved.
		splitIdx--
	}
	return splitIdx
}

// repairOrphanedToolUse scans messages for assistant tool_use blocks that lack
// matching tool_result blocks in the immediately following user message, and
// injects synthetic error tool_results. Returns a new slice; the input is not modified.
func repairOrphanedToolUse(messages []provider.Message) []provider.Message {
	result := make([]provider.Message, 0, len(messages))
	for i := 0; i < len(messages); i++ {
		msg := messages[i]
		result = append(result, msg)

		if msg.Role != "assistant" || !hasToolUse(msg) {
			continue
		}

		useIDs := toolUseIDs(msg)

		// Collect tool_result IDs from the next message (if it's a user message).
		var matched map[string]bool
		if i+1 < len(messages) && messages[i+1].Role == "user" {
			matched = toolResultIDs(messages[i+1])
		}

		// Find unmatched tool_use IDs.
		var unmatched []string
		for _, id := range useIDs {
			if !matched[id] {
				unmatched = append(unmatched, id)
			}
		}
		if len(unmatched) == 0 {
			continue
		}

		log.Warnf("compaction", "repairing %d orphaned tool_use blocks", len(unmatched))

		// Build synthetic tool_results.
		var synthetic []provider.ContentBlock
		for _, id := range unmatched {
			synthetic = append(synthetic, provider.ToolResultBlock(
				id, "Tool result lost (repaired during compaction)", true,
			))
		}

		// If the next message is a user message, clone it and prepend the
		// synthetic results so the pair stays together.
		if i+1 < len(messages) && messages[i+1].Role == "user" {
			next := messages[i+1]
			combined := make([]provider.ContentBlock, 0, len(synthetic)+len(next.Content))
			combined = append(combined, synthetic...)
			combined = append(combined, next.Content...)
			result = append(result, provider.Message{Role: "user", Content: combined})
			i++ // skip the original
		} else {
			// No user message follows — inject a standalone user message.
			result = append(result, provider.Message{
				Role:    "user",
				Content: synthetic,
			})
		}
	}
	return result
}

// estimateTokens gives a rough token estimate for messages.
// ~4 chars per token is a common heuristic.
func estimateTokens(messages []provider.Message) int {
	total := 0
	for _, msg := range messages {
		for _, block := range msg.Content {
			total += len(block.Text) / 4
			total += len(block.Content) / 4
		}
	}
	return total
}

// ShouldCompact returns true if the session likely exceeds the threshold.
func (c *Compactor) ShouldCompact(messages []provider.Message, lastUsage *provider.Usage) bool {
	limit := contextLimit(c.model)
	threshold := int(float64(limit) * c.threshold)
	estimated := estimateTokens(messages)

	var result bool
	var input int

	// Use actual usage if available
	if lastUsage != nil {
		input = lastUsage.InputTokens + lastUsage.CacheReadInputTokens + lastUsage.CacheCreationInputTokens
		result = input > threshold
	} else {
		input = estimated
		result = estimated > threshold
	}

	c.log.Debugf("should_compact: input=%d threshold=%d estimated=%d result=%v", input, threshold, estimated, result)
	return result
}

// DefaultHandoffMessage is the default message injected after compaction.
// Loaded from prompts/compaction-handoff.md at build time.
var DefaultHandoffMessage = prompts.CompactionHandoff()

// Compact summarizes a session's history and replaces it.
// summaryPrompt is read from a file at call time; if empty, compaction uses a
// minimal fallback. handoffMessage uses DefaultHandoffMessage if empty.
// When dryRun is true, the full pipeline runs (API call, summary generation)
// but sessions.Replace() is skipped — the session is left unchanged.
func (c *Compactor) Compact(ctx context.Context, client provider.Client, sessionKey string, system []provider.SystemBlock, summaryPrompt, handoffMessage string, dryRun bool) (string, error) {
	if summaryPrompt == "" {
		summaryPrompt = prompts.CompactionSummary()
	}
	if handoffMessage == "" {
		handoffMessage = DefaultHandoffMessage
	}

	messages, err := c.sessions.LoadFull(sessionKey)
	if err != nil {
		return "", fmt.Errorf("load session for compaction: %w", err)
	}

	if len(messages) < c.minMessages {
		return "", nil // not enough to compact
	}

	c.log.Infof("compacting session %s (%d messages)", sessionKey, len(messages))

	// Determine how many messages to preserve through compaction.
	// Preserved messages are appended verbatim after the summary.
	preserveN := c.preserveMessages
	if preserveN > len(messages) {
		preserveN = len(messages)
	}
	// Ensure we still have at least minMessages to summarize
	if len(messages)-preserveN < c.minMessages {
		preserveN = len(messages) - c.minMessages
		if preserveN < 0 {
			preserveN = 0
		}
	}

	toSummarise := messages
	var preserved []provider.Message
	if preserveN > 0 {
		splitIdx := len(messages) - preserveN

		// Walk the split backward (bounded) to avoid breaking tool_use/tool_result pairs.
		maxWalkBack := c.preserveMessages
		if maxWalkBack <= 0 {
			maxWalkBack = 25
		}
		safeSplit := safeSplitPoint(messages, splitIdx, maxWalkBack)
		if safeSplit != splitIdx {
			c.log.Infof("split adjusted from %d to %d to preserve tool_use pairs", splitIdx, safeSplit)
		}
		splitIdx = safeSplit

		// Re-check minMessages constraint after adjustment.
		if splitIdx < c.minMessages {
			c.log.Infof("walk-back pushed split below minMessages (%d < %d), preserving nothing", splitIdx, c.minMessages)
			splitIdx = len(messages)
			preserveN = 0
		} else {
			preserveN = len(messages) - splitIdx
		}

		if preserveN > 0 {
			toSummarise = messages[:splitIdx]
			preserved = messages[splitIdx:]
			c.log.Infof("preserving %d messages through compaction", preserveN)
		}
	}

	// Repair any orphaned tool_use blocks in toSummarise before sending to API.
	// This handles mid-session data corruption (missing tool_results).
	repairedSummary := repairOrphanedToolUse(toSummarise)

	// Ask model to summarize the conversation
	summaryMessages := make([]provider.Message, len(repairedSummary))
	copy(summaryMessages, repairedSummary)
	summaryMessages = append(summaryMessages, provider.Message{
		Role:    "user",
		Content: provider.TextContent(summaryPrompt),
	})

	c.log.Debugf("summary request: model=%s max_tokens=%d messages=%d effort=%s", c.model, c.maxTokens, len(summaryMessages), c.effort)
	start := time.Now()
	req := &provider.MessageRequest{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		System:    system,
		Messages:  summaryMessages,
	}
	if c.effort != "" {
		req.Output = &provider.OutputConfig{Effort: c.effort}
	}

	resp, err := provider.Send(ctx, client, req, nil)
	if err != nil {
		return "", fmt.Errorf("summarize for compaction: %w", err)
	}

	duration := time.Since(start)
	cost := log.CalculateCost(c.model,
		resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)
	log.API(log.APIEntry{
		Timestamp:  start.UTC(),
		Session:    sessionKey,
		Model:      c.model,
		Input:      resp.Usage.InputTokens,
		Output:     resp.Usage.OutputTokens,
		CacheRead:  resp.Usage.CacheReadInputTokens,
		CacheWrite: resp.Usage.CacheCreationInputTokens,
		CostUSD:    cost,
		DurationMS: duration.Milliseconds(),
		StopReason: resp.StopReason,
		CallType:   "compaction",
	})

	summary := provider.TextOf(resp.Content)

	// Collect scratchpad contents to preserve through compaction
	handoff := handoffMessage
	if c.Scratchpad != nil {
		if entries, err := c.Scratchpad.All(c.AgentID); err != nil {
			c.log.Warnf("read scratchpad for %s: %v", sessionKey, err)
		} else if len(entries) > 0 {
			c.log.Infof("scratchpad preserved: %d entries through compaction of %s", len(entries), sessionKey)
			handoff += "\n\n[scratchpad — working state preserved through compaction]"
			for _, e := range entries {
				handoff += fmt.Sprintf("\n--- %s ---\n%s", e.Key, e.Content)
			}
		}
	}

	// Append preservation note to summary if messages are being preserved
	if preserveN > 0 {
		summary += fmt.Sprintf("\n\nThe last %d messages from before compaction follow.", preserveN)
	}

	// Build compacted message sequence, ensuring role alternation.
	// The Anthropic API requires strictly alternating user/assistant roles.
	// When preserved messages start with "user", folding the handoff into the
	// assistant summary avoids consecutive user messages:
	//   [user_marker, assistant_summary+handoff, user_preserved[0], ...]
	// When preserved messages start with "assistant" (or there are none),
	// keep the standard 3-message header:
	//   [user_marker, assistant_summary, user_handoff, assistant_preserved[0], ...]
	var compacted []provider.Message
	if preserveN > 0 && preserved[0].Role == "user" {
		// Fold handoff into assistant summary to avoid user→user
		compacted = []provider.Message{
			{
				Role:    "user",
				Content: provider.TextContent("[Session compacted. Previous conversation summary follows.]"),
			},
			{
				Role:    "assistant",
				Content: provider.TextContent(summary + "\n\n" + handoff),
			},
		}
	} else {
		compacted = []provider.Message{
			{
				Role:    "user",
				Content: provider.TextContent("[Session compacted. Previous conversation summary follows.]"),
			},
			{
				Role:    "assistant",
				Content: provider.TextContent(summary),
			},
			{
				Role:    "user",
				Content: provider.TextContent(handoff),
			},
		}
	}
	compacted = append(compacted, preserved...)

	if dryRun {
		c.log.Infof("dry-run complete for %s, summary generated (%d messages would compact to %d)", sessionKey, len(messages), len(compacted))
		return summary, nil
	}

	if err := c.sessions.Replace(sessionKey, compacted); err != nil {
		return "", fmt.Errorf("replace session after compaction: %w", err)
	}

	c.log.Infof("session %s compacted from %d messages to %d", sessionKey, len(messages), len(compacted))
	return summary, nil
}
