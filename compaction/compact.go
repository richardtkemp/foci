package compaction

import (
	"context"
	"fmt"

	"clod/anthropic"
	"clod/log"
	"clod/memory"
	"clod/session"
)

// Compactor handles session compaction when context gets too large.
type Compactor struct {
	client      *anthropic.Client
	sessions    *session.Store
	model       string
	threshold   float64 // fraction of context window (e.g. 0.8)
	maxTokens   int
	minMessages int
	Scratchpad  *memory.Scratchpad // nil disables scratchpad injection
	AgentID     string             // agent ID for per-agent scratchpad queries
}

// NewCompactor creates a new Compactor with defaults.
func NewCompactor(client *anthropic.Client, sessions *session.Store, model string, threshold float64) *Compactor {
	return &Compactor{
		client:      client,
		sessions:    sessions,
		model:       model,
		threshold:   threshold,
		maxTokens:   4096,
		minMessages: 4,
	}
}

// WithConfig updates compactor settings from configuration.
func (c *Compactor) WithConfig(maxTokens, minMessages int) *Compactor {
	if maxTokens > 0 {
		c.maxTokens = maxTokens
	}
	if minMessages > 0 {
		c.minMessages = minMessages
	}
	c.checkConfig()
	return c
}

// checkConfig warns if compaction settings could exceed the context window.
func (c *Compactor) checkConfig() {
	limit := contextLimit(c.model)
	triggerPoint := int(float64(limit) * c.threshold)
	if triggerPoint+c.maxTokens > limit {
		log.Warnf("compaction", "compaction_max_tokens (%d) + threshold trigger point (%d) exceeds context window (%d) — summary may not fit",
			c.maxTokens, triggerPoint, limit)
	}
}

// contextLimit returns the approximate context window for a model.
func contextLimit(model string) int {
	switch model {
	case "claude-haiku-4-5":
		return 200_000
	case "claude-sonnet-4-5":
		return 200_000
	case "claude-opus-4-6":
		return 200_000
	default:
		return 200_000
	}
}

// ContextLimit returns the approximate context window for a model (exported).
func ContextLimit(model string) int {
	return contextLimit(model)
}

// estimateTokens gives a rough token estimate for messages.
// ~4 chars per token is a common heuristic.
func estimateTokens(messages []anthropic.Message) int {
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
func (c *Compactor) ShouldCompact(messages []anthropic.Message, lastUsage *anthropic.Usage) bool {
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

	log.Debugf("compaction", "should_compact: input=%d threshold=%d estimated=%d result=%v", input, threshold, estimated, result)
	return result
}

// DefaultHandoffMessage is the default message injected after compaction.
const DefaultHandoffMessage = "[Compaction complete. The conversation continues from here. You have full access to your tools and memory.]"

// Compact summarizes a session's history and replaces it.
// summaryPrompt is read from a file at call time; if empty, compaction uses a
// minimal fallback. handoffMessage uses DefaultHandoffMessage if empty.
func (c *Compactor) Compact(ctx context.Context, sessionKey string, system []anthropic.SystemBlock, summaryPrompt, handoffMessage string) (string, error) {
	if summaryPrompt == "" {
		summaryPrompt = "Provide a concise summary of the conversation so far, capturing key decisions and context. This summary will replace the conversation history."
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

	log.Infof("compaction", "compacting session %s (%d messages)", sessionKey, len(messages))

	// Ask model to summarize the conversation
	summaryMessages := make([]anthropic.Message, len(messages))
	copy(summaryMessages, messages)
	summaryMessages = append(summaryMessages, anthropic.Message{
		Role:    "user",
		Content: anthropic.TextContent(summaryPrompt),
	})

	log.Debugf("compaction", "summary request: model=%s max_tokens=%d messages=%d", c.model, c.maxTokens, len(summaryMessages))
	resp, err := c.client.SendMessage(ctx, &anthropic.MessageRequest{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		System:    system,
		Messages:  summaryMessages,
	})
	if err != nil {
		return "", fmt.Errorf("summarize for compaction: %w", err)
	}

	summary := anthropic.TextOf(resp.Content)

	// Collect scratchpad contents to preserve through compaction
	handoff := handoffMessage
	if c.Scratchpad != nil {
		if entries, err := c.Scratchpad.All(c.AgentID); err != nil {
			log.Warnf("compaction", "read scratchpad for %s: %v", sessionKey, err)
		} else if len(entries) > 0 {
			log.Infof("compaction", "scratchpad preserved: %d entries through compaction of %s", len(entries), sessionKey)
			handoff += "\n\n[scratchpad — working state preserved through compaction]"
			for _, e := range entries {
				handoff += fmt.Sprintf("\n--- %s ---\n%s", e.Key, e.Content)
			}
		}
	}

	// Replace session with summary + handoff note
	compacted := []anthropic.Message{
		{
			Role:    "user",
			Content: anthropic.TextContent("[Session compacted. Previous conversation summary follows.]"),
		},
		{
			Role:    "assistant",
			Content: anthropic.TextContent(summary),
		},
		{
			Role:    "user",
			Content: anthropic.TextContent(handoff),
		},
	}

	if err := c.sessions.Replace(sessionKey, compacted); err != nil {
		return "", fmt.Errorf("replace session after compaction: %w", err)
	}

	log.Infof("compaction", "session %s compacted from %d messages to %d", sessionKey, len(messages), len(compacted))
	return summary, nil
}
