package compaction

import (
	"context"
	"fmt"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/memory"
	"foci/internal/messages"
	"foci/internal/modelcaps"
	"foci/internal/modelinfo"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/shared/prompts"
)

// Compactor handles session compaction when context gets too large.
type Compactor struct {
	log              *log.ComponentLogger
	sessions         *session.Store
	threshold        float64 // fraction of context window (e.g. 0.8)
	maxTokens        int
	minMessages      int
	preserveMessages int                                       // preserve last N messages through compaction (0 disables)
	ModelMetaFn      func(model string) modelinfo.ModelMeta    // per-model meta from config (context window)
	ModelCapsFn      func(model string) (modelcaps.Caps, bool) // live caps from this agent's backend record; nil disables
	ModelDefaultsFn  func(model string) config.ModelDefaults   // per-model defaults from [models.*] config
	Scratchpad       *memory.Scratchpad                        // nil disables scratchpad injection
	TaskListStore    *memory.TaskListStore                     // nil disables task list injection
	AgentID          string                                    // agent ID for per-agent store queries
	FallbackFunc     provider.FallbackFunc                     // nil disables automatic model fallback
	ClientProvider   provider.ClientProvider                   // resolves clients for fallback models; nil = reuse caller's client
}

// NewCompactor creates a new Compactor with defaults.
func NewCompactor(sessions *session.Store, threshold float64) *Compactor {
	return &Compactor{
		log:         log.NewComponentLogger("compaction"),
		sessions:    sessions,
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
	return c
}

// SetLogger replaces the component logger (e.g. after AgentID is known).
func (c *Compactor) SetLogger(l *log.ComponentLogger) { c.log = l }

// ContextLimit returns the context window for a model, preferring the
// config-defined value (via ModelMetaFn) over the modelinfo registry default.
func (c *Compactor) ContextLimit(model string) int {
	if c.ModelMetaFn != nil {
		if meta := c.ModelMetaFn(model); meta.ContextWindow > 0 {
			return meta.ContextWindow
		}
	}
	if c.ModelCapsFn != nil {
		if mc, ok := c.ModelCapsFn(model); ok && mc.ContextWindow > 0 {
			return mc.ContextWindow
		}
	}
	return modelinfo.ContextWindow(model)
}

// Threshold returns the base compaction threshold.
func (c *Compactor) Threshold() float64 {
	return c.threshold
}

// PreserveMessages returns the current preserve messages count.
func (c *Compactor) PreserveMessages() int {
	return c.preserveMessages
}

// SetPreserveMessages sets the preserve messages count.
func (c *Compactor) SetPreserveMessages(n int) {
	c.preserveMessages = n
}

// ManaResetImminent returns true if the mana reset time is in the future
// and within the given threshold duration.
func ManaResetImminent(manaResetsAt time.Time, threshold time.Duration) bool {
	if manaResetsAt.IsZero() || threshold <= 0 {
		return false
	}
	untilReset := time.Until(manaResetsAt)
	return untilReset > 0 && untilReset < threshold
}

// safeSplitPoint adjusts splitIdx backward (up to maxWalkBack steps) so that
// tool_use/tool_result pairs are not broken across the split boundary.
// An assistant message with tool_use blocks must be followed by a user message
// with matching tool_result blocks — splitting between them creates orphans.
func safeSplitPoint(msgs []provider.Message, splitIdx, maxWalkBack int) int {
	for steps := 0; steps < maxWalkBack && splitIdx > 0; steps++ {
		prev := msgs[splitIdx-1]
		if prev.Role != "assistant" || !messages.HasToolUse(prev) {
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
func repairOrphanedToolUse(msgs []provider.Message) []provider.Message {
	result := make([]provider.Message, 0, len(msgs))
	for i := 0; i < len(msgs); i++ {
		msg := msgs[i]
		result = append(result, msg)

		if msg.Role != "assistant" || !messages.HasToolUse(msg) {
			continue
		}

		useIDs := messages.ToolUseIDs(msg)

		// Collect tool_result IDs from the next message (if it's a user message).
		var matched map[string]bool
		if i+1 < len(msgs) && msgs[i+1].Role == "user" {
			matched = messages.ToolResultIDs(msgs[i+1])
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
		if i+1 < len(msgs) && msgs[i+1].Role == "user" {
			next := msgs[i+1]
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
// model is used to determine context window size. Use ShouldCompactWithLimit
// to supply an explicit limit instead.
func (c *Compactor) ShouldCompact(model, sessionKey string, messages []provider.Message, lastUsage *provider.Usage) bool {
	return c.ShouldCompactWithLimit(sessionKey, messages, lastUsage, c.ContextLimit(model))
}

// ShouldCompactWithLimit returns true if the session likely exceeds the threshold,
// using the provided context limit instead of the compactor's model default.
func (c *Compactor) ShouldCompactWithLimit(sessionKey string, messages []provider.Message, lastUsage *provider.Usage, limit int) bool {
	threshold := int(float64(limit) * c.threshold)
	estimated := estimateTokens(messages)

	// Use actual usage if available
	if lastUsage != nil {
		input := lastUsage.InputTokens + lastUsage.CacheReadInputTokens + lastUsage.CacheCreationInputTokens
		if input > threshold {
			c.log.Infof("session=%s hit threshold: input=%d threshold=%d", sessionKey, input, threshold)
			return true
		}
		return false
	}

	if estimated > threshold {
		c.log.Infof("session=%s hit threshold: estimated=%d threshold=%d", sessionKey, estimated, threshold)
		return true
	}
	return false
}

// DefaultHandoffMessage is the default message injected after compaction.
// Loaded from prompts/compaction-handoff.md at build time.
var DefaultHandoffMessage = prompts.CompactionHandoff()

// Compact summarizes a session's history and replaces it with a rotated key.
// model and format specify the compaction model (e.g. from GroupResolver).
// summaryPrompt is read from a file at call time; if empty, compaction uses a
// minimal fallback. handoffMessage uses DefaultHandoffMessage if empty.
// When dryRun is true, the full pipeline runs (API call, summary generation)
// but the session is left unchanged. On success the session file is replaced
// in place (old content archived); the session key is unchanged.
func (c *Compactor) Compact(ctx context.Context, client provider.Client, sessionKey, model, format string, system []provider.SystemBlock, summaryPrompt, handoffMessage string, dryRun bool) (string, error) {
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

	c.log.Infof("session=%s compacting (%d messages)", sessionKey, len(messages))

	// Split messages into two groups:
	//   toSummarise: older messages sent to the summary model (only these go to the API)
	//   preserved:   recent messages appended verbatim after the summary
	//
	// The split point is: splitIdx = len(messages) - preserveN
	// Messages [0..splitIdx) are summarised; messages [splitIdx..] are preserved.
	preserveN := c.preserveMessages
	if preserveN > len(messages) {
		preserveN = len(messages)
	}
	// Ensure we still have at least minMessages to summarize — without enough
	// messages, the summary model can't produce a useful result.
	if len(messages)-preserveN < c.minMessages {
		preserveN = len(messages) - c.minMessages
	}

	toSummarise := messages
	var preserved []provider.Message
	if preserveN > 0 {
		splitIdx := len(messages) - preserveN

		// Walk the split backward to avoid breaking tool_use/tool_result pairs.
		// Cap walk-back at 10 steps — tool pairs are never more than a few messages
		// apart, and unbounded walk-back (e.g. preserveN=185) can push the split
		// all the way to the start, triggering the minMessages guard unnecessarily.
		maxWalkBack := preserveN
		if maxWalkBack > 10 {
			maxWalkBack = 10
		}
		originalSplit := splitIdx
		safeSplit := safeSplitPoint(messages, splitIdx, maxWalkBack)
		if safeSplit != splitIdx {
			c.log.Infof("split adjusted from %d to %d to preserve tool_use pairs", splitIdx, safeSplit)
		}

		// If walk-back would leave too few messages to summarize, revert to the
		// original split. Any orphaned tool_use/tool_result pairs at the boundary
		// will be repaired by repairOrphanedToolUse below. This is far better than
		// the alternative of preserving nothing and summarising the entire session.
		if safeSplit < c.minMessages {
			c.log.Warnf("walk-back would push split below minMessages (%d < %d), keeping original split at %d",
				safeSplit, c.minMessages, originalSplit)
			splitIdx = originalSplit
		} else {
			splitIdx = safeSplit
		}
		preserveN = len(messages) - splitIdx

		if preserveN > 0 {
			toSummarise = messages[:splitIdx]
			preserved = messages[splitIdx:]
			c.log.Infof("preserving %d messages through compaction", preserveN)
		}
	}

	// Repair any orphaned tool_use blocks in toSummarise before sending to API.
	// Handles both mid-session data corruption and boundary splits where walk-back
	// was reverted (see above).
	repairedSummary := repairOrphanedToolUse(toSummarise)

	// Ask model to summarize the conversation
	summaryMessages := make([]provider.Message, len(repairedSummary))
	copy(summaryMessages, repairedSummary)
	summaryMessages = append(summaryMessages, provider.Message{
		Role:    "user",
		Content: provider.TextContent(summaryPrompt),
	})

	// Apply per-model defaults from [models.*] config.
	var md config.ModelDefaults
	if c.ModelDefaultsFn != nil {
		md = c.ModelDefaultsFn(model)
	}
	mdThinking, mdEffort := md.Thinking, md.Effort

	c.log.Debugf("summary request: model=%s max_tokens=%d messages=%d effort=%s thinking=%s", model, c.maxTokens, len(summaryMessages), mdEffort, mdThinking)
	start := time.Now()
	req := &provider.MessageRequest{
		Model:     model,
		MaxTokens: c.maxTokens,
		System:    system,
		Messages:  summaryMessages,
	}
	if mdEffort != "" && mdEffort != "off" {
		req.Output = &provider.OutputConfig{Effort: mdEffort}
	}
	if mdThinking == "adaptive" {
		req.Thinking = &provider.ThinkingConfig{Type: "adaptive"}
	}

	// Use streaming for compaction (required for large sessions)
	handler := &provider.StreamHandler{}
	resp, err := provider.Send(ctx, client, req, handler,
		c.FallbackFunc, c.ClientProvider, c.log.Errorf)
	if err != nil {
		return "", fmt.Errorf("summarize for compaction: %w", err)
	}

	duration := time.Since(start)
	cost := modelinfo.Cost(model,
		resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)
	log.API(log.APIEntry{
		Timestamp:   start,
		Provider:    format,
		Session:     sessionKey,
		Model:       model,
		Input:       resp.Usage.InputTokens,
		Output:      resp.Usage.OutputTokens,
		CacheRead:   resp.Usage.CacheReadInputTokens,
		CacheWrite:  resp.Usage.CacheCreationInputTokens,
		CostUSD:     cost,
		DurationMS:  duration.Milliseconds(),
		StopReason:  resp.StopReason,
		CallType:    "compaction",
		PreMessages: len(messages),
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

	// Collect tasks to preserve through compaction
	if c.TaskListStore != nil {
		if tasks, err := c.TaskListStore.List(c.AgentID); err != nil {
			c.log.Warnf("read tasks for %s: %v", sessionKey, err)
		} else if len(tasks) > 0 {
			c.log.Infof("tasks preserved through compaction of %s", sessionKey)
			handoff += "\n\n[task list — preserved through compaction]\n"
			handoff += tools.FormatTasks(tasks)
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

	writer := c.sessions.For(sessionKey)
	if err := writer.Replace(sessionKey, compacted); err != nil {
		return "", fmt.Errorf("replace session after compaction: %w", err)
	}

	c.log.Infof("session=%s compacted from %d messages to %d (archived in place)", sessionKey, len(messages), len(compacted))
	return summary, nil
}
