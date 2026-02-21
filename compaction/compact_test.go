package compaction

import (
	"testing"

	"clod/anthropic"
)

func TestEstimateTokens(t *testing.T) {
	msgs := []anthropic.Message{
		{Role: "user", Content: anthropic.TextContent("hello world")},      // 11 chars / 4 = 2
		{Role: "assistant", Content: anthropic.TextContent("hi there!")},    // 9 chars / 4 = 2
	}

	tokens := estimateTokens(msgs)
	if tokens < 2 {
		t.Errorf("estimateTokens = %d, expected >= 2", tokens)
	}
}

func TestEstimateTokensEmpty(t *testing.T) {
	tokens := estimateTokens(nil)
	if tokens != 0 {
		t.Errorf("estimateTokens(nil) = %d, want 0", tokens)
	}
}

func TestContextLimit(t *testing.T) {
	models := []string{"claude-haiku-4-5", "claude-sonnet-4-5", "claude-opus-4-6", "unknown-model"}
	for _, model := range models {
		limit := contextLimit(model)
		if limit != 200_000 {
			t.Errorf("contextLimit(%q) = %d, want 200000", model, limit)
		}
	}
}

func TestShouldCompactWithUsage(t *testing.T) {
	c := NewCompactor(nil, nil, "claude-haiku-4-5", 0.8)

	// Under threshold (160k = 200k * 0.8)
	usage := &anthropic.Usage{InputTokens: 100_000}
	if c.ShouldCompact(nil, usage) {
		t.Error("should not compact at 100k tokens")
	}

	// Over threshold
	usage = &anthropic.Usage{InputTokens: 170_000}
	if !c.ShouldCompact(nil, usage) {
		t.Error("should compact at 170k tokens")
	}

	// Cache tokens count toward total
	usage = &anthropic.Usage{
		InputTokens:          50_000,
		CacheReadInputTokens: 120_000,
	}
	if !c.ShouldCompact(nil, usage) {
		t.Error("should compact when cache_read + input > threshold")
	}
}

func TestShouldCompactWithEstimate(t *testing.T) {
	c := NewCompactor(nil, nil, "claude-haiku-4-5", 0.8)

	// Small conversation — should not compact
	small := []anthropic.Message{
		{Role: "user", Content: anthropic.TextContent("hello")},
		{Role: "assistant", Content: anthropic.TextContent("hi")},
	}
	if c.ShouldCompact(small, nil) {
		t.Error("should not compact small conversation")
	}
}

func TestShouldCompactExactThreshold(t *testing.T) {
	c := NewCompactor(nil, nil, "claude-haiku-4-5", 0.8)

	// Exactly at threshold (200k * 0.8 = 160k)
	usage := &anthropic.Usage{InputTokens: 160_000}
	if c.ShouldCompact(nil, usage) {
		t.Error("should not compact at exact threshold (> not >=)")
	}

	// One over
	usage = &anthropic.Usage{InputTokens: 160_001}
	if !c.ShouldCompact(nil, usage) {
		t.Error("should compact one above threshold")
	}
}
