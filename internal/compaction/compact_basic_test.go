package compaction

import (
	"testing"

	"foci/internal/provider"
)

func TestEstimateTokens(t *testing.T) {
	// Verifies that the token estimator returns a non-zero count for a
	// small set of messages, confirming the character-based heuristic produces a sensible
	// lower bound rather than returning zero.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("hello world")},    // 11 chars / 4 = 2
		{Role: "assistant", Content: provider.TextContent("hi there!")}, // 9 chars / 4 = 2
	}

	tokens := estimateTokens(msgs)
	if tokens < 2 {
		t.Errorf("estimateTokens = %d, expected >= 2", tokens)
	}
}

func TestEstimateTokensEmpty(t *testing.T) {
	// Verifies that estimating tokens for a nil or empty message
	// list returns exactly zero, so callers can safely use the result without a zero-check
	// guard.
	tokens := estimateTokens(nil)
	if tokens != 0 {
		t.Errorf("estimateTokens(nil) = %d, want 0", tokens)
	}
}

func TestContextLimit(t *testing.T) {
	// Verifies the internal contextLimit function covers all supported model
	// families — Claude (200k), Gemini 2.x (1M), Gemini 1.5 (2M) — as well as unknown
	// models, which must fall back to the 200k default.
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

func TestContextLimitExported(t *testing.T) {
	// Verifies that the exported ContextLimit wrapper delegates
	// correctly to the internal function, covering a representative Claude, Gemini, and
	// unknown model to confirm the public API behaves identically to the private one.
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

func TestShouldCompactWithUsage(t *testing.T) {
	// Verifies that ShouldCompact returns true only when the
	// total token count (input + cache read) exceeds the threshold, and that both token
	// types are correctly summed before comparison — testing under, at, and over the limit.
	c := NewCompactor(nil, 0.8)

	// Under threshold (160k = 200k * 0.8)
	usage := &provider.Usage{InputTokens: 100_000}
	if c.ShouldCompact("claude-haiku-4-5", "test/session", nil, usage) {
		t.Error("should not compact at 100k tokens")
	}

	// Over threshold
	usage = &provider.Usage{InputTokens: 170_000}
	if !c.ShouldCompact("claude-haiku-4-5", "test/session", nil, usage) {
		t.Error("should compact at 170k tokens")
	}

	// Cache tokens count toward total
	usage = &provider.Usage{
		InputTokens:          50_000,
		CacheReadInputTokens: 120_000,
	}
	if !c.ShouldCompact("claude-haiku-4-5", "test/session", nil, usage) {
		t.Error("should compact when cache_read + input > threshold")
	}
}

func TestShouldCompactWithEstimate(t *testing.T) {
	// Verifies that when no usage stats are provided,
	// ShouldCompact falls back to the character-based token estimate and correctly rejects
	// a tiny conversation that is well under any reasonable compaction threshold.
	c := NewCompactor(nil, 0.8)

	// Small conversation — should not compact
	small := []provider.Message{
		{Role: "user", Content: provider.TextContent("hello")},
		{Role: "assistant", Content: provider.TextContent("hi")},
	}
	if c.ShouldCompact("claude-haiku-4-5", "test/session", small, nil) {
		t.Error("should not compact small conversation")
	}
}

func TestShouldCompactExactThreshold(t *testing.T) {
	// Verifies the boundary condition: exactly at the
	// threshold is not a trigger (uses strict greater-than), while one token over the
	// threshold is, confirming the comparison operator is correct.
	c := NewCompactor(nil, 0.8)

	// Exactly at threshold (200k * 0.8 = 160k)
	usage := &provider.Usage{InputTokens: 160_000}
	if c.ShouldCompact("claude-haiku-4-5", "test/session", nil, usage) {
		t.Error("should not compact at exact threshold (> not >=)")
	}

	// One over
	usage = &provider.Usage{InputTokens: 160_001}
	if !c.ShouldCompact("claude-haiku-4-5", "test/session", nil, usage) {
		t.Error("should compact one above threshold")
	}
}
