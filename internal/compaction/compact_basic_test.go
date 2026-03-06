package compaction

import (
	"testing"

	"foci/internal/provider"
)

// TestEstimateTokens verifies token estimation for messages.
func TestEstimateTokens(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("hello world")},    // 11 chars / 4 = 2
		{Role: "assistant", Content: provider.TextContent("hi there!")}, // 9 chars / 4 = 2
	}

	tokens := estimateTokens(msgs)
	if tokens < 2 {
		t.Errorf("estimateTokens = %d, expected >= 2", tokens)
	}
}

// TestEstimateTokensEmpty verifies token estimation for empty message list.
func TestEstimateTokensEmpty(t *testing.T) {
	tokens := estimateTokens(nil)
	if tokens != 0 {
		t.Errorf("estimateTokens(nil) = %d, want 0", tokens)
	}
}

// TestContextLimit verifies context window limits for various models.
func TestContextLimit(t *testing.T) {
	tests := []struct {
		model string
		want  int
	}{
		{"claude-haiku-4-5", 200_000},
		{"claude-sonnet-4-5", 200_000},
		{"claude-opus-4-6", 200_000},
		{"gemini-2.5-pro", 1_000_000},
		{"gemini-2.5-flash", 1_000_000},
		{"gemini-2.0-flash", 1_000_000},
		{"gemini-1.5-pro", 2_000_000},
		{"gemini-1.5-flash", 2_000_000},
		{"gemini-2.0-pro", 1_000_000},
		{"gemini-other", 1_000_000},
		{"unknown-model", 200_000},
		{"gpt-4", 200_000},
		{"", 200_000},
	}
	for _, tt := range tests {
		if got := contextLimit(tt.model); got != tt.want {
			t.Errorf("contextLimit(%q) = %d, want %d", tt.model, got, tt.want)
		}
	}
}

// TestContextLimitExported verifies exported ContextLimit function.
func TestContextLimitExported(t *testing.T) {
	tests := []struct {
		model string
		want  int
	}{
		{"claude-haiku-4-5", 200_000},
		{"gemini-2.5-flash", 1_000_000},
		{"unknown-model", 200_000},
	}
	for _, tt := range tests {
		if got := ContextLimit(tt.model); got != tt.want {
			t.Errorf("ContextLimit(%q) = %d, want %d", tt.model, got, tt.want)
		}
	}
}

// TestShouldCompactWithUsage verifies compaction decision based on token usage.
func TestShouldCompactWithUsage(t *testing.T) {
	c := NewCompactor(nil, "claude-haiku-4-5", 0.8)

	// Under threshold (160k = 200k * 0.8)
	usage := &provider.Usage{InputTokens: 100_000}
	if c.ShouldCompact("test/session", nil, usage) {
		t.Error("should not compact at 100k tokens")
	}

	// Over threshold
	usage = &provider.Usage{InputTokens: 170_000}
	if !c.ShouldCompact("test/session", nil, usage) {
		t.Error("should compact at 170k tokens")
	}

	// Cache tokens count toward total
	usage = &provider.Usage{
		InputTokens:          50_000,
		CacheReadInputTokens: 120_000,
	}
	if !c.ShouldCompact("test/session", nil, usage) {
		t.Error("should compact when cache_read + input > threshold")
	}
}

// TestShouldCompactWithEstimate verifies compaction decision based on message estimate.
func TestShouldCompactWithEstimate(t *testing.T) {
	c := NewCompactor(nil, "claude-haiku-4-5", 0.8)

	// Small conversation — should not compact
	small := []provider.Message{
		{Role: "user", Content: provider.TextContent("hello")},
		{Role: "assistant", Content: provider.TextContent("hi")},
	}
	if c.ShouldCompact("test/session", small, nil) {
		t.Error("should not compact small conversation")
	}
}

// TestShouldCompactExactThreshold verifies compaction at exact threshold boundary.
func TestShouldCompactExactThreshold(t *testing.T) {
	c := NewCompactor(nil, "claude-haiku-4-5", 0.8)

	// Exactly at threshold (200k * 0.8 = 160k)
	usage := &provider.Usage{InputTokens: 160_000}
	if c.ShouldCompact("test/session", nil, usage) {
		t.Error("should not compact at exact threshold (> not >=)")
	}

	// One over
	usage = &provider.Usage{InputTokens: 160_001}
	if !c.ShouldCompact("test/session", nil, usage) {
		t.Error("should compact one above threshold")
	}
}
