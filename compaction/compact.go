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
	client     *anthropic.Client
	sessions   *session.Store
	model      string
	threshold  float64 // fraction of context window (e.g. 0.8)
	Scratchpad *memory.Scratchpad // nil disables scratchpad injection
}

// NewCompactor creates a new Compactor.
func NewCompactor(client *anthropic.Client, sessions *session.Store, model string, threshold float64) *Compactor {
	return &Compactor{
		client:    client,
		sessions:  sessions,
		model:     model,
		threshold: threshold,
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

	// Use actual usage if available
	if lastUsage != nil {
		totalInput := lastUsage.InputTokens + lastUsage.CacheReadInputTokens + lastUsage.CacheCreationInputTokens
		return totalInput > threshold
	}

	// Fall back to estimate
	return estimateTokens(messages) > threshold
}

// Compact summarizes a session's history and replaces it.
func (c *Compactor) Compact(ctx context.Context, sessionKey string, system []anthropic.SystemBlock) error {
	messages, err := c.sessions.LoadFull(sessionKey)
	if err != nil {
		return fmt.Errorf("load session for compaction: %w", err)
	}

	if len(messages) < 4 {
		return nil // not enough to compact
	}

	log.Infof("compaction", "compacting session %s (%d messages)", sessionKey, len(messages))

	// Ask model to summarize the conversation
	summaryMessages := make([]anthropic.Message, len(messages))
	copy(summaryMessages, messages)
	summaryMessages = append(summaryMessages, anthropic.Message{
		Role:    "user",
		Content: anthropic.TextContent("Please provide a concise summary of our entire conversation so far, capturing all key decisions, context, and important details. This summary will replace the conversation history."),
	})

	resp, err := c.client.SendMessage(ctx, &anthropic.MessageRequest{
		Model:     c.model,
		MaxTokens: 4096,
		System:    system,
		Messages:  summaryMessages,
	})
	if err != nil {
		return fmt.Errorf("summarize for compaction: %w", err)
	}

	summary := anthropic.TextOf(resp.Content)

	// Collect scratchpad contents to preserve through compaction
	handoff := "[Compaction complete. The conversation continues from here. You have full access to your tools and memory.]"
	if c.Scratchpad != nil {
		if entries, err := c.Scratchpad.All(); err == nil && len(entries) > 0 {
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
		return fmt.Errorf("replace session after compaction: %w", err)
	}

	log.Infof("compaction", "session %s compacted from %d messages to %d", sessionKey, len(messages), len(compacted))
	return nil
}
